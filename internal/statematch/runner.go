package statematch

import (
	"atlas-render/internal/model"
	"fmt"
	"sort"
)

// ExecutorFactory builds a fresh SUT plus its paired reference model for a
// replay run. Both share the snapshot ordinal identity map.
type ExecutorFactory func(statePath string) (Executor, *ReferenceModel, error)

// RunOptions controls verification cost.
type RunOptions struct {
	// ReopenEveryStep closes and re-opens the persisted store after every
	// command, proving each committed transaction survives recovery and
	// startup validation. When false, a single reopen runs at the end.
	ReopenEveryStep bool
}

// Mismatch is one rule violation found while replaying a sequence.
type Mismatch struct {
	Index   int     `json:"index"`
	Rule    string  `json:"rule"`
	Command Command `json:"command"`
	Detail  string  `json:"detail"`
}

func (m Mismatch) String() string {
	return fmt.Sprintf("step %d [%s] %s\n  rule: %s", m.Index, m.Command.Kind, m.Command.String(), m.Rule)
}

// Rule names reported in mismatches. Each maps to a clause of PROJECT_SPEC.md
// (documented in docs/state-verification.md).
const (
	RuleOutcome              = "outcome-classification" // 400/404/409/200 分类
	RuleStaleOverwrite       = "stale-request-rejected" // 过期请求不得覆盖新内容
	RuleSceneState           = "scene-state-agreement"  // 修订号/状态/文档与模型一致
	RuleAssetState           = "asset-state-agreement"
	RuleSnapshotSet          = "snapshot-set-agreement" // 归档不得产生新快照
	RuleSnapshotImmutability = "snapshot-immutability"  // 旧快照不受当前状态影响
	RuleEventStream          = "event-stream-agreement" // 失败动作不能推进事件
	RuleFailureAtomicity     = "failure-atomicity"      // 失败不留下一半修订
	RulePersistence          = "persistence-recovery"   // 重开后状态逐字节一致
)

// Run replays seq against a freshly built SUT and reference model. It stops
// at the first mismatch and returns it; nil means every invariant held.
func Run(seq []Command, statePath string, factory ExecutorFactory, opts RunOptions) (*Mismatch, []string) {
	exec, ref, e := factory(statePath)
	trace := []string{}
	if e != nil {
		return &Mismatch{Index: -1, Rule: RulePersistence, Detail: "factory: " + e.Error()}, trace
	}
	defer exec.Close()

	for i, c := range seq {
		before := fingerprint(exec.Read())
		want := ref.Apply(c)
		got := exec.Execute(c)
		trace = append(trace, fmt.Sprintf("%02d %s -> want=%s got=%s", i, c.String(), want.Class, got.Class))

		if mm := compareOutcome(i, c, want, got); mm != nil {
			return mm, trace
		}
		// Semantic comparisons first, so a rejected command's defect is
		// attributed to the specific rule it violates (snapshot set, event
		// stream, scene content...) rather than to a generic fingerprint.
		if mm := compareScenes(i, c, got, ref, exec); mm != nil {
			return mm, trace
		}
		if mm := compareAssets(i, c, ref, exec); mm != nil {
			return mm, trace
		}
		if mm := compareSnapshots(i, c, got, ref, exec); mm != nil {
			return mm, trace
		}
		if mm := compareEvents(i, c, got, ref, exec); mm != nil {
			return mm, trace
		}
		if got.Class != OutcomeOK {
			// Any rejected request must leave the full persisted state
			// untouched: no overwrite, no half revision, no event advance.
			if fingerprint(exec.Read()) != before {
				return &Mismatch{Index: i, Rule: rejectedRule(c), Command: c,
					Detail: "rejected request changed persisted state without a corresponding model transition"}, trace
			}
		}
		if opts.ReopenEveryStep {
			committed := fingerprint(exec.Read())
			if e := exec.Reopen(); e != nil {
				return &Mismatch{Index: i, Rule: RulePersistence, Command: c, Detail: e.Error()}, trace
			}
			if fingerprint(exec.Read()) != committed {
				return &Mismatch{Index: i, Rule: RulePersistence, Command: c,
					Detail: "state changed across store reopen"}, trace
			}
		}
	}

	if !opts.ReopenEveryStep {
		before := fingerprint(exec.Read())
		if e := exec.Reopen(); e != nil {
			return &Mismatch{Index: len(seq) - 1, Rule: RulePersistence, Detail: e.Error()}, trace
		}
		if fingerprint(exec.Read()) != before {
			return &Mismatch{Index: len(seq) - 1, Rule: RulePersistence, Detail: "state changed across store reopen"}, trace
		}
	}
	return nil, trace
}

