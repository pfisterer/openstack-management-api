package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// createOpenstackProjectForLeaf creates a new OpenStack project for an approved leaf
// and applies the full initial quota (managed fields + network defaults).
func (r *Reconciler) createOpenstackProjectForLeaf(_ context.Context, leaf tree.Node, description string) (osclient.ProjectInfo, error) {
	name := buildProjectName(leaf)

	r.log.Infow("Creating OS project for leaf",
		"node_id", leaf.ID, "project_name", name, "dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return osclient.ProjectInfo{ID: "dry-run-" + leaf.ID, Name: name}, nil
	}

	project, err := r.osClient.CreateManagedProject(name, description, r.scopeParentID, leaf.ID)
	if err != nil {
		return osclient.ProjectInfo{}, fmt.Errorf("create project: %w", err)
	}

	// Compose a full quota set: managed resources from the leaf + static defaults.
	// Static defaults (network quotas, volumes, snapshots) are driven entirely by the
	// ManagedProject definitions — no separate DefaultNetworkQuotas struct needed.
	fullQuota := ProjectQuotaToQuotaSet(r.resources(), leaf.EffectiveLimit())
	staticQuota := StaticProjectQuotaDefaults(r.resources())
	mergeStaticIntoQuotaSet(&fullQuota, staticQuota)
	fullQuota.ProjectID = project.ID

	// Retry quota set a few times: Nova/Cinder may not have propagated the new Keystone
	// project yet and returns 503 for a few seconds after creation.
	// If all attempts fail we do NOT delete the orphan — instead we return the project
	// info so the reconciler stores the OSProjectID. On the next tick the project already
	// exists and quota sync goes through syncQuota, which will keep retrying every interval
	// until Nova is healthy again. Deleting and recreating on every failure loops forever.
	const maxQuotaAttempts = 4
	const quotaRetryDelay = 6 * time.Second
	var quotaErr error
	for attempt := 1; attempt <= maxQuotaAttempts; attempt++ {
		quotaErr = r.osClient.UpdateProjectQuotas(project.ID, fullQuota)
		if quotaErr == nil {
			break
		}
		r.log.Warnw("Quota set attempt failed",
			"project_id", project.ID, "attempt", attempt, "max", maxQuotaAttempts, "error", quotaErr)
		if attempt < maxQuotaAttempts {
			time.Sleep(quotaRetryDelay)
		}
	}
	if quotaErr != nil {
		r.log.Warnw("Quota set failed; project created but quota not applied — will retry on next reconcile tick",
			"os_project_id", project.ID, "node_id", leaf.ID, "error", quotaErr)
		// Return the project so the caller persists OSProjectID. The next reconcile cycle
		// will find the project via its tag and call syncQuota, which retries quota updates.
	} else {
		r.log.Infow("OS project created and quota set",
			"node_id", leaf.ID, "os_project_id", project.ID)
	}
	return osclient.ProjectInfo{ID: project.ID, Name: project.Name, Tags: project.Tags}, nil
}

// persistOSProjectID records the project created for a leaf. Written onto the
// node as it is now and regardless of its status: a leaf released meanwhile
// still needs the ID, or its project could never be cleaned up.
func (r *Reconciler) persistOSProjectID(ctx context.Context, leafID, osProjectID string) {
	if err := r.nodes.RecordExternalID(ctx, leafID, osProjectID); err != nil {
		r.log.Warnw("Failed to persist OSProjectID on leaf", "node_id", leafID, "error", err)
	}
}

// persistOSSyncState writes what the pass measured in OpenStack onto the node as
// it is now, touching only those fields.
func (r *Reconciler) persistOSSyncState(ctx context.Context, leafID, osProjectID string, m *tree.Observation) {
	if err := r.nodes.RecordObservation(ctx, leafID, osProjectID, m); err != nil {
		r.log.Warnw("Failed to persist OS sync state on leaf", "node_id", leafID, "error", err)
	}
}

func (r *Reconciler) syncQuota(leaf tree.Node, osProject osclient.ProjectInfo, description string) (*tree.Observation, error) {
	osProjectID := osProject.ID
	quotaSet := ProjectQuotaToQuotaSet(r.resources(), leaf.EffectiveLimit())

	r.log.Debugw("Syncing managed quota",
		"node_id", leaf.ID, "os_project_id", osProjectID,
		"cores", quotaSet.Cores, "ram_mb", quotaSet.RAM, "gigabytes", quotaSet.Gigabytes,
		"dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return nil, nil
	}

	if err := r.osClient.UpdateManagedQuotas(osProjectID, quotaSet); err != nil {
		return nil, fmt.Errorf("update managed quotas: %w", err)
	}

	// Name is only sent when it actually changed: an unchanged name would be a no-op
	// write every tick, and it lets an operator's manual rename of an *imported*
	// project survive until the node itself is renamed.
	updateOpts := osclient.ProjectUpdateOpts{
		BaseProjectOpts: osclient.BaseProjectOpts{Description: &description},
	}
	desiredName := buildProjectName(leaf)
	if desiredName != osProject.Name {
		updateOpts.Name = desiredName
		r.log.Infow("Renaming OS project to match node",
			"node_id", leaf.ID, "os_project_id", osProjectID,
			"old_name", osProject.Name, "new_name", desiredName)
	}
	if _, err := r.osClient.UpdateProject(osProjectID, updateOpts); err != nil {
		r.log.Warnw("Failed to update OS project name/description",
			"node_id", leaf.ID, "os_project_id", osProjectID,
			"desired_name", desiredName, "error", err)
	}

	r.syncManagedTags(leaf, osProject)

	// Overcommit check: OpenStack accepts a quota reduction below current usage but blocks
	// new resource creation. We surface this in the UI via the node's Overcommitted flag.
	detail, err := r.osClient.GetProjectQuotaDetail(osProjectID)
	if err != nil {
		// Not an error for the caller: the quota push above succeeded, so the
		// pass is not a failure. But measured=false, or the caller would store
		// "no usage" for a project it simply could not read.
		r.log.Warnw("Skipping overcommit check (quota detail unavailable); keeping the last known usage",
			"node_id", leaf.ID, "os_project_id", osProjectID, "error", err)
		return nil, nil
	}

	return &tree.Observation{
		Overcommitted: IsProjectOvercommitted(r.resources(), leaf.EffectiveLimit(), detail),
		InUse:         ProjectInUse(r.resources(), detail),
		Servers:       detail.InUse.Instances,
		ServerLimit:   detail.Limit.Instances,
	}, nil
}
