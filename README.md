# ButterStack Connector

An outbound-only daemon a game studio runs inside its own network so ButterStack can reach a private, on-premises Perforce, TeamCity, Jenkins, GitHub Enterprise Server, or Horde without the studio opening a single inbound port. One outbound TLS connection to one hostname on 443. A typed command allowlist, never a tunnel and never a shell. Credentials stay on the studio's disk and never cross the wire.

Status: pre-release. Tracking: [ButterStack/butter_stack#1575](https://github.com/ButterStack/butter_stack/issues/1575).

## What it is

The connector is a daemon the studio runs on its own hardware (or in a container on its own network). It opens exactly one outbound TLS connection to `wss://connect.butterstack.com/connect` on port 443, announces what it can do, and then executes only commands from a typed, versioned allowlist with constrained arguments, each one logged locally. Every credential the studio configures (Perforce tickets, TeamCity tokens) stays in the studio's config file and never crosses the wire. There is no inbound port, no tunnel, no shell, and no remote configuration: the broker cannot tell the connector where to find a credential.

## Requirements

- Go 1.25 or later (for building from source)
- Ruby 3.2+ (only for running the drill harness in `test/`)
- A ButterStack account with a connector token issued from the project's Connectors UI

## Install

**From source:**

```bash
go build -o butterstack-connector ./cmd/butterstack-connector
```

**Docker image (from the in-tree Dockerfile):**

```bash
docker build -t butterstack-connector .
```

The Dockerfile produces a minimal image with a static Go binary and a Ruby runtime for the UAT entrypoint. In production, the entrypoint runs the connector directly from `/usr/local/bin/butterstack-connector`.

## Configure

Copy `connector.example.yml` to your install location (e.g. `/etc/butterstack/connector.yml`) and set its permissions to 0600. The daemon refuses to start if the config file or any `*_file` path is readable by group or other users.

```bash
install -m 0600 connector.example.yml /etc/butterstack/connector.yml
```

Every credential comes from this file, or from a `*_file` path it names. There is no environment-variable fallback, no flag that takes a secret, and no remote configuration.

### Fields

| Field | Type | Secret | Source | Description |
|---|---|---|---|---|
| `endpoint` | string | no | file | The broker URL. Must be `wss://` with no query string, no userinfo, no fragment. This is the one hostname your egress rule needs. |
| `endpoint_ca_file` | string | no | file | Optional. Pins the trust anchor for the endpoint, for a private CA or a TLS-inspecting proxy. There is no option to skip verification for the broker connection. |
| `token` | string | **yes** | file | The connector token issued in the ButterStack UI. Shown exactly once at issue time. Format: `bsc_<project-id>_<secret>`. |
| `token_file` | string | no | file | Alternative to `token`: path to a file containing the token (for vault-injected secrets). Mutually exclusive with `token`. The file must be mode 0600. |
| `connector_id` | string | no | file | A name for this host, shown in the Connection Status panel. |
| `log_dir` | string | no | file | Local audit log directory. One JSON line per command, including every denial. Defaults to a `logs/` directory next to the config file. |
| `max_concurrent` | int | no | file | Commands executed in parallel. Range: 1-32. Default: 4. |
| `scopes.depot_scope` | list | no | file | Literal Perforce depot prefixes (no wildcards). A path whose literal prefix is not inside one of these is denied. |
| `scopes.allowed_build_types` | list | no | file | For the reserved `teamcity.build.queue` verb. Not compiled in v0. |
| `scopes.repo_allowlist` | list | no | file | For the reserved `ghes.*` verbs. Not compiled in v0. |
| `toggles.content_verbs` | bool | no | file | Content-class verbs are off in v0 at the schema level. This switch cannot turn one on yet. |
| `perforce.enabled` | bool | no | file | Enable the Perforce tool. |
| `perforce.binary` | string | no | file | Path to the `p4` CLI. Default: `p4`. |
| `perforce.port` | string | no | file | Helix Core server address (e.g. `ssl:perforce.studio.lan:1666`). |
| `perforce.user` | string | no | file | A read-only Perforce user. |
| `perforce.ticket` | string | **yes** | file | The Perforce ticket. Passed to `p4` via the `P4PASSWD` environment variable so it does not appear in the process list. |
| `perforce.ticket_file` | string | no | file | Alternative to `perforce.ticket`. Must be mode 0600. |
| `perforce.timeout` | duration | no | file | Timeout for `p4` commands. Default: `20s`. |
| `teamcity.enabled` | bool | no | file | Enable the TeamCity tool. |
| `teamcity.url` | string | no | file | The TeamCity server URL on your LAN (e.g. `https://teamcity.studio.lan`). |
| `teamcity.token` | string | **yes** | file | A project-limited TeamCity access token with a read-only role. Never crosses the wire. |
| `teamcity.token_file` | string | no | file | Alternative to `teamcity.token`. Must be mode 0600. |
| `teamcity.ca_file` | string | no | file | Optional. Trust anchor for a self-signed certificate on your LAN TeamCity. Scoped to this server only, not the broker connection. |
| `teamcity.allow_insecure_tls` | bool | no | file | Skip TLS verification for the LAN TeamCity. There is no equivalent for the broker connection, which always verifies. |
| `teamcity.timeout` | duration | no | file | Timeout for TeamCity REST calls. Default: `10s`. |

Two rules the daemon enforces at startup rather than documents:

- `connector.yml` and every `*_file` must be mode 0600 or stricter, or it refuses to start.
- The endpoint must be `wss://` with no query string. A copy-pasted `?token=...` URL cannot start the daemon at all.

## Run

**Foreground:**

```bash
./butterstack-connector -config /etc/butterstack/connector.yml
```

**Print the compiled vocabulary:**

```bash
./butterstack-connector -print-vocabulary
```

**systemd unit (example):**

```ini
[Unit]
Description=ButterStack Connector
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=butterstack-connector
ExecStart=/usr/local/bin/butterstack-connector -config /etc/butterstack/connector.yml
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

**Docker Compose (example):**

```yaml
services:
  connector:
    image: butterstack-connector
    build: .
    entrypoint:
      - /usr/local/bin/butterstack-connector
      - -config
      - /etc/butterstack/connector.yml
    volumes:
      - ./connector.yml:/etc/butterstack/connector.yml:ro
    restart: unless-stopped
```

## Verify

The drill harness (`test/drills.rb`) runs seven drills from design note section 4.3 against the mock broker (`test/mock_broker.rb`), plus a round-trip phase and the broker-side half of drill (f). Requires Ruby 3.2+ and a built binary.

```bash
make check        # go vet + go test + drills
make test         # go vet + go test only
make drills       # drills only (needs a built binary)
make build        # build the binary
make vocabulary   # print the compiled allowlist
```

`make build` uses `docker run golang:1.23-alpine` by default. If you have Go on the host, `GO=go make build` uses it directly.

See `test/README.md` for details on the individual drills and the mock broker.

## Supported backends

| Backend | Status | Compiled verbs |
|---|---|---|
| **Perforce** (Helix Core) | Implemented | `p4.describe`, `p4.changes` |
| **TeamCity** | Implemented | `teamcity.server.info`, `teamcity.build.get` |
| **Jenkins** | Planned (reserved) | `jenkins.build.trigger` (denied in v0) |
| **GitHub Enterprise Server** | Planned (reserved) | `ghes.commit.get` (denied in v0) |
| **Horde** | Planned (reserved) | `horde.server.info` (denied in v0) |

System verbs (`sys.ping`, `sys.version`, `sys.capabilities`) are always compiled and touch no studio tool.

Reserved verbs are listed in the vocabulary so that the schema is self-documenting and the drills exercise their denial path, but they cannot be executed in this build. `p4.file_contents` is reserved as a content-class verb and is off at both the schema level and the config level. `teamcity.build.queue` is reserved as a mutation-class verb. No mutating verb and no content-class verb is compiled in v0.

See `internal/vocab/vocab.go` for the full allowlist.

## Protocol

See [PROTOCOL.md](PROTOCOL.md) for the wire-level protocol details: transport, authentication, frame format, vocabulary resolution, and the audit log.
