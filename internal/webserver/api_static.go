package webserver

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/generated_docs"
	"github.com/pfisterer/openstack-management-api/internal/helper"
)

// appVersion is the version this process reports (index page, config.json): APP_VERSION, which the Helm chart sets from the chart's appVersion — the tag the image is deployed under —, else the embedded VERSION file. The embedded file alone is not enough: a stable release retags the image of the pre-release before it without rebuilding, so the binary carries the pre-release's number.
var appVersion = runningVersion(os.Getenv("APP_VERSION"), generated_docs.Version)

// runningVersion is env when set, else the embedded version; both trimmed (the VERSION file ends in a newline).
func runningVersion(env, embedded string) string {
	if v := strings.TrimSpace(env); v != "" {
		return v
	}
	return strings.TrimSpace(embedded)
}

// StaticConfig contains values needed by static endpoints.
type StaticConfig struct {
	OIDCIssuerURL string
	OIDCClientID  string
	// SignInAvailable reports whether the identity provider is answering. On
	// the public config.json rather than /v1/config because the latter needs a
	// verified token — exactly what cannot be had while the provider is away.
	SignInAvailable func() bool
}

// RegisterStaticRoutes wires all static/documentation routes on the given group.
func RegisterStaticRoutes(group *gin.RouterGroup, cfg StaticConfig) *gin.RouterGroup {
	// Serve index.html with the __VERSION__ placeholder replaced by the version
	group.GET("/", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, strings.ReplaceAll(helper.IndexHTML, "__VERSION__", appVersion))
	})

	group.GET("/swagger.json", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusOK, generated_docs.SwaggerJSON)
	})

	// Unauthenticated bootstrap config (mirrors the Dynamic Zones API's
	// /config.json): lets the frontend read this service's version + auth
	// without a client/auth setup (used e.g. by the self-service-ui footer).
	group.GET("/config.json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version": appVersion,
			"auth": gin.H{
				"auth_provider": "oidc",
				"issuer_url":    cfg.OIDCIssuerURL,
				"client_id":     cfg.OIDCClientID,
				// False while the identity provider cannot be reached. Sessions
				// that already exist keep working; what fails is signing in, and
				// the UI says so rather than offering a login that cannot work.
				"sign_in_available": cfg.SignInAvailable == nil || cfg.SignInAvailable(),
			},
		})
	})

	return group
}
