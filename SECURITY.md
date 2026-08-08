# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub Security Advisories](https://github.com/suro4ek/burrow/security/advisories/new)
rather than a public issue.

Include what you did, what happened, and what you expected. A proof of concept
helps a lot. I will acknowledge the report and keep you updated as it is fixed.

This is a hobby project maintained in spare time, so please do not expect an
enterprise response time — but security reports go to the front of the queue.

## What burrow assumes

Being honest about the threat model saves everyone time:

- **A tunnel publishes a local service to the internet.** That is the whole
  point of the tool. Anyone who knows the URL reaches your service; burrow does
  not add authentication in front of it. If the thing you tunnel has no auth,
  neither does the tunnel.
- **Agent tokens are bearer credentials.** Whoever holds one can publish
  tunnels as that token. They cross the network in the clear unless the control
  listener has TLS (`-tls-cert` / `-tls-key`) — configure it on any server that
  is not on a trusted private network.
- **The admin panel is a single-password gate.** It is meant for one operator,
  not for a team with roles. If you would rather not expose it at all, leave it
  off the public host and bind it to loopback with `-admin-addr 127.0.0.1:7002`,
  reaching it over an SSH port-forward.
- **`-insecure` disables certificate verification.** The connection stays
  encrypted but is no longer authenticated, so it is only reasonable while
  setting up a self-signed certificate.
- **Tunnel operators can see tunnel traffic.** The server proxies plaintext
  HTTP to the agent; end-to-end secrecy from the server operator is not a
  property this design has.

## In scope

Reports that fit the model above are very much wanted, for example:

- Bypassing admin authentication or the CSRF protections on the API.
- One token claiming a subdomain or port reserved by another.
- Reaching a tunnel or the panel from a host that should not route there.
- Crashes or resource exhaustion triggered by a remote, unauthenticated peer.
- Anything that lets an agent read or affect another agent's traffic.

## Out of scope

- Missing authentication on whatever *you* chose to tunnel.
- Attacks that require the admin password or a valid agent token you were given.
- Denial of service by an authenticated agent that you yourself issued a token
  to (revoke it in the panel).
