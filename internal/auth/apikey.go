package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	"github.com/italypaleale/provino/internal/config"
)

// APIKeys authenticates the API keys in the configuration
type APIKeys struct {
	keys []apiKey
}

type apiKey struct {
	digest    []byte
	principal Principal
}

// NewAPIKeys creates the authenticator from keys the configuration has already validated
func NewAPIKeys(keys []config.ConfigAPIKey) *APIKeys {
	a := &APIKeys{keys: make([]apiKey, 0, len(keys))}
	for _, key := range keys {
		// Validation guarantees a well-formed digest
		digest, _ := hex.DecodeString(key.Digest())
		a.keys = append(a.keys, apiKey{
			digest:    digest,
			principal: Principal{Subject: key.Subject, Scopes: key.Scopes, Method: "apikey"},
		})
	}
	return a
}

// Verify returns the caller a key authenticates
// Every configured key is compared, in constant time, so the time a lookup takes says nothing about which key is closest to the one presented
func (a *APIKeys) Verify(token string) (Principal, error) {
	presented := sha256.Sum256([]byte(token))

	var (
		found     bool
		principal Principal
	)
	for _, key := range a.keys {
		if subtle.ConstantTimeCompare(presented[:], key.digest) == 1 {
			found = true
			principal = key.principal
		}
	}
	if !found {
		return Principal{}, fmt.Errorf("%w: the API key is not recognized", ErrUnauthorized)
	}

	return principal, nil
}
