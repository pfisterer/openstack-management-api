// Package catalog keeps the availabilities of the resource catalogue — networks,
// images and flavours a project may be granted — editable at runtime by root
// admins, instead of in the deployment configuration.
//
// Quantities (cores, RAM, storage and the static quotas) stay configuration:
// they map onto Nova and Cinder quota fields and feed the usage evaluation, and
// removing one by accident would be expensive. Availabilities come and go — a
// GPU flavour, a course image, a network for a lab — and taking one out used to
// be broken twice over: the reconciler only touches what the catalogue names,
// so the grants stayed in OpenStack for good, and the stored ones and zeros
// made every budget carrying them unsavable.
//
// An availability goes through three states:
//
//	active    — offered and grantable; the root holds it.
//	withdrawn — set to 0 on every node, the root included, and revoked in
//	            OpenStack by the reconciler. Still known, so the zeros validate;
//	            no longer offered and never grantable. Can be restored.
//	removed   — gone from every node and from the catalogue. Only reachable
//	            from withdrawn once nothing holds it and OpenStack shows no
//	            managed project with the grant. The row is kept as a record.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

const (
	StateActive    = "active"
	StateWithdrawn = "withdrawn"
	StateRemoved   = "removed"
)

// Entry is one availability.
type Entry struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Group   string       `json:"group,omitempty"`
	Message string       `json:"message,omitempty"`
	Grant   common.Grant `json:"grant"`
	State   string       `json:"state"`

	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	ChangedAt time.Time `json:"changed_at"`
	ChangedBy string    `json:"changed_by"`
}

// resource is the entry as the rest of the service sees it.
func (e Entry) resource() common.ManagedProject {
	g := e.Grant
	return common.ManagedProject{
		ID:        e.ID,
		Name:      e.Name,
		Group:     e.Group,
		Message:   e.Message,
		Kind:      common.KindBool,
		Grant:     &g,
		ShowOnUI:  true,
		Withdrawn: e.State == StateWithdrawn,
	}
}

// Store keeps the entries, removed ones included.
type Store interface {
	List(ctx context.Context) ([]Entry, error)
	Upsert(ctx context.Context, e Entry) error
}

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// Catalog is the whole catalogue: the configured quantities plus the stored
// availabilities. Reads are a lock-free snapshot, because the tree validates
// against it on every request; writes are serialised.
type Catalog struct {
	quantities []common.ManagedProject
	store      Store
	log        *zap.SugaredLogger

	mu      sync.Mutex
	entries atomic.Pointer[[]Entry]
	current atomic.Pointer[[]common.ManagedProject]
}

var _ common.ResourceCatalog = (*Catalog)(nil)

// Load builds the catalogue from configuration and store. The configured
// availabilities seed an empty store once; after that the store is the only
// source, and configured ones it does not know are reported and ignored —
// otherwise one removed in the portal would come back with the next restart.
func Load(ctx context.Context, configured []common.ManagedProject, store Store, log *zap.SugaredLogger) (*Catalog, error) {
	c := &Catalog{store: store, log: log}
	var seed []common.ManagedProject
	for _, r := range configured {
		if r.IsBool() {
			seed = append(seed, r)
		} else {
			c.quantities = append(c.quantities, r)
		}
	}
	stored, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("load availabilities: %w", err)
	}
	if len(stored) == 0 && len(seed) > 0 {
		now := time.Now().UTC()
		for _, r := range seed {
			e := Entry{ID: r.ID, Name: r.Name, Group: r.Group, Message: r.Message, Grant: *r.Grant, State: StateActive,
				CreatedAt: now, CreatedBy: "configuration", ChangedAt: now, ChangedBy: "configuration"}
			if err := store.Upsert(ctx, e); err != nil {
				return nil, fmt.Errorf("seed availability %q: %w", r.ID, err)
			}
			stored = append(stored, e)
		}
		log.Infow("Availabilities taken over from the configuration; from now on they are managed in the portal", "count", len(seed))
	} else {
		known := map[string]bool{}
		for _, e := range stored {
			known[e.ID] = true
		}
		for _, r := range seed {
			if !known[r.ID] {
				log.Warnw("Configured availability ignored: availabilities are managed in the portal", "resource", r.ID)
			}
		}
	}
	if err := c.publish(stored); err != nil {
		return nil, err
	}
	return c, nil
}

