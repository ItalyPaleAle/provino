package server

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// Methods a span may be named after, as the HTTP semantic conventions list them
// Any other method is named "HTTP", so a client sending made-up methods can't create spans with names of its own
var spanMethods = map[string]struct{}{
	http.MethodConnect: {},
	http.MethodDelete:  {},
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodOptions: {},
	http.MethodPatch:   {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodTrace:   {},
}

// middlewareSpanRoute names a request's span after the route the mux matched, such as "POST /v1/public/ingest", and records that route on the span and on its metrics
//
// The mux sets the pattern it matched on the request it receives, and on the same request it passes to a nested mux, so the pattern is there once the handler returns
// The tracing middleware can't see it: the request log in between hands a copy of the request down
func middlewareSpanRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		// A request no route matched keeps the span name the tracing middleware gave it, which is its method
		route := routeOf(r.Pattern)
		if route == "" {
			return
		}

		span := trace.SpanFromContext(r.Context())
		if span.IsRecording() {
			span.SetName(spanMethod(r.Method) + " " + route)
			span.SetAttributes(semconv.HTTPRoute(route))
		}

		labeler, ok := otelhttp.LabelerFromContext(r.Context())
		if ok {
			labeler.Add(semconv.HTTPRoute(route))
		}
	})
}

// Return the path of a mux pattern, without the method and host it may start with
func routeOf(pattern string) string {
	idx := strings.IndexByte(pattern, '/')
	if idx < 0 {
		return ""
	}
	return pattern[idx:]
}

// Return the method a span is named after
func spanMethod(method string) string {
	method = strings.ToUpper(method)
	_, ok := spanMethods[method]
	if !ok {
		return "HTTP"
	}
	return method
}
