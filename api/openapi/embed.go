package openapi

import (
	"embed"
	"net/http"

	"github.com/italypaleale/go-kit/httpserver/openapidoc"
)

// Specs contains the generated provino OpenAPI documents embedded into the service binary
// Only the minified JSON form is embedded: the YAML representation is rendered on demand
//
//go:embed *.json
var Specs embed.FS

// ServeHTTP serves the default JSON document or one embedded document by file name
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The extension-free endpoint is the stable discovery URL for the service
	openapidoc.Serve(w, r, Specs, "provino.json")
}
