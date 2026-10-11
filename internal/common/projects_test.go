package common

import "testing"

// An amount per window sums and caps like a count — a budget's projects may not hold more hours per window than the budget — and must name one of the windows the providers book in.
func TestWindowResources(t *testing.T) {
	w := Resource{ID: "gpu_hours", Kind: KindWindow, Window: "30d"}
	if !w.IsCount() || !w.IsWindow() || w.IsBool() {
		t.Errorf("window resource: count %v, window %v, bool %v", w.IsCount(), w.IsWindow(), w.IsBool())
	}
	if err := ValidateResources([]Resource{w, {ID: "cores"}}); err != nil {
		t.Errorf("valid catalogue refused: %v", err)
	}
	for name, r := range map[string]Resource{
		"no window":         {ID: "x", Kind: KindWindow},
		"unknown window":    {ID: "x", Kind: KindWindow, Window: "1y"},
		"window on a count": {ID: "x", Window: "30d"},
		"window on a bool":  {ID: "x", Kind: KindBool, Window: "7d"},
	} {
		if err := ValidateResources([]Resource{r}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !(Resource{ID: "x"}).BelongsTo("workspaces") || (Resource{ID: "x", Provider: "openstack"}).BelongsTo("workspaces") {
		t.Error("a resource without a provider belongs to all, one with a provider only to it")
	}
}
