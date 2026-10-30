package server

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"

	httpserver "github.com/italypaleale/go-kit/httpserver"
)

// Headers a reverse proxy names the client of a request in
const (
	headerXForwardedFor = "X-Forwarded-For"
	headerForwarded     = "Forwarded"
	headerXRealIP       = "X-Real-IP"
)

// middlewareClientIP makes the request's remote address the client's own, which is what the request log and the traces record
//
// A request from a trusted proxy names its client in X-Forwarded-For or Forwarded, or in X-Real-IP when it carries neither, and its remote address becomes that client's
// The forwarding headers are dropped from every request afterwards: the tracing middleware would otherwise report the first X-Forwarded-For entry as the client, which a client can set to anything it likes
func middlewareClientIP(trustedProxies []netip.Prefix) httpserver.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			client, ok := forwardedClient(r, trustedProxies)
			if ok {
				r.RemoteAddr = formatAddr(client)
			}
			r.Header.Del(headerXForwardedFor)
			r.Header.Del(headerForwarded)
			r.Header.Del(headerXRealIP)

			next.ServeHTTP(w, r)
		})
	}
}

// Find the client a trusted proxy forwarded a request for, reporting false when the request didn't come through one or names no client
func forwardedClient(r *http.Request, trustedProxies []netip.Prefix) (netip.Addr, bool) {
	// Only the peer that connected is known for certain, and a peer that isn't a trusted proxy may have written the headers itself
	peer, ok := parseAddr(r.RemoteAddr)
	if !ok || !isTrusted(peer, trustedProxies) {
		return netip.Addr{}, false
	}

	// A single proxy may name the client in X-Real-IP instead of a list of hops
	// A proxy that maintains a list passes X-Real-IP through as the client sent it, though, so it's only read when there's no list at all
	hops := forwardedHops(r.Header)
	if len(hops) == 0 {
		return parseAddr(strings.TrimSpace(r.Header.Get(headerXRealIP)))
	}

	// Every proxy appends the address it received the request from, so the list is read from the end, past the trusted proxies, and the first address that isn't one is the client
	// The entries before it were written by the client or by proxies nobody trusts, so they're never read
	var outermost netip.Addr
	for _, entry := range slices.Backward(hops) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		addr, ok := parseAddr(entry)
		if !ok {
			// An entry that isn't an address ends what can be trusted
			break
		}
		if !isTrusted(addr, trustedProxies) {
			return addr, true
		}
		outermost = addr
	}

	// When every hop is a trusted proxy, the furthest one is as close to the client as the request says
	if outermost.IsValid() {
		return outermost, true
	}
	return netip.Addr{}, false
}

// Read the hops a request was forwarded through, nearest last
//
// X-Forwarded-For is read when the request carries it, and Forwarded only otherwise, so only one list is ever trusted
// A proxy appends to the list it maintains and passes any other through as the client sent it, and X-Forwarded-For is the one most proxies maintain
func forwardedHops(header http.Header) []string {
	values := header.Values(headerXForwardedFor)
	if len(values) > 0 {
		return strings.Split(strings.Join(values, ","), ",")
	}
	return forwardedFor(header.Values(headerForwarded))
}

// Read the "for" parameter of every element of the Forwarded header, as RFC 7239 defines it: `for=192.0.2.60;proto=https, for="[2001:db8::17]:4711"`
// An element that names no address gives "unknown", like the obfuscated names a proxy may write instead of one, so it ends what can be trusted just like a malformed entry
func forwardedFor(values []string) []string {
	var hops []string
	for _, value := range values {
		for _, element := range splitUnquoted(value, ',') {
			element = strings.TrimSpace(element)
			if element == "" {
				continue
			}

			hop := "unknown"
			for _, pair := range splitUnquoted(element, ';') {
				key, val, found := strings.Cut(pair, "=")
				if found && strings.EqualFold(strings.TrimSpace(key), "for") {
					hop = unquote(strings.TrimSpace(val))
					break
				}
			}
			hops = append(hops, hop)
		}
	}
	return hops
}

// Split a header value on a separator that isn't inside a quoted string, so a quoted value can't add elements of its own
func splitUnquoted(value string, sep byte) []string {
	var (
		parts    []string
		start    int
		quoted   bool
		escaping bool
	)
	for i := range len(value) {
		c := value[i]
		switch {
		case escaping:
			escaping = false
		case quoted && c == '\\':
			escaping = true
		case c == '"':
			quoted = !quoted
		case !quoted && c == sep:
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

// Remove the quotes around a quoted string and the backslashes that escape characters within it
func unquote(value string) string {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value
	}

	value = value[1 : len(value)-1]
	var b strings.Builder
	b.Grow(len(value))
	escaping := false
	for i := range len(value) {
		c := value[i]
		if c == '\\' && !escaping {
			escaping = true
			continue
		}
		escaping = false
		b.WriteByte(c)
	}
	return b.String()
}

// Parse an address with or without a port, as a remote address or a forwarding header carries it
func parseAddr(value string) (netip.Addr, bool) {
	addrPort, err := netip.ParseAddrPort(value)
	if err == nil {
		return addrPort.Addr().Unmap(), true
	}

	// An IPv6 address without a port may still be in brackets
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// Report whether an address belongs to one of the trusted proxies
func isTrusted(addr netip.Addr, trustedProxies []netip.Prefix) bool {
	for _, prefix := range trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Format a client's address as a remote address, without the port that only the proxy knew
// An IPv6 address keeps its brackets, so nothing that splits a remote address on its last colon reads part of it as a port
func formatAddr(addr netip.Addr) string {
	if addr.Is6() {
		return "[" + addr.String() + "]"
	}
	return addr.String()
}
