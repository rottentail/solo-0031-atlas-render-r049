package statematch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// MaxShrinkReplays bounds the work spent reducing one failure so shrinking
// always terminates even when deletions are repeatedly rejected.
const MaxShrinkReplays = 4000

var replayCounter atomic.Int64

// ShrinkResult is the reduced failing sequence.
type ShrinkResult struct {
	Sequence []Command `json:"sequence"`
	Mismatch Mismatch  `json:"mismatch"`
	Replays  int       `json:"replays"`
	Budget   bool      `json:"budgetReached"` // true when the replay budget was reached before a fixed point
}

// Shrink reduces a failing sequence while preserving the violated rule.
// Replay uses a fresh state directory per attempt so attempts never share
// state. Reduction alternates:
//   - effective-prefix reduction (commands after the failure are irrelevant),
//   - greedy single-command deletion,
//   - delta-debugging chunk removal (ddmin),
//   - parameter simplification (revision numbers, doc indexes, targets).
//
// Every accepted deletion strictly shortens the sequence; every accepted
// parameter change strictly decreases a well-founded complexity measure. The
// loop therefore reaches a fixed point, and MaxShrinkReplays is a hard cap.
func Shrink(seq []Command, first Mismatch, baseDir string, factory ExecutorFactory, opts RunOptions) ShrinkResult {
	if e := os.MkdirAll(baseDir, 0o700); e != nil {
		return ShrinkResult{Sequence: seq, Mismatch: first}
	}
	res := ShrinkResult{Sequence: append([]Command(nil), seq...), Mismatch: first}

	// Only the prefix up to and including the failing step matters.
	if first.Index >= 0 && first.Index+1 < len(res.Sequence) {
		res.Sequence = res.Sequence[:first.Index+1]
	}

	for res.Replays < MaxShrinkReplays {
		progress := false

		// 1. Remove one command at a time (strictly shorter only).
		for i := 0; i < len(res.Sequence); i++ {
			cand := append(append([]Command(nil), res.Sequence[:i]...), res.Sequence[i+1:]...)
			mm, n := replayFails(cand, baseDir, factory, opts, res.Mismatch.Rule)
			res.Replays += n
			if mm != nil {
				res.Sequence = cand
				res.Mismatch = *mm
				progress = true
				break
			}
			if res.Replays >= MaxShrinkReplays {
				break
			}
		}
		if progress {
			continue
		}

		// 2. ddmin chunk removal (strictly shorter only).
		cand, mm, n := ddmin(res.Sequence, baseDir, factory, opts, res.Mismatch.Rule, MaxShrinkReplays-res.Replays)
		res.Replays += n
		if mm != nil && len(cand) < len(res.Sequence) {
			res.Sequence = cand
			res.Mismatch = *mm
			continue
		}

		// 3. One sweep of parameter simplification. Each acceptance lowers
		//    the command's complexity measure, so the sweep cannot cycle.
		for i := range res.Sequence {
			cand, mm, n := simplifyAt(res.Sequence, i, baseDir, factory, opts, res.Mismatch.Rule)
			if mm != nil {
				res.Sequence = cand
				res.Mismatch = *mm
				progress = true
			}
			res.Replays += n
			if res.Replays >= MaxShrinkReplays {
				break
			}
		}
		if !progress {
			break
		}
	}
	res.Budget = res.Replays >= MaxShrinkReplays
	return res
}

// replayFails replays seq and returns the mismatch only if it violates wantRule.
func replayFails(seq []Command, baseDir string, factory ExecutorFactory, opts RunOptions, wantRule string) (*Mismatch, int) {
	dir := filepath.Join(baseDir, fmt.Sprintf("attempt-%d", replayCounter.Add(1)))
	mm, _ := Run(seq, filepath.Join(dir, "state.json"), factory, opts)
	if mm == nil || mm.Rule != wantRule {
		return nil, 1
	}
	return mm, 1
}

// ddmin implements the classic delta-debugging loop with 2..n granularity.
// Only strictly shorter candidates that still fail are accepted.
func ddmin(seq []Command, baseDir string, factory ExecutorFactory, opts RunOptions, wantRule string, budget int) ([]Command, *Mismatch, int) {
	replays := 0
	current := append([]Command(nil), seq...)
	n := 2
	for n <= len(current) && replays < budget {
		chunk := (len(current) + n - 1) / n
		found := false
		for start := 0; start < len(current) && replays < budget; start += chunk {
			end := start + chunk
			if end > len(current) {
				end = len(current)
			}
			cand := append(append([]Command(nil), current[:start]...), current[end:]...)
			mm, n2 := replayFails(cand, baseDir, factory, opts, wantRule)
			replays += n2
			if mm != nil {
				current = cand
				n = 2
				found = true
				break
			}
		}
		if found {
			continue
		}
		if n >= len(current) {
			break
		}
		n = min(n*2, len(current))
	}
	if len(current) >= len(seq) {
		return current, nil, replays
	}
	mm, n2 := replayFails(current, baseDir, factory, opts, wantRule)
	replays += n2
	return current, mm, replays
}

// simplifyAt tries cheaper parameter values for the command at index i.
// Accepted changes strictly reduce the complexity measure (Expected -> 0,
// DocIndex -> 0/-1, SnapshotRef -> 1, BadName -> false).
func simplifyAt(seq []Command, idx int, baseDir string, factory ExecutorFactory, opts RunOptions, wantRule string) ([]Command, *Mismatch, int) {
	replays := 0
	base := seq[idx]
	candidates := []Command{}
	c := base
	if c.Expected > 0 {
		c.Expected = 0
		candidates = append(candidates, c)
		c = base
	}
	if c.DocIndex > 0 {
		c.DocIndex = 0
		candidates = append(candidates, c)
		c = base
	}
	if c.DocIndex < 0 && c.BadName {
		c.BadName = false
		candidates = append(candidates, c)
		c = base
	}
	if c.Kind == CmdRevoke && c.SnapshotRef > 1 {
		c.SnapshotRef = 1
		candidates = append(candidates, c)
	}
	for _, candCmd := range candidates {
		cand := append([]Command(nil), seq...)
		cand[idx] = candCmd
		mm, n := replayFails(cand, baseDir, factory, opts, wantRule)
		replays += n
		if mm != nil {
			return cand, mm, replays
		}
	}
	return nil, nil, replays
}
