//go:build unit

package actors

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/italypaleale/provino/internal/config"
)

// The management API itself belongs to Francis, so this only checks that provino's configuration reaches it: the listener, the tokens, the allowed origins, and a certificate loaded from a folder
func TestManagementAPI(t *testing.T) {
	readOnlyToken := strings.Repeat("r", 32)
	certDir, rootCAs := writeTestCertificate(t)
	bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))

	startTestHost(t, testHostOptions{
		configure: func(c *config.Config) {
			c.Actors.Management = config.ConfigActorsManagement{
				Enabled:        true,
				Bind:           bind,
				ReadOnlyTokens: []string{readOnlyToken},
				AllowedOrigins: []string{"https://my.gofrancis.dev"},
				TLS:            config.ConfigActorsManagementTLS{Path: certDir},
			}
		},
	})

	// The client trusts only the test certificate, so a request that succeeds proves the API serves it
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12},
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	// The response is read in full and closed here, so each test only looks at what came back
	type response struct {
		status int
		header http.Header
		body   []byte
	}
	do := func(t *testing.T, method string, path string, headers map[string]string) response {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), method, "https://"+bind+path, nil)
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		res, err := client.Do(req)
		require.NoError(t, err)
		defer res.Body.Close() //nolint:errcheck

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		return response{status: res.StatusCode, header: res.Header, body: body}
	}

	// The listener starts alongside the host's other services, which can be after the host reports it's ready
	// The health check is public, and probing it over HTTPS keeps the server from logging handshakes that a bare TCP probe would abort
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+bind+"/healthz", nil)
		if !assert.NoError(c, err) {
			return
		}
		res, err := client.Do(req)
		if !assert.NoError(c, err) {
			return
		}
		_ = res.Body.Close()
		assert.Equal(c, http.StatusOK, res.StatusCode)
	}, 10*time.Second, 50*time.Millisecond)

	t.Run("requires a token", func(t *testing.T) {
		res := do(t, http.MethodGet, "/api/v1/hosts", nil)
		assert.Equal(t, http.StatusUnauthorized, res.status)
	})

	t.Run("lists the host with a read-only token", func(t *testing.T) {
		res := do(t, http.MethodGet, "/api/v1/hosts", map[string]string{"Authorization": "Bearer " + readOnlyToken})
		require.Equal(t, http.StatusOK, res.status)

		var body struct {
			Items []map[string]any `json:"items"`
		}
		err := json.Unmarshal(res.body, &body)
		require.NoError(t, err)
		assert.Len(t, body.Items, 1)
	})

	t.Run("allows the hosted dashboard's origin", func(t *testing.T) {
		res := do(t, http.MethodOptions, "/api/v1/hosts", map[string]string{
			"Origin":                        "https://my.gofrancis.dev",
			"Access-Control-Request-Method": http.MethodGet,
		})
		assert.Equal(t, http.StatusNoContent, res.status)
		assert.Equal(t, "https://my.gofrancis.dev", res.header.Get("Access-Control-Allow-Origin"))
	})
}

// Write a self-signed certificate for 127.0.0.1 into a new folder, under the file names the management API loads it from
// It returns the folder and a pool that trusts the certificate
func writeTestCertificate(t *testing.T) (string, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	// The certificate is its own root, so it needs to be allowed to sign
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "provino-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	dir := t.TempDir()
	err = os.WriteFile(filepath.Join(dir, "tls-cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir, "tls-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return dir, pool
}
