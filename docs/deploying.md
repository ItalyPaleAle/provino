# Deploying provino

## The container image

Release images are published at `ghcr.io/italypaleale/provino`, for `linux/amd64` and `linux/arm64`. To build one yourself:

```sh
docker build -t provino .
```

The image is distroless and runs as a non-root user. libvips and its codecs are copied in from a Debian stage, including the libheif AV1 encoder that AVIF output needs. The binary is linked against them through cgo, so there are no standalone binaries to download: build from source with the libvips headers installed if you don't use the image.

The service reads its configuration from `PROVINO_CONFIG` if it's set, otherwise from `config.yaml` in the working directory, then `~/.provino/`, then `/etc/provino/`. Unknown keys are rejected, so a stale option fails startup with a message naming it.

The service answers `/healthz` as soon as its process does, and `/readyz` once its actor host has registered and it can run jobs, which is what a readiness probe should check. The image declares a `HEALTHCHECK` that runs `curl` against `/healthz` on port 3000, the default of `server.port`. Running on another port means overriding the check along with the port:

```sh
docker run -p 8080:8080 \
  --health-cmd 'curl --fail --silent --max-time 3 http://127.0.0.1:8080/healthz' \
  provino
```

Ready-made deployments run the image unprivileged, with a read-only root file system:

- [`deploy/`](../deploy) runs a single instance on one host, with Docker Compose or as a Podman Quadlet unit.
- [`helm/`](../helm) is a Helm chart for Kubernetes, which runs one replica or several. It checks the values it's given against the requirements of [running more than one instance](#the-actor-runtime).

## Authentication

Configure at least one way for callers to authenticate.

### JWTs

Browsers usually authenticate with a JWT from an OpenID Connect provider:

```yaml
auth:
  jwt:
    issuer: https://tenant.example.com/
    # Defaults to server.publicUrl
    audience: https://images.example.com
```

The key set is discovered from `<issuer>/.well-known/openid-configuration` and refreshed in the background. The discovered key set, and every redirect, must stay on the issuer's origin, and startup fails if the key set can't be fetched. Set `jwksUrl` for a provider that doesn't publish discovery, and `subjectClaim` or `scopeClaim` for one that names its claims differently.

The key set decides which signature algorithms a token may use, but a symmetric algorithm, or `none`, is never accepted.

### API keys

An API key is the simplest credential for a backend:

```yaml
auth:
  apiKeys:
    - id: backend
      # The SHA-256 of the key, so the key itself isn't in the configuration
      sha256: "..."
      scopes: [server]
```

Generate a key, and its digest, with:

```sh
KEY="pvk_$(openssl rand -hex 24)"
echo "$KEY"
printf '%s' "$KEY" | sha256sum
```

A caller needs the `ingest` scope for `/v1/public/*` and the `server` scope for `/v1/client/*`. Rename them with `auth.scopes`, or set one to an empty string to accept any authenticated caller.

### No authentication

For development only, `auth.none.enabled: true` accepts every request without credentials.

## Browser uploads

Browser uploads need an [authorizer](authorizer.md), and the origins of the pages that upload:

```yaml
server:
  publicUrl: https://images.example.com
  corsAllowedOrigins: [https://app.example.com]
```

Only `/v1/public/*` answers cross-origin requests.

Uploads are rate-limited per caller, by a token bucket keyed on the subject of the caller's token. The defaults admit ten uploads a minute, with a burst of thirty, which lets a user drop a folder of photos in at once. Tune them with `rateLimit.rate`, `rateLimit.per` and `rateLimit.burst`.

When the service runs behind a load balancer or a reverse proxy, list the addresses it connects from in `server.trustedProxies`. The request log and the traces then record the client each request was forwarded for, rather than the proxy. It's read from `X-Forwarded-For`, from the standard `Forwarded` header when a request doesn't carry `X-Forwarded-For`, or from `X-Real-IP`. Those headers are dropped from every other request, so a client can't put an address of its choosing in the logs.

A proxy appends to the header it maintains and passes the others through as the client sent them. If yours maintains `Forwarded` only, have it drop `X-Forwarded-For` from incoming requests, or a client could name an address of its choosing there.

