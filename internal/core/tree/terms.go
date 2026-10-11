package tree

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// endsNoLater reports whether the RFC 3339 date next is not after bound; a nil
// bound means "no end" and bounds nothing. Unparseable dates answer false, so
// they take the path through a human rather than past one.
func endsNoLater(next string, bound *string) bool {
	if bound == nil {
		return true
	}
	n, err := time.Parse(time.RFC3339, next)
	if err != nil {
		return false
	}
	b, err := time.Parse(time.RFC3339, *bound)
	if err != nil {
		return false
	}
	return !n.After(b)
}

// chainEnd is the earliest end date along a chain of budgets — the date no node
// below them may outlive. Nil when none of them ends. Unparseable dates bound
// nothing; the budget form never writes one.
func chainEnd(chain []model.Node) *string {
	var earliest *string
	var earliestAt time.Time
	for _, n := range chain {
		if n.TerminationDate == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, *n.TerminationDate)
		if err != nil {
			continue
		}
		if earliest == nil || at.Before(earliestAt) {
			earliest, earliestAt = n.TerminationDate, at
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
		return nil, fmt.Errorf("the end date %s is after the end of the budget above (%s) — nothing can outlive the budget it draws from", dateOnly(*end), dateOnly(*bound))
	}
	return end, nil
}

// chainMaxTerm is the shortest maximum project term along a chain of budgets —
// the one every project below them obeys. Nil when none of them sets one.
func chainMaxTerm(chain []model.Node) *int {
	var shortest *int
	for _, n := range chain {
		if n.MaxProjectTermDays != nil && (shortest == nil || *n.MaxProjectTermDays < *shortest) {
			shortest = n.MaxProjectTermDays
		}
	}
	return shortest
}

// termEnd is the latest end a term of days allows for a project created or
// extended at now: the end of that day, in UTC. The whole day rather than the
// second, so a date picked in a browser whose clock runs a little ahead — or
// a request that waited a few minutes for its approval — is not refused over
// the time of day.
func termEnd(now time.Time, days int) string {
	day := now.UTC().AddDate(0, 0, days)
	return time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, time.UTC).Format(time.RFC3339)
}

// projectBound is the latest end a project below the chain may have at now:
// the earliest budget end, or the end of the shortest term, whichever comes
// first. term reports which of the two it is, for the message that names it.
func projectBound(chain []model.Node, now time.Time) (bound *string, term *int) {
	end := chainEnd(chain)
	days := chainMaxTerm(chain)
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
		return nil, fmt.Errorf("the end date %s is too far ahead: projects in this budget run at most %d days at a time, so until %s at the latest", dateOnly(*end), *term, dateOnly(*bound))
	}
	return end, nil
}

// fitMaxTerm applies the term rule to the cap asked for a budget below a chain
// whose shortest cap is bound: no cap asked means the bound, a longer one is
// refused — a sub-budget may tighten the rule, not escape it.
func fitMaxTerm(days, bound *int) (*int, error) {
	if days != nil && *days < 1 {
		return nil, fmt.Errorf("the maximum project term must be at least one day")
	}
	if bound == nil {
		return days, nil
	}
	if days == nil {
		return bound, nil
	}
	if *days > *bound {
		return nil, fmt.Errorf("projects can run at most %d days in the budget above, so this one cannot allow %d", *bound, *days)
	}
	return days, nil
}

// dateOnly prints an RFC 3339 timestamp as its day for messages.
func dateOnly(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format(time.DateOnly)
	}
	return ts
}

// shortenSubtreeEnds makes every live node below the given parents end no later
// than end: a budget that ends earlier takes its sub-budgets and projects with
// it, pending requests and proposals included. Each node is changed on its
// current stored state (the reconciler writes too) and says why in its history.
// Returns how many nodes it changed. Callers hold approvalMu.
func (s *Service) shortenSubtreeEnds(ctx context.Context, parentIDs []string, end string, actor model.Actor, reason string) (int, error) {
	changed := 0
	for len(parentIDs) > 0 {
		children, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: parentIDs}, 0, 0)
		if err != nil {
			return changed, fmt.Errorf("load children for end date: %w", err)
		}
		parentIDs = nil
		for _, child := range children {
			if model.IsTerminalStatus(child.Status) || child.Status == model.StatusImported {
				continue
			}
			if child.Kind == model.KindBudget {
				parentIDs = append(parentIDs, child.ID)
			}
			wrote, err := s.store.UpdateNode(ctx, child.ID, func(n *model.Node) error {
				return shortenEnd(n, end, actor, reason)
			})
			if err != nil {
				return changed, fmt.Errorf("shorten end of %s: %w", nodeLabel(child), err)
			}
			if wrote {
				changed++
			}
		}
	}
	return changed, nil
}

// shortenSubtreeTerms carries a lowered maximum project term down: sub-budgets
// with a longer cap (or none) take the new one, and every live project below
// that ends later than the term allows from now — or never — is shortened to
// it, waiting requests and proposals included. Returns how many nodes it
// changed. Callers hold approvalMu.
func (s *Service) shortenSubtreeTerms(ctx context.Context, parentIDs []string, days int, now time.Time, actor model.Actor, reason string) (int, error) {
	end := termEnd(now, days)
	changed := 0
	for len(parentIDs) > 0 {
		children, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: parentIDs}, 0, 0)
		if err != nil {
			return changed, fmt.Errorf("load children for project term: %w", err)
		}
		parentIDs = nil
		for _, child := range children {
			if model.IsTerminalStatus(child.Status) || child.Status == model.StatusImported {
				continue
			}
			mutate := func(n *model.Node) error { return shortenEnd(n, end, actor, reason) }
			if child.Kind == model.KindBudget {
				parentIDs = append(parentIDs, child.ID)
				mutate = func(n *model.Node) error { return shortenTerm(n, days, actor, reason) }
			}
			wrote, err := s.store.UpdateNode(ctx, child.ID, mutate)
			if err != nil {
				return changed, fmt.Errorf("shorten project term of %s: %w", nodeLabel(child), err)
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
		return ErrSkipUpdate
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
		return ErrSkipUpdate
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
