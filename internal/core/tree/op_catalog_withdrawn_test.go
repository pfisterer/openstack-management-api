package tree

import (
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// A withdrawn availability stays known — the zeros the withdrawal wrote must
// validate, or every budget carrying it would be unsavable — but it is offered
// to nobody and cannot be granted again.
func TestWithdrawnAvailability_KnownButNotGrantable(t *testing.T) {
	svc := availabilityService(t)
	withdrawn := slices.Clone(availabilityCatalogue)
	withdrawn[1].Withdrawn = true
	svc.UseCatalog(common.StaticCatalog(withdrawn))

	if err := svc.validateKnownResources(common.ProjectQuota{"cores": 2, "dhbw-ipv4": 0}); err != nil {
		t.Errorf("a stored 0 must validate, got %v", err)
	}
	err := svc.validateKnownResources(common.ProjectQuota{"dhbw-ipv4": 1})
	if err == nil || !strings.Contains(err.Error(), "withdrawn") {
		t.Errorf("granting a withdrawn availability: err = %v, want refused", err)
	}
	for _, n := range []model.Node{{ID: model.RootNodeID}, {ID: "b", Limit: common.ProjectQuota{"dhbw-ipv4": 1}}} {
		if slices.Contains(svc.availableResourcesFor(n), "dhbw-ipv4") {
			t.Errorf("%s still offers the withdrawn availability", n.ID)
		}
	}
	root := model.Node{ID: model.RootNodeID, Limit: common.ProjectQuota{"cores": -1}}
	if added := svc.adoptNewCatalogueResources(&root); slices.Contains(added, "dhbw-ipv4") {
		t.Error("the root must not adopt a withdrawn availability at startup")
	}
}
