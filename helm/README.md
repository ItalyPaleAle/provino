# provino Helm chart

Runs [provino](https://github.com/italypaleale/provino) on Kubernetes: the API that receives uploads, and the actor host that generates their variants in durable jobs.

The chart deploys a StatefulSet. Its smallest form is a single replica that keeps everything on a persistent volume: the originals, the variants, and the SQLite database of the embedded [Francis](https://gofrancis.dev) actor runtime. Running more replicas means moving that state somewhere they can share, as described [below](#replicas).

## Requirements

- Kubernetes 1.25 or newer
- Helm 3.9 or newer
- A StorageClass that can provision a volume, or object storage and a Postgres database

## Installing

provino's configuration is nested, so it's easier to pass in a values file than to use `--set`. The smallest one needs only a way for callers to authenticate:

```yaml
# provino-values.yaml
config:
  server:
    publicUrl: https://images.example.com
  auth:
    apiKeys:
      - id: backend
        # The hex-encoded SHA-256 of the key, so the key itself isn't in the values
        sha256: "<digest>"
        scopes: [server]
```

[Deploying](../docs/deploying.md#api-keys) shows how to generate an API key and its digest.

Every release publishes the chart as an OCI artifact to the GitHub Container Registry, alongside the image:

```sh
helm install provino oci://ghcr.io/italypaleale/charts/provino \
  --version <version> \
  --namespace provino --create-namespace \
  --values provino-values.yaml
```

The chart's version matches the provino version it deploys. To install from a checkout of the repository instead, point Helm at the directory:

```sh
helm install provino ./helm \
  --namespace provino --create-namespace \
  --values provino-values.yaml
```

Then check that it's serving:

```sh
helm test provino -n provino
```

## Configuration

`config` takes provino's configuration as it would appear in its `config.yaml`, and every option in [config.md](../config.md) can go there. The chart renders it into a Secret and mounts it at `/etc/provino/config.yaml`.

The chart sets a few options itself, and fails the install if `config` sets them too:

- `server.bind` and `server.port` come from `ports.http`, since the container port, the probes and the Services all depend on them.
- `actors.address`, `actors.bindAddress` and `actors.bindPort` come from the pod. Every pod advertises its own IP to the other actor hosts, which a shared config file can't express, so the chart passes `$(POD_IP):<ports.peers>` in the `PROVINO_ACTORS_ADDRESS` env var, which overrides `actors.address`.

Everything else keeps provino's defaults when it's left out. Those defaults are relative to the image's working directory, `/var/lib/provino`, so the data volume is mounted at `/var/lib/provino/data`, where `storage.originals`, `storage.variants` and the SQLite `actors.database` point.

The chart also checks the configuration it renders, so a values file that can't work fails at install time with a message saying what to change, rather than in a crash-looping pod.

### Secrets

The rendered configuration usually holds credentials: webhook signing secrets, an OAuth2 client secret, an S3 key, a Postgres password. The Secret keeps them out of the pod spec, but they're still in the values you pass. Either keep that values file out of source control, or create the Secret yourself and point the chart at it:

```sh
kubectl create secret generic provino-config -n provino --from-file=config.yaml=./config.yaml

helm install provino oci://ghcr.io/italypaleale/charts/provino -n provino \
  --set existingConfigSecret=provino-config
```

With `existingConfigSecret` the chart can't look inside the file, so its checks don't apply. The file must leave `server.bind` unset (or `0.0.0.0`) and set `server.port` to `ports.http` if that isn't 3000. The chart still sets the actor address through the environment.

A credential that a storage DSN names by environment variable, such as the `tokenEnv` of a Vercel Blob store, can come from its own Secret with `extraEnvFrom`:

```yaml
config:
  storage:
    originals: vercelblob:?tokenEnv=BLOB_READ_WRITE_TOKEN&access=private&prefix=originals/
    variants: vercelblob:?tokenEnv=BLOB_READ_WRITE_TOKEN&access=public&prefix=variants/
extraEnvFrom:
  - secretRef:
      name: provino-blob
```

## Replicas

Every replica runs the API and an actor host. Francis spreads the jobs across all of them, so adding replicas adds encoding capacity as well as availability. More than one replica needs state the replicas can share:

| | One replica | More than one |
|---|---|---|
| Actor runtime | SQLite in the data volume | Postgres in `config.actors.database`, or an [external runtime](#external-francis-runtime) |
| Host-to-host mTLS | `config.actors.runtimePSK` is optional | `config.actors.runtimePSK` is required, and the same for every replica |
| Originals and variants | The data volume | S3 or Vercel Blob, or a ReadWriteMany claim in `persistence.existingClaim` |
| Data volume | One PersistentVolumeClaim | `persistence.enabled: false`, or the shared `existingClaim` |

The chart refuses a multi-replica install that is missing any of these. For example:

```yaml
replicaCount: 3
persistence:
  enabled: false
config:
  actors:
    database: postgres://provino:<password>@postgres.databases.svc.cluster.local:5432/provino
    # openssl rand -base64 32
    runtimePSK: "<random key>"
  storage:
    originals: s3://<access-key>:<secret-key>@s3.eu-west-1.amazonaws.com/provino-originals
    variants: s3://<access-key>:<secret-key>@s3.eu-west-1.amazonaws.com/provino-variants
```

The replicas reach each other on `ports.peers` (UDP 7571), by pod IP. A NetworkPolicy has to allow that port between the release's pods. The port is published on the headless Service only, and never needs to be reachable from outside the release.

Do not put the SQLite database on a networked file system such as NFS or SMB. Use a block-backed `ReadWriteOnce` volume, the kind the chart requests by default.

### Moving from one replica to several

The SQLite database only holds job state, which is short-lived: the image manifests in storage are the durable record of what every job produced. To scale out:

1. Let the running jobs finish.
2. Copy the originals and variants from the volume into the object store.
3. Delete the StatefulSet without its pods, since dropping the data volume is a change to its claim template, which Kubernetes refuses to apply (see [Upgrading](#upgrading)).
4. Upgrade with Postgres, the object store, and `persistence.enabled: false`.

```sh
kubectl delete statefulset provino -n provino --cascade=orphan
helm upgrade provino oci://ghcr.io/italypaleale/charts/provino -n provino --values provino-values.yaml
```

The chart leaves the old PersistentVolumeClaim in place, since StatefulSet claims aren't deleted automatically. Remove it once the migration is done.

## Exposing the API

Browsers upload to `/v1/public/*`, so most installs put the API behind an Ingress or a Gateway API HTTPRoute:

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: images.example.com
      paths:
        - path: /
  tls:
    - secretName: images-example-com-tls
      hosts: [images.example.com]
```

```yaml
httpRoute:
  enabled: true
  parentRefs:
    - name: gateway
      namespace: gateway-system
  hostnames: [images.example.com]
```

Behind either one, set `config.server.publicUrl` to the public origin, and list the addresses the proxy connects from in `config.server.trustedProxies`, or rate limiting sees every upload as coming from the proxy.

The proxy's limit on request bodies has to be at least `config.images.maxUploadSize`, which is 32 MiB by default, and its read timeout has to allow for a slow upload, up to `config.server.readTimeout`.

The probes use HTTPS when `config.server.tls` names a certificate, and HTTP otherwise.

## External Francis runtime

With `config.actors.runtime: external`, provino is only an actor host, and it joins a standalone Francis runtime that owns placement, scheduling, and the database. The [Francis chart](https://gofrancis.dev/docs/deploying-the-runtime/) deploys one. Its default JWT bootstrap accepts projected service account tokens, which this chart can mount:

```yaml
replicaCount: 2
persistence:
  enabled: false
config:
  actors:
    runtime: external
    external:
      addresses:
        - francis-0.francis-headless.francis.svc.cluster.local:7400
        - francis-1.francis-headless.francis.svc.cluster.local:7400
      bootstrapTokenFile: /var/run/secrets/francis/token
      # The runtime's cluster CA, from `francis print-ca`
      caPEM: |
        -----BEGIN CERTIFICATE-----
        ...
  storage:
    originals: s3://...
    variants: s3://...
projectedServiceAccountTokens:
  - name: francis
    audience: francis-runtime
    mountPath: /var/run/secrets/francis
```

The audience must match the `bootstrap.jwt.audience` of the Francis release.

## Federated credentials for webhooks

An `oauth2` endpoint can authenticate with a federated credential instead of a client secret, per RFC 7523, using a projected token as the client assertion:

```yaml
config:
  notify:
    endpoints:
      app:
        url: https://app.example.com/hooks/provino
        auth:
          type: oauth2
          oauth2:
            tokenUrl: https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token
            clientId: "<client-id>"
            clientAssertion: KubernetesServiceAccountToken=/var/run/secrets/azure/token
            scopes: ["api://<app>/.default"]
projectedServiceAccountTokens:
  - name: azure
    audience: api://AzureADTokenExchange
    mountPath: /var/run/secrets/azure
```

The identity provider has to trust the cluster's service account issuer for this service account. The chart names the service account after the release unless `serviceAccount.name` says otherwise.

## Values

### provino

| Key | Type | Default | Description |
|---|---|---|---|
| `replicaCount` | int | `1` | Number of replicas. More than one needs [shared state](#replicas). |
| `config` | object | `{}` | provino's configuration, rendered into `config.yaml`. See [config.md](../config.md). |
| `existingConfigSecret` | string | `""` | Mount this Secret's `config.yaml` instead of rendering `config`. |
| `existingConfigSecretKey` | string | `config.yaml` | Key of the config file in `existingConfigSecret`. |
| `ports.http` | int | `3000` | Port the API listens on, which the chart sets as `server.port`. |
| `ports.peers` | int | `7571` | UDP port the actor hosts reach each other on. |
| `persistence.enabled` | bool | `true` | Mount a PersistentVolumeClaim at `/var/lib/provino/data`, rather than an emptyDir. |
| `persistence.existingClaim` | string | `""` | Mount this claim instead of the one the chart creates. |
| `persistence.storageClass` | string | `""` | StorageClass of the claim. Empty uses the default, `-` disables dynamic provisioning. |
| `persistence.accessModes` | list | `[ReadWriteOnce]` | Access modes of the claim. |
| `persistence.size` | string | `10Gi` | Size of the claim. |
| `persistence.annotations` | object | `{}` | Extra annotations for the claim. |
| `tmp.sizeLimit` | string | `2Gi` | Size limit of the `/tmp` emptyDir, which holds the source cache and upload spool files. |
| `tmp.medium` | string | `""` | `Memory` makes `/tmp` a tmpfs, which counts against the memory limit. |
| `projectedServiceAccountTokens` | list | `[]` | Service account tokens to project, each with a `name`, an `audience`, a `mountPath`, and an optional `expirationSeconds`. |
| `openTelemetry.enabled` | bool | `false` | Export telemetry over OTLP. |
| `openTelemetry.endpoint` | string | `""` | Endpoint of the collector. Required when enabled. |
| `openTelemetry.protocol` | string | `http/protobuf` | `http/protobuf` or `grpc`. |
| `openTelemetry.traces` / `metrics` / `logs` | string | `otlp` | Exporter for each signal: `otlp`, `console`, or `none`. |
| `openTelemetry.sampler` / `samplerArg` | string | `""` | Trace sampler, such as `parentbased_traceidratio` with `0.1`. |

### Image

| Key | Type | Default | Description |
|---|---|---|---|
| `image.repository` | string | `ghcr.io/italypaleale/provino` | Image repository. |
| `image.tag` | string | `""` | Image tag, defaulting to `v` followed by the chart's appVersion. |
| `image.digest` | string | `""` | Image digest, which takes precedence over the tag. |
| `image.pullPolicy` | string | `IfNotPresent` | Pull policy. |
| `imagePullSecrets` | list | `[]` | Pull secrets. |
| `nameOverride` / `fullnameOverride` | string | `""` | Overrides for the generated names. |
| `commonLabels` / `commonAnnotations` | object | `{}` | Added to every resource. |

### Networking

| Key | Type | Default | Description |
|---|---|---|---|
| `service.type` | string | `ClusterIP` | Type of the API Service. |
| `service.port` | int | `3000` | Port the Service exposes the API on. |
| `service.nodePort` | int | `null` | Node port, for `NodePort` and `LoadBalancer`. |
| `service.loadBalancerIP` | string | `""` | Requested load balancer IP. |
| `service.loadBalancerSourceRanges` | list | `[]` | Source ranges allowed through the load balancer. |
| `service.externalTrafficPolicy` | string | `""` | `Cluster` or `Local`, for `NodePort` and `LoadBalancer`. |
| `service.annotations` | object | `{}` | Extra annotations for the Service. |
| `service.headless.annotations` | object | `{}` | Extra annotations for the headless Service. |
| `ingress.enabled` | bool | `false` | Create an Ingress. |
| `ingress.className` | string | `""` | Ingress class. |
| `ingress.annotations` | object | `{}` | Extra annotations for the Ingress. |
| `ingress.hosts` | list | `images.example.com` | Hosts, each with its `paths`. |
| `ingress.tls` | list | `[]` | TLS settings. |
| `httpRoute.enabled` | bool | `false` | Create a Gateway API HTTPRoute. |
| `httpRoute.annotations` | object | `{}` | Extra annotations for the HTTPRoute. |
| `httpRoute.parentRefs` | list | `[]` | Gateways to attach to. Required when enabled. |
| `httpRoute.hostnames` | list | `[]` | Hostnames to match. |

### Scheduling and pod settings

| Key | Type | Default | Description |
|---|---|---|---|
| `serviceAccount.create` | bool | `true` | Create a ServiceAccount. |
| `serviceAccount.name` | string | `""` | Name of the ServiceAccount, generated when empty. |
| `serviceAccount.annotations` | object | `{}` | Extra annotations for the ServiceAccount. |
| `serviceAccount.automountServiceAccountToken` | bool | `false` | Mount an API token in the pods. provino doesn't talk to the Kubernetes API. |
| `podDisruptionBudget.enabled` | bool | `true` | Create a PodDisruptionBudget, which only happens with more than one replica. |
| `podDisruptionBudget.minAvailable` | int/string | `1` | Minimum available pods. |
| `podDisruptionBudget.maxUnavailable` | int/string | `null` | Maximum unavailable pods, taking precedence over `minAvailable`. |
| `podAntiAffinity` | string | `soft` | Spread replicas across nodes: `soft`, `hard`, or `""` to disable. Ignored when `affinity` is set. |
| `podAnnotations` / `podLabels` | object | `{}` | Extra pod annotations and labels. |
| `podSecurityContext` | object | runs as UID/GID 65532 | Pod-level security context. |
| `securityContext` | object | no privilege escalation, read-only root, all capabilities dropped | Container-level security context. |
| `resources` | object | `{}` | Resource requests and limits. With no `config.images.maxConcurrentEncodes`, the CPU limit also bounds how many variants a pod encodes at once. |
| `livenessProbe` / `readinessProbe` / `startupProbe` | object | enabled | Probe settings. Each requests `/healthz`. |
| `terminationGracePeriodSeconds` | int | `60` | Time a pod gets to shut down. |
| `updateStrategy` | object | `{type: RollingUpdate}` | StatefulSet update strategy. |
| `podManagementPolicy` | string | `Parallel` | `Parallel` or `OrderedReady`. |
| `nodeSelector` | object | `{}` | Node selector. |
| `tolerations` | list | `[]` | Tolerations. |
| `affinity` | object | `{}` | Full affinity rules, taking precedence over `podAntiAffinity`. |
| `topologySpreadConstraints` | list | `[]` | Topology spread constraints. |
| `priorityClassName` | string | `""` | Priority class. |
| `extraEnv` / `extraEnvFrom` | list | `[]` | Extra environment variables and `envFrom` sources. |
| `extraVolumes` / `extraVolumeMounts` | list | `[]` | Extra volumes and mounts. |

## Upgrading

Jobs are durable, so a rolling upgrade resumes the ones that were running when their pod stopped. A release that changes the job workflow's definition gives it a new version, which lets Francis finish the jobs that started under the old one.

The data volume's claim template is fixed when the StatefulSet is created, and Kubernetes refuses an upgrade that changes it with `Forbidden: updates to statefulset spec for fields other than ... are forbidden`. That covers every change to `persistence` (turning it on or off, switching to an `existingClaim`, and changing `size`, `storageClass`, `accessModes`, or `annotations`), and to `commonAnnotations` while the chart creates the claim.

To apply one of these, delete the StatefulSet with `--cascade=orphan`, which keeps the pods running and the claims in place, then upgrade:

```sh
kubectl delete statefulset provino -n provino --cascade=orphan
helm upgrade provino oci://ghcr.io/italypaleale/charts/provino -n provino --reuse-values
```

The upgrade recreates the StatefulSet, which adopts the running pods and replaces them according to `updateStrategy`. A new claim template doesn't change the claims that already exist: to resize the volume, expand its PersistentVolumeClaim directly, if its StorageClass allows it.
