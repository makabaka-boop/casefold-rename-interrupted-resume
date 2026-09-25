package main

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// namesOf returns the exact file names currently in the in-memory table.
func namesOf(t *fsTable) []string {
	out := make([]string, 0, len(t.byNorm))
	for _, v := range t.byNorm {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// layoutAt replays the first k steps of p from the original files and returns
// the exact names present: the honest layout an operator would observe after
// an interrupt at step k.
func layoutAt(files []string, p Plan, k int) []string {
	table := newFSTable(files)
	for i := 0; i < k; i++ {
		if err := table.move(p.Steps[i].From, p.Steps[i].To); err != nil {
			panic(err)
		}
	}
	return namesOf(table)
}

// checkResumePoint verifies recovery at every interrupt point of one manifest:
// exact match, remaining steps equal the plan tail, reverse steps equal both
// the inverted executed prefix and the tail of the plan's rollback, the
// remaining steps continue the job to the final mapping, and the reverse
// steps restore the original layout. The observed order must not matter.
func checkResumePoint(t *testing.T, m Manifest, k int) {
	t.Helper()
	p, err := plan(m)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	observed := layoutAt(m.Files, p, k)

	r, err := resume(m, k, observed)
	if err != nil {
		t.Fatalf("resume(k=%d): %v", k, err)
	}
	if !r.Match {
		t.Fatalf("k=%d: expected match, got diffs %+v", k, r.Diffs)
	}
	if r.Done != k || r.Total != len(p.Steps) {
		t.Fatalf("k=%d: counters=%d/%d, want %d/%d", k, r.Done, r.Total, k, len(p.Steps))
	}

	wantRemaining := p.Steps[k:]
	if !reflect.DeepEqual(r.Remaining, wantRemaining) {
		t.Fatalf("k=%d: remaining=%+v, want %+v", k, r.Remaining, wantRemaining)
	}
	wantReverse := []Step{}
	for i := k - 1; i >= 0; i-- {
		s := p.Steps[i]
		wantReverse = append(wantReverse, Step{From: s.To, To: s.From})
	}
	if !reflect.DeepEqual(r.Reverse, wantReverse) {
		t.Fatalf("k=%d: reverse=%+v, want %+v", k, r.Reverse, wantReverse)
	}
	// Reverse-for-prefix must be the tail of the plan's full rollback.
	if !reflect.DeepEqual(r.Reverse, p.Rollback[len(p.Steps)-k:]) {
		t.Fatalf("k=%d: reverse %+v is not the rollback tail %+v", k, r.Reverse, p.Rollback[len(p.Steps)-k:])
	}

	// Continue: applying the remaining steps from the observed layout must
	// land exactly on the mapping's final names.
	table := newFSTable(observed)
	for i, s := range r.Remaining {
		if err := table.move(s.From, s.To); err != nil {
			t.Fatalf("k=%d: continuation step %d (%+v) rejected: %v", k, i, s, err)
		}
	}
	participants := map[string]bool{}
	for _, rn := range m.Renames {
		participants[rn.Source] = true
		if got := table.byNorm[norm(rn.Target)]; got != rn.Target {
			t.Fatalf("k=%d: after continuation %q holds %q, want exact %q", k, rn.Target, got, rn.Target)
		}
	}
	for _, f := range m.Files {
		if participants[f] {
			continue
		}
		if got := table.byNorm[norm(f)]; got != f {
			t.Fatalf("k=%d: bystander %q disturbed (now %q)", k, f, got)
		}
	}
	if len(table.byNorm) != len(m.Files) {
		t.Fatalf("k=%d: file count changed to %d", k, len(table.byNorm))
	}

	// Undo: the reverse steps from the observed layout must restore the
	// original files exactly and never move onto an occupied name.
	back := newFSTable(observed)
	for i, s := range r.Reverse {
		if err := back.move(s.From, s.To); err != nil {
			t.Fatalf("k=%d: reverse step %d (%+v) rejected: %v", k, i, s, err)
		}
	}
	for _, f := range m.Files {
		if got := back.byNorm[norm(f)]; got != f {
			t.Fatalf("k=%d: reverse left %q holding %q, want %q", k, f, got, f)
		}
	}
	if len(back.byNorm) != len(m.Files) {
		t.Fatalf("k=%d: reverse file count %d, want %d", k, len(back.byNorm), len(m.Files))
	}

	// Observation is an unordered set and recovery must be deterministic.
	shuffled := make([]string, len(observed))
	copy(shuffled, observed)
	for i := len(shuffled) - 1; i > 0; i-- {
		j := (i*7 + 1) % (i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	r2, err := resume(m, k, shuffled)
	if err != nil {
		t.Fatalf("resume shuffled(k=%d): %v", k, err)
	}
	if !reflect.DeepEqual(r, r2) {
		t.Fatalf("k=%d: recovery depends on observation order: %+v != %+v", k, r, r2)
	}
}

func walkInterruptPoints(t *testing.T, name string, m Manifest) {
	t.Helper()
	p, err := plan(m)
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	for k := 0; k <= len(p.Steps); k++ {
		t.Run(fmt.Sprintf("%s/after_%d_of_%d", name, k, len(p.Steps)), func(t *testing.T) {
			checkResumePoint(t, m, k)
		})
	}
}

// Chains: tail-first, no temp name at any point.
func TestResumeChain(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "b", "c", "z"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "d"}},
	}
	walkInterruptPoints(t, "chain3", m)

	two := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}},
	}
	walkInterruptPoints(t, "chain2", two)
}

