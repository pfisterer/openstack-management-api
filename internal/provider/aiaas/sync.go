package aiaas

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"go.uber.org/zap"
)

// Tree is what the syncer needs of the tree (tree.ProviderSync).
type Tree interface {
	Leaves(ctx context.Context, statuses []string) ([]model.Node, error)
	RecordObservation(ctx context.Context, leafID, externalID string, o *tree.Observation) error
	RemoveRetired(ctx context.Context, leafID string) (bool, error)
	Changes() <-chan struct{}
}

// Service is what the syncer needs of the AI service (Client).
type Service interface {
	List(ctx context.Context) ([]Status, error)
	Put(ctx context.Context, node string, p Project) (Status, error)
	Delete(ctx context.Context, node string) error
}

// Syncer carries the tree's projects out at the AI service, on an interval and whenever the tree changed.
type Syncer struct {
	Tree      Tree
	Service   Service
	Resources func() []common.Resource
	Interval  time.Duration
	Log       *zap.SugaredLogger
	now       func() time.Time
}

// Result is what one pass did.
type Result struct {
	Synced, Ended, Deleted, Removed, Orphans, Failed int
}

// Run passes once right away and then on every tick and change until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		res, err := s.Pass(ctx)
		if err != nil {
			s.Log.Warnw("aiaas: pass failed", "error", err)
		} else {
			s.Log.Infow("aiaas: pass complete", "synced", res.Synced, "ended", res.Ended, "deleted", res.Deleted, "removed", res.Removed, "orphans", res.Orphans, "failed", res.Failed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.Tree.Changes():
		}
	}
}

var statuses = []string{model.StatusApproved, model.StatusChangePending, model.StatusReleased, model.StatusArchived}

// Pass brings the AI service in line with the tree:
//   - an approved project gets its project there, with its amounts, people and end;
//   - a released or archived one ends there (nobody can start any more) and is deleted when its deletion was requested — then the leaf goes too, unless OpenStack still carries it out;
//   - a project there whose leaf is gone is deleted.
//
// What was used comes back as the leaf's in-use, per window resource.
func (s *Syncer) Pass(ctx context.Context) (Result, error) {
	var res Result
	leaves, err := s.Tree.Leaves(ctx, statuses)
	if err != nil {
		return res, err
	}
	remote, err := s.Service.List(ctx)
	if err != nil {
		return res, err
	}
	resources := s.Resources()
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	known := map[string]bool{}
	for _, leaf := range leaves {
		known[leaf.ID] = true
		retired := model.IsRetiredStatus(leaf.Status)
		if retired && slices.Contains(leaf.Flags, model.FlagDeleteRequested) {
			if err := s.Service.Delete(ctx, leaf.ID); err != nil && !errors.Is(err, ErrNotFound) {
				s.Log.Warnw("aiaas: delete failed", "node_id", leaf.ID, "error", err)
				res.Failed++
				continue
			}
			res.Deleted++
			if gone, err := s.Tree.RemoveRetired(ctx, leaf.ID); err != nil {
				s.Log.Warnw("aiaas: remove leaf failed", "node_id", leaf.ID, "error", err)
			} else if gone {
				res.Removed++
			}
			continue
		}
		p := ProjectFor(leaf, resources)
		if retired {
			// Ended the day before: the project stays, with its usage, but nobody starts in it.
			end := now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
			p.EndsAt = &end
			res.Ended++
		} else {
			res.Synced++
		}
		st, err := s.Service.Put(ctx, leaf.ID, p)
		if err != nil {
			s.Log.Warnw("aiaas: project not carried out", "node_id", leaf.ID, "error", err)
			res.Failed++
			continue
		}
		obs := &tree.Observation{InUse: InUseFrom(st, resources)}
		if err := s.Tree.RecordObservation(ctx, leaf.ID, st.Slug, obs); err != nil {
			s.Log.Warnw("aiaas: recording failed", "node_id", leaf.ID, "error", err)
		}
	}
	for _, st := range remote {
		if known[st.Node] {
			continue
		}
		if err := s.Service.Delete(ctx, st.Node); err != nil && !errors.Is(err, ErrNotFound) {
			s.Log.Warnw("aiaas: deleting a project without a leaf failed", "node_id", st.Node, "error", err)
			res.Failed++
			continue
		}
		res.Orphans++
	}
	return res, nil
}
