package osclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/containerinfra/v1/clusters"
	"github.com/gophercloud/gophercloud/openstack/dns/v2/zones"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/openstack/keymanager/v1/containers"
	"github.com/gophercloud/gophercloud/openstack/keymanager/v1/secrets"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	swiftcontainers "github.com/gophercloud/gophercloud/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/objects"
	"github.com/gophercloud/gophercloud/pagination"
)

// Some services only work on the project a token is scoped to: Heat, Swift and
// Barbican answer an admin from another project with that project's resources,
// not the target's. Emptying a project for deletion therefore signs the service
// user in to the project itself (after taking everyone else out of it, see the
// reconciler) and uses that session for every service beyond the core ones.
// Scoped like this, a listing can only ever return the project's own things.

// ErrNoProjectSession means the client cannot sign in to another project: it
// authenticates with an application credential, which is bound to its own.
var ErrNoProjectSession = errors.New("the service user cannot sign in to another project (application credential)")

// ProjectSession holds service clients signed in to one project. A client is
// nil when the cloud's catalogue has no such service.
type ProjectSession struct {
	ProjectID string
	// CatalogTypes are the service types the cloud offers, for warning about
	// ones nothing here empties.
	CatalogTypes []string

	magnum   *gophercloud.ServiceClient
	heat     *gophercloud.ServiceClient
	blazar   *gophercloud.ServiceClient
	octavia  *gophercloud.ServiceClient
	dns      *gophercloud.ServiceClient
	swift    *gophercloud.ServiceClient
	barbican *gophercloud.ServiceClient
}

// ServiceUserID is the ID of the user this client signs in as.
func (c *OpenStackClient) ServiceUserID() (string, error) {
	res := c.Identity.ProviderClient.GetAuthResult()
	created, ok := res.(tokens.CreateResult)
	if !ok {
		return "", fmt.Errorf("unexpected auth result %T", res)
	}
	user, err := created.ExtractUser()
	if err != nil {
		return "", err
	}
	return user.ID, nil
}

// OpenProjectSession signs the service user in to projectID. The project must
// be enabled and the user must hold a role in it.
func (c *OpenStackClient) OpenProjectSession(projectID string) (*ProjectSession, error) {
	if c.passwordAuth == nil {
		return nil, ErrNoProjectSession
	}
	provider, err := openstack.NewClient(c.authURL)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	provider.HTTPClient = http.Client{Transport: transport}
	if err := openstack.Authenticate(provider, gophercloud.AuthOptions{
		IdentityEndpoint: c.authURL,
		Username:         c.passwordAuth.Username,
		Password:         c.passwordAuth.Password,
		DomainName:       c.passwordAuth.UserDomainName,
		Scope:            &gophercloud.AuthScope{ProjectID: projectID},
	}); err != nil {
		return nil, fmt.Errorf("sign in to project %s: %w", projectID, err)
	}

	s := &ProjectSession{ProjectID: projectID}
	if created, ok := provider.GetAuthResult().(tokens.CreateResult); ok {
		if catalog, err := created.ExtractServiceCatalog(); err == nil {
			for _, e := range catalog.Entries {
				s.CatalogTypes = append(s.CatalogTypes, e.Type)
			}
		}
	}
	eo := gophercloud.EndpointOpts{Region: c.region, Availability: gophercloud.AvailabilityPublic}
	// A service missing from the catalogue leaves its client nil: nothing of
	// that kind can exist in the project, and its stage counts as empty.
	optional := func(sc *gophercloud.ServiceClient, err error) *gophercloud.ServiceClient {
		if err != nil {
			return nil
		}
		return sc
	}
	s.magnum = optional(openstack.NewContainerInfraV1(provider, eo))
	s.heat = optional(openstack.NewOrchestrationV1(provider, eo))
	s.octavia = optional(openstack.NewLoadBalancerV2(provider, eo))
	s.dns = optional(openstack.NewDNSV2(provider, eo))
	s.swift = optional(openstack.NewObjectStorageV1(provider, eo))
	s.barbican = optional(openstack.NewKeyManagerV1(provider, eo))
	s.blazar = optional(serviceClientFor(provider, eo, "reservation"))
	return s, nil
}

// serviceClientFor builds a client for a service gophercloud has no package
// for, from its catalogue entry.
func serviceClientFor(provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts, serviceType string) (*gophercloud.ServiceClient, error) {
	eo.ApplyDefaults(serviceType)
	url, err := provider.EndpointLocator(eo)
	if err != nil {
		return nil, err
	}
	return &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: gophercloud.NormalizeURL(url), Type: serviceType}, nil
}

func (s *ProjectSession) collect(pager pagination.Pager, what string, each func(pagination.Page) ([]Resource, error)) ([]Resource, error) {
	return listResources(pager, what, s.ProjectID, each)
}

// ── Magnum ────────────────────────────────────────────────────────────────────

