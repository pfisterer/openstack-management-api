package webserver

import (
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// mustGetAuthContext returns who the request is from, as resolved by
// identity.EffectiveAuthMiddleware. A local name, because every handler keeps
// the result in a variable called auth.
func mustGetAuthContext(c *gin.Context) (identity.AuthContext, error) {
	return identity.FromContext(c)
}
