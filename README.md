# provino

provino is a high-performance HTTP service that stores the images your users upload and turns each one into the set of variants your application needs.

- **Browsers upload directly.** An upload goes straight from the browser to provino, and an *authorizer* you control (an HTTP endpoint you run, or a static block in the config) decides its namespace, how large it may be, and which variants to generate.
- **Backends upload and re-render.** A trusted backend can upload images and ask for new variants of existing ones through the client API.
- **One or more replicas.** provino is built on the [Francis](https://gofrancis.dev) actor framework, so it can run as a single instance or scale out to many replicas to handle more concurrent requests.
- **Durable workflows.** Every upload and every request for variants runs as a durable workflow built on Francis. A workflow survives a restart of the service, and a job that fails undoes the changes it made.
- **Signed webhooks.** A job reports its outcome to the endpoints named in the configuration, signed following the [Standard Webhooks](https://www.standardwebhooks.com) specification (HMAC or Ed25519), or authenticated with an OAuth2 client-credentials token.
- **Your storage.** Originals and variants live in the object store you choose, next to a manifest that lists every file by its key. provino doesn't serve images: you serve them from the store, e.g. with a CDN. Supported storages:
   - Local file system
   - AWS S3 or S3-compatible endpoints
   - Vercel Blob

provino never makes a variant larger than its source, reuses the variants that are already stored when a set changes, and detects every upload's format from its content rather than its name.

## How it works

```mermaid
sequenceDiagram
    participant B as Browser
    participant P as provino
    participant A as Authorizer (your app)
    participant S as Object store
    participant W as Webhook (your app)

    B->>P: POST /v1/public/ingest (metadata, image)
    P->>A: Who is this, and what may they upload?
    A-->>P: namespace, image ID, limits, variants, notify
    P->>S: Store the original and its manifest
    P-->>B: 202 Accepted (job ID)
    Note over P: The job generates the variants
    P->>S: Store the variants and the completed manifest
    P->>W: image.optimized (signed)
    B->>P: GET /v1/public/images/.../jobs/{jobId}
```

A backend does the same through `POST /v1/client/upload` (without the authorizer) and asks for new variants of an existing image with `POST /v1/client/optimize`.

Every image belongs to a namespace, a short name that is the first segment of every object key: a tenant, a site, or `default`. The objects of an image are stored under `<namespace>/<id[0:2]>/<id[2:4]>/`, spreading it into sub-folders for performance.

## Quick start

provino needs libvips to build:

```sh
# Debian/Ubuntu
sudo apt-get install libvips-dev libheif-plugin-aomenc
# macOS
brew install vips

go build -o provino ./cmd/provino
```

This configuration runs everything locally: an API key for the backend, browser uploads decided by a static authorizer that generates two WebP variants, and storage on the local file system.

```yaml
# config.yaml
auth:
  apiKeys:
    - id: dev
      key: pvk_dev_0123456789abcdefghijklmnop
      scopes: [ingest, server]
authorizer:
  type: static
  static:
    namespace: default
    variants:
      - { format: webp, quality: 80, size: { width: 500, height: 500 } }
      - { format: webp, quality: 80, size: { width: 1600 } }
storage:
  originals: ./data/originals
  variants: ./data/variants
```

```sh
./provino

# Upload an image the way a browser does, then follow its job
curl -s http://127.0.0.1:3000/v1/public/ingest \
  -H "Authorization: Bearer pvk_dev_0123456789abcdefghijklmnop" \
  -F 'image=@photo.jpg'
# {"namespace":"default","imageId":"...","jobId":"...","status":"running",...}

curl -s http://127.0.0.1:3000/v1/client/images/default/<imageId>/jobs/<jobId> \
  -H "Authorization: Bearer pvk_dev_0123456789abcdefghijklmnop"
```

Once the job completes, the manifest at `data/variants/default/<id[0:2]>/<id[2:4]>/<imageId>_manifest.json` lists every variant and its key.

## API

The OpenAPI document is served at `/.openapi` and as `/.openapi/provino.json` and `/.openapi/provino.yaml`.

| Route | Who calls it | Purpose |
| --- | --- | --- |
| `POST /v1/public/ingest` | Browsers, with a token holding the `ingest` scope | Upload an image, as the authorizer decides |
| `GET /v1/public/images/{namespace}/{imageId}/jobs/{jobId}` | Browsers | The status of a job the caller started |
| `POST /v1/client/upload` | Backends, with the `server` scope | Upload an image |
| `POST /v1/client/optimize` | Backends | Generate variants for an existing image |
| `GET /v1/client/images/{namespace}/{imageId}/jobs/{jobId}` | Backends | The status of any job |
| `GET /.well-known/provino-keys.json` | Webhook receivers | The Ed25519 keys provino signs with |
| `GET /healthz` | Health checks | Liveness: the process answers |
| `GET /readyz` | Health checks | Readiness: the actor host is registered and jobs can run |

Uploads and optimize requests answer `202 Accepted` with a job ID as soon as the job has started. The jobs of an image run one at a time, so a request for an image that has another job running gets `409 image_busy`, and can be sent again once that job has stored its manifest, without waiting for its webhook to be delivered.

- An upload is `multipart/form-data`, with the `metadata` part before the `image` part, so provino can stream the image straight to the store.
- A variant looks like `{"format": "webp", "quality": 80, "effort": 5, "size": {"width": 1200, "height": 400}, "fit": "contain"}`. Its format is `webp`, `avif`, `jpeg` or `png`, and its fit is `contain` (the default) or `cover`.
- A source can be JPEG, PNG, WebP, AVIF or HEIC/HEIF.

Callers authenticate with a JWT from an OpenID Connect provider or with an API key.

CORS is available on public routes to cross-origin requests, from the origins in `server.corsAllowedOrigins`.

## Documentation

- [The authorizer](docs/authorizer.md): the contract browser uploads are decided by, and the static authorizer
- [Webhooks](docs/webhooks.md): the events, how they're signed, and how to verify them
- [Storage](docs/storage.md): the object layout and the manifest
- [Deploying](docs/deploying.md): the container image, the actor runtime, scaling, and operations
  - [Docker Compose and Podman](deploy/README.md): a single instance on one host
  - [Helm chart](helm/README.md): Kubernetes, with one replica or several
- [Configuration reference](config.md), and an annotated [sample](config.sample.yaml)

## License

provino is released under the [MIT License](LICENSE.md).
