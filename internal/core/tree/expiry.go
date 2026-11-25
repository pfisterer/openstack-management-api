package tree

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/day"
	"github.com/pfisterer/cloud-self-service-golib/loop"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// Expiry is how expired allocations and ending projects are followed up (decided 25.11.2026, "ein Ablauf für beide Fälle"). An allocation whose end has passed — or whose budget was deleted — is only marked: the project keeps what it has, and its owner, admins and the budget's managers are told. GraceDays after that it is cut and the project shrinks by it, but only where a notification service is configured: without one nobody was told, so nothing is cut and the mark stays until somebody acts.
type Expiry struct {
	// GraceDays is the period between marking an allocation as expired and cutting it (EXPIRY_GRACE_DAYS, 14).
	GraceDays int
	// EndingNoticeDays is how long before a project's end its owner and admins are told (EXPIRY_ENDING_NOTICE_DAYS, 14).
	EndingNoticeDays int
	// PortalURL is the self-service portal, for links in notifications (PORTAL_URL); empty sends none.
	PortalURL string
}

// DefaultExpiry is used where SetExpiry sets nothing.
var DefaultExpiry = Expiry{GraceDays: 14, EndingNoticeDays: 14}

// today is the current day in Europe/Berlin by the service's clock.
func (s *Service) today() day.Date { return day.Of(s.now()) }

// ExpiryResult counts what one sweep did.
type ExpiryResult struct {
	Marked   int
	Dated    int
	Cut      int
	Restored int
	Ending   int
}

// endPassed reports whether an end day lies before today.
func endPassed(end *string, today day.Date) bool {
	if end == nil {
		return false
	}
	d, err := model.EndDay(*end)
	if err != nil {
		return false
	}
	return d.Before(today)
}

