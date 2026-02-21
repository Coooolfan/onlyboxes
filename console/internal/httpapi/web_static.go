package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const embeddedWebDistDir = "web_dist"

//go:embed web_dist
var embeddedWebDist embed.FS

var embeddedWebFS = mustEmbeddedWebFS()

func mustEmbeddedWebFS() fs.FS {
	webFS, err := fs.Sub(embeddedWebDist, embeddedWebDistDir)
	if err != nil {
		panic("failed to load embedded web dist: " + err.Error())
	}
	return webFS
}

func registerEmbeddedWebRoutes(router *gin.Engine) {
	fileServer := http.FileServer(http.FS(embeddedWebFS))

	router.GET("/", gin.WrapH(fileServer))
	router.HEAD("/", gin.WrapH(fileServer))
	router.GET("/favicon.ico", gin.WrapH(fileServer))
	router.GET("/onlyboxes.avif", gin.WrapH(fileServer))
	router.GET("/assets/*filepath", gin.WrapH(fileServer))

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
