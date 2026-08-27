package app

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// The catalogue is deployment configuration, so these tests stand in for the
// operator who mistypes it. What matters in every case is that the process
// STOPS: the alternative is a portal that offers a resource governing nothing.

func TestLoadResourceCatalogue_FallsBackToTheBuiltInSet(t *testing.T) {
	t.Setenv(resourceCatalogueEnv, "")

	defs, err := loadResourceCatalogue()
	if err != nil {
		t.Fatalf("empty env should yield the defaults: %v", err)
	}

	byID := map[string]common.ManagedProject{}
	for _, d := range defs {
		byID[d.ID] = d
	}
	for _, want := range []string{"cores", "ram", "storage", "gpu"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("default catalogue is missing %q", want)
		}
	}
	// The four originals predate kinds and must still count, or every existing
	// budget's arithmetic changes underneath it.
	if !byID["cores"].IsCount() {
		t.Error("cores must remain a counted resource")
	}
}

func TestLoadResourceCatalogue_ReadsAConfiguredCatalogue(t *testing.T) {
	t.Setenv(resourceCatalogueEnv, `[
	  {"id":"cores","name":"Cores","kind":"count","group":"Compute","min":1,"max":64},
	  {"id":"dhbw-ipv4","name":"DHBW IPv4","kind":"bool","group":"Networks",
	   "grant":{"type":"network","target":"3f9a-net-uuid"}}
	]`)

	defs, err := loadResourceCatalogue()
	if err != nil {
		t.Fatalf("valid catalogue rejected: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d resources, want 2", len(defs))
	}
	if !defs[1].IsBool() || defs[1].Grant.Target != "3f9a-net-uuid" {
		t.Fatalf("availability did not survive the round trip: %+v", defs[1])
	}
	// The configured catalogue REPLACES the defaults rather than extending them —
	// merging would make it impossible to remove a resource. Asserted by the
	// count above: the defaults alone are twelve.
}