// publish makes entries the current state, after checking that the catalogue
// they make is one the service could have started with.
func (c *Catalog) publish(entries []Entry) error {
	resources := slices.Clone(c.quantities)
	for _, e := range entries {
		if e.State != StateRemoved {
			resources = append(resources, e.resource())
		}
	}
	if err := common.ValidateManagedProjects(resources); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b Entry) int {
		if a.Group != b.Group {
			return compareStrings(a.Group, b.Group)
		}
		return compareStrings(a.Name, b.Name)
	})
	c.entries.Store(&sorted)
	c.current.Store(&resources)
	return nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Resources is the current catalogue: quantities, then the active and
// withdrawn availabilities.
func (c *Catalog) Resources() []common.ManagedProject { return *c.current.Load() }

// Entries lists the availabilities that are not removed.
func (c *Catalog) Entries() []Entry {
	var out []Entry
	for _, e := range *c.entries.Load() {
		if e.State != StateRemoved {
			out = append(out, e)
		}
	}
	return out
}

// Get returns one entry, removed ones included.
func (c *Catalog) Get(id string) (Entry, bool) {
	for _, e := range *c.entries.Load() {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// idPattern keeps ids usable as map keys everywhere they end up — JSON, log
// fields, the UI's form state — and readable in all of them.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// NewEntry is what an admin gives for an availability.
type NewEntry struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Group   string       `json:"group,omitempty"`
	Message string       `json:"message,omitempty"`
	Grant   common.Grant `json:"grant"`
}

// add stores a new availability, or brings back a removed one under the same
// id — removing stripped it from every node, so it starts clean either way.
func (c *Catalog) add(ctx context.Context, in NewEntry, actor string) (Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !idPattern.MatchString(in.ID) {
		return Entry{}, fmt.Errorf("%w: id must be 2-63 lowercase letters, digits or dashes", ErrInvalid)
	}
	if in.Name == "" {
		return Entry{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	for _, q := range c.quantities {
		if q.ID == in.ID {
			return Entry{}, fmt.Errorf("%w: %q is a configured quantity", ErrConflict, in.ID)
		}
	}
	created := time.Now().UTC()
	if old, ok := c.Get(in.ID); ok {
		if old.State != StateRemoved {
			return Entry{}, fmt.Errorf("%w: %q exists", ErrConflict, in.ID)
		}
	}
	e := Entry{ID: in.ID, Name: in.Name, Group: in.Group, Message: in.Message, Grant: in.Grant, State: StateActive,
		CreatedAt: created, CreatedBy: actor, ChangedAt: created, ChangedBy: actor}
	return e, c.write(ctx, e)
}

// update changes how an availability is presented. The grant is not
// editable: pointing an id at another network would move every existing grant
// with it, unseen by the people who hold it.
func (c *Catalog) update(ctx context.Context, id, name, group, message, actor string) (Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.Get(id)
	if !ok || e.State == StateRemoved {
		return Entry{}, ErrNotFound
	}
	if name == "" {
		return Entry{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	e.Name, e.Group, e.Message = name, group, message
	e.ChangedAt, e.ChangedBy = time.Now().UTC(), actor
	return e, c.write(ctx, e)
}

// setState moves an entry from one of the states in from to state to.
func (c *Catalog) setState(ctx context.Context, id string, from []string, to, actor string) (Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.Get(id)
	if !ok || e.State == StateRemoved {
		return Entry{}, ErrNotFound
	}
	if e.State == to {
		return e, nil
	}
	if !slices.Contains(from, e.State) {
		return Entry{}, fmt.Errorf("%w: %q is %s", ErrConflict, id, e.State)
	}
	e.State = to
	e.ChangedAt, e.ChangedBy = time.Now().UTC(), actor
	return e, c.write(ctx, e)
}

// write stores e and publishes the result; on a catalogue that would not
// validate nothing is stored.
func (c *Catalog) write(ctx context.Context, e Entry) error {
	entries := slices.Clone(*c.entries.Load())
	i := slices.IndexFunc(entries, func(x Entry) bool { return x.ID == e.ID })
	if i >= 0 {
		entries[i] = e
	} else {
		entries = append(entries, e)
	}
	// Validate before storing, by building what would be published.
	probe := &Catalog{quantities: c.quantities}
	if err := probe.publish(entries); err != nil {
		return err
	}
	if err := c.store.Upsert(ctx, e); err != nil {
		return fmt.Errorf("store availability %q: %w", e.ID, err)
	}
	return c.publish(entries)
}
