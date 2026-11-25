package tree

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/notify"
)

// Events for the notification service (package notify). The service resolves roles to people itself: the owner, the project's admins and the managers of a budget become recipients here, a group in an admin scope stays a group. Nothing here may fail an operation; without a notification service every call is a no-op.

// systemActor is the actor of what the service does by itself (the expiry sweep).
const systemActor = "system:openstack-management-api"

// actorOf is who an event names as having caused it: the person acting. What the service does by itself (the expiry sweep) has no person behind it, and the notification service takes only a mail address there, so it goes without one.
func actorOf(a model.Actor) string {
	if a.Email == "" || strings.HasPrefix(a.Email, "system") {
		return ""
	}
	return a.Email
}

// recipientsOf turns token lists into recipients, each once: user:x a person ({"user": x}), group:y a group as its userset ({"group": "group:y"}).
func recipientsOf(lists ...common.TokenList) []notify.Recipient {
	seen := map[string]bool{}
	out := []notify.Recipient{}
	for _, list := range lists {
		for _, t := range list {
			key := strings.ToLower(t)
			if seen[key] {
				continue
			}
			switch {
			case strings.HasPrefix(t, "user:"):
				seen[key] = true
				out = append(out, notify.Recipient{User: strings.TrimPrefix(t, "user:")})
			case strings.HasPrefix(t, "group:"):
				// The userset as the role provider names it: group:<id> or group:<id>#<relation>.
				seen[key] = true
				out = append(out, notify.Recipient{Group: t})
			}
		}
	}
	return out
}

// projectHolders are the owner and the admins of a project.
func projectHolders(p model.Node) common.TokenList {
	out := common.TokenList{}
	if p.Owner != "" {
		out = append(out, p.Owner)
	}
	return append(out, p.AdminScope...)
}

// budgetManagers are the managers of a budget: its own admin scope, or that of the nearest budget above that has one (a budget for structure only has nobody of its own).
func (s *Service) budgetManagers(ctx context.Context, budgetID string) (common.TokenList, string) {
	chain, err := s.nodeChain(ctx, budgetID)
	if err != nil || len(chain) == 0 {
		return nil, budgetID
	}
	for _, b := range chain {
		if len(b.AdminScope) > 0 {
			return b.AdminScope, nodeLabel(chain[0])
		}
	}
	return nil, nodeLabel(chain[0])
}

// projectURL is the portal's page of a project, empty without a portal address.
func (s *Service) projectURL(id string) string {
	base := strings.TrimRight(s.expiry.PortalURL, "/")
	if base == "" {
		return ""
	}
	return base + "/projects?project=" + url.QueryEscape(id)
}

func (s *Service) projectObject(p model.Node) notify.Object {
	return notify.Object{Kind: model.KindProject, ID: p.ID, Name: p.Name, URL: s.projectURL(p.ID)}
}

// expiryDue is the day an allocation marked as expired today is cut: GraceDays from today. Nil without a notification service — then nothing is cut automatically (decided 25.11.2026), because nobody would have been told.
func (s *Service) expiryDue() *string {
	if !s.notifier.Enabled() {
		return nil
	}
	return new(s.today().AddDays(s.expiry.GraceDays).String())
}

// dueRFC3339 is an end day as the contract's due: the start of the day after it in Berlin, the moment it has passed.
func dueRFC3339(day *string) string {
	if day == nil {
		return ""
	}
	d, err := model.EndDay(*day)
	if err != nil {
		return ""
	}
	return d.End().Format("2006-01-02T15:04:05Z07:00")
}

// allocationData describes an allocation in an event.
func allocationData(a model.Node, budgetName string) map[string]any {
	data := map[string]any{
		"allocation_id": a.ID,
		"budget_id":     derefParent(a.ParentID),
		"budget_name":   budgetName,
		"limit":         a.Limit,
		"status":        a.Status,
	}
	if a.TerminationDate != nil {
		data["termination_date"] = *a.TerminationDate
	}
	if a.Pending != nil {
		if a.Pending.Limit != nil {
			data["requested_limit"] = *a.Pending.Limit
		}
		if a.Pending.TerminationDate != nil {
			data["requested_termination_date"] = *a.Pending.TerminationDate
		}
	}
	if a.Expired != nil {
		data["expired_cause"] = a.Expired.Cause
		if a.Expired.BudgetName != "" {
			data["deleted_budget_name"] = a.Expired.BudgetName
		}
	}
	return data
}

// notifyWaiting tells the managers of an allocation's budget that it waits for their decision, and remembers the key on the allocation so the decision resolves it. Callers hold approvalMu.
func (s *Service) notifyWaiting(ctx context.Context, alloc model.Node, project model.Node, actor model.Actor) {
	if !s.notifier.Enabled() || alloc.ParentID == nil {
		return
	}
	key := fmt.Sprintf("approval-waiting:%s:%d", alloc.ID, alloc.CurrentRevision())
	if _, err := s.store.UpdateNode(ctx, alloc.ID, func(n *model.Node) error {
		n.WaitingKey = key
		return nil
	}); err != nil {
		s.log.Warnw("approval notification key not stored", "node_id", alloc.ID, "error", err)
	}
	managers, name := s.budgetManagers(ctx, *alloc.ParentID)
	data := allocationData(alloc, name)
	data["requested_by"] = actorOf(actor)
	s.notifier.Notify(notify.Event{Type: notify.TypeApprovalWaiting, Key: key, Class: notify.ClassAction, Recipients: recipientsOf(managers), Actor: actorOf(actor), Object: s.projectObject(project), Data: data})
}

