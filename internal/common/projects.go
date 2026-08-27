package common

import "fmt"

// Resource kinds. A kind decides how a value is read, validated and aggregated —
// everything else about a resource is presentation.
//
// Deliberately strings rather than a Go enum: the catalogue is deployment
// configuration now, so a kind has to survive a round trip through JSON. Unknown
// kinds are REJECTED when the catalogue loads (see ValidateManagedProjects); a
// typo must not produce a resource that quietly takes part in nothing.
const (
	// KindCount is a quantity that sums across siblings and is capped by its
	// parent's total. Cores, RAM, storage — the original behaviour, and the
	// default for a definition that names no kind.
	KindCount = "count"

	// KindBool is an availability: a network, an image, a GPU flavour. It is
	// held as 0 or 1 and never sums — three children with a network do not add
	// up to three networks. What a parent grants is not divided among children,
	// it is passed to them.
	KindBool = "bool"

	// KindHours is reserved, not implemented. Consumption over time needs a
	// measurement window and a reset per period, which is a second set of books
	// beside the existing one. It is named here so the aggregation gains one
	// more branch when it arrives rather than being rebuilt.
	KindHours = "hours"
)

// Grant types: what an availability means in OpenStack.
const (
	GrantNetwork = "network" // Neutron RBAC policy, access_as_shared
	GrantImage   = "image"   // Glance image member
	GrantFlavor  = "flavor"  // Nova flavour access
)

// ManagedProject is the single source of truth for a managed resource type.
// It combines the UI-facing definition with the OpenStack mapping, so adding a
// resource is a catalogue entry and not a code change.
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

	// Kind decides the arithmetic (see the Kind* constants). Empty means
	// KindCount, which is what every resource was before kinds existed — an
	// older catalogue keeps working unchanged.
	Kind string `json:"kind,omitempty"`

	// Group is a heading the UI sorts resources under ("Compute", "Networks",
	// "GPU flavours"). Purely presentational, and the reason a catalogue of
	// fifty entries can still be read at the root of the tree.
	Group string `json:"group,omitempty"`

	// Grant says what granting this availability DOES in OpenStack. Required for
	// KindBool and rejected for every other kind: a quantity has a quota field,
	// not a grant.
	Grant *Grant `json:"grant,omitempty"`

	// ShowOnUI controls whether this resource is returned to the frontend via
	// /v1/config. Set to true for user-configurable resources; leave false for
	// static infrastructure quotas that should not be exposed in the UI.
	ShowOnUI bool `json:"show_on_ui,omitempty"`

	// Static marks a resource whose quota is fixed at Default and is not
	// user-configurable per-project. Static resources are applied once at OS
	// project creation using their Default value; the reconciler never asks the
	// user to provide a value for them.
	Static bool `json:"static,omitempty"`

	// ── OpenStack quota mapping ──────────────────────────────────────────────
	//
	// These carry json tags because the catalogue is CONFIGURATION now, and a
	// catalogue replaces the built-in set rather than extending it. Without them
	// a configured deployment would define "cores" with no quota field at all —
	// the portal would grant it, the reconciler would map it to nothing, and
	// every managed project would come up with no quota in OpenStack.
	//
	// They are not sent to the browser: the config endpoint builds its response
	// from an explicit list of display fields (see webserver.uiResource), so
	// adding a field here does not leak it by default.

	// OSQuotaField is the QuotaSet field this resource maps to.
	// Known values: "cores", "ram", "gigabytes", "volumes", "snapshots",
	// "networks", "subnets", "ports", "routers", "floating_ips", "security_groups".
	// Leave empty when the resource has no OpenStack quota equivalent.
	OSQuotaField string `json:"os_quota_field,omitempty"`

	// OSMultiplier converts the stored value to OS units. 0 is treated as 1.
	// Set to 1024 when the resource is stored in GB but OpenStack expects MB (RAM).
	OSMultiplier int `json:"os_multiplier,omitempty"`

	// OSLinkedField, when set, receives the same converted value as OSQuotaField.
	// Used so "instances" in Nova mirrors "cores" (1 instance per core upper bound).
	OSLinkedField string `json:"os_linked_field,omitempty"`

	// OSOvercommitCheck marks this resource for overcommit detection.
	// When true, the reconciler compares OS in-use against OS limit and sets
	// OSOvercommitted on the project if in-use exceeds the configured quota.
	OSOvercommitCheck bool `json:"os_overcommit_check,omitempty"`
}

