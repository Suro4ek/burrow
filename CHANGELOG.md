# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `-route host=127.0.0.1:port` serves a fixed hostname from a local upstream,
  so a panel can share the TLS listener without a reverse proxy in front.

### Added

- `burrow ssh` greets an interactive session with where it landed — host,
  directory and user — and names the terminal tab. Written only when a
  terminal was requested, so scp and rsync are untouched.

### Fixed

- The container image declared `/data` as a volume but left it owned by root,
  so the image's own `nonroot` user could not write the tokens file: any
  `docker run -v … :/data ghcr.io/suro4ek/burrow` exited immediately with a
  permission error. The directory now ships owned by `nonroot`, and Docker
  seeds a fresh named volume from it.
- The container image built against a Go older than `go.mod` requires. CI now
  compares the two, since a dependency bump can raise the minimum without
  anyone touching the Dockerfile and the only symptom is a failed release.

### Changed

- **`burrow ssh` now runs its own SSH server** instead of forwarding to the
  system sshd. It serves the directory it was started in, as the user who
  started it, and prints the connect command, a per-run password and the
  known_hosts line that skips the first-connection prompt.

### Fixed

- `burrow login` no longer carries `-no-tls` and `-insecure` over from a
  previous login. A stale `-no-tls` survived every later login and could only
  be cleared with `-no-tls=false`, which surfaced as an unexplained `EOF` once
  the server gained a certificate.
- A handshake that the server closes now explains the two ways it happens —
  wrong port, or disagreeing with the server about TLS — instead of logging a
  bare `EOF`.

### Added

- `-auth-hook-url` and `-usage-hook-url`: delegate agent authentication to an
  external service and report per-tunnel traffic to it, with a `disconnect`
  reply for enforcing quotas. Keeps accounts, plans and billing out of the
  tunnel path entirely.

- `install.sh`: one-line install of a prebuilt binary with checksum
  verification, no Go or Docker required.
- Homebrew casks published to `suro4ek/tap` on every release.
- `-https` and `-redirect-https`: burrowd terminates TLS for tunnels and the
  panel itself, so a wildcard certificate removes the need for a reverse proxy.

## [0.1.0] - 2026-08-08

First public release.

### Added

- HTTP tunnels routed by `Host` on a wildcard base domain, proxied through
  `httputil.ReverseProxy` so websockets, SSE and `X-Forwarded-*` work.
- TCP tunnels on a configurable public port pool, for SSH and anything else.
- `burrow` agent with `login`, `http`, `tcp`, `ssh`, `start`, `config` and
  `logout` commands, a saved login in `~/.config/burrow/config.json`, and an
  automatic reconnect loop with backoff.
- `burrow ssh` prefers a port reserved for its token, so the published address
  stays the same across restarts.
- Web admin panel at `/_admin` (Vite + React, embedded with `go:embed`):
  create, edit, disable, rotate and delete tokens; watch live tunnels with
  traffic counters; disconnect agents.
- Token store persisted as JSON with atomic writes, plus reservations of
  subdomains and TCP ports per token.
- TLS on the control listener with certificates re-read on renewal, without a
  restart.
- systemd unit, Caddyfile and Docker Compose examples.

[Unreleased]: https://github.com/suro4ek/burrow/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/suro4ek/burrow/releases/tag/v0.1.0
