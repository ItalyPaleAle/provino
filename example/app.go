package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	// The web UI, which Vite builds into web/dist/app from the Svelte project in web
	//
	//go:embed all:web/dist
	webFS embed.FS

	// The sign-in page of the identity provider, which is part of the provider rather than of the web UI
	//
	//go:embed templates
	templatesFS embed.FS
)

// app holds everything the handlers share
type app struct {
	log *slog.Logger
	// URL the app is reached at, without a trailing slash, which is also the issuer of its tokens
	publicURL string
	// URL provino is reached at, without a trailing slash
	provinoURL string
	// Folder provino stores the variants in, which the app serves them from
	variantsDir string

	db    *db
	oauth *oauthServer
}

// Create the app, opening its database in the data folder
func newApp(ctx context.Context, log *slog.Logger, opts options) (*app, error) {
	publicURL, err := baseURL(opts.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("invalid -url: %w", err)
	}
	provinoURL, err := baseURL(opts.ProvinoURL)
	if err != nil {
		return nil, fmt.Errorf("invalid -provino: %w", err)
	}

	// The app's own files go in a folder of their own, next to the ones provino writes
	appDir := filepath.Join(opts.DataDir, "app")
	err = os.MkdirAll(appDir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("failed to create the data folder: %w", err)
	}

	database, err := openDB(ctx, filepath.Join(appDir, "example.db"))
	if err != nil {
		return nil, err
	}
	oauth, err := newOAuthServer(ctx, database, publicURL, provinoURL)
	if err != nil {
		_ = database.Close()
		return nil, err
	}

	return &app{
		log:         log,
		publicURL:   publicURL,
		provinoURL:  provinoURL,
		variantsDir: filepath.Join(opts.DataDir, "variants"),
		db:          database,
		oauth:       oauth,
	}, nil
}

// Close the app's database
func (a *app) Close() error {
	return a.db.Close()
}

// Build the app's routes
func (a *app) routes() http.Handler {
	mux := http.NewServeMux()

	// The identity provider: discovery and keys for provino, sign-in for browsers, and tokens for both
	mux.HandleFunc("GET /.well-known/openid-configuration", a.oauth.handleDiscovery)
	mux.HandleFunc("GET /.well-known/jwks.json", a.oauth.handleJWKS)
	mux.HandleFunc("GET /oauth/authorize", a.oauth.handleAuthorizePage)
	mux.HandleFunc("POST /oauth/authorize", a.oauth.handleAuthorizeSubmit)
	mux.HandleFunc("POST /oauth/token", a.oauth.handleToken)

	// What provino calls, with a client-credentials token from the provider above
	// Every request is recorded with the app's answer, which the web UI shows
	mux.Handle("POST /api/provino/authorize", a.requireProvino(a.capture(a.saveAuthorization)(a.handleAuthorize())))
	mux.Handle("POST /api/provino/events", a.requireProvino(a.capture(a.saveDelivery)(a.handleEvent())))

	// What the web UI calls, with the signed-in user's token
	mux.HandleFunc("GET /api/config", a.handleConfig)
	mux.Handle("GET /api/images", a.requireUser(a.handleImages))
	mux.Handle("GET /api/authorizations", a.requireUser(a.handleAuthorizations))
	mux.Handle("GET /api/deliveries", a.requireUser(a.handleDeliveries))

	// The variants provino stored, the way a CDN in front of the variants store would serve them
	mux.HandleFunc("GET /media/{key...}", a.handleMedia)

	// The web UI is a single page, which also handles the sign-in callback
	mux.HandleFunc("GET /", serveUI())

	return a.canonicalHost(mux)
}

// Send a browser that reached the app by another host name, such as 127.0.0.1, to its public URL
// The page's origin has to be the one provino allows in `server.corsAllowedOrigins`, and the one the sign-in redirects back to
func (a *app) canonicalHost(next http.Handler) http.Handler {
	public, _ := url.Parse(a.publicURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Host != public.Host && !strings.HasPrefix(r.URL.Path, "/api/") {
			http.Redirect(w, r, a.publicURL+r.URL.RequestURI(), http.StatusFound) //nolint:gosec // The target is always on the app's own public URL, and only the path comes from the request
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve the built web UI: its files by path, and its page for every other path, so the sign-in callback lands on it
func serveUI() http.HandlerFunc {
	ui, _ := fs.Sub(webFS, "web/dist/app")
	_, err := fs.Stat(ui, "index.html")
	built := err == nil

	return func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("The web UI isn't built yet. Start the example with `make example` from the repository's root, which builds it.\n"))
			return
		}

		// Vite names the built assets after their content, so they can be cached for good, unlike the page that references them
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		info, statErr := fs.Stat(ui, name)
		if name != "" && statErr == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, ui, name) //nolint:gosec // An fs.FS only opens valid paths inside it, so no name reaches outside the embedded UI
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, ui, "index.html")
	}
}

// Tell the web UI where provino is, and which profiles it can upload with
func (a *app) handleConfig(w http.ResponseWriter, _ *http.Request) {
	type profileInfo struct {
		ID          string `json:"id"`
		Label       string `json:"label"`
		Description string `json:"description"`
	}
	profilesOut := make([]profileInfo, 0, len(profileOrder))
	for _, id := range profileOrder {
		p := profiles[id]
		profilesOut = append(profilesOut, profileInfo{ID: id, Label: p.Label, Description: p.Description})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provinoUrl": a.provinoURL,
		"clientId":   webClientID,
		"profiles":   profilesOut,
	})
}

// Serve a variant from provino's variants store
// Every key provino writes is a new file whose bytes never change, so it's served as immutable, like Pixel does
func (a *app) handleMedia(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	// Only image files are served, so the manifests and folder listings stay private
	switch path.Ext(key) {
	case ".webp", ".avif", ".jpg", ".png":
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFileFS(w, r, os.DirFS(a.variantsDir), key) //nolint:gosec // os.DirFS only opens valid paths inside the folder, so no key reaches outside the variants store
}

// Write a JSON response
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errchkjson // The status is already sent, so a failed write can't be reported to the client anyway
}
