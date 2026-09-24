package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fsTable is an in-memory model of the case-insensitive shared drive. It maps
// each normalized name to the exact name currently stored there. Every move
// is refused when its target is already occupied, which is exactly the
// overwriting hazard the planner must avoid.
type fsTable struct {
	byNorm map[string]string
}

func newFSTable(files []string) *fsTable {
	t := &fsTable{byNorm: map[string]string{}}
	for _, f := range files {
		t.byNorm[norm(f)] = f
	}
	return t
}

func (t *fsTable) move(from, to string) error {
	nf, nt := norm(from), norm(to)
	if cur, ok := t.byNorm[nf]; !ok || cur != from {
		return fmt.Errorf("move %q -> %q: source not present with that exact name", from, to)
	}
	if cur, taken := t.byNorm[nt]; taken {
		return fmt.Errorf("move %q -> %q: target already occupied by %q", from, to, cur)
	}
	delete(t.byNorm, nf)
	t.byNorm[nt] = to
	return nil
}

// validManifest independently restates the manifest rules, without calling
// plan, so the exhaustive test can cross-check planner acceptance/rejection.
func validManifest(m Manifest) bool {
	if len(m.Files) == 0 || len(m.Files) > maxFiles {
		return false
	}
	owner := map[string]string{}
	for _, f := range m.Files {
		if !isASCIIName(f) {
			return false
		}
		if _, dup := owner[norm(f)]; dup {
			return false
		}
		owner[norm(f)] = f
	}
	sources := map[string]bool{}
	targets := map[string]bool{}
	for _, r := range m.Renames {
		if !isASCIIName(r.Source) || !isASCIIName(r.Target) {
			return false
		}
		if r.Source == r.Target {
			return false
		}
		if owner[norm(r.Source)] != r.Source { // unknown or case-mismatched source
			return false
		}
		if sources[r.Source] {
			return false
		}
		if targets[norm(r.Target)] {
			return false
		}
		sources[r.Source] = true
		targets[norm(r.Target)] = true
	}
	for _, r := range m.Renames {
		if cur, occupied := owner[norm(r.Target)]; occupied && !sources[cur] {
			return false
		}
	}
	return true
}

// countCycles independently counts cycle components (a self loop counts as
// one: it is a case-only rename).
func countCycles(next map[string]string) int {
	state := map[string]int{}
	cycles := 0
	for s := range next {
		if state[s] != 0 {
			continue
		}
		cur := s
		for cur != "" && state[cur] == 0 {
			state[cur] = 1
			cur = next[cur]
		}
		if cur != "" && state[cur] == 1 {
			cycles++
		}
		cur = s
		for cur != "" && state[cur] == 1 {
			state[cur] = 2
			cur = next[cur]
		}
	}
	return cycles
}

// componentKeys independently labels each participant with the minimum
// normalized source name of its weakly connected component.
func componentKeys(m Manifest, next map[string]string) map[string]string {
	adj := map[string]map[string]bool{}
	link := func(a, b string) {
		if adj[a] == nil {
			adj[a] = map[string]bool{}
		}
		adj[a][b] = true
	}
	for _, r := range m.Renames {
		link(r.Source, r.Source)
		if nx := next[r.Source]; nx != "" {
			link(r.Source, nx)
			link(nx, r.Source)
		}
	}
	keys := map[string]string{}
	for s := range adj {
		if _, done := keys[s]; done {
			continue
		}
		members := []string{s}
		seen := map[string]bool{s: true}
		keys[s] = ""
		for q := 0; q < len(members); q++ {
			for w := range adj[members[q]] {
				if !seen[w] {
					seen[w] = true
					keys[w] = ""
					members = append(members, w)
				}
			}
		}
		k := norm(members[0])
		for _, mm := range members[1:] {
			if norm(mm) < k {
				k = norm(mm)
			}
		}
		for _, mm := range members {
			keys[mm] = k
		}
	}
	return keys
}

func checkManifest(t *testing.T, m Manifest) {
	t.Helper()
	p, err := plan(m)
	if !validManifest(m) {
		if err == nil {
			t.Fatalf("invalid manifest accepted: files=%v renames=%+v", m.Files, m.Renames)
		}
		return
	}
	if err != nil {
		t.Fatalf("valid manifest rejected: files=%v renames=%+v: %v", m.Files, m.Renames, err)
	}
	verifyPlan(t, m, p)

	p2, err := plan(m)
	if err != nil || !reflect.DeepEqual(p, p2) {
		t.Fatalf("non-deterministic plan for files=%v renames=%+v", m.Files, m.Renames)
	}
}

