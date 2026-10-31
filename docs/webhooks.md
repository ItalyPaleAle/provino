# Webhooks

A job can report its outcome to your application with a webhook. Endpoints are named in the configuration, and whatever starts the job picks one by name: the authorizer's answer for a browser upload or the `notify` object of a client upload or optimize request.

```yaml
notify:
  # Used when a request asks for a notification without naming an endpoint
  # If empty and exactly one endpoint is configured, that one is the default
  defaultEndpoint: app
  endpoints:
    app:
      url: https://app.example.com/hooks/provino
      timeout: 15s
      auth:
        type: hmac
        hmac:
          secrets: [whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw]
```

A request asks for a notification with:

```json
"notify": { "endpoint": "app", "context": { "uploadId": "u_123" } }
```

The `context` is any JSON object of at most 4 KB, and it's echoed back in the event, so your handler can tie the event to whatever started the job.

## Events

| Event | Sent when |
| --- | --- |
| `image.stored` | A job that asked for no variants completed: the original is stored and recorded in its manifest |
| `image.optimized` | A job that asked for variants completed: every variant is stored and listed in the manifest |
| `image.failed` | A job failed, after its effects were undone and the previous manifest put back |

The body follows the Standard Webhooks payload structure:

```json
{
  "type": "image.optimized",
  "timestamp": "2026-10-11T10:11:14Z",
  "data": {
    "namespace": "default",
    "imageId": "0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a",
    "jobId": "019a7c3e-5b2d-7f41-9c8a-2d4e6f8a0b1c",
    "manifestKey": "default/0b/8c/0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a_manifest.json",
    "manifest": { "version": "provino/v1", "status": "completed", "variants": [ ... ] },
    "context": { "uploadId": "u_123" }
  }
}
```

- `data.manifest` is the manifest the job wrote. On `image.failed` it's the manifest the image is left with, which is absent when the image had none.
- `data.error` is set on `image.failed`. It starts with `image_not_found` when the image's original doesn't exist.
- `timestamp` is when the event happened, and it stays the same across delivery attempts: when the job completed, or for `image.failed`, when its failure freed the image for its next job.

## Delivery

Delivery is a step of the job's durable workflow. The app must respond with a `2xx` status code. Provino automatically retries webhook deliveries after a failure.

A job whose variants are stored completes even if its event is never acknowledged, since the outcome is also in the manifest. An `image.failed` event is retried up to five times while the job unwinds, and then dropped.

The image is free for its next job before its event is delivered. The events of two jobs of one image that run back to back can then arrive out of order if a webhook receiver is slow, so a receiver that keeps the latest state of an image should go by `timestamp`.

Deliveries are at least once, so a handler must be idempotent. Every attempt carries the same `webhook-id`, derived from the job ID and the event type, so a handler can recognize a repeat.

## Authentication

Every request, whatever authenticates it, carries the Standard Webhooks `webhook-id` and `webhook-timestamp` headers. `auth.type` decides what else it carries. The same options configure how provino authenticates to the [authorizer](authorizer.md).

### `hmac`

```yaml
auth:
  type: hmac
  hmac:
    # Every listed secret signs, so a receiver can rotate by adding the new secret here first, then removing the old one once it has switched
    secrets: [whsec_123_new..., whsec_234_old...]
```

The request carries `webhook-signature: v1,<base64 HMAC-SHA256>`, with one `v1,` entry per secret, separated by spaces. The signature covers `<webhook-id>.<webhook-timestamp>.<body>`, using the bytes after `whsec_` decoded from base64 as the key. Generate a secret with:

```sh
echo "whsec_$(openssl rand -base64 32)"
```

### `ed25519`

```yaml
auth:
  type: ed25519
```

The request carries `webhook-signature: v1a,<base64 Ed25519 signature>`, over the same content as an HMAC signature. provino signs it with a private key, and receivers verify it with the matching public key.

The keys aren't part of `auth`. They're in the top-level `signing` section, shared by every endpoint, and by the authorizer, whose `auth.type` is `ed25519`:

