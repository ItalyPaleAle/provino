package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test vector published with the Standard Webhooks specification
const (
	vectorSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" // #nosec G101 -- The published test vector, not a credential
	vectorID        = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTimestamp = 1614265330
	vectorBody      = `{"test": 2432232314}`
	vectorSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func signedHeader(id string, timestamp time.Time, signatures string) http.Header {
	h := http.Header{}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(timestamp.Unix(), 10))
	h.Set(HeaderSignature, signatures)
	return h
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return public, private
}

func TestSignHMACMatchesTheReferenceVector(t *testing.T) {
	key, err := DecodeSecret(vectorSecret)
	require.NoError(t, err)

	assert.Equal(t, vectorSignature, SignHMAC(key, vectorID, time.Unix(vectorTimestamp, 0), []byte(vectorBody)))
}

func TestVerifierHMAC(t *testing.T) {
	timestamp := time.Unix(vectorTimestamp, 0)
	v, err := NewVerifier(t.Context(), WithHMACSecrets(vectorSecret))
	require.NoError(t, err)
	v.now = func() time.Time { return timestamp.Add(time.Minute) }

	t.Run("accepts the reference vector", func(t *testing.T) {
		err := v.Verify(t.Context(), signedHeader(vectorID, timestamp, vectorSignature), []byte(vectorBody))
		require.NoError(t, err)
	})

	t.Run("accepts a header listing several signatures when one matches", func(t *testing.T) {
		err := v.Verify(t.Context(), signedHeader(vectorID, timestamp, "v1,Zm9v "+vectorSignature), []byte(vectorBody))
		require.NoError(t, err)
	})

	t.Run("refuses a changed body", func(t *testing.T) {
		err := v.Verify(t.Context(), signedHeader(vectorID, timestamp, vectorSignature), []byte(`{"test": 1}`))
		require.ErrorIs(t, err, ErrInvalidSignature)
	})

	t.Run("refuses a changed ID", func(t *testing.T) {
		err := v.Verify(t.Context(), signedHeader("msg_other", timestamp, vectorSignature), []byte(vectorBody))
		require.ErrorIs(t, err, ErrInvalidSignature)
	})

	t.Run("refuses a request older than the tolerance", func(t *testing.T) {
		old := timestamp.Add(-10 * time.Minute)
		key, err := DecodeSecret(vectorSecret)
		require.NoError(t, err)
		err = v.Verify(t.Context(), signedHeader(vectorID, old, SignHMAC(key, vectorID, old, []byte(vectorBody))), []byte(vectorBody))
		require.ErrorIs(t, err, ErrTimestampOutOfRange)
	})

	t.Run("refuses a request from the future", func(t *testing.T) {
		future := timestamp.Add(10 * time.Minute)
		key, err := DecodeSecret(vectorSecret)
		require.NoError(t, err)
		err = v.Verify(t.Context(), signedHeader(vectorID, future, SignHMAC(key, vectorID, future, []byte(vectorBody))), []byte(vectorBody))
		require.ErrorIs(t, err, ErrTimestampOutOfRange)
	})

	t.Run("refuses a request without the headers", func(t *testing.T) {
		err := v.Verify(t.Context(), http.Header{}, []byte(vectorBody))
		require.ErrorIs(t, err, ErrMissingHeaders)
	})
}

// A receiver rotating its secret lists both, and provino signs with both, so deliveries keep verifying throughout the rotation
func TestVerifierHMACRotation(t *testing.T) {
	newSecret := "whsec_" + "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo="
	v, err := NewVerifier(t.Context(), WithHMACSecrets(newSecret, vectorSecret))
	require.NoError(t, err)
	v.now = func() time.Time { return time.Unix(vectorTimestamp, 0) }

	err = v.Verify(t.Context(), signedHeader(vectorID, time.Unix(vectorTimestamp, 0), vectorSignature), []byte(vectorBody))
	require.NoError(t, err)
}

func TestVerifierEd25519(t *testing.T) {
	public, private := newKey(t)
	_, otherPrivate := newKey(t)
	now := time.Now()
	body := []byte(`{"type":"image.optimized"}`)

	v, err := NewVerifier(t.Context(), WithEd25519PublicKeys(public))
	require.NoError(t, err)

	err = v.Verify(t.Context(), signedHeader("msg_1", now, SignEd25519(private, "msg_1", now, body)), body)
	require.NoError(t, err)

	err = v.Verify(t.Context(), signedHeader("msg_1", now, SignEd25519(otherPrivate, "msg_1", now, body)), body)
	require.ErrorIs(t, err, ErrInvalidSignature)

	// A v1a signature is never checked as an HMAC, and a v1 signature never as Ed25519
	err = v.Verify(t.Context(), signedHeader("msg_1", now, "v1,"+SignEd25519(private, "msg_1", now, body)[4:]), body)
	require.ErrorIs(t, err, ErrInvalidSignature)
}

