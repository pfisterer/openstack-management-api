package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/members"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/rbacpolicies"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/pagination"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// CheckGrantTarget reports why a target cannot carry the availability, before
// root admins offer it. What the reconciler cannot do itself is exactly what is
// checked: the object has to exist, a flavour has to be private (Nova keeps an
// access list only for those), and an image has to be shared and owned by the
// scope parent (Glance takes members only from the owner).
func (c *OpenStackClient) CheckGrantTarget(g common.Grant, scopeParentID string) error {
	switch g.Type {
	case common.GrantNetwork:
		if _, err := networks.Get(c.networkSvc(), g.Target).Extract(); err != nil {
			return describeLookup("network", g.Target, err)
		}
		return nil

	case common.GrantFlavor:
		f, err := flavors.Get(c.computeSvc(), g.Target).Extract()
		if err != nil {
			return describeLookup("flavor", g.Target, err)
		}
		if f.IsPublic {
			return fmt.Errorf("flavor %s (%s) is public — only a private flavor can be granted per project", f.Name, g.Target)
		}
		return nil

	case common.GrantImage:
		img, err := images.Get(c.imageSvc(), g.Target).Extract()
		if err != nil {
			return describeLookup("image", g.Target, err)
		}
		if img.Visibility != images.ImageVisibilityShared {
			return fmt.Errorf("image %s (%s) is %s — it has to be shared", img.Name, g.Target, img.Visibility)
		}
		if scopeParentID != "" && img.Owner != scopeParentID {
			return fmt.Errorf("image %s (%s) belongs to project %s — it has to belong to the scope parent %s", img.Name, g.Target, img.Owner, scopeParentID)
		}
		return nil
	}
	return fmt.Errorf("unknown grant type %q", g.Type)
}

func describeLookup(kind, id string, err error) error {
	if _, notFound := err.(gophercloud.ErrDefault404); notFound {
		return fmt.Errorf("%s %s does not exist (or is not visible to the service user)", kind, id)
	}
	return fmt.Errorf("look up %s %s: %w", kind, id, err)
}

// GrantedProjects lists the projects the target is granted to, by whoever.
// A network shared with everyone shows as "*".
func (c *OpenStackClient) GrantedProjects(g common.Grant) ([]string, error) {
	var out []string
	switch g.Type {
	case common.GrantNetwork:
		err := rbacpolicies.List(c.networkSvc(), rbacpolicies.ListOpts{ObjectType: "network", ObjectID: g.Target}).
			EachPage(func(page pagination.Page) (bool, error) {
				ps, err := rbacpolicies.ExtractRBACPolicies(page)
				if err != nil {
					return false, err
				}
				for _, p := range ps {
					if p.Action == rbacpolicies.ActionAccessShared {
						out = append(out, p.TargetTenant)
					}
				}
				return true, nil
			})
		return out, wrapList("network rbac policies", g.Target, err)

	case common.GrantFlavor:
		err := flavors.ListAccesses(c.computeSvc(), g.Target).EachPage(func(page pagination.Page) (bool, error) {
			as, err := flavors.ExtractAccesses(page)
			if err != nil {
				return false, err
			}
			for _, a := range as {
				out = append(out, a.TenantID)
			}
			return true, nil
		})
		return out, wrapList("flavor access", g.Target, err)

	case common.GrantImage:
		err := members.List(c.imageSvc(), g.Target).EachPage(func(page pagination.Page) (bool, error) {
			ms, err := members.ExtractMembers(page)
			if err != nil {
				return false, err
			}
			for _, m := range ms {
				out = append(out, m.MemberID)
			}
			return true, nil
		})
		return out, wrapList("image members", g.Target, err)
	}
	return nil, fmt.Errorf("unknown grant type %q", g.Type)
}

func wrapList(what, target string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("list %s of %s: %w", what, target, err)
}
