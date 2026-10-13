package aiaas

import (
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The resource ids this provider understands. The catalogue names them (PROVIDER_RESOURCES); the GPU class is the rest of the id.
const (
	idVCPUHours    = "ws-vcpu-hours"
	idRAMGBHours   = "ws-ram-gb-hours"
	idServerHours  = "ws-server-hours"
	idLLMMTokens   = "llm-mtokens"
	idLLMRPM       = "llm-rpm"
	idLLMTPM       = "llm-tpm"
	idStorageGB    = "ws-storage-gb"
	prefixLLMModel = "llm-model-"
	prefixGPUHours = "ws-gpu-hours-"
	prefixGPUs     = "ws-gpus-"
)

// unlimitedLLM is what an unlimited LLM share becomes: the AI service books tokens in whole millions and has no "no limit" for them.
const unlimitedLLM = 1_000_000

// unlimitedStorage is what unlimited workspace storage becomes, in GB: a volume always has a size.
const unlimitedStorage = 10_000

// unlimitedRate is what an unlimited rate becomes: 0 would mean "not set" there, which is the AI service's default, not no limit.
const unlimitedRate = 1_000_000_000

// budgetFor is the AI service's budget a window resource maps to; false for every other id.
func budgetFor(r common.Resource) (Budget, bool) {
	if !r.IsWindow() {
		return Budget{}, false
	}
	switch {
	case r.ID == idVCPUHours:
		return Budget{Unit: "vcpu_hours", Window: r.Window}, true
	case r.ID == idRAMGBHours:
		return Budget{Unit: "ram_gb_hours", Window: r.Window}, true
	case r.ID == idServerHours:
		return Budget{Unit: "server_hours", Window: r.Window}, true
	case strings.HasPrefix(r.ID, prefixGPUHours) && len(r.ID) > len(prefixGPUHours):
		return Budget{Unit: "gpu_hours", GPUClass: strings.TrimPrefix(r.ID, prefixGPUHours), Window: r.Window}, true
	}
	return Budget{}, false
}

// ProjectFor is what the AI service is told about a leaf: its name and people, its end, and its amounts of this provider's resources. An unlimited budget is sent with amount 0, which the AI service reads as no cap.
func ProjectFor(n model.Node, resources []common.Resource) Project {
	p := Project{Name: strings.TrimSpace(n.Name), GPUsByClass: map[string]int{}, Budgets: []Budget{}, LLMModels: []string{}}
	if p.Name == "" {
		p.Name = strings.TrimSpace(n.Reason)
	}
	p.Members = membersOf(n)
	if n.TerminationDate != nil {
		if end, ok := parseDay(*n.TerminationDate); ok {
			p.EndsAt = &end
		}
	}
	limit := n.EffectiveLimit()
	for _, r := range resources {
		if !r.BelongsTo(Provider) || r.Provider == "" {
			continue
		}
		v := limit[r.ID]
		if v == 0 {
			continue
		}
		if b, ok := budgetFor(r); ok {
			if v != common.UnlimitedQuota {
				b.Amount = float64(v)
			}
			p.Budgets = append(p.Budgets, b)
			continue
		}
		switch {
		case r.IsBool() && strings.HasPrefix(r.ID, prefixLLMModel) && len(r.ID) > len(prefixLLMModel):
			if v == 1 {
				p.LLMModels = append(p.LLMModels, strings.TrimPrefix(r.ID, prefixLLMModel))
			}
		case r.ID == idLLMRPM || r.ID == idLLMTPM:
			rate := v
			if v == common.UnlimitedQuota {
				rate = unlimitedRate
			}
			if r.ID == idLLMRPM {
				p.LLMRPM = rate
			} else {
				p.LLMTPM = rate
			}
		case r.ID == idStorageGB:
			p.StorageGB = v
			if v == common.UnlimitedQuota {
				p.StorageGB = unlimitedStorage
			}
		case r.ID == idLLMMTokens:
			p.LLMMTokens = v
			if v == common.UnlimitedQuota {
				p.LLMMTokens = unlimitedLLM
			}
		case strings.HasPrefix(r.ID, prefixGPUs) && len(r.ID) > len(prefixGPUs) && v > 0:
			p.GPUsByClass[strings.TrimPrefix(r.ID, prefixGPUs)] = v
		}
	}
	return p
}

// membersOf: the owner and the leaf's admins manage the project at the AI service, everybody the leaf authorizes uses it.
func membersOf(n model.Node) []Member {
	var out []Member
	seen := map[string]bool{}
	add := func(token, role string) {
		token = strings.TrimSpace(token)
		if token == "" || seen[strings.ToLower(token)] {
			return
		}
		seen[strings.ToLower(token)] = true
		out = append(out, Member{Token: token, Role: role})
	}
	add(n.Owner, "manager")
	for _, t := range n.AdminScope {
		add(t, "manager")
	}
	for _, u := range n.AuthorizedUsers {
		add(u.Token, "member")
	}
	return out
}

func parseDay(s string) (time.Time, bool) {
	for _, layout := range []string{time.DateOnly, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// unitLLMMTokens is how the AI service reports what a project's members used of its LLM pool, in millions of tokens in the current window.
const unitLLMMTokens = "llm_mtokens"

// unitStorageGB is how the AI service reports the space a project's workspace volume holds, in GiB, without a window.
const unitStorageGB = "storage_gb"

// InUseFrom is what the AI service reports as used, by resource id: for each tree budget the use in its current window, what the project's LLM pool used and what its workspace storage holds. Budgets the catalogue does not know are left out.
func InUseFrom(st Status, resources []common.Resource) common.ProjectQuota {
	out := common.ProjectQuota{}
	for _, r := range resources {
		want, ok := budgetFor(r)
		if r.ID == idLLMMTokens && r.IsWindow() {
			want, ok = Budget{Unit: unitLLMMTokens, Window: r.Window}, true
		}
		if r.ID == idStorageGB && !r.IsWindow() {
			want, ok = Budget{Unit: unitStorageGB}, true
		}
		if !ok || r.Provider != Provider {
			continue
		}
		for _, u := range st.Used {
			if u.Unit == want.Unit && u.GPUClass == want.GPUClass && u.Window == want.Window {
				out[r.ID] = int(u.Used + 0.5)
			}
		}
	}
	return out
}
