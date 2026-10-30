# Deploying provino

## The container image

Release images are published at `ghcr.io/italypaleale/provino`, for `linux/amd64` and `linux/arm64`. To build one yourself:

```sh
docker build -t provino .
```

The image is distroless and runs as a non-root user.

The service reads its configuration from `PROVINO_CONFIG` if it's set, otherwise from `config.yaml` in the working directory, then `~/.provino/`, then `/etc/provino/`.

The service answers `/healthz` as soon as its process does, and `/readyz` once its actor host has registered and it can run jobs, which is what a readiness probe should check.

The container image declares a `HEALTHCHECK` that runs `curl` against `/healthz` on port 3000, the default of `server.port`. Running on another port means overriding the check along with the port:

```sh
docker run -p 8080:8080 \
  --health-cmd 'curl --fail --silent --max-time 3 http://127.0.0.1:8080/healthz' \
  provino
```

Ready-made deployments run the image unprivileged, with a read-only root file system:

- [`deploy/`](../deploy) runs a single instance on one host, with Docker Compose or as a Podman Quadlet unit.
- [`helm/`](../helm) is a Helm chart for Kubernetes, which runs one replica or several. It checks the values it's given against the requirements of [running more than one instance](#the-actor-runtime).

## Authentication

Configure **at least one way** for callers to authenticate.

### JWTs

Browsers usually authenticate with a JWT from an OpenID Connect provider:

```yaml
auth:
  jwt:
    issuer: https://tenant.example.com/
    # Defaults to server.publicUrl
    audience: https://images.example.com
```

The key set is discovered from `<issuer>/.well-known/openid-configuration` and refreshed in the background (symmetric algorithms are not accepted). Set `jwksUrl` for a provider that doesn't publish discovery, and `subjectClaim` or `scopeClaim` for one that names its claims differently.

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

**For local testing/development only.** Setting `auth.none.enabled: true` accepts every request without credentials.

## Browser uploads

Browser uploads need an [authorizer](authorizer.md), and the origins of the pages that upload:

```yaml
server:
  publicUrl: https://images.example.com
  corsAllowedOrigins: [https://app.example.com]
```

Only `/v1/public/*` answers cross-origin requests.

Uploads are rate-limited per caller, based on the token's subject. The defaults is 10/minute, with a burst of 30/minute, which lets a user drop a folder of photos in at once. Tune them with `rateLimit.rate`, `rateLimit.per` and `rateLimit.burst`.

Job status lookups, on both `/v1/public/images/…/jobs/…` and `/v1/client/images/…/jobs/…`, are rate-limited per caller too, with a budget of their own so polling never uses up the uploads': 600/minute by default, with a burst of 300, which lets a browser poll every job of a folder it just uploaded. Tune them with `rateLimit.status.rate`, `rateLimit.status.per` and `rateLimit.status.burst`. A throttled lookup gets `429 rate_limited` with a `Retry-After` header, and the Go client's `WaitForJob` simply polls again at its next interval.

When the service runs behind a load balancer or a reverse proxy, list the addresses it connects from in `server.trustedProxies`. Provino reads the client's address from `X-Forwarded-For`, or from `Forwarded` when that's missing, and from `X-Real-IP` only when the request carries neither, since a proxy that maintains one of the lists passes `X-Real-IP` through as the client sent it.