func verifyPlan(t *testing.T, m Manifest, p Plan) {
	t.Helper()
	if len(p.Steps) != p.Moves {
		t.Fatalf("moves=%d but %d steps: %+v", p.Moves, len(p.Steps), p.Steps)
	}
	if len(p.Rollback) != len(p.Steps) {
		t.Fatalf("rollback length %d != steps %d", len(p.Rollback), len(p.Steps))
	}

	owner := map[string]string{}
	for _, f := range m.Files {
		owner[norm(f)] = f
	}
	targetOf := map[string]string{}
	srcOfTargetNorm := map[string]string{}
	isSource, isTarget := map[string]bool{}, map[string]bool{}
	for _, r := range m.Renames {
		targetOf[r.Source] = r.Target
		srcOfTargetNorm[norm(r.Target)] = r.Source
		isSource[r.Source] = true
		isTarget[r.Target] = true
	}
	next := map[string]string{}
	for s, tg := range targetOf {
		if cur, occupied := owner[norm(tg)]; occupied {
			next[s] = cur
		}
	}
	cycles := countCycles(next)

	// Minimality: every file named in a mapping must move at least once; each
	// cyclic component needs exactly one additional temp move.
	wantMoves := len(m.Renames) + cycles
	if p.Moves != wantMoves {
		t.Fatalf("moves=%d, want minimal %d (%d renames + %d cycles): files=%v renames=%+v",
			p.Moves, wantMoves, len(m.Renames), cycles, m.Files, m.Renames)
	}

	// Rollback is exactly the reverse sequence of inverse moves.
	for i, s := range p.Steps {
		r := p.Rollback[len(p.Steps)-1-i]
		if r.From != s.To || r.To != s.From {
			t.Fatalf("rollback[%d]=%+v is not the inverse of step %+v", i, r, s)
		}
	}

	// Temp-name accounting.
	temps := map[string]bool{}
	stores, loads := 0, 0
	for _, s := range p.Steps {
		if !isSource[s.From] {
			temps[s.From] = true
			loads++
		}
		if !isTarget[s.To] {
			temps[s.To] = true
			stores++
		}
	}
	if cycles == 0 && len(temps) != 0 {
		t.Fatalf("temp name used although there is no cycle: %v", temps)
	}
	if cycles > 0 {
		if len(temps) != 1 {
			t.Fatalf("want a single reused temp name, got %v", temps)
		}
		if stores != cycles || loads != cycles {
			t.Fatalf("temp stores=%d loads=%d, want exactly %d of each", stores, loads, cycles)
		}
		var temp string
		for tp := range temps {
			temp = tp
		}
		if cur, bad := owner[norm(temp)]; bad {
			t.Fatalf("temp %q collides with current file %q", temp, cur)
		}
		if src, bad := srcOfTargetNorm[norm(temp)]; bad {
			t.Fatalf("temp %q collides with target of %q", temp, src)
		}
		// Each store X->temp must be paired with the next load temp->targetOf[X].
		for i, s := range p.Steps {
			if s.To != temp {
				continue
			}
			j := i + 1
			for j < len(p.Steps) && p.Steps[j].From != temp {
				j++
			}
			if j == len(p.Steps) {
				t.Fatalf("temp store at step %d is never loaded", i)
			}
			if p.Steps[j].To != targetOf[s.From] {
				t.Fatalf("temp load at step %d goes to %q, want %q", j, p.Steps[j].To, targetOf[s.From])
			}
		}
	}

	// Steps are grouped by component, components sorted by minimum source name.
	keys := componentKeys(m, next)
	prevKey := ""
	participantOf := func(s Step) string {
		if isSource[s.From] {
			return s.From
		}
		return srcOfTargetNorm[norm(s.To)]
	}
	for i, s := range p.Steps {
		k := keys[participantOf(s)]
		if k < prevKey {
			t.Fatalf("step %d (%+v) belongs to component %q after %q: components not sorted", i, s, k, prevKey)
		}
		prevKey = k
	}

	// Replay the forward plan on the in-memory table: every target must be
	// free, the final layout must match the mapping, untouched files stay put.
	table := newFSTable(m.Files)
	for i, s := range p.Steps {
		if err := table.move(s.From, s.To); err != nil {
			t.Fatalf("forward step %d (%+v) rejected: %v\nmanifest: %+v", i, s, err, m)
		}
	}
	for _, r := range m.Renames {
		if got := table.byNorm[norm(r.Target)]; got != r.Target {
			t.Fatalf("after plan, %q holds %q, want exact %q (renames=%+v)", r.Target, got, r.Target, m.Renames)
		}
	}
	participants := map[string]bool{}
	for _, r := range m.Renames {
		participants[r.Source] = true
	}
	for _, f := range m.Files {
		if participants[f] {
			continue
		}
		if got := table.byNorm[norm(f)]; got != f {
			t.Fatalf("non-participating file %q disturbed (now %q)", f, got)
		}
	}
	if len(table.byNorm) != len(m.Files) {
		t.Fatalf("file count changed: %d -> %d", len(m.Files), len(table.byNorm))
	}

	// Replay the rollback from the final layout: it must never overwrite and
	// must restore the original layout exactly.
	for i, s := range p.Rollback {
		if err := table.move(s.From, s.To); err != nil {
			t.Fatalf("rollback step %d (%+v) rejected (would overwrite): %v", i, s, err)
		}
	}
	for _, f := range m.Files {
		if got := table.byNorm[norm(f)]; got != f {
			t.Fatalf("rollback did not restore %q (got %q)", f, got)
		}
	}
}

