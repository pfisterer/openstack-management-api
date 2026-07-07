package webserver

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/generated_docs"
	"github.com/pfisterer/openstack-management-api/internal/helper"
)

// StaticConfig contains values needed by static endpoints.
type StaticConfig struct {
	OIDCIssuerURL string
	OIDCClientID  string
}

// RegisterStaticRoutes wires all static/documentation routes on the given group.
func RegisterStaticRoutes(group *gin.RouterGroup, cfg StaticConfig) *gin.RouterGroup {
	// Serve index.html with the __VERSION__ placeholder replaced by the version
	group.GET("/", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, strings.ReplaceAll(helper.IndexHTML, "__VERSION__", generated_docs.Version))
	})

	subFS, _ := fs.Sub(generated_docs.ClientDist, "client-dist")
	group.StaticFS("/client", http.FS(subFS))

	group.GET("/swagger.json", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusOK, generated_docs.SwaggerJSON)
	})

	// Unauthenticated bootstrap config (mirrors the Dynamic Zones API's
	// /config.json): lets the frontend read this service's version + auth
	// without a client/auth setup (used e.g. by the self-service-ui footer).
	group.GET("/config.json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version": generated_docs.Version,
			"auth": gin.H{
				"auth_provider": "oidc",
				"issuer_url":    cfg.OIDCIssuerURL,
				"client_id":     cfg.OIDCClientID,
			},
		})
	})

	return group
}