// ListClusters lists the project's Kubernetes clusters.
//
// Magnum does not scope this by the token: a user with the admin role gets the
// clusters of every project, even when signed in to one. The short listing
// also leaves out project_id, so every cluster looked foreign — the purge
// skipped them all, its own included. The detail listing names the project;
// other projects' clusters are dropped here, since that is how Magnum behaves
// rather than a listing gone wrong.
func (s *ProjectSession) ListClusters() ([]Resource, error) {
	if s.magnum == nil {
		return nil, nil
	}
	return s.collect(clusters.ListDetail(s.magnum, clusters.ListOpts{}), "clusters", func(page pagination.Page) ([]Resource, error) {
		list, err := clusters.ExtractClusters(page)
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(list))
		for _, c := range list {
			if c.ProjectID != "" && c.ProjectID != s.ProjectID {
				continue
			}
			out = append(out, Resource{ID: c.UUID, Name: c.Name, Status: c.Status, ProjectID: c.ProjectID})
		}
		return out, nil
	})
}

// DeleteCluster deletes a cluster; Magnum removes its stack, servers and load
// balancers in the background.
func (s *ProjectSession) DeleteCluster(id string) error {
	return clusters.Delete(s.magnum, id).ExtractErr()
}

// ── Heat ──────────────────────────────────────────────────────────────────────

// ListStacks lists the project's stacks. The session is the project's, so
// these are its own; Heat reports no project per stack. Plain HTTP: the
// gophercloud package drags in a YAML library for template handling.
func (s *ProjectSession) ListStacks() ([]Resource, error) {
	if s.heat == nil {
		return nil, nil
	}
	var body struct {
		Stacks []struct {
			ID     string `json:"id"`
			Name   string `json:"stack_name"`
			Status string `json:"stack_status"`
		} `json:"stacks"`
	}
	if _, err := s.heat.Get(s.heat.ServiceURL("stacks"), &body, nil); err != nil {
		return nil, fmt.Errorf("list stacks of %s: %w", s.ProjectID, err)
	}
	out := make([]Resource, 0, len(body.Stacks))
	for _, st := range body.Stacks {
		out = append(out, Resource{ID: st.ID, Name: st.Name, Status: st.Status, ProjectID: s.ProjectID})
	}
	return out, nil
}

// DeleteStack deletes a stack with everything it created.
func (s *ProjectSession) DeleteStack(name, id string) error {
	_, err := s.heat.Delete(s.heat.ServiceURL("stacks", name, id), &gophercloud.RequestOpts{OkCodes: []int{204}})
	return err
}

// ── Blazar ────────────────────────────────────────────────────────────────────

// ListLeases lists the project's reservations.
func (s *ProjectSession) ListLeases() ([]Resource, error) {
	if s.blazar == nil {
		return nil, nil
	}
	var body struct {
		Leases []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			ProjectID string `json:"project_id"`
		} `json:"leases"`
	}
	if _, err := s.blazar.Get(s.blazar.ServiceURL("leases"), &body, nil); err != nil {
		return nil, fmt.Errorf("list leases of %s: %w", s.ProjectID, err)
	}
	out := make([]Resource, 0, len(body.Leases))
	for _, l := range body.Leases {
		out = append(out, Resource{ID: l.ID, Name: l.Name, Status: l.Status, ProjectID: l.ProjectID})
	}
	return out, nil
}

// DeleteLease deletes a reservation.
func (s *ProjectSession) DeleteLease(id string) error {
	_, err := s.blazar.Delete(s.blazar.ServiceURL("leases", id), &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}})
	return err
}

// ── Octavia ───────────────────────────────────────────────────────────────────

// ListLoadBalancers lists the project's load balancers. Filtered by project
// although the session is the project's: an admin role sees every project's.
func (s *ProjectSession) ListLoadBalancers() ([]Resource, error) {
	if s.octavia == nil {
		return nil, nil
	}
	return s.collect(loadbalancers.List(s.octavia, loadbalancers.ListOpts{ProjectID: s.ProjectID}), "load balancers", func(page pagination.Page) ([]Resource, error) {
		list, err := loadbalancers.ExtractLoadBalancers(page)
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(list))
		for _, lb := range list {
			out = append(out, Resource{ID: lb.ID, Name: lb.Name, Status: lb.ProvisioningStatus, ProjectID: lb.ProjectID})
		}
		return out, nil
	})
}

// DeleteLoadBalancer deletes a load balancer with its listeners, pools and
// members.
func (s *ProjectSession) DeleteLoadBalancer(id string) error {
	return loadbalancers.Delete(s.octavia, id, loadbalancers.DeleteOpts{Cascade: true}).ExtractErr()
}

// ── Designate ─────────────────────────────────────────────────────────────────

// HasDNS and HasObjectStorage report whether the cloud offers the service.
func (s *ProjectSession) HasDNS() bool           { return s.dns != nil }
func (s *ProjectSession) HasObjectStorage() bool { return s.swift != nil }

// ListZones lists the project's DNS zones.
func (s *ProjectSession) ListZones() ([]Resource, error) {
	if s.dns == nil {
		return nil, nil
	}
	return s.collect(zones.List(s.dns, zones.ListOpts{}), "zones", func(page pagination.Page) ([]Resource, error) {
		list, err := zones.ExtractZones(page)
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(list))
		for _, z := range list {
			out = append(out, Resource{ID: z.ID, Name: z.Name, Status: z.Status, ProjectID: z.ProjectID})
		}
		return out, nil
	})
}

