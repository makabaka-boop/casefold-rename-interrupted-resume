package main

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// layoutAfter replays the first k plan steps on a fresh in-memory
// case-insensitive table and returns the exact names present, sorted.
func layoutAfter(t *testing.T, m Manifest, p Plan, k int) []string {
	t.Helper()
	table := newFSTable(m.Files)
	for i := 0; i < k; i++ {
		if err := table.move(p.Steps[i].From, p.Steps[i].To); err != nil {
			t.Fatalf("setup: step %d (%+v) rejected: %v", i, p.Steps[i], err)
		}
	}
	return sortedExactNames(table.byNorm)
}

func sortedExactNames(byNorm map[string]string) []string {
	out := make([]string, 0, len(byNorm))
	for _, exact := range byNorm {
		out = append(out, exact)
	}
	sort.Strings(out)
	return out
}

// checkInterruptPoint certifies one interruption point: resume must accept the
// exact predicted layout, return the untouched plan tail as Forward and the
// inverse of the executed prefix as Rollback. Both returned sequences are then
// replayed independently from the observed layout: Forward must reach the full
// plan's final layout, Rollback must restore the original layout, and neither
// may ever move onto an occupied name.
func checkInterruptPoint(t *testing.T, m Manifest, done int) {
	t.Helper()
	p, err := plan(m)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if done > len(p.Steps) {
		t.Fatalf("bad test: done=%d > %d steps", done, len(p.Steps))
	}
	observed := layoutAfter(t, m, p, done)

	rec, diffs, err := resume(m, done, observed)
	if err != nil {
		t.Fatalf("resume(done=%d, %v): unexpected error: %v", done, observed, err)
	}
	if diffs != nil {
		t.Fatalf("resume(done=%d, %v): unexpected diffs: %+v", done, observed, diffs)
	}
	if rec.Done != done || rec.Remaining != len(p.Steps)-done {
		t.Fatalf("counts: got done=%d remaining=%d, want %d/%d", rec.Done, rec.Remaining, done, len(p.Steps)-done)
	}
	wantForward := p.Steps[done:]
	if !reflect.DeepEqual(rec.Forward, wantForward) {
		t.Fatalf("forward tail at done=%d:\n got %+v\nwant %+v", done, rec.Forward, wantForward)
	}
	wantRollback := []Step{}
	for i := done - 1; i >= 0; i-- {
		s := p.Steps[i]
		wantRollback = append(wantRollback, Step{From: s.To, To: s.From})
	}
	if !reflect.DeepEqual(rec.Rollback, wantRollback) {
		t.Fatalf("prefix rollback at done=%d:\n got %+v\nwant %+v", done, rec.Rollback, wantRollback)
	}
	// Safety: prefix rollback must never touch a step that still lies ahead.
	if len(rec.Forward)+len(rec.Rollback) != len(p.Steps) {
		t.Fatalf("forward(%d)+rollback(%d) != steps(%d)", len(rec.Forward), len(rec.Rollback), len(p.Steps))
	}

	// Continue: replay Forward from the observed state and expect the exact
	// final layout the whole plan produces.
	finalTable := newFSTable(m.Files)
	for i := range p.Steps {
		if err := finalTable.move(p.Steps[i].From, p.Steps[i].To); err != nil {
			t.Fatalf("setup full replay: %v", err)
		}
	}
	contTable := newFSTable(observed)
	for i, s := range rec.Forward {
		if err := contTable.move(s.From, s.To); err != nil {
			t.Fatalf("continue step %d (%+v) from %v rejected: %v", i, s, observed, err)
		}
	}
	if !reflect.DeepEqual(contTable.byNorm, finalTable.byNorm) {
		t.Fatalf("after continuing:\n got %v\nwant %v", contTable.byNorm, finalTable.byNorm)
	}

	// Undo: replay Rollback from the observed state and expect the original
	// layout exactly, including case.
	undoTable := newFSTable(observed)
	for i, s := range rec.Rollback {
		if err := undoTable.move(s.From, s.To); err != nil {
			t.Fatalf("undo step %d (%+v) from %v rejected: %v", i, s, observed, err)
		}
	}
	initial := newFSTable(m.Files)
	if !reflect.DeepEqual(undoTable.byNorm, initial.byNorm) {
		t.Fatalf("after undo:\n got %v\nwant %v", undoTable.byNorm, initial.byNorm)
	}
}

