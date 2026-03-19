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
	DummyAuth     bool
	OIDCIssuerURL string
	OIDCClientID  string
}

// resourceConfig contains system-wide resource configuration.
type resourceConfig struct {
	Resources            []ResourceDefinition `json:"resources"`
	OpenstackRoles       []string             `json:"openstackRoles"`
	DelegationStrategies []string             `json:"delegationStrategies"`
}

// ResourceDefinition defines a resource type with constraints.
type ResourceDefinition struct {
	ID      string `json:"id" validate:"required"`
	Name    string `json:"name"`
	Default int    `json:"default"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Unit    string `json:"unit"`
	Message string `json:"message"`
}

// DefaultResourceDefinitions returns the built-in resource type definitions.
func DefaultResourceDefinitions() []ResourceDefinition {
	return []ResourceDefinition{
		{ID: "cores", Name: "Cores", Default: 4, Min: 1, Max: 50, Unit: "", Message: "1-50 cores"},
		{ID: "ram", Name: "RAM", Default: 16, Min: 1, Max: 256, Unit: "GB", Message: "1-256 GB"},
		{ID: "storage", Name: "Storage", Default: 100, Min: 1, Max: 1000, Unit: "GB", Message: "1-1000 GB"},
		{ID: "gpu", Name: "GPUs", Default: 0, Min: 0, Max: 10, Unit: "units", Message: "0-10 GPUs"},
	}
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

	group.GET("/config.json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version": generated_docs.Version,
		})
	})

	return group
}

// getConfig returns the system-wide resource configuration.
//
//	@Summary		Get resource configuration
//	@Description	Retrieves system-wide configuration including resource types and roles.
//	@Tags			config
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	resourceConfig	"Resource configuration."
//	@ID				getConfig
//	@Router			/v1/config [get]
func getConfig(cfg ResourceAPIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		resources := cfg.ResourceDefinitions
		if len(resources) == 0 {
			resources = DefaultResourceDefinitions()
		}

		config := resourceConfig{
			Resources:            resources,
			DelegationStrategies: []string{DelegationStrategyPool, DelegationStrategyAllowance},
			OpenstackRoles:       []string{"admin", "member", "reader"},
		}

		c.JSON(http.StatusOK, config)
	}
}
