package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/italypaleale/provino/variant"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// An API key used by the fixtures, long enough to pass validation
const testAPIKey = "pvk_0123456789abcdefghijklmnopqrstuv"

// validConfigForTest supplies the authentication the raw defaults intentionally omit
func validConfigForTest() *Config {
	cfg := GetDefaultConfig()
	cfg.Auth.APIKeys = []ConfigAPIKey{{ID: "backend", Key: testAPIKey, Scopes: []string{"server"}}}
	return cfg
}

// A configuration that connects to a standalone runtime, which needs more options than the embedded one
func externalConfig() *Config {
	cfg := validConfigForTest()
	cfg.Actors.Runtime = ActorRuntimeExternal
	cfg.Actors.External.Addresses = []string{"runtime.example:7570"}
	cfg.Actors.External.BootstrapPSK = "a-bootstrap-key"
	cfg.Actors.External.UnsafeNoPinnedCA = true
	return cfg
}

// A webhook endpoint signed with HMAC, which is the simplest valid endpoint
func hmacEndpoint() ConfigNotifyEndpoint {
	return ConfigNotifyEndpoint{
		URL:  "https://app.example.com/hooks/provino",
		Auth: ConfigOutboundAuth{Type: OutboundAuthHMAC, HMAC: ConfigOutboundHMAC{Secrets: []string{"whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"}}},
	}
}

func TestValidateDefaults(t *testing.T) {
	cfg := validConfigForTest()

	err := cfg.Validate(testLogger())
	require.NoError(t, err)

	// The address is split into the bind address and port the actor host listens on
	assert.Equal(t, "127.0.0.1", cfg.Actors.BindAddress)
	assert.Equal(t, 7571, cfg.Actors.BindPort)

	// A PSK is generated when the operator didn't set one, since the embedded runtime requires one even for a single instance
	assert.Len(t, cfg.GetRuntimePSK(), 32)

	// The default database is a SQLite file, with the scheme stripped off for the provider
	assert.Equal(t, Database{Kind: DatabaseKindSQLite, ConnectionString: "./data/actors.db"}, cfg.GetDatabase())

	// The source image cache defaults to a folder in the system temporary directory, which is only known at runtime
	assert.Equal(t, filepath.Join(os.TempDir(), defaultSourceCacheDirName), cfg.Storage.SourceCache)

	// An API key without a subject authenticates as its own ID
	assert.Equal(t, "backend", cfg.Auth.APIKeys[0].Subject)

	// No authorizer means browser uploads are off, which is a valid deployment
	assert.Empty(t, cfg.Authorizer.Type)
}

// The cache is a local folder rather than a DSN, and an operator that names one keeps it
func TestValidateTLS(t *testing.T) {
	// Write the TLS files a folder holds; the contents are parsed when the server starts, not here
	folder := func(t *testing.T, files ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, name := range files {
			err := os.WriteFile(filepath.Join(dir, name), []byte("pem"), 0o600)
			require.NoError(t, err)
		}
		return dir
	}

	t.Run("no TLS at all", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.SetLoadedConfigPath(filepath.Join(folder(t), "config.yaml"))
		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})

	t.Run("both PEM values", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Server.TLS.CertPEM = "cert"
		cfg.Server.TLS.KeyPEM = "key"
		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})

	t.Run("both files in the folder", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Server.TLS.Path = folder(t, tlsCertFileName, tlsKeyFileName)
		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})

	failures := map[string]struct {
		configure func(cfg *Config)
		message   string
	}{
		"a certificate PEM without its key": {
			configure: func(cfg *Config) { cfg.Server.TLS.CertPEM = "cert" },
			message:   "must be set together",
		},
		"a key PEM without its certificate": {
			configure: func(cfg *Config) { cfg.Server.TLS.KeyPEM = "key" },
			message:   "must be set together",
		},
		"a path without the files": {
			configure: func(cfg *Config) { cfg.Server.TLS.Path = folder(t) },
			message:   "doesn't contain",
		},
		"a path that doesn't exist": {
			configure: func(cfg *Config) { cfg.Server.TLS.Path = filepath.Join(folder(t), "missing") },
			message:   "doesn't contain",
		},
		"a path with only the certificate": {
			configure: func(cfg *Config) { cfg.Server.TLS.Path = folder(t, tlsCertFileName) },
			message:   "contains only one of",
		},
		"only the key next to the configuration file": {
			configure: func(cfg *Config) {
				cfg.SetLoadedConfigPath(filepath.Join(folder(t, tlsKeyFileName), "config.yaml"))
			},
			message: "contains only one of",
		},
	}
	for name, tt := range failures {
		t.Run(name, func(t *testing.T) {
			cfg := validConfigForTest()
			tt.configure(cfg)
			err := cfg.Validate(testLogger())
			require.ErrorContains(t, err, tt.message)
		})
	}
}