// Cycles: the head sits at the temp name for one or more interrupt points.
func TestResumeCycles(t *testing.T) {
	swap := Manifest{
		Files:   []string{"a", "b", "z"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	walkInterruptPoints(t, "swap2", swap)

	three := Manifest{
		Files:   []string{"a", "b", "c"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "a"}},
	}
	walkInterruptPoints(t, "cycle3", three)
}

// Case-only rename is a length-1 cycle: the only middle point has the file
// parked at the temp name — both resume and withdrawal must work from there.
func TestResumeCaseOnly(t *testing.T) {
	m := Manifest{
		Files:   []string{"readme.txt"},
		Renames: []Rename{{"readme.txt", "README.TXT"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	if !reflect.DeepEqual(p.Steps, []Step{{"readme.txt", tmp}, {tmp, "README.TXT"}}) {
		t.Fatalf("precondition changed: %+v", p.Steps)
	}
	walkInterruptPoints(t, "case-only", m)

	// Pin the critical observation: interrupted right after parking, the disk
	// shows just the temp name, nothing at either case variant.
	r, err := resume(m, 1, []string{tmp})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Match {
		t.Fatalf("diffs at park point: %+v", r.Diffs)
	}
	if !reflect.DeepEqual(r.Remaining, []Step{{tmp, "README.TXT"}}) {
		t.Fatalf("continue from temp: %+v", r.Remaining)
	}
	if !reflect.DeepEqual(r.Reverse, []Step{{tmp, "readme.txt"}}) {
		t.Fatalf("withdraw from temp: %+v", r.Reverse)
	}
}

// A batch mixing both component kinds, temp name reused between them.
func TestResumeMixedComponents(t *testing.T) {
	m := Manifest{
		Files: []string{"a", "m", "n", "z"},
		Renames: []Rename{
			{"a", "A"},
			{"m", "n"}, {"n", "m"},
		},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	want := []Step{
		{"a", tmp}, {tmp, "A"},
		{"m", tmp}, {"n", "m"}, {tmp, "n"},
	}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("precondition changed: %+v", p.Steps)
	}
	walkInterruptPoints(t, "mixed", m)
}

// Endpoints: k=0 hands back the untouched plan; k=total hands back a reverse
// path identical to the original rollback and no forward work.
func TestResumeEndpoints(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}

	none, err := resume(m, 0, append([]string{}, m.Files...))
	if err != nil {
		t.Fatal(err)
	}
	if !none.Match || !reflect.DeepEqual(none.Remaining, p.Steps) || len(none.Reverse) != 0 {
		t.Fatalf("k=0 recovery wrong: %+v", none)
	}

	final := layoutAt(m.Files, p, len(p.Steps))
	all, err := resume(m, len(p.Steps), final)
	if err != nil {
		t.Fatal(err)
	}
	if !all.Match || len(all.Remaining) != 0 || !reflect.DeepEqual(all.Reverse, p.Rollback) {
		t.Fatalf("k=total recovery wrong: %+v", all)
	}
}

// At the park point of a case-only rename, every kind of foreign/absent/case
// discrepancy — including a foreign temp-style name — must be reported and no
// executable actions may be returned.
func TestResumeMismatchAtTempPark(t *testing.T) {
	m := Manifest{
		Files:   []string{"readme.txt"},
		Renames: []Rename{{"readme.txt", "README.TXT"}},
	}
	tmp := "__rename_tmp_0__"

	t.Run("foreign file", func(t *testing.T) {
		got := resumeMismatch(t, m, 1, []string{tmp}, func(names map[string]string) {
			names["intruder.log"] = "intruder.log"
		})
		assertKinds(t, got, map[string]int{diffForeign: 1})
		if got.Diffs[0].Temp {
			t.Fatalf("intruder must not be flagged as temp-slot: %+v", got.Diffs[0])
		}
	})

	t.Run("missing parked file", func(t *testing.T) {
		got := resumeMismatch(t, m, 1, []string{tmp}, func(names map[string]string) {
			delete(names, tmp)
		})
		assertKinds(t, got, map[string]int{diffMissing: 1})
		if !got.Diffs[0].Temp || got.Diffs[0].Expected != tmp {
			t.Fatalf("want missing temp slot %q, got %+v", tmp, got.Diffs[0])
		}
	})

	t.Run("case drift on temp name", func(t *testing.T) {
		got := resumeMismatch(t, m, 1, []string{tmp}, func(names map[string]string) {
			delete(names, tmp)
			names["__RENAME_TMP_0__"] = "__RENAME_TMP_0__"
		})
		assertKinds(t, got, map[string]int{diffCaseDrift: 1})
		d := got.Diffs[0]
		if !d.Temp || d.Expected != tmp || d.Observed != "__RENAME_TMP_0__" {
			t.Fatalf("want case-drift at temp slot, got %+v", d)
		}
	})

	t.Run("wrong temp index occupied", func(t *testing.T) {
		// The operator picked (or the drive shows) a different temp slot than
		// the rebuilt plan uses: missing temp0 + foreign temp1, both flagged.
		got := resumeMismatch(t, m, 1, []string{tmp}, func(names map[string]string) {
			delete(names, tmp)
			names["__rename_tmp_1__"] = "__rename_tmp_1__"
		})
		assertKinds(t, got, map[string]int{diffMissing: 1, diffForeign: 1})
		for _, d := range got.Diffs {
			if !d.Temp {
				t.Fatalf("both rows must touch temp slots, got %+v", d)
			}
		}
	})

	t.Run("temp parked while original name also present", func(t *testing.T) {
		// Operator claims one step done and the parked copy is at tmp, but the
		// source name is also occupied (a copy or a different file): foreign.
		got := resumeMismatch(t, m, 1, []string{tmp}, func(names map[string]string) {
			names["readme.txt"] = "readme.txt"
		})
		assertKinds(t, got, map[string]int{diffForeign: 1})
		if d := got.Diffs[0]; d.Norm != "readme.txt" || d.Temp {
			t.Fatalf("want non-temp foreign row for readme.txt, got %+v", d)
		}
	})
}

// Discrepancies at non-temp interrupt points: chains and a mid-cycle step.
func TestResumeMismatchGeneral(t *testing.T) {
	chain := Manifest{
		Files:   []string{"a", "b", "z"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}},
	}
	// After step 1 (b->c): expected exact names {a, c, z}.
	t.Run("chain foreign", func(t *testing.T) {
		got := resumeMismatch(t, chain, 1, []string{"a", "c", "z"}, func(names map[string]string) {
			names["new"] = "new"
		})
		assertKinds(t, got, map[string]int{diffForeign: 1})
	})
	t.Run("chain missing", func(t *testing.T) {
		got := resumeMismatch(t, chain, 1, []string{"a", "c", "z"}, func(names map[string]string) {
			delete(names, "c")
		})
		assertKinds(t, got, map[string]int{diffMissing: 1})
	})
	t.Run("chain bystander case drift", func(t *testing.T) {
		got := resumeMismatch(t, chain, 1, []string{"a", "c", "z"}, func(names map[string]string) {
			delete(names, "z")
			names["Z"] = "Z"
		})
		assertKinds(t, got, map[string]int{diffCaseDrift: 1})
		if d := got.Diffs[0]; d.Expected != "z" || d.Observed != "Z" {
			t.Fatalf("bystander drift row wrong: %+v", d)
		}
	})

	swap := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	tmp := "__rename_tmp_0__"
	// After steps 0..1 (a->tmp, b->a): expected {a, tmp}; the parked file is
	// still at tmp, while "a" holds b's content under its final case already.
	t.Run("mid-cycle case drift", func(t *testing.T) {
		got := resumeMismatch(t, swap, 2, []string{"a", tmp}, func(names map[string]string) {
			delete(names, "a")
			names["A"] = "A"
		})
		assertKinds(t, got, map[string]int{diffCaseDrift: 1})
		if d := got.Diffs[0]; d.Temp || d.Expected != "a" || d.Observed != "A" {
			t.Fatalf("non-temp drift row wrong: %+v", d)
		}
	})
	t.Run("mid-cycle missing", func(t *testing.T) {
		got := resumeMismatch(t, swap, 2, []string{"a", tmp}, func(names map[string]string) {
			delete(names, tmp)
		})
		assertKinds(t, got, map[string]int{diffMissing: 1})
		if !got.Diffs[0].Temp {
			t.Fatalf("missing parked file must flag temp slot: %+v", got.Diffs[0])
		}
	})
}

// resumeMismatch applies a mutation to the exact-observed set and asserts that
// recovery refuses to emit actions.
func resumeMismatch(t *testing.T, m Manifest, done int, observed []string, mutate func(map[string]string)) ResumePlan {
	t.Helper()
	names := map[string]string{}
	for _, f := range observed {
		names[f] = f
	}
	mutate(names)
	seen := make([]string, 0, len(names))
	for _, f := range names {
		seen = append(seen, f)
	}
	r, err := resume(m, done, seen)
	if err != nil {
		t.Fatalf("resume errored instead of reporting diff: %v", err)
	}
	if r.Match || len(r.Diffs) == 0 {
		t.Fatalf("expected mismatch, got %+v", r)
	}
	if r.Remaining != nil || r.Reverse != nil {
		t.Fatalf("mismatch must not emit actions, got remaining=%+v reverse=%+v", r.Remaining, r.Reverse)
	}
	return r
}

func assertKinds(t *testing.T, r ResumePlan, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for _, d := range r.Diffs {
		got[d.Kind]++
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff kinds=%v, want %v (diffs=%+v)", got, want, r.Diffs)
	}
}

func TestResumeInputErrors(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := resume(m, -1, m.Files); err == nil {
		t.Fatal("negative done accepted")
	}
	if _, err := resume(m, len(p.Steps)+1, m.Files); err == nil {
		t.Fatal("done beyond plan length accepted")
	}
	if _, err := resume(m, 0, []string{"a", "A", "b"}); err == nil {
		t.Fatal("case-colliding observations accepted")
	}
	if _, err := resume(m, 0, []string{"a", "b", ""}); err == nil {
		t.Fatal("empty observed name accepted")
	}

	bad := Manifest{Files: []string{"a", "A"}}
	if _, err := resume(bad, 0, nil); err == nil {
		t.Fatal("invalid original manifest accepted")
	}
}

// Recovery must rebuild the same plan the old planner emits, and the old
// planner's output for the manifest shapes recovery relies on must not shift.
func TestRecoveryUsesRebuiltPlan(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	wantSteps := []Step{{"a", tmp}, {"b", "a"}, {tmp, "b"}}
	if !reflect.DeepEqual(p.Steps, wantSteps) {
		t.Fatalf("old planner output changed: %+v", p.Steps)
	}
	wantRollback := []Step{{"b", tmp}, {"a", "b"}, {tmp, "a"}}
	if !reflect.DeepEqual(p.Rollback, wantRollback) {
		t.Fatalf("old rollback changed: %+v", p.Rollback)
	}

	// Resuming from the temp park point must name exactly the rebuilt temp.
	r, err := resume(m, 1, []string{"b", tmp})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Reverse, []Step{{tmp, "a"}}) {
		t.Fatalf("reverse must reference rebuilt temp name: %+v", r.Reverse)
	}

	// Recovery is itself deterministic.
	r2, err := resume(m, 1, []string{tmp, "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, r2) {
		t.Fatalf("non-deterministic recovery: %+v != %+v", r, r2)
	}
}
