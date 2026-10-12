# Deploying provino

## The container image

Release images are published at `ghcr.io/italypaleale/provino`. To build one yourself:

```sh
docker build -t provino .
```

The image is distroless and runs as a non-root user. libvips and its codecs are copied in from a Debian stage, including the libheif AV1 encoder that AVIF output needs. The binary is linked against them through cgo, so there are no standalone binaries to download: build from source with the libvips headers installed if you don't use the image.

The service reads its configuration from `PROVINO_CONFIG` if it's set, otherwise from `config.yaml` in the working directory, then `~/.provino/`, then `/etc/provino/`. Unknown keys are rejected, so a stale option fails startup with a message naming it.

The image declares a `HEALTHCHECK` that runs `curl` against `/healthz` on port 3000, the default of `server.port`. Running on another port means overriding the check along with the port:

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

**JWTs** from an OpenID Connect provider are how browsers usually authenticate:

```yaml
auth:
  jwt:
    issuer: https://tenant.example.com/
    # Defaults to server.publicUrl
    audience: https://images.example.com
```

- **Key set.** It's discovered from `<issuer>/.well-known/openid-configuration` and refreshed in the background. A discovered key set and every redirect must stay on the issuer's origin. Startup fails if the key set can't be fetched.
- **Algorithms.** The key set decides which signature algorithms a token may use, but a symmetric algorithm is never accepted.
- **Claims.** Set `jwksUrl` for a provider that doesn't publish discovery, and `subjectClaim` or `scopeClaim` for one that names its claims differently.

**API keys** are the simplest credential for a backend:

```yaml
auth:
  apiKeys:
    - id: backend
      # The SHA-256 of the key, so the key itself isn't in the configuration
      sha256: "…"
      scopes: [server]
```

Generate a key, and its digest, with:

```sh
KEY="pvk_$(openssl rand -hex 24)"
echo "$KEY"
printf '%s' "$KEY" | sha256sum
```

A caller needs the `ingest` scope for `/v1/public/*` and the `server` scope for `/v1/client/*`. Rename them with `auth.scopes`, or set one to an empty string to accept any authenticated caller.

**For development only**, `auth.none.enabled: true` accepts every request without credentials.

## Browser uploads

Browser uploads need an [authorizer](authorizer.md), and the origins of the pages that upload:

```yaml
server:
  publicUrl: https://images.example.com
  corsAllowedOrigins: [https://app.example.com]
```

Only `/v1/public/*` answers cross-origin requests.

Uploads are rate-limited per caller, by a token bucket keyed on the subject of the caller's token. The defaults admit two uploads a minute, with a burst of ten. When the service runs behind a load balancer, list its address in `server.trustedProxies`, so that a caller without a subject is limited by its own address rather than the balancer's.

An upload may take up to `server.readTimeout` (5 minutes by default) to arrive, and may be up to `images.maxUploadSize` (32 MiB by default) and `images.maxSourcePixels` (100 megapixels by default).

## The actor runtime

Jobs run on [Francis](https://gofrancis.dev), in one of two topologies selected by `actors.runtime`:

- **`embedded`** (the default) runs the actor runtime inside this process, and keeps its state in the database at `actors.database`.
  - SQLite (the default, `sqlite://./data/actors.db`) is enough for a single instance.
  - To run several instances against the same jobs, point them all at the same Postgres database, and give them the same `actors.runtimePSK`, which their mutual TLS is derived from.
- **`external`** makes this process an actor host only. It connects to a standalone Francis runtime that owns placement, scheduling, and the database:

```yaml
actors:
  runtime: external
  # Address other actor hosts reach this one at
  address: "10.0.1.7:7571"
  external:
    addresses: ["francis-runtime.internal:7570"]
    # Exactly one of bootstrapPSK and bootstrapTokenFile
    bootstrapPSK: "…"
    # Pinning the cluster CA is what lets this host verify the runtime on its first connection
    caPath: /etc/provino/cluster-ca.pem
```

Every instance listens on `actors.address` (UDP port 7571 by default, since Francis speaks WebTransport over HTTP/3) for the other actor hosts, in both topologies. A single instance never receives a connection on it, and it should never be exposed publicly.

The address is the one other hosts dial, so each instance needs its own. When the instances share one configuration file, set it per instance with the `PROVINO_ACTORS_ADDRESS` env var, which overrides `actors.address`. The Helm chart sets it to the pod's IP.

### Capacity

- **Encoding.** `images.maxConcurrentEncodes` (default: the number of CPUs the process can use, which follows a container's CPU limit) bounds how many variants a host encodes at once. libvips is set to one thread per operation, so this is the only concurrency setting. Work beyond it waits, or moves to another host.
- **Per image.** `images.maxQueuedJobs` (default 10) bounds how many jobs can wait for one image while another of its jobs runs. Beyond it, requests get `429 queue_full`.
- **Time budgets.** `actors.workflowTimeout` (default 10 minutes) is each step's execution budget, and six times that is a job's deadline.

### Status retention

The status of a finished job is kept for `actors.completedWorkflowRetention` (24 hours by default). After that, the job routes answer `404`, and the image's manifest is the durable record of what the job produced.

## The source cache

Every variant of an image is encoded from the same original, so a job downloads it once into `storage.sourceCache`, a folder on the local disk. It defaults to a folder in the system temporary directory, and in a container it belongs on writable ephemeral storage. An upload also leaves a copy there as it streams in, so the job that follows usually doesn't download anything.

The cache only holds copies, so every part of it is best-effort. A job removes its file when it ends, and files left behind by a host that stopped mid-job are swept once they're older than twice `actors.workflowTimeout`.

## Upgrading

Jobs are durable across restarts, so a rolling restart resumes the jobs that were running. A release that changes the workflow's definition increments its version, which is what lets Francis keep running the jobs that started under the previous definition.
