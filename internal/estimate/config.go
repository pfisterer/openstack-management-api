// Package estimate is the resource estimator (Schätzhelfer): from a description of the intended use — a tool and how many people use it how long, a composition of flavours, workspaces of a kind — it computes the amounts a project would ask for, deterministically and with the assumptions it used. The numbers of the usage profiles are configuration (profiles.json, overridable by ESTIMATE_PROFILES_FILE), not code: they come from measurements and change when the measurements do.
//
// The package knows nothing of the tree: what a budget grants and which private flavours it holds are asked by the caller (webserver) and handed in as plain values.
package estimate

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

//go:embed profiles.json
var defaultProfiles []byte

// Level chooses which number of a profile applies: typical (the median of what was measured, or the assumption standing in for it) or high (the P90, or a generous assumption).
type Level string

const (
	LevelTypical Level = "typical"
	LevelHigh    Level = "high"
)

// Pair is a number at both levels.
type Pair struct {
	Typical float64 `json:"typical"`
	High    float64 `json:"high"`
}

// At is the number for a level; anything but high is typical.
func (p Pair) At(l Level) float64 {
	if l == LevelHigh {
		return p.High
	}
	return p.Typical
}

// ToolProfile is how one kind of LLM tool is used by one person.
type ToolProfile struct {
	NameDE string `json:"name_de"`
	NameEN string `json:"name_en"`
	// Model is the alias the tool talks to by default (code, agent, chat-default); it decides the price.
	Model                  string  `json:"model"`
	InputTokensPerRequest  float64 `json:"input_tokens_per_request"`
	OutputTokensPerRequest float64 `json:"output_tokens_per_request"`
	RequestsPerHour        Pair    `json:"requests_per_hour"`
	// PeakTPMPerPerson is what one active person sends in their busiest minute, input and output together (LiteLLM counts both against a TPM limit).
	PeakTPMPerPerson Pair `json:"peak_tpm_per_person"`
	// ConcurrentShare is the share of the people active in the same minute when they do not work at the same time.
	ConcurrentShare float64 `json:"concurrent_share"`
	Source          string  `json:"source"`
}

// ModelPrice is what LiteLLM bills per token of a model.
type ModelPrice struct {
	InputPerToken  float64 `json:"input_per_token"`
	OutputPerToken float64 `json:"output_per_token"`
}

// PricingConfig is the price list behind ConfigPricer.
type PricingConfig struct {
	// ReferencePerToken is the price one token of llm-mtokens stands for: the AI service turns a project's llm-mtokens into a money budget at this price.
	ReferencePerToken float64               `json:"reference_per_token"`
	Models            map[string]ModelPrice `json:"models"`
	Source            string                `json:"source"`
}

// KeyWindow is the short-term budget of every key at the AI service: Share of the pool per Hours.
type KeyWindow struct {
	Hours  float64 `json:"hours"`
	Share  float64 `json:"share"`
	Source string  `json:"source"`
}

type LLMConfig struct {
	Tools            map[string]ToolProfile `json:"tools"`
	TokenHeadroom    float64                `json:"token_headroom"`
	RateHeadroom     float64                `json:"rate_headroom"`
	TPMRoundTo       int                    `json:"tpm_round_to"`
	RPMPerPersonPeak Pair                   `json:"rpm_per_person_peak"`
	KeyWindow        KeyWindow              `json:"key_window"`
	Pricing          PricingConfig          `json:"pricing"`
}

// WorkspaceKind is a JupyterHub profile as the AI service books it.
type WorkspaceKind struct {
	NameDE  string  `json:"name_de"`
	NameEN  string  `json:"name_en"`
	Profile string  `json:"profile"`
	VCPU    float64 `json:"vcpu"`
	RAMGB   float64 `json:"ram_gb"`
	GPUs    float64 `json:"gpus"`
	// GPUTimeShare is the share of a session the GPU is held (CPU + GPU kernel holds it only while a GPU kernel lives).
	GPUTimeShare float64 `json:"gpu_time_share"`
}

type WorkspaceConfig struct {
	Kinds           map[string]WorkspaceKind `json:"kinds"`
	ConcurrentShare float64                  `json:"concurrent_share"`
	Headroom        float64                  `json:"headroom"`
	Source          string                   `json:"source"`
}

type CloudConfig struct {
	Headroom        float64  `json:"headroom"`
	FallbackFlavors []Flavor `json:"fallback_flavors"`
	Source          string   `json:"source"`
}