func fingerprint(st model.State) string {
	return model.CanonicalDigest(st)
}

func compareOutcome(i int, c Command, want, got Outcome) *Mismatch {
	if want.Class != got.Class {
		return &Mismatch{Index: i, Rule: RuleOutcome, Command: c,
			Detail: fmt.Sprintf("expected class %s, SUT returned %s", want.Class, got.Class)}
	}
	if want.Class != OutcomeOK {
		return nil
	}
	if (c.Kind == CmdPutScene || c.Kind == CmdSetState) && (want.Rev != got.Rev || want.State != got.State) {
		return &Mismatch{Index: i, Rule: RuleOutcome, Command: c,
			Detail: fmt.Sprintf("expected revision/state %d/%s, got %d/%s", want.Rev, want.State, got.Rev, got.State)}
	}
	if c.Kind == CmdFreeze && want.Digest != got.Digest {
		return &Mismatch{Index: i, Rule: RuleOutcome, Command: c,
			Detail: "snapshot digest disagrees with the frozen model content"}
	}
	return nil
}

// rejectedRule names the guarantee violated when a rejected request still
// mutates state. Stale document writes that lose the optimistic-concurrency
// check overwrite committed content; rejected transitions leave half
// revisions behind and are failure-atomicity breaks.
func rejectedRule(c Command) string {
	if c.Kind == CmdPutScene {
		return RuleStaleOverwrite
	}
	return RuleFailureAtomicity
}

type sceneView struct {
	revision int
	state    string
	name     string
	doc      string
}

func compareScenes(i int, c Command, got Outcome, ref *ReferenceModel, exec Executor) *Mismatch {
	view := map[string]sceneView{}
	for id, sc := range exec.Read().Scenes {
		view[id] = sceneView{revision: sc.Revision, state: sc.State, name: sc.Name, doc: model.CanonicalDigest(sc.Document)}
	}
	want := map[string]sceneView{}
	for id, sc := range ref.Scenes {
		want[id] = sceneView{revision: sc.Revision, state: sc.State, name: sc.Name, doc: model.CanonicalDigest(sc.Document)}
	}
	if fmt.Sprint(want) != fmt.Sprint(view) {
		rule := RuleSceneState
		if got.Class != OutcomeOK {
			rule = rejectedRule(c)
		}
		return &Mismatch{Index: i, Rule: rule, Command: c, Detail: diffMaps("scene", want, view)}
	}
	return nil
}

type assetView struct {
	state  string
	digest string
}

func compareAssets(i int, c Command, ref *ReferenceModel, exec Executor) *Mismatch {
	got := map[string]assetView{}
	for id, a := range exec.Read().Assets {
		got[id] = assetView{state: a.State, digest: a.Digest}
	}
	want := map[string]assetView{}
	for id, a := range ref.Assets {
		want[id] = assetView{state: a.State, digest: a.Digest}
	}
	if fmt.Sprint(want) != fmt.Sprint(got) {
		return &Mismatch{Index: i, Rule: RuleAssetState, Command: c,
			Detail: diffMaps("asset", want, got)}
	}
	return nil
}

type snapshotView struct {
	ordinal  int
	scene    string
	revision int
	state    string
	digest   string
	doc      string
	assets   string
}