func TestValidateSourceCache(t *testing.T) {
	cfg := validConfigForTest()
	cfg.Storage.SourceCache = "  /var/cache/provino  "

	err := cfg.Validate(testLogger())
	require.NoError(t, err)

	assert.Equal(t, "/var/cache/provino", cfg.Storage.SourceCache)
}

func TestValidateAuth(t *testing.T) {
	t.Run("requires some form of authentication", func(t *testing.T) {
		cfg := GetDefaultConfig()
		err := cfg.Validate(testLogger())
		require.ErrorContains(t, err, "no authentication is configured")
	})

	t.Run("fills in the JWT defaults", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Auth.JWT.Issuer = "https://tenant.example.com/"

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Equal(t, "https://tenant.example.com/", cfg.Auth.JWT.Issuer)
		assert.Equal(t, cfg.Server.PublicURL, cfg.Auth.JWT.Audience)
		assert.Equal(t, "sub", cfg.Auth.JWT.SubjectClaim)
		assert.Equal(t, "scope", cfg.Auth.JWT.ScopeClaim)
	})

	t.Run("accepts a key by its digest", func(t *testing.T) {
		sum := sha256.Sum256([]byte(testAPIKey))
		cfg := GetDefaultConfig()
		cfg.Auth.APIKeys = []ConfigAPIKey{{ID: "backend", SHA256: hex.EncodeToString(sum[:])}}

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Equal(t, ConfigAPIKey{ID: "x", Key: testAPIKey}.Digest(), cfg.Auth.APIKeys[0].Digest())
	})

	t.Run("accepts none on its own", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Auth.None.Enabled = true

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})

	// An empty server scope is allowed, but it opens the client API to every caller, so it's worth a warning
	t.Run("warns about an empty server scope", func(t *testing.T) {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

		cfg := validConfigForTest()
		err := cfg.Validate(logger)
		require.NoError(t, err)
		assert.NotContains(t, logs.String(), "auth.scopes.server")

		cfg = validConfigForTest()
		cfg.Auth.Scopes.Server = ""
		err = cfg.Validate(logger)
		require.NoError(t, err)
		assert.Contains(t, logs.String(), "auth.scopes.server")
	})

	tests := map[string]func(c *Config){
		"none together with an API key": func(c *Config) { c.Auth.None.Enabled = true },
		"an issuer that isn't a URL":    func(c *Config) { c.Auth.JWT.Issuer = "tenant" },
		"an issuer without a host name": func(c *Config) { c.Auth.JWT.Issuer = "https://:443" },
		"an issuer with a query":        func(c *Config) { c.Auth.JWT.Issuer = "https://tenant.example.com/?realm=a" },
		"a JWKS URL that isn't a URL":   func(c *Config) { c.Auth.JWT.Issuer = "https://tenant.example.com"; c.Auth.JWT.JWKSURL = "keys.json" },
		"a key without the prefix":      func(c *Config) { c.Auth.APIKeys[0].Key = "0123456789abcdefghijklmnopqrstuvwxyz" },
		"a short key":                   func(c *Config) { c.Auth.APIKeys[0].Key = "pvk_short" },
		"both forms of a key":           func(c *Config) { c.Auth.APIKeys[0].SHA256 = "00" },
		"neither form of a key":         func(c *Config) { c.Auth.APIKeys[0].Key = "" },
		"a digest that isn't one":       func(c *Config) { c.Auth.APIKeys[0].Key = ""; c.Auth.APIKeys[0].SHA256 = "abc" },
		"a key without an ID":           func(c *Config) { c.Auth.APIKeys[0].ID = "" },
		"two keys with one ID": func(c *Config) {
			c.Auth.APIKeys = append(c.Auth.APIKeys, ConfigAPIKey{ID: "backend", Key: testAPIKey + "x"})
		},
		"the same key twice": func(c *Config) {
			c.Auth.APIKeys = append(c.Auth.APIKeys, ConfigAPIKey{ID: "other", Key: testAPIKey})
		},
	}
	for name, updater := range tests {
		t.Run("rejects "+name, func(t *testing.T) {
			cfg := validConfigForTest()
			updater(cfg)
			err := cfg.Validate(testLogger())
			require.Error(t, err)
		})
	}
}

