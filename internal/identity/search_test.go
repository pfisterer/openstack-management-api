package identity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/roles"
	"go.uber.org/zap"
)

// manyRoles answers every search with more entries than the service may return, ignoring the limit it is asked for, and counts the calls.
type manyRoles struct{ calls int }

func (m *manyRoles) GetUserTokens(context.Context, *common.UserClaims) (common.TokenList, error) {
	return nil, nil
}
func (m *manyRoles) SearchGroups(_ context.Context, query string, _ int) ([]roles.GroupSummary, error) {
	m.calls++
	out := make([]roles.GroupSummary, 30)
	for i := range out {
		out[i] = roles.GroupSummary{Token: fmt.Sprintf("group:%s-%02d", query, i)}
	}
	return out, nil
}
func (m *manyRoles) SearchUsers(_ context.Context, query string, _ int) ([]string, error) {
	m.calls++
	out := make([]string, 30)
	for i := range out {
		out[i] = fmt.Sprintf("%s%02d@example.org", query, i)
	}
	return out, nil
}
func (m *manyRoles) GetGroupUsers(context.Context, string) ([]string, error) { return nil, nil }
func (m *manyRoles) ListRelations(context.Context) ([]string, error)         { return nil, nil }

type manyParticipants struct{}

func (manyParticipants) ListParticipants(context.Context) ([]string, error) {
	out := make([]string, 30)
	for i := range out {
		out[i] = fmt.Sprintf("abc-participant%02d@example.org", i)
	}
	return out, nil
}

// A query shorter than three characters finds nothing and does not even ask the directory; a longer one returns at most ten of each kind, whatever limit the caller passes.
func TestSearchPrincipals_ShortQueryAndClamp(t *testing.T) {
	r := &manyRoles{}
	svc := NewService(manyParticipants{}, r, time.Second, zap.NewNop().Sugar())

	for _, q := range []string{"", "a", "ab", "  ab  ", "äb"} {
		groups, users, err := svc.SearchPrincipals(q, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 0 || len(users) != 0 {
			t.Errorf("q=%q must find nothing, got %d groups and %d users", q, len(groups), len(users))
		}
	}
	if r.calls != 0 {
		t.Errorf("a short query must not reach the directory, got %d calls", r.calls)
	}

	for _, limit := range []int{0, 5, 10, 11, 500} {
		groups, users, err := svc.SearchPrincipals("abc", limit)
		if err != nil {
			t.Fatal(err)
		}
		want := MaxPrincipalResults
		if limit > 0 && limit < MaxPrincipalResults {
			want = limit
		}
		if len(groups) > want || len(users) != want {
			t.Errorf("limit %d: want at most %d groups and %d users, got %d and %d", limit, want, want, len(groups), len(users))
		}
	}
}
