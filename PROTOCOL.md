# ButterStack Connector protocol, v0

Status: **day-1 spike schema** for issue #1575 group 1. This document is the
written-down form of checkbox group 0 (the day-1 protocol schema), which had to
land before any verb did, because argument constraints *are* schema.

Sources: Devin's design note §2.2 and §2.4
(`ai/team/agents/devin/runbooks/2026-08-29-private-instance-reach-connector-design.md`,
branch `plan/teamcity-private-reach`) and Shuri's must-fix list §6
(`ai/team/agents/shuri/reports/2026-08-29-connector-design-security-review.md`).

Two things this document does **not** do. It does not describe an endpoint that
exists: the broker side is a later PR, and the only implementation of this
protocol's server half today is the drill harness in `test/mock_broker.rb`. And
it does not restate the six survival conditions; it records the parts of them
that are schema.

---

## 1. Transport

One outbound TLS connection, from the studio's network to one ButterStack
hostname, on 443. The connector never listens on anything.

```
wss://<connect-host>/connect
```

| Rule | Why |
|---|---|
| `wss://` only. There is no plaintext option and no "skip verification" option for the broker connection. | `allow_insecure_tls` exists for a studio's self-signed *LAN* TeamCity, and is scoped to that host. The connection that carries commands is always verified. |
| The endpoint must carry **no query string**, no userinfo, and no fragment. | See §2. Enforced at config load, before a socket is opened. |
| TLS 1.2 minimum. | Matches the production ALB policy (`ELBSecurityPolicy-TLS13-1-2-2021-06`) and the instance nginx (`TLSv1.2 TLSv1.3`). |
| Heartbeat every 25 s by default; the broker may negotiate 5–120 s in `welcome`. | Must stay under the shortest idle timeout on the path: ALB `idle_timeout = 300`, instance nginx `proxy_read_timeout 300`, local docker nginx 60 s. |
| Reconnect: exponential backoff with full jitter, floor 500 ms, cap 60 s, forever. The backoff resets after a session that actually came up. | A connector that cannot connect is a log line on the studio side, never an alert. |

### The broker endpoint (stated here so the later PR inherits it)

Shuri §6 item 4, verbatim in effect: the broker is a **dedicated Rack endpoint
at `/connect`. Not ActionCable. Not `/cable`. Not `ApplicationCable::Connection`.
No change to `allowed_request_origins`. No `disable_request_forgery_protection`.**

The reasons are code-verified in her review and are worth keeping next to the
schema, because the tempting shortcut is real:

- `app/channels/application_cable/connection.rb:19-25` authenticates from the
  warden session cookie and rejects everything else, so a bearer-token machine
  client has no path through it.
- `config/environments/production.rb:56` restricts `allowed_request_origins` to
  the two `RAILS_HOST` origins. A non-browser client sends no `Origin`, so
  admitting one through ActionCable would mean widening that list or disabling
  forgery protection, either of which weakens a live cross-site
  WebSocket-hijacking control that protects real users' browser sockets.
- That connection carries `identified_by :current_user, :current_account`,
  `impersonates :user`, and sets `ActsAsTenant.current_tenant`. A machine client
  must inherit none of it.

`/connect` therefore has its own auth (§2), its own connection object, and no
session, cookie, or CSRF machinery in the path.

---

## 2. Authentication

### Token format

```
bsc_<project public id>_<32 characters, 58-symbol alphabet>
```

- The `bsc_` prefix makes the credential greppable by secret scanners, ours and
  the studio's.
- The id segment makes the broker's hashed lookup a primary-key read rather than
  a table scan.
- The secret segment is 32 characters drawn from a 58-symbol alphabet
  (`[A-Za-z2-7]`, i.e. A-Z + a-z + 2-7), not standard base32 - that's
  `32 * log2(58) ~ 187` bits of entropy, which is why plain SHA-256 is the
  correct storage: a slow KDF buys nothing against an input with that much
  entropy.

