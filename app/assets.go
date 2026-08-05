package main

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/labstack/echo/v4"
)

//go:embed static
var staticFiles embed.FS

func registerStaticRoutes(e *echo.Echo) {
	sub, _ := fs.Sub(staticFiles, "static")
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", http.FileServer(http.FS(sub)))))
}
