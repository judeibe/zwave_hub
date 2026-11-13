# zwave-hub image. Three stages feed one runtime:
#
#   build    golang:1.27   compiles the Go supervisor binary
#   sidecar  node:26-slim  installs the pinned @zwave-js/server production tree
#   (final)  node:26-slim  runtime: the binary plus the Node child it supervises
#
# The Go toolchain never ships, and Node ships only to run the sidecar. This
# matches the deployment the plan fixed: CGO_ENABLED=0 pure-Go SQLite for the
# binary (plan.md, Technical Context), Node >= 20 for @zwave-js/server 3.12.0,
# and quickstart.md §2's defaults.

# --- sidecar stage ----------------------------------------------------------
# npm runs in its own Node stage, not inside `build`: the golang image carries
# no npm, and the sidecar's install scripts should run under the same Node
# major that will execute it at runtime.
FROM node:26-slim AS sidecar

WORKDIR /src/sidecar

# Only the two package files are copied — never the working tree's
# node_modules — so the lockfile is the single source of truth for what ships.
COPY sidecar/package.json sidecar/package-lock.json ./

# --omit=dev: the container runs exactly one script, the @zwave-js/server
# entry point (internal/driver/supervisor.go command()); dev tooling would be
# dead weight and attack surface in the shipped image. The npm cache and its
# tmp files are removed in the same RUN, not a later one: each layer is a
# filesystem snapshot, so a `rm` in its own layer would only hide the files
# and leave them in the layer below.
RUN npm ci --omit=dev \
 && rm -rf /root/.npm /tmp/npm-* \
 && mkdir -p /out/sidecar \
 && cp -a node_modules package.json /out/sidecar/

# --- build stage ------------------------------------------------------------
FROM golang:1.27 AS build

# Never fetch a different toolchain mid-build: go.mod requests Go 1.27 and the
# image provides it, so a surprise download would only make the build depend
# on golang.org reachability for no benefit.
ENV GOTOOLCHAIN=local

WORKDIR /src

# Module files first, so `go mod download` is its own layer: a source-only
# edit reuses this layer instead of re-downloading every module.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# The exact compile the Makefile's `build` target runs — the image must ship
# that binary, not a differently-configured one.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/zwave-hub ./cmd/zwave-hub

# --- runtime stage ----------------------------------------------------------
FROM node:26-slim

# Debian's `dialout` group owns Linux serial device nodes. Its GID here is 20,
# matching quickstart.md §2's DIALOUT_GID default, so `group_add: ["20"]`
# names a group that really exists inside the image; the guard keeps that true
# even if a future base image dropped the group.
RUN getent group dialout >/dev/null || groupadd --system --gid 20 dialout

# Dedicated unprivileged account for the hub's whole life. It needs no more:
# HTTP, SQLite in /data, and — with the runtime flags recorded below — USB
# character devices.
RUN groupadd --system --gid 10001 zwave \
 && useradd --system --uid 10001 --gid zwave \
            --no-create-home --home-dir /nonexistent \
            --shell /usr/sbin/nologin zwave

# Both directories are created now, owned by zwave, so an operator's first-use
# named volume is seeded with that ownership and a plain `docker run` without
# the tmpfs mount still boots:
#   /data           hub.db, master.key (0600) and zwave-cache live here
#   /run/zwave-hub  the rendered driver config — internal/driver/config.go
#                   WriteConfig creates it 0700 and writes 0600 files, and it
#                   must be writable by this user (or private) because it
#                   briefly holds every network key in plaintext
# deploy/compose.yaml mounts a tmpfs over /run/zwave-hub; this 0700 image
# directory is the fallback that keeps an unmounted start working.
RUN install -d -o zwave -g zwave -m 0700 /data /run/zwave-hub

# Exactly two artifacts from the build stages. The binary stays root-owned
# (mode 0755): user zwave must be able to run it, never to replace it.
COPY --from=build /out/zwave-hub /usr/local/bin/zwave-hub
COPY --from=sidecar --chown=zwave:zwave /out/sidecar/ /opt/zwave-sidecar/

# Defaults copied from internal/config/config.go lines 33–40, overridable per
# container. Deliberately absent: ZWAVE_HUB_DRIVER_MODE (hardware is the
# default; mock is a development-only mode no image may bake in),
# ZWAVE_HUB_MASTER_KEY (key material belongs to the operator's environment,
# not the image — config.go keeps it out of argv for the same reason), and
# ZWAVEJS_LOG_SECURITY_DETAILS (the plan forbids it, and zwave-js only honours
# it when the child's explicit environment carries it — supervisor.go never
# forwards it).
ENV ZWAVE_HUB_BIND=0.0.0.0:8080 \
 ZWAVE_HUB_DATA_DIR=/data \
 ZWAVE_HUB_SIDECAR_DIR=/opt/zwave-sidecar \
 ZWAVE_HUB_LOG_LEVEL=info

# USB access is runtime configuration, not image content: device-cgroup rules
# are written by the container runtime (runc) when the container starts, and
# the host's dialout GID is unknowable at build time. The exact requirement,
# restated by the LABEL below:
#   --device-cgroup-rule='c 188:* rmw'   188 = ttyUSB* coordinators
#   --device-cgroup-rule='c 166:* rmw'   166 = ttyACM* coordinators
#   --group-add <host dialout gid>       the group owning the device node
# plus the device node itself via --device (compose: devices:,
# device_cgroup_rules: and group_add:). See quickstart.md §1.
LABEL org.zwave_hub.usb.device-cgroup-rules="c 188:* rmw c 166:* rmw" \
 org.zwave_hub.usb.group-add="host dialout GID (default 20, quickstart.md §1)"

USER zwave

# The unauthenticated API of FR-020 is this image's only network service.
EXPOSE 8080

# Everything the hub persists lives under /data (internal/store/store.go);
# the image itself adds nothing there, so it is the one volume.
VOLUME ["/data"]

# The hub is PID 1 and handles SIGTERM itself (cmd/zwave-hub/main.go): it
# drains HTTP, stops the sidecar child, then exits. SIGTERM is the default
# stop signal, but this makes the contract of `docker stop` explicit.
STOPSIGNAL SIGTERM

# /readyz answers 200 with no coordinator attached (SC-008), so it is the one
# health check. node already ships in the image — curl would be a package
# added for a single request. Unhealthy means "could not confirm a 200": a
# non-200 answer exits 1, a refused or hung fetch fails either through node's
# default unhandled-rejection exit or through this HEALTHCHECK's own timeout.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
 CMD ["node", "-e", "fetch('http://127.0.0.1:8080/readyz').then(r => process.exit(r.ok ? 0 : 1))"]

# No CMD: the binary takes only its validated flags, and the defaults a
# container needs are already in the ENV block above.
ENTRYPOINT ["/usr/local/bin/zwave-hub"]
