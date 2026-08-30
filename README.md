# butterstack-connector (spike)

An outbound-only daemon a studio runs inside its own network so ButterStack can
reach a private, on-premises Perforce or TeamCity **without the studio opening a
single inbound port**.

It opens exactly one outbound TLS connection to one hostname on 443, announces
what it can do, and then executes only commands from a typed, versioned
allowlist with constrained arguments — each one logged locally. It holds the
studio's tool credentials in its own config file and never sends them.

This is the **spike** from issue #1575, checkbox groups 0 and 1: a standalone
proof of the daemon and the protocol schema. Nothing here is deployed, and
nothing here touches the Rails app.

- [`PROTOCOL.md`](PROTOCOL.md) — the day-1 protocol schema (issue #1575 group 0)
- [`internal/vocab/vocab.go`](internal/vocab/vocab.go) — the whole allowlist, in
  one readable file, on purpose
- [`test/`](test/) — the mock broker and the seven drills

Design sources, on branch `plan/teamcity-private-reach`:
`ai/team/agents/devin/runbooks/2026-08-29-private-instance-reach-connector-design.md`
§2.2–2.6, §4.3, §5, and
`ai/team/agents/shuri/reports/2026-08-29-connector-design-security-review.md` §6.

---

## Build and run

Go 1.23. No host Go install is needed; the Makefile builds in a container.

```bash
make build        # go build -> build/butterstack-connector
make test         # go vet + go test ./...
make drills       # the seven drills against the mock broker (needs Ruby 3.2+)
make check        # test + drills
make vocabulary   # print the compiled allowlist
```

`make build` uses `docker run golang:1.23-alpine`. If you have Go on the host,
`GO=go make build` uses it instead.

```bash
./build/butterstack-connector -config /etc/butterstack/connector.yml
./build/butterstack-connector -print-vocabulary
```

## Configuration

See [`connector.example.yml`](connector.example.yml). Two rules the daemon
enforces rather than documents:

- **`connector.yml` and every `*_file` must be mode 0600 or stricter**, or it
  refuses to start.
- **The endpoint must be `wss://` with no query string.** A copy-pasted
  `?token=...` URL cannot start the daemon at all.

Every credential comes from that file, or from a `*_file` path it names. There
is no environment-variable fallback, no flag that takes a secret, and no remote
configuration — the broker cannot tell the connector where to find a credential.

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

No verb accepts a host, port, URL, or shell string. No verb accepts
caller-supplied build parameters or properties — that is enforced structurally
(`bannedArgNames` plus `Selfcheck()`, which runs at process start as well as in
the tests), because a parameter map on a build-triggering verb interpolates into
shell build steps and would make the allowlist a code-execution primitive inside
the studio's LAN. No mutating verb and no content-class verb is compiled in.

---

## The drills

`make drills` runs the seven drills from design note §4.3 against
`test/mock_broker.rb`, plus a round-trip phase and the broker-side half of
drill (f). Every drill passes today:

| | Drill | What it asserts |
|---|---|---|
| P0 | verbs round-trip | all five compiled verbs answer `ok`; results carry only the declared fields; the TeamCity token used on the LAN is the one from `connector.yml` |
| D1 | out-of-vocabulary verb | `sys.exec`, `p4.print`, … → `denied / unknown_verb`; reserved names → `denied / verb_not_compiled`; each with a local audit line |
| D2 | out-of-scope argument | `//...`, `//depot/...`, a smuggled `params`/`properties` map, a quoted integer, `include_diff:true` → all `denied`, none reaching the tool; an in-scope path carrying shell metacharacters reaches `p4` as one literal argv element and no shell runs |
| D3 | query-string token | the daemon refuses such an endpoint; the broker answers HTTP 400 and never a 101; no session state is allocated; missing and wrong bearer tokens are 401 |
| F* | cross-session result | a `result` whose command id the session never issued is discarded, not dispatched by id alone |
| R1 | network drop | the session dies and the connector reconnects with no operator action |
| R2 | connector stopped | we flip to offline; a verb-dependent feature renders "needs connector" instead of raising; the daemon logs a clean shutdown |
| R3 | our side stopped | the connector backs off, logs it, and reconnects when the broker returns |
| R4 | token revoked | the socket closes within one heartbeat; reconnects are refused; `connector.yml` is byte-identical and still 0600; and **no tool credential, LAN host, port, or URL ever appeared in a frame** |

The mock broker is not the broker. The real one is a dedicated Rack endpoint at
`/connect` on the Rails app with hashed-token auth, Redis-routed command and
result, and explicit tenant scoping — a later PR. What `test/mock_broker.rb`
models is the surface these drills need, and it does implement faithfully the
four rules they exist to prove: refusal before session-state allocation,
header-only tokens, SHA-256 digest storage with constant-time compare, and
per-session result matching.

---

## What this spike does **not** prove

Carried forward from design note §5 and Shuri §6 item 7, plus what this
standalone shape adds:

- **The argument-constraint layer end to end.** The drills prove denial at the
  frame boundary against a mock broker. They do not prove it against a real
  broker, a real TeamCity, or a real p4d.
- **Anything on the Rails side.** There is no `/connect` endpoint in this PR, no
  ActionCable change, no migration, no UI. The tenant-context drill — "assert
  tenant context is nil at the start of a request that follows a connector frame
  on the same Puma thread" — is Rails-side and is **not** covered here. Only the
  broker-side half of drill (f) is.
- **Anything on real infrastructure.** Nothing ran against staging, demo, or
  production. No terraform, no security group, no hostname, no certificate.
  Stage A (the Tier 1 TeamCity webhook run) has not been run, and it is gated on
  the #1574 Phase -1 app fixes landing first; the "no token in
  `webhook_events.payload` or the app log" drill therefore has no result yet.
- **The frame codec against an independent production stack.** Both ends here
  were written from RFC 6455 — the Go client and the Ruby server independently,
  which is why a masking or handshake mistake shows up as a failed drill. But
  neither has met a real ALB, a real nginx `Upgrade` hop, or a real proxy.
- **Latency over a home connection.** The drills run on loopback. The design's
  under-2-second target is untested against a NATed home network, and the
  `ss`/`netstat` capture showing exactly one outbound established connection and
  zero listeners has not been taken.
- **Survival conditions 1, 4, and 5.** No Sigstore keyless signing, no SBOM, no
  build-from-source instructions, no digest-pinned base image, no version-skew
  handling, and no `egress.md` with a per-verb output schema enforced as a field
  allowlist with a conformance test. The fixed `fields=` projections in the
  TeamCity executor are the beginning of that, not the whole of it.
- **Scale and multi-node routing.** Puma behaviour at tens of connectors, socket
  routing under a real ASG scale-out, and the per-integration connection cap and
  per-session command budget in the broker.
- **Everything beyond the five compiled verbs.** No Jenkins, GHES, or Horde
  verb; no Perforce verb beyond `describe` and `changes`; no mutating verb; no
  content verb; no poll-loop mode; no Windows service.
- **An actual IT-director review.** Appendix B is a script, not a test.

This is the go/no-go input for the build, and it is deliberately smaller than
the product.