// DeleteZone deletes a zone with its records.
func (s *ProjectSession) DeleteZone(id string) error {
	_, err := zones.Delete(s.dns, id).Extract()
	return err
}

// ── Swift ─────────────────────────────────────────────────────────────────────

// ListBuckets lists the project's object storage containers.
func (s *ProjectSession) ListBuckets() ([]Resource, error) {
	if s.swift == nil {
		return nil, nil
	}
	return s.collect(swiftcontainers.List(s.swift, swiftcontainers.ListOpts{}), "containers", func(page pagination.Page) ([]Resource, error) {
		names, err := swiftcontainers.ExtractNames(page)
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(names))
		for _, n := range names {
			out = append(out, Resource{ID: n, Name: n, ProjectID: s.ProjectID})
		}
		return out, nil
	})
}

// EmptyBucket deletes up to limit objects of a container and reports how many
// it deleted and whether the container is empty now.
func (s *ProjectSession) EmptyBucket(name string, limit int) (int, bool, error) {
	var names []string
	err := objects.List(s.swift, name, objects.ListOpts{Limit: limit}).EachPage(func(page pagination.Page) (bool, error) {
		batch, err := objects.ExtractNames(page)
		if err != nil {
			return false, err
		}
		names = append(names, batch...)
		return len(names) < limit, nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("list objects of %s: %w", name, err)
	}
	if len(names) > limit {
		names = names[:limit]
	}
	deleted := 0
	for _, o := range names {
		if _, err := objects.Delete(s.swift, name, o, nil).Extract(); err != nil {
			return deleted, false, fmt.Errorf("delete object %s/%s: %w", name, o, err)
		}
		deleted++
	}
	return deleted, len(names) < limit, nil
}

// DeleteBucket deletes an empty container.
func (s *ProjectSession) DeleteBucket(name string) error {
	_, err := swiftcontainers.Delete(s.swift, name).Extract()
	return err
}

// ── Barbican ──────────────────────────────────────────────────────────────────

// ListSecrets lists the project's secrets; the ID is the last part of the
// secret's reference.
func (s *ProjectSession) ListSecrets() ([]Resource, error) {
	if s.barbican == nil {
		return nil, nil
	}
	return s.collect(secrets.List(s.barbican, secrets.ListOpts{}), "secrets", func(page pagination.Page) ([]Resource, error) {
		list, err := secrets.ExtractSecrets(page)
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(list))
		for _, sec := range list {
			out = append(out, Resource{ID: path.Base(sec.SecretRef), Name: sec.Name, ProjectID: s.ProjectID})
		}
		return out, nil
	})
}

// DeleteSecret deletes a secret.
func (s *ProjectSession) DeleteSecret(id string) error {
	return secrets.Delete(s.barbican, id).ExtractErr()
}

// ListSecretContainers lists the project's secret containers (certificate
// bundles and the like).
func (s *ProjectSession) ListSecretContainers() ([]Resource, error) {
	if s.barbican == nil {
		return nil, nil
	}
	return s.collect(containers.List(s.barbican, containers.ListOpts{}), "secret containers", func(page pagination.Page) ([]Resource, error) {
		list, err := containers.ExtractContainers(page)
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(list))
		for _, ct := range list {
			out = append(out, Resource{ID: path.Base(ct.ContainerRef), Name: ct.Name, ProjectID: s.ProjectID})
		}
		return out, nil
	})
}

// DeleteSecretContainer deletes a secret container (not the secrets in it).
func (s *ProjectSession) DeleteSecretContainer(id string) error {
	return containers.Delete(s.barbican, id).ExtractErr()
}

// ── What nothing here empties ─────────────────────────────────────────────────

// emptiedServiceTypes are the catalogue types whose resources emptying a
// project covers, or that hold nothing of a project's (identity, placement,
// rating, dashboards).
var emptiedServiceTypes = []string{
	"compute", "network", "image", "volume", "volumev2", "volumev3", "block-storage",
	"container-infra", "orchestration", "cloudformation", "reservation", "load-balancer",
	"dns", "object-store", "key-manager",
	"identity", "placement", "rating", "panel", "metric", "alarming", "event",
}

// UnemptiedServiceTypes lists the catalogue's services whose resources
// emptying a project does not cover — what would be left behind.
func (s *ProjectSession) UnemptiedServiceTypes() []string {
	var out []string
	for _, t := range s.CatalogTypes {
		if !slices.Contains(emptiedServiceTypes, strings.ToLower(t)) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// RoleIDByName returns the ID of the role with that name.
func (c *OpenStackClient) RoleIDByName(name string) (string, error) {
	role, err := c.FindRoleByName(name)
	if err != nil {
		return "", err
	}
	if role == nil {
		return "", fmt.Errorf("role %q not found", name)
	}
	return role.ID, nil
}
