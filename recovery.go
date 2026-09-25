package main

import (
	"fmt"
	"sort"
)

// occupancyTable is the in-memory model of the case-insensitive shared drive
// used during recovery. Each normalized name maps to the exact name stored
// there; a move refuses both a missing/exact-mismatched source and an occupied
// target, matching the drive's overwrite hazard.
type occupancyTable struct {
	byNorm map[string]string
}

func newOccupancyTable(files []string) *occupancyTable {
	t := &occupancyTable{byNorm: make(map[string]string, len(files))}
	for _, f := range files {
		t.byNorm[norm(f)] = f
	}
	return t
}

func (t *occupancyTable) move(from, to string) error {
	nf, nt := norm(from), norm(to)
	cur, ok := t.byNorm[nf]
	if !ok || cur != from {
		return fmt.Errorf("move %q -> %q: source not present with that exact name", from, to)
	}
	if holder, taken := t.byNorm[nt]; taken {
		return fmt.Errorf("move %q -> %q: target already occupied by %q", from, to, holder)
	}
	delete(t.byNorm, nf)
	t.byNorm[nt] = to
	return nil
}

// Recovery is the resume-mode output after the observed layout has been
// certified as exactly the state the deterministic plan predicts after the
// same number of completed steps. Forward is the untouched tail of the plan;
// Rollback reverses only the executed prefix. Neither sequence ever moves onto
// an occupied name. The recovery itself never touches the file system.
type Recovery struct {
	Done      int    `json:"done"`
	Remaining int    `json:"remaining"`
	Forward   []Step `json:"forward"`
	Rollback  []Step `json:"rollback"`
}

// Diff kinds layoutDiffs can report.
const (
	diffForeign   = "foreign"    // observed name that the prefix cannot produce
	diffMissing   = "missing"    // expected prefix name not observed
	diffCaseDrift = "case-drift" // same normalized name, different exact case
)

// Diff names one layout discrepancy between observation and the replayed
// prefix. Expected is the exact name the planner predicts (empty for foreign
// files); Observed is the exact name on disk (empty for missing names).
type Diff struct {
	Kind     string `json:"kind"`
	Norm     string `json:"name"` // normalized name the two layouts disagree on
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
}

// resume rebuilds the very same deterministic plan for m, replays its first
// done steps on an in-memory case-insensitive occupancy table, and certifies
// that observed matches the predicted layout both by normalized occupancy and
// by exact case. Only then does it return the remaining forward steps and the
// rollback of the executed prefix. On any discrepancy (foreign file, missing
// name, case drift, or a temp-name occupant the prefix would not hold) it
// reports the differences and returns no executable actions. The temp name is
// always the one the rebuilt plan chose; it is never reselected to fit the
// observation.
func resume(m Manifest, done int, observed []string) (Recovery, []Diff, error) {
	p, err := plan(m)
	if err != nil {
		return Recovery{}, nil, err
	}
	if done < 0 || done > len(p.Steps) {
		return Recovery{}, nil, fmt.Errorf("completed steps %d out of range for plan of %d steps", done, len(p.Steps))
	}

	obs := make(map[string]string, len(observed)) // normalized -> exact
	for _, f := range observed {
		if !isASCIIName(f) {
			return Recovery{}, nil, fmt.Errorf("observed name %q is not printable ASCII", f)
		}
		n := norm(f)
		if prev, dup := obs[n]; dup {
			return Recovery{}, nil, fmt.Errorf("observed names %q and %q collide on a case-insensitive drive", prev, f)
		}
		obs[n] = f
	}

	// Rebuild the prefix purely in memory: the initial files and the moves the
	// planner claims were executed. Reusing the same refusal-to-overwrite
	// discipline means an inconsistent prefix cannot slip through.
	table := newOccupancyTable(m.Files)
	for i := 0; i < done; i++ {
		if err := table.move(p.Steps[i].From, p.Steps[i].To); err != nil {
			return Recovery{}, nil, fmt.Errorf("prefix step %d cannot have executed: %w", i, err)
		}
	}

	var diffs []Diff
	for n, want := range table.byNorm {
		got, ok := obs[n]
		switch {
		case !ok:
			diffs = append(diffs, Diff{Kind: diffMissing, Norm: n, Expected: want})
		case got != want:
			diffs = append(diffs, Diff{Kind: diffCaseDrift, Norm: n, Expected: want, Observed: got})
		}
	}
	for n, got := range obs {
		if _, expected := table.byNorm[n]; !expected {
			diffs = append(diffs, Diff{Kind: diffForeign, Norm: n, Observed: got})
		}
	}
	if len(diffs) != 0 {
		sortLayoutDiffs(diffs)
		return Recovery{}, diffs, nil
	}

	forward := p.Steps[done:]
	// Rollback inverts exactly the executed prefix, undone last-first.
	rollback := make([]Step, 0, done)
	for i := done - 1; i >= 0; i-- {
		s := p.Steps[i]
		rollback = append(rollback, Step{From: s.To, To: s.From})
	}
	return Recovery{
		Done:      done,
		Remaining: len(forward),
		Forward:   forward,
		Rollback:  rollback,
	}, nil, nil
}

// sortLayoutDiffs gives diff reports a deterministic order: by normalized
// name, then by kind.
func sortLayoutDiffs(d []Diff) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Norm != d[j].Norm {
			return d[i].Norm < d[j].Norm
		}
		return d[i].Kind < d[j].Kind
	})
}
