package main

import (
	"fmt"
	"sort"
	"strings"
)

// ResumeRequest is the interrupt-recovery input: the original manifest, the
// number of forward steps that had executed when the terminal died, and the
// exact file names observed on the drive at that moment.
type ResumeRequest struct {
	Files    []string `json:"files"`
	Renames  []Rename `json:"renames"`
	Done     int      `json:"done"`
	Observed []string `json:"observed"`
}

// Diff kinds reported when the observed layout does not match the layout that
// replaying the executed prefix must have produced.
const (
	diffMissing   = "missing"    // expected name absent from the drive
	diffForeign   = "foreign"    // observed name the plan never put there
	diffCaseDrift = "case-drift" // same case-folded slot, different exact case
)

// ResumeDiff is one discrepancy. Norm is the case-folded slot. Expected is set
// for missing/case-drift; Observed is set for foreign/case-drift. Temp marks
// the row touching the rebuilt plan's temp slot (a temp-name occupancy error).
type ResumeDiff struct {
	Norm     string `json:"norm"`
	Kind     string `json:"kind"`
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
	Temp     bool   `json:"temp,omitempty"`
}

// ResumePlan is the recovery output. When Match is false, Remaining and
// Reverse are omitted: no executable actions are emitted, only Diffs.
type ResumePlan struct {
	Match     bool         `json:"match"`
	Done      int          `json:"done"`
	Total     int          `json:"total"`
	Remaining []Step       `json:"remaining,omitempty"`
	Reverse   []Step       `json:"reverse,omitempty"`
	Diffs     []ResumeDiff `json:"diffs,omitempty"`
}

// resume rebuilds the deterministic plan for the original manifest, replays
// the first done steps on an in-memory case-insensitive occupancy table, and
// verifies that observed matches that expected layout both in normalized
// occupancy and in exact casing. On a match it returns the unexecuted forward
// steps plus reverse moves for the executed prefix only. On any discrepancy
// (foreign file, missing file, case drift, temp-slot mismatch) it returns the
// diff report with no executable actions. It never touches the file system
// and never reselects a temp name: the temp name comes from the rebuilt plan.
func resume(m Manifest, done int, observed []string) (ResumePlan, error) {
	// Rebuild the very same plan the operator started with.
	p, err := plan(m)
	if err != nil {
		return ResumePlan{}, fmt.Errorf("rebuild plan: %w", err)
	}
	if done < 0 || done > len(p.Steps) {
		return ResumePlan{}, fmt.Errorf("done must be in 0..%d, got %d", len(p.Steps), done)
	}

	// Observed names are a set of exact names; reject ambiguous input.
	obs := make(map[string]string, len(observed))
	for _, f := range observed {
		if f == "" {
			return ResumePlan{}, fmt.Errorf("observed contains an empty file name")
		}
		n := norm(f)
		if prev, dup := obs[n]; dup {
			return ResumePlan{}, fmt.Errorf("observed names %q and %q collide on a case-insensitive drive", prev, f)
		}
		obs[n] = f
	}

	// Replay the executed prefix purely in memory: normalized slot -> exact name.
	expected := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		expected[norm(f)] = f
	}
	for i := 0; i < done; i++ {
		s := p.Steps[i]
		nf, nt := norm(s.From), norm(s.To)
		cur, ok := expected[nf]
		if !ok || cur != s.From {
			// Unreachable for a plan produced by plan: every step's source was
			// present with that exact name when the step was emitted.
			return ResumePlan{}, fmt.Errorf("replay step %d %+v: source not present with that exact name", i, s)
		}
		if holder, busy := expected[nt]; busy {
			return ResumePlan{}, fmt.Errorf("replay step %d %+v: target already occupied by %q", i, s, holder)
		}
		delete(expected, nf)
		expected[nt] = s.To
	}

	// Identify the rebuilt plan's temp slot (a move destination that is no
	// rename target) purely from the plan, never by choosing a fresh name.
	targetNorm := make(map[string]bool, len(m.Renames))
	for _, r := range m.Renames {
		targetNorm[norm(r.Target)] = true
	}
	tempNorm := ""
	for _, s := range p.Steps {
		if !targetNorm[norm(s.To)] {
			tempNorm = norm(s.To)
			break
		}
	}

	// A diff is temp-related when it touches the rebuilt plan's temp slot or a
	// foreign name shaped like the planner's temp names (e.g. the wrong index),
	// so the operator can spot a temp-name occupancy error at a glance.
	tempRow := func(n string) bool {
		return tempNorm != "" && (n == tempNorm || isTempStyle(n))
	}

	// Compare occupancy and exact casing.
	diffs := []ResumeDiff{}
	for n, exact := range expected {
		got, ok := obs[n]
		switch {
		case !ok:
			diffs = append(diffs, ResumeDiff{
				Norm: n, Kind: diffMissing, Expected: exact, Temp: tempRow(n),
			})
		case got != exact:
			diffs = append(diffs, ResumeDiff{
				Norm: n, Kind: diffCaseDrift, Expected: exact, Observed: got, Temp: tempRow(n),
			})
		}
	}
	for n, exact := range obs {
		if _, ok := expected[n]; !ok {
			diffs = append(diffs, ResumeDiff{
				Norm: n, Kind: diffForeign, Observed: exact, Temp: tempRow(n),
			})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Norm < diffs[j].Norm })

	if len(diffs) > 0 {
		return ResumePlan{Match: false, Done: done, Total: len(p.Steps), Diffs: diffs}, nil
	}

	remaining := make([]Step, 0, len(p.Steps)-done)
	remaining = append(remaining, p.Steps[done:]...)
	reverse := make([]Step, 0, done)
	for i := done - 1; i >= 0; i-- {
		s := p.Steps[i]
		reverse = append(reverse, Step{From: s.To, To: s.From})
	}
	return ResumePlan{Match: true, Done: done, Total: len(p.Steps), Remaining: remaining, Reverse: reverse}, nil
}

// isTempStyle reports whether n matches the planner's temp-name pattern
// __rename_tmp_<digits>__ (case-folded).
func isTempStyle(n string) bool {
	const prefix, suffix = "__rename_tmp_", "__"
	if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
		return false
	}
	mid := n[len(prefix) : len(n)-len(suffix)]
	if mid == "" {
		return false
	}
	for i := 0; i < len(mid); i++ {
		if mid[i] < '0' || mid[i] > '9' {
			return false
		}
	}
	return true
}