func TestParseEd25519Keys(t *testing.T) {
	public, private := newKey(t)

	t.Run("Standard Webhooks form", func(t *testing.T) {
		parsedPublic, err := ParseEd25519PublicKey(EncodeEd25519PublicKey(public))
		require.NoError(t, err)
		assert.Equal(t, public, parsedPublic)

		parsedPrivate, err := ParseEd25519PrivateKey("whsk_" + base64.StdEncoding.EncodeToString(private.Seed()))
		require.NoError(t, err)
		assert.Equal(t, private, parsedPrivate)

		parsedPrivate, err = ParseEd25519PrivateKey("whsk_" + base64.StdEncoding.EncodeToString(private))
		require.NoError(t, err)
		assert.Equal(t, private, parsedPrivate)

		// The long form whose public half isn't its seed's is rejected rather than publishing the wrong public key
		otherPublic, _ := newKey(t)
		mismatched := append(append([]byte(nil), private.Seed()...), otherPublic...)
		_, err = ParseEd25519PrivateKey("whsk_" + base64.StdEncoding.EncodeToString(mismatched))
		require.ErrorContains(t, err, "doesn't match its seed")
	})

	t.Run("PEM form", func(t *testing.T) {
		publicDER, err := x509.MarshalPKIXPublicKey(public)
		require.NoError(t, err)
		parsedPublic, err := ParseEd25519PublicKey(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})))
		require.NoError(t, err)
		assert.Equal(t, public, parsedPublic)

		privateDER, err := x509.MarshalPKCS8PrivateKey(private)
		require.NoError(t, err)
		parsedPrivate, err := ParseEd25519PrivateKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})))
		require.NoError(t, err)
		assert.Equal(t, private, parsedPrivate)
	})

	t.Run("JWK form", func(t *testing.T) {
		x := base64.RawURLEncoding.EncodeToString(public)
		d := base64.RawURLEncoding.EncodeToString(private.Seed())
		publicJWK := `{"kty":"OKP","crv":"Ed25519","x":"` + x + `","kid":"ignored"}`
		privateJWK := `{"kty":"OKP","crv":"Ed25519","x":"` + x + `","d":"` + d + `"}`

		parsedPublic, err := ParseEd25519PublicKey(publicJWK)
		require.NoError(t, err)
		assert.Equal(t, public, parsedPublic)

		// A JWK in a file written by an editor ends with a newline, which is trimmed like the other forms' whitespace
		parsedPrivate, err := ParseEd25519PrivateKey(privateJWK + "\n")
		require.NoError(t, err)
		assert.Equal(t, private, parsedPrivate)

		// Each parser accepts only its own kind of key, so a private key can't be configured where a public one is expected
		_, err = ParseEd25519PublicKey(privateJWK)
		require.Error(t, err)
		_, err = ParseEd25519PrivateKey(publicJWK)
		require.Error(t, err)

		// A private key whose x doesn't match its d is rejected rather than publishing the wrong public key
		otherPublic, _ := newKey(t)
		mismatched := `{"kty":"OKP","crv":"Ed25519","x":"` + base64.RawURLEncoding.EncodeToString(otherPublic) + `","d":"` + d + `"}`
		_, err = ParseEd25519PrivateKey(mismatched)
		require.Error(t, err)

		// Keys of other types and curves are rejected
		_, err = ParseEd25519PublicKey(`{"kty":"OKP","crv":"X25519","x":"` + x + `"}`)
		require.Error(t, err)
		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		ecJWK, err := jwk.Import[jwk.Key](ecKey.Public())
		require.NoError(t, err)
		ecJSON, err := json.Marshal(ecJWK)
		require.NoError(t, err)
		_, err = ParseEd25519PublicKey(string(ecJSON))
		require.Error(t, err)

		// A key set isn't a key, even when it holds a single one
		_, err = ParseEd25519PublicKey(`{"keys":[` + publicJWK + `]}`)
		require.Error(t, err)
	})

	t.Run("malformed keys", func(t *testing.T) {
		_, err := ParseEd25519PublicKey("whpk_dG9vIHNob3J0")
		require.Error(t, err)
		_, err = ParseEd25519PrivateKey("whsk_not base64")
		require.Error(t, err)
		_, err = ParseEd25519PublicKey("-----BEGIN PUBLIC KEY-----\nbroken\n-----END PUBLIC KEY-----")
		require.Error(t, err)
	})
}

