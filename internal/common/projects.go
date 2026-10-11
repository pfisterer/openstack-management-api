package common

import "fmt"

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
)

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

	// Group is a heading the UI sorts resources under ("Compute", "Networks",
	// "GPU flavours"). Purely presentational, and the reason a catalogue of
	// fifty entries can still be read at the root of the tree.
	Group string `json:"group,omitempty"`

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

// IsBool reports whether this resource is an availability rather than a
// quantity. Everything that sums, caps or charges asks this first.
func (m Resource) IsBool() bool { return m.Kind == KindBool }

// IsCount reports whether this resource takes part in the arithmetic — summing
// across siblings, capacity checks, usage roll-ups.
//
// An empty kind counts, which is what keeps a catalogue written before kinds
// existed behaving exactly as it did.
func (m Resource) IsCount() bool { return m.Kind == "" || m.Kind == KindCount }

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
		default:
			return fmt.Errorf("resource %q: unknown kind %q", d.ID, d.Kind)
		}
	}

	return nil
}
