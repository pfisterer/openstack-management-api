package model

import (
	"maps"
	"slices"
	"strings"

	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ResolveCaps is what applies below the given budgets, in any order, of the caps in defs (common.Cap): for each, the strictest value any of them sets. A Grant cap is off (missing) as soon as one of them that does not inherit its limit sets none, and when all of them inherit theirs. Values of ids defs does not know are ignored. Nil when nothing applies.
func ResolveCaps(chain []Node, defs []common.Cap) common.Caps {
	out := common.Caps{}
	for _, d := range defs {
		v, ok := resolveCap(chain, d)
		if ok {
			out[d.ID] = v
		}
	}
	return out.Clone()
}

func resolveCap(chain []Node, d common.Cap) (int, bool) {
	var v int
	set := false
	for _, n := range chain {
		if d.Grant && n.InheritsLimit {
			continue
		}
		own, ok := n.Caps[d.ID]
		if !ok {
			if d.Grant {
				return 0, false
			}
			continue
		}
		if !set {
			v, set = own, true
			continue
		}
		v = d.Stricter(v, own)
	}
	return v, set
}

// ValidateCaps checks a budget's own caps against defs and against inherited, what the budgets above allow (ResolveCaps over them): every id known, every value within its cap, none looser than above; a Grant cap only where those above have it (root: there is nothing above) and not on a budget that inherits its limit; a Together set all or none.
func ValidateCaps(own, inherited common.Caps, root, inheritsLimit bool, defs []common.Cap) error {
	catalog := common.CapCatalog(defs)
	for _, id := range slices.Sorted(maps.Keys(own)) {
		v := own[id]
		d, ok := catalog.Find(id)
		if !ok {
			return invalid("Unbekannte Einstellung %q.", "Unknown setting %q.", id)
		}
		if err := d.Check(v); err != nil {
			return err
		}
		if d.Grant && inheritsLimit {
			return invalid("%s: Ein Budget, das das Limit des Budgets darüber übernimmt, gibt weiter, was jenes festlegt.", "%s: a budget that inherits the limit of the budget above passes on what that one sets.", d.Name)
		}
		above, has := inherited[id]
		if d.Grant && !root && !has {
			return invalid("%s: Das Budget darüber hat das nicht, deshalb kann dieses es auch nicht haben.", "%s: the budget above does not have this, so this one cannot have it either.", d.Name)
		}
		if has && d.Looser(v, above) {
			return invalid("%s: Das Budget darüber erlaubt höchstens %s, nicht %s.", "%s: the budget above allows at most %s, not %s.", d.Name, d.Label(above), d.Label(v))
		}
	}
	for _, d := range defs {
		if d.Together == "" {
			continue
		}
		_, set := own[d.ID]
		for _, o := range defs {
			if o.Together != d.Together {
				continue
			}
			if _, other := own[o.ID]; other != set {
				var de, en []string
				for _, m := range defs {
					if m.Together == d.Together {
						de, en = append(de, m.Name.In(msg.DE)), append(en, m.Name.In(msg.EN))
					}
				}
				return invalid("%s werden zusammen gesetzt oder gar nicht.", "%s are set together or not at all.", msg.T(strings.Join(de, ", "), strings.Join(en, ", ")))
			}
		}
	}
	return nil
}

// GrantCaps are those of caps whose cap in defs is a Grant; nil when none.
func GrantCaps(caps common.Caps, defs []common.Cap) common.Caps {
	out := common.Caps{}
	for _, d := range defs {
		if v, ok := caps[d.ID]; ok && d.Grant {
			out[d.ID] = v
		}
	}
	return out.Clone()
}

// invalid is a validation message for people in German and English; %w wraps as in fmt.Errorf (msg.Errorf). A function of its own because the texts are sentences, capitalised and with a full stop, which staticcheck refuses in a literal fmt.Errorf.
func invalid(de, en string, args ...any) error {
	return msg.Errorf(de, en, args...)
}
