package statematch

import (
	"atlas-render/internal/model"
	"atlas-render/internal/scene"
	"strings"
)

// OutcomeClass is the model/SUT agreed classification of a request.
type OutcomeClass string

const (
	OutcomeOK       OutcomeClass = "ok"
	OutcomeInvalid  OutcomeClass = "invalid"  // 400, request rejected before state work
	OutcomeMissing  OutcomeClass = "missing"  // 404, referenced entity absent
	OutcomeConflict OutcomeClass = "conflict" // 409, stale revision or lifecycle state
)

// Outcome is what the oracle predicts and the executor reports.
type Outcome struct {
	Class  OutcomeClass `json:"class"`
	Rev    int          `json:"rev,omitempty"`    // returned scene revision (ok only)
	State  string       `json:"state,omitempty"`  // returned scene state (ok only)
	Digest string       `json:"digest,omitempty"` // returned snapshot digest (freeze ok)
}

// RefSnapshot captures the immutable facts a freeze must preserve.
type RefSnapshot struct {
	Ordinal  int
	ID       string
	SceneID  string
	Revision int
	Digest   string
	Doc      model.Document
	Assets   map[string]model.Asset
	Revoked  bool
}

// SnapshotEventSubject is the logical subject used in RefEvent streams.
// Snapshot IDs are service-generated, so the oracle addresses snapshots by
// ordinal and the harness maps real IDs back via the shared id map.
const SnapshotEventPrefix = "snapshot#"

func snapshotSubject(ordinal int) string {
	return SnapshotEventPrefix + itoa(ordinal)
}

// RefEvent keeps the logical event stream without service-generated IDs or
// timestamps, which the harness cannot predict.
type RefEvent struct {
	Kind    string
	Subject string // scene/asset ID, or snapshot ordinal marker for snapshot events
}

// ReferenceModel is an independent re-implementation of the lifecycle rules
// in PROJECT_SPEC.md. It never imports the service package: its expected
// behavior comes from the prose specification, not from production code.
type ReferenceModel struct {
	Scenes    map[string]model.Scene
	Assets    map[string]model.Asset
	Snapshots map[int]*RefSnapshot // keyed by creation ordinal
	Ordinals  map[string]int       // real snapshot ID -> ordinal, shared with harness
	Events    []RefEvent
	snapCount int
	documents []model.Document
}

// NewReferenceModel builds an empty oracle. idMap is shared so that the
// executor can register concrete snapshot IDs as freezes succeed.
func NewReferenceModel(idMap map[string]int, docs []model.Document) *ReferenceModel {
	if idMap == nil {
		idMap = map[string]int{}
	}
	return &ReferenceModel{
		Scenes:    map[string]model.Scene{},
		Assets:    map[string]model.Asset{},
		Snapshots: map[int]*RefSnapshot{},
		Ordinals:  idMap,
		documents: docs,
	}
}

func (m *ReferenceModel) doc(c Command) (model.Document, bool) {
	if c.DocIndex < 0 || c.DocIndex >= len(m.documents) {
		return model.Document{}, false
	}
	return m.documents[c.DocIndex], true
}

// Apply advances the oracle exactly as the specification requires and
// returns the predicted outcome. Invalid/missing/conflict outcomes never
// mutate any state and never append an event.
func (m *ReferenceModel) Apply(c Command) Outcome {
	switch c.Kind {
	case CmdPutScene:
		return m.applyPut(c)
	case CmdSetState:
		return m.applyState(c)
	case CmdFreeze:
		return m.applyFreeze(c)
	case CmdRevoke:
		return m.applyRevoke(c)
	case CmdImportAsset:
		return m.applyImport(c)
	case CmdRetireAsset:
		return m.applyRetire(c)
	default:
		return Outcome{Class: OutcomeInvalid}
	}
}

// SPEC: 修订检查与状态写入在同一事务内完成；冲突不能覆盖已提交内容。
// SPEC: 场景 active ↔ archived；归档后拒绝修订和冻结。
// SPEC: 所有JSON请求 400 输入不合法、409 状态或版本冲突。
func (m *ReferenceModel) applyPut(c Command) Outcome {
	name := SceneName
	if c.BadName {
		name = "   "
	}
	if !scene.ValidID(c.SceneID) || strings.TrimSpace(name) == "" || len(name) > 160 {
		return Outcome{Class: OutcomeInvalid}
	}
	d, ok := m.doc(c)
	if !ok || scene.Validate(d) != nil {
		return Outcome{Class: OutcomeInvalid}
	}
	old, exists := m.Scenes[c.SceneID]
	// Stale (including wrong) expected revisions conflict; an unknown scene
	// can only be created with expectedRevision 0.
	if old.Revision != c.Expected {
		return Outcome{Class: OutcomeConflict}
	}
	if exists && old.State != "active" {
		return Outcome{Class: OutcomeConflict}
	}
	for _, ref := range scene.References(d) {
		a, ok := m.Assets[ref]
		if !ok || a.State != "available" {
			return Outcome{Class: OutcomeConflict}
		}
	}
	next := model.Scene{ID: c.SceneID, Name: name, Revision: old.Revision + 1, State: "active", Document: d}
	m.Scenes[c.SceneID] = next
	m.Events = append(m.Events, RefEvent{Kind: "scene.saved", Subject: c.SceneID})
	return Outcome{Class: OutcomeOK, Rev: next.Revision, State: next.State}
}

