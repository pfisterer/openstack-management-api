package estimate

import (
	"strings"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// Measured are the usage profiles the AI service computed from LiteLLM's spend logs (GET /v1/usage-profiles of aiaas-management-api), per model alias. Where a figure is there, it replaces the configured profile's number; where it is not (too few keys stood behind it), the profile's number stays. The keys stand in for people: a person with two keys counts twice, a shared key once.
type Measured struct {
	From       time.Time                `json:"from"`
	To         time.Time                `json:"to"`
	ComputedAt time.Time                `json:"computed_at"`
	MinKeys    int                      `json:"min_keys"`
	Models     map[string]MeasuredModel `json:"models"`
	// FetchedAt is when this service fetched them.
	FetchedAt time.Time `json:"fetched_at"`
}

// MeasuredStat is one measured figure.
type MeasuredStat struct {
	Median  float64 `json:"median"`
	P90     float64 `json:"p90"`
	Samples int     `json:"samples"`
	Keys    int     `json:"keys"`
}

// At is the figure at a level: the median for typical, the P90 for high.
func (s MeasuredStat) At(l Level) float64 {
	if l == LevelHigh {
		return s.P90
	}
	return s.Median
}

// MeasuredModel are the figures of one alias; nil = not measured enough.
type MeasuredModel struct {
	Requests               int           `json:"requests"`
	Keys                   int           `json:"keys"`
	InputTokensPerRequest  *MeasuredStat `json:"input_tokens_per_request,omitempty"`
	OutputTokensPerRequest *MeasuredStat `json:"output_tokens_per_request,omitempty"`
	RequestsPerKeyHour     *MeasuredStat `json:"requests_per_key_hour,omitempty"`
	TokensPerKeyHour       *MeasuredStat `json:"tokens_per_key_hour,omitempty"`
	PeakTPMPerKey          *MeasuredStat `json:"peak_tpm_per_key,omitempty"`
	ConcurrencyPerKey      *MeasuredStat `json:"concurrency_per_key,omitempty"`
	TokensPerKeyWindow     *MeasuredStat `json:"tokens_per_key_window,omitempty"`
	// UserAgents are the shares of the requests by kind of client, where the spend logs name it.
	UserAgents map[string]float64 `json:"user_agents,omitempty"`
}

// SetMeasured replaces the measured profiles (nil: none, the configured ones apply throughout).
func (e *Estimator) SetMeasured(m *Measured) { e.measured.Store(m) }

// Measured are the measured profiles in use; nil for none.
func (e *Estimator) Measured() *Measured { return e.measured.Load() }

// measuredFor is what was measured of a model alias, with the report; false for nothing.
func (e *Estimator) measuredFor(alias string) (MeasuredModel, *Measured, bool) {
	m := e.measured.Load()
	if m == nil {
		return MeasuredModel{}, nil, false
	}
	mm, ok := m.Models[alias]
	return mm, m, ok
}

// Figure sources in LLMResult.Sources.
const (
	SourceMeasured = "measured"
	SourceProfile  = "profile"
	SourceGiven    = "given"
)

// figure is one number of the LLM estimate: from the input, from the measurement, or from the profile.
type figure struct {
	name  string
	label msg.Text
	stat  *MeasuredStat
	given *float64
}

// joinTexts joins texts in each language.
func joinTexts(parts []msg.Text, sep string) msg.Text {
	de := make([]string, 0, len(parts))
	en := make([]string, 0, len(parts))
	for _, p := range parts {
		de = append(de, p.In(msg.DE))
		en = append(en, p.In(msg.EN))
	}
	return msg.T(strings.Join(de, sep), strings.Join(en, sep))
}