//nolint:gosec // The credentials in these tests are fixtures
func TestValidateNotify(t *testing.T) {
	t.Run("a single endpoint becomes the default and gets a timeout", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": hmacEndpoint()}

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Equal(t, "app", cfg.Notify.DefaultEndpoint)
		assert.Equal(t, 15*time.Second, cfg.Notify.Endpoints["app"].Timeout)
	})

	t.Run("several endpoints leave the default to the operator", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": hmacEndpoint(), "other": hmacEndpoint()}

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Empty(t, cfg.Notify.DefaultEndpoint)
	})

	t.Run("accepts every kind of outbound authentication", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Signing.Ed25519 = ConfigSigningEd25519{Keys: []string{"whsk_x"}}
		cfg.Notify.Endpoints = map[string]ConfigNotifyEndpoint{
			"hmac":    hmacEndpoint(),
			"ed25519": {URL: "https://a.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthEd25519}},
			"secret": {URL: "https://b.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthOAuth2, OAuth2: ConfigOutboundOAuth2{
				TokenURL: "https://idp.example.com/token", ClientID: "provino", ClientSecret: "s",
			}}},
			"assertion": {URL: "https://c.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthOAuth2, OAuth2: ConfigOutboundOAuth2{
				TokenURL: "https://idp.example.com/token", ClientID: "provino", ClientAssertion: "AzureWorkloadIdentity",
			}}},
			"none": {URL: "http://d.internal", Auth: ConfigOutboundAuth{Type: OutboundAuthNone, AllowUnauthenticated: true}},
		}
		cfg.Notify.DefaultEndpoint = "hmac"

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})

	tests := map[string]func(c *Config){
		"an endpoint without a URL": func(c *Config) {
			e := hmacEndpoint()
			e.URL = ""
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": e}
		},
		"an endpoint with an invalid name": func(c *Config) { c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"has space": hmacEndpoint()} },
		"a default that doesn't exist": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": hmacEndpoint()}
			c.Notify.DefaultEndpoint = "other"
		},
		"no authentication type": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": {URL: "https://a.example.com"}}
		},
		"none without the acknowledgement": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": {URL: "https://a.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthNone}}}
		},
		"hmac without secrets": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": {URL: "https://a.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthHMAC}}}
		},
		"hmac with a secret in another form": func(c *Config) {
			e := hmacEndpoint()
			e.Auth.HMAC.Secrets = []string{"plain-secret"}
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": e}
		},
		"ed25519 without keys": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": {URL: "https://a.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthEd25519}}}
		},
		"oauth2 with both a secret and an assertion": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": {URL: "https://a.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthOAuth2, OAuth2: ConfigOutboundOAuth2{
				TokenURL: "https://idp.example.com/token", ClientID: "provino", ClientSecret: "s", ClientAssertion: "AzureWorkloadIdentity",
			}}}}
		},
		"oauth2 without a token URL": func(c *Config) {
			c.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": {URL: "https://a.example.com", Auth: ConfigOutboundAuth{Type: OutboundAuthOAuth2, OAuth2: ConfigOutboundOAuth2{
				ClientID: "provino", ClientSecret: "s",
			}}}}
		},
	}
	for name, updater := range tests {
		t.Run("rejects "+name, func(t *testing.T) {
			cfg := validConfigForTest()
			updater(cfg)
			err := cfg.Validate(testLogger())
			require.Error(t, err)
		})
	}
}

func TestValidateSigning(t *testing.T) {
	t.Run("rejects an empty key", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Signing.Ed25519 = ConfigSigningEd25519{Keys: []string{"whsk_x", " "}}
		err := cfg.Validate(testLogger())
		require.ErrorContains(t, err, "entry 1 is empty")
	})

	t.Run("accepts keys that don't sign after the one that does", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Signing.Ed25519 = ConfigSigningEd25519{Keys: []string{"whsk_x", "whsk_y"}}
		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})
}

