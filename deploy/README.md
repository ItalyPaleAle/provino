# Running provino

This folder has two ways to run a single instance of provino on one host:

- [`compose.yaml`](./compose.yaml) runs it with Docker Compose, or anything else that reads a Compose file.
- [`provino.kube`](./provino.kube) is a Podman [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) unit, which runs it as a systemd service.

Both run the container as the uid 65532 the image was built for, with a read-only root file system and every capability dropped. To run provino on Kubernetes, use the [Helm chart](../helm) instead.

## The image

The release workflow pushes `ghcr.io/italypaleale/provino`, which is the name both files point at. A push to `main` publishes `:edge`; a `v*` tag publishes `:vX.Y.Z`, `:vX.Y`, `:vX` and `:latest`, so both files as written track the most recent release.

The image is published for `linux/amd64` and `linux/arm64`, and every tag covers both, so the engine pulls the one that matches the host.

## Docker Compose

The configuration is [`compose.config.yaml`](./compose.config.yaml), which Compose mounts read-only at `/etc/provino/config.yaml`. Before the first start, generate an API key and put its SHA-256 in `auth.apiKeys`, in place of the example value:

```sh
KEY="pvk_$(openssl rand -hex 24)"
echo "$KEY"
printf '%s' "$KEY" | sha256sum
```

Then start it and check it:

```sh
docker compose up -d

docker compose ps                         # healthy once the first start is done
curl -i http://127.0.0.1:3000/healthz     # 204 No Content
docker compose logs provino
```

Compose bind-mounts the config file instead of copying it in, since it can't copy anything into a container whose root file system is read-only. The file keeps the permissions it has on the host, so uid 65532 has to be able to read it:

- With Docker Desktop, or with rootless Docker or Podman, `chmod 644 compose.config.yaml` is enough.
- With a rootful Docker on Linux, the file can be readable by that group alone rather than by everyone: `sudo chgrp 65532 compose.config.yaml && chmod 640 compose.config.yaml`.

Once you configure webhooks or an authorizer, the file holds their signing secrets, so don't commit a copy with real values.

### What it stores

The `provino-data` volume holds everything the service persists: the uploaded originals, the generated variants, and the SQLite database of the embedded actor runtime. It is mounted at `/var/lib/provino`, the image's working directory. The image creates that directory owned by uid 65532, and a new volume takes the ownership of the directory it's first mounted on, so the service can write to it without a `chown`.

`provino-tmp` is mounted at `/tmp`, which holds the source cache: each running job keeps a copy of the original its variants are encoded from. Uploads to S3 or Vercel Blob are also spooled through it. It's a volume rather than a tmpfs, so a burst of uploads doesn't come out of memory, and nothing in it has to survive a restart.

To start over from nothing, `docker compose down --volumes` removes both.

The service isn't given a memory limit. If you add one, allow about 1 GB for each variant it encodes at once, which is one per CPU unless `images.maxConcurrentEncodes` says otherwise: see [Memory](../docs/deploying.md#memory).

### Editing the configuration

