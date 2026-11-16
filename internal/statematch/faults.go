package statematch

import (
	"atlas-render/internal/model"
	"atlas-render/internal/scene"
)

// Fault names understood by FaultExecutor. These deliberately violate one
// lifecycle rule each, so the harness can be demonstrated end-to-end without
// touching production code.
const (
	FaultNone          = "none"
	FaultStalePut      = "stale-put"      // stale write overwrites committed content
	FaultArchiveFreeze = "archive-freeze" // freeze against an archived scene still snapshots
	FaultHalfRevision  = "half-revision"  // failed transition leaves a revision bump behind
	FaultPhantomEvent  = "phantom-event"  // rejected requests still emit events
	FaultSnapshotDrift = "snapshot-drift" // later edits rewrite captured snapshots
)

// ValidFaults lists selectable fault injections.
func ValidFaults() []string {
	return []string{FaultNone, FaultStalePut, FaultArchiveFreeze, FaultHalfRevision, FaultPhantomEvent, FaultSnapshotDrift}
}

// FaultExecutor wraps a real executor and applies one named implementation
// defect after the genuine call completes. The defect runs in its own
// persisted transaction; defects that fail validation are rolled back by
// the store just like any other mutation.
type FaultExecutor struct {
	inner *RealExecutor
	fault string
}

// NewFaultExecutor decorates a real executor with the named fault.
func NewFaultExecutor(inner *RealExecutor, fault string) *FaultExecutor {
	return &FaultExecutor{inner: inner, fault: fault}
}

func (f *FaultExecutor) Execute(c Command) Outcome {
	out := f.inner.Execute(c)
	switch f.fault {
	case FaultStalePut:
		if c.Kind == CmdPutScene && out.Class == OutcomeConflict {
			f.stalePut(c)
		}
	case FaultArchiveFreeze:
		if c.Kind == CmdFreeze && out.Class == OutcomeConflict {
			f.archiveFreeze(c)
		}
	case FaultHalfRevision:
		if c.Kind == CmdSetState && out.Class == OutcomeConflict {
			f.halfRevision(c)
		}
	case FaultPhantomEvent:
		if out.Class != OutcomeOK {
			f.phantomEvent(c)
		}
	case FaultSnapshotDrift:
		if c.Kind == CmdPutScene && out.Class == OutcomeOK {
			f.snapshotDrift(c)
		}
	}
	return out
}

func (f *FaultExecutor) Read() model.State { return f.inner.Read() }
func (f *FaultExecutor) Reopen() error     { return f.inner.Reopen() }
func (f *FaultExecutor) Close() error      { return f.inner.Close() }

// stalePut forces the rejected document into the scene at a bumped revision
// and emits no event: the stale request wins over committed content.
func (f *FaultExecutor) stalePut(c Command) {
	if c.DocIndex < 0 || c.DocIndex >= len(f.inner.docs) {
		return
	}
	_ = f.inner.store.Update(func(st *model.State) error {
		sc, ok := st.Scenes[c.SceneID]
		if !ok {
			return nil
		}
		sc.Document = f.inner.docs[c.DocIndex]
		sc.Revision++
		st.Scenes[c.SceneID] = sc
		return nil
	})
}

// archiveFreeze captures a snapshot for a scene whose freeze was rejected,
// so archiving must not produce snapshots is violated.
func (f *FaultExecutor) archiveFreeze(c Command) {
	_ = f.inner.store.Update(func(st *model.State) error {
		sc, ok := st.Scenes[c.SceneID]
		if !ok || sc.State != "archived" {
			return nil
		}
		assets := map[string]model.Asset{}
		for _, ref := range scene.References(sc.Document) {
			if a, ok := st.Assets[ref]; ok {
				assets[ref] = a
			}
		}
		snap := model.Snapshot{
			ID:       model.ID("snap", st.Sequence+1),
			SceneID:  c.SceneID,
			Revision: sc.Revision,
			State:    "active",
			Document: sc.Document,
			Assets:   assets,
		}
		snap.Digest = model.SnapshotDigest(snap)
		st.Snapshots[snap.ID] = snap
		return nil
	})
}

// halfRevision commits a revision bump even though the transition failed,
// leaving a partial revision with no matching event or state change.
func (f *FaultExecutor) halfRevision(c Command) {
	_ = f.inner.store.Update(func(st *model.State) error {
		sc, ok := st.Scenes[c.SceneID]
		if !ok {
			return nil
		}
		sc.Revision++
		st.Scenes[c.SceneID] = sc
		return nil
	})
}

// phantomEvent advances the event stream for a rejected request.
func (f *FaultExecutor) phantomEvent(c Command) {
	_ = f.inner.store.Update(func(st *model.State) error {
		st.Emit("phantom."+string(c.Kind), c.SceneID+c.AssetID)
		return nil
	})
}

// snapshotDrift rewrites already-captured snapshots of the edited scene so
// old snapshots become coupled to the scene's current state.
func (f *FaultExecutor) snapshotDrift(c Command) {
	if c.DocIndex < 0 || c.DocIndex >= len(f.inner.docs) {
		return
	}
	_ = f.inner.store.Update(func(st *model.State) error {
		for id, snap := range st.Snapshots {
			if snap.SceneID != c.SceneID {
				continue
			}
			assets := map[string]model.Asset{}
			for _, ref := range scene.References(f.inner.docs[c.DocIndex]) {
				if a, ok := st.Assets[ref]; ok && a.State == "available" {
					assets[ref] = a
				}
			}
			snap.Document = f.inner.docs[c.DocIndex]
			snap.Assets = assets
			snap.Digest = model.SnapshotDigest(snap)
			st.Snapshots[id] = snap
		}
		return nil
	})
}
