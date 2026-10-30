package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStringMasksSecrets(t *testing.T) {
	secrets := []string{
		"tls-private-key",
		testAPIKey,
		"whsec_authorizer",
		"authorizer-client-secret",
		"whsec_endpoint",
		"endpoint-client-secret",
		"whsk_signing",
		"s3-secret-key",
		"s3-session-token",
		"vercel-token",
		"postgres-password",
		"runtime-psk",
		"bootstrap-psk",
	}

	cfg := validConfigForTest()
	cfg.Server.TLS.KeyPEM = "tls-private-key"
	cfg.Auth.APIKeys = append(cfg.Auth.APIKeys, ConfigAPIKey{ID: "hashed", SHA256: "0011223344556677889900112233445566778899001122334455667788990011"})
	cfg.Authorizer.HTTP.Auth = ConfigOutboundAuth{Type: OutboundAuthHMAC, HMAC: ConfigOutboundHMAC{Secrets: []string{"whsec_authorizer"}}, OAuth2: ConfigOutboundOAuth2{ClientSecret: "authorizer-client-secret"}}
	cfg.Notify.Endpoints = map[string]ConfigNotifyEndpoint{
		"app": {URL: "https://app.example.com/hooks", Auth: ConfigOutboundAuth{Type: OutboundAuthHMAC, HMAC: ConfigOutboundHMAC{Secrets: []string{"whsec_endpoint"}}, OAuth2: ConfigOutboundOAuth2{ClientSecret: "endpoint-client-secret"}}},
	}
	cfg.Signing.Ed25519.Keys = []string{"whsk_signing"}
	cfg.Storage.Originals = "s3://access-key:s3-secret-key@s3.us-east-1.amazonaws.com/bucket?prefix=originals&sessionToken=s3-session-token"
	cfg.Storage.Variants = "vercelblob:?token=vercel-token&tokenEnv=BLOB_TOKEN"
	cfg.Actors.Database = "postgres://provino:postgres-password@db.example.com:5432/provino?sslmode=require"
	cfg.Actors.RuntimePSK = "runtime-psk"
	cfg.Actors.External.BootstrapPSK = "bootstrap-psk"

	out := cfg.String()
	for _, secret := range secrets {
		assert.NotContains(t, out, secret)
	}
	assert.Contains(t, out, maskedValue)

	// What isn't a secret is still printed, so the output is useful for debugging
	assert.Contains(t, out, "0011223344556677889900112233445566778899001122334455667788990011")
	assert.Contains(t, out, "access-key")
	assert.Contains(t, out, "prefix=originals")
	assert.Contains(t, out, "tokenEnv=BLOB_TOKEN")
	assert.Contains(t, out, "db.example.com")
	assert.Contains(t, out, "https://app.example.com/hooks")

	// The configuration in use keeps its secrets
	assert.Equal(t, "tls-private-key", cfg.Server.TLS.KeyPEM)
	assert.Equal(t, testAPIKey, cfg.Auth.APIKeys[0].Key)
	assert.Equal(t, []string{"whsec_endpoint"}, cfg.Notify.Endpoints["app"].Auth.HMAC.Secrets)
	assert.Equal(t, []string{"whsk_signing"}, cfg.Signing.Ed25519.Keys)
	assert.Contains(t, cfg.Storage.Originals, "s3-secret-key")
}

//nolint:gosec // The credentials in these tests are fixtures
func TestMaskDSN(t *testing.T) {
	tests := map[string]string{
		// Connection strings without credentials are printed exactly as they're written
		"./data/originals":          "./data/originals",
		"fs:/var/lib/provino":       "fs:/var/lib/provino",
		"sqlite://./data/actors.db": "sqlite://./data/actors.db",
		"vercelblob:?tokenEnv=BLOB": "vercelblob:?tokenEnv=BLOB",
		"postgres://provino@db/p":   "postgres://provino@db/p",

		"postgres://provino:hunter2@db/p":         "postgres://provino:" + maskedValue + "@db/p",
		"postgres://db/p?password=hunter2&x=1":    "postgres://db/p?password=" + maskedValue + "&x=1",
		"s3://ak:sk@host/bucket?SessionToken=tok": "s3://ak:" + maskedValue + "@host/bucket?SessionToken=" + maskedValue,
		"postgres://db/p?password=hunter2%zz&x=1": maskedValue,
		"": "",
	}
	for dsn, want := range tests {
		t.Run(dsn, func(t *testing.T) {
			assert.Equal(t, want, maskDSN(dsn))
		})
	}
}