Edit `compose.config.yaml` and recreate the container with `docker compose up -d --force-recreate`, which mounts the file afresh: an editor that saves by replacing the file leaves a running container with the old copy. The same advice as for the Podman pod [below](#changing-the-configuration) applies: replace the example values, and see [deploying](../docs/deploying.md) for the rest.

To store images in S3 or Vercel Blob instead of the volume, change `storage.originals` and `storage.variants`. Credentials that a Vercel Blob DSN reads from an environment variable go under `environment:` in `compose.yaml`, or in an `env_file`.

To serve on another port, change the host side of `ports` in `compose.yaml`. Changing the port provino itself listens on means setting `server.port` in the config, and changing the container side of `ports` and the port in the health check to match.

## Podman Quadlet

[`provino.kube`](./provino.kube) is a Quadlet unit that plays the pod in [`provino.yaml`](./provino.yaml). Quadlet needs Podman 4.4 or newer.

Installed as a user unit, it runs under rootless Podman, so nothing here runs as root.

### Installing

```sh
mkdir -p ~/.config/containers/systemd
cp provino.kube provino.yaml ~/.config/containers/systemd/

systemctl --user daemon-reload
systemctl --user start provino
```

There is no `systemctl --user enable`: Quadlet generates the unit from the `[Install]` section, so `daemon-reload` installs it, and it starts on login from then on.

To have it start at boot rather than at login, and keep running after logout, enable lingering for the user once:

```sh
loginctl enable-linger "$USER"
```

Then check it:

```sh
systemctl --user status provino
curl -i http://127.0.0.1:3000/healthz     # 204 No Content

# The containers are named after the pod, so this is where the logs are
podman logs provino-provino
podman healthcheck run provino-provino
```

### A locally built image

An image that only exists in local storage needs `imagePullPolicy: Never` on the container in the manifest. Without it the `:latest` tag makes Podman pull on every start, which overwrites the local build with the published one.

### What it stores

One volume holds everything the service persists: the uploaded originals, the generated variants, and the SQLite database the embedded actor runtime keeps its job state and alarms in. It is claimed as `provino-data` and Podman backs it with a named volume of that name.

`/tmp` is a separate `emptyDir`, backed by disk rather than by memory so that a burst of uploads doesn't come out of the container's memory limit. It holds the source cache: each running job keeps a copy of the original its variants are encoded from, and an upload leaves one there as it streams in. Uploads to S3 or Vercel Blob are also spooled through `/tmp`, while the file system storage configured above writes its temporary files inside the data volume instead.

Nothing in `/tmp` has to survive a restart. Its 1Gi limit covers several concurrent jobs and uploads of `images.maxUploadSize`.

The container is limited to 2 CPUs and 2Gi of memory. `images.maxConcurrentEncodes` follows the CPU limit when it's unset, so the pod encodes two variants at once, and each can take up to about 1 GB: raise the two limits together, as [Memory](../docs/deploying.md#memory) explains.

If the container fails to start with a permission error on `/var/lib/provino/data`, the volume was created before the pod claimed it and is owned by the wrong uid. Podman chowns a volume it creates empty, so removing it and letting the unit recreate it is usually enough:

```sh
systemctl --user stop provino
podman volume rm provino-data
systemctl --user start provino
```

### Health checks

The manifest carries a startup probe and a liveness probe, both running the `curl` the image already carries for its own `HEALTHCHECK`. `podman kube play` turns them into container health checks. The startup probe gives the first run time to migrate the actor database before the liveness probe starts counting failures.

There is no readiness probe, because a pod Podman plays has no service in front of it to take the pod out of.

### Changing the configuration

The service's configuration is the `provino-config` Secret in the same file, mounted read-only at `/etc/provino`. The manifest uses `stringData` so the YAML stays editable while Kubernetes or Podman stores the Secret in its encoded form. Edit it there and restart the unit.

Before the first start, replace every example value:

- `server.publicUrl` is provino's public origin, which is also the default audience of the JWTs it accepts, and `server.corsAllowedOrigins` lists the pages browsers upload from.
- `auth.jwt.issuer` is your identity provider. `auth.apiKeys` lists the keys backends call the client API with, each by its SHA-256 so the key itself isn't in the manifest. See [deploying](../docs/deploying.md#api-keys) for how to generate one.
- `authorizer.http` is the endpoint that decides each browser upload, see [the authorizer](../docs/authorizer.md), and `notify.endpoints` are where jobs report their outcome, see [webhooks](../docs/webhooks.md). Both are signed with a `whsec_` secret your application verifies.

The example uses local storage to make the pod self-contained. To serve the variants from object storage, point `storage.originals` and `storage.variants` at an S3 or Vercel Blob store instead; see [storage](../docs/storage.md).

The defaults suit a single instance. Running more than one, on separate hosts, takes four changes:

- `actors.database` has to be Postgres rather than SQLite, because the instances share the job state.
- `actors.runtimePSK` has to be set, since the instances derive their mutual-TLS CA from it. A random one is generated at startup when it is empty, which cannot work across instances. Generate it with `openssl rand -base64 32`.
- `actors.address` has to be an address the other instances can reach, with its UDP port published, because that is where they send each other work.
- `storage.originals` and `storage.variants` have to point at a store every instance reaches, such as S3, rather than at the local volume.

The mounted configuration is already a Secret because it contains the signing secrets. For separately rotated S3 or Vercel Blob credentials, mount another Secret as a file or pass it as an environment variable the storage DSN names with `tokenEnv`.

If the service sits behind a reverse proxy, set `server.trustedProxies` to the proxy's address, so the request log and the traces record each client's address rather than the proxy's.

### Publishing on another port

`PublishPort` in the unit maps the host port, and `server.port` in the Secret sets the port the service listens on inside the pod. Changing the port the service listens on means changing both, plus the port in the two probes.

The actor host also listens on `actors.address`, which defaults to `127.0.0.1:7571`. It is only used by other actor hosts, so a single-instance pod never receives a connection on it, and it is deliberately not published.
