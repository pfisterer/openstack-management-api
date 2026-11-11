package tree

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/day"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// normalizeEnd checks an end date from a request and writes it as it is stored, "2006-01-02" (model.NormalizeEnd: a timestamp counts as its day in Berlin); anything unreadable is refused; nil stays nil.
func normalizeEnd(s *string) (*string, error) { return model.NormalizeEnd(s) }

// endsNoLater reports whether the end next is not after bound, as days (model.EndsNoLater); a nil bound means "no end" and bounds nothing. An unreadable end counts as later, so it takes the path through a human rather than past one.
func endsNoLater(next string, bound *string) bool { return model.EndsNoLater(next, bound) }

// chainEnd is the earliest end date along a chain of budgets — the date no node
// below them may outlive. Nil when none of them ends. An unreadable date counts
// as later than any other (model.EndsNoLater), so it bounds nothing.
func chainEnd(chain []model.Node) *string {
	var earliest *string
	var earliestAt day.Date
	for _, n := range chain {
		if n.TerminationDate == nil {
			continue
		}
		at, err := model.EndDay(*n.TerminationDate)
		if err != nil {
			continue
		}
		if earliest == nil || at.Before(earliestAt) {
			v := at.String() // as a day, so a node that takes it over stores no older spelling
			earliest, earliestAt = &v, at
		}
	}
	return earliest
}

// fitEnd applies the lifetime rule to the end date asked for a node below a
// budget chain ending at bound: nothing outlives the budget it draws from.
// Asking for no end there means the budget's end; asking for a later one is
// refused rather than cut, so nobody finds a date they did not pick.
func fitEnd(end, bound *string) (*string, error) {
	if bound == nil {
		return end, nil
	}
	if end == nil {
		return bound, nil
	}
	if !endsNoLater(*end, bound) {
		return nil, invalid("Das Enddatum %s liegt nach dem Ende des Budgets darüber (%s). Nichts kann länger laufen als das Budget, aus dem es kommt.", dateOnly(*end), dateOnly(*bound))
	}
	return end, nil
}

// termEnd is the latest end a term of days allows for a project created or
// extended at now: that day, counted in Berlin like every end date. A day rather
// than a second, so a date picked in a browser whose clock runs a little ahead —
// or a request that waited a few minutes for its approval — is not refused over
// the time of day.
func termEnd(now time.Time, days int) string {
	return day.Of(now).AddDays(days).String()
}

// projectBound is the latest end a project below the chain may have at now:
// the earliest budget end, or the end of the shortest term, whichever comes
// first. term reports which of the two it is, for the message that names it.
func projectBound(chain []model.Node, now time.Time) (bound *string, term *int) {
	end := chainEnd(chain)
	days := model.ResolveMaxProjectTermDays(chain)
	if days == nil {
		return end, nil
	}
	byTerm := termEnd(now, *days)
	if end != nil && endsNoLater(*end, &byTerm) {
		return end, nil
	}
	return &byTerm, days
}

// fitProjectEnd is fitEnd for a project, which also obeys the maximum term of
// the budgets above: no end asked means the bound, a later one is refused.
func fitProjectEnd(end *string, chain []model.Node, now time.Time) (*string, error) {
	bound, term := projectBound(chain, now)
	if term == nil {
		return fitEnd(end, bound)
	}
	if end == nil {
		return bound, nil
	}
	if !endsNoLater(*end, bound) {
		return nil, invalid("Das Enddatum %s liegt zu weit in der Zukunft: Projekte in diesem Budget laufen höchstens %d Tage am Stück, also längstens bis %s.", dateOnly(*end), *term, dateOnly(*bound))
	}
	return end, nil
}

// endBound is the latest end a node of kind may have below chain, nearest first, today: for a project projectBound (the earliest budget end or the end of the shortest term, term naming the latter), for a budget the earliest budget end alone. Nil when nothing bounds it. The one place that says which rule bounds which kind of node.
func (s *Service) endBound(kind string, chain []model.Node) (bound *string, term *int) {
	if kind == model.KindProject {
		return projectBound(chain, s.now())
	}
	return chainEnd(chain), nil
}

// fitNodeEnd applies the lifetime rule to the end asked for a node of kind below chain: fitProjectEnd for a project, fitEnd under the chain's end for a budget. No end asked means the bound; a later one is refused.
func (s *Service) fitNodeEnd(kind string, end *string, chain []model.Node) (*string, error) {
	if kind == model.KindProject {
		return fitProjectEnd(end, chain, s.now())
	}
	return fitEnd(end, chainEnd(chain))
}

// fitMaxTerm applies the term rule to the cap asked for a budget below a chain
// whose shortest cap is bound: no cap asked means the bound, a longer one is
// refused — a sub-budget may tighten the rule, not escape it.
func fitMaxTerm(days, bound *int) (*int, error) {
	if days != nil && *days < 1 {
		return nil, invalid("Die längste Laufzeit von Projekten muss mindestens einen Tag betragen.")
	}
	if bound == nil {
		return days, nil
	}
	if days == nil {
		return bound, nil
	}
	if *days > *bound {
		return nil, invalid("Im Budget darüber laufen Projekte höchstens %d Tage, deshalb kann dieses keine %d erlauben.", *bound, *days)
	}
	return days, nil
}