// resolveWaiting closes the approval notification of an allocation that was decided, withdrawn or moved (before is the allocation as it was), and tells whoever asked how it was decided.
func (s *Service) resolveWaiting(ctx context.Context, before model.Node, actor model.Actor) {
	if before.WaitingKey == "" || !s.notifier.Enabled() || before.ParentID == nil {
		return
	}
	managers, name := s.budgetManagers(ctx, *before.ParentID)
	project := model.Node{ID: before.ProjectID, Name: before.Name, Kind: model.KindProject}
	if p, err := s.store.GetNode(ctx, before.ProjectID); err == nil && p != nil {
		project = *p
	}
	data := allocationData(before, name)
	s.notifier.Notify(notify.Event{Type: notify.TypeApprovalWaiting, Key: before.WaitingKey, Class: notify.ClassAction, Resolve: true, Recipients: recipientsOf(managers), Actor: actorOf(actor), Object: s.projectObject(project), Data: data})
}

// notifyDecided tells the person who asked how an allocation request was decided.
func (s *Service) notifyDecided(ctx context.Context, before, after model.Node, decision string, reason *string, actor model.Actor) {
	if !s.notifier.Enabled() || after.ParentID == nil {
		return
	}
	requester := requesterOf(before)
	if requester == "" {
		return
	}
	_, name := s.budgetManagers(ctx, *after.ParentID)
	project := model.Node{ID: after.ProjectID, Name: after.Name, Kind: model.KindProject}
	if p, err := s.store.GetNode(ctx, after.ProjectID); err == nil && p != nil {
		project = *p
	}
	data := allocationData(after, name)
	data["decision"] = decision
	if reason != nil && strings.TrimSpace(*reason) != "" {
		data["reason"] = strings.TrimSpace(*reason)
	}
	key := fmt.Sprintf("approval-decided:%s:%d", after.ID, after.CurrentRevision())
	s.notifier.Notify(notify.Event{Type: notify.TypeApprovalDecided, Key: key, Class: notify.ClassInfo, Recipients: recipientsOf(common.TokenList{"user:" + requester}), Actor: actorOf(actor), Object: s.projectObject(project), Data: data})
}

// requesterOf is who last asked for something on an allocation: the actor of its latest request entry.
func requesterOf(n model.Node) string {
	for i := len(n.History) - 1; i >= 0; i-- {
		h := n.History[i]
		if slices.Contains([]string{"created", "change_requested", "amended"}, h.Event) && h.Actor != "" && !strings.HasPrefix(h.Actor, "system") {
			return h.Actor
		}
	}
	return strings.TrimPrefix(n.Owner, "user:")
}

// notifyExpired tells the owner, the admins of the project and the managers of the allocation's budget that an allocation expired and what happens next.
func (s *Service) notifyExpired(ctx context.Context, alloc model.Node, project model.Node, actor model.Actor) {
	if !s.notifier.Enabled() || alloc.ParentID == nil || alloc.Expired == nil {
		return
	}
	managers, name := s.budgetManagers(ctx, *alloc.ParentID)
	data := allocationData(alloc, name)
	s.notifier.Notify(notify.Event{Type: notify.TypeAllocationExpired, Key: "allocation-expired:" + alloc.ID, Class: notify.ClassAction, Recipients: recipientsOf(projectHolders(project), managers), Actor: actorOf(actor), Due: dueRFC3339(alloc.Expired.Due), Object: s.projectObject(project), Data: data})
}

// notifyShortened tells the same people that a project lost an allocation it was using: cut after its period, or withdrawn by the budget's managers.
func (s *Service) notifyShortened(ctx context.Context, alloc model.Node, project model.Node, actor model.Actor, reason string) {
	if !s.notifier.Enabled() || alloc.ParentID == nil {
		return
	}
	managers, name := s.budgetManagers(ctx, *alloc.ParentID)
	data := allocationData(alloc, name)
	data["reason"] = reason
	data["project_limit"] = project.Limit
	if p, err := s.store.GetNode(ctx, project.ID); err == nil && p != nil {
		data["project_limit"] = p.Limit
	}
	s.notifier.Notify(notify.Event{Type: notify.TypeAllocationShortened, Key: "allocation-shortened:" + alloc.ID, Class: notify.ClassDisruption, Recipients: recipientsOf(projectHolders(project), managers), Actor: actorOf(actor), Object: s.projectObject(project), Data: data})
	// The expiry is settled with it.
	if alloc.Expired != nil || slices.ContainsFunc(alloc.History, func(h model.HistoryEntry) bool { return h.Event == "expired" }) {
		s.notifier.Notify(notify.Event{Type: notify.TypeAllocationExpired, Key: "allocation-expired:" + alloc.ID, Class: notify.ClassAction, Resolve: true, Recipients: recipientsOf(projectHolders(project), managers), Actor: actorOf(actor), Object: s.projectObject(project), Data: data})
	}
}

// notifyEnding tells the owner and admins that their project ends soon.
func (s *Service) notifyEnding(project model.Node) {
	if !s.notifier.Enabled() || project.TerminationDate == nil {
		return
	}
	data := map[string]any{"termination_date": *project.TerminationDate}
	s.notifier.Notify(notify.Event{Type: notify.TypeProjectEnding, Key: "project-ending:" + project.ID + ":" + *project.TerminationDate, Class: notify.ClassAction, Recipients: recipientsOf(projectHolders(project)), Due: dueRFC3339(project.TerminationDate), Object: s.projectObject(project), Data: data})
}
