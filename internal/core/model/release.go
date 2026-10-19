package model

import "fmt"

// Release times are when a workspace or a GPU kernel at the AI service goes back, per kind of use: CPU (CPU workspaces and the notebook of "CPU + GPU kernel") and GPU (GPU workspaces and GPU kernels). Counted is idle time, not runtime; the optional upper bound ends a workspace whatever it does.
//
// A budget sets the most the projects below it may choose (Node.ReleaseMax); the project chooses within it at the AI service. Unlike a limit this is no quantity: nothing is summed or handed down in parts. It is a cap that holds for every project below the budget, and a budget below may only tighten it. What applies to a project is the strictest value along its chain of budgets (ResolveReleaseMax), field by field; a field no budget sets is left to the AI service, which then keeps today's time of the kind of workspace.

// Bounds of the release times, the same as the AI service's: idle times from one minute to a day, upper bounds up to 30 days, 0 = no upper bound (GPU only).
const (
	MaxReleaseIdleMinutes = 24 * 60
	MaxReleaseMaxHours    = 30 * 24
)

// ReleaseTimes are the three times of one kind of use; nil = not set here (the budget above decides, or the AI service's default).
type ReleaseTimes struct {
	// IdleOpenMinutes: minutes without activity while a tab of the workspace is open.
	IdleOpenMinutes *int `json:"idle_open_minutes,omitempty" example:"60"`
	// IdleClosedMinutes: minutes without activity once no tab is open any more.
	IdleClosedMinutes *int `json:"idle_closed_minutes,omitempty" example:"10"`
	// MaxHours: hours after its start a workspace or GPU kernel ends at the latest; 0 = no upper bound, only for GPU (and at the AI service only for projects with GPUs of their own).
	MaxHours *int `json:"max_hours,omitempty" example:"24"`
}

// ReleaseMax are the most the projects below a budget may choose as release times, per kind of use.
type ReleaseMax struct {
	CPU ReleaseTimes `json:"cpu"`
	GPU ReleaseTimes `json:"gpu"`
}

// IsZero reports whether r sets nothing at all.
func (t ReleaseTimes) IsZero() bool {
	return t.IdleOpenMinutes == nil && t.IdleClosedMinutes == nil && t.MaxHours == nil
}

// IsZero reports whether r is nil or sets nothing at all.
func (r *ReleaseMax) IsZero() bool { return r == nil || (r.CPU.IsZero() && r.GPU.IsZero()) }

