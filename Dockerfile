# Multi-stage build with TWO publishable targets. Build with --target.
#
#   runtime  (default, and what `v*` tags publish)  alpine + the Go binary
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

# --- runtime ---------------------------------------------------------------
# Default target and the one `v*` tags publish. alpine rather than scratch or
# distroless on purpose: the connector execs the binary named by
# connector.yml's `perforce.binary` (default "p4",
# internal/tools/perforce.go's exec.CommandContext), so a studio has to be
# able to supply a real p4 client, by bind-mount or by adding it to a derived
# image. scratch forecloses both. Nothing is installed here beyond CA
# certificates, and the image carries no interpreter.
FROM alpine:3.22 AS runtime

RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 connector \
 && adduser -D -u 10001 -G connector -h /home/connector -s /sbin/nologin connector \
 && mkdir -p /etc/butterstack /var/log/connector /tls \
 && chown -R connector:connector /etc/butterstack /var/log/connector /tls /home/connector

COPY --from=build /out/butterstack-connector /usr/local/bin/butterstack-connector
RUN chmod 0755 /usr/local/bin/butterstack-connector

# No EXPOSE: the connector never listens on anything. It opens exactly one
# outbound TLS connection and never accepts an inbound one.

USER connector:connector
WORKDIR /home/connector

# Identical to the uat stage's entrypoint, config path and uid/gid (10001),
# so an existing deployment moves to this image by changing only the digest.
ENTRYPOINT ["/usr/local/bin/butterstack-connector", "-config", "/etc/butterstack/connector.yml"]