// walkInterruptPoints runs checkInterruptPoint at every possible cut,
// including the parked-at-temp cuts (the step right after a X->temp store).
func walkInterruptPoints(t *testing.T, m Manifest) {
	t.Helper()
	p, err := plan(m)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for done := 0; done <= len(p.Steps); done++ {
		t.Run(fmt.Sprintf("after-%d", done), func(t *testing.T) {
			checkInterruptPoint(t, m, done)
		})
	}
}

func TestResumeChainEveryInterruptPoint(t *testing.T) {
	// Chain a->b->(free c): steps b->c then a->b.
	m := Manifest{
		Files:   []string{"a", "b", "by.stander"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}},
	}
	walkInterruptPoints(t, m)

	// Mid-chain: one step done, c exists, b is free.
	p, _ := plan(m)
	rec, diffs, err := resume(m, 1, layoutAfter(t, m, p, 1))
	if err != nil || diffs != nil {
		t.Fatalf("resume: err=%v diffs=%+v", err, diffs)
	}
	if got, want := rec.Forward, []Step{{"a", "b"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("continue=%+v, want %+v", got, want)
	}
	if got, want := rec.Rollback, []Step{{"c", "b"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("undo=%+v, want %+v", got, want)
	}
}

func TestResumeLongChainEveryInterruptPoint(t *testing.T) {
	// a->b, b->c, c->d (free): three-node chain, tail first.
	m := Manifest{
		Files:   []string{"a", "b", "c"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "d"}},
	}
	walkInterruptPoints(t, m)
}

func TestResumeSwapCycleEveryInterruptPoint(t *testing.T) {
	// Steps: a->tmp, b->a, tmp->b. The cut after step 1 is the parked state.
	m := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	walkInterruptPoints(t, m)
}

func TestResumeThreeNodeCycleEveryInterruptPoint(t *testing.T) {
	// Steps: a->tmp, c->a, b->c, tmp->b.
	m := Manifest{
		Files:   []string{"a", "b", "c"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "a"}},
	}
	walkInterruptPoints(t, m)
}

func TestResumeCaseOnlyEveryInterruptPoint(t *testing.T) {
	// Steps: readme.txt->tmp, tmp->README.TXT.
	m := Manifest{
		Files:   []string{"readme.txt"},
		Renames: []Rename{{"readme.txt", "README.TXT"}},
	}
	walkInterruptPoints(t, m)
}

