package estimate

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Flavor is a machine size of OpenStack.
type Flavor struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	VCPUs  int    `json:"vcpus"`
	RAMMB  int    `json:"ram_mb"`
	DiskGB int    `json:"disk_gb"`
	GPUs   int    `json:"gpus,omitempty"`
	Public bool   `json:"public"`
	// Resource is the catalogue availability that grants a private flavour (grant type flavor); empty for a public one, and for a private one nobody offers.
	Resource string `json:"resource,omitempty"`
}

// FlavorSource lists the flavours OpenStack has, public and private.
type FlavorSource interface {
	Flavors(ctx context.Context) ([]Flavor, error)
}

// StaticFlavors is a fixed list (configuration, tests).
type StaticFlavors []Flavor

func (s StaticFlavors) Flavors(context.Context) ([]Flavor, error) { return s, nil }

// CachedFlavors asks Source at most every TTL and falls back to Fallback while it cannot answer (OpenStack not connected yet).
type CachedFlavors struct {
	Source   FlavorSource
	Fallback []Flavor
	TTL      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	cached  []Flavor
	fetched time.Time
}

func (c *CachedFlavors) Flavors(ctx context.Context) ([]Flavor, error) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && now().Sub(c.fetched) < c.TTL {
		return c.cached, nil
	}
	if c.Source != nil {
		fresh, err := c.Source.Flavors(ctx)
		if err == nil {
			c.cached, c.fetched = fresh, now()
			return fresh, nil
		}
		if c.cached != nil {
			return c.cached, nil
		}
		if len(c.Fallback) == 0 {
			return nil, err
		}
	}
	return c.Fallback, nil
}

// AllowedFlavors are the flavours a person may use under the given availabilities: every public one, and a private one when held (its catalogue availability, by id, is in held). byTarget maps a private flavour's OpenStack id to its availability id (the catalogue's flavour grants). Sorted by size, then name.
func AllowedFlavors(all []Flavor, byTarget map[string]string, held map[string]bool) []Flavor {
	var out []Flavor
	for _, f := range all {
		if f.Public {
			out = append(out, f)
			continue
		}
		res := byTarget[f.ID]
		if res == "" || !held[res] {
			continue
		}
		f.Resource = res
		out = append(out, f)
	}
	sortFlavors(out)
	return out
}

func sortFlavors(fs []Flavor) {
	slices.SortStableFunc(fs, func(a, b Flavor) int {
		return cmp.Or(cmp.Compare(a.GPUs, b.GPUs), cmp.Compare(a.VCPUs, b.VCPUs), cmp.Compare(a.RAMMB, b.RAMMB), cmp.Compare(a.Name, b.Name))
	})
}

// findFlavor looks a flavour up by id, then by name (case-insensitive).
func findFlavor(fs []Flavor, key string) (Flavor, bool) {
	key = strings.TrimSpace(key)
	for _, f := range fs {
		if f.ID == key {
			return f, true
		}
	}
	for _, f := range fs {
		if strings.EqualFold(f.Name, key) {
			return f, true
		}
	}
	return Flavor{}, false
}
