package osclient

import (
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Grant types: what an availability means in OpenStack.
const (
	GrantNetwork = "network" // Neutron RBAC policy, access_as_shared
	GrantImage   = "image"   // Glance image member
	GrantFlavor  = "flavor"  // Nova flavour access
)

// Grant is the OpenStack side of an availability: which object, granted how.
//
// Target is an ID rather than a name on purpose. Names are not unique across
// projects in Glance and are editable in Neutron, so a rename elsewhere would
// silently redirect a grant — an ID cannot.
type Grant struct {
	Type   string `json:"type"   validate:"required"`
	Target string `json:"target" validate:"required"`
}

// ManagedResource is one entry of the resource catalogue as a deployment
// configures it: the resource the tree knows (see Core) and how it is carried
// out in OpenStack. The tree only ever sees the Core part; the OpenStack
// mapping stays on this side.
type ManagedResource struct {
	// ── The resource, as the tree and the browser know it (common.Resource) ──
	ID      string `json:"id"      validate:"required"`
	Name    string `json:"name"    validate:"required"`
	Default int    `json:"default"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Unit    string `json:"unit,omitempty"`
	Message string `json:"message,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Group   string `json:"group,omitempty"`
	// ShowOnUI and Static: see common.Resource.
	ShowOnUI bool `json:"show_on_ui,omitempty"`
	Static   bool `json:"static,omitempty"`
	// Withdrawn: see common.Resource. Never read from configuration.
	Withdrawn bool `json:"-"`

	// ── How it is carried out in OpenStack ───────────────────────────────────
	//
	// These carry json tags because the catalogue is CONFIGURATION, and a
	// catalogue replaces the built-in set rather than extending it. Without them
	// a configured deployment would define "cores" with no quota field at all —
	// the portal would grant it, the reconciler would map it to nothing, and
	// every managed project would come up with no quota in OpenStack.
	//
	// They are not sent to the browser: the config endpoint builds its response
	// from common.Resource, which does not have them.

	// Grant says what granting this availability DOES in OpenStack. Required for
	// KindBool and rejected for every other kind: a quantity has a quota field,
	// not a grant.
	Grant *Grant `json:"grant,omitempty"`

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

func (m ManagedResource) IsBool() bool  { return m.Kind == common.KindBool }
func (m ManagedResource) IsCount() bool { return m.Kind == "" || m.Kind == common.KindCount }

// Core is the resource as the tree knows it. What it means in the usage books
// is read from the quota field it maps to — the tree is told "this is storage",
// not "this is Cinder's gigabytes".
func (m ManagedResource) Core() common.Resource {
	r := common.Resource{
		ID: m.ID, Name: m.Name, Default: m.Default, Min: m.Min, Max: m.Max,
		Unit: m.Unit, Message: m.Message, Kind: m.Kind, Group: m.Group,
		ShowOnUI: m.ShowOnUI, Static: m.Static, Withdrawn: m.Withdrawn,
	}
	switch m.OSQuotaField {
	case "cores":
		r.Accounts = common.AccountsCores
	case "ram":
		// Nova counts RAM in MB; a catalogue in GB carries a multiplier of 1024.
		mult := m.OSMultiplier
		if mult <= 0 {
			mult = 1
		}
		r.Accounts, r.AccountsPerGB = common.AccountsRAM, float64(mult)/1024
	case "gigabytes":
		r.Accounts = common.AccountsStorage
	}
	if m.Grant != nil {
		r.GrantType = m.Grant.Type
	}
	return r
}

// CoreResources is Core for a whole catalogue.
func CoreResources(defs []ManagedResource) []common.Resource {
	out := make([]common.Resource, len(defs))
	for i, d := range defs {
		out[i] = d.Core()
	}
	return out
}

// ManagedCatalog is where the reconciler reads the current catalogue with its
// OpenStack side. Root admins add and remove availabilities while the service
// runs, so it is asked on every pass.
type ManagedCatalog interface {
	Managed() []ManagedResource
}

// StaticManaged is a catalogue that never changes — configuration without
// runtime additions, and tests.
type StaticManaged []ManagedResource

func (c StaticManaged) Managed() []ManagedResource { return c }

// ValidateManagedResources checks a catalogue before anything is built on it:
// what the tree needs (common.ValidateResources), and that every OpenStack
// mapping can mean something.
//
// It runs at startup and returns an error rather than dropping bad entries,
// because every silent alternative is worse: an availability without a grant
// would be handed out in the portal and mean nothing in OpenStack. That looks
// like the feature working.
func ValidateManagedResources(defs []ManagedResource) error {
	if err := common.ValidateResources(CoreResources(defs)); err != nil {
		return err
	}
	for _, d := range defs {
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
func validateQuotaMapping(d ManagedResource) error {
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
func validateGrant(d ManagedResource) error {
	if !d.IsBool() {
		if d.Grant != nil {
			return fmt.Errorf("resource %q: grant is only valid for kind %q", d.ID, common.KindBool)
		}
		return nil
	}

	if d.Grant == nil {
		return fmt.Errorf("resource %q: kind %q requires a grant", d.ID, common.KindBool)
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
