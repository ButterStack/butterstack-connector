# UAT

`tests/uat/connector.spec.js` (in the ButterStack/butter_stack repo) is a second, Docker-based test of this daemon: TeamCity Tier 1 webhook intake into the real Rails app, and the compiled verbs, denials, and degradation drills against a containerized version of this connector, run inside a Docker-modelled "studio LAN" that is NOT reachable from the cloud side (outbound allowed, inbound blocked), which is the shape issue #1574/#1575 actually sell.

```bash
docker-compose --profile core --profile connector up -d --build connector
npm run test:uat:connector
npm run test:uat:connector:keep-data   # leaves the signup/project for inspection
```

**Always scope `--build` to `connector`.** `web`/`sidekiq` still carry a `build:` key in the base `docker-compose.yml` even though `docker-compose.uat-connector.yml` pins them to the prebuilt `image: butter_stack-web:latest`; an unscoped `--build` rebuilds and retags that shared image, which every worktree's dev stack uses.

## Topology

```
   "cloud" side (network: default)          "studio LAN" (network: studio_lan,
                                              internal: true -- no route out)
   +-----------+     +--------------+                +----------------+
   |    web    |     | mock-broker  |<===wss:9443====>|   connector    |
   | (Rails)   |     | (drill stand-|                 | (the daemon    |
   +-----+-----+     |  in for the  |                 |  under test)   |
         ^           |  real /connect|                +--------+-------+
         |           |  endpoint)   |                          |
         | http :3000|              |                          | (LAN-local)
   +-----+---------+ +--------------+                          |
   | studio-egress |<===================studio_lan==============+
   | (socat, models|                          ^
   |  outbound-only|                 +--------+---------+
   |  firewall)    |                 |   teamcity-stub   |
   +---------------+                 | (fake on-prem CI, |
                                     |  no published port)|
                                     +--------------------+
```

`internal: true` on `studio_lan` is the whole isolation guarantee. `web` cannot resolve or reach `teamcity-stub`; `connector` publishes no port at all (`docker inspect` shows an empty port map, and `netstat -tln` inside it lists nothing but Docker's own embedded DNS resolver); `teamcity-stub`'s only way to reach Rails is the one TCP port `studio-egress` forwards, which is the studio firewall's "outbound allowed" in miniature. `connector.spec.js` phase 100 asserts this shape directly before doing anything else.

## What the UAT proves

- The TeamCity Tier 1 webhook path both ways Teddy's design note describes: the curl-step flat payload (`X-Webhook-Token`) creates a `BuildRun` with `ci_provider == 'teamcity'`; TeamCity's own built-in webhook envelope authenticates and records a `WebhookEvent` but does not yet create a `BuildRun` (there is no adapter for the `{eventType, payload}` shape in `jenkins_controller.rb`, issue #1574 Phase 1, `normalize_ci_payload`). The spec documents this as the real current behavior; the assertion is written to flip the day that adapter lands.
- A real TeamCity 2026.1.3 server (captured on Ryan's LAN with a request logger) authenticates its built-in webhook with a standard `Authorization: Basic base64(username:password)` header built from `teamcity.internal.webhooks.username`/`.password`, and only when `.password` is declared as a plain-typed parameter. `teamcity-stub`'s `mode: 'native'` mirrors this.
- Neither webhook token nor its HTTP headers ever land in a persisted `WebhookEvent`.
- The connector's compiled vocabulary, denials (unknown verb, out-of-scope path, reserved-but-not-compiled verb, wrong argument type, a disabled content toggle, an unknown argument), and the no-shell proof on the p4 argv log, all against a real containerized daemon rather than the in-process drill harness.
- Broker-side refusals (query-string token, missing bearer token) before any session state exists, cross-session result discard, and the degradation drills (connector stop/start, token revoke/re-register), the same four rules `test/drills.rb` proves, exercised over the network instead of a Ruby method call.

## What the UAT does not prove

Everything `test/README.md`'s own "what this spike does not prove" section already says, plus:

- **There is still no real `/connect` Rack endpoint.** `mock-broker` here is the same drill stand-in as `test/mock_broker.rb`, containerized (`test/mock_broker_server.rb`) with a small HTTP admin API bolted on so the Playwright test can drive sessions/refusals/revoke/partition without speaking the wire protocol itself.
- **No real TeamCity.** `teamcity-stub` (`test/support/teamcity_stub.rb`) is a hand-rolled fake that answers exactly the REST calls the connector makes and can fire the two outbound webhook shapes a real TeamCity delivers.
- **No real p4d.** `test/support/fake_p4` stands in for the `p4` CLI, same as in the drills.
- **Idle-connector liveness** is a WS-layer ping/pong rule: the heartbeat goroutine sends a WebSocket ping alongside the application-level heartbeat frame, and the read loop's deadline only means "reconnect" when no inbound frame of any kind (a pong included) arrived within heartbeat x readSlack, so a quiet-but-healthy broker no longer cycles the session offline at rest.
- Everything else `test/README.md` already lists: real infrastructure, a production-hardened image (no Sigstore signing, no SBOM, no digest-pinned base), scale, and any verb beyond the five compiled ones.
