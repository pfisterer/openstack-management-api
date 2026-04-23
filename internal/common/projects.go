package common

// ManagedProject is the single source of truth for a managed resource type.
// It combines the UI-facing definition with the OpenStack quota mapping so that
// adding a new resource type (e.g. "object_storage") requires changing only the
// DefaultManagedResources list in config.go.
//
// OS mapping fields are optional: leave OSQuotaField empty when the resource has
// no standard OpenStack quota (e.g. GPU without custom quota support).
type ManagedProject struct {
	// ── UI definition (returned to the frontend via /v1/config) ──────────────
	ID      string `json:"id"      validate:"required"`
	Name    string `json:"name"    validate:"required"`
	Default int    `json:"default"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Unit    string `json:"unit,omitempty"`
	Message string `json:"message,omitempty"`

	// ShowOnUI controls whether this resource is returned to the frontend via
	// /v1/config. Set to true for user-configurable resources; leave false for
	// static infrastructure quotas that should not be exposed in the UI.
	ShowOnUI bool `json:"show_on_ui,omitempty"`

	// Static marks a resource whose quota is fixed at Default and is not
	// user-configurable per-project. Static resources are applied once at OS
	// project creation using their Default value; the reconciler never asks the
	// user to provide a value for them.
	Static bool `json:"-"`

	// ── OpenStack quota mapping (server-side, ignored by the frontend) ────────

	// OSQuotaField is the QuotaSet field this resource maps to.
	// Known values: "cores", "ram", "gigabytes", "volumes", "snapshots",
	// "networks", "subnets", "ports", "routers", "floating_ips", "security_groups".
	// Leave empty when the resource has no OpenStack quota equivalent.
	OSQuotaField string `json:"-"`

	// OSMultiplier converts the stored value to OS units. 0 is treated as 1.
	// Set to 1024 when the resource is stored in GB but OpenStack expects MB (RAM).
	OSMultiplier int `json:"-"`

	// OSLinkedField, when set, receives the same converted value as OSQuotaField.
	// Used so "instances" in Nova mirrors "cores" (1 instance per core upper bound).
	OSLinkedField string `json:"-"`

	// OSOvercommitCheck marks this resource for overcommit detection.
	// When true, the reconciler compares OS in-use against OS limit and sets
	// OSOvercommitted on the project if in-use exceeds the configured quota.
	OSOvercommitCheck bool `json:"-"`
}