// TestParkedAtTempContinueAndUndo is the operator's core scenario: the
// terminal died while a file is sitting at the temp name. Verify both the
// continue tail and the safe undo for every component shape.
func TestParkedAtTempContinueAndUndo(t *testing.T) {
	cases := []struct {
		name          string
		m             Manifest
		parkedSrc     string
		afterPark     []Step // steps still ahead when parked (first must vacate a real name)
		firstLoadsTmp bool   // length-1 cycle: nothing else needs vacating
	}{
		{
			name:          "swap",
			m:             Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "a"}}},
			parkedSrc:     "a",
			afterPark:     []Step{{"b", "a"}, {"__rename_tmp_0__", "b"}},
			firstLoadsTmp: false,
		},
		{
			name:          "three-cycle",
			m:             Manifest{Files: []string{"a", "b", "c"}, Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "a"}}},
			parkedSrc:     "a",
			afterPark:     []Step{{"c", "a"}, {"b", "c"}, {"__rename_tmp_0__", "b"}},
			firstLoadsTmp: false,
		},
		{
			name:          "case-only",
			m:             Manifest{Files: []string{"readme.txt"}, Renames: []Rename{{"readme.txt", "README.TXT"}}},
			parkedSrc:     "readme.txt",
			afterPark:     []Step{{"__rename_tmp_0__", "README.TXT"}},
			firstLoadsTmp: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := plan(tc.m)
			if err != nil {
				t.Fatal(err)
			}
			tmp := ""
			for _, s := range p.Steps {
				if s.From == tc.parkedSrc && hasTmpPrefix(s.To) {
					tmp = s.To
					break
				}
			}
			if tmp == "" {
				t.Fatal("test setup: no park step found")
			}
			// done=1 for each of these plans is exactly the parked cut.
			observed := layoutAfter(t, tc.m, p, 1)
			if !contains(observed, tmp) {
				t.Fatalf("parked layout %v should hold temp %q", observed, tmp)
			}
			if contains(observed, tc.parkedSrc) {
				t.Fatalf("parked layout %v should no longer hold %q", observed, tc.parkedSrc)
			}

			rec, diffs, err := resume(tc.m, 1, observed)
			if err != nil || diffs != nil {
				t.Fatalf("resume parked: err=%v diffs=%+v", err, diffs)
			}
			// Continue: nothing else has moved, so the tail is unchanged.
			if !reflect.DeepEqual(rec.Forward, tc.afterPark) {
				t.Fatalf("continue tail=%+v, want %+v", rec.Forward, tc.afterPark)
			}
			// In a cycle of length > 1 some other node must vacate a real name
			// before the parked file is loaded. In a length-1 (case-only) cycle
			// there is nobody else, so the tail starts by loading the parked file
			// straight onto its final name -- still safe (that slot is free).
			loadsFirst := hasTmpPrefix(rec.Forward[0].From)
			if loadsFirst != tc.firstLoadsTmp {
				t.Fatalf("first continue step %+v loads temp=%v, want %v",
					rec.Forward[0], loadsFirst, tc.firstLoadsTmp)
			}
			if !tc.firstLoadsTmp && rec.Forward[0].From == tmp {
				t.Fatalf("first continue step must not load the parked file, got %+v", rec.Forward[0])
			}
			// Undo: exactly one inverse move, from the temp name back home.
			wantUndo := []Step{{From: tmp, To: tc.parkedSrc}}
			if !reflect.DeepEqual(rec.Rollback, wantUndo) {
				t.Fatalf("undo=%+v, want %+v", rec.Rollback, wantUndo)
			}
		})
	}
}

// TestParkedTempAcrossComponents parks while a *later* component runs: the
// reused temp name holds that component's minimum source, and only the steps
// of components already finished plus the park prefix are reversible.
func TestParkedTempAcrossComponents(t *testing.T) {
	// Two case-only renames (two parked cuts) plus a chain in the middle.
	m := Manifest{
		Files: []string{"a", "m", "x", "y"},
		Renames: []Rename{
			{"a", "A"},
			{"m", "M"},
			{"x", "y"}, {"y", "z"},
		},
	}
	walkInterruptPoints(t, m)

	p, _ := plan(m)
	tmp := "__rename_tmp_0__"
	// Plan order: component "a" (2), "m" (2), then chain x/y (2).
	want := []Step{
		{"a", tmp}, {tmp, "A"},
		{"m", tmp}, {tmp, "M"},
		{"y", "z"}, {"x", "y"},
	}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
	// Second park cut (done=3): "a" already finished, "m" sits at temp.
	rec, diffs, err := resume(m, 3, layoutAfter(t, m, p, 3))
	if err != nil || diffs != nil {
		t.Fatalf("resume: err=%v diffs=%+v", err, diffs)
	}
	if !reflect.DeepEqual(rec.Forward, []Step{{tmp, "M"}, {"y", "z"}, {"x", "y"}}) {
		t.Fatalf("forward=%+v", rec.Forward)
	}
	// Undo covers only executed prefix: un-park m, then reverse component a.
	wantUndo := []Step{
		{tmp, "m"},
		{"A", tmp}, {tmp, "a"},
	}
	if !reflect.DeepEqual(rec.Rollback, wantUndo) {
		t.Fatalf("rollback=%+v, want %+v", rec.Rollback, wantUndo)
	}
}