// A receiver that trusts the JWKS picks up a rotated key on the first request signed with it
func TestVerifierJWKSRefreshesOnUnknownKey(t *testing.T) {
	oldPublic, oldPrivate := newKey(t)
	newPublic, newPrivate := newKey(t)

	var published atomic.Value
	published.Store(JWKS{Keys: []JWK{NewEd25519JWK("old", oldPublic)}})
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(published.Load())
	}))
	t.Cleanup(server.Close)

	v, err := NewVerifier(t.Context(), WithJWKS(server.URL, server.Client()))
	require.NoError(t, err)
	t.Cleanup(func() {
		// The test's context is already canceled when cleanups run
		closeErr := v.Close(context.Background())
		assert.NoError(t, closeErr)
	})
	clock := time.Now()
	v.now = func() time.Time {
		return clock
	}
	body := []byte(`{}`)

	err = v.Verify(t.Context(), signedHeader("msg_1", clock, SignEd25519(oldPrivate, "msg_1", clock, body)), body)
	require.NoError(t, err)
	assert.EqualValues(t, 1, fetches.Load())

	// provino rotates: the new key is published, and the receiver refetches once the minimum interval has passed
	published.Store(JWKS{Keys: []JWK{NewEd25519JWK("new", newPublic), NewEd25519JWK("old", oldPublic)}})
	clock = clock.Add(2 * jwksMinRefreshInterval)
	err = v.Verify(t.Context(), signedHeader("msg_2", clock, SignEd25519(newPrivate, "msg_2", clock, body)), body)
	require.NoError(t, err)
	assert.EqualValues(t, 2, fetches.Load())

	// A forged signature can't make the receiver fetch again within the minimum interval
	_, forger := newKey(t)
	err = v.Verify(t.Context(), signedHeader("msg_3", clock, SignEd25519(forger, "msg_3", clock, body)), body)
	require.ErrorIs(t, err, ErrInvalidSignature)
	assert.EqualValues(t, 2, fetches.Load())
}

// A JWKS that can't be fetched again leaves the receiver with the keys it already had, and doesn't make every request try
func TestVerifierJWKSKeepsKeysWhenRefreshesFail(t *testing.T) {
	public, private := newKey(t)
	_, forger := newKey(t)

	var failing atomic.Bool
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{NewEd25519JWK("k1", public)}})
	}))
	t.Cleanup(server.Close)

	v, err := NewVerifier(t.Context(), WithJWKS(server.URL, server.Client()))
	require.NoError(t, err)
	t.Cleanup(func() {
		// The test's context is already canceled when cleanups run
		closeErr := v.Close(context.Background())
		assert.NoError(t, closeErr)
	})
	clock := time.Now()
	v.now = func() time.Time {
		return clock
	}
	body := []byte(`{}`)
	sign := func(id string, key ed25519.PrivateKey) http.Header {
		return signedHeader(id, clock, SignEd25519(key, id, clock, body))
	}

	// provino becomes unreachable, long after the set was fetched
	failing.Store(true)
	clock = clock.Add(2 * jwksMaxAge)

	// A signature that misses tries to fetch the set once, and fails
	err = v.Verify(t.Context(), sign("msg_1", forger), body)
	require.ErrorIs(t, err, ErrInvalidSignature)
	assert.EqualValues(t, 2, fetches.Load())

	// The keys fetched before still verify, without another fetch
	err = v.Verify(t.Context(), sign("msg_2", private), body)
	require.NoError(t, err)

	// A failed attempt counts against the interval like a successful one
	err = v.Verify(t.Context(), sign("msg_3", forger), body)
	require.ErrorIs(t, err, ErrInvalidSignature)
	assert.EqualValues(t, 2, fetches.Load())
}

func TestNewVerifierFailsWhenTheJWKSCantBeFetched(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	_, err := NewVerifier(t.Context(), WithJWKS(server.URL, server.Client()))
	require.ErrorContains(t, err, "failed to fetch the JWKS")
}

func TestNewVerifierRequiresAKey(t *testing.T) {
	_, err := NewVerifier(t.Context())
	require.Error(t, err)

	_, err = NewVerifier(t.Context(), WithHMACSecrets("whsec_c2hvcnQ="))
	require.ErrorContains(t, err, "at least 16 bytes")
}
