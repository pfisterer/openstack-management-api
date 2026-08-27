package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/members"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/rbacpolicies"
	"github.com/gophercloud/gophercloud/pagination"

	"github.com/pfisterer/openstack-management-api/internal/common"
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
func (c *OpenStackClient) HasGrant(grant common.Grant, projectID string) (bool, error) {
	switch grant.Type {
	case common.GrantNetwork:
		id, err := c.findNetworkRBAC(grant.Target, projectID)
		return id != "", err
	case common.GrantImage:
		return c.hasImageMember(grant.Target, projectID)
	case common.GrantFlavor:
		return c.hasFlavorAccess(grant.Target, projectID)
	default:
		return false, fmt.Errorf("unknown grant type %q", grant.Type)
	}
}

// AddGrant gives a project one availability. Adding what is already there is not
// an error — the reconciler runs every few minutes, and a grant created between
// its read and its write must not fail the whole pass.
func (c *OpenStackClient) AddGrant(grant common.Grant, projectID string) error {
	switch grant.Type {
	case common.GrantNetwork:
		existing, err := c.findNetworkRBAC(grant.Target, projectID)
		if err != nil {
			return err
		}
		if existing != "" {
			return nil
		}
		_, err = rbacpolicies.Create(c.Network, rbacpolicies.CreateOpts{
			Action:       rbacpolicies.ActionAccessShared,
			ObjectType:   "network",
			ObjectID:     grant.Target,
			TargetTenant: projectID,
		}).Extract()
		return wrapGrant("share network", grant, projectID, err)

	case common.GrantImage:
		err := members.Create(c.Image, grant.Target, projectID).Err
		if err != nil && !isConflict(err) {
			return wrapGrant("add image member", grant, projectID, err)
		}
		// Glance members start as "pending" and only count once accepted. The
		// project itself would normally accept; as the admin creating it we say
		// so directly, or the image stays invisible to the very project we just
		// granted it to.
		err = members.Update(c.Image, grant.Target, projectID,
			members.UpdateOpts{Status: "accepted"}).Err
		return wrapGrant("accept image member", grant, projectID, err)

	case common.GrantFlavor:
		err := flavors.AddAccess(c.Compute, grant.Target,
			flavors.AddAccessOpts{Tenant: projectID}).Err
		if isConflict(err) {
			return nil
		}
		return wrapGrant("add flavor access", grant, projectID, err)

	default:
		return fmt.Errorf("unknown grant type %q", grant.Type)
	}
}

// RemoveGrant takes one availability away again. Removing what is not there is
// not an error, for the same reason adding twice is not.
func (c *OpenStackClient) RemoveGrant(grant common.Grant, projectID string) error {
	switch grant.Type {
	case common.GrantNetwork:
		id, err := c.findNetworkRBAC(grant.Target, projectID)
		if err != nil {
			return err
		}
		if id == "" {
			return nil
		}
		err = rbacpolicies.Delete(c.Network, id).Err
		return wrapGrant("unshare network", grant, projectID, ignoreNotFound(err))

	case common.GrantImage:
		err := members.Delete(c.Image, grant.Target, projectID).Err
		return wrapGrant("remove image member", grant, projectID, ignoreNotFound(err))

	case common.GrantFlavor:
		err := flavors.RemoveAccess(c.Compute, grant.Target,
			flavors.RemoveAccessOpts{Tenant: projectID}).Err
		return wrapGrant("remove flavor access", grant, projectID, ignoreNotFound(err))

	default:
		return fmt.Errorf("unknown grant type %q", grant.Type)
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
	err := rbacpolicies.List(c.Network, rbacpolicies.ListOpts{
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

func (c *OpenStackClient) hasImageMember(imageID, projectID string) (bool, error) {
	_, err := members.Get(c.Image, imageID, projectID).Extract()
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
	err := flavors.ListAccesses(c.Compute, flavorID).EachPage(func(page pagination.Page) (bool, error) {
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
func wrapGrant(what string, grant common.Grant, projectID string, err error) error {
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
