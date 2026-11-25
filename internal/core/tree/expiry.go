package tree

import (
	"context"
	"fmt"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/day"
	"github.com/pfisterer/cloud-self-service-golib/loop"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// Expiry is how the ends of projects are followed up (decided 25.11.2026, revised the same day): a project ends at the earliest end of its allocations, so no allocation runs out while its project goes on — except one the project was extended without (Node.EndsAlone), which ends at its own date. Nothing is marked or cut afterwards.
type Expiry struct {
	// EndingNoticeDays is how long before a project's end its owner and admins are told (EXPIRY_ENDING_NOTICE_DAYS, 14).
	EndingNoticeDays int
	// PortalURL is the self-service portal, for links in notifications (PORTAL_URL); empty sends none.
	PortalURL string
}

// DefaultExpiry is used where SetExpiry sets nothing.
var DefaultExpiry = Expiry{EndingNoticeDays: 14}

// today is the current day in Europe/Berlin by the service's clock.
func (s *Service) today() day.Date { return day.Of(s.now()) }

// ExpiryResult counts what one sweep did.
type ExpiryResult struct {
	Ended  int
	Ending int
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

// SweepExpiry follows up the ends once:
//   - an allocation the project was extended without (EndsAlone) ends once its own end has passed, the project shrinks by it, and allocation.shortened is sent;
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
	for _, a := range allocs {
		if !a.EndsAlone || !endPassed(a.TerminationDate, today) {
			continue
		}
		reason := fmt.Sprintf("Endet am %s; das Projekt wurde ohne diese Zuteilung verlängert", model.EndLabel(*a.TerminationDate))
		if err := s.setAllocationStatus(ctx, a.ID, model.StatusEnded, "allocation_ended", actor, reason); err != nil {
			return res, err
		}
		res.Ended++
		project, err := s.refreshProject(ctx, a.ProjectID, actor)
		if err != nil {
			return res, err
		}
		s.notifyShortened(ctx, a, *project, actor, reason)
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
			s.log.Infow("expiry sweep", "ended", res.Ended, "ending", res.Ending)
		}
		return nil
	}, loop.Options{Name: "expiry", Log: s.log})
}
