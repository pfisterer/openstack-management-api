package common

import (
	"fmt"
	"slices"
)

// Resource kinds. A kind decides how a value is read, validated and aggregated —
// everything else about a resource is presentation.
//
// Deliberately strings rather than a Go enum: the catalogue is deployment
// configuration now, so a kind has to survive a round trip through JSON. Unknown
// kinds are REJECTED when the catalogue loads (see ValidateResources); a
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

	// KindWindow is an amount per time window: "200 GPU hours per 30 days".
	// It sums and caps like a count — the projects under a budget may not be
	// granted more hours per window than the budget has — but what is measured
	// against it is what was consumed in the current window, not what is held
	// right now. Window names the window.
	KindWindow = "window"
)

// The windows a KindWindow resource may have: calendar windows in UTC, the
// same the aiaas provider books in.
var Windows = []string{"24h", "7d", "30d"}

// What a resource means in the usage books and in the charge of an archived
// project — told to the tree by the system that carries it out, so the tree
// does not have to know that "storage" is Cinder's gigabytes.
const (
	AccountsCores   = "cores"
	AccountsRAM     = "ram"
	AccountsStorage = "storage"
)

// Resource is a resource type as the tree knows it: how it is entered,
// validated, summed and shown. How it is carried out — which OpenStack quota
// field, which network — is the executing system's business and not here
// (for OpenStack: osclient.ManagedResource).
type Resource struct {
	// ── UI definition (returned to the frontend via /v1/config) ──────────────
	ID      string `json:"id"`
	Name    string `json:"name"`
	Default int    `json:"default"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Unit    string `json:"unit,omitempty"`
	Message string `json:"message,omitempty"`

	// Kind decides the arithmetic (see the Kind* constants). Empty means
	// KindCount, which is what every resource was before kinds existed — an
	// older catalogue keeps working unchanged.
	Kind string `json:"kind,omitempty"`

	// Provider is the system that carries the resource out ("openstack",
	// "aiaas"). A project is that provider's business as soon as it holds
	// more than zero of one of its resources. Empty only in catalogues written
	// before providers existed, and then it counts for every provider.
	Provider string `json:"provider,omitempty"`

	// Window is the time window of a KindWindow resource (see Windows); empty
	// for every other kind.
	Window string `json:"window,omitempty"`

	// Group is a heading the UI sorts resources under ("Compute", "Networks",
	// "GPU flavours"). Purely presentational, and the reason a catalogue of
	// fifty entries can still be read at the root of the tree.
	Group string `json:"group,omitempty"`

	// GPUClass is set on the two resources of a GPU class of the AI service: its pool and tier, so the UI can arrange them by pool.
	GPUClass *GPUClassInfo `json:"gpu_class,omitempty"`

	// ShowOnUI controls whether this resource is returned to the frontend via
	// /v1/config. Set to true for user-configurable resources; leave false for
	// static infrastructure quotas that should not be exposed in the UI.
	ShowOnUI bool `json:"show_on_ui,omitempty"`

	// Static marks a resource whose quota is fixed at Default and is not
	// user-configurable per-project. Static resources are applied once at
	// project creation using their Default value; nobody is asked for a value.
	Static bool `json:"static,omitempty"`

	// Withdrawn marks an availability on its way out of the catalogue: still
	// known, so stored zeros validate and the executing system revokes it
	// everywhere, but no longer offered and never grantable.
	Withdrawn bool `json:"-"`

	// Accounts is what the resource reserves in the usage books (Accounts*
	// constants), empty for everything else; AccountsPerGB converts its value
	// to GB where the books count GB (RAM). 0 means 1.
	Accounts      string  `json:"-"`
	AccountsPerGB float64 `json:"-"`

	// GrantType is the kind of an availability (a network, a flavour, an
	// image), for showing how its use is counted. The target is not here.
	GrantType string `json:"-"`
}

// GPUClassInfo is what the UI shows of a GPU class: the pool it belongs to (with the GPU types of its nodes and whether it lends idle GPUs to the other pools of the tier) and its tier.
type GPUClassInfo struct {
	Pool     string `json:"pool"`
	PoolName string `json:"pool_name"`
	Tier     string `json:"tier"`
	TierName string `json:"tier_name"`
	Products string `json:"products,omitempty"`
	Lends    bool   `json:"lends"`
}

// ResourceCatalog is where the current catalogue is read. Root admins add and
// remove availabilities while the service runs, so anything that outlives a
// request asks it each time instead of keeping a copy.
type ResourceCatalog interface {
	Resources() []Resource
}

// StaticCatalog is a catalogue that never changes — configuration without
// runtime additions, and tests.
type StaticCatalog []Resource

func (c StaticCatalog) Resources() []Resource { return c }

// CombinedCatalog is the catalogues of several providers read as one, in order.
type CombinedCatalog []ResourceCatalog

func (c CombinedCatalog) Resources() []Resource {
	var out []Resource
	for _, part := range c {
		out = append(out, part.Resources()...)
	}
	return out
}

// BelongsTo reports whether the resource is provider's to carry out. One
// without a provider belongs to every provider, as before there were several.
func (m Resource) BelongsTo(provider string) bool {
	return m.Provider == "" || m.Provider == provider
}

// IsBool reports whether this resource is an availability rather than a
// quantity. Everything that sums, caps or charges asks this first.
func (m Resource) IsBool() bool { return m.Kind == KindBool }

// IsCount reports whether this resource takes part in the arithmetic — summing
// across siblings, capacity checks, usage roll-ups.
//
// An empty kind counts, which is what keeps a catalogue written before kinds
// existed behaving exactly as it did.
func (m Resource) IsCount() bool { return m.Kind == "" || m.Kind == KindCount || m.Kind == KindWindow }

// IsWindow reports whether this resource is an amount per time window.
func (m Resource) IsWindow() bool { return m.Kind == KindWindow }

// ValidateResources checks what the tree needs of a catalogue: unique ids and
// known kinds. What an entry means in the executing system is checked there
// (for OpenStack: osclient.ValidateManagedResources).
//
// It runs at startup and returns an error rather than dropping bad entries: a
// resource with a misspelled kind would take part in no arithmetic while still
// being requestable, which looks like the feature working.
func ValidateResources(defs []Resource) error {
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
			if d.Window != "" {
				return fmt.Errorf("resource %q: only kind %q has a window", d.ID, KindWindow)
			}
		case KindWindow:
			if !slices.Contains(Windows, d.Window) {
				return fmt.Errorf("resource %q: window %q is not one of %v", d.ID, d.Window, Windows)
			}
		default:
			return fmt.Errorf("resource %q: unknown kind %q", d.ID, d.Kind)
		}
	}

	return nil
}