// SweepExpiry follows up the ends of allocations and projects once:
//   - an allocation that pays and whose end has passed is marked as expired (Node.Expired), with the day it is cut where a notification service is configured; allocation.expired is sent;
//   - a mark made without a notification service gets that day once one is configured, and is sent then;
//   - an allocation whose day to be cut has passed ends (StatusEnded), the project shrinks by it, allocation.shortened is sent;
//   - a mark whose reason is gone (the end was moved later) is removed;
//   - a running project whose end is within EndingNoticeDays is announced once per end (project.ending).
func (s *Service) SweepExpiry(ctx context.Context) (ExpiryResult, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	var res ExpiryResult
	today := s.today()
	actor := model.Actor{Email: systemActor, Via: model.ChannelSystem}

	allocs, err := s.store.ListNodes(ctx, nodestore.NodeQuery{Kinds: []string{model.KindAllocation}, Statuses: model.ActiveStatuses}, 0, 0)
	if err != nil {
		return res, fmt.Errorf("load allocations: %w", err)
	}
	touched := map[string]bool{}
	for _, a := range allocs {
		switch {
		case a.Expired == nil && endPassed(a.TerminationDate, today):
			var marked model.Node
			if _, err := s.store.UpdateNode(ctx, a.ID, func(n *model.Node) error {
				if n.Expired != nil || !endPassed(n.TerminationDate, today) || !isPaying(*n) {
					return nodestore.ErrSkipUpdate
				}
				entry := model.NewHistoryEntry("expired", actor, n.Status)
				entry.Reason = new(fmt.Sprintf("Abgelaufen am %s", model.EndLabel(*n.TerminationDate)))
				n.Expired = &model.Expiry{At: entry.Timestamp, Cause: model.ExpiryEnded, Due: s.expiryDue()}
				n.History = append(slices.Clone(n.History), entry)
				marked = *n
				return nil
			}); err != nil {
				return res, err
			}
			if marked.ID != "" {
				res.Marked++
				s.sendExpired(ctx, marked, actor)
			}
		case a.Expired != nil && a.Expired.Cause == model.ExpiryEnded && !endPassed(a.TerminationDate, today):
			if _, err := s.store.UpdateNode(ctx, a.ID, func(n *model.Node) error {
				if n.Expired == nil || n.Expired.Cause != model.ExpiryEnded || endPassed(n.TerminationDate, today) {
					return nodestore.ErrSkipUpdate
				}
				n.Expired = nil
				return nil
			}); err != nil {
				return res, err
			}
			res.Restored++
		case a.Expired != nil && a.Expired.Due == nil && s.notifier.Enabled():
			var dated model.Node
			if _, err := s.store.UpdateNode(ctx, a.ID, func(n *model.Node) error {
				if n.Expired == nil || n.Expired.Due != nil {
					return nodestore.ErrSkipUpdate
				}
				e := *n.Expired
				e.Due = s.expiryDue()
				n.Expired = &e
				dated = *n
				return nil
			}); err != nil {
				return res, err
			}
			if dated.ID != "" {
				res.Dated++
				s.sendExpired(ctx, dated, actor)
			}
		case a.Expired != nil && a.Expired.Due != nil && s.notifier.Enabled() && !endPassed(a.Expired.Due, today.AddDays(1)):
			// Sent again on every sweep until it is cut or lifted: the notification service drops a key it has seen, and an event lost on the way is not lost for good.
			s.sendExpired(ctx, a, actor)
		case a.Expired != nil && a.Expired.Due != nil && s.notifier.Enabled() && endPassed(a.Expired.Due, today.AddDays(1)):
			reason := "Nach Ablauf der Frist gekürzt"
			if a.Expired.Cause == model.ExpiryBudgetDeleted {
				reason = fmt.Sprintf("Nach Ablauf der Frist gekürzt (Budget %q gelöscht)", a.Expired.BudgetName)
			}
			if err := s.setAllocationStatus(ctx, a.ID, model.StatusEnded, "shortened", actor, reason); err != nil {
				return res, err
			}
			res.Cut++
			touched[a.ProjectID] = true
			project, err := s.refreshProject(ctx, a.ProjectID, actor)
			if err != nil {
				return res, err
			}
			s.notifyShortened(ctx, a, *project, actor, reason)
		}
	}
	for pid := range touched {
		if _, err := s.refreshProject(ctx, pid, actor); err != nil {
			return res, err
		}
	}

	projects, err := s.store.ListNodes(ctx, nodestore.NodeQuery{Kinds: []string{model.KindProject}, Statuses: []string{model.StatusApproved}}, 0, 0)
	if err != nil {
		return res, fmt.Errorf("load projects: %w", err)
	}
	horizon := today.AddDays(s.expiry.EndingNoticeDays)
	for _, p := range projects {
		if p.TerminationDate == nil || p.EndingNotified == *p.TerminationDate || !s.notifier.Enabled() {
			continue
		}
		end, err := model.EndDay(*p.TerminationDate)
		if err != nil || end.After(horizon) || end.Before(today) {
			continue
		}
		var marked model.Node
		if _, err := s.store.UpdateNode(ctx, p.ID, func(n *model.Node) error {
			if n.TerminationDate == nil || n.EndingNotified == *n.TerminationDate {
				return nodestore.ErrSkipUpdate
			}
			n.EndingNotified = *n.TerminationDate
			marked = *n
			return nil
		}); err != nil {
			return res, err
		}
		if marked.ID != "" {
			res.Ending++
			s.notifyEnding(marked)
		}
	}
	return res, nil
}

// sendExpired sends allocation.expired for an allocation just marked or dated.
func (s *Service) sendExpired(ctx context.Context, alloc model.Node, actor model.Actor) {
	project, err := s.store.GetNode(ctx, alloc.ProjectID)
	if err != nil || project == nil {
		return
	}
	s.notifyExpired(ctx, alloc, *project, actor)
}

// RunExpiryLoop runs SweepExpiry every interval (an hour when not positive) until ctx ends, the first time a minute after the start.
func (s *Service) RunExpiryLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	loop.Run(ctx, loop.Schedule{Interval: interval, First: time.Minute}, func(ctx context.Context) error {
		res, err := s.SweepExpiry(ctx)
		if err != nil {
			return err
		}
		if res != (ExpiryResult{}) {
			s.log.Infow("expiry sweep", "marked", res.Marked, "dated", res.Dated, "cut", res.Cut, "restored", res.Restored, "ending", res.Ending)
		}
		return nil
	}, loop.Options{Name: "expiry", Log: s.log})
}
