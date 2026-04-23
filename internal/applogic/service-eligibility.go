package applogic

import (
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// GetMyEligibilityRules returns all eligibility rules owned by any token in the caller's set.
func (s *Service) GetMyEligibilityRules(userTokens common.TokenList) ([]common.TokenEligibilityRule, error) {
	if len(userTokens) == 0 {
		return []common.TokenEligibilityRule{}, nil
	}
	ctx, cancel := s.newCtx()
	defer cancel()
	rules, err := s.store.GetEligibilityRulesByOwnerTokens(ctx, userTokens)
	if err != nil {
		return nil, fmt.Errorf("load eligibility rules: %w", err)
	}
	return rules, nil
}

// SetEligibilityRule creates or replaces the eligibility rule for ownerToken.
// The caller must hold ownerToken in their effective token set.
func (s *Service) SetEligibilityRule(ownerToken string, eligibleRequesters common.TokenList, actorEmail string, userTokens common.TokenList) (common.TokenEligibilityRule, error) {
	if ownerToken == "" {
		return common.TokenEligibilityRule{}, fmt.Errorf("owner_token must not be empty")
	}
	if actorEmail == "" || !slices.Contains(userTokens, ownerToken) {
		return common.TokenEligibilityRule{}, common.ErrForbidden
	}

	rule := common.TokenEligibilityRule{
		OwnerToken:         ownerToken,
		EligibleRequesters: eligibleRequesters,
		CreatedBy:          actorEmail,
		UpdatedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	ctx, cancel := s.newCtx()
	defer cancel()
	if err := s.store.UpsertEligibilityRule(ctx, rule); err != nil {
		return common.TokenEligibilityRule{}, fmt.Errorf("persist eligibility rule: %w", err)
	}
	return rule, nil
}

// DeleteEligibilityRule removes the eligibility rule for ownerToken.
// The caller must hold ownerToken in their effective token set.
func (s *Service) DeleteEligibilityRule(ownerToken string, actorEmail string, userTokens common.TokenList) error {
	if ownerToken == "" {
		return fmt.Errorf("owner_token must not be empty")
	}
	if actorEmail == "" || !slices.Contains(userTokens, ownerToken) {
		return common.ErrForbidden
	}
	ctx, cancel := s.newCtx()
	defer cancel()
	if err := s.store.DeleteEligibilityRule(ctx, ownerToken); err != nil {
		return fmt.Errorf("delete eligibility rule: %w", err)
	}
	return nil
}