An upload may take up to `server.readTimeout` (5 minutes by default) to arrive, and may be up to `images.maxUploadSize` (32 MB by default) and `images.maxSourcePixels` (50 megapixels by default). The write deadline, `server.writeTimeout`, starts when the request headers are read, so it includes the time the upload takes to arrive: when you raise `server.readTimeout`, keep `server.writeTimeout` longer by about a minute (it's 6 minutes by default), which leaves time to store the image and start its job before the response is sent.

## The actor runtime (Francis)

Jobs run on [Francis](https://gofrancis.dev), in one of two topologies selected by `actors.runtime`.

With `embedded`, the default, the actor runtime runs inside this process and keeps its state in the database at `actors.database`. SQLite (the default, `sqlite://./data/actors.db`) is enough for a single instance. To run several instances against the same jobs, point them all at the same Postgres database and give them the same `actors.runtimePSK`.

With `external`, this process is only an actor host. It connects to a standalone Francis runtime that owns placement, scheduling and the database:

```yaml
actors:
  runtime: external
  # Address other actor hosts reach this one at
  # Uses UDP
  address: "10.0.1.7:7571"
  external:
    addresses: ["francis-runtime.internal:7570"]
    # Exactly one of bootstrapPSK and bootstrapTokenFile
    bootstrapPSK: "..."
    # With the cluster CA pinned, this host can verify the runtime on its first connection
    caPath: /etc/provino/cluster-ca.pem
```

Every instance listens on `actors.address` (UDP port 7571 by default) for the other actor hosts, in both topologies. A single instance never receives a connection on it, and it should never be exposed publicly.

The address is the one other hosts dial, so each instance needs its own. When the instances share one configuration file, set it per instance with the `PROVINO_ACTORS_ADDRESS` env var, which overrides `actors.address`. The Helm chart sets it to the pod's IP by default.

### Capacity

| Setting | Default | What it bounds |
| --- | --- | --- |
| `images.maxConcurrentEncodes` | The number of CPUs the process can use, which follows a container's CPU limit | How many variants a host encodes at once, and so how much memory it needs: see [Memory](#memory). Work beyond it waits or moves to another host |
| `actors.workflowTimeout` | 10 minutes | How long one attempt at a step may run. The job deadline is 6x the workflow timeout |

### Memory

Depending on the resolution of the images, their target size and desired formats, provino may consume significant amounts of memory. Each encode holds the work of one variant, and a host runs up to `images.maxConcurrentEncodes` encodes at once, so it needs that many times the memory of one encode, plus about 100 MB for the rest of the process.

How much memory an encode takes depends on the size (in pixel) of the image:

- JPEG, PNG and WebP sources are shrunk as they're decoded, so it's mostly the size of the variant that counts.
- AVIF and HEIC sources are decoded at full size whatever the variant, so even a small variant takes more as the source grows.
- An AVIF variant takes several times as much memory to encode as a WebP variant of the same size.

Indicatively, for sources of 5, 10 and 50 megapixels (the default limit of `images.maxSourcePixels`), you should budget:

| Variant | 5 megapixels | 10 megapixels | 50 megapixels |
| --- | --- | --- | --- |
| WebP up to 2560 pixels wide, of a JPEG, PNG or WebP | 100 MB | 100 MB | 250 MB |
| AVIF up to 2560 pixels wide, of a JPEG, PNG or WebP | 250 MB | 250 MB | 300 MB |
| Any format up to 2560 pixels wide, of an AVIF or HEIC | 200 MB | 350 MB | 1 GB |
| Full size, of any source | 300 MB | 400 MB | 1.6 GB |

A job of eight variants, WebP and AVIF at 320, 640, 1280 and 2560 pixels wide, with four encodes at once:

| Source | 5 megapixels | 10 megapixels | 50 megapixels |
| --- | --- | --- | --- |
| JPEG | 260 MB | 290 MB | 470 MB |
| WebP | 400 MB | 370 MB | 590 MB |
| HEIC | 390 MB | 520 MB | 1.8 GB |
| AVIF | 540 MB | 900 MB | 3.4 GB |

To size a host, take the figure for the largest sources and variants it may get, multiply it by `images.maxConcurrentEncodes`, and add the 100 MB. For example, a 4-CPU host with the default `images.maxConcurrentEncodes`, which is one per CPU, needs:

- About 4 GB for sources up to the default 50 megapixels and variants up to 2560 pixels wide, or 6.5 GB if variants can be full size.
- About 1.3 GB for the same sources and variants, if the sources are only JPEG, PNG or WebP.
- About 1.7 GB for any source and variant, with `images.maxSourcePixels` lowered to 10 megapixels.

When the hosts have less memory than they need, set `images.maxConcurrentEncodes` lower, or lower `images.maxSourcePixels` to the largest images you expect.

### Status retention

The status of a finished job is kept for `actors.completedWorkflowRetention` (24 hours by default). After that, the job routes answer `404`, and the image's manifest is the durable record of what the job produced.

## The source cache

Every variant of an image is encoded from the same original, so a job downloads it once into `storage.sourceCache`, a folder on the local disk. It defaults to a folder in the system temporary directory, and in a container it belongs on writable ephemeral storage. An upload also leaves a copy there as it streams in, so the job that follows usually doesn't download anything.

The cache only holds copies, so every part of it is best-effort. A job removes its file when it ends, and files left behind by a host that stopped mid-job are swept once they're older than twice `actors.workflowTimeout`.

## Upgrading

Jobs are durable across restarts, so a rolling restart resumes the jobs that were running. A release that changes the workflow's definition increments its version, so Francis can keep running the jobs that started under the previous definition.