**Superseded (Ryan, 2026-08-30, #1575 group 2b):** the id segment was
originally `<integration public id>`. A Connector is now a first-class
per-project record rather than a per-integration one, so the id segment is
`<project public id>` instead - a compromise or misconfiguration of one
connector can only ever be scoped to its own project's account, never leak
across projects. This is a cross-project-isolation change to the id segment
only, not a wire-format redesign: the frame shapes in §3 are unaffected, and
`connector/internal/config`'s `tokenPattern` is id-segment-agnostic
(`\Absc_[a-z0-9][a-z0-9\-]{0,62}_[A-Za-z2-7]{32,128}\z`), so the daemon needs
no code change to accept a project-scoped token.

### Storage on our side

Store **only** `SHA-256(secret segment)`. Compare with
`ActiveSupport::SecurityUtils.secure_compare`. Display the token exactly once at
issue time; never make it retrievable afterwards, which is what makes the revoke
story in §6 honest.

This is a **new pattern for this codebase and the nearest precedent is the wrong
one**: `Integration#webhook_token` is generated with good entropy
(`app/models/integration.rb:575-577`) but is stored reversibly and looked up by
equality (`jenkins_controller.rb:46`). A connector token stored that way means
one database read yields every studio's live connector credential. Do not copy
it.

### The token travels in a header. Only in a header.

```
Authorization: Bearer bsc_...
```

**A token supplied as a query parameter on the `/connect` upgrade is rejected**,
and the rejection happens before any per-connection state is allocated. A
connector is not a browser: the only reason WebSocket clients put credentials in
query strings is a browser limitation that does not apply here, and a URL is
logged by every proxy on the path. This is the F1 lesson (issue #935, a live
webhook token in an nginx access log) applied to a new surface *before* it
exists rather than after.

Both halves are enforced and drilled:

- **Client half:** an endpoint carrying a query string is refused at config load
  (`internal/config`), so a copy-pasted `?token=` URL cannot start the daemon.
- **Broker half:** the upgrade is refused with an HTTP status, never a 101, and
  there is no code path that reads a token from a query string
  (`test/mock_broker.rb`).

### Refusal order

The broker refuses, in this order, **before allocating any per-connection
state**: wrong path, query string present, not an upgrade, missing
`Sec-WebSocket-Key`, missing bearer token, malformed token, unknown token,
revoked token. An unknown verb must likewise never reveal what is configured
(§4).

### Rate limiting

`/connect` upgrades need a **dedicated, stricter throttle** in addition to the
generic `req/ip` 300-per-5-minutes at
`config/initializers/rack_attack.rb:112-115`. That generic budget is shared with
a studio's webhook traffic arriving from the same NAT address, and rack-attack
sees only the upgrade, never the frames, so per-session command budgets and the
per-integration connection cap (v0: 2) belong in the broker, not in rack-attack.

---

## 3. Frames

All frames are single JSON objects with a `type` discriminator. There is no
binary frame type and no streaming.

### Connector → broker

```jsonc
// hello -- first frame after the upgrade. Every field here is egress.
{ "type": "hello", "connector_id": "...", "integration_id": "...",
  "version": "0.1.0", "protocol": "0",
  "capabilities": ["p4.changes", "..."], "tool_versions": {"teamcity": "2025.03"} }

// heartbeat -- egress too; queue_depth is a signal about the studio's load.
{ "type": "heartbeat", "ts": "2026-08-30T12:00:00Z", "queue_depth": 0 }

// result -- answers exactly one command, keyed by its id.
{ "type": "result", "id": "<uuid>", "status": "ok|error|denied|timeout",
  "reason": "<stable token>", "body": {}, "truncated": false, "bytes": 1234 }
```

`reason` is always a stable machine token, on every status that carries one,
never free text. For `status: "error"` (a tool call that reached the tool and
failed), `reason` is the fixed token `tool_error`: the real error text -- p4
stderr with a server host:port, a Go `*url.Error` with the TeamCity URL, any
of it -- carries detail about the studio's LAN and never leaves it. That text
is written only to the connector's local audit log (see §7); the broker never
receives it.

### Broker → connector

```jsonc
{ "type": "welcome", "session_id": "...", "server_time": "...",
  "min_supported_version": "0", "heartbeat_interval": 25 }

{ "type": "reject", "reason": "<stable token>" }

// command -- the only frame that can cause the connector to touch a tool.
{ "type": "command", "id": "<uuid>", "verb": "teamcity.build.get",
  "args": {"build_id": 9001}, "deadline_ms": 8000, "max_bytes": 32768 }
```

Note what a `command` does **not** contain: no host, no port, no URL, no shell
string, no credential. Those live in `connector.yml` on the studio's disk and
never appear on the wire.

### Ordering, replay, and size

- `id` is a UUID minted by us. The connector rejects a duplicate `id` inside a
  10-minute sliding window with `denied / duplicate_command_id`, rather than
  re-executing it.
- A command past `deadline_ms` answers `timeout / deadline_exceeded`. The
  connector caps the deadline at 60 s regardless of what the broker asks for.
- Every verb has a `max_bytes` default; the connector truncates and sets
  `truncated: true` rather than streaming unbounded. (Same instinct as the
  256,000-character pushed-log cap at `jenkins_controller.rb:293`.)
- A `result` is discarded unless the responding session's authenticated
  `integration_id` is the one that issued the command. Reply routing is derived
  **server-side** from the authenticated session, never from a field in a frame.

### Tenant scoping for `Connector.call` (Shuri §6 item 5)

`config/initializers/acts_as_tenant.rb:8` sets `require_tenant = false`, so a
missed scope returns cross-tenant rows *silently* instead of raising. Therefore:

- every command execution wraps in an explicit
  `ActsAsTenant.with_tenant(integration.account)`;
- nothing relies on ambient `Current.account`, which is set by a controller
  `before_action` (`app/controllers/concerns/set_current_request_details.rb:6-9`)
  that does not run on a socket;
- the Redis reply channel is derived server-side from the socket's authenticated
  `integration_id`;
- a `result` frame is dropped unless it matches the issuing session.

The drill: assert tenant context is nil at the start of a request that follows a
connector frame on the same Puma thread. **That drill is Rails-side and is not
covered by this PR** (see `README.md`, "what this does not prove").

---

## 4. Vocabulary and the argument-constraint layer

The authoritative, executable form of this section is
[`internal/vocab/vocab.go`](internal/vocab/vocab.go), which is one file on
purpose: an IT director is asked to read the source, so the source has to be
readable. `butterstack-connector -print-vocabulary` prints it.

### Two layers, both before any tool call

1. **The verb must be in the compiled vocabulary.** A name that is not in the
   vocabulary at all → `denied / unknown_verb`. A name the schema *reserves* but
   this build does not compile in → `denied / verb_not_compiled`. There is no
   dynamic registration, no plugin path, no verb name read from config, and
   there is no `sys.exec`.

2. **Every argument is validated against a fixed per-verb schema.**

   | Rule | Denial reason |
   |---|---|
   | An argument the schema does not name stops the command. It is never ignored into the tool call. | `unknown_argument` |
   | Every argument is a scalar. The schema has no map, object, or free-form kind, so it *cannot express* a parameter bag. | (structural) |
   | Integers are type- and range-checked at the frame boundary. A JSON string `"42"` is not an integer. | `argument_type`, `argument_range` |
   | Depot paths must begin `//`, carry no revision specifier (`@ # %`) and no traversal segment. | `argument_pattern` |
   | Scoped arguments must fall inside a list that lives only in `connector.yml`: `depot_scope`, `allowed_build_types`, `repo_allowlist`. | `out_of_scope_path`, `out_of_scope_build_type`, `out_of_scope_repo` |
   | A depot path's **literal prefix** (everything before the first wildcard) must already sit inside a scoped prefix, so a wildcard can never climb above the scope. `//...` is denied even to a P4 user who could read it. | `out_of_scope_path` |
   | Content-class output is off in v0 at the schema level, not merely in config. | `content_verb_disabled` |

A well-formed verb with an out-of-scope argument is denied **exactly like an
unknown verb**, and both write a local audit line. Only the second kind of
denial actually tests survival condition 2; the first only tests the dispatcher.
Both are drilled separately for that reason (Shuri §6 item 7b).

### No caller-supplied trigger parameters. Ever, in v0.

This is the finding that made this layer day-1 work rather than v1 polish
(Shuri F4). `allowed_jobs` constrains *which* job runs; a `params` map is
unconstrained, and Jenkins build parameters and TeamCity properties are
interpolated into shell build steps by design, including in our own
`Jenkinsfile.unreal` and `Jenkinsfile.minimobile` templates. A caller-supplied
parameter bag would therefore turn the typed allowlist into a code-execution
primitive on the studio's build agents, and falsify the single sentence the
Tier 2 sale rests on (README Appendix B answer 2: "a compromise of our cloud
yields read-access to changelist metadata via a P4 user **you** scoped, a
bounded blast radius").

So:

- `jenkins.build.trigger` takes `{job}` and nothing else. When it ships it
  triggers with the job's own default parameters.
- `teamcity.build.queue` takes `{build_type_id}` and nothing else; the connector
  composes a **fixed** request body, `{"buildType":{"id":"<id>"}}`. No
  `properties`, no `branchName`, no `comment`.
- Neither verb is compiled into v0 at all.
- A v1 may add per-job `allowed_params` in `connector.yml` - an allowlist of
  parameter *names*, each with a value pattern or enum, enforced connector-side
  before the call.

This is enforced structurally, not by convention: `bannedArgNames` in
`vocab.go` lists the argument names no verb may declare - compiled or reserved -
each with the reason it is banned, and `Selfcheck()` runs both in the test suite
and at process start. A build whose vocabulary grew one of them refuses to run.

### Perforce invocation

If the connector shells out to the `p4` CLI rather than using P4Ruby, **every
invocation is an argv array with no shell interpretation** (Shuri F5's smaller
sibling). The ticket is passed through `P4PASSWD` in a minimal environment
rather than on the command line, so it never appears in the studio host's
process list. `p4 describe` is always invoked with `-s`, so diffs are excluded
at the tool boundary as well as in the schema.

### v0 vocabulary

| Verb | Class | v0 |
|---|---|---|
| `sys.ping`, `sys.version`, `sys.capabilities` | M | compiled |
| `teamcity.server.info` | M | compiled |
| `teamcity.build.get {build_id}` | M | compiled |
| `p4.describe {change, max_files, include_diff:false}` | P | compiled |
| `p4.changes {path, max}` | P | compiled |
| `teamcity.build.queue {build_type_id}` | X | reserved, denied |
| `jenkins.build.trigger {job}` | X | reserved, denied |
| `p4.file_contents {depot_path, rev, max_bytes}` | C | reserved, denied |
| `ghes.commit.get {repo, sha}` | P | reserved, denied |
| `horde.server.info` | M | reserved, denied |

Classes are the egress classes from the design note: M = metadata, P = paths,
C = content, X = mutation. **No X-class and no C-class verb is compiled into
v0**, and `Selfcheck()` fails the build if one ever is.

`p4.changes` is the one verb here beyond the three the spike was scoped to
(`teamcity.server.info`, `teamcity.build.get`, `p4.describe`). It is in the
design note's own §2.4 list, and it is compiled in because it is the natural
carrier for `depot_scope`: without a path-bearing argument, the out-of-scope
drill has nothing to deny before a tool call, and that drill is the one Shuri
singled out as the only real test of condition 2.

---

## 5. Credential custody

Every credential the connector uses comes from `connector.yml`, or from a
`*_file` path that `connector.yml` names. There is deliberately:

- no environment-variable fallback for any credential;
- no command-line flag that takes a secret;
- no remote configuration - the broker cannot tell the connector where to find a
  credential.

If the broker could, "your credentials never leave your network" would depend on
our good behaviour rather than on the studio's file permissions.

Enforced: `connector.yml` and every `*_file` must be mode 0600 or stricter, or
the daemon refuses to start. An unrecognised key in `connector.yml` is an error,
not a silent ignore. The redacted config rendering used by the startup banner
and the audit log never prints a secret; no code path prints the config
directly.

Our side stores only the SHA-256 digest of the connector token, and
`credentials_ciphertext` holds nothing for a Connector-transport integration.

---

## 6. Revocation and degradation

- Revoking the connector token on our side closes the socket within one
  heartbeat. The studio's own credentials are untouched, and reconnects with the
  revoked token are refused at the upgrade.
- For every tool with a Tier 1 push path, no event path depends on the
  connector. Stopping it removes pull verbs only.
- A verb-dependent feature checks connector presence and renders the offline
  state; it records "needs connector" and moves on rather than raising. Nothing
  retries a verb against an offline connector more than once.
- Horde is the stated exception: it has no push path, so it is connector-only
  with no degraded mode, and its own setup docs have to say so.

---

## 7. Audit

One local audit line per command, whatever the outcome, including every rejected
and denied one: timestamp, session id, command id, verb, `SHA-256` of the
canonical arguments, status, denial reason, bytes out, duration, truncation
flag. JSON lines, mode 0600, dated files.

Arguments are hashed rather than recorded verbatim, so the log correlates with
our side ("we sent command X, they ran command X") without becoming a second
copy of whatever the arguments contained. Denial detail strings stay local and
never travel in a `result` frame: a denial message that echoed the rejected
value back would be a small egress channel of its own. The same rule applies
to a failed tool call: the `result` frame's `reason` is the stable token
`tool_error` only, and the real error text -- which can carry LAN detail like
a p4 server host:port or a TeamCity URL -- is written to the local audit
line's `detail` field and never leaves the studio.

The log is the studio's evidence, not ours. No verb can read it.