func hasTmpPrefix(s string) bool {
	const pfx = "__rename_tmp_"
	return len(s) >= len(pfx) && s[:len(pfx)] == pfx
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// expectMismatch resumes and asserts that no executable action is returned.
func expectMismatch(t *testing.T, m Manifest, done int, observed []string, wantKinds ...string) {
	t.Helper()
	rec, diffs, err := resume(m, done, observed)
	if err != nil {
		t.Fatalf("resume(done=%d, %v): unexpected error: %v", done, observed, err)
	}
	if diffs == nil {
		t.Fatalf("resume(done=%d, %v): expected mismatch, got recovery %+v", done, observed, rec)
	}
	if !reflect.DeepEqual(rec, Recovery{}) {
		t.Fatalf("mismatch must return zero recovery, got %+v", rec)
	}
	gotKinds := map[string]int{}
	for _, d := range diffs {
		gotKinds[d.Kind]++
	}
	wantK := map[string]int{}
	for _, k := range wantKinds {
		wantK[k]++
	}
	if !reflect.DeepEqual(gotKinds, wantK) {
		t.Fatalf("diffs=%+v kinds %v, want kinds %v", diffs, gotKinds, wantK)
	}
}

func TestResumeForeignFile(t *testing.T) {
	m := Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "c"}}}
	p, _ := plan(m)
	// Half done: [a, c] is predicted. A stranger appears alongside.
	observed := append(layoutAfter(t, m, p, 1), "stranger.txt")
	expectMismatch(t, m, 1, observed, diffForeign)

	// Stranger before anything ran: the plan cannot have produced it.
	expectMismatch(t, m, 0, append(append([]string{}, m.Files...), "x"), diffForeign)
}

func TestResumeMissingFile(t *testing.T) {
	m := Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "c"}}}
	p, _ := plan(m)
	// Cut 1 layout is [a, c]; drop the tail's result c. Name b is legitimately
	// free at this cut, so only c is missing.
	got := layoutAfter(t, m, p, 1)
	expectMismatch(t, m, 1, without(got, "c"), diffMissing)

	// Drop a bystander at done=0.
	m2 := Manifest{Files: []string{"a", "keep"}, Renames: []Rename{{"a", "A"}}}
	expectMismatch(t, m2, 0, []string{"a"}, diffMissing)
}

func without(xs []string, s string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func TestResumeCaseDriftOnResultAndBystander(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "Note"},
		Renames: []Rename{{"a", "B"}},
	}
	// Step a->B done; predicted exact [B, Note]. Observe lowercase b: drift.
	expectMismatch(t, m, 1, []string{"b", "Note"}, diffCaseDrift)
	// The moved file is fine, untouched Note drifted: drift.
	expectMismatch(t, m, 1, []string{"B", "note"}, diffCaseDrift)
	// Nothing ran, a itself drifted.
	expectMismatch(t, m, 0, []string{"A", "Note"}, diffCaseDrift)
}

func TestResumeParkedTempOccupancyMismatch(t *testing.T) {
	m := Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "a"}}}
	p, _ := plan(m)
	tmp := "__rename_tmp_0__"
	parked := layoutAfter(t, m, p, 1)
	if !reflect.DeepEqual(parked, []string{"__rename_tmp_0__", "b"}) {
		t.Fatalf("sanity: parked layout = %v", parked)
	}

	// Wrong temp index: that name is foreign and the expected temp slot is
	// missing -- the planner must not quietly reselect the temp name to fit.
	expectMismatch(t, m, 1, []string{"__rename_tmp_1__", "b"}, diffForeign, diffMissing)

	// Temp slot held with different case: exact-case mismatch on the temp name.
	expectMismatch(t, m, 1, []string{"__RENAME_TMP_0__", "b"}, diffCaseDrift)

	// Temp occupied while done claims zero steps executed: foreign temp, and
	// the file that would be parked ("a") missing.
	expectMismatch(t, m, 0, []string{tmp, "b"}, diffForeign, diffMissing)

	// Temp present but parked source also present (b missing instead): counts
	// cannot line up with one executed step.
	expectMismatch(t, m, 1, []string{tmp, "a"}, diffForeign, diffMissing)
}

func TestResumeWrongStepCount(t *testing.T) {
	m := Manifest{Files: []string{"a", "b", "c"}, Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "d"}}}
	p, _ := plan(m)
	// Claim one fewer / one more step than the observed layout represents.
	layout1 := layoutAfter(t, m, p, 1)
	layout2 := layoutAfter(t, m, p, 2)
	expectMismatch(t, m, 0, layout1, diffForeign, diffMissing)
	expectMismatch(t, m, 2, layout1, diffForeign, diffMissing)
	expectMismatch(t, m, 1, layout2, diffForeign, diffMissing)
	expectMismatch(t, m, 3, layout2, diffForeign, diffMissing)
}

