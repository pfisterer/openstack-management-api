package reconciler

import (
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// osID is the leaf's OpenStack project ID; empty while none was created.
func osID(leaf model.Node) string { return leaf.Binding(osclient.Provider).ExternalID }
