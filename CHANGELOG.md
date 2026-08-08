# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `install.sh`: one-line install of a prebuilt binary with checksum
  verification, no Go or Docker required.

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
