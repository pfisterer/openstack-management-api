package reconciler

import (
	"testing"

	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// Nova reports memory in MB-hours; the books keep GB-hours.
func TestMeasuredFrom(t *testing.T) {
	got := measuredFrom([]osclient.TenantUsage{{ProjectID: "os-1", ServerHours: 24, VCPUHours: 96, MemoryMBHours: 24 * 8192, LocalGBHours: 480}})
	m := got["os-1"]
	if m.ServerHours != 24 || m.VCPUHours != 96 || m.RAMGBHours != 192 || m.DiskGBHours != 480 {
		t.Errorf("got %+v", m)
	}
}