func TestResumeCombinedForeignMissingAndDrift(t *testing.T) {
	m := Manifest{Files: []string{"a", "b", "Note"}, Renames: []Rename{{"a", "b"}, {"b", "a"}}}
	// Parked: [tmp, b, Note]. Observe a stranger, lose tmp, drift on Note.
	observed := []string{"b", "note", "intruder"}
	expectMismatch(t, m, 1, observed, diffForeign, diffMissing, diffCaseDrift)
	// Diff report is sorted and stable across calls.
	_, d1, _ := resume(m, 1, observed)
	_, d2, _ := resume(m, 1, append([]string{}, observed...))
	if !reflect.DeepEqual(d1, d2) {
		t.Fatalf("diff report non-deterministic: %+v vs %+v", d1, d2)
	}
}

func TestResumeErrors(t *testing.T) {
	m := Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "a"}}}
	p, _ := plan(m)

	if _, _, err := resume(m, -1, m.Files); err == nil {
		t.Fatal("negative done accepted")
	}
	if _, _, err := resume(m, len(p.Steps)+1, m.Files); err == nil {
		t.Fatal("done beyond plan length accepted")
	}
	// Two observed names that collide case-insensitively: the observation
	// itself cannot be a real single directory listing.
	if _, _, err := resume(m, 0, []string{"a", "A", "b"}); err == nil {
		t.Fatal("case-colliding observation accepted")
	}
	if _, _, err := resume(m, 0, []string{"a", ""}); err == nil {
		t.Fatal("empty observed name accepted")
	}
	if _, _, err := resume(m, 0, []string{"a", "café"}); err == nil {
		t.Fatal("non-ASCII observed name accepted")
	}
	// An invalid original manifest is rebuilt and rejected first.
	if _, _, err := resume(Manifest{Files: []string{"a", "A"}}, 0, []string{"a", "A"}); err == nil {
		t.Fatal("invalid manifest accepted in resume mode")
	}
}

func TestResumeBoundaries(t *testing.T) {
	// No renames: only cut 0 exists; the certified action sets are empty.
	m := Manifest{Files: []string{"a", "B"}}
	rec, diffs, err := resume(m, 0, []string{"a", "B"})
	if err != nil || diffs != nil {
		t.Fatalf("err=%v diffs=%+v", err, diffs)
	}
	if rec.Done != 0 || rec.Remaining != 0 || len(rec.Forward) != 0 || len(rec.Rollback) != 0 {
		t.Fatalf("empty-plan recovery = %+v", rec)
	}
	if _, _, err := resume(m, 1, m.Files); err == nil {
		t.Fatal("done=1 accepted for empty plan")
	}

	// Fully executed: Forward empty, Rollback is the whole plan inversed.
	m2 := Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "a"}}}
	p2, _ := plan(m2)
	final := layoutAfter(t, m2, p2, len(p2.Steps))
	rec2, diffs, err := resume(m2, len(p2.Steps), final)
	if err != nil || diffs != nil {
		t.Fatalf("err=%v diffs=%+v", err, diffs)
	}
	if len(rec2.Forward) != 0 || rec2.Remaining != 0 {
		t.Fatalf("finished plan should have no forward tail, got %+v", rec2)
	}
	if !reflect.DeepEqual(rec2.Rollback, p2.Rollback) {
		t.Fatalf("full rollback=%+v, want %+v", rec2.Rollback, p2.Rollback)
	}
}

func TestResumeOldPlanOutputUnchanged(t *testing.T) {
	// The existence of resume mode must not perturb plan(): spot-check the
	// fixed plans the CLI documents.
	cases := []struct {
		m     Manifest
		steps []Step
	}{
		{
			Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "b"}, {"b", "a"}}},
			[]Step{{"a", "__rename_tmp_0__"}, {"b", "a"}, {"__rename_tmp_0__", "b"}},
		},
		{
			Manifest{Files: []string{"readme.txt"}, Renames: []Rename{{"readme.txt", "README.TXT"}}},
			[]Step{{"readme.txt", "__rename_tmp_0__"}, {"__rename_tmp_0__", "README.TXT"}},
		},
	}
	for _, tc := range cases {
		p, err := plan(tc.m)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.Steps, tc.steps) {
			t.Fatalf("steps=%+v, want %+v", p.Steps, tc.steps)
		}
	}
}