```yaml
signing:
  ed25519:
    keys:
      # The first key signs every request
      - !file /etc/provino/webhook-2026-10.pem
      # The previous key: still published, but no longer used to sign
      - !file /etc/provino/webhook-2026-01.pem
```

`keys` is a list of private keys. Every request is signed with the first one, and the others are only published.

`!file <path>` reads a value from a file, so the key stays out of the configuration file, and a relative path is resolved against the folder that holds the configuration file. A key could also be written in the configuration itself.

Receivers need only the public keys. provino derives each key's public key from its private key, and publishes every configured key as a JWKS at `/.well-known/provino-keys.json`. To rotate:

1. Add the new key at the end of the list.
2. Move it to the top once receivers have refreshed the set.
3. Later, remove the old one.

A key can be in any of these forms:

- PEM-encoded PKCS #8 (as OpenSSL writes it)
- A JWK: a JSON object with `"kty": "OKP"`, `"crv": "Ed25519"`, and the key's `x` and `d`. A key set (e.g. `{"keys": [...]}`)  is not accepted
- The form the [Standard Webhooks](https://www.standardwebhooks.com/) specification defines: `whsk_` followed by the base64 of the key, which is either its 32-byte seed or the 64-byte form that ends with the public key.

Each key is published with an ID, its `kid`. A JWK that has a `kid` keeps it. Any other key gets one derived from its public key: the first 7 bytes of its SHA-256, base64url-encoded, such as `KuHYNFa2kw`. The derived ID is the same whichever form the key is written in. Two keys can't have the same ID, so the same key can't be listed twice.

To generate a key with OpenSSL:

```sh
openssl genpkey -algorithm ed25519 -out webhook-2026-10.pem
```

### `oauth2`

```yaml
auth:
  type: oauth2
  oauth2:
    tokenUrl: https://tenant.example.com/oauth/token
    clientId: provino
    clientSecret: "..."
    scopes: [hooks:write]
    # Extra form parameters, for providers that need them
    endpointParams:
      audience: https://app.example.com
```

The request carries `Authorization: Bearer <token>` from a client-credentials grant. The token is cached until it nears expiry. A token request that fails makes the delivery fail and retry; the request is never sent unauthenticated.

Instead of a client secret, provino can authenticate to the token endpoint with a federated credential, sent as a JWT bearer client assertion (RFC 7523, section 2.2):

```yaml
  oauth2:
    tokenUrl: https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token
    clientId: "00000000-0000-0000-0000-000000000000"
    clientAssertion: AzureWorkloadIdentity
    scopes: [api://app.example.com/.default]
```

| `clientAssertion` | Where the assertion comes from |
| --- | --- |
| `AzureManagedIdentity` | The system-assigned Azure managed identity |
| `AzureManagedIdentity=<client-id>` | A user-assigned Azure managed identity |
| `AzureWorkloadIdentity` | Azure Workload Identity, for example in Kubernetes |
| `KubernetesServiceAccountToken` | A projected service account token at `/var/run/secrets/kubernetes.io/serviceaccount/token`, read again for every token request so a rotated token is picked up |
| `KubernetesServiceAccountToken=<path>` | A projected token at another path |
| `tsiam=<endpoint>` | A token from [tsiam](https://tsiam.italypaleale.me/) on a Tailscale network, which also needs `clientAssertionAudience` to name the resource to request |

### `none`

```yaml
auth:
  type: none
  allowUnauthenticated: true
```

The request carries only the message headers. `allowUnauthenticated` has to be set as an explicit acknowledgement that the receiver can't tell these requests from forged ones.

## Verifying a request

Verify a request before trusting anything in it:

1. Check that `webhook-timestamp` is within five minutes of your clock, which keeps a captured request from being replayed.
2. Compute the signature over the exact bytes of the body, before any decoding.
3. Compare it with every entry in `webhook-signature`, in constant time.

### Go

The `webhook` package implements all of the above. `HandleEvent` returns a handler that verifies the request, answering `401` when no signature matches, then decodes the event, passes it to your callback along with the request, and answers `204`:

```go
import "github.com/italypaleale/provino/webhook"

// HMAC: list the secrets provino signs with
verifier, err := webhook.NewVerifier(ctx, webhook.WithHMACSecrets(os.Getenv("PROVINO_WEBHOOK_SECRET")))

// Ed25519: trust the keys provino publishes, which also picks up a rotated key
// The set is refreshed in the background until Close; a refresh that fails keeps the keys fetched last
verifier, err := webhook.NewVerifier(ctx, webhook.WithJWKS("https://images.example.com/.well-known/provino-keys.json", nil))
defer verifier.Close(context.Background())

http.HandleFunc("POST /hooks/provino", webhook.HandleEvent(verifier, func(r *http.Request, e *webhook.Event) error {
	// r.Header.Get(webhook.HeaderID) is the same on every attempt, so it identifies a repeat
	switch e.Type {
	case webhook.EventImageOptimized:
		// e.Data.Manifest lists every variant by key
	case webhook.EventImageFailed:
		// e.Data.Error says why
	}
	return nil
}))
```

An error from the callback answers `500`, so provino retries the delivery. The error itself is not sent, so log it in the callback. For an event type it doesn't know, the callback should return `nil`, because a newer provino may send more.

#### With OAuth2

With `oauth2`, the request carries a bearer token rather than a signature. Pass a nil verifier, and check the token in a middleware in front of the handler. This example uses [`golang-jwt/jwt/v4`](https://pkg.go.dev/github.com/golang-jwt/jwt/v4) and [`MicahParks/keyfunc`](https://pkg.go.dev/github.com/MicahParks/keyfunc) v1, which fetches the provider's signing keys, with the provider and audience of the [`oauth2` configuration](#oauth2) above:

```go
import (
	"github.com/MicahParks/keyfunc"
	"github.com/golang-jwt/jwt/v4"
	"github.com/italypaleale/provino/webhook"
)

const (
	issuer   = "https://tenant.example.com/"
	audience = "https://app.example.com"
	clientID = "provino"
)

// The claims of the access token provino gets from the client-credentials grant
type tokenClaims struct {
	jwt.RegisteredClaims
	// The client the token was issued to; Auth0 and Entra ID name it `azp`, other providers may use `client_id` or `sub`
	AuthorizedParty string `json:"azp"`
}

// The provider's signing keys, refreshed in the background, and fetched again when a token names a key that isn't known yet
jwks, err := keyfunc.Get("https://tenant.example.com/.well-known/jwks.json", keyfunc.Options{
	RefreshInterval:   time.Hour,
	RefreshUnknownKID: true,
})

requireProvino := func(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// ParseWithClaims checks the signature and the expiry; the issuer, the audience and the client are checked here
		// Checking the client matters: any client of the provider may be able to get a token for this audience
		var claims tokenClaims
		_, err := jwt.ParseWithClaims(token, &claims, jwks.Keyfunc, jwt.WithValidMethods([]string{"RS256"}))
		if err != nil || !claims.VerifyIssuer(issuer, true) || !claims.VerifyAudience(audience, true) || claims.AuthorizedParty != clientID {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

http.Handle("POST /hooks/provino", requireProvino(webhook.HandleEvent(nil, func(r *http.Request, e *webhook.Event) error {
	// ...
	return nil
})))
```

The authorizer is protected the same way, with `webhook.HandleAuthorize(nil, ...)` behind the same middleware.

### Other languages

Use any of the Standard Webhooks libraries, for example in TypeScript with [`standardwebhooks`](https://www.npmjs.com/package/standardwebhooks):

```ts
import { Webhook } from "standardwebhooks";

const wh = new Webhook(process.env.PROVINO_WEBHOOK_SECRET!);

export async function POST(req: Request) {
  const body = await req.text();
  // Throws when the signature doesn't match or the timestamp is out of range
  const event = wh.verify(body, Object.fromEntries(req.headers)) as ProvinoEvent;
  // ...
  return new Response(null, { status: 204 });
}
```

The libraries handle the `whsec_` secret and the `v1,` signatures. For Ed25519 (`v1a,`), check that your library supports asymmetric signatures, or verify with the public keys from the JWKS directly.

With `oauth2`, verify the bearer token against your identity provider instead, as you would any machine-to-machine token.
