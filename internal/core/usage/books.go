package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"go.uber.org/zap"
)

// Measured is what the system running a project counted for it over one day:
// how long its servers existed, weighted by their size.
type Measured struct {
	ServerHours float64
	VCPUHours   float64
	RAMGBHours  float64
	DiskGBHours float64
}

// DayReport is what the system running the projects reports for one day,
// keyed by a project's ID there (the node's OSProjectID). How it counts is its
// business; this package only books it.
type DayReport struct {
	Usage map[string]Measured
	// PublicIPv4 is each project's public IPv4 addresses, counted now — only
	// for a day that is sampled (see Books.Sampled); nil where nothing was
	// counted, which is not the same as zero.
	PublicIPv4 map[string]int
}

// Nodes is the tree, read-only.
type Nodes interface {
	ListNodes(ctx context.Context, q nodestore.NodeQuery, limit, offset int) ([]model.Node, error)
}

// Books writes one row per project and finished UTC day, from what the
// executing system reports. That system asks on every pass which days are
// missing (DaysToRecord), and there are none once yesterday is stored, so a pod
// that was down for a while catches up by itself.
type Books struct {
	store   Store
	nodes   Nodes
	catalog []common.Resource
	log     *zap.SugaredLogger

	// BackfillDays is how far back the first run reaches. Nova's records go
	// back as far as they have not been archived, so the history from before
	// collection started is not lost — only its snapshot is today's.
	BackfillDays int
	// MaxDaysPerRun bounds one call, so a long backfill is spread over passes
	// instead of holding one up.
	MaxDaysPerRun int
	now           func() time.Time
}

func NewBooks(store Store, nodes Nodes, catalog []common.Resource, log *zap.SugaredLogger) *Books {
	return &Books{store: store, nodes: nodes, catalog: catalog, log: log,
		BackfillDays: 365, MaxDaysPerRun: 31, now: time.Now}
}

// DaysToRecord are the finished UTC days not stored yet, oldest first and at
// most MaxDaysPerRun of them; the first time reaching BackfillDays back.
func (c *Books) DaysToRecord(ctx context.Context) ([]time.Time, error) {
	today := dayOf(c.now())
	last, ok, err := c.store.LastDay(ctx)
	if err != nil {
		return nil, fmt.Errorf("last usage day: %w", err)
	}
	next := today.AddDate(0, 0, -c.BackfillDays)
	if ok {
		next = last.AddDate(0, 0, 1)
	}
	var days []time.Time
	for ; next.Before(today) && len(days) < c.MaxDaysPerRun; next = next.AddDate(0, 0, 1) {
		days = append(days, next)
	}
	return days, nil
}

// Sampled reports whether a day is recent enough for what can only be counted
// now — storage, public addresses: only for yesterday is now what it was on
// the day. Older days are backfilled without them.
func (c *Books) Sampled(day time.Time) bool {
	return !dayOf(day).Before(dayOf(c.now()).AddDate(0, 0, -1))
}

// RecordDay stores the rows of one UTC day from what was reported for it,
// replacing what it held.
func (c *Books) RecordDay(ctx context.Context, day time.Time, report DayReport) error {
	day = dayOf(day)
	byProject := report.Usage

	all, err := c.nodes.ListNodes(ctx, nodestore.NodeQuery{}, 0, 0)
	if err != nil {
		return fmt.Errorf("load tree: %w", err)
	}
	budgets := map[string]model.Node{}
	for _, n := range all {
		if n.Kind == model.KindBudget {
			budgets[n.ID] = n
		}
	}

	now := c.now()
	// The snapshot is the tree as it is now; only for yesterday is that what
	// it was on the day.
	backfilled := !c.Sampled(day)
	storageID := c.resourceFor(common.AccountsStorage)
	// Addresses are a sample of now, like storage: only for yesterday is that
	// the day being recorded. Reported for an older day, they are not used.
	ipv4 := report.PublicIPv4
	if backfilled {
		ipv4 = nil
	}

	var rows []Day
	for _, n := range all {
		if n.Kind != model.KindProject || n.ExternalID == "" {
			continue
		}
		u, used := byProject[n.ExternalID]
		// A project that existed but ran nothing still gets its row: an idle
		// reservation is exactly what utilisation has to show.
		if !used && !holdsResources(n.Status) {
			continue
		}
		people, groups := peopleOf(n)
		row := Day{
			Day: day, NodeID: n.ID, ExternalID: n.ExternalID,
			ProjectName: n.Name, Owner: n.OwnerEmail(), Status: n.Status,
			People: people, Groups: groups,
			ServerHours: u.ServerHours, VCPUHours: u.VCPUHours,
			RAMGBHours: u.RAMGBHours, DiskGBHours: u.DiskGBHours,
			Reserved:   c.reserved(n),
			Backfilled: backfilled, CollectedAt: now,
		}
		if n.ParentID != nil {
			row.BudgetID = *n.ParentID
			row.BudgetPath = pathOf(*n.ParentID, budgets)
		}
		row.Attributes = attributesOf(n, budgets)
		if !backfilled && storageID != "" {
			row.StorageGB = float64(n.InUse[storageID])
		}
		if ipv4 != nil {
			v := ipv4[n.ExternalID]
			row.PublicIPv4 = &v
		}
		rows = append(rows, row)
	}
	if err := c.store.ReplaceDay(ctx, day, rows); err != nil {
		return fmt.Errorf("store usage of %s: %w", day.Format(time.DateOnly), err)
	}
	c.log.Infow("Usage collected", "day", day.Format(time.DateOnly), "projects", len(rows), "backfilled", backfilled)
	return nil
}

