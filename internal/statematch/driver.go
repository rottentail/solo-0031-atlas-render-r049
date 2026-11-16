package statematch

import (
	"fmt"
	"os"
	"path/filepath"
)

// SearchConfig controls a verification campaign.
type SearchConfig struct {
	Iterations int
	BaseSeed   int64
	Gen        GenConfig
	Run        RunOptions
	Shrink     bool
}

// DefaultSearchConfig returns modest, fast defaults.
func DefaultSearchConfig() SearchConfig {
	return SearchConfig{
		Iterations: 200,
		BaseSeed:   1,
		Gen:        DefaultGenConfig(),
		Run:        RunOptions{ReopenEveryStep: false},
		Shrink:     true,
	}
}

// SearchResult reports one campaign.
type SearchResult struct {
	FailingSeed int64         `json:"failingSeed,omitempty"`
	Sequence    []Command     `json:"sequence,omitempty"`
	Mismatch    *Mismatch     `json:"mismatch,omitempty"`
	Shrunk      *ShrinkResult `json:"shrunk,omitempty"`
	Iterations  int           `json:"iterations"`
	Executed    int           `json:"executed"`
}

// FactoryFor returns the ExecutorFactory paired with an independent
// reference model. The fault argument selects an injected defect; use
// FaultNone to verify production behavior.
func FactoryFor(fault string) ExecutorFactory {
	return func(statePath string) (Executor, *ReferenceModel, error) {
		docs := ValidDocuments()
		real, e := NewRealExecutor(statePath, docs)
		if e != nil {
			return nil, nil, e
		}
		var exec Executor = real
		if fault != FaultNone {
			exec = NewFaultExecutor(real, fault)
		}
		ref := NewReferenceModel(real.IDMap(), docs)
		return exec, ref, nil
	}
}

// Search runs iterations of generate/verify and, on the first failure,
// optionally shrinks the sequence and replays the reduced form to confirm.
func Search(cfg SearchConfig, workDir, fault string) SearchResult {
	factory := FactoryFor(fault)
	res := SearchResult{Iterations: cfg.Iterations}
	for i := 0; i < cfg.Iterations; i++ {
		seed := cfg.BaseSeed + int64(i)
		seq := Generate(seed, cfg.Gen)
		dir := filepath.Join(workDir, fmt.Sprintf("seed-%d", seed))
		if e := os.MkdirAll(dir, 0o700); e != nil {
			res.Mismatch = &Mismatch{Index: -1, Rule: RulePersistence, Detail: e.Error()}
			return res
		}
		res.Executed++
		mm, _ := Run(seq, filepath.Join(dir, "state.json"), factory, cfg.Run)
		if mm == nil {
			continue
		}
		res.FailingSeed = seed
		res.Sequence = seq
		res.Mismatch = mm
		if cfg.Shrink {
			shrinkDir := filepath.Join(dir, "shrink")
			if e := os.MkdirAll(shrinkDir, 0o700); e == nil {
				shrunk := Shrink(seq, *mm, shrinkDir, factory, cfg.Run)
				// Confirm the reduced sequence still reproduces the same rule.
				if confirm, _ := Run(shrunk.Sequence, filepath.Join(shrinkDir, "confirm.json"), factory, cfg.Run); confirm != nil && confirm.Rule == shrunk.Mismatch.Rule {
					res.Shrunk = &shrunk
				}
			}
		}
		return res
	}
	return res
}
