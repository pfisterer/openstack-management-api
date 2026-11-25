package estimate

import "strings"

// Pricer turns tokens of a model into what they cost of a project's llm-mtokens pool, in millions. The pool is money at the AI service: it books llm-mtokens × the reference price per token as a LiteLLM budget, and LiteLLM bills every request at its model's input and output price. So a million "pool tokens" buys a million input tokens of a model at the reference price, fewer output tokens or tokens of a dearer model.
//
// This is the one place where tokens become money. DEPENDENCY: the token price fix of 25.11.2026 (openstack-management-api / aiaas-management-api) introduces a documented reference price per token; once it is there, a Pricer over it replaces ConfigPricer (or ConfigPricer reads its numbers), and nothing else in this package changes.
type Pricer interface {
	PoolMTokens(model string, inputTokens, outputTokens float64) float64
}

// ConfigPricer prices by the configured list (PricingConfig): a model without its own entry costs what "default" costs.
type ConfigPricer struct{ Config PricingConfig }

func (p ConfigPricer) price(model string) ModelPrice {
	if m, ok := p.Config.Models[strings.TrimSpace(model)]; ok {
		return m
	}
	return p.Config.Models["default"]
}

// PoolMTokens is (input × input price + output × output price) / reference price, in millions.
func (p ConfigPricer) PoolMTokens(model string, inputTokens, outputTokens float64) float64 {
	m := p.price(model)
	money := inputTokens*m.InputPerToken + outputTokens*m.OutputPerToken
	return money / p.Config.ReferencePerToken / 1e6
}