// enumerateManifests lets every file stay unmapped or map to any name in the
// target pool, covering chains, cycles, case-only renames and all invalid
// combinations at small scale.
func enumerateManifests(files, pool []string, fn func(Manifest)) {
	choices := make([]int, len(files))
	var rec func(i int)
	rec = func(i int) {
		if i == len(files) {
			m := Manifest{Files: files}
			for j, c := range choices {
				if c >= 0 {
					m.Renames = append(m.Renames, Rename{Source: files[j], Target: pool[c]})
				}
			}
			fn(m)
			return
		}
		choices[i] = -1
		rec(i + 1)
		for c := range pool {
			choices[i] = c
			rec(i + 1)
		}
	}
	rec(0)
}

func TestExhaustiveSmall(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		pool  []string
	}{
		{"one", []string{"a"}, []string{"a", "A", "x"}},
		{"two-lower", []string{"a", "b"}, []string{"a", "A", "b", "B", "x", "y"}},
		{"two-mixed-case", []string{"a", "B"}, []string{"a", "A", "b", "B", "x", "y"}},
		{"three", []string{"a", "b", "c"}, []string{"a", "A", "b", "B", "c", "C", "x", "y"}},
		{"four", []string{"a", "b", "c", "d"}, []string{"a", "A", "b", "B", "c", "C", "d", "D", "x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			enumerateManifests(tc.files, tc.pool, func(m Manifest) {
				checkManifest(t, m)
				count++
			})
			t.Logf("replayed %d manifests", count)
		})
	}
}

func TestChainRunsInReverse(t *testing.T) {
	// a wants b's name, b wants the free name c: the tail moves first.
	m := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	wantSteps := []Step{{"b", "c"}, {"a", "b"}}
	if !reflect.DeepEqual(p.Steps, wantSteps) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, wantSteps)
	}
	wantRollback := []Step{{"b", "a"}, {"c", "b"}}
	if !reflect.DeepEqual(p.Rollback, wantRollback) {
		t.Fatalf("rollback=%+v, want %+v", p.Rollback, wantRollback)
	}
	if p.Moves != 2 {
		t.Fatalf("moves=%d, want 2", p.Moves)
	}
}

func TestTwoNodeSwapIsOneCycle(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "b"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	want := []Step{{"a", tmp}, {"b", "a"}, {tmp, "b"}}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
	if p.Moves != 3 {
		t.Fatalf("moves=%d, want 3 (2 renames + 1 temp move)", p.Moves)
	}
}