An upload may take up to `server.readTimeout` (5 minutes by default) to arrive, and may be up to `images.maxUploadSize` (32 MiB by default) and `images.maxSourcePixels` (50 megapixels by default).

## The actor runtime

Jobs run on [Francis](https://gofrancis.dev), in one of two topologies selected by `actors.runtime`.

With `embedded`, the default, the actor runtime runs inside this process and keeps its state in the database at `actors.database`. SQLite (the default, `sqlite://./data/actors.db`) is enough for a single instance. To run several instances against the same jobs, point them all at the same Postgres database and give them the same `actors.runtimePSK`, which their mutual TLS is derived from.

With `external`, this process is only an actor host. It connects to a standalone Francis runtime that owns placement, scheduling and the database:

```yaml
actors:
  runtime: external
  # Address other actor hosts reach this one at
  address: "10.0.1.7:7571"
  external:
    addresses: ["francis-runtime.internal:7570"]
    # Exactly one of bootstrapPSK and bootstrapTokenFile
    bootstrapPSK: "..."
    # With the cluster CA pinned, this host can verify the runtime on its first connection
    caPath: /etc/provino/cluster-ca.pem
```

Every instance listens on `actors.address` (UDP port 7571 by default, since Francis speaks WebTransport over HTTP/3) for the other actor hosts, in both topologies. A single instance never receives a connection on it, and it should never be exposed publicly.

The address is the one other hosts dial, so each instance needs its own. When the instances share one configuration file, set it per instance with the `PROVINO_ACTORS_ADDRESS` env var, which overrides `actors.address`. The Helm chart sets it to the pod's IP.

### Capacity

| Setting | Default | What it bounds |
| --- | --- | --- |
| `images.maxConcurrentEncodes` | The number of CPUs the process can use, which follows a container's CPU limit | How many variants a host encodes at once, and so how much memory it needs: see [Memory](#memory). libvips runs one thread per operation, so this is the only concurrency setting. Work beyond it waits, or moves to another host |
| `images.maxQueuedJobs` | 10 | How many jobs can wait for one image while another of its jobs runs. Beyond it, requests get `429 queue_full` |
| `actors.workflowTimeout` | 10 minutes | How long one attempt at a step may run. Six times that is a job's deadline |

### Memory

Memory is what usually runs out first, and it's `images.maxConcurrentEncodes` that bounds it: each encode holds one variant's work, and how much that takes depends on the source.

- JPEG, PNG and WebP sources are resized as they're decoded, without ever holding the full-size image, so the variant's size decides. Variants up to 2560 pixels wide take 50 to 250 MB each, and one close to the source's full size takes up to about 900 MB.
- AVIF and HEIC sources are decoded at full size whatever the variant, which takes about 800 MB for a 50-megapixel source.

These figures are for the 50-megapixel default of `images.maxSourcePixels`, and they scale with it. The rest of the process takes about 100 MB.

So give each host about 1 GB for every encode it runs at once, plus that 100 MB: with the default `images.maxConcurrentEncodes`, which is one per CPU, a 4-CPU host needs about 4 GB. When the hosts have less memory than that, set `images.maxConcurrentEncodes` lower, or lower `images.maxSourcePixels`. If the uploads are JPEG, PNG and WebP photos and the variants are web-sized, a quarter of that is enough: a job of eight WebP and AVIF variants up to 2560 pixels wide, with four encodes at once, peaked at about 520 MB for a 50-megapixel JPEG and 700 MB for a WebP.

### Status retention

The status of a finished job is kept for `actors.completedWorkflowRetention` (24 hours by default). After that, the job routes answer `404`, and the image's manifest is the durable record of what the job produced.

## The source cache

Every variant of an image is encoded from the same original, so a job downloads it once into `storage.sourceCache`, a folder on the local disk. It defaults to a folder in the system temporary directory, and in a container it belongs on writable ephemeral storage. An upload also leaves a copy there as it streams in, so the job that follows usually doesn't download anything.

The cache only holds copies, so every part of it is best-effort. A job removes its file when it ends, and files left behind by a host that stopped mid-job are swept once they're older than twice `actors.workflowTimeout`.

## Upgrading

Jobs are durable across restarts, so a rolling restart resumes the jobs that were running. A release that changes the workflow's definition increments its version, so Francis can keep running the jobs that started under the previous definition.