// holdsResources reports whether a project in this status holds resources even
// when nothing runs in it — released and archived ones their volumes, until
// they are deleted. Recorded whatever the budget is charged for them.
func holdsResources(status string) bool {
	switch status {
	case model.StatusApproved, model.StatusChangePending, model.StatusImported, model.StatusReleased, model.StatusArchived:
		return true
	}
	return false
}

// resourceFor names the catalogue resource that accounts as what (common.Accounts*).
func (c *Books) resourceFor(accounts string) string {
	for _, r := range c.catalog {
		if r.Accounts == accounts {
			return r.ID
		}
	}
	return ""
}

// reserved is what the project holds back for itself: its limit, allocations
// included — and once archived only the storage, the rest is frozen at zero.
func (c *Books) reserved(n model.Node) common.ProjectQuota {
	q := c.quantities(n.EffectiveLimit())
	if n.Status != model.StatusArchived {
		return q
	}
	out := common.ProjectQuota{}
	for _, r := range c.catalog {
		if r.Accounts == common.AccountsStorage && q[r.ID] != 0 {
			out[r.ID] = q[r.ID]
		}
	}
	return out
}

// quantities keeps the counted resources of a limit — availabilities say
// nothing about consumption.
func (c *Books) quantities(limit common.ProjectQuota) common.ProjectQuota {
	out := common.ProjectQuota{}
	for _, r := range c.catalog {
		if r.IsBool() || r.Static {
			continue
		}
		if v := limit[r.ID]; v != 0 {
			out[r.ID] = v
		}
	}
	return out
}

// pathOf lists the budgets from start up to the root.
func pathOf(start string, budgets map[string]model.Node) []PathEntry {
	var path []PathEntry
	seen := map[string]bool{}
	for id := start; id != "" && !seen[id]; {
		seen[id] = true
		b, ok := budgets[id]
		if !ok {
			break
		}
		path = append(path, PathEntry{ID: b.ID, Name: b.Name})
		if b.ParentID == nil {
			break
		}
		id = *b.ParentID
	}
	return path
}

// attributesOf is what applies to the project: its own attribute groups and
// those inherited from its budgets, values only.
func attributesOf(n model.Node, budgets map[string]model.Node) model.Attributes {
	chain := []model.Node{n}
	seen := map[string]bool{n.ID: true}
	for p := n.ParentID; p != nil && !seen[*p]; {
		seen[*p] = true
		b, ok := budgets[*p]
		if !ok {
			break
		}
		chain = append(chain, b)
		p = b.ParentID
	}
	groups := model.ResolveAttributes(chain)
	if len(groups) == 0 {
		return nil
	}
	out := make(model.Attributes, len(groups))
	for name, g := range groups {
		out[name] = g.Values
	}
	return out
}

// peopleOf counts the distinct persons named on a project and the groups among
// its members.
func peopleOf(n model.Node) (people, groups int) {
	persons := map[string]bool{}
	if e := n.OwnerEmail(); e != "" {
		persons[e] = true
	}
	add := func(token string) {
		switch {
		case strings.HasPrefix(token, "user:"):
			persons[strings.TrimPrefix(token, "user:")] = true
		case strings.HasPrefix(token, "group:"):
			groups++
		}
	}
	for _, t := range n.AdminScope {
		add(t)
	}
	for _, u := range n.AuthorizedUsers {
		add(u.Token)
	}
	return len(persons), groups
}