func TestThreeNodeCycleUsesOneTempMove(t *testing.T) {
	m := Manifest{
		Files:   []string{"a", "b", "c"},
		Renames: []Rename{{"a", "b"}, {"b", "c"}, {"c", "a"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	want := []Step{{"a", tmp}, {"c", "a"}, {"b", "c"}, {tmp, "b"}}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
	if p.Moves != 4 {
		t.Fatalf("moves=%d, want 4 (3 renames + 1 temp move)", p.Moves)
	}
}

func TestCaseOnlyRenameUsesTemp(t *testing.T) {
	m := Manifest{
		Files:   []string{"readme.txt"},
		Renames: []Rename{{"readme.txt", "README.TXT"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	want := []Step{{"readme.txt", tmp}, {tmp, "README.TXT"}}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
	if p.Moves != 2 {
		t.Fatalf("moves=%d, want 2", p.Moves)
	}
}

func TestComponentsOrderedByMinimumSource(t *testing.T) {
	// Two case-only renames: component "a" must be emitted before "m".
	m := Manifest{
		Files:   []string{"m", "a"},
		Renames: []Rename{{"m", "M"}, {"a", "A"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	want := []Step{
		{"a", tmp}, {tmp, "A"},
		{"m", tmp}, {tmp, "M"},
	}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
}

func TestMixedCaseCycle(t *testing.T) {
	// File "B" must end up as "A" while "a" becomes "b": a true cycle under
	// case folding even though exact names differ.
	m := Manifest{
		Files:   []string{"a", "B"},
		Renames: []Rename{{"a", "b"}, {"B", "A"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "__rename_tmp_0__"
	want := []Step{{"a", tmp}, {"B", "A"}, {tmp, "b"}}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
	verifyPlan(t, m, p)
}

func TestTempNameSkipsCollisions(t *testing.T) {
	// tmp index 0 collides with a current file name; the cycle must take 1.
	m := Manifest{
		Files:   []string{"a", "b", "__rename_tmp_0__"},
		Renames: []Rename{{"a", "b"}, {"b", "a"}},
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Steps {
		if strings.Contains(s.From, "tmp") || strings.Contains(s.To, "tmp") {
			if s.From != "__rename_tmp_1__" && s.To != "__rename_tmp_1__" {
				t.Fatalf("temp step %+v does not use index 1", s)
			}
		}
	}

	// tmp index 0 collides with a target of another rename; the cycle must
	// still skip it, while the rename targeting that name stays a plain move.
	m = Manifest{
		Files: []string{"a", "b", "c"},
		Renames: []Rename{
			{"a", "b"}, {"b", "a"},
			{"c", "__rename_tmp_0__"},
		},
	}
	p, err = plan(m)
	if err != nil {
		t.Fatal(err)
	}
	tmp1 := "__rename_tmp_1__"
	want := []Step{
		{"a", tmp1}, {"b", "a"}, {tmp1, "b"},
		{"c", "__rename_tmp_0__"},
	}
	if !reflect.DeepEqual(p.Steps, want) {
		t.Fatalf("steps=%+v, want %+v", p.Steps, want)
	}
}

func TestEmptyRenameSet(t *testing.T) {
	p, err := plan(Manifest{Files: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Moves != 0 || len(p.Steps) != 0 || len(p.Rollback) != 0 {
		t.Fatalf("empty plan expected, got %+v", p)
	}
}

func TestMaxScale200(t *testing.T) {
	// 100 two-node swaps (cycles) interleaved in input order, at the 200-file
	// limit: verifies ordering, single-name temp reuse and replay at scale.
	var m Manifest
	for i := 0; i < 100; i++ {
		m.Files = append(m.Files, fmt.Sprintf("f%03d", i), fmt.Sprintf("g%03d", i))
	}
	for i := 0; i < 100; i++ {
		m.Renames = append(m.Renames,
			Rename{fmt.Sprintf("f%03d", i), fmt.Sprintf("g%03d", i)},
			Rename{fmt.Sprintf("g%03d", i), fmt.Sprintf("f%03d", i)},
		)
	}
	p, err := plan(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != maxFiles {
		t.Fatalf("setup expected %d files", maxFiles)
	}
	if p.Moves != 200+100 {
		t.Fatalf("moves=%d, want 300", p.Moves)
	}
	verifyPlan(t, m, p)
}

func TestValidationErrors(t *testing.T) {
	bad := []struct {
		name string
		m    Manifest
	}{
		{"no files", Manifest{}},
		{"too many files", Manifest{Files: make([]string, 201)}},
		{"non-ascii file", Manifest{Files: []string{"café.txt"}}},
		{"colliding current names", Manifest{Files: []string{"a", "A"}}},
		{"unknown source", Manifest{Files: []string{"a"}, Renames: []Rename{{"b", "c"}}}},
		{"case-mismatched source", Manifest{Files: []string{"A"}, Renames: []Rename{{"a", "b"}}}},
		{"no-op rename", Manifest{Files: []string{"a"}, Renames: []Rename{{"a", "a"}}}},
		{"duplicate source", Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "x"}, {"a", "y"}}}},
		{"colliding targets", Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "X"}, {"b", "x"}}}},
		{"target on bystander", Manifest{Files: []string{"a", "b"}, Renames: []Rename{{"a", "B"}}}},
		{"empty target", Manifest{Files: []string{"a"}, Renames: []Rename{{"a", ""}}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := plan(tc.m); err == nil {
				t.Fatalf("expected error for %s, manifest=%+v", tc.name, tc.m)
			}
		})
	}
}
