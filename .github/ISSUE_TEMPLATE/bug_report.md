---
name: Bug report
about: Something isn't working the way it should
title: ""
labels: bug
assignees: ""
---

**What happened**

A clear description of the bug.

**What you expected**

What you expected to happen instead.

**Steps to reproduce**

1.
2.
3.

**Connector log**

```
paste the relevant lines from the daemon's log here

The daemon redacts token and ticket values as [redacted], but please read
before pasting anyway: do not include a token, a ticket, or anything else
you would not put in a public issue.
```

**Your connector.yml, with secrets removed**

```yaml
# endpoint, connector_id, scopes, and the perforce/teamcity sections are the
# useful part. Remove token, token_file contents, and any ticket value.
```

**Environment**

- Connector version: (the image tag you ran, or `butterstack-connector -version`)
- How you run it: (published image, locally built image, or binary)
- Backend and version: (Perforce/Helix Core, TeamCity, etc.)
- Host OS:

**Additional context**

Anything else worth knowing.
