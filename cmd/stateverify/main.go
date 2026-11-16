// Command stateverify runs stateful model-based verification of the scene
// revision / archive / restore / freeze lifecycle. It generates random
// interleaved command sequences, replays them against the real service and
// an independent reference model, shrinks any failing sequence, and prints
// a fully reproducible counterexample.
//
// Usage:
//
//	go run ./cmd/stateverify [flags]
//
// With --fault none the campaign must report zero mismatches; the other
// --fault values inject one spec violation each and are used to demonstrate
// generation and shrinking.
package main

import (
	"atlas-render/internal/statematch"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	var (
		iterations = flag.Int("iterations", 200, "number of sequences to generate and verify")
		steps      = flag.Int("steps", 60, "commands per generated sequence")
		baseSeed   = flag.Int64("seed", 1, "first RNG seed; seeds are seed..seed+iterations-1")
		fault      = flag.String("fault", statematch.FaultNone, "injected defect: none|stale-put|archive-freeze|half-revision|phantom-event|snapshot-drift")
		workDir    = flag.String("workdir", "", "directory for temporary state files (default: OS temp dir)")
		reopen     = flag.Bool("reopen-every-step", false, "close and re-open the persisted store after every command")
		noShrink   = flag.Bool("no-shrink", false, "report the first failing sequence without reducing it")
		jsonOut    = flag.Bool("json", false, "emit machine-readable JSON instead of text")
	)
	flag.Parse()

	if !validFault(*fault) {
		fmt.Fprintf(os.Stderr, "unknown fault %q; valid values: %v\n", *fault, statematch.ValidFaults())
		os.Exit(2)
	}
	dir, e := os.MkdirTemp(*workDir, "atlas-stateverify-")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(2)
	}
	defer os.RemoveAll(dir)

	cfg := statematch.DefaultSearchConfig()
	cfg.Iterations = *iterations
	cfg.BaseSeed = *baseSeed
	cfg.Gen.Steps = *steps
	cfg.Run.ReopenEveryStep = *reopen
	cfg.Shrink = !*noShrink

	res := statematch.Search(cfg, filepath.Join(dir, "runs"), *fault)
	if *jsonOut {
		emitJSON(res)
	} else {
		emitText(res, *fault, cfg)
	}
	if res.Mismatch != nil {
		os.Exit(1)
	}
}

func validFault(f string) bool {
	for _, v := range statematch.ValidFaults() {
		if v == f {
			return true
		}
	}
	return false
}

func emitJSON(res statematch.SearchResult) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e := enc.Encode(res); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(2)
	}
}

func emitText(res statematch.SearchResult, fault string, cfg statematch.SearchConfig) {
	fmt.Printf("stateful verification: fault=%q iterations=%d steps=%d reopenEveryStep=%v\n",
		fault, res.Iterations, cfg.Gen.Steps, cfg.Run.ReopenEveryStep)
	if res.Mismatch == nil {
		fmt.Printf("PASS: %d interleaved sequences matched the reference model and all invariants held\n", res.Executed)
		return
	}
	fmt.Printf("FAIL: seed=%d violated rule %q\n", res.FailingSeed, res.Mismatch.Rule)
	fmt.Printf("  %s\n", res.Mismatch.String())
	seq := res.Sequence
	if res.Shrunk != nil {
		fmt.Printf("\nshrunk counterexample (%d -> %d commands, %d replay attempts, confirmed):\n",
			len(res.Sequence), len(res.Shrunk.Sequence), res.Shrunk.Replays)
		seq = res.Shrunk.Sequence
		fmt.Printf("  reduced mismatch: %s\n", res.Shrunk.Mismatch.String())
	} else {
		fmt.Println("\nfull failing sequence:")
	}
	fmt.Println("reproduce with: go run ./cmd/stateverify --fault", fault, "--seed", res.FailingSeed,
		"--iterations 1 --steps", cfg.Gen.Steps, flagSuffix(cfg.Run.ReopenEveryStep))
	for i, c := range seq {
		marker := "  "
		if res.Shrunk != nil && i == res.Shrunk.Mismatch.Index {
			marker = ">>"
		}
		fmt.Printf("%s %02d %s\n", marker, i, c.String())
	}
}

func flagSuffix(reopen bool) string {
	if reopen {
		return "--reopen-every-step"
	}
	return ""
}
