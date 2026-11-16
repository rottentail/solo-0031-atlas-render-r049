package statematch

import (
	"math/rand"
)

// GenConfig bounds the random sequences.
type GenConfig struct {
	Steps       int
	InvalidRate int // percent of put/import requests that are deliberately invalid
	StaleRate   int // percent of revision-bearing requests that use a stale revision
	MissingRate int // percent of revoke requests that address a non-existent snapshot
}

// DefaultGenConfig favors realistic traffic while keeping enough rejections
// to exercise every invariant in short sequences.
func DefaultGenConfig() GenConfig {
	return GenConfig{Steps: 60, InvalidRate: 10, StaleRate: 25, MissingRate: 20}
}

// Generator produces command sequences. Choices are driven by the reference
// model's current view, so most requests are plausible; stale and invalid
// requests are injected deliberately because rejected paths are the focus.
type generatorHandle struct {
	rng *rand.Rand
	cfg GenConfig
}

// Generate builds one deterministic sequence for the given seed. The same
// seed always yields the same commands, which makes failures reproducible.
func Generate(seed int64, cfg GenConfig) []Command {
	rng := rand.New(rand.NewSource(seed))
	docs := ValidDocuments()
	ref := NewReferenceModel(nil, docs)
	h := generatorHandle{rng: rng, cfg: cfg}
	seq := []Command{}

	// Preamble: import both assets, then create both scenes at revision 1.
	seq = append(seq, Command{Kind: CmdImportAsset, AssetID: Asset1})
	ref.Apply(seq[len(seq)-1])
	seq = append(seq, Command{Kind: CmdImportAsset, AssetID: Asset2})
	ref.Apply(seq[len(seq)-1])
	seq = append(seq, h.put(SceneA, 0, 0, ref, false))
	ref.Apply(seq[len(seq)-1])
	seq = append(seq, h.put(SceneB, 0, 1, ref, false))
	ref.Apply(seq[len(seq)-1])

	for len(seq) < cfg.Steps {
		seq = append(seq, h.next(ref))
		ref.Apply(seq[len(seq)-1])
	}
	return seq
}

// next picks one operation from the full lifecycle, weighted toward puts,
// state transitions and freezes (the interleave under verification).
func (h generatorHandle) next(ref *ReferenceModel) Command {
	roll := h.rng.Intn(100)
	switch {
	case roll < 35:
		return h.randomPut(ref)
	case roll < 60:
		return h.randomState(ref)
	case roll < 82:
		return h.randomFreeze(ref)
	case roll < 92:
		return h.randomRevoke(ref)
	case roll < 96:
		return Command{Kind: CmdImportAsset, AssetID: h.sceneOr(Asset1, Asset2)}
	default:
		return Command{Kind: CmdRetireAsset, AssetID: h.sceneOr(Asset1, Asset2)}
	}
}

func (h generatorHandle) sceneID() string {
	if h.rng.Intn(2) == 0 {
		return SceneA
	}
	return SceneB
}

func (h generatorHandle) sceneOr(a, b string) string {
	if h.rng.Intn(2) == 0 {
		return a
	}
	return b
}

func (h generatorHandle) chance(pct int) bool { return h.rng.Intn(100) < pct }

// randomPut mixes fresh edits with stale-expectedRevision writes and
// validation rejections so that "stale must not overwrite" is exercised
// alongside valid revisions.
func (h generatorHandle) randomPut(ref *ReferenceModel) Command {
	id := h.sceneID()
	sc, _ := ref.Scene(id)
	expected := sc.Revision
	if h.chance(h.cfg.StaleRate) {
		expected = h.staleRevision(sc.Revision)
	}
	badName := false
	doc := h.rng.Intn(len(ValidDocuments()))
	if h.chance(h.cfg.InvalidRate) {
		if h.chance(50) {
			badName = true
		} else {
			doc = -1
		}
	}
	return h.put(id, expected, doc, ref, badName)
}

func (h generatorHandle) put(id string, expected, docIndex int, ref *ReferenceModel, badName bool) Command {
	return Command{Kind: CmdPutScene, SceneID: id, Expected: expected, DocIndex: docIndex, BadName: badName}
}

// randomState alternates archive and restore so the two directions of the
// active ↔ archived lifecycle interleave with edits and freezes.
func (h generatorHandle) randomState(ref *ReferenceModel) Command {
	id := h.sceneID()
	sc, _ := ref.Scene(id)
	expected := sc.Revision
	if h.chance(h.cfg.StaleRate) {
		expected = h.staleRevision(sc.Revision)
	}
	target := "archived"
	if sc.State == "archived" {
		target = "active"
	}
	return Command{Kind: CmdSetState, SceneID: id, Expected: expected, Target: target}
}

func (h generatorHandle) randomFreeze(ref *ReferenceModel) Command {
	id := h.sceneID()
	sc, _ := ref.Scene(id)
	expected := sc.Revision
	if h.chance(h.cfg.StaleRate) {
		expected = h.staleRevision(sc.Revision)
	}
	return Command{Kind: CmdFreeze, SceneID: id, Expected: expected}
}

func (h generatorHandle) randomRevoke(ref *ReferenceModel) Command {
	count := ref.SnapshotCount()
	if count == 0 || h.chance(h.cfg.MissingRate) {
		return Command{Kind: CmdRevoke, SnapshotRef: -1}
	}
	return Command{Kind: CmdRevoke, SnapshotRef: 1 + h.rng.Intn(count)}
}

// staleRevision picks a revision strictly below the current one; revision 0
// on an existing scene is itself stale.
func (h generatorHandle) staleRevision(current int) int {
	if current <= 1 {
		return 0
	}
	return h.rng.Intn(current)
}
