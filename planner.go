package main

import (
	"fmt"
	"sort"
	"strings"
)

// maxFiles bounds the manifest size: 1..200 current file names.
const maxFiles = 200

// Rename maps one current file name to its desired name.
type Rename struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Manifest is the dry-run input: current file names plus the source->target
// mappings that should be realized.
type Manifest struct {
	Files   []string `json:"files"`
	Renames []Rename `json:"renames"`
}

// Step is one atomic move on the case-insensitive drive.
type Step struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Plan is the dry-run output: the ordered forward steps plus the reverse
// rollback steps. The planner never touches the file system itself.
type Plan struct {
	Moves    int    `json:"moves"`
	Steps    []Step `json:"steps"`
	Rollback []Step `json:"rollback"`
}

// norm folds an ASCII name the way the case-insensitive shared drive does.
func norm(s string) string { return strings.ToLower(s) }

// isASCIIName reports whether s is a non-empty printable-ASCII file name.
func isASCIIName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// component is a weakly connected part of the rename graph. Because every
// source has at most one outgoing edge and every normalized target is unique,
// each component is either a chain (its last target is free) or a cycle
// (including the length-1 self loop that a case-only rename produces).
type component struct {
	nodes  []string // chain: head..tail ; cycle: rotated so the minimum source is first
	cyclic bool
}

// plan validates the manifest and produces a minimal, collision-free move
// plan. Every step moves to a name that is free at that moment.
func plan(m Manifest) (Plan, error) {
	p := Plan{Steps: []Step{}, Rollback: []Step{}}

	if len(m.Files) == 0 || len(m.Files) > maxFiles {
		return p, fmt.Errorf("manifest must list 1..%d current files, got %d", maxFiles, len(m.Files))
	}

	owner := make(map[string]string, len(m.Files)) // normalized name -> current exact name
	for _, f := range m.Files {
		if !isASCIIName(f) {
			return p, fmt.Errorf("file name %q is not printable ASCII", f)
		}
		n := norm(f)
		if prev, dup := owner[n]; dup {
			return p, fmt.Errorf("current names %q and %q collide on a case-insensitive drive", prev, f)
		}
		owner[n] = f
	}

	targetOf := make(map[string]string, len(m.Renames))    // source -> exact target
	targetNorms := make(map[string]string, len(m.Renames)) // normalized target -> exact target
	for _, r := range m.Renames {
		if !isASCIIName(r.Source) || !isASCIIName(r.Target) {
			return p, fmt.Errorf("rename %q -> %q: names must be printable ASCII", r.Source, r.Target)
		}
		if r.Source == r.Target {
			return p, fmt.Errorf("rename %q -> %q is a no-op", r.Source, r.Target)
		}
		cur, ok := owner[norm(r.Source)]
		if !ok {
			return p, fmt.Errorf("source %q is not a current file", r.Source)
		}
		if cur != r.Source {
			return p, fmt.Errorf("source %q does not match current file %q exactly", r.Source, cur)
		}
		if _, dup := targetOf[r.Source]; dup {
			return p, fmt.Errorf("source %q has more than one rename", r.Source)
		}
		tn := norm(r.Target)
		if prev, dup := targetNorms[tn]; dup {
			return p, fmt.Errorf("targets %q and %q collide on a case-insensitive drive", prev, r.Target)
		}
		targetOf[r.Source] = r.Target
		targetNorms[tn] = r.Target
	}

	// A target must never land on a file that does not participate: renaming
	// it would silently overwrite an untouched file.
	for _, r := range m.Renames {
		if cur, occupied := owner[norm(r.Target)]; occupied {
			if _, participates := targetOf[cur]; !participates {
				return p, fmt.Errorf("target %q occupies non-participating file %q", r.Target, cur)
			}
		}
	}

	// Functional graph on participants: next[s] is the participant currently
	// occupying s's target, or "" when the target is free.
	next := make(map[string]string, len(targetOf))
	for s, tgt := range targetOf {
		if cur, occupied := owner[norm(tgt)]; occupied {
			next[s] = cur
		}
	}

	sources := make([]string, 0, len(targetOf))
	for s := range targetOf {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return norm(sources[i]) < norm(sources[j]) })

	// Walk the functional graph to find cycles. A node can have in-degree 1 at
	// most, so nothing feeds into a cycle from outside; the remaining
	// components are plain chains.
	const (
		unseen = iota
		onWalk
		done
	)
	state := make(map[string]int, len(sources))
	inCycle := make(map[string]bool, len(sources))
	var comps []component
	for _, s := range sources {
		if state[s] != unseen {
			continue
		}
		var path []string
		index := map[string]int{}
		cur := s
		for cur != "" && state[cur] == unseen {
			state[cur] = onWalk
			index[cur] = len(path)
			path = append(path, cur)
			cur = next[cur]
		}
		if cur != "" && state[cur] == onWalk {
			comps = append(comps, component{nodes: rotateMin(path[index[cur]:]), cyclic: true})
			for _, n := range path[index[cur]:] {
				inCycle[n] = true
			}
		}
		for _, n := range path {
			state[n] = done
		}
	}

	// Chains start at a node with no incoming edge.
	hasIncoming := make(map[string]bool, len(next))
	for _, nx := range next {
		if nx != "" {
			hasIncoming[nx] = true
		}
	}
	for _, s := range sources {
		if inCycle[s] || hasIncoming[s] {
			continue
		}
		var chain []string
		for cur := s; cur != ""; cur = next[cur] {
			chain = append(chain, cur)
		}
		comps = append(comps, component{nodes: chain})
	}

	// Emit whole components ordered by their minimum (normalized) source name.
	sort.Slice(comps, func(i, j int) bool {
		return componentKey(comps[i].nodes) < componentKey(comps[j].nodes)
	})

	temp := tempName(owner, targetNorms, comps)

	steps := make([]Step, 0, len(m.Renames)+cycleCount(comps))
	for _, c := range comps {
		if !c.cyclic {
			// Chain c[0]->c[1]->...->c[k-1]->free: execute in reverse so each
			// target has been vacated by the preceding step.
			for i := len(c.nodes) - 1; i >= 0; i-- {
				s := c.nodes[i]
				steps = append(steps, Step{From: s, To: targetOf[s]})
			}
			continue
		}
		// Cycle c[0]->c[1]->...->c[n-1]->c[0]: park c[0] at the temp name once,
		// then free names from the back until c[0]'s target is free. Exactly
		// one extra move for the whole cycle.
		c0 := c.nodes[0]
		steps = append(steps, Step{From: c0, To: temp})
		for i := len(c.nodes) - 1; i >= 1; i-- {
			s := c.nodes[i]
			steps = append(steps, Step{From: s, To: targetOf[s]})
		}
		steps = append(steps, Step{From: temp, To: targetOf[c0]})
	}

	rollback := make([]Step, len(steps))
	for i, s := range steps {
		rollback[len(steps)-1-i] = Step{From: s.To, To: s.From}
	}

	return Plan{Moves: len(steps), Steps: steps, Rollback: rollback}, nil
}

