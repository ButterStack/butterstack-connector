# Design notes

This file preserves the design rationale and review history that was originally in the README, from when the connector lived inside the ButterStack monorepo and before it was extracted into this repository.

## Design sources

Origin: an internal ButterStack planning branch (2026-08-29):
the ButterStack connector design note (2026-08-29, internal) sections 2.2-2.6, 4.3, 5, and
the connector security review (2026-08-29, internal) section 6.

## What is in the vocabulary

| Verb | v0 |
|---|---|
| `sys.ping`, `sys.version`, `sys.capabilities` | compiled |
| `teamcity.server.info` | compiled |
| `teamcity.build.get {build_id}` | compiled |
| `p4.describe {change, max_files, include_diff:false}` | compiled |
| `p4.changes {path, max}` | compiled |
| `teamcity.build.queue`, `jenkins.build.trigger` | reserved, denied |
| `p4.file_contents`, `ghes.commit.get`, `horde.server.info` | reserved, denied |

No verb accepts a host, port, URL, or shell string. No verb accepts caller-supplied build parameters or properties, that is enforced structurally (`bannedArgNames` plus `Selfcheck()`, which runs at process start as well as in the tests), because a parameter map on a build-triggering verb interpolates into shell build steps and would make the allowlist a code-execution primitive inside the studio's LAN. No mutating verb and no content-class verb is compiled in.

## What is not yet proven

This list started as the spike's go/no-go input and is kept current as things are proven, so it is a running record of what is claimed and what is not.

**Proven since:** on 2026-09-08 a real Perforce changelist travelled from a studio's own Helix Core server, through a connector on the studio's box, over one outbound TLS connection to the production broker, and was recorded on a ButterStack project - which closed out, in one run, the argument-constraint layer against a real p4d, the frame codec against a real ALB, and the broker half existing at all. The published image is digest-pinnable and each release tag carries build provenance and an SBOM.

Still open:

- **The seven drills against the production broker.** They pass against `test/mock_broker.rb` on loopback. The production run has been exercised by real traffic rather than by the drill harness, so the denial paths in particular have no production-side result.
- **TeamCity in production.** The verbs are compiled and drilled, but the one live deployment runs with `teamcity.enabled: false`, because handing the daemon an admin-scoped TeamCity token would give any `teamcity.*` verb an admin session's blast radius. This turns on once a connector-scoped TeamCity credential exists.
- **Latency over a home connection.** The drills run on loopback. The design's under-2-second target is untested against a NATed home network.
- **Survival conditions 1 and 5.** No Sigstore keyless signing, and no `egress.md` with a per-verb output schema enforced as a field allowlist with a conformance test. The fixed `fields=` projections in the TeamCity executor are the beginning of that, not the whole of it.
- **Scale and multi-node routing.** Broker behaviour at tens of connectors, socket routing under a real scale-out, and the per-integration connection cap and per-session command budget.
- **Everything beyond the five compiled verbs.** No Jenkins, GHES, or Horde verb; no Perforce verb beyond `describe` and `changes`; no mutating verb; no content verb; no poll-loop mode; no Windows service.
- **An actual IT-director review.** Appendix B is a script, not a test.

The scope is deliberately smaller than the product, and that is the point: every verb that is not compiled in cannot be executed, whatever the broker asks for.
