package reconciler

import (
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// archiveClient is the part of OpenStack archiving needs; tests substitute a fake.
type archiveClient interface {
	ListProjectServers(projectID string) ([]osclient.ServerInfo, error)
	ShelveServer(serverID string) error
	ListProjectFloatingIPs(projectID string) ([]osclient.FloatingIPInfo, error)
	ReleaseFloatingIP(id string) error
	FreezeProjectQuotas(projectID string) error
	UpdateProject(projectID string, opts osclient.ProjectUpdateOpts) (*projects.Project, error)
}

// shelvable are the server states Nova accepts a shelve from. A server in
// another state (ERROR, BUILD, a migration) is left as it is and named in the
// log; it must not keep the rest of the project from being archived.
var shelvable = []string{"ACTIVE", "SHUTOFF", "PAUSED", "SUSPENDED"}

// archiveResult counts what one archiving did.
type archiveResult struct {
	archived    bool
	shelved     int
	ipsReleased int
}

// archiveReleasedProject archives the OpenStack project of a released leaf so
// it consumes nothing that can be freed without deleting it:
//
//   - the project is disabled — nobody gets a token for it any more;
//   - the quotas that let something new run (instances, cores, RAM, floating
//     IPs) are set to zero;
//   - its floating IPs are released — they are scarce, and this is the one
//     step that cannot be undone, the address may go to someone else;
//   - its servers are shelved — off their hypervisor, CPU and RAM free, the
//     disk kept as an image.
//
// Volumes, networks and routers stay until the project is deleted.
//
// When every step went through, the project gets <ArchivedTagPrefix><date> and
// is left alone from then on: an admin restoring it by hand (enable, unshelve,
// quotas back) is not undone on the next pass. A step that failed leaves the
// tag off, so the next pass tries again.
//
// Reports whether the project is archived now.
func (r *Reconciler) archiveReleasedProject(osProject osclient.ProjectInfo, leaf tree.Node, res *reconcileResult) bool {
	out := archiveProject(r.osClient, osProject, r.cfg.ArchivedTagPrefix, r.cfg.DryRun, time.Now(),
		r.log.With("node_id", leaf.ID, "os_project_id", osProject.ID))
	res.serversShelved += out.shelved
	res.floatingIPsReleased += out.ipsReleased
	if out.archived {
		res.projectsArchived++
	}
	return out.archived
}

func archiveProject(c archiveClient, p osclient.ProjectInfo, tagPrefix string, dryRun bool, now time.Time, log *zap.SugaredLogger) archiveResult {
	var out archiveResult
	if tagPrefix == "" {
		tagPrefix = "archived:"
	}
	if slices.ContainsFunc(p.Tags, func(t string) bool { return strings.HasPrefix(t, tagPrefix) }) {
		return out
	}
	if dryRun {
		log.Infow("Dry run: would archive released project")
		return out
	}
	complete := true

	if err := c.FreezeProjectQuotas(p.ID); err != nil {
		log.Warnw("Archive: could not set quotas to zero", "error", err)
		complete = false
	}

	ips, err := c.ListProjectFloatingIPs(p.ID)
	if err != nil {
		log.Warnw("Archive: could not list floating IPs", "error", err)
		complete = false
	}
	for _, ip := range ips {
		if ip.ProjectID != p.ID {
			log.Errorw("Archive: listing returned a floating IP of another project — skipped", "address", ip.Address, "its_project_id", ip.ProjectID)
			continue
		}
		if err := c.ReleaseFloatingIP(ip.ID); err != nil {
			log.Warnw("Archive: could not release floating IP", "address", ip.Address, "error", err)
			complete = false
			continue
		}
		log.Infow("Archive: released floating IP", "address", ip.Address)
		out.ipsReleased++
	}

	servers, err := c.ListProjectServers(p.ID)
	if err != nil {
		log.Warnw("Archive: could not list servers", "error", err)
		complete = false
	}
	for _, s := range servers {
		if s.ProjectID != p.ID {
			log.Errorw("Archive: listing returned a server of another project — skipped", "server", s.Name, "its_project_id", s.ProjectID)
			continue
		}
		if s.Shelved() {
			continue
		}
		if !slices.Contains(shelvable, s.Status) {
			log.Warnw("Archive: server left as it is, its state cannot be shelved", "server", s.Name, "status", s.Status)
			continue
		}
		if err := c.ShelveServer(s.ID); err != nil {
			log.Warnw("Archive: could not shelve server", "server", s.Name, "error", err)
			complete = false
			continue
		}
		log.Infow("Archive: shelved server", "server", s.Name)
		out.shelved++
	}

	if !complete {
		return out
	}
	// Disabling and marking in one write, and last: the tag says everything
	// above went through, and an admin who restores the project later finds
	// both decisions in one place.
	disabled := false
	tags := append(slices.Clone(p.Tags), tagPrefix+now.UTC().Format(time.DateOnly))
	opts := osclient.ProjectUpdateOpts{Tags: &tags}
	opts.Enabled = &disabled
	if _, err := c.UpdateProject(p.ID, opts); err != nil {
		log.Warnw("Archive: could not disable and mark the project", "error", err)
		return out
	}
	log.Infow("Archived released project", "servers_shelved", out.shelved, "floating_ips_released", out.ipsReleased)
	out.archived = true
	return out
}
