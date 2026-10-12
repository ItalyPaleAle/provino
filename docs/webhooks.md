# Webhooks

A job can report its outcome to your application with a webhook. Endpoints are named in the configuration, and a request picks one by name:

- the authorizer's answer, for browser uploads;
- the `notify` object of a client upload or optimize request.

No URL ever comes from a request, so there is nothing a caller can point provino at.

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

The `context` is any JSON object of at most 4 KiB, and it's echoed back in the event, so your handler can tie the event to whatever started the job.

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
    "manifest": { "version": "provino/v1", "status": "completed", "variants": [ … ] },
    "context": { "uploadId": "u_123" }
  }
}
```

- `data.manifest` is the manifest the job wrote. On `image.failed` it's the manifest the image is left with, which is absent when the image had none.
- `data.error` is set on `image.failed`. It starts with `image_not_found` when the image's original doesn't exist.
- `timestamp` is when the event happened, and it stays the same across delivery attempts.

## Delivery

Delivery is a step of the job's durable workflow:

- **Retries.** A `2xx` response acknowledges it, and anything else is retried with backoff. A job whose variants are stored completes even when its event is never acknowledged; the outcome is still in the manifest.
- **Failure events.** These are retried up to five times while the job unwinds, then given up on.
- **No redirects.** Redirects are never followed: a `3xx` is a failed attempt.
- **Duplicates.** Deliveries are at least once, so a handler must be idempotent. Every attempt carries the same `webhook-id`, derived from the job ID and the event type, which a handler can use to recognize a repeat.

## Authentication

Every request, whatever authenticates it, carries the Standard Webhooks `webhook-id` and `webhook-timestamp` headers. `auth.type` decides what else it carries. The same options configure how provino authenticates to the [authorizer](authorizer.md).

### `hmac`

```yaml
auth:
  type: hmac
  hmac:
    # Every listed secret signs, so a receiver can rotate by adding the new secret here first, then removing the old one once it has switched
    secrets: [whsec_new…, whsec_old…]
```

The request carries `webhook-signature: v1,<base64 HMAC-SHA256>`, with one `v1,` entry per secret, separated by spaces. The signature covers `<webhook-id>.<webhook-timestamp>.<body>`, using the bytes after `whsec_` decoded from base64 as the key. Generate a secret with:

```sh
echo "whsec_$(openssl rand -base64 32)"
```

### `ed25519`

```yaml
auth:
  type: ed25519
signing:
  ed25519:
    activeKey: 2026-10
    keys:
      - id: 2026-10
        privateKeyFile: /etc/provino/webhook-2026-10.pem
      # A retired key: still published, no longer used to sign
      - id: 2026-01
        publicKeyFile: /etc/provino/webhook-2026-01.pub.pem
```

The request carries `webhook-signature: v1a,<base64 Ed25519 signature>`, made with the active key over the same content as an HMAC signature.

Receivers need only the public keys, and provino publishes every configured key, active or retired, as a JWKS at `/.well-known/provino-keys.json`. To rotate:

1. Add the new key.
2. Make it active once receivers have refreshed the set.
3. Later, remove the old one.

Keys are PEM-encoded (PKCS #8 for private keys, PKIX for public keys) or in the Standard Webhooks `whsk_…` and `whpk_…` forms:

```sh
openssl genpkey -algorithm ed25519 -out webhook-2026-10.pem
openssl pkey -in webhook-2026-10.pem -pubout -out webhook-2026-10.pub.pem
```

### `oauth2`

```yaml
auth:
  type: oauth2
  oauth2:
    tokenUrl: https://tenant.example.com/oauth/token
    clientId: provino
    clientSecret: "…"
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
| `tsiam=<endpoint>` | A token from tsiam on a Tailscale network, which also needs `clientAssertionAudience` to name the resource to request |

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

The `webhook` package implements all of it:

```go
import "github.com/italypaleale/provino/webhook"

// HMAC: list the secrets provino signs with
verifier, err := webhook.NewVerifier(ctx, webhook.WithHMACSecrets(os.Getenv("PROVINO_WEBHOOK_SECRET")))

// Ed25519: trust the keys provino publishes, which also picks up a rotated key
verifier, err := webhook.NewVerifier(ctx, webhook.WithJWKS("https://images.example.com/.well-known/provino-keys.json", nil))

http.HandleFunc("POST /hooks/provino", func(w http.ResponseWriter, r *http.Request) {
	body, err := verifier.VerifyRequest(r, 1<<20)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var event webhook.Event
	_ = json.Unmarshal(body, &event)
	switch event.Type {
	case webhook.EventImageOptimized:
		// event.Data.Manifest lists every variant by key
	case webhook.EventImageFailed:
		// event.Data.Error says why
	}
	w.WriteHeader(http.StatusNoContent)
})
```

### Other languages

Use any of the Standard Webhooks libraries, for example in TypeScript with [`standardwebhooks`](https://www.npmjs.com/package/standardwebhooks):

```ts
import { Webhook } from "standardwebhooks";

const wh = new Webhook(process.env.PROVINO_WEBHOOK_SECRET!);

export async function POST(req: Request) {
  const body = await req.text();
  // Throws when the signature doesn't match or the timestamp is out of range
  const event = wh.verify(body, Object.fromEntries(req.headers)) as ProvinoEvent;
  // …
  return new Response(null, { status: 204 });
}
```

The libraries handle the `whsec_` secret and the `v1,` signatures. For Ed25519 (`v1a,`), check that your library supports asymmetric signatures, or verify with the public keys from the JWKS directly.

With `oauth2`, verify the bearer token against your identity provider instead, as you would any machine-to-machine token.