func TestValidateAuthorizer(t *testing.T) {
	t.Run("http gets a default timeout", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Authorizer = ConfigAuthorizer{Type: AuthorizerHTTP, HTTP: ConfigAuthorizerHTTP{URL: "https://app.example.com/authorize", Auth: hmacEndpoint().Auth}}

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Equal(t, 10*time.Second, cfg.Authorizer.HTTP.Timeout)
	})

	t.Run("static variants are normalized", func(t *testing.T) {
		cfg := validConfigForTest()
		cfg.Notify.Endpoints = map[string]ConfigNotifyEndpoint{"app": hmacEndpoint()}
		cfg.Authorizer = ConfigAuthorizer{Type: AuthorizerStatic, Static: ConfigAuthorizerStatic{
			Namespace:      "default",
			AllowedFormats: []string{"jpeg", "PNG", ".heic"},
			Variants:       []variant.Spec{{Format: "jpg", Quality: 80, Size: variant.Resolution{Width: 800}}},
			Notify:         &ConfigNotifyTarget{},
		}}

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		// Formats are spelled the way they are stored, as the HTTP authorizer's answers are
		assert.Equal(t, []string{"jpg", "png", "heic"}, cfg.Authorizer.Static.AllowedFormats)
		assert.Equal(t, variant.FormatJPEG, cfg.Authorizer.Static.Variants[0].Format)
		assert.Equal(t, variant.FitContain, cfg.Authorizer.Static.Variants[0].Fit)
	})

	tests := map[string]ConfigAuthorizer{
		"an unknown type":                      {Type: "magic"},
		"http without a URL":                   {Type: AuthorizerHTTP, HTTP: ConfigAuthorizerHTTP{Auth: hmacEndpoint().Auth}},
		"http without auth":                    {Type: AuthorizerHTTP, HTTP: ConfigAuthorizerHTTP{URL: "https://app.example.com/authorize"}},
		"static without a namespace":           {Type: AuthorizerStatic},
		"static with an invalid namespace":     {Type: AuthorizerStatic, Static: ConfigAuthorizerStatic{Namespace: "Not Valid"}},
		"static with an unknown format":        {Type: AuthorizerStatic, Static: ConfigAuthorizerStatic{Namespace: "default", AllowedFormats: []string{"gif"}}},
		"static with an invalid variant":       {Type: AuthorizerStatic, Static: ConfigAuthorizerStatic{Namespace: "default", Variants: []variant.Spec{{Format: "webp"}}}},
		"static notifying without an endpoint": {Type: AuthorizerStatic, Static: ConfigAuthorizerStatic{Namespace: "default", Notify: &ConfigNotifyTarget{}}},
		"static notifying an unknown endpoint": {Type: AuthorizerStatic, Static: ConfigAuthorizerStatic{Namespace: "default", Notify: &ConfigNotifyTarget{Endpoint: "missing"}}},
	}
	for name, authorizer := range tests {
		t.Run("rejects "+name, func(t *testing.T) {
			cfg := validConfigForTest()
			cfg.Authorizer = authorizer
			err := cfg.Validate(testLogger())
			require.Error(t, err)
		})
	}
}

func TestValidateRuntimePSK(t *testing.T) {
	cfg := validConfigForTest()
	cfg.Actors.RuntimePSK = "a-key-set-by-the-operator"

	err := cfg.Validate(testLogger())
	require.NoError(t, err)
	assert.Equal(t, []byte("a-key-set-by-the-operator"), cfg.GetRuntimePSK())
}

func TestValidateActorsAddressFromEnv(t *testing.T) {
	t.Setenv(EnvActorsAddress, "10.1.2.3:7600")

	cfg := validConfigForTest()
	err := cfg.Validate(testLogger())
	require.NoError(t, err)
	assert.Equal(t, "10.1.2.3:7600", cfg.Actors.Address)
	assert.Equal(t, "10.1.2.3", cfg.Actors.BindAddress)
	assert.Equal(t, 7600, cfg.Actors.BindPort)
}