// ResourceIDs names the catalogue ids the estimate is written into, so a renamed resource is a configuration change.
type ResourceIDs struct {
	LLMMTokens       string `json:"llm_mtokens"`
	LLMTPM           string `json:"llm_tpm"`
	LLMRPM           string `json:"llm_rpm"`
	LLMModelPrefix   string `json:"llm_model_prefix"`
	WSVCPUHours      string `json:"ws_vcpu_hours"`
	WSRAMGBHours     string `json:"ws_ram_gb_hours"`
	WSStorageGB      string `json:"ws_storage_gb"`
	WSGPUHoursPrefix string `json:"ws_gpu_hours_prefix"`
	WSGPUsPrefix     string `json:"ws_gpus_prefix"`
	WSProfilePrefix  string `json:"ws_profile_prefix"`
	Cores            string `json:"cores"`
	RAMGB            string `json:"ram_gb"`
	StorageGB        string `json:"storage_gb"`
	// WindowDays is the window of the window resources (30 days): what the pools and hour budgets count per.
	WindowDays float64 `json:"window_days"`
}

// Config is the whole estimator configuration: profiles, prices, sizes and resource ids, with where the numbers come from.
type Config struct {
	Version    string          `json:"version"`
	Source     string          `json:"source"`
	LLM        LLMConfig       `json:"llm"`
	Workspaces WorkspaceConfig `json:"workspaces"`
	Cloud      CloudConfig     `json:"cloud"`
	Resources  ResourceIDs     `json:"resources"`
}

// DefaultConfig is the configuration built into the binary (profiles.json).
func DefaultConfig() Config {
	cfg, err := ParseConfig(defaultProfiles)
	if err != nil {
		panic("estimate: built-in profiles.json is invalid: " + err.Error())
	}
	return cfg
}

// LoadConfig reads the configuration from path, or the built-in one when path is empty.
func LoadConfig(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return DefaultConfig(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read estimator profiles %s: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig reads and checks a configuration.
func ParseConfig(raw []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("estimator profiles: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate refuses a configuration the arithmetic cannot work with: a missing tool number, a headroom below 1, a price of 0.
func (c Config) Validate() error {
	if len(c.LLM.Tools) == 0 {
		return fmt.Errorf("estimator profiles: no LLM tools")
	}
	for id, t := range c.LLM.Tools {
		if t.InputTokensPerRequest < 0 || t.OutputTokensPerRequest < 0 || t.InputTokensPerRequest+t.OutputTokensPerRequest <= 0 {
			return fmt.Errorf("estimator profiles: tool %s needs tokens per request", id)
		}
		if t.RequestsPerHour.Typical <= 0 || t.RequestsPerHour.High < t.RequestsPerHour.Typical {
			return fmt.Errorf("estimator profiles: tool %s needs requests_per_hour with 0 < typical <= high", id)
		}
		if t.PeakTPMPerPerson.Typical <= 0 || t.PeakTPMPerPerson.High < t.PeakTPMPerPerson.Typical {
			return fmt.Errorf("estimator profiles: tool %s needs peak_tpm_per_person with 0 < typical <= high", id)
		}
		if t.ConcurrentShare <= 0 || t.ConcurrentShare > 1 {
			return fmt.Errorf("estimator profiles: tool %s needs 0 < concurrent_share <= 1", id)
		}
	}
	if c.LLM.TokenHeadroom < 1 || c.LLM.RateHeadroom < 1 || c.Workspaces.Headroom < 1 || c.Cloud.Headroom < 1 {
		return fmt.Errorf("estimator profiles: every headroom is at least 1")
	}
	if c.LLM.KeyWindow.Hours <= 0 || c.LLM.KeyWindow.Share <= 0 || c.LLM.KeyWindow.Share > 1 {
		return fmt.Errorf("estimator profiles: key_window needs hours > 0 and 0 < share <= 1")
	}
	if c.LLM.Pricing.ReferencePerToken <= 0 {
		return fmt.Errorf("estimator profiles: pricing.reference_per_token must be more than 0")
	}
	if p, ok := c.LLM.Pricing.Models["default"]; !ok || p.InputPerToken <= 0 || p.OutputPerToken <= 0 {
		return fmt.Errorf("estimator profiles: pricing.models.default needs input and output prices")
	}
	for id, k := range c.Workspaces.Kinds {
		if k.VCPU < 0 || k.RAMGB < 0 || k.GPUs < 0 || k.GPUTimeShare < 0 || k.GPUTimeShare > 1 {
			return fmt.Errorf("estimator profiles: workspace kind %s has a negative size or a gpu_time_share beyond 1", id)
		}
	}
	if c.Workspaces.ConcurrentShare <= 0 || c.Workspaces.ConcurrentShare > 1 {
		return fmt.Errorf("estimator profiles: workspaces.concurrent_share must be in (0, 1]")
	}
	if c.Resources.WindowDays <= 0 {
		return fmt.Errorf("estimator profiles: resources.window_days must be more than 0")
	}
	return nil
}

// ToolIDs are the tools in a stable order.
func (c Config) ToolIDs() []string {
	return sortedKeys(c.LLM.Tools)
}

// WorkspaceKindIDs are the workspace kinds in a stable order.
func (c Config) WorkspaceKindIDs() []string {
	return sortedKeys(c.Workspaces.Kinds)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
