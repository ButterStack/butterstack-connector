# Drills

`ruby drills.rb` (or `make drills` from the parent directory) runs the seven
drills from design note §4.3 against `mock_broker.rb`, plus a round-trip phase
and the broker-side half of drill (f).

```
CONNECTOR_BIN=../build/butterstack-connector ruby drills.rb
VERBOSE=1 CONNECTOR_BIN=../build/butterstack-connector ruby drills.rb   # per-assertion output
```

Everything runs on loopback in a temporary directory: a throwaway CA and server
certificate so the connector dials a real `wss://` endpoint with real
certificate verification, a stub TeamCity that refuses any request not bearing
the token from `connector.yml`, and a fake `p4` that records its own argv.

Requires Ruby 3.2+ (stdlib only) and a built connector binary. Takes about
15 seconds; the recovery drills contain real waits.

## Files

| | |
|---|---|
| `drills.rb` | the harness and the assertions |
| `mock_broker.rb` | the ButterStack side: `/connect` upgrade, header-only token auth, frame exchange, revoke, partition |
| `support/ws.rb` | server-side RFC 6455, written independently of the Go client so a framing mistake fails a drill rather than agreeing with itself |
| `support/tls.rb` | throwaway CA and server certificate |
| `support/teamcity_stub.rb` | a minimal on-prem TeamCity |
| `support/fake_p4` | a `p4` stand-in emitting `-Mj -ztag` records and logging its argv |

## What the mock broker is not

It is not the broker. The real one is a dedicated Rack endpoint at `/connect` on
the Rails app with hashed-token auth, a Redis-routed command and result path,
and explicit tenant scoping - none of which exists yet and none of which this
models. It does implement faithfully the four rules the drills exist to prove:

1. the upgrade is refused **before any per-connection state is allocated** when
   the token is absent, malformed, revoked, or wrong;
2. a query-string token is refused, and no code path here reads a token from a
   query string;
3. tokens are stored as the SHA-256 digest of the secret segment and compared in
   constant time - the plaintext is never held;
4. a `result` frame is matched against the issuing session's own outstanding
   commands; one carrying another session's command id is discarded.
