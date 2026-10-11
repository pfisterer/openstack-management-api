package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
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

// Source is where the used amounts come from — the system that runs the
// projects, keyed by a project's ID there (the node's OSProjectID). How it
// counts is its business; this package only books what it reports.
type Source interface {
	DayUsage(start, end time.Time) (map[string]Measured, error)
	// PublicIPv4 counts each project's public IPv4 addresses now.
	PublicIPv4() (map[string]int, error)
}

// Nodes is the tree, read-only.
type Nodes interface {
	ListNodes(ctx context.Context, q tree.NodeQuery, limit, offset int) ([]tree.Node, error)
}

// Collector writes one row per project and finished UTC day. It is asked on
// every reconciler pass and does nothing once yesterday is stored, so a pod
// that was down for a while catches up by itself.
type Collector struct {
	store   Store
	nodes   Nodes
	source  Source
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

func NewCollector(store Store, nodes Nodes, source Source, catalog []common.Resource, log *zap.SugaredLogger) *Collector {
	return &Collector{store: store, nodes: nodes, source: source, catalog: catalog, log: log,
		BackfillDays: 365, MaxDaysPerRun: 31, now: time.Now}
}

// CatchUp collects the finished days not stored yet, oldest first. Returns how
// many it collected.
func (c *Collector) CatchUp(ctx context.Context) (int, error) {
	today := dayOf(c.now())
	last, ok, err := c.store.LastDay(ctx)
	if err != nil {
		return 0, fmt.Errorf("last usage day: %w", err)
	}
	next := today.AddDate(0, 0, -c.BackfillDays)
	if ok {
		next = last.AddDate(0, 0, 1)
	}
	done := 0
	for ; next.Before(today) && done < c.MaxDaysPerRun; next = next.AddDate(0, 0, 1) {
		if err := c.CollectDay(ctx, next); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// CollectDay stores the rows of one UTC day, replacing what it held.
func (c *Collector) CollectDay(ctx context.Context, day time.Time) error {
	day = dayOf(day)
	byProject, err := c.source.DayUsage(day, day.AddDate(0, 0, 1))
	if err != nil {
		return err
	}

	all, err := c.nodes.ListNodes(ctx, tree.NodeQuery{}, 0, 0)
	if err != nil {
		return fmt.Errorf("load tree: %w", err)
	}
	budgets := map[string]tree.Node{}
	for _, n := range all {
		if n.Kind == tree.KindBudget {
			budgets[n.ID] = n
		}
	}

	now := c.now()
	// The snapshot is the tree as it is now; only for yesterday is that what
	// it was on the day.
	backfilled := day.Before(dayOf(now).AddDate(0, 0, -1))
	storageID := c.resourceFor(common.AccountsStorage)
	// Addresses are a sample of now, like storage: only for yesterday is that
	// the day being collected. A failed count leaves the day unmeasured rather
	// than failing it.
	var ipv4 map[string]int
	if !backfilled {
		if ipv4, err = c.source.PublicIPv4(); err != nil {
			c.log.Warnw("Usage: could not count public IPv4 addresses, the day goes without", "error", err)
			ipv4 = nil
		}
	}

	var rows []Day
	for _, n := range all {
		if n.Kind != tree.KindProject || n.OSProjectID == "" {
			continue
		}
		u, used := byProject[n.OSProjectID]
		// A project that existed but ran nothing still gets its row: an idle
		// reservation is exactly what utilisation has to show.
		if !used && !holdsResources(n.Status) {
			continue
		}
		people, groups := peopleOf(n)
		row := Day{
			Day: day, NodeID: n.ID, OSProjectID: n.OSProjectID,
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
			row.StorageGB = float64(n.OSInUse[storageID])
		}
		if ipv4 != nil {
			v := ipv4[n.OSProjectID]
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
	case tree.StatusApproved, tree.StatusChangePending, tree.StatusImported, tree.StatusReleased, tree.StatusArchived:
		return true
	}
	return false
}

// resourceFor names the catalogue resource that accounts as what (common.Accounts*).
func (c *Collector) resourceFor(accounts string) string {
	for _, r := range c.catalog {
		if r.Accounts == accounts {
			return r.ID
		}
	}
	return ""
}

// reserved is what the project holds back for itself: its limit, allocations
// included — and once archived only the storage, the rest is frozen at zero.
func (c *Collector) reserved(n tree.Node) common.ProjectQuota {
	q := c.quantities(n.EffectiveLimit())
	if n.Status != tree.StatusArchived {
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
func (c *Collector) quantities(limit common.ProjectQuota) common.ProjectQuota {
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
func pathOf(start string, budgets map[string]tree.Node) []PathEntry {
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
func attributesOf(n tree.Node, budgets map[string]tree.Node) tree.Attributes {
	chain := []tree.Node{n}
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
	groups := tree.ResolveAttributes(chain)
	if len(groups) == 0 {
		return nil
	}
	out := make(tree.Attributes, len(groups))
	for name, g := range groups {
		out[name] = g.Values
	}
	return out
}

// peopleOf counts the distinct persons named on a project and the groups among
// its members.
func peopleOf(n tree.Node) (people, groups int) {
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