// Grant is the OpenStack side of an availability: which object, granted how.
//
// Target is an ID rather than a name on purpose. Names are not unique across
// projects in Glance and are editable in Neutron, so a rename elsewhere would
// silently redirect a grant — an ID cannot.
type Grant struct {
	Type   string `json:"type"   validate:"required"`
	Target string `json:"target" validate:"required"`
}

// IsBool reports whether this resource is an availability rather than a
// quantity. Everything that sums, caps or charges asks this first.
func (m ManagedProject) IsBool() bool { return m.Kind == KindBool }

// IsCount reports whether this resource takes part in the arithmetic — summing
// across siblings, capacity checks, usage roll-ups.
//
// An empty kind counts, which is what keeps a catalogue written before kinds
// existed behaving exactly as it did.
func (m ManagedProject) IsCount() bool { return m.Kind == "" || m.Kind == KindCount }

// ValidateManagedProjects checks a catalogue before anything is built on it.
//
// It runs at startup and returns an error rather than dropping bad entries,
// because every silent alternative is worse: a resource with a misspelled kind
// would take part in no arithmetic and no grant while still being requestable,
// and an availability without a grant would be handed out in the portal and mean
// nothing in OpenStack. Both look like the feature working.
func ValidateManagedProjects(defs []ManagedProject) error {
	if len(defs) == 0 {
		return fmt.Errorf("resource catalogue is empty")
	}

	seen := make(map[string]struct{}, len(defs))
	for _, d := range defs {
		if d.ID == "" {
			return fmt.Errorf("resource with an empty id")
		}
		if _, dup := seen[d.ID]; dup {
			// Duplicates cannot be resolved by picking one: quota maps are keyed
			// by id, so the second definition would silently govern the first
			// one's stored values.
			return fmt.Errorf("duplicate resource id %q", d.ID)
		}
		seen[d.ID] = struct{}{}

		switch d.Kind {
		case "", KindCount, KindBool:
		case KindHours:
			return fmt.Errorf("resource %q: kind %q is reserved and not implemented yet", d.ID, KindHours)
		default:
			return fmt.Errorf("resource %q: unknown kind %q", d.ID, d.Kind)
		}

		if err := validateGrant(d); err != nil {
			return err
		}
		if err := validateQuotaMapping(d); err != nil {
			return err
		}
	}

	return nil
}

// validateQuotaMapping rejects a mapping that cannot mean anything.
//
// Both cases describe a resource whose OpenStack side is half-written, which is
// what a hand-authored catalogue produces: a multiplier converts a value for a
// quota field, and measuring in-use reads one back. Neither does anything
// without the field, and neither fails loudly — the resource is simply never
// written to OpenStack, and with the overcommit flag ignored the accounting goes
// back to billing the declared limit.
//
// What this does NOT catch is the whole mapping being absent: a resource may
// legitimately have none (GPUs have no quota field at all), so "cores without a
// quota field" is indistinguishable from a deliberate choice. That one is caught
// by reading the startup log, where the effective catalogue is printed.
func validateQuotaMapping(d ManagedProject) error {
	if d.OSQuotaField != "" {
		return nil
	}
	if d.OSMultiplier != 0 {
		return fmt.Errorf("resource %q: os_multiplier converts a value for a quota field, but none is set", d.ID)
	}
	if d.OSOvercommitCheck {
		return fmt.Errorf("resource %q: os_overcommit_check measures a quota field, but none is set", d.ID)
	}
	if d.OSLinkedField != "" {
		return fmt.Errorf("resource %q: os_linked_field mirrors a quota field, but none is set", d.ID)
	}
	return nil
}

// validateGrant enforces that a grant and an availability imply each other.
func validateGrant(d ManagedProject) error {
	if !d.IsBool() {
		if d.Grant != nil {
			return fmt.Errorf("resource %q: grant is only valid for kind %q", d.ID, KindBool)
		}
		return nil
	}

	if d.Grant == nil {
		return fmt.Errorf("resource %q: kind %q requires a grant", d.ID, KindBool)
	}
	switch d.Grant.Type {
	case GrantNetwork, GrantImage, GrantFlavor:
	default:
		return fmt.Errorf("resource %q: unknown grant type %q", d.ID, d.Grant.Type)
	}
	if d.Grant.Target == "" {
		return fmt.Errorf("resource %q: grant needs a target", d.ID)
	}

	// A quota field on an availability would be applied as a number by the
	// reconciler — 1 core, 1 gigabyte — which is not what "available" means.
	if d.OSQuotaField != "" || d.OSLinkedField != "" {
		return fmt.Errorf("resource %q: an availability maps to a grant, not to a quota field", d.ID)
	}

	return nil
}