func TestValidateDatabase(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		expected Database
	}{
		{
			name:     "sqlite with the scheme",
			dsn:      "sqlite://./data/actors.db",
			expected: Database{Kind: DatabaseKindSQLite, ConnectionString: "./data/actors.db"},
		},
		{
			name:     "sqlite with an absolute path",
			dsn:      "sqlite:///var/lib/provino/actors.db",
			expected: Database{Kind: DatabaseKindSQLite, ConnectionString: "/var/lib/provino/actors.db"},
		},
		{
			name:     "sqlite without the double slash",
			dsn:      "sqlite:actors.db",
			expected: Database{Kind: DatabaseKindSQLite, ConnectionString: "actors.db"},
		},
		{
			name:     "a bare path is sqlite",
			dsn:      "./data/actors.db",
			expected: Database{Kind: DatabaseKindSQLite, ConnectionString: "./data/actors.db"},
		},
		{
			name:     "a sqlite connection string is kept as it is",
			dsn:      "file:actors.db?_txlock=immediate",
			expected: Database{Kind: DatabaseKindSQLite, ConnectionString: "file:actors.db?_txlock=immediate"},
		},
		{
			name:     "postgres",
			dsn:      "postgres://user@localhost:5432/provino",
			expected: Database{Kind: DatabaseKindPostgres, ConnectionString: "postgres://user@localhost:5432/provino"},
		},
		{
			name:     "postgres with options",
			dsn:      "postgresql://user@localhost:5432/provino?sslmode=require",
			expected: Database{Kind: DatabaseKindPostgres, ConnectionString: "postgresql://user@localhost:5432/provino?sslmode=require"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfigForTest()
			cfg.Actors.Database = tt.dsn

			err := cfg.Validate(testLogger())
			require.NoError(t, err)
			assert.Equal(t, tt.expected, cfg.GetDatabase())
		})
	}
}

// The folder of a plain SQLite path is created at startup, which is why the two forms are told apart
func TestDatabaseIsFilePath(t *testing.T) {
	tests := []struct {
		dsn      string
		expected bool
	}{
		{dsn: "sqlite://./data/actors.db", expected: true},
		{dsn: "/var/lib/provino/actors.db", expected: true},
		{dsn: "file:actors.db", expected: false},
		{dsn: "sqlite://actors.db?_txlock=immediate", expected: false},
		{dsn: "postgres://user@localhost:5432/provino", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			database, err := ParseDatabase(tt.dsn)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, database.IsFilePath())
		})
	}
}

