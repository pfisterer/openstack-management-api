package webserver

import (
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// mustGetAuthContext returns who the request is from, as resolved by
// identity.EffectiveAuthMiddleware. A local name, because every handler keeps
// the result in a variable called auth.
func mustGetAuthContext(c *gin.Context) (identity.AuthContext, error) {
	return identity.FromContext(c)
}

// uiActor is the caller of a change through the REST API: the person acted as, and the admin who acted for them when that was somebody else.
func uiActor(auth identity.AuthContext) model.Actor {
	a := model.UIActor(auth.UserEmail)
	if auth.OnBehalf() {
		a.ActedBy = auth.ActorEmail
	}
	return a
}