func TestLoadResourceCatalogue_StopsOnBadInput(t *testing.T) {
	cases := map[string]string{
		"malformed JSON":                `[{"id":"cores"`,
		"an unknown kind":               `[{"id":"cores","name":"Cores","kind":"conut"}]`,
		"an availability with no grant": `[{"id":"net","name":"Net","kind":"bool"}]`,
		"an empty array":                `[]`,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(resourceCatalogueEnv, raw)

			if _, err := loadResourceCatalogue(); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

// Guards the WIRING, not the parser: a rejected catalogue has to stop the
// service coming up, and a good one must not stand in its way.
//
// The other required settings are filled in deliberately. Without them
// loadAppConfiguration fails on missing OIDC configuration whatever the
// catalogue says — which is how the first version of this test passed while
// proving nothing. Checked by removing the guard: with the OIDC values in place
// the test goes red, without them it stays green either way.
func TestLoadAppConfiguration_TheCatalogueDecidesWhetherItStarts(t *testing.T) {
	// Every OTHER required setting, so the catalogue is the only variable.
	//
	// Spelled out rather than relying on what happens to be in the environment:
	// loadAppConfiguration reads the repository's .env, so a developer's file
	// supplies these locally and CI has none. The first version of this test was
	// green here and red there for exactly that reason.
	setMinimumConfig := func(t *testing.T) {
		t.Helper()
		t.Setenv("OIDC_ISSUER_URL", "https://sso.example/realms/test")
		t.Setenv("OIDC_CLIENT_ID", "test-client")
		t.Setenv("OPENSTACK_AUTH_URL", "https://openstack.example/v3")
		t.Setenv("OPENSTACK_REGION", "TestRegion")
	}

	t.Run("a valid catalogue does not block startup", func(t *testing.T) {
		setMinimumConfig(t)
		t.Setenv(resourceCatalogueEnv, `[{"id":"cores","name":"Cores"}]`)

		cfg, err := loadAppConfiguration()
		if err != nil {
			t.Fatalf("valid catalogue blocked startup: %v", err)
		}
		if len(cfg.ProjectDefinitions) != 1 || cfg.ProjectDefinitions[0].ID != "cores" {
			t.Fatalf("configured catalogue did not reach the config: %+v", cfg.ProjectDefinitions)
		}
	})

	t.Run("a catalogue naming an unimplemented kind stops it", func(t *testing.T) {
		setMinimumConfig(t)
		t.Setenv(resourceCatalogueEnv, `[{"id":"gpu-hours","name":"GPU hours","kind":"hours"}]`)

		if _, err := loadAppConfiguration(); err == nil {
			t.Fatal("the service started with a catalogue naming an unimplemented kind")
		}
	})
}

// The catalogue is written in Helm values and arrives here as the JSON that
// `toJson` made of them, so the field names in the chart have to be the field
// names the struct tags declare.
//
// They very nearly were not: the chart said showOnUI while the tag says
// show_on_ui, which Go does not match — every resource would have arrived with
// ShowOnUI false and vanished from the portal, with nothing anywhere reporting
// an error. This is the shape the chart actually renders.
func TestLoadResourceCatalogue_ParsesWhatTheChartRenders(t *testing.T) {
	t.Setenv(resourceCatalogueEnv, `[
	  {"id":"cores","name":"Cores","group":"Compute","default":4,"min":1,"max":64,"show_on_ui":true},
	  {"id":"dhbw-ipv4","name":"DHBW IPv4","kind":"bool","group":"Networks","show_on_ui":true,
	   "grant":{"type":"network","target":"net-uuid"}}
	]`)

	defs, err := loadResourceCatalogue()
	if err != nil {
		t.Fatalf("the chart's own shape was rejected: %v", err)
	}

	for _, d := range defs {
		if !d.ShowOnUI {
			t.Errorf("%q did not survive as a UI resource — check the json tag against the chart", d.ID)
		}
	}
	if defs[0].Min != 1 || defs[0].Max != 64 || defs[0].Default != 4 {
		t.Errorf("bounds did not survive: %+v", defs[0])
	}
	if defs[1].Grant == nil || defs[1].Grant.Target != "net-uuid" {
		t.Errorf("grant did not survive: %+v", defs[1])
	}
}

// A configured catalogue REPLACES the built-in one, so it has to be able to say
// everything the built-in one says — including how a resource maps to OpenStack.
//
// It could not, briefly: the mapping fields carried json:"-" while the rest of
// the entry was configurable. A deployment that named its own catalogue would
// have defined "cores" with no quota field, and every managed project would have
// come up with no quota at all — granted in the portal, unbounded in the cloud.
func TestLoadResourceCatalogue_CarriesTheOpenStackMapping(t *testing.T) {
	t.Setenv(resourceCatalogueEnv, `[
	  {"id":"cores","name":"Cores","os_quota_field":"cores","os_linked_field":"instances","os_overcommit_check":true},
	  {"id":"ram","name":"RAM","os_quota_field":"ram","os_multiplier":1024},
	  {"id":"ports","name":"Ports","default":50,"static":true,"os_quota_field":"ports"}
	]`)

	defs, err := loadResourceCatalogue()
	if err != nil {
		t.Fatalf("catalogue rejected: %v", err)
	}

	byID := map[string]common.ManagedProject{}
	for _, d := range defs {
		byID[d.ID] = d
	}

	if got := byID["cores"]; got.OSQuotaField != "cores" || got.OSLinkedField != "instances" || !got.OSOvercommitCheck {
		t.Errorf("cores lost its mapping: %+v", got)
	}
	if got := byID["ram"]; got.OSMultiplier != 1024 {
		t.Errorf("ram lost its unit conversion: GB would be written to OpenStack as MB, got %+v", got)
	}
	if got := byID["ports"]; !got.Static || got.Default != 50 {
		t.Errorf("the static infrastructure quota did not survive: %+v", got)
	}
}
