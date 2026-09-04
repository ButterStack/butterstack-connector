# Multi-stage build with TWO publishable targets. Build with --target.
#
#   runtime  (default, and what `v*` tags publish)  distroless + the binary
#   uat      (UAT/drill environment only)           adds Ruby for fake_p4
#
# `runtime` is last on purpose, so a bare `docker build` with no --target
# produces the safe artifact rather than the drill one.
#
# Why the split (issue #5): the daemon is a static CGO_ENABLED=0 Go binary
# with no runtime dependencies, but the final stage used to be Ruby-based
# unconditionally, which shipped a 33.9 MiB interpreter layer to every studio
# that pulled a release. Worse, that stage's own header declared it "for the
# UAT drill environment only" and "NOT hardened for production" -- and it was
# exactly what v0.1.0 published and what studios ran.
#
# The Ruby is genuinely needed, just not here: the UAT "studio LAN" stands the
# `p4` CLI up with test/support/fake_p4, a Ruby script bind-mounted at
# /usr/local/bin/p4 by docker-compose.uat-connector.yml, and the connector
# execs whatever binary connector.yml names, argv-only, no shell. So `uat`
# keeps Ruby and `runtime` does not. The seven-drill harness is unaffected
# either way: `make drills` runs `ruby test/drills.rb` on the HOST
# (RUBY ?= ruby in the Makefile), never inside an image.
#
# Still unmet for production, tracked separately (issue #1575 survival
# conditions 1/4/5): no Sigstore keyless signing, no SBOM, no digest-pinned
# base images, no reproducible-build docs.

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# -trimpath drops build-machine file paths from the binary; -s -w strips the
# symbol table and DWARF debug info. CGO_ENABLED=0 keeps the binary static, so
# the runtime stage needs no libc compatibility shim.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/butterstack-connector ./cmd/butterstack-connector

# --- uat -------------------------------------------------------------------
# Drill/UAT image. Ruby is load-bearing here and only here: fake_p4 is a Ruby
# script the UAT compose file bind-mounts over /usr/local/bin/p4.
FROM ruby:3.3-alpine AS uat

RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 connector \
 && adduser -D -u 10001 -G connector -h /home/connector -s /sbin/nologin connector \
 && mkdir -p /etc/butterstack /var/log/connector /tls \
 && chown -R connector:connector /etc/butterstack /var/log/connector /tls /home/connector

COPY --from=build /out/butterstack-connector /usr/local/bin/butterstack-connector
COPY test/uat/entrypoint.sh /usr/local/bin/uat-entrypoint.sh
RUN chmod 0755 /usr/local/bin/butterstack-connector /usr/local/bin/uat-entrypoint.sh

# No EXPOSE: the connector never listens on anything. It opens exactly one
# outbound TLS connection and never accepts an inbound one.

USER connector:connector
WORKDIR /home/connector

# Production entrypoint: reads connector.yml from the path a studio wrote.
# The UAT compose service overrides this with uat-entrypoint.sh, which
# renders that file from UAT_CONNECTOR_* env vars first (see that script's
# header for why that is a UAT-only pattern, not a protocol exception).
ENTRYPOINT ["/usr/local/bin/butterstack-connector", "-config", "/etc/butterstack/connector.yml"]

# --- skel ------------------------------------------------------------------
# Builds the unprivileged account and the directory skeleton for the runtime
# stage, which has no shell of its own to run adduser/mkdir in.
FROM alpine:3.22 AS skel
RUN addgroup -g 10001 connector \
 && adduser -D -u 10001 -G connector -h /home/connector -s /sbin/nologin connector \
 && mkdir -p /skel/etc/butterstack /skel/var/log/connector /skel/tls /skel/home/connector \
 && chown -R 10001:10001 /skel/etc/butterstack /skel/var/log/connector /skel/tls /skel/home/connector

# --- runtime ---------------------------------------------------------------
# Default target and the one `v*` tags publish.
#
# glibc, not musl, and that is forced rather than preferred. The connector
# execs the binary named by connector.yml's `perforce.binary` (default "p4",
# internal/tools/perforce.go's exec.CommandContext), and a studio supplies
# that binary itself -- on bsg-cp-01 by bind-mounting the p4 client out of the
# Perforce container so the client version tracks the server automatically.
# Perforce's packaged p4 is DYNAMICALLY linked against glibc (ldd: libc.so.6,
# librt, libdl, libm, libpthread, /lib64/ld-linux-x86-64.so.2), so an
# alpine/musl runtime rejects it outright:
#
#   alpine:3.22       -> "Dynamic loader not found: /lib64/ld-linux-x86-64.so.2"
#   distroless/base   -> `p4 -V` runs
#
# Both measured against the real binary from ButterStack/perforce-debian's
# helix-p4d package, not assumed.
#
# distroless/base rather than debian-slim because it supplies glibc and CA
# certificates and nothing else: no shell, no package manager, no interpreter.
# That matters more than usual here, since this image deliberately executes an
# operator-supplied binary.
#
# Pinned by digest, which is one of the four production survival conditions
# (#1575) the header above lists as unmet. The digest is a manifest INDEX, so
# multi-arch still resolves; bump it deliberately, not by tag drift.
FROM gcr.io/distroless/base-debian12:nonroot@sha256:7f0c72cd138b442ae0deeb69c08b1acf5525439ba251a49ad93c320a061567e5 AS runtime

# distroless has no shell, so the account and the directories are built in a
# stage that does, then copied in. COPY --from preserves numeric ownership.
# uid/gid stay 10001 to match the uat stage exactly, so moving a deployment
# between the two targets changes only the digest.
COPY --from=skel /etc/passwd /etc/group /etc/
COPY --from=skel /skel/ /

COPY --from=build /out/butterstack-connector /usr/local/bin/butterstack-connector

# No EXPOSE: the connector never listens on anything. It opens exactly one
# outbound TLS connection and never accepts an inbound one.

# Numeric on purpose: independent of /etc/passwd lookup, which a derived image
# could overwrite.
USER 10001:10001
WORKDIR /home/connector

ENTRYPOINT ["/usr/local/bin/butterstack-connector", "-config", "/etc/butterstack/connector.yml"]
