package nodestore

import (
	"fmt"
	"math"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// LLM tokens at their real price (decided 25.11.2026). The AI service turned a project's llm-mtokens into a LiteLLM money budget at 0.1 per million, on the assumption that every model costs 1e-7 per token; LiteLLM prices the default model at 5e-7 per input token, so a "million tokens" here bought about a fifth of that. The AI service now uses 0.5 per million (aiaas-management-api, LLM_DOLLARS_PER_MTOKENS), and every stored amount is converted once with the factor below, so the money behind each budget, allocation and project stays what it was and the token numbers become honest.
const (
	// LLMTokenResource is the resource whose amounts are converted.
	LLMTokenResource = "llm-mtokens"
	// LLMTokenFactor is old price / new price (0.1 / 0.5).
	LLMTokenFactor = 0.2
	// llmRepricingMigration names the one-time conversion in node_migrations.
	llmRepricingMigration = "llm-mtokens-reprice-0.1-to-0.5"
)

// RepricedTokens converts one stored amount: unlimited stays unlimited, 0 stays 0, anything else is the amount × LLMTokenFactor rounded to the nearest million, at least 1 — an amount that was granted does not become nothing.
func RepricedTokens(v int) int {
	if v <= 0 {
		return v
	}
	n := int(math.Round(float64(v) * LLMTokenFactor))
	return max(n, 1)
}

// TokenRepricingReport says what the conversion did.
type TokenRepricingReport struct {
	Nodes   int      `json:"nodes"`
	Amounts int      `json:"amounts"`
	Inexact int      `json:"inexact"`
	Samples []string `json:"samples,omitempty"`
}

// RepriceLLMTokens converts every stored llm-mtokens amount of the nodes — a node's limit, a waiting change, a per-requester cap — and then sets each project's amount to the sum of its paying allocations, so the cache agrees with them after rounding. Returns the nodes to write.
func RepriceLLMTokens(nodes []model.Node) ([]model.Node, TokenRepricingReport) {
	var rep TokenRepricingReport
	convert := func(q common.ProjectQuota) bool {
		v, ok := q[LLMTokenResource]
		if !ok || v <= 0 {
			return false
		}
		n := RepricedTokens(v)
		q[LLMTokenResource] = n
		rep.Amounts++
		if float64(n) != float64(v)*LLMTokenFactor {
			rep.Inexact++
		}
		return true
	}
	changed := map[string]bool{}
	byID := map[string]*model.Node{}
	for i := range nodes {
		n := &nodes[i]
		byID[n.ID] = n
		touched := false
		if n.Kind != model.KindProject && n.Limit != nil {
			before := n.Limit[LLMTokenResource]
			if convert(n.Limit) && len(rep.Samples) < 12 {
				rep.Samples = append(rep.Samples, fmt.Sprintf("%s %q: %d → %d", n.Kind, n.Name, before, n.Limit[LLMTokenResource]))
			}
			touched = touched || before != n.Limit[LLMTokenResource]
		}
		if n.Pending != nil && n.Pending.Limit != nil {
			q := common.ProjectQuota{}
			for k, v := range *n.Pending.Limit {
				q[k] = v
			}
			if convert(q) {
				p := *n.Pending
				p.Limit = &q
				n.Pending = &p
				touched = true
			}
		}
		if n.AutoApprove != nil && n.AutoApprove.PerRequesterLimit != nil && convert(n.AutoApprove.PerRequesterLimit) {
			touched = true
		}
		if touched {
			changed[n.ID] = true
		}
	}
	// A project's amount is the sum of its paying allocations, as refreshProject keeps it; with legacy projects (in the tree, own limit) converted like any leaf.
	allocs := map[string][]model.Node{}
	for _, n := range nodes {
		if n.Kind == model.KindAllocation {
			allocs[n.ProjectID] = append(allocs[n.ProjectID], n)
		}
	}
	for i := range nodes {
		p := &nodes[i]
		if p.Kind != model.KindProject || p.Limit == nil {
			continue
		}
		if _, has := p.Limit[LLMTokenResource]; !has {
			continue
		}
		before := p.Limit[LLMTokenResource]
		if own, ok := allocs[p.ID]; ok && p.ParentID == nil {
			sum := SumAllocations(own, ProjectChargedStatuses(p.Status))
			if v, has := sum[LLMTokenResource]; has {
				p.Limit[LLMTokenResource] = v
			} else {
				p.Limit[LLMTokenResource] = RepricedTokens(before)
			}
		} else {
			p.Limit[LLMTokenResource] = RepricedTokens(before)
		}
		if p.Limit[LLMTokenResource] != before {
			changed[p.ID] = true
			if len(rep.Samples) < 12 {
				rep.Samples = append(rep.Samples, fmt.Sprintf("project %q: %d → %d", p.Name, before, p.Limit[LLMTokenResource]))
			}
		}
	}
	var out []model.Node
	for _, n := range nodes {
		if changed[n.ID] {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b model.Node) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	rep.Nodes = len(out)
	return out, rep
}

// repriceLLMTokens runs RepriceLLMTokens once per database, inside the migration's transaction, and records it in node_migrations — also on an empty database, so amounts written later in the new price are never converted again.
func repriceLLMTokens(tx *gorm.DB, log *zap.SugaredLogger, rep *AllocationMigrationReport) error {
	if err := tx.Exec(`CREATE TABLE IF NOT EXISTS node_migrations (name text PRIMARY KEY, done_at timestamptz NOT NULL DEFAULT now())`).Error; err != nil {
		return fmt.Errorf("node_migrations: %w", err)
	}
	var done int64
	if err := tx.Raw(`SELECT count(*) FROM node_migrations WHERE name = ?`, llmRepricingMigration).Scan(&done).Error; err != nil {
		return err
	}
	if done > 0 {
		return nil
	}
	var rows []nodeRow
	if err := tx.Find(&rows).Error; err != nil {
		return err
	}
	nodes, err := fromNodeRows(rows)
	if err != nil {
		return err
	}
	writes, r := RepriceLLMTokens(nodes)
	for _, n := range writes {
		row := toNodeRow(n)
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("write %s: %w", n.ID, err)
		}
	}
	if err := tx.Exec(`INSERT INTO node_migrations (name) VALUES (?)`, llmRepricingMigration).Error; err != nil {
		return err
	}
	rep.Tokens = &r
	if log != nil {
		log.Infow("LLM token amounts converted to the real price (× 0.2, the money stays)", "nodes", r.Nodes, "amounts", r.Amounts, "inexact", r.Inexact, "samples", r.Samples)
	}
	return nil
}
