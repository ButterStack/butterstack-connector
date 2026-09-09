# Contributing

Thanks for taking a look at the ButterStack Connector.

## Project shape

A single static Go binary with no runtime dependencies beyond the tools it shells out to (`p4`). Ruby appears in exactly one place, the drill harness in `test/`, and it stays there: the shipped runtime image has no Ruby in it.

The load-bearing constraint of this codebase is that **the allowlist is the security boundary**. `internal/vocab/vocab.go` is the authoritative list of what a broker can ask the daemon to do, and `PROTOCOL.md` is its written form. Argument constraints are schema, so a change to one is a change to both.

Before you propose a new verb, read `PROTOCOL.md` section 4 and `docs/design-notes.md`. A verb that accepts a host, port, URL, shell string, or caller-supplied build parameters will not be accepted: `bannedArgNames` and `Selfcheck()` exist to make that structural rather than a matter of review attention, and `Selfcheck()` runs at process start as well as in the tests.

## Getting set up

```
git clone https://github.com/ButterStack/butterstack-connector.git
cd butterstack-connector
make build
```

Requires Go 1.23 or later (the `go` directive in `go.mod`); release builds use 1.25. Ruby 3.2+ is needed only to run the drills.

## Running the tests

```
make test      # gofmt, go vet, go test
make drills    # the seven drills against test/mock_broker.rb
make check     # both of the above, and what CI runs
```

The drills run entirely on loopback in a temporary directory: a throwaway CA and server certificate so the connector dials a real `wss://` endpoint with real certificate verification, a stub TeamCity that refuses any request not carrying the token from `connector.yml`, and a fake `p4` that records its own argv. They cover the round trip plus out-of-vocabulary verbs, out-of-scope arguments, a query-string token, cross-session results, and four recovery conditions (network drop, connector stopped, broker stopped, token revoked).

If you touch the frame codec, the vocabulary, or the argument-constraint layer, run `make drills` and expect to add one. Several drills exist specifically to prove a denial path, and a denial path with no drill is an untested security claim.

## Reporting issues

Please use the issue templates. For anything that looks like a security issue, do not open a public issue: see [SECURITY.md](./SECURITY.md).

## Pull requests

- Keep changes focused and explain the "why," not just the "what."
- Add or update drills for behavior changes, and update `PROTOCOL.md` in the same PR if you changed anything a broker can observe.
- Run `make check` before opening a PR.
- Match the surrounding code style; there is no external formatter dependency beyond `gofmt`.

## Releases

Releases are tag-driven. Pushing a `v*` tag builds the per-OS/arch archives, publishes the container image to `ghcr.io/butterstack/butterstack-connector`, and attaches provenance and an SBOM. Merging to `main` publishes nothing on its own.

## Code of conduct

Be respectful and constructive. This is a small project maintained alongside a larger product; response times may vary.
