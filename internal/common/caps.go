package common

import (
	"fmt"
	"maps"
	"slices"
)

// A cap is an upper bound a budget sets for everything below it, declared by the provider that carries it out (decided 10.11.2026, docs/plan-caps-katalog.md): the release times, the local storage per workspace, the batch priority and the batch jobs of the AI service, and whatever a provider brings next. Unlike a resource it is no quantity: nothing is summed or handed down in parts. It holds for every project below the budget, a budget below may only tighten it, and what applies is the strictest value along the chain of budgets. The tree knows only this rule; what a cap means is the provider's business.
//
// Values are integers, and the smaller one is always the stricter: a choice is stored as its index into Choices, which run from the strictest to the loosest; under Unbounded, 0 means no bound and is the loosest of all.

// Cap is one cap as a provider declares it, and as /v1/config hands it to the UI.
type Cap struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Area is the concern the cap serves (common.Areas), where the UI shows it.
	Area string `json:"area,omitempty"`
	// Group is the heading the UI shows the cap under.
	Group   string `json:"group,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Message string `json:"message,omitempty"`
	// Min and Max bound a value that is set (except 0 under Unbounded); ignored for a choice.
	Min int `json:"min"`
	Max int `json:"max"`
	// Unbounded allows 0, which means no bound and is looser than any other value.
	Unbounded bool `json:"unbounded,omitempty"`
	// Choices make the cap a selection, strictest first; the value is the index.
	Choices []string `json:"choices,omitempty"`
	// Grant makes a missing value mean "none" rather than "nothing of its own": like a resource, a budget has it only where it sets it, and may switch it on only where the budgets above have it (the root excepted). A budget that inherits its limit (Node.InheritsLimit) sets none of its own and passes on those above.
	Grant bool `json:"grant,omitempty"`
	// Together names a set of caps that are set all together or not at all.
	Together string `json:"together,omitempty"`
}

// Caps are the values of caps by id: a budget's own, or what applies below a chain of budgets.
type Caps map[string]int

// Clone is a copy; nil and empty both give nil, so "none" has one spelling.
func (c Caps) Clone() Caps {
	if len(c) == 0 {
		return nil
	}
	return maps.Clone(c)
}

// Looser reports whether v allows more than bound.
func (d Cap) Looser(v, bound int) bool {
	if d.Unbounded && bound == 0 {
		return false
	}
	if d.Unbounded && v == 0 {
		return true
	}
	return v > bound
}

// Stricter is the stricter of two values.
func (d Cap) Stricter(a, b int) int {
	if d.Looser(a, b) {
		return b
	}
	return a
}

// Check refuses a value outside what the cap allows.
func (d Cap) Check(v int) error {
	switch {
	case len(d.Choices) > 0:
		if v < 0 || v >= len(d.Choices) {
			return fmt.Errorf("%s: %d is none of the choices %v", d.Name, v, d.Choices)
		}
	case d.Unbounded && v == 0:
	case v < d.Min || v > d.Max:
		if d.Unbounded {
			return fmt.Errorf("%s: must be between %d and %d%s, or 0 for no bound", d.Name, d.Min, d.Max, d.unitSuffix())
		}
		return fmt.Errorf("%s: must be between %d and %d%s", d.Name, d.Min, d.Max, d.unitSuffix())
	}
	return nil
}

// Label is a value as a message names it.
func (d Cap) Label(v int) string {
	switch {
	case len(d.Choices) > 0 && v >= 0 && v < len(d.Choices):
		return d.Choices[v]
	case d.Unbounded && v == 0:
		return "no bound"
	}
	return fmt.Sprintf("%d%s", v, d.unitSuffix())
}

func (d Cap) unitSuffix() string {
	if d.Unit == "" {
		return ""
	}
	return " " + d.Unit
}

// ValidateCapDefs checks the caps of all providers at startup: ids unique and set, bounds sensible, a Together set within one provider and either all Grant or none.
func ValidateCapDefs(defs []Cap) error {
	seen := map[string]bool{}
	together := map[string]Cap{}
	for _, d := range defs {
		if d.ID == "" || d.Name == "" || d.Provider == "" {
			return fmt.Errorf("cap %q: id, name and provider are required", d.ID)
		}
		if d.Area != "" && !slices.Contains(Areas, d.Area) {
			return fmt.Errorf("cap %q: area %q is not one of %v", d.ID, d.Area, Areas)
		}
		if seen[d.ID] {
			return fmt.Errorf("duplicate cap id %q", d.ID)
		}
		seen[d.ID] = true
		if len(d.Choices) == 0 && (d.Min < 1 || d.Max < d.Min) {
			return fmt.Errorf("cap %q: needs 1 <= min <= max", d.ID)
		}
		if len(d.Choices) > 0 && d.Unbounded {
			return fmt.Errorf("cap %q: a choice has no unbounded value", d.ID)
		}
		if d.Together == "" {
			continue
		}
		if first, ok := together[d.Together]; ok && (first.Provider != d.Provider || first.Grant != d.Grant) {
			return fmt.Errorf("cap %q: the caps of %q must share provider and grant", d.ID, d.Together)
		}
		together[d.Together] = d
	}
	return nil
}

// CapCatalog is where the caps are read.
type CapCatalog []Cap

// Find is the cap of id.
func (c CapCatalog) Find(id string) (Cap, bool) {
	i := slices.IndexFunc(c, func(d Cap) bool { return d.ID == id })
	if i < 0 {
		return Cap{}, false
	}
	return c[i], true
}

// DiffCaps are the values of the ids that differ between a and b, from each side; nil, nil when nothing differs. An id missing on one side is not set there.
func DiffCaps(a, b Caps) (from, to Caps) {
	for id, v := range a {
		if w, ok := b[id]; !ok || w != v {
			if from == nil {
				from = Caps{}
			}
			from[id] = v
		}
	}
	for id, v := range b {
		if w, ok := a[id]; !ok || w != v {
			if to == nil {
				to = Caps{}
			}
			to[id] = v
		}
	}
	return from, to
}
