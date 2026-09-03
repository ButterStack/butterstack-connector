# Design notes

This file preserves the design rationale and review history that was originally in the README when the connector lived inside the butter_stack monorepo as the issue #1575 spike.

## Design sources

Branch `plan/teamcity-private-reach` in ButterStack/butter_stack:
`ai/team/agents/devin/runbooks/2026-08-29-private-instance-reach-connector-design.md` sections 2.2-2.6, 4.3, 5, and
`ai/team/agents/shuri/reports/2026-08-29-connector-design-security-review.md` section 6.

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

## What the spike does not prove

Carried forward from design note section 5 and Shuri section 6 item 7, plus what the standalone shape adds:

- **The argument-constraint layer end to end.** The drills prove denial at the frame boundary against a mock broker. They do not prove it against a real broker, a real TeamCity, or a real p4d.
- **Anything on the Rails side.** There is no `/connect` endpoint, no ActionCable change, no migration, no UI. The tenant-context drill ("assert tenant context is nil at the start of a request that follows a connector frame on the same Puma thread") is Rails-side and is not covered here. Only the broker-side half of drill (f) is.
- **Anything on real infrastructure.** Nothing ran against staging, demo, or production. No terraform, no security group, no hostname, no certificate. Stage A (the Tier 1 TeamCity webhook run) has not been run, and it is gated on the #1574 Phase -1 app fixes landing first; the "no token in `webhook_events.payload` or the app log" drill therefore has no result yet.
- **The frame codec against an independent production stack.** Both ends here were written from RFC 6455, the Go client and the Ruby server independently, which is why a masking or handshake mistake shows up as a failed drill. But neither has met a real ALB, a real nginx `Upgrade` hop, or a real proxy.
- **Latency over a home connection.** The drills run on loopback. The design's under-2-second target is untested against a NATed home network, and the `ss`/`netstat` capture showing exactly one outbound established connection and zero listeners has not been taken.
- **Survival conditions 1, 4, and 5.** No Sigstore keyless signing, no SBOM, no build-from-source instructions, no digest-pinned base image, no version-skew handling, and no `egress.md` with a per-verb output schema enforced as a field allowlist with a conformance test. The fixed `fields=` projections in the TeamCity executor are the beginning of that, not the whole of it.
- **Scale and multi-node routing.** Puma behaviour at tens of connectors, socket routing under a real ASG scale-out, and the per-integration connection cap and per-session command budget in the broker.
- **Everything beyond the five compiled verbs.** No Jenkins, GHES, or Horde verb; no Perforce verb beyond `describe` and `changes`; no mutating verb; no content verb; no poll-loop mode; no Windows service.
- **An actual IT-director review.** Appendix B is a script, not a test.

This is the go/no-go input for the build, and it is deliberately smaller than the product.
