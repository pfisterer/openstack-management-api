package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/members"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/rbacpolicies"
	"github.com/gophercloud/gophercloud/pagination"
)

// Availabilities, granted and revoked in OpenStack.
//
// Each kind is a different service with a different notion of "this project may
// use this thing", and none of them is a quota:
//
//   - a network is shared with a project by an RBAC policy (Neutron),
//   - an image by a member entry (Glance),
//   - a flavour by an access entry (Nova).
//
// The three are read and written through one pair of methods so the reconciler
// does not grow a switch per call site.

// HasGrant reports whether a project already holds one availability.
func (c *OpenStackClient) HasGrant(grant Grant, projectID string) (bool, error) {
	switch grant.Type {
	case GrantNetwork:
		id, err := c.findNetworkRBAC(grant.Target, projectID)
		return id != "", err
	case GrantImage:
		return c.hasImageMember(grant.Target, projectID)
	case GrantFlavor:
		return c.hasFlavorAccess(grant.Target, projectID)
	default:
		return false, fmt.Errorf("unknown grant type %q", grant.Type)
	}
}

// AddGrant gives a project one availability and reports whether anything
// changed. Adding what is already there is not an error — the reconciler runs
// every few minutes, and a grant created between its read and its write must not
// fail the whole pass.
//
// The bool is what makes an access change visible: the reconciler calls this on
// every pass, so logging each call would be noise, and logging nothing leaves a
// change to who may reach a network with no trace at all. Every branch below
// already knows the answer — it was being thrown away.
func (c *OpenStackClient) AddGrant(grant Grant, projectID string) (bool, error) {
	switch grant.Type {
	case GrantNetwork:
		existing, err := c.findNetworkRBAC(grant.Target, projectID)
		if err != nil {
			return false, err
		}
		if existing != "" {
			return false, nil
		}
		_, err = rbacpolicies.Create(c.networkSvc(), rbacpolicies.CreateOpts{
			Action:       rbacpolicies.ActionAccessShared,
			ObjectType:   "network",
			ObjectID:     grant.Target,
			TargetTenant: projectID,
		}).Extract()
		return err == nil, wrapGrant("share network", grant, projectID, err)

	case GrantImage:
		err := members.Create(c.imageSvc(), grant.Target, projectID).Err
		fresh := err == nil
		if err != nil && !isConflict(err) {
			return false, wrapGrant("add image member", grant, projectID, err)
		}
		// Glance members start as "pending" and only count once accepted. The
		// project itself would normally accept; as the admin creating it we say
		// so directly, or the image stays invisible to the very project we just
		// granted it to.
		err = members.Update(c.imageSvc(), grant.Target, projectID,
			members.UpdateOpts{Status: "accepted"}).Err
		return fresh && err == nil, wrapGrant("accept image member", grant, projectID, err)

	case GrantFlavor:
		err := flavors.AddAccess(c.computeSvc(), grant.Target,
			flavors.AddAccessOpts{Tenant: projectID}).Err
		if isConflict(err) {
			return false, nil
		}
		return err == nil, wrapGrant("add flavor access", grant, projectID, err)

	default:
		return false, fmt.Errorf("unknown grant type %q", grant.Type)
	}
}

// RemoveGrant takes one availability away again and reports whether anything
// changed. Removing what is not there is not an error, for the same reason
// adding twice is not.
func (c *OpenStackClient) RemoveGrant(grant Grant, projectID string) (bool, error) {
	switch grant.Type {
	case GrantNetwork:
		id, err := c.findNetworkRBAC(grant.Target, projectID)
		if err != nil {
			return false, err
		}
		if id == "" {
			return false, nil
		}
		err = rbacpolicies.Delete(c.networkSvc(), id).Err
		return err == nil, wrapGrant("unshare network", grant, projectID, ignoreNotFound(err))

	case GrantImage:
		err := members.Delete(c.imageSvc(), grant.Target, projectID).Err
		return err == nil, wrapGrant("remove image member", grant, projectID, ignoreNotFound(err))

	case GrantFlavor:
		err := flavors.RemoveAccess(c.computeSvc(), grant.Target,
			flavors.RemoveAccessOpts{Tenant: projectID}).Err
		return err == nil, wrapGrant("remove flavor access", grant, projectID, ignoreNotFound(err))

	default:
		return false, fmt.Errorf("unknown grant type %q", grant.Type)
	}
}