// compareSnapshots checks both the set of snapshots (no snapshot may appear
// on archive/freeze rejection) and every captured record (old snapshots are
// immutable and keep their original digest under later scene edits).
func compareSnapshots(i int, c Command, got Outcome, ref *ReferenceModel, exec Executor) *Mismatch {
	st := exec.Read()
	ids := make([]string, 0, len(st.Snapshots))
	for id := range st.Snapshots {
		ids = append(ids, id)
	}
	sort.Strings(ids) // service IDs are zero-padded, so lexical order is creation order

	if len(ids) != ref.SnapshotCount() {
		rule := RuleSnapshotSet
		if got.Class != OutcomeOK {
			// A freeze rejected from an archived scene still snapshotting.
			rule = RuleSnapshotSet
		}
		return &Mismatch{Index: i, Rule: rule, Command: c,
			Detail: fmt.Sprintf("model holds %d snapshots, SUT holds %d", ref.SnapshotCount(), len(ids))}
	}
	for ord, id := range ids {
		ordinal := ord + 1
		sut := st.Snapshots[id]
		rs, ok := ref.Snapshot(ordinal)
		if !ok {
			return &Mismatch{Index: i, Rule: RuleSnapshotSet, Command: c,
				Detail: fmt.Sprintf("unexpected SUT snapshot %s at ordinal %d", id, ordinal)}
		}
		view := snapshotView{
			ordinal: ordinal, scene: sut.SceneID, revision: sut.Revision, state: sut.State,
			digest: sut.Digest, doc: model.CanonicalDigest(sut.Document),
			assets: model.CanonicalDigest(sut.Assets),
		}
		want := snapshotView{
			ordinal: ordinal, scene: rs.SceneID, revision: rs.Revision, state: rs.State(),
			digest: rs.Digest, doc: model.CanonicalDigest(rs.Doc),
			assets: model.CanonicalDigest(rs.Assets),
		}
		if view != want {
			return &Mismatch{Index: i, Rule: RuleSnapshotImmutability, Command: c,
				Detail: fmt.Sprintf("snapshot ordinal %d (%s) drifted from captured content", ordinal, id)}
		}
		// The stored digest must still authenticate the stored bytes.
		if model.SnapshotDigest(sut) != sut.Digest {
			return &Mismatch{Index: i, Rule: RuleSnapshotImmutability, Command: c,
				Detail: fmt.Sprintf("snapshot %s digest no longer authenticates its bytes", id)}
		}
	}
	return nil
}

func compareEvents(i int, c Command, got Outcome, ref *ReferenceModel, exec Executor) *Mismatch {
	st := exec.Read()
	view := make([]RefEvent, 0, len(st.Events))
	for _, ev := range st.Events {
		subject := ev.Subject
		if ev.Kind == "snapshot.frozen" || ev.Kind == "snapshot.revoked" {
			if ord, ok := ref.Ordinals[ev.Subject]; ok {
				subject = snapshotSubject(ord)
			}
		}
		view = append(view, RefEvent{Kind: ev.Kind, Subject: subject})
	}
	want := tailEvents(ref.Events)
	view = tailEvents(view)
	if fmt.Sprint(want) != fmt.Sprint(view) {
		rule := RuleEventStream
		if got.Class != OutcomeOK {
			rule = RuleFailureAtomicity // a failed action advanced the event stream
		}
		return &Mismatch{Index: i, Rule: rule, Command: c,
			Detail: fmt.Sprintf("model events %v\n                 SUT events   %v", want, view)}
	}
	return nil
}

func tailEvents(ev []RefEvent) []RefEvent {
	if len(ev) > 500 {
		return ev[len(ev)-500:]
	}
	return ev
}

func diffMaps[V any](kind string, want, got map[string]V) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	ids := make([]string, 0, len(keys))
	for k := range keys {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	for _, k := range ids {
		w, okW := want[k]
		g, okG := got[k]
		if !okW || !okG || fmt.Sprint(w) != fmt.Sprint(g) {
			return fmt.Sprintf("%s %s: model=%v sut=%v", kind, k, w, g)
		}
	}
	return kind + " maps differ"
}
