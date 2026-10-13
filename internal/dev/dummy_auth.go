package dev

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// DefaultDevUser is who the development auth signs in: the seed's root admin.
const DefaultDevUser = "root.admin@uni.example"

// DummyAuthMiddleware signs every request in as one fixed person, for development and tests without SSO. There is no header naming the caller: to see the service as somebody else, the dev user — a root admin — acts on their behalf with X-Act-As, exactly as an admin does in production, so the dev setup exercises the same path.
func DummyAuthMiddleware(devUser string) gin.HandlerFunc {
	devUser = strings.ToLower(strings.TrimSpace(devUser))
	if devUser == "" {
		devUser = DefaultDevUser
	}
	claims := &common.UserClaims{Email: devUser, Name: devUser}
	var tokens common.TokenList = common.TokenList{"user:" + devUser}
	identities, _ := DefaultMockTreeState()
	for _, ident := range identities {
		if strings.EqualFold(ident.Email, devUser) {
			tokens = ident.Tokens
			break
		}
	}
	return func(c *gin.Context) {
		ginweb.SetReadOnly(c, false)
		identity.SetAuthenticated(c, &common.UserClaims{Email: claims.Email, Name: claims.Name}, tokens)
		c.Next()
	}
}
