package webserver

// White-box test for canUseRoleSwitch (unexported). After Option 1 the gate
// matches ANY token type (user: or group:) via plain set membership, consistent
// with the other root-admin gates — so a bare user: token works and a mixed
// allowlist no longer silently disables role switching.

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

func TestCanUseRoleSwitch(t *testing.T) {
	tests := []struct {
		name    string
		user    common.TokenList
		allowed common.TokenList
		want    bool
	}{
		{"group token matches", common.TokenList{"group:root_uni"}, common.TokenList{"group:root_uni"}, true},
		{"bare user token matches", common.TokenList{"user:root@x", "group:other"}, common.TokenList{"user:root@x"}, true},
		{"mixed allowlist matches via user token", common.TokenList{"user:root@x"}, common.TokenList{"group:root_uni", "user:root@x"}, true},
		{"mixed allowlist matches via group token", common.TokenList{"group:root_uni"}, common.TokenList{"group:root_uni", "user:root@x"}, true},
		{"no match", common.TokenList{"user:someone@else", "group:x"}, common.TokenList{"group:root_uni", "user:root@x"}, false},
		{"empty allowlist denies", common.TokenList{"user:root@x"}, nil, false},
		{"empty user denies", nil, common.TokenList{"group:root_uni"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canUseRoleSwitch(tt.user, tt.allowed); got != tt.want {
				t.Errorf("canUseRoleSwitch(%v, %v) = %v, want %v", tt.user, tt.allowed, got, tt.want)
			}
		})
	}
}
