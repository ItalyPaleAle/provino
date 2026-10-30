package config

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
)

// Placeholder that takes the place of a secret in the printed configuration
// It's plain letters, so it reads the same inside a connection string, which would percent-encode most symbols
const maskedValue = "REDACTED"

// Query parameters of a connection string that carry a credential themselves
// Parameters such as `tokenEnv` only name where a credential is read from, so they're printed as they are
var secretDSNParams = []string{"password", "secret", "sessionToken", "token"}

// String implements fmt.Stringer and prints out the config for debugging
// Every secret in it is masked, so the output is safe to log
func (c *Config) String() string {
	//nolint:errchkjson,musttag
	enc, _ := json.Marshal(c.masked())
	return string(enc)
}

// Return a copy of the configuration with every secret masked
// The slices and maps that hold secrets are replaced rather than changed in place, so the configuration in use is never touched
func (c *Config) masked() *Config {
	m := *c

	m.Server.TLS.KeyPEM = maskSecret(m.Server.TLS.KeyPEM)

	// The digest of an API key is kept, since it can't be turned back into the key
	m.Auth.APIKeys = slices.Clone(m.Auth.APIKeys)
	for i := range m.Auth.APIKeys {
		m.Auth.APIKeys[i].Key = maskSecret(m.Auth.APIKeys[i].Key)
	}

	m.Authorizer.HTTP.Auth = m.Authorizer.HTTP.Auth.masked()
	if m.Notify.Endpoints != nil {
		endpoints := make(map[string]ConfigNotifyEndpoint, len(m.Notify.Endpoints))
		for name, endpoint := range m.Notify.Endpoints {
			endpoint.Auth = endpoint.Auth.masked()
			endpoints[name] = endpoint
		}
		m.Notify.Endpoints = endpoints
	}

	m.Signing.Ed25519.Keys = maskSecrets(m.Signing.Ed25519.Keys)

	// Connection strings can carry credentials of their own, such as an S3 secret key or a Postgres password
	m.Storage.Originals = maskDSN(m.Storage.Originals)
	m.Storage.Variants = maskDSN(m.Storage.Variants)
	m.Actors.Database = maskDSN(m.Actors.Database)

	m.Actors.RuntimePSK = maskSecret(m.Actors.RuntimePSK)
	m.Actors.External.BootstrapPSK = maskSecret(m.Actors.External.BootstrapPSK)

	return &m
}

// Return a copy of an outbound authentication with its secrets masked
func (a ConfigOutboundAuth) masked() ConfigOutboundAuth {
	a.HMAC.Secrets = maskSecrets(a.HMAC.Secrets)
	a.OAuth2.ClientSecret = maskSecret(a.OAuth2.ClientSecret)
	return a
}

// Mask a secret, leaving an empty one empty so the output still shows which options are unset
func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	return maskedValue
}

// Mask every secret of a list into a new list
func maskSecrets(values []string) []string {
	if values == nil {
		return nil
	}

	out := make([]string, len(values))
	for i, value := range values {
		out[i] = maskSecret(value)
	}
	return out
}

// Mask the credentials in a connection string: the password in its user info, and the query parameters that carry a secret
func maskDSN(dsn string) string {
	// A connection string that doesn't parse can't have its credentials told apart from the rest, so none of it is printed
	// The query is parsed on its own because url.Parse doesn't check it, and reading it through URL.Query would silently drop a malformed parameter rather than mask it
	u, err := url.Parse(dsn)
	if err != nil {
		return maskSecret(dsn)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return maskSecret(dsn)
	}

	changed := false
	_, hasPassword := u.User.Password()
	if hasPassword {
		u.User = url.UserPassword(u.User.Username(), maskedValue)
		changed = true
	}

	for name := range query {
		isSecret := slices.ContainsFunc(secretDSNParams, func(secret string) bool {
			return strings.EqualFold(name, secret)
		})
		if isSecret {
			query.Set(name, maskedValue)
			changed = true
		}
	}

	// A connection string without credentials, such as a local path, is printed exactly as it was written
	if !changed {
		return dsn
	}
	u.RawQuery = query.Encode()
	return u.String()
}
