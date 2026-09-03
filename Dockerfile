# Multi-stage build for the butterstack-connector UAT/drill image
# (connector.spec.js, issue #1574/#1575).
#
# The daemon itself is a static Go binary with no runtime dependencies. The
# final stage is Ruby-based anyway because the UAT "studio LAN" stands the
# real `p4` CLI up with test/support/fake_p4 -- a Ruby script bind-mounted at
# /usr/local/bin/p4 by docker-compose.uat-connector.yml -- and the connector
# execs whatever binary connector.yml names, argv-only, no shell.
#
# NOT hardened for production (issue #1575 survival conditions 1/4/5: no
# Sigstore keyless signing, no SBOM, no digest-pinned base image, no
# reproducible-build docs -- see connector/README.md "what this does not
# prove"). This image exists for the UAT drill environment only.

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# -trimpath drops build-machine file paths from the binary; -s -w strips the
# symbol table and DWARF debug info. CGO_ENABLED=0 keeps the binary static, so
# the runtime stage needs no libc compatibility shim.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/butterstack-connector ./cmd/butterstack-connector

FROM ruby:3.3-alpine

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