// SPEC: 场景 active ↔ archived，每次有效状态转换增加修订。
// Same-state requests are idempotent no-ops and do not bump revisions.
func (m *ReferenceModel) applyState(c Command) Outcome {
	if c.Target != "active" && c.Target != "archived" {
		return Outcome{Class: OutcomeInvalid}
	}
	v, ok := m.Scenes[c.SceneID]
	if !ok {
		return Outcome{Class: OutcomeMissing}
	}
	if v.Revision != c.Expected {
		return Outcome{Class: OutcomeConflict}
	}
	if v.State == c.Target {
		return Outcome{Class: OutcomeOK, Rev: v.Revision, State: v.State}
	}
	v.State = c.Target
	v.Revision++
	m.Scenes[c.SceneID] = v
	m.Events = append(m.Events, RefEvent{Kind: "scene." + c.Target, Subject: c.SceneID})
	return Outcome{Class: OutcomeOK, Rev: v.Revision, State: v.State}
}

// SPEC: 冻结把场景修订与可用素材字节捕获成快照；快照 active → revoked，不可恢复。
// SPEC: 归档后拒绝修订和冻结。
func (m *ReferenceModel) applyFreeze(c Command) Outcome {
	v, ok := m.Scenes[c.SceneID]
	if !ok {
		return Outcome{Class: OutcomeMissing}
	}
	if v.Revision != c.Expected || v.State != "active" {
		return Outcome{Class: OutcomeConflict}
	}
	assets := map[string]model.Asset{}
	for _, ref := range scene.References(v.Document) {
		a, ok := m.Assets[ref]
		if !ok || a.State != "available" {
			return Outcome{Class: OutcomeConflict}
		}
		assets[ref] = a
	}
	m.snapCount++
	snap := RefSnapshot{
		Ordinal:  m.snapCount,
		SceneID:  c.SceneID,
		Revision: v.Revision,
		Doc:      v.Document,
		Assets:   assets,
	}
	snap.Digest = model.SnapshotDigest(model.Snapshot{Document: snap.Doc, Assets: snap.Assets})
	m.Snapshots[snap.Ordinal] = &snap
	m.Events = append(m.Events, RefEvent{Kind: "snapshot.frozen", Subject: snapshotSubject(snap.Ordinal)})
	return Outcome{Class: OutcomeOK, Rev: v.Revision, State: v.State, Digest: snap.Digest}
}

func (m *ReferenceModel) applyRevoke(c Command) Outcome {
	if c.SnapshotRef < 1 {
		return Outcome{Class: OutcomeMissing}
	}
	snap, ok := m.Snapshots[c.SnapshotRef]
	if !ok {
		return Outcome{Class: OutcomeMissing}
	}
	if snap.State() == "revoked" {
		return Outcome{Class: OutcomeOK} // idempotent, no event
	}
	snap.Revoked = true
	m.Events = append(m.Events, RefEvent{Kind: "snapshot.revoked", Subject: snapshotSubject(snap.Ordinal)})
	return Outcome{Class: OutcomeOK}
}

// SPEC: 素材 available → retired，相同 ID 不能替换为不同内容。
func (m *ReferenceModel) applyImport(c Command) Outcome {
	if !scene.ValidID(c.AssetID) {
		return Outcome{Class: OutcomeInvalid}
	}
	png := TinyPNG()
	if a, ok := m.Assets[c.AssetID]; ok {
		if a.Digest == model.Digest(png) && a.State == "available" {
			return Outcome{Class: OutcomeOK} // idempotent re-upload, no event
		}
		return Outcome{Class: OutcomeConflict}
	}
	m.Assets[c.AssetID] = model.Asset{ID: c.AssetID, Digest: model.Digest(png), State: "available", Width: 2, Height: 2, PNG: png}
	m.Events = append(m.Events, RefEvent{Kind: "asset.imported", Subject: c.AssetID})
	return Outcome{Class: OutcomeOK}
}

func (m *ReferenceModel) applyRetire(c Command) Outcome {
	a, ok := m.Assets[c.AssetID]
	if !ok {
		return Outcome{Class: OutcomeMissing}
	}
	if a.State == "retired" {
		return Outcome{Class: OutcomeOK} // idempotent, no event
	}
	a.State = "retired"
	m.Assets[c.AssetID] = a
	m.Events = append(m.Events, RefEvent{Kind: "asset.retired", Subject: c.AssetID})
	return Outcome{Class: OutcomeOK}
}

// State accessor used by the generator, which drives choices from the
// oracle's current view.
func (m *ReferenceModel) Scene(id string) (model.Scene, bool) {
	v, ok := m.Scenes[id]
	return v, ok
}

func (m *ReferenceModel) Asset(id string) (model.Asset, bool) {
	v, ok := m.Assets[id]
	return v, ok
}

// SnapshotCount returns how many snapshots the oracle holds, including
// revoked ones (revocation changes mutable state, not existence).
func (m *ReferenceModel) SnapshotCount() int { return m.snapCount }

// Snapshot returns the oracle snapshot for an ordinal.
func (m *ReferenceModel) Snapshot(ordinal int) (*RefSnapshot, bool) {
	s, ok := m.Snapshots[ordinal]
	return s, ok
}

// Revoked mirrors the snapshot lifecycle flag without needing model.Snapshot.
func (s *RefSnapshot) State() string {
	if s.Revoked {
		return "revoked"
	}
	return "active"
}
