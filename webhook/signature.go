package webhook

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwk"
)

// Headers of a Standard Webhooks request, in the canonical form net/http uses; header names are case-insensitive, so they match the lowercase names in the specification
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"
)

// Prefixes of the Standard Webhooks key and signature encodings
const (
	secretPrefix     = "whsec_"
	publicKeyPrefix  = "whpk_"
	privateKeyPrefix = "whsk_"

	signatureVersionHMAC    = "v1"
	signatureVersionEd25519 = "v1a"
)

// Build the bytes a signature covers
func signedContent(id string, timestamp time.Time, body []byte) []byte {
	ts := strconv.FormatInt(timestamp.Unix(), 10)
	content := make([]byte, 0, len(id)+len(ts)+len(body)+2)
	content = append(content, id...)
	content = append(content, '.')
	content = append(content, ts...)
	content = append(content, '.')
	content = append(content, body...)
	return content
}

// DecodeSecret decodes an HMAC secret in the Standard Webhooks form, `whsec_` followed by the base64 of the key
// A secret without the prefix is decoded as base64 too
func DecodeSecret(secret string) ([]byte, error) {
	encoded := strings.TrimPrefix(strings.TrimSpace(secret), secretPrefix)
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("the secret is not valid base64: %w", err)
	}
	if len(key) < 16 {
		return nil, errors.New("the secret must be at least 16 bytes long")
	}
	return key, nil
}

// SignHMAC returns the `v1` signature of a message, as it appears in the webhook-signature header
func SignHMAC(key []byte, id string, timestamp time.Time, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(signedContent(id, timestamp, body))
	return signatureVersionHMAC + "," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SignEd25519 returns the `v1a` signature of a message, as it appears in the webhook-signature header
func SignEd25519(key ed25519.PrivateKey, id string, timestamp time.Time, body []byte) string {
	signature := ed25519.Sign(key, signedContent(id, timestamp, body))
	return signatureVersionEd25519 + "," + base64.StdEncoding.EncodeToString(signature)
}

// ParseEd25519PublicKey parses a public key in the Standard Webhooks form (`whpk_` followed by the base64 of the key), as a PEM-encoded PKIX key, or as a JWK
func ParseEd25519PublicKey(value string) (ed25519.PublicKey, error) {
	value = strings.TrimSpace(value)

	// A JWK is a JSON object and a PEM block is recognized by its header, so everything else is the Standard Webhooks form
	switch {
	case strings.HasPrefix(value, "{"):
		return parseEd25519PublicJWK(value)

	case strings.HasPrefix(value, "-----BEGIN"):
		block, _ := pem.Decode([]byte(value))
		if block == nil {
			return nil, errors.New("the PEM block is malformed")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse the PKIX public key: %w", err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("the PEM block does not hold an Ed25519 public key")
		}
		return key, nil

	default:
		// Try parsing as standard webhook keys (whpk_*)
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, publicKeyPrefix))
		if err != nil {
			return nil, fmt.Errorf("the public key is not valid base64: %w", err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("the public key must be %d bytes long", ed25519.PublicKeySize)
		}

		return ed25519.PublicKey(raw), nil
	}
}

// ParseEd25519PrivateKey parses a private key in the Standard Webhooks form (`whsk_` followed by the base64 of the 64-byte key or of its 32-byte seed), as a PEM-encoded PKCS #8 key, or as a JWK
func ParseEd25519PrivateKey(value string) (ed25519.PrivateKey, error) {
	value = strings.TrimSpace(value)

	switch {
	case strings.HasPrefix(value, "{"):
		return parseEd25519PrivateJWK(value)

	case strings.HasPrefix(value, "-----BEGIN"):
		block, _ := pem.Decode([]byte(value))
		if block == nil {
			return nil, errors.New("the PEM block is malformed")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse the PKCS #8 private key: %w", err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("the PEM block does not hold an Ed25519 private key")
		}
		return key, nil

	default:
		// Try parsing as standard webhook keys (whsk_*)
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, privateKeyPrefix))
		if err != nil {
			return nil, fmt.Errorf("the private key is not valid base64: %w", err)
		}
		switch len(raw) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(raw), nil
		case ed25519.PrivateKeySize:
			return ed25519.PrivateKey(raw), nil
		default:
			return nil, fmt.Errorf("the private key must be %d or %d bytes long", ed25519.SeedSize, ed25519.PrivateKeySize)
		}
	}
}

// Parse a JWK that holds an Ed25519 public key
func parseEd25519PublicJWK(value string) (ed25519.PublicKey, error) {
	key, err := jwk.ParseKey([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("failed to parse the JWK: %w", err)
	}

	// A private JWK holds the public key too, but accepting one would leave a private key where only a public key is expected
	_, isPrivate := key.(jwk.OKPPrivateKey)
	if isPrivate {
		return nil, errors.New("the JWK holds a private key, where a public key is expected")
	}

	public, err := jwk.Export[ed25519.PublicKey](key)
	if err != nil {
		return nil, fmt.Errorf("the JWK does not hold an Ed25519 public key: %w", err)
	}
	return public, nil
}

// Parse a JWK that holds an Ed25519 private key
// Parsing checks that the JWK's public key matches its private key
func parseEd25519PrivateJWK(value string) (ed25519.PrivateKey, error) {
	key, err := jwk.ParseKey([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("failed to parse the JWK: %w", err)
	}

	private, err := jwk.Export[ed25519.PrivateKey](key)
	if err != nil {
		return nil, fmt.Errorf("the JWK does not hold an Ed25519 private key: %w", err)
	}
	return private, nil
}

// EncodeEd25519PublicKey returns a public key in the Standard Webhooks form, `whpk_` followed by the base64 of the key
func EncodeEd25519PublicKey(key ed25519.PublicKey) string {
	return publicKeyPrefix + base64.StdEncoding.EncodeToString(key)
}
