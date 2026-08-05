package main

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	minifyHTML "github.com/tdewolff/minify/v2/html"
	"github.com/tdewolff/minify/v2/js"
)

func newMinifyMiddleware() echo.MiddlewareFunc {
	m := minify.New()
	m.Add("text/html", &minifyHTML.Minifier{KeepDefaultAttrVals: true})
	m.AddFunc("text/css", css.Minify)
	m.AddFuncRegexp(regexp.MustCompile(`^(application|text)/(x-)?(java|ecma)script$`), js.Minify)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			res := c.Response()
			orig := res.Writer
			buf := new(bytes.Buffer)
			res.Writer = &bufferingWriter{ResponseWriter: orig, buf: buf}

			handlerErr := next(c)
			res.Writer = orig

			body := buf.Bytes()
			ct := res.Header().Get("Content-Type")
			if ct == "" && len(body) > 0 {
				ct = http.DetectContentType(body)
			}
			if strings.HasPrefix(ct, "text/html") {
				if minified, err := m.String("text/html", string(body)); err == nil {
					body = []byte(minified)
				}
			}

			if _, writeErr := orig.Write(body); writeErr != nil && handlerErr == nil {
				return writeErr
			}
			return handlerErr
		}
	}
}

type bufferingWriter struct {
	http.ResponseWriter
	buf *bytes.Buffer
}

func (w *bufferingWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *bufferingWriter) Flush()                      {} // satisfies http.Flusher; no-op while buffering
