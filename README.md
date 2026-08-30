# ButterStack Connector

An outbound-only daemon a game studio runs inside its own network so ButterStack can reach a private, on-premises Perforce, TeamCity, Jenkins, GitHub Enterprise Server, or Horde without the studio opening a single inbound port.

One outbound TLS connection to one hostname on 443. A typed command allowlist, never a tunnel and never a shell. Credentials stay on the studio's disk and never cross the wire.

Status: pre-release. The working spike currently lives in the main ButterStack repository and will be extracted here. Tracking: ButterStack/butter_stack#1575.
