package common

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// A cap is an upper bound a budget sets for everything below it, declared by the provider that carries it out (decided 10.11.2026, docs/plan-caps-katalog.md): the release times, the local storage per workspace, the batch priority and the batch jobs of the AI service, and whatever a provider brings next. Unlike a resource it is no quantity: nothing is summed or handed down in parts. It holds for every project below the budget, a budget below may only tighten it, and what applies is the strictest value along the chain of budgets. The tree knows only this rule; what a cap means is the provider's business.
//
// Values are integers, and the smaller one is always the stricter: a choice is stored as its index into Choices, which run from the strictest to the loosest; under Unbounded, 0 means no bound and is the loosest of all.

// Cap is one cap as a provider declares it. /v1/config hands it to the UI as a CapView, its name and message in the caller's language.
type Cap struct {
	ID string `json:"id"`
	// Name and Message are for people, in German and English; the tree's validation messages name a cap by Name.
	Name     msg.Text `json:"-"`
	Message  msg.Text `json:"-"`
	Provider string   `json:"provider"`
	// Area is the concern the cap serves (common.Areas), where the UI shows it.
	Area string `json:"area,omitempty"`
	// Group is the heading the UI shows the cap under.
	Group string `json:"group,omitempty"`
	Unit  string `json:"unit,omitempty"`
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
	// Default is the value the UI pre-fills when someone switches the cap on (a Grant cap, before the budgets above tighten it); it passes Check. Absent, the UI starts from what the budgets above allow, else from Min.
	Default *int `json:"default,omitempty"`
}

// CapView is a cap as /v1/config hands it to the UI: its name and message in one language, the rest as declared. In the API documentation it keeps the name common.Cap, so the generated clients do not change.
type CapView struct {
	Cap
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
} //@name common.Cap

// In is d as the UI is handed it in l.
func (d Cap) In(l msg.Lang) CapView {
	return CapView{Cap: d, Name: d.Name.In(l), Message: d.Message.In(l)}
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

// Check refuses a value outside what the cap allows, with a message for people in German and English, like the tree's validation messages.
func (d Cap) Check(v int) error {
	switch {
	case len(d.Choices) > 0:
		if v < 0 || v >= len(d.Choices) {
			return invalid("%s: %d ist keiner der Werte 0 bis %d (%s).", "%s: %d is not one of the values 0 to %d (%s).", d.Name, v, len(d.Choices)-1, strings.Join(d.Choices, ", "))
		}
	case d.Unbounded && v == 0:
	case v < d.Min || v > d.Max:
		if d.Unbounded {
			return invalid("%s: Erlaubt sind %d bis %d%s oder 0 für keine Obergrenze.", "%s: allowed are %d to %d%s, or 0 for no upper bound.", d.Name, d.Min, d.Max, d.unitSuffix())
		}
		return invalid("%s: Erlaubt sind %d bis %d%s.", "%s: allowed are %d to %d%s.", d.Name, d.Min, d.Max, d.unitSuffix())
	}
	return nil
}

// Label is a value as a message names it: the choice (its id, which is also what the provider is told), "ohne Obergrenze" / "no upper bound" for 0 under Unbounded, or the number with its unit.
func (d Cap) Label(v int) msg.Text {
	switch {
	case len(d.Choices) > 0 && v >= 0 && v < len(d.Choices):
		return msg.Raw(d.Choices[v])
	case d.Unbounded && v == 0:
		return msg.T("ohne Obergrenze", "no upper bound")
	}
	return msg.Raw(fmt.Sprintf("%d%s", v, d.unitSuffix()))
}

// invalid is a validation message for people in German and English, as in package model and tree: a function of its own because the texts are sentences, capitalised and with a full stop, which staticcheck refuses in a literal fmt.Errorf.
func invalid(de, en string, args ...any) error {
	return msg.Errorf(de, en, args...)
}

func (d Cap) unitSuffix() string {
	if d.Unit == "" {
		return ""
	}
	return " " + d.Unit
}

// ValidateCapDefs checks the caps of all providers at startup: ids unique and set, bounds sensible, a default within them, a Together set within one provider and either all Grant or none.
func ValidateCapDefs(defs []Cap) error {
	seen := map[string]bool{}
	together := map[string]Cap{}
	for _, d := range defs {
		if d.ID == "" || !d.Name.Complete() || d.Provider == "" {
			return fmt.Errorf("cap %q: id, name (German and English) and provider are required", d.ID)
		}
		if !d.Message.IsZero() && !d.Message.Complete() {
			return fmt.Errorf("cap %q: a message needs German and English", d.ID)
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
		if d.Default != nil {
			if err := d.Check(*d.Default); err != nil {
				return fmt.Errorf("cap %q: default: %w", d.ID, err)
			}
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
