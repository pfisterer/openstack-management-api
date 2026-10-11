package osclient

import "github.com/pfisterer/cloud-self-service-golib/envconf"

// DefaultCatalogue is the catalogue a deployment gets when it configures none
// (RESOURCE_DEFINITIONS): cores, RAM, storage and the static network and volume
// quotas, each with its OpenStack mapping.
func DefaultCatalogue() []ManagedResource {
	// Default set — single source of truth for UI display AND OpenStack quota mapping.
	// ShowOnUI: true  → returned to the frontend via /v1/config (user-configurable).
	// Static: true    → quota is fixed at Default; applied once at project creation.
	return []ManagedResource{
		// ── User-configurable resources (shown on UI) ────────────
		{
			ID: "cores", Name: "Cores", Default: 4, Min: 1, Max: 100000,
			Group:             "Compute",
			Message:           "1 - 100000 cores",
			ShowOnUI:          true,
			OSQuotaField:      "cores",
			OSLinkedField:     "instances", // instances cap mirrors cores
			OSOvercommitCheck: true,
		},
		{
			ID: "ram", Name: "RAM", Default: 16, Min: 1, Max: 256000,
			Group:             "Compute",
			Unit:              "GB",
			Message:           "1 GB - 256 TB",
			ShowOnUI:          true,
			OSQuotaField:      "ram",
			OSMultiplier:      1024, // stored in GB, OpenStack expects MB
			OSOvercommitCheck: true,
		},
		{
			ID: "storage", Name: "Storage", Default: 50, Min: 1, Max: 100000,
			Group:    "Storage",
			Unit:     "GB",
			Message:  "1 GB - 100 TB",
			ShowOnUI: true,
			// No OSMultiplier: OpenStack counts `gigabytes` in GB and so do we.
			OSQuotaField: "gigabytes",
			// Measured, like cores and ram — and for the same reason. Without
			// this flag ProjectInUse skips the resource entirely, so storage
			// never reaches the measured in-use and the accounting keeps billing the
			// declared limit. That leaves the shrink-after-filling loophole
			// wide open for exactly the resource that is cheapest to abuse:
			// request 500 GB, fill it with volumes, shrink to 5. OpenStack
			// accepts the smaller quota and keeps the volumes; the books say 5.
			// The number was available all along — it rides in the same quota
			// detail response the reconciler already fetches.
			OSOvercommitCheck: true,
		},

		// ── Static network/storage quotas (not shown on UI, fixed at project creation) ─
		// To change a default, update the Default field here. The OSQuotaField drives the
		// mapping to OpenStack — no other file needs to change.
		{Group: "Infrastructure", ID: "networks", Name: "Networks", Default: envconf.Int("RECONCILER_DEFAULT_NETWORKS", 2),
			Static: true, OSQuotaField: "networks"},
		{Group: "Infrastructure", ID: "subnets", Name: "Subnets", Default: envconf.Int("RECONCILER_DEFAULT_SUBNETS", 4),
			Static: true, OSQuotaField: "subnets"},
		{Group: "Infrastructure", ID: "ports", Name: "Ports", Default: envconf.Int("RECONCILER_DEFAULT_PORTS", 50),
			Static: true, OSQuotaField: "ports"},
		{Group: "Infrastructure", ID: "routers", Name: "Routers", Default: envconf.Int("RECONCILER_DEFAULT_ROUTERS", 1),
			Static: true, OSQuotaField: "routers"},
		{Group: "Infrastructure", ID: "floating_ips", Name: "Floating IPs", Default: envconf.Int("RECONCILER_DEFAULT_FLOATING_IPS", 2),
			Static: true, OSQuotaField: "floating_ips"},
		{Group: "Infrastructure", ID: "security_groups", Name: "Security Groups", Default: envconf.Int("RECONCILER_DEFAULT_SECURITY_GROUPS", 10),
			Static: true, OSQuotaField: "security_groups"},
		{Group: "Infrastructure", ID: "volumes", Name: "Volumes", Default: envconf.Int("RECONCILER_DEFAULT_VOLUMES", 10),
			Static: true, OSQuotaField: "volumes"},
		{Group: "Infrastructure", ID: "snapshots", Name: "Snapshots", Default: envconf.Int("RECONCILER_DEFAULT_SNAPSHOTS", 10),
			Static: true, OSQuotaField: "snapshots"},
	}
}