// findNetworkRBAC returns the id of the policy sharing this network with this
// project, or "" when there is none.
//
// Neutron has no "get the policy for (network, project)" call, so the list is
// filtered server-side as far as it goes and matched here. The policy id is
// needed because deletion takes the POLICY, not the pair that describes it.
func (c *OpenStackClient) findNetworkRBAC(networkID, projectID string) (string, error) {
	var found string
	err := rbacpolicies.List(c.networkSvc(), rbacpolicies.ListOpts{
		ObjectType:   "network",
		ObjectID:     networkID,
		TargetTenant: projectID,
	}).EachPage(func(page pagination.Page) (bool, error) {
		policies, err := rbacpolicies.ExtractRBACPolicies(page)
		if err != nil {
			return false, err
		}
		for _, p := range policies {
			// The action is checked here rather than in ListOpts: a policy
			// granting access_as_external to the same pair is a different grant,
			// and deleting it would take away something we never gave.
			if p.Action == rbacpolicies.ActionAccessShared {
				found = p.ID
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("list network rbac policies for %s: %w", networkID, err)
	}
	return found, nil
}

// NetworkShare is one RBAC policy sharing a network with a project (access_as_shared).
type NetworkShare struct {
	PolicyID  string
	NetworkID string
	// TargetProject is the project the network is shared with, "*" for everyone.
	TargetProject string
	// OwnerProject is the project that owns the policy: the one the token that created it was scoped to, and the one whose rbac_policy quota it counts against.
	OwnerProject string
}

// ListNetworkShares lists the policies sharing a network as access_as_shared, whoever created them.
func (c *OpenStackClient) ListNetworkShares(networkID string) ([]NetworkShare, error) {
	var out []NetworkShare
	err := rbacpolicies.List(c.networkSvc(), rbacpolicies.ListOpts{ObjectType: "network", ObjectID: networkID}).
		EachPage(func(page pagination.Page) (bool, error) {
			ps, err := rbacpolicies.ExtractRBACPolicies(page)
			if err != nil {
				return false, err
			}
			for _, p := range ps {
				if p.Action != rbacpolicies.ActionAccessShared || p.ObjectType != "network" || p.ObjectID != networkID {
					continue
				}
				owner := p.ProjectID
				if owner == "" {
					owner = p.TenantID
				}
				out = append(out, NetworkShare{PolicyID: p.ID, NetworkID: p.ObjectID, TargetProject: p.TargetTenant, OwnerProject: owner})
			}
			return true, nil
		})
	return out, wrapList("network rbac policies", networkID, err)
}

// DeleteNetworkShare deletes one RBAC policy; one that is gone already counts as deleted.
func (c *OpenStackClient) DeleteNetworkShare(policyID string) error {
	if err := ignoreNotFound(rbacpolicies.Delete(c.networkSvc(), policyID).Err); err != nil {
		return fmt.Errorf("delete network rbac policy %s: %w", policyID, err)
	}
	return nil
}

// GrantOwnerProject is the project the network client's token is scoped to: the owner of every RBAC policy this service creates. "" when the token is not scoped to a project or cannot be read.
func (c *OpenStackClient) GrantOwnerProject() string {
	svc := c.networkSvc()
	if svc == nil || svc.ProviderClient == nil {
		return ""
	}
	var project *tokens.Project
	switch r := svc.ProviderClient.GetAuthResult().(type) {
	case tokens.CreateResult:
		project, _ = r.ExtractProject()
	case *tokens.CreateResult:
		project, _ = r.ExtractProject()
	case tokens.GetResult:
		project, _ = r.ExtractProject()
	}
	if project == nil {
		return ""
	}
	return project.ID
}

// ProjectExists asks Keystone whether a project exists. Only a 404 counts as "does not exist": a refusal (403) or any other error is returned as an error, so a caller deleting things for projects that are gone never acts on a project it merely may not see.
func (c *OpenStackClient) ProjectExists(projectID string) (bool, error) {
	_, err := projects.Get(c.userLookupSvc(), projectID).Extract()
	if err == nil {
		return true, nil
	}
	if _, notFound := err.(gophercloud.ErrDefault404); notFound {
		return false, nil
	}
	return false, fmt.Errorf("get project %s: %w", projectID, err)
}

func (c *OpenStackClient) hasImageMember(imageID, projectID string) (bool, error) {
	_, err := members.Get(c.imageSvc(), imageID, projectID).Extract()
	if err != nil {
		if _, notFound := err.(gophercloud.ErrDefault404); notFound {
			return false, nil
		}
		return false, fmt.Errorf("get image member %s/%s: %w", imageID, projectID, err)
	}
	return true, nil
}

func (c *OpenStackClient) hasFlavorAccess(flavorID, projectID string) (bool, error) {
	var found bool
	err := flavors.ListAccesses(c.computeSvc(), flavorID).EachPage(func(page pagination.Page) (bool, error) {
		accesses, err := flavors.ExtractAccesses(page)
		if err != nil {
			return false, err
		}
		for _, a := range accesses {
			if a.TenantID == projectID {
				found = true
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("list flavor access for %s: %w", flavorID, err)
	}
	return found, nil
}

// wrapGrant names the grant in the error. Without it the message says "409" and
// leaves the reader to work out which of forty availabilities it was about.
func wrapGrant(what string, grant Grant, projectID string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s (%s %s) for project %s: %w", what, grant.Type, grant.Target, projectID, err)
}

func isConflict(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(gophercloud.ErrDefault409)
	return ok
}

// ignoreNotFound treats "it is already gone" as success: the desired state is
// reached either way, and a reconciler that fails on it would retry forever.
func ignoreNotFound(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(gophercloud.ErrDefault404); ok {
		return nil
	}
	return err
}
