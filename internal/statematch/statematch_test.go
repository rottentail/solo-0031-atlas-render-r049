package statematch

import (
	"path/filepath"
	"testing"
)

// TestReferenceModelLifecycle walks one hand-built interleaving of edit,
// archive, restore, freeze and revoke, pinning the oracle's own transitions
// and the rule that rejected requests never advance its event stream.
func TestReferenceModelLifecycle(t *testing.T) {
	docs := ValidDocuments()
	ref := NewReferenceModel(nil, docs)

	must := func(c Command, want OutcomeClass) Outcome {
		t.Helper()
		got := ref.Apply(c)
		if got.Class != want {
			t.Fatalf("%s: want %s got %s", c.Kind, want, got.Class)
		}
		return got
	}
	must(Command{Kind: CmdImportAsset, AssetID: Asset1}, OutcomeOK)
	created := must(Command{Kind: CmdPutScene, SceneID: SceneA, Expected: 0, DocIndex: 0}, OutcomeOK)
	if created.Rev != 1 {
		t.Fatalf("first revision = %d, want 1", created.Rev)
	}

	// A stale revision cannot overwrite the committed scene.
	must(Command{Kind: CmdPutScene, SceneID: SceneA, Expected: 0, DocIndex: 1}, OutcomeConflict)
	if sc, _ := ref.Scene(SceneA); sc.Revision != 1 {
		t.Fatalf("stale put moved revision to %d", sc.Revision)
	}

	// Archive bumps the revision; edits and freezes are then rejected.
	archived := must(Command{Kind: CmdSetState, SceneID: SceneA, Expected: 1, Target: "archived"}, OutcomeOK)
	if archived.Rev != 2 {
		t.Fatalf("archive revision = %d, want 2", archived.Rev)
	}
	must(Command{Kind: CmdPutScene, SceneID: SceneA, Expected: 2, DocIndex: 1}, OutcomeConflict)
	must(Command{Kind: CmdFreeze, SceneID: SceneA, Expected: 2}, OutcomeConflict)
	if ref.SnapshotCount() != 0 {
		t.Fatal("archived freeze produced a snapshot in the model")
	}

	// Restore, then a valid freeze captures the current document.
	must(Command{Kind: CmdSetState, SceneID: SceneA, Expected: 2, Target: "active"}, OutcomeOK)
	frozen := must(Command{Kind: CmdFreeze, SceneID: SceneA, Expected: 3}, OutcomeOK)
	if frozen.Digest == "" || ref.SnapshotCount() != 1 {
		t.Fatal("freeze did not capture exactly one snapshot")
	}

	// Later edits must not change the captured snapshot.
	snapBefore, _ := ref.Snapshot(1)
	must(Command{Kind: CmdPutScene, SceneID: SceneA, Expected: 3, DocIndex: 1}, OutcomeOK)
	snapAfter, _ := ref.Snapshot(1)
	if snapAfter.Digest != snapBefore.Digest {
		t.Fatal("snapshot digest drifted after a later edit")
	}

	// Revoke is idempotent and never revives.
	must(Command{Kind: CmdRevoke, SnapshotRef: 1}, OutcomeOK)
	must(Command{Kind: CmdRevoke, SnapshotRef: 1}, OutcomeOK)
	if s, _ := ref.Snapshot(1); s.State() != "revoked" {
		t.Fatal("snapshot not revoked")
	}
	must(Command{Kind: CmdRevoke, SnapshotRef: 99}, OutcomeMissing)

	// Every rejection above left the event stream at exactly the successful
	// transitions: import, save, archive, restore, freeze, later save, revoke.
	wantKinds := []string{"asset.imported", "scene.saved", "scene.archived", "scene.active", "snapshot.frozen", "scene.saved", "snapshot.revoked"}
	if len(ref.Events) != len(wantKinds) {
		t.Fatalf("event count = %d, want %d (%v)", len(ref.Events), len(wantKinds), ref.Events)
	}
	for i, k := range wantKinds {
		if ref.Events[i].Kind != k {
			t.Fatalf("event %d = %s, want %s", i, ref.Events[i].Kind, k)
		}
	}
}

// TestRealServicePasses runs generated interleavings against the real
// service with no injected fault and requires every invariant to hold.
func TestRealServicePasses(t *testing.T) {
	cfg := DefaultSearchConfig()
	cfg.Iterations = 25
	cfg.Gen.Steps = 40
	res := Search(cfg, t.TempDir(), FaultNone)
	if res.Mismatch != nil {
		t.Fatalf("unexpected mismatch at seed %d: %v", res.FailingSeed, res.Mismatch)
	}
}

// TestEveryFaultDetectedAndShrunk proves each injected defect is found and
// reduced to a sequence that still reproduces the same rule.
func TestEveryFaultDetectedAndShrunk(t *testing.T) {
	for _, fault := range ValidFaults() {
		if fault == FaultNone {
			continue
		}
		t.Run(fault, func(t *testing.T) {
			cfg := DefaultSearchConfig()
			cfg.Iterations = 30
			cfg.Gen.Steps = 40
			res := Search(cfg, t.TempDir(), fault)
			if res.Mismatch == nil {
				t.Fatalf("fault %q was not detected", fault)
			}
			if res.Shrunk == nil {
				t.Fatalf("fault %q produced no shrink result", fault)
			}
			if len(res.Shrunk.Sequence) > len(res.Sequence) {
				t.Fatalf("shrink grew the sequence: %d -> %d", len(res.Sequence), len(res.Shrunk.Sequence))
			}
			if res.Shrunk.Mismatch.Rule != res.Mismatch.Rule {
				t.Fatalf("rule changed during shrink: %s -> %s", res.Mismatch.Rule, res.Shrunk.Mismatch.Rule)
			}
		})
	}
}

// TestReopenEveryStep exercises the persistence recovery invariant on a
// short generated sequence, re-validating the store after every command.
func TestReopenEveryStep(t *testing.T) {
	seq := Generate(7, GenConfig{Steps: 25, InvalidRate: 10, StaleRate: 25, MissingRate: 20})
	mm, _ := Run(seq, filepath.Join(t.TempDir(), "state.json"), FactoryFor(FaultNone), RunOptions{ReopenEveryStep: true})
	if mm != nil {
		t.Fatalf("reopen run mismatch: %v", mm)
	}
}

// TestShrinkKeepsFailingRule checks that shrinking never silently turns a
// failing sequence into a passing one.
func TestShrinkKeepsFailingRule(t *testing.T) {
	cfg := DefaultSearchConfig()
	cfg.Iterations = 10
	cfg.Gen.Steps = 30
	res := Search(cfg, t.TempDir(), FaultHalfRevision)
	if res.Shrunk == nil {
		t.Skip("no failure found in this seed range")
	}
	mm, _ := Run(res.Shrunk.Sequence, filepath.Join(t.TempDir(), "confirm.json"),
		FactoryFor(FaultHalfRevision), RunOptions{})
	if mm == nil || mm.Rule != res.Shrunk.Mismatch.Rule {
		t.Fatalf("shrunk sequence does not reproduce rule %s", res.Shrunk.Mismatch.Rule)
	}
}
