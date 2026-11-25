package aiaas

import (
	"context"
	"net/http"
	"time"
)

// ProfileStat is a measured distribution of the AI service's usage profiles: median, 90th percentile, number of values and of distinct keys.
type ProfileStat struct {
	Median  float64 `json:"median"`
	P90     float64 `json:"p90"`
	Samples int     `json:"samples"`
	Keys    int     `json:"keys"`
}

// ModelProfile is what the AI service measured of one model alias; a figure is nil where too few keys stand behind it.
type ModelProfile struct {
	Alias                  string             `json:"alias"`
	Requests               int                `json:"requests"`
	Keys                   int                `json:"keys"`
	KeyHours               int                `json:"key_hours"`
	InputTokensPerRequest  *ProfileStat       `json:"input_tokens_per_request"`
	OutputTokensPerRequest *ProfileStat       `json:"output_tokens_per_request"`
	TokensPerKeyHour       *ProfileStat       `json:"tokens_per_key_hour"`
	RequestsPerKeyHour     *ProfileStat       `json:"requests_per_key_hour"`
	PeakTPMPerKey          *ProfileStat       `json:"peak_tpm_per_key"`
	ConcurrencyPerKey      *ProfileStat       `json:"concurrency_per_key"`
	TokensPerKeyWindow     *ProfileStat       `json:"tokens_per_key_window"`
	UserAgents             map[string]float64 `json:"user_agents"`
}

// UsageProfiles is the AI service's report (GET /v1/usage-profiles): aggregates per model alias from LiteLLM's spend logs, never per person or key.
type UsageProfiles struct {
	From              time.Time      `json:"from"`
	To                time.Time      `json:"to"`
	ComputedAt        time.Time      `json:"computed_at"`
	MinKeys           int            `json:"min_keys"`
	WindowHours       int            `json:"window_hours"`
	Requests          int            `json:"requests"`
	UserAgentCoverage float64        `json:"user_agent_coverage"`
	Models            []ModelProfile `json:"models"`
}

// UsageProfiles fetches the report with the tree's token.
func (c *Client) UsageProfiles(ctx context.Context) (UsageProfiles, error) {
	var out UsageProfiles
	return out, c.do(ctx, http.MethodGet, "/v1/usage-profiles", nil, &out)
}
