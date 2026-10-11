package dev

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// DummyAuthMiddleware injects a default user in group:uni_root for development/testing without SSO.
func DummyAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get the dummy user email from header X-Dummy-Auth-User
		dev_user := c.GetHeader("X-Dummy-Auth-User")

		// If not set, default to the root
		if strings.TrimSpace(dev_user) == "" {
			dev_user = "root.admin@uni.example"
		}

		// Set the user in the context as if it was authenticated, with a token representing group membership for "uni_root".
		claims := &common.UserClaims{
			Email: dev_user,
			Name:  fmt.Sprintf("User: %s", dev_user),
		}

		//Resolve the tokens from the identies for the email given
		identities, _ := DefaultMockTreeState()
		var userTokens common.TokenList = []string{}
		for _, identity := range identities {
			if identity.Email == dev_user {
				userTokens = identity.Tokens
				break
			}
		}

		// The dev email may not be one of the mock identities — the self-service UI
		// defaults dummy auth to real DHBW emails (e.g. dennis.pfisterer@dhbw.de),
		// which map to no tokens here and make every handler dead-end with
		// "no user tokens found". Fall back to the root mock identity's tokens, i.e.
		// the same behaviour as sending no X-Dummy-Auth-User header at all, so any
		// dev email can drive the full API. Dev-only (DummyAuth must be enabled).
		if len(userTokens) == 0 {
			for _, identity := range identities {
				if identity.Email == "root.admin@uni.example" {
					userTokens = identity.Tokens
					break
				}
			}
			fmt.Printf("DummyAuthMiddleware: dev user '%s' is not a mock identity; falling back to root tokens\n", dev_user)
		}

		// Set the claims and tokens in the context for downstream handlers to use.
		fmt.Printf("DummyAuthMiddleware: setting dummy auth for user '%s' with tokens: %v\n", dev_user, userTokens)
		ginweb.SetReadOnly(c, false)
		identity.SetAuthenticated(c, claims, userTokens)
		c.Next()
	}
}
