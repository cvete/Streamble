// Package web serves the built-in monitor and test player page.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var index []byte

// Handler serves the monitor page.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index) //nolint:errcheck
	})
}