// dateOnly prints an end date as its day for messages (model.EndLabel).
func dateOnly(ts string) string { return model.EndLabel(ts) }

// shortenSubtreeEnds makes every live node below the given parents end no later
// than end: a budget that ends earlier takes its sub-budgets and projects with
// it, pending requests and proposals included. Each node is changed on its
// current stored state (the reconciler writes too) and says why in its history.
// Returns how many nodes it changed. Callers hold approvalMu.
func (s *Service) shortenSubtreeEnds(ctx context.Context, parentIDs []string, end string, actor model.Actor, reason string) (int, error) {
	return s.mutateLiveSubtree(ctx, parentIDs, "end date", func(model.Node) func(*model.Node) error {
		return func(n *model.Node) error { return shortenEnd(n, end, actor, reason) }
	})
}

// shortenSubtreeTerms carries a lowered maximum project term down: sub-budgets
// with a longer cap (or none) take the new one, and every live project below
// that ends later than the term allows from now — or never — is shortened to
// it, waiting requests and proposals included. Returns how many nodes it
// changed. Callers hold approvalMu.
func (s *Service) shortenSubtreeTerms(ctx context.Context, parentIDs []string, days int, now time.Time, actor model.Actor, reason string) (int, error) {
	end := termEnd(now, days)
	return s.mutateLiveSubtree(ctx, parentIDs, "project term", func(child model.Node) func(*model.Node) error {
		if child.Kind == model.KindBudget {
			return func(n *model.Node) error { return shortenTerm(n, days, actor, reason) }
		}
		return func(n *model.Node) error { return shortenEnd(n, end, actor, reason) }
	})
}

// mutateLiveSubtree walks the subtree below parentIDs level by level and changes every live node in it: mutate says, for a node as listed, what to do to its current stored state (nodestore.ErrSkipUpdate where nothing changes). Imports and retired or rejected nodes are skipped, and so is what lies below them. what names the change in errors. Returns how many nodes it changed. Callers hold approvalMu.
func (s *Service) mutateLiveSubtree(ctx context.Context, parentIDs []string, what string, mutate func(child model.Node) func(*model.Node) error) (int, error) {
	changed := 0
	for len(parentIDs) > 0 {
		children, err := s.store.ListNodes(ctx, nodestore.NodeQuery{ParentIDs: parentIDs}, 0, 0)
		if err != nil {
			return changed, fmt.Errorf("load children for %s: %w", what, err)
		}
		parentIDs = nil
		for _, child := range children {
			if model.IsTerminalStatus(child.Status) || child.Status == model.StatusImported {
				continue
			}
			if child.Kind == model.KindBudget {
				parentIDs = append(parentIDs, child.ID)
			}
			wrote, err := s.store.UpdateNode(ctx, child.ID, mutate(child))
			if err != nil {
				return changed, fmt.Errorf("shorten %s of %s: %w", what, nodeLabel(child), err)
			}
			if wrote {
				changed++
			}
		}
	}
	return changed, nil
}

// shortenTerm lowers a budget's maximum project term to days where it allows
// longer or sets none. ErrSkipUpdate when it is already as short.
func shortenTerm(n *model.Node, days int, actor model.Actor, reason string) error {
	if n.MaxProjectTermDays != nil && *n.MaxProjectTermDays <= days {
		return nodestore.ErrSkipUpdate
	}
	n.MaxProjectTermDays = &days
	entry := model.NewHistoryEntry("term_shortened", actor, n.Status)
	entry.Reason = &reason
	n.History = append(slices.Clone(n.History), entry)
	return nil
}

// shortenEnd moves n's end date — and that of a proposal waiting on it — to end
// where it runs longer or has none. ErrSkipUpdate when nothing runs longer.
func shortenEnd(n *model.Node, end string, actor model.Actor, reason string) error {
	longer := func(d *string) bool { return d == nil || !endsNoLater(*d, &end) }
	cutProposal := n.Pending != nil && n.Pending.TerminationDate != nil && longer(n.Pending.TerminationDate)
	cutNode := longer(n.TerminationDate)
	if !cutNode && !cutProposal {
		return nodestore.ErrSkipUpdate
	}
	entry := model.NewHistoryEntry("end_shortened", actor, n.Status)
	entry.Reason = &reason
	if cutNode {
		entry.TerminationDateFrom = n.TerminationDate
		entry.TerminationDateTo = &end
		n.TerminationDate = &end
	}
	if cutProposal {
		if !cutNode {
			entry.TerminationDateFrom = n.Pending.TerminationDate
			entry.TerminationDateTo = &end
		}
		pending := *n.Pending
		pending.TerminationDate = &end
		n.Pending = &pending
	}
	n.History = append(slices.Clone(n.History), entry)
	return nil
}

// endsWithReason is the history note on a node whose end date followed the
// budget it draws from.
func endsWithReason(budget model.Node) string {
	return fmt.Sprintf("Ends with budget %q", nodeLabel(budget))
}

// termReason is the history note on a node shortened to the maximum project
// term of a budget above it.
func termReason(budget model.Node, days int) string {
	return fmt.Sprintf("Projects in budget %q run at most %d days at a time", nodeLabel(budget), days)
}
