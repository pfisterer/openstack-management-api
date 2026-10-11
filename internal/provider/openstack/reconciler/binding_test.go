package reconciler

import (
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// boundTo is a leaf's bindings when its OpenStack project is osID.
func boundTo(osID string) map[string]model.Binding {
	return map[string]model.Binding{osclient.Provider: {ExternalID: osID}}
}