// Clone is a deep copy; nil stays nil.
func (r *ReleaseMax) Clone() *ReleaseMax {
	if r == nil {
		return nil
	}
	c := func(t ReleaseTimes) ReleaseTimes {
		return ReleaseTimes{IdleOpenMinutes: cloneInt(t.IdleOpenMinutes), IdleClosedMinutes: cloneInt(t.IdleClosedMinutes), MaxHours: cloneInt(t.MaxHours)}
	}
	return &ReleaseMax{CPU: c(r.CPU), GPU: c(r.GPU)}
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

// Normalized is r with nothing set collapsed to nil, so "no maximums of its own" has one spelling in the store.
func (r *ReleaseMax) Normalized() *ReleaseMax {
	if r.IsZero() {
		return nil
	}
	return r.Clone()
}

// stricterIdle is the shorter of two idle times; nil sets nothing.
func stricterIdle(a, b *int) *int {
	switch {
	case a == nil:
		return cloneInt(b)
	case b == nil || *a <= *b:
		return cloneInt(a)
	default:
		return cloneInt(b)
	}
}

// stricterMax is the stricter of two upper bounds: 0 (none) is the loosest value, nil sets nothing.
func stricterMax(a, b *int) *int {
	switch {
	case a == nil:
		return cloneInt(b)
	case b == nil:
		return cloneInt(a)
	case *a == 0:
		return cloneInt(b)
	case *b == 0 || *a <= *b:
		return cloneInt(a)
	default:
		return cloneInt(b)
	}
}

func stricterTimes(a, b ReleaseTimes) ReleaseTimes {
	return ReleaseTimes{
		IdleOpenMinutes:   stricterIdle(a.IdleOpenMinutes, b.IdleOpenMinutes),
		IdleClosedMinutes: stricterIdle(a.IdleClosedMinutes, b.IdleClosedMinutes),
		MaxHours:          stricterMax(a.MaxHours, b.MaxHours),
	}
}

// StricterRelease is, field by field, the stricter of two sets of maximums; nil when neither sets anything.
func StricterRelease(a, b *ReleaseMax) *ReleaseMax {
	switch {
	case a.IsZero():
		return b.Normalized()
	case b.IsZero():
		return a.Normalized()
	}
	return &ReleaseMax{CPU: stricterTimes(a.CPU, b.CPU), GPU: stricterTimes(a.GPU, b.GPU)}
}

// ResolveReleaseMax is what applies below the given nodes, in any order: the strictest value of each field over all of them. For a project it is called with the budgets above it, for a budget's inherited maximums with the budgets above that one. Nil when none of them sets anything.
func ResolveReleaseMax(chain []Node) *ReleaseMax {
	var out *ReleaseMax
	for _, n := range chain {
		out = StricterRelease(out, n.ReleaseMax)
	}
	return out
}

var releaseKinds = [...]string{"cpu", "gpu"}

func (r *ReleaseMax) kind(kind string) ReleaseTimes {
	if r == nil {
		return ReleaseTimes{}
	}
	if kind == "gpu" {
		return r.GPU
	}
	return r.CPU
}

// ValidateReleaseMax checks a budget's own maximums against the bounds and against inherited, what the budgets above it allow: a budget may only tighten, never loosen. An idle time may not be longer than the one above; an upper bound may not be longer or missing (0) where the budgets above have one; no upper bound only for GPU.
func ValidateReleaseMax(r, inherited *ReleaseMax) error {
	if r == nil {
		return nil
	}
	for _, kind := range releaseKinds {
		own, above := r.kind(kind), inherited.kind(kind)
		name := map[string]string{"cpu": "CPU", "gpu": "GPU"}[kind]
		for _, f := range []struct {
			label    string
			own, max *int
		}{{"idle time with an open tab", own.IdleOpenMinutes, above.IdleOpenMinutes}, {"idle time without a tab", own.IdleClosedMinutes, above.IdleClosedMinutes}} {
			if f.own == nil {
				continue
			}
			if *f.own < 1 || *f.own > MaxReleaseIdleMinutes {
				return fmt.Errorf("release times (%s): the %s must be between 1 and %d minutes", name, f.label, MaxReleaseIdleMinutes)
			}
			if f.max != nil && *f.own > *f.max {
				return fmt.Errorf("release times (%s): the budget above allows an %s of at most %d minutes, so this one cannot allow %d", name, f.label, *f.max, *f.own)
			}
		}
		if own.MaxHours == nil {
			continue
		}
		h := *own.MaxHours
		if h < 0 || h > MaxReleaseMaxHours {
			return fmt.Errorf("release times (%s): the upper bound must be between 1 and %d hours, or 0 for none", name, MaxReleaseMaxHours)
		}
		if h == 0 && kind == "cpu" {
			return fmt.Errorf("release times (CPU): an upper bound is required; only GPU workspaces of projects with GPUs of their own may run without one")
		}
		if above.MaxHours != nil && *above.MaxHours > 0 && (h == 0 || h > *above.MaxHours) {
			return fmt.Errorf("release times (%s): the budget above allows an upper bound of at most %d hours, so this one cannot allow %s", name, *above.MaxHours, hoursText(h))
		}
	}
	return nil
}

func hoursText(h int) string {
	if h == 0 {
		return "none"
	}
	return fmt.Sprintf("%d", h)
}
