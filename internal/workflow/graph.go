package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// Edges returns the workflow's dependency edges as dependent → dependencies.
//
// Two declarations create an edge, and both are honoured. `depends_on` is
// ordering — "do not start me before that one finished" — and a routing
// entry is data flow, which implies the same ordering. Treating routing as
// an implicit dependency is what lets a workflow declare only its routes
// and still execute in a sensible order; a file that says both is
// consistent rather than contradictory.
func (s *Spec) Edges() map[string][]string {
	deps := map[string]map[string]bool{}
	for name := range s.Agents {
		deps[name] = map[string]bool{}
	}
	for name, a := range s.Agents {
		for _, d := range a.DependsOn {
			if _, ok := s.Agents[d]; ok {
				deps[name][d] = true
			}
		}
	}
	for _, r := range s.Routing {
		if _, ok := s.Agents[r.From]; !ok {
			continue
		}
		if _, ok := s.Agents[r.To]; !ok {
			continue
		}
		deps[r.To][r.From] = true
	}

	out := map[string][]string{}
	for name, set := range deps {
		list := make([]string, 0, len(set))
		for d := range set {
			list = append(list, d)
		}
		sort.Strings(list)
		out[name] = list
	}
	return out
}

// Order returns agents in a topological execution order, or an error
// naming the cycle. Ties are broken alphabetically so the order a
// workflow runs in is a property of the file, not of map iteration.
func (s *Spec) Order() ([]string, error) {
	deps := s.Edges()
	remaining := map[string]bool{}
	for _, n := range s.AgentNames() {
		remaining[n] = true
	}

	var order []string
	for len(remaining) > 0 {
		var ready []string
		for n := range remaining {
			satisfied := true
			for _, d := range deps[n] {
				if remaining[d] {
					satisfied = false
					break
				}
			}
			if satisfied {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("dependency cycle among agents: %s", describeCycle(deps, remaining))
		}
		sort.Strings(ready)
		for _, n := range ready {
			order = append(order, n)
			delete(remaining, n)
		}
	}
	return order, nil
}

// Inbound returns the routes delivering into an agent, in declaration
// order. Declaration order is the delivery order when several routes feed
// one agent, which makes the concatenated prompt reproducible.
func (s *Spec) Inbound(agent string) []Route {
	var out []Route
	for _, r := range s.Routing {
		if r.To == agent {
			out = append(out, r)
		}
	}
	return out
}

// Outbound returns the routes leaving an agent, in declaration order.
func (s *Spec) Outbound(agent string) []Route {
	var out []Route
	for _, r := range s.Routing {
		if r.From == agent {
			out = append(out, r)
		}
	}
	return out
}

// Sources are agents nothing routes into. They receive the workflow input.
func (s *Spec) Sources() []string {
	var out []string
	for _, n := range s.AgentNames() {
		if len(s.Inbound(n)) == 0 {
			out = append(out, n)
		}
	}
	return out
}

// Sinks are agents that route nowhere. Their outputs are the workflow's.
func (s *Spec) Sinks() []string {
	var out []string
	for _, n := range s.AgentNames() {
		if len(s.Outbound(n)) == 0 {
			out = append(out, n)
		}
	}
	return out
}

// describeCycle walks the still-unresolved agents to produce a concrete
// path a reader can follow, rather than an unordered set. A cycle report
// that just lists names leaves the reader to rediscover the edge.
func describeCycle(deps map[string][]string, remaining map[string]bool) string {
	names := make([]string, 0, len(remaining))
	for n := range remaining {
		names = append(names, n)
	}
	sort.Strings(names)

	// Depth-first from the alphabetically first survivor; every agent left
	// in the set is on or downstream of a cycle, so this always finds one.
	state := map[string]int{} // 0 unseen, 1 on stack, 2 done
	var stack []string
	var found []string

	var visit func(string) bool
	visit = func(n string) bool {
		state[n] = 1
		stack = append(stack, n)
		for _, d := range deps[n] {
			if !remaining[d] {
				continue
			}
			if state[d] == 1 {
				for i, s := range stack {
					if s == d {
						found = append(append([]string{}, stack[i:]...), d)
						return true
					}
				}
			}
			if state[d] == 0 && visit(d) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = 2
		return false
	}
	for _, n := range names {
		if state[n] == 0 && visit(n) {
			break
		}
	}
	if len(found) == 0 {
		return strings.Join(names, ", ")
	}
	// The edges point at dependencies, so the readable direction — who
	// feeds whom — is the reverse of the walk.
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return strings.Join(found, " → ")
}
