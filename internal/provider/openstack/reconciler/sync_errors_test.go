package reconciler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// neutronOverQuota is the error of incident 19.10.2026, as the client returns it.
func neutronOverQuota() error {
	e := gophercloud.ErrDefault409{ErrUnexpectedResponseCode: gophercloud.ErrUnexpectedResponseCode{
		Method: "POST", URL: "https://network.example/v2.0/rbac-policies", Expected: []int{201}, Actual: 409,
		Body: []byte(`{"NeutronError": {"type": "OverQuota", "message": "Quota exceeded for resources: ['rbac_policy'].", "detail": ""}}`),
	}}
	return fmt.Errorf("share network (network net-1) for project p-1: %w", e)
}

func TestSyncErrorFor_NeutronOverQuota(t *testing.T) {
	e := syncErrorFor(model.SyncKindAvailability, neutronOverQuota())
	if e.Reason != model.SyncOverQuota || e.Kind != model.SyncKindAvailability {
		t.Fatalf("got %+v", e)
	}
	want := "share network (network net-1) for project p-1: HTTP 409 OverQuota: Quota exceeded for resources: ['rbac_policy']."
	if e.Detail != want {
		t.Fatalf("detail = %q, want %q", e.Detail, want)
	}
}

func TestSyncReason(t *testing.T) {
	code := func(c int) error {
		return gophercloud.ErrUnexpectedResponseCode{Actual: c, Expected: []int{200}, Method: "PUT", URL: "https://x"}
	}
	cases := map[string]error{
		model.SyncForbidden:   gophercloud.ErrDefault403{ErrUnexpectedResponseCode: code(403).(gophercloud.ErrUnexpectedResponseCode)},
		model.SyncNotFound:    fmt.Errorf("wrapped: %w", gophercloud.ErrDefault404{ErrUnexpectedResponseCode: code(404).(gophercloud.ErrUnexpectedResponseCode)}),
		model.SyncConflict:    code(409),
		model.SyncUnreachable: code(503),
		model.SyncFailed:      errors.New("something odd"),
	}
	for want, err := range cases {
		if got := syncReason(err); got != want {
			t.Errorf("syncReason(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestSyncDetailIsBounded(t *testing.T) {
	if d := syncDetail(errors.New(strings.Repeat("x", 2000))); len(d) > maxSyncDetail+len("…") {
		t.Fatalf("detail of %d bytes", len(d))
	}
}

// A failed availability is reported by resource; the others are not.
func TestSyncGrants_ReportsWhatFailed(t *testing.T) {
	f := newFakeGrants()
	f.failAdd = true
	leaf := leafWithLimit(common.ProjectQuota{"dhbw-ipv4": 1, "gpu-rtx6000": 0})

	failed := syncGrants(f, grantCatalogue, leaf, "os-project", false, zap.NewNop().Sugar())

	if len(failed) != 1 || failed["dhbw-ipv4"].Kind != model.SyncKindAvailability {
		t.Fatalf("failed = %+v, want just the network", failed)
	}
	if syncGrants(f, grantCatalogue, leaf, "os-project", true, zap.NewNop().Sugar()) != nil {
		t.Fatal("a dry run attempts nothing and reports nothing")
	}
}

// A quota refused by one service is pinned on that service's resources.
func TestQuotaSyncErrors(t *testing.T) {
	defs := []osclient.ManagedResource{
		{ID: "cores", OSQuotaField: "cores"},
		{ID: "ram", OSQuotaField: "ram"},
		{ID: "storage", OSQuotaField: "gigabytes"},
		{ID: "dhbw-ipv4", Kind: common.KindBool, Grant: &netGrant},
	}
	got := quotaSyncErrors(defs, &osclient.QuotaUpdateError{Service: osclient.QuotaServiceBlockStorage, Err: errors.New("boom")})
	if len(got) != 1 || got["storage"].Kind != model.SyncKindQuota {
		t.Fatalf("block storage: %+v", got)
	}
	if got := quotaSyncErrors(defs, errors.New("unknown")); len(got) != 3 {
		t.Fatalf("unknown service: %+v", got)
	}
}
