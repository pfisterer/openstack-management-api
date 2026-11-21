package app

import (
	"strings"
	"testing"

	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// The other providers' resources are configured beside OpenStack's and checked with them: ids are one namespace, because a project's limit is one map.
func TestLoadProviderResources(t *testing.T) {
	openstack := osclient.CoreResources(osclient.DefaultCatalogue())

	t.Setenv(providerResourcesEnv, "")
	if got, err := loadProviderResources(openstack); err != nil || got != nil {
		t.Fatalf("unset: %v %v, want nothing", got, err)
	}

	t.Setenv(providerResourcesEnv, `{"workspaces": [
	  {"id": "ws_vcpu_hours", "name": "vCPU hours", "kind": "window", "window": "30d", "area": "workspaces"},
	  {"id": "ws_home_gb", "name": "Home", "unit": "GB", "area": "workspaces"}]}`)
	got, err := loadProviderResources(openstack)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Provider != "workspaces" || got[1].Provider != "workspaces" || got[0].Area != "workspaces" || !got[0].IsWindow() || !got[1].IsCount() {
		t.Errorf("got %+v", got)
	}

	// An availability is a switch the provider interprets (e.g. an extra model group).
	t.Setenv(providerResourcesEnv, `{"aiaas": [{"id": "llm-model-code", "name": "Code model", "kind": "bool", "area": "llm"}]}`)
	if got, err := loadProviderResources(openstack); err != nil || len(got) != 1 || !got[0].IsBool() || got[0].Provider != "aiaas" {
		t.Errorf("availability: %+v %v", got, err)
	}

	// An availability may name the real models it grants, for names its id cannot carry.
	t.Setenv(providerResourcesEnv, `{"aiaas": [{"id": "llm-model-flash-next", "name": "Flash", "kind": "bool", "area": "llm", "models": ["qwen3.8-flash-next-iq3_s", "qwen3.8:flash"]}]}`)
	if got, err := loadProviderResources(openstack); err != nil || len(got) != 1 || len(got[0].Models) != 2 || got[0].Models[0] != "qwen3.8-flash-next-iq3_s" {
		t.Errorf("models: %+v %v", got, err)
	}

	for name, raw := range map[string]string{
		"empty model name":        `{"aiaas": [{"id": "llm-model-x", "name": "x", "kind": "bool", "area": "llm", "models": [""]}]}`,
		"model name with a space": `{"aiaas": [{"id": "llm-model-x", "name": "x", "kind": "bool", "area": "llm", "models": ["qwen 3"]}]}`,
		"models on a count":       `{"aiaas": [{"id": "llm-x", "name": "x", "area": "llm", "models": ["qwen3"]}]}`,
		"not an object":           `[{"id": "x", "name": "x"}]`,
		"openstack here":          `{"openstack": [{"id": "x", "name": "x"}]}`,
		"id taken by openstack":   `{"workspaces": [{"id": "cores", "name": "x", "area": "workspaces"}]}`,
		"window without a window": `{"workspaces": [{"id": "x", "name": "x", "kind": "window", "area": "workspaces"}]}`,
		"unknown window":          `{"workspaces": [{"id": "x", "name": "x", "kind": "window", "window": "1y", "area": "workspaces"}]}`,
		"without an area":         `{"workspaces": [{"id": "x", "name": "x"}]}`,
		"unknown area":            `{"workspaces": [{"id": "x", "name": "x", "area": "gpu"}]}`,
		"window on a count":       `{"workspaces": [{"id": "x", "name": "x", "window": "30d", "area": "workspaces"}]}`,
	} {
		t.Setenv(providerResourcesEnv, raw)
		if _, err := loadProviderResources(openstack); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), providerResourcesEnv) {
			t.Errorf("%s: error %q does not name %s", name, err, providerResourcesEnv)
		}
	}
}
