# Security Policy

The connector is a piece of security infrastructure: it exists so a studio can give ButterStack read access to a private Perforce, TeamCity, Jenkins, GitHub Enterprise Server, or Horde server without opening an inbound port or handing over a credential. A defect here is a defect in that boundary, so please report one privately rather than in a public issue.

Email hello@butterstack.com with details. We will acknowledge receipt and work with you on a fix and a disclosure timeline.

## What we consider a security issue here

Anything that breaks one of the daemon's four standing claims:

1. **Outbound only.** The daemon opens exactly one outbound TLS connection and never listens. Anything that makes it accept an inbound connection, bind a port, or act as a tunnel.
2. **The allowlist is the boundary.** Only verbs compiled into `internal/vocab/vocab.go` can execute, with constrained arguments. Anything that executes a verb outside the compiled vocabulary, gets an argument past its constraint, reaches a shell, or turns a verb into a code-execution primitive inside the studio's network. A verb accepting a host, port, URL, or shell string would be one of these.
3. **Credentials stay local.** Perforce tickets and TeamCity tokens live in the studio's own files and are never transmitted. Anything that puts a credential, a LAN hostname, a port, or an internal URL into a frame, an error message, or the audit log.
4. **The broker cannot reconfigure the connector.** Configuration comes from `connector.yml` on the studio's disk. Anything that lets a broker frame change what the daemon is allowed to do or where it looks for a credential.

Findings against the ButterStack broker side (`wss://connect.butterstack.com/connect`) are also in scope and go to the same address.

## What is not a security issue

- A reserved verb returning a denial. `jenkins.build.trigger`, `ghes.commit.get`, `horde.server.info`, `teamcity.build.queue`, and `p4.file_contents` are in the vocabulary so the schema is self-documenting and the drills exercise their denial path. They are not executable in this build, by design.
- The daemon refusing a `ws://` endpoint, a malformed `connector.yml`, or an unknown YAML key. Those are intentional hard failures at startup.

## Supported versions

This project is pre-1.0. Security fixes land on the latest published tag; there is no separate maintenance branch at this stage. Pin an exact version tag rather than `latest`, and watch the Releases page.
