package statematch

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
)

// Executor is the system-under-test side of the harness. Every command is
// applied through the same Service API the HTTP layer calls, and the
// executor reports the classified outcome together with concrete snapshot
// identity so the harness can track ordinal -> real ID mapping.
type Executor interface {
	Execute(c Command) Outcome
	// Read returns a deep copy of the whole persisted state.
	Read() model.State
	// Reopen closes and re-opens the store, replaying startup validation.
	Reopen() error
	Close() error
}

// classify maps domain errors onto the outcome classes the oracle uses.
func classify(e error) OutcomeClass {
	if e == nil {
		return OutcomeOK
	}
	if de, ok := e.(*model.Error); ok {
		switch de.Status {
		case 400:
			return OutcomeInvalid
		case 404:
			return OutcomeMissing
		case 409:
			return OutcomeConflict
		}
	}
	return OutcomeInvalid // unexpected error shape: oracle will flag the mismatch
}

// RealExecutor drives an actual service.Service backed by a real on-disk
// store, so persistence validation and atomic-replace behavior are covered.
type RealExecutor struct {
	storePath string
	store     *persist.Store
	svc       *service.Service
	idMap     map[string]int
	docs      []model.Document
}

// NewRealExecutor opens (or creates) a store under statePath.
func NewRealExecutor(statePath string, docs []model.Document) (*RealExecutor, error) {
	st, e := persist.Open(statePath)
	if e != nil {
		return nil, e
	}
	return &RealExecutor{
		storePath: statePath,
		store:     st,
		svc:       &service.Service{Store: st},
		idMap:     map[string]int{},
		docs:      docs,
	}, nil
}

// IDMap exposes the ordinal map so the reference model shares identity.
func (x *RealExecutor) IDMap() map[string]int { return x.idMap }

func (x *RealExecutor) Execute(c Command) Outcome {
	switch c.Kind {
	case CmdPutScene:
		var d model.Document
		if c.DocIndex >= 0 && c.DocIndex < len(x.docs) {
			d = x.docs[c.DocIndex]
		} else {
			d = InvalidDocument()
		}
		name := SceneName
		if c.BadName {
			name = "   "
		}
		sc, e := x.svc.PutScene(c.SceneID, name, c.Expected, d)
		out := Outcome{Class: classify(e)}
		if e == nil {
			out.Rev, out.State = sc.Revision, sc.State
		}
		return out
	case CmdSetState:
		sc, e := x.svc.SceneState(c.SceneID, c.Expected, c.Target)
		out := Outcome{Class: classify(e)}
		if e == nil {
			out.Rev, out.State = sc.Revision, sc.State
		}
		return out
	case CmdFreeze:
		snap, e := x.svc.Freeze(c.SceneID, c.Expected)
		out := Outcome{Class: classify(e)}
		if e == nil {
			x.idMap[snap.ID] = len(x.idMap) + 1
			out.Rev = snap.Revision
			out.Digest = snap.Digest
		}
		return out
	case CmdRevoke:
		e := x.svc.Revoke(x.snapshotID(c.SnapshotRef))
		return Outcome{Class: classify(e)}
	case CmdImportAsset:
		_, e := x.svc.ImportAsset(c.AssetID, TinyPNG())
		return Outcome{Class: classify(e)}
	case CmdRetireAsset:
		e := x.svc.RetireAsset(c.AssetID)
		return Outcome{Class: classify(e)}
	default:
		return Outcome{Class: OutcomeInvalid}
	}
}

// snapshotID translates a logical ordinal into the concrete service ID.
// Unknown ordinals resolve to a well-formed ID that cannot exist.
func (x *RealExecutor) snapshotID(ref int) string {
	if ref < 1 {
		return MissingSnapshotID
	}
	for id, ord := range x.idMap {
		if ord == ref {
			return id
		}
	}
	return MissingSnapshotID
}

func (x *RealExecutor) Read() model.State { return x.store.Read() }

func (x *RealExecutor) Reopen() error {
	if e := x.store.Close(); e != nil {
		return e
	}
	st, e := persist.Open(x.storePath)
	if e != nil {
		return e
	}
	x.store = st
	x.svc = &service.Service{Store: st}
	return nil
}

func (x *RealExecutor) Close() error {
	if x.store == nil {
		return nil
	}
	return x.store.Close()
}
