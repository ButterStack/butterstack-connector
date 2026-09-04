# AGENTS.md

Guidance for AI coding agents working in this repository. Humans: see README.md.

## What this is

The ButterStack Connector: an outbound-only Go daemon a game studio runs inside its own network so ButterStack can reach a private Perforce, TeamCity, Jenkins, GitHub Enterprise Server, or Horde. One outbound TLS connection, a typed command allowlist, no tunnel, no shell, credentials never leave the studio's disk. PROTOCOL.md is the wire contract and wins over any code comment.

## Layout

- `cmd/butterstack-connector/`: the binary's entry point. `main.Version` is stamped at build time.
- `internal/config`: config file parsing and validation (permissions, `*_file` secrets, scope rules).
- `internal/vocab`: the command vocabulary, one typed verb per entry with its argument constraints. Adding a verb means adding it here, in PROTOCOL.md, and in the drills.
- `internal/tools`: per-backend executors (Perforce, TeamCity).
- `internal/wsclient`: the single outbound WebSocket connection and liveness.
- `test/`: the drill harness (`drills.rb`), the mock broker, and backend stubs.

## Commands

- `make build`: builds `build/butterstack-connector` with the version stamped.
- `make test`: `go vet` and `go test ./...`.
- `make drills`: runs the seven recovery and denial drills against the in-process mock broker.
- `make check`: test then drills. CI runs vet, test, and build on every push; a `v*` tag runs the release workflow.

## Rules

- Never widen the allowlist implicitly. Every verb has fixed argument names and patterns; `bannedArgNames` and `depot_scope` checks are security boundaries, not conveniences.
- No secret may be accepted from a flag, an environment variable, or the wire. Only the config file and the `*_file` paths it names.
- Do not add inbound listeners, remote configuration, or shell execution of any kind.
- Keep `connector.example.yml` in sync with `internal/config`, and PROTOCOL.md in sync with `internal/vocab`.
- Plain prose in docs: no em dashes, one paragraph per line.
- Commit messages and PR bodies are written to a file and passed with `-F` / `--body-file`, never inline.