func TestValidateExternalRuntime(t *testing.T) {
	t.Run("pins the CA from a file", func(t *testing.T) {
		caPath := filepath.Join(t.TempDir(), "ca.pem")
		err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600)
		require.NoError(t, err)

		cfg := externalConfig()
		cfg.Actors.External.UnsafeNoPinnedCA = false
		cfg.Actors.External.CAPath = caPath

		err = cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Equal(t, "-----BEGIN CERTIFICATE-----\n", string(cfg.GetExternalCAPEM()))
	})

	t.Run("pins the CA from the configuration", func(t *testing.T) {
		cfg := externalConfig()
		cfg.Actors.External.UnsafeNoPinnedCA = false
		cfg.Actors.External.CAPEM = "-----BEGIN CERTIFICATE-----\n"

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Equal(t, "-----BEGIN CERTIFICATE-----\n", string(cfg.GetExternalCAPEM()))
	})

	t.Run("opts out of pinning the CA", func(t *testing.T) {
		cfg := externalConfig()

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Empty(t, cfg.GetExternalCAPEM())
	})

	t.Run("authenticates with a token file", func(t *testing.T) {
		cfg := externalConfig()
		cfg.Actors.External.BootstrapPSK = ""
		cfg.Actors.External.BootstrapTokenFile = "/var/run/secrets/token"

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
	})

	// The embedded runtime is the only one that persists to a database, so an external one doesn't need a valid connection string
	t.Run("ignores the database", func(t *testing.T) {
		cfg := externalConfig()
		cfg.Actors.Database = ""

		err := cfg.Validate(testLogger())
		require.NoError(t, err)
		assert.Empty(t, cfg.GetDatabase().Kind)
	})
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  func() *Config
		updater func(c *Config)
	}{
		{
			name:    "negative server port",
			updater: func(c *Config) { c.Server.Port = -1 },
		},
		{
			name:    "invalid trusted proxy",
			updater: func(c *Config) { c.Server.TrustedProxies = []string{"not-an-ip"} },
		},
		{
			name:    "no rate limit",
			updater: func(c *Config) { c.RateLimit.Rate = 0 },
		},
		{
			name:    "no rate limit period",
			updater: func(c *Config) { c.RateLimit.Per = 0 },
		},
		{
			name:    "no rate limit burst",
			updater: func(c *Config) { c.RateLimit.Burst = 0 },
		},
		{
			name:    "no status rate limit",
			updater: func(c *Config) { c.RateLimit.Status.Rate = 0 },
		},
		{
			name:    "no status rate limit period",
			updater: func(c *Config) { c.RateLimit.Status.Per = 0 },
		},
		{
			name:    "no status rate limit burst",
			updater: func(c *Config) { c.RateLimit.Status.Burst = 0 },
		},
		{
			name:    "actor address without a port",
			updater: func(c *Config) { c.Actors.Address = "127.0.0.1" },
		},
		{
			name:    "actor address with an invalid port",
			updater: func(c *Config) { c.Actors.Address = "127.0.0.1:abc" },
		},
		{
			name:    "unknown runtime",
			updater: func(c *Config) { c.Actors.Runtime = "hosted" },
		},
		{
			name:    "no database",
			updater: func(c *Config) { c.Actors.Database = "" },
		},
		{
			name:    "postgres without a host",
			updater: func(c *Config) { c.Actors.Database = "postgres://" },
		},
		{
			name:    "no workflow timeout",
			updater: func(c *Config) { c.Actors.WorkflowTimeout = 0 },
		},
		{
			name:    "negative workflow retention",
			updater: func(c *Config) { c.Actors.CompletedWorkflowRetention = -1 },
		},
		{
			name:    "external runtime without addresses",
			config:  externalConfig,
			updater: func(c *Config) { c.Actors.External.Addresses = nil },
		},
		{
			name:    "external runtime with an empty address",
			config:  externalConfig,
			updater: func(c *Config) { c.Actors.External.Addresses = []string{""} },
		},
		{
			name:    "external runtime without a bootstrap method",
			config:  externalConfig,
			updater: func(c *Config) { c.Actors.External.BootstrapPSK = "" },
		},
		{
			name:    "external runtime with two bootstrap methods",
			config:  externalConfig,
			updater: func(c *Config) { c.Actors.External.BootstrapTokenFile = "/var/run/secrets/token" },
		},
		{
			name:    "external runtime without a trust decision",
			config:  externalConfig,
			updater: func(c *Config) { c.Actors.External.UnsafeNoPinnedCA = false },
		},
		{
			name:    "external runtime pinning a CA it also opts out of",
			config:  externalConfig,
			updater: func(c *Config) { c.Actors.External.CAPEM = "-----BEGIN CERTIFICATE-----\n" },
		},
		{
			name:   "external runtime pinning a CA that doesn't exist",
			config: externalConfig,
			updater: func(c *Config) {
				c.Actors.External.UnsafeNoPinnedCA = false
				c.Actors.External.CAPath = "/not/a/file.pem"
			},
		},
		{
			name:    "no storage for the originals",
			updater: func(c *Config) { c.Storage.Originals = "" },
		},
		{
			name:    "no storage for the variants",
			updater: func(c *Config) { c.Storage.Variants = "" },
		},
		{
			name:    "the same location for originals and variants",
			updater: func(c *Config) { c.Storage.Variants = c.Storage.Originals },
		},
		{
			name:    "no maximum upload size",
			updater: func(c *Config) { c.Images.MaxUploadSize = 0 },
		},
		{
			name:    "no maximum number of variants",
			updater: func(c *Config) { c.Images.MaxVariants = 0 },
		},
		{
			name:    "no maximum number of source pixels",
			updater: func(c *Config) { c.Images.MaxSourcePixels = 0 },
		},
		{
			name:    "no read timeout",
			updater: func(c *Config) { c.Server.ReadTimeout = 0 },
		},
		{
			name:    "a CORS origin with a path",
			updater: func(c *Config) { c.Server.CORSAllowedOrigins = []string{"https://app.example.com/path"} },
		},
		{
			name:    "a wildcard CORS origin",
			updater: func(c *Config) { c.Server.CORSAllowedOrigins = []string{"*"} },
		},
		{
			name:    "no maximum resolution",
			updater: func(c *Config) { c.Images.MaxResolution = 0 },
		},
		{
			name:    "negative concurrency",
			updater: func(c *Config) { c.Images.MaxConcurrentEncodes = -1 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfigForTest()
			if tt.config != nil {
				cfg = tt.config()
			}
			tt.updater(cfg)

			err := cfg.Validate(testLogger())
			require.Error(t, err)
		})
	}
}

// The sample is generated from the struct, so it must decode with unknown keys refused, the same way the service loads a configuration
func TestSampleConfigMatchesTheStruct(t *testing.T) {
	f, err := os.Open("../../config.sample.yaml")
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	cfg := GetDefaultConfig()
	err = dec.Decode(cfg)
	require.NoError(t, err)
}
