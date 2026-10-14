# The authorizer

A browser upload to `POST /v1/public/ingest` is decided by an authorizer. Before the image is stored, provino asks it who is uploading and what that caller may do: which namespace the image goes into, how large it may be, which formats it may be in, which variants to generate, and where to report the outcome.

Browser uploads are disabled when no authorizer is configured. The client API (`/v1/client/*`) never consults it, because a caller with the `server` scope is trusted with every namespace.

## The static authorizer

A single-tenant deployment can decide every upload the same way, from the configuration:

```yaml
authorizer:
  type: static
  static:
    namespace: default
    # Applied together with images.maxUploadSize, 0 leaves only that limit
    maxBytes: 20971520
    # Empty allows every supported format
    allowedFormats: [jpg, png, heic]
    variants:
      - { format: avif, quality: 60, size: { width: 800, height: 800 }, fit: cover }
      - { format: webp, quality: 80, size: { width: 1600 } }
    # Optional: where every upload's job reports its outcome
    notify:
      endpoint: app
      context: { source: browser }
```

## The HTTP authorizer

A multi-tenant application runs an endpoint that provino calls:

```yaml
authorizer:
  type: http
  http:
    url: https://app.example.com/api/provino/authorize
    timeout: 10s
    auth:
      type: hmac
      hmac:
        secrets: [whsec_…]
```

The request is authenticated the same way as a webhook delivery: signed with HMAC or Ed25519 following the Standard Webhooks specification, or carrying an OAuth2 bearer token. See [Webhooks](webhooks.md) for every option and for how to verify the request. Verifying it is what tells your endpoint the request really comes from provino.

### Request

The browser's upload starts with an optional `metadata` part, a JSON object whose `context` is forwarded to the authorizer verbatim. That's how a browser says which site, album, or form field the upload is for:

```sh
curl https://images.example.com/v1/public/ingest \
  -H "Authorization: Bearer $TOKEN" \
  -F 'metadata={"context":{"site":"6ba7b810-…","profile":"hero"}}' \
  -F 'image=@photo.jpg'
```

provino posts:

```json
{
  "version": "provino/v1",
  "subject": "user_6512…",
  "context": { "site": "6ba7b810-…", "profile": "hero" },
  "file": { "name": "photo.jpg", "contentType": "image/jpeg" }
}
```

| Field | Meaning |
| --- | --- |
| `version` | Version of the contract, always `provino/v1` |
| `subject` | The subject of the caller's token, which is who your application authorizes |
| `context` | The browser's context, at most 4 KB, never interpreted by provino |
| `file` | What the browser said about the file. Neither value is trusted: provino detects the format from the file's content |

### Response

Answer `200` with the decision:

```json
{
  "namespace": "6ba7b810-…",
  "imageId": "0b8cb0f7-2a1a-4f0f-9a9e-2b3a3f2f5f2a",
  "maxBytes": 20971520,
  "allowedFormats": ["jpg", "png", "webp", "avif", "heic", "heif"],
  "variants": [
    { "format": "webp", "quality": 80, "effort": 5, "size": { "width": 500, "height": 500 }, "fit": "contain" }
  ],
  "notify": { "endpoint": "app", "context": { "uploadId": "u_123" } }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `namespace` | yes | Namespace the image is stored in: 1 to 64 lowercase letters, digits, dots, underscores or dashes, starting with a letter or digit |
| `imageId` | no | ID of the image, a canonical UUID that isn't in use yet. If omitted, provino mints a new UUID v4 |
| `maxBytes` | no | Size limit for this upload, applied together with `images.maxUploadSize` |
| `allowedFormats` | no | Source formats the upload may be in. Formats provino doesn't support are ignored, and if none is left the answer is invalid. If omitted, every supported format is allowed |
| `variants` | no | Variants to generate once the original is stored. If omitted, the job only records the image |
| `notify` | no | Where the job reports its outcome: the name of an endpoint in `notify.endpoints` (or omit it for the default one), and a context object echoed back in the event |

Answer `403` or `404` to refuse the upload, and the browser gets a `403 upload_refused`.

Any other answer is treated as a failure of the authorizer, and the browser gets a `502 authorizer_failed`. That includes:

- any other status, or a redirect
- no answer within the timeout
- an answer larger than 256 KB
- invalid JSON, or a decision that fails validation, such as an invalid namespace, a UUID that is not v4, an invalid variant, or an unknown notification endpoint

### Example in Go

The `webhook` package has the types and the verifier:

```go
verifier, err := webhook.NewVerifier(ctx, webhook.WithHMACSecrets(os.Getenv("PROVINO_SECRET")))

http.HandleFunc("POST /api/provino/authorize", func(w http.ResponseWriter, r *http.Request) {
	body, err := verifier.VerifyRequest(r, 1<<20)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req webhook.AuthorizeRequest
	_ = json.Unmarshal(body, &req)

	var uploadCtx struct {
		Site string `json:"site"`
	}
	_ = json.Unmarshal(req.Context, &uploadCtx)
	if !userCanUpload(req.Subject, uploadCtx.Site) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	json.NewEncoder(w).Encode(webhook.AuthorizeResponse{
		Namespace: uploadCtx.Site,
		Variants:  variantsFor(uploadCtx.Site),
		Notify:    &webhook.Notify{Endpoint: "app"},
	})
})
```

## Following the upload

The ingest response carries the job's ID. The browser can poll `GET /v1/public/images/{namespace}/{imageId}/jobs/{jobId}` with the same token. That route only answers for jobs the caller started, so a browser can follow its own uploads and nobody else's. A job started through the client API is never visible there.

When a job completes, the image's manifest describes it. A notification, if the authorizer asked for one, tells your application without polling.
