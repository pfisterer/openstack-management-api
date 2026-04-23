package webserver

import (
	"io/fs"
	"net/http"

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
	group.GET("/", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, helper.IndexHTML)
	})

	subFS, _ := fs.Sub(generated_docs.ClientDist, "client-dist")
	group.StaticFS("/client", http.FS(subFS))

	group.GET("/swagger.json", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusOK, generated_docs.SwaggerJSON)
	})

	return group
}
