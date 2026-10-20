package server

import (
	"net/http"
	"slices"
	"strings"
)

// How long, in seconds, browsers may cache the answer to a preflight request
const corsMaxAge = "7200"

// middlewareCORS lets browsers on the configured origins call the public routes
// The routes use bearer tokens rather than cookies, so a cross-origin request carries nothing a browser would add on its own
func middlewareCORS(allowedOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimRight(r.Header.Get("Origin"), "/")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Refuse preflights from other origins without reflecting caller-controlled values
			if !slices.Contains(allowedOrigins, origin) {
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				// Browsers keep the answer for this long, so an upload or a status poll doesn't wait for a preflight every time
				// A browser caps it at its own limit, which for Chromium is these two hours
				w.Header().Set("Access-Control-Max-Age", corsMaxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
