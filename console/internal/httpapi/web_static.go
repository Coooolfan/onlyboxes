package httpapi

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const embeddedWebDistDir = "web_dist"

//go:embed web_dist
var embeddedWebDist embed.FS

var ErrEmbeddedWebDistRequired = errors.New("failed to load embedded web dist")

func loadEmbeddedWebFS() (fs.FS, error) {
	webFS, err := fs.Sub(embeddedWebDist, embeddedWebDistDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbeddedWebDistRequired, err)
	}
	return webFS, nil
}

func registerEmbeddedWebRoutes(router *gin.Engine) error {
	webFS, err := loadEmbeddedWebFS()
	if err != nil {
		return err
	}
	registerWebRoutes(router, webFS)
	return nil
}

func registerWebRoutes(router *gin.Engine, webFS fs.FS) {
	fileServer := http.FileServer(http.FS(webFS))

	router.GET("/", gin.WrapH(fileServer))
	router.HEAD("/", gin.WrapH(fileServer))
	router.GET("/favicon.ico", gin.WrapH(fileServer))
	router.GET("/onlyboxes.avif", gin.WrapH(fileServer))
	router.GET("/assets/*filepath", gin.WrapH(fileServer))
	router.GET("/static/*filepath", gin.WrapH(fileServer))
	router.HEAD("/static/*filepath", gin.WrapH(fileServer))

	router.NoRoute(func(c *gin.Context) {
		method := c.Request.Method
		if method != http.MethodGet && method != http.MethodHead {
			c.Status(http.StatusNotFound)
			return
		}
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/mcp/") {
			c.Status(http.StatusNotFound)
			return
		}
		c.Status(http.StatusNotFound)
	})
}
