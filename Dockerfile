# syntax=docker/dockerfile:1

# Thumbnails are generated with libvips through cgo, so the binary is dynamically linked and needs the libvips shared libraries at runtime
# The runtime image is still distroless: the libraries are copied in from a Debian stage and the final image carries them without a shell or a package manager
# It has to be the "base" variant rather than "static" because the binary needs glibc, and that glibc has to match the one the libraries were built against
# That is why all three stages are pinned to the same Debian release: golang:*-trixie and debian:trixie-slim are Debian 13, as is distroless base-debian13

FROM golang:1.27-trixie AS builder

# Set by the build workflow to stamp version information into the binary
ARG BUILD_LDFLAGS=""

RUN apt-get update && \
    apt-get install -y --no-install-recommends libvips-dev && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# The module files are copied on their own so the dependency download stays cached until they change
COPY go.mod go.sum ./
RUN go mod download

# .dockerignore is an allowlist, so this copies the Go source trees and nothing else
COPY . .

ENV CGO_ENABLED=1
RUN go build -ldflags "${BUILD_LDFLAGS}" -trimpath -o /out/provino ./cmd/provino

# This stage collects libvips and everything it needs into /runtime, which is the tree the final image is built from
FROM debian:trixie-slim AS runtime-deps

# libvips42t64 is the libvips runtime, and the libheif AV1 encoder plugin is what makes AVIF output work
# curl is here for the HEALTHCHECK below, since a distroless image has no shell for Docker or Podman to run one in
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      libvips42t64 \
      libheif-plugin-aomenc \
      curl && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/provino /out/provino

RUN <<'EOF'
set -eux

# libvips dlopens its format handlers from vips-modules-*, and libheif dlopens its codecs from libheif/plugins
# Neither is reachable through ldd, so they are listed explicitly and their own dependencies are resolved alongside the binary's
dlopened="$(ls /usr/lib/*/vips-modules-*/*.so /usr/lib/*/libheif/plugins/*.so 2>/dev/null || true)"
test -n "${dlopened}"

# curl is resolved the same way as the service binary, since it brings a TLS stack and a name-resolution stack of its own
executables="/out/provino /usr/bin/curl"

# ldd flattens the whole dependency tree, so one pass over the executables and the dlopened objects is the full closure
{
    for exe in ${executables}; do
        ldd "${exe}"
    done
    for so in ${dlopened}; do
        ldd "${so}"
    done
} | awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^\//) { print $i; break } }' | sort -u > /tmp/closure

# --parents keeps each file at the path it was resolved from, which is what makes this work on any architecture
# The service binary is left out because the final stage copies it from the build stage, to its own path
mkdir -p /runtime
for file in $(cat /tmp/closure) ${dlopened} /usr/bin/curl; do
    cp -L --parents "${file}" /runtime/
done

# The dynamic loader is the one file left to the base image, since every binary in it is launched through that copy
rm -f /runtime/lib64/ld-linux-*.so.* /runtime/lib/ld-linux-*.so.*

# Both images have a merged /usr, where /lib and /lib64 are symlinks into it, but `cp --parents` recreates the path it was given, so most of the tree above landed in a real /runtime/lib
# The final stage copies this tree onto the base image, and BuildKit fails that copy rather than following a symlink into a directory, so the same merge is applied here
# The loader looks in /usr/lib/<triplet> as well as /lib/<triplet>, so nothing has to be linked back
for dir in lib lib64; do
    test -d "/runtime/${dir}" || continue
    mkdir -p "/runtime/usr/${dir}"
    cp -a "/runtime/${dir}/." "/runtime/usr/${dir}/"
    rm -rf "/runtime/${dir}"
done

# The working directory the default file-system storage writes into, owned by the user the image runs as
install -d -o 65532 -g 65532 /runtime/var/lib/provino
EOF

FROM gcr.io/distroless/base-debian13:nonroot

# Ownership is preserved from the stage above
COPY --from=runtime-deps /runtime /
COPY --from=builder /out/provino /bin/provino

USER nonroot
WORKDIR /var/lib/provino

# The port is the one `server.port` defaults to: an image that runs on another port needs the check overridden with it, which `docker run --health-cmd` and Podman's `--health-cmd` both take
# curl's own timeout is shorter than the check's, so a hung request is reported as a failed check rather than killed as a timed-out one
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD [ "/usr/bin/curl", "--fail", "--silent", "--show-error", "--max-time", "3", "http://127.0.0.1:3000/healthz" ]

ENTRYPOINT [ "/bin/provino" ]
