// Package features holds the feature gates: a new feature of the portal is shown to a small group first (e.g. on production), before everyone sees it.
//
// Gates hide, they do not protect. The UI asks GET /v1/config for the caller's gates and leaves out what is closed to them; the API behind a gated feature (REST and MCP) stays open to everyone who may use it otherwise. That is deliberate: a gate is for trying something out, not for access control, which belongs in the rules of the feature itself.
//
// Configuration (FEATURE_GATES, Helm value featureGates): a JSON object from gate name to the tokens it is open to, e.g. {"estimator": ["group:leiter-zwr"], "assistant": ["group:leiter-zwr"]}. A gate that is not named, or whose list contains "*", is open to everyone; an empty list closes it for everybody. Tokens are compared in canonical spelling against the caller's tokens (person and groups, as the role provider resolves them, so a group works exactly as in an admin scope).
//
// Adding a gate: add its name to Known, so /v1/config answers for it even where no environment configures it, and read config.features.<name> in the UI (self-service-ui, web/projects/use-features.js, useFeature).
package features

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Everyone, as the single entry of a gate's list, opens it for everybody.
const Everyone = "*"

// Known are the gates this version knows: /v1/config answers for each of them, open unless configured otherwise.
var Known = []string{"estimator", "assistant"}

// Gates maps a gate's name to the tokens it is open to (see the package comment).
type Gates map[string]common.TokenList

// Parse reads FEATURE_GATES. Empty is no gate configured: everything open.
func Parse(raw string) (Gates, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Gates{}, nil
	}
	var in map[string][]string
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, fmt.Errorf("FEATURE_GATES: %w", err)
	}
	out := Gates{}
	for name, tokens := range in {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("FEATURE_GATES: a gate without a name")
		}
		if tokens == nil {
			tokens = []string{}
		}
		list := common.TokenList{}
		for _, t := range tokens {
			if t = strings.TrimSpace(t); t == Everyone {
				list = append(list, Everyone)
			} else if t != "" {
				list = append(list, common.CanonicalTokens(common.TokenList{t})...)
			}
		}
		out[name] = list
	}
	return out, nil
}

// Open reports whether the gate is open to a caller with the given tokens.
func (g Gates) Open(name string, tokens common.TokenList) bool {
	list, configured := g[name]
	if !configured || slices.Contains(list, Everyone) {
		return true
	}
	return common.NewTokenSet(common.CanonicalTokens(tokens)).ContainsAny(list)
}

// For is every gate, known or configured, with whether it is open to the caller.
func (g Gates) For(tokens common.TokenList) map[string]bool {
	out := make(map[string]bool, len(Known)+len(g))
	for _, name := range Known {
		out[name] = g.Open(name, tokens)
	}
	for _, name := range slices.Sorted(maps.Keys(g)) {
		out[name] = g.Open(name, tokens)
	}
	return out
}