// componentKey is the minimum normalized source name in a component, used to
// order components deterministically.
func componentKey(nodes []string) string {
	k := norm(nodes[0])
	for _, n := range nodes[1:] {
		if nk := norm(n); nk < k {
			k = nk
		}
	}
	return k
}

// rotateMin rotates a cycle so its minimum normalized source is first; this
// is the node that gets parked at the temp name.
func rotateMin(nodes []string) []string {
	mi := 0
	for i := range nodes {
		if norm(nodes[i]) < norm(nodes[mi]) {
			mi = i
		}
	}
	out := make([]string, 0, len(nodes))
	out = append(out, nodes[mi:]...)
	out = append(out, nodes[:mi]...)
	return out
}

// tempName returns the smallest-numbered temp name that collides with neither
// any current name nor any target (case-insensitively). Components run
// sequentially and each releases the temp name, so one name is reused.
func tempName(owner, targetNorms map[string]string, comps []component) string {
	if cycleCount(comps) == 0 {
		return ""
	}
	for i := 0; ; i++ {
		cand := fmt.Sprintf("__rename_tmp_%d__", i)
		n := norm(cand)
		if _, bad := owner[n]; bad {
			continue
		}
		if _, bad := targetNorms[n]; bad {
			continue
		}
		return cand
	}
}

func cycleCount(comps []component) int {
	n := 0
	for _, c := range comps {
		if c.cyclic {
			n++
		}
	}
	return n
}
