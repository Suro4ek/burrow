# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.1] - 2026-08-17

### Fixed

- The container image declared `/data` as a volume but left it owned by root,
  so the image's own `nonroot` user could not write the tokens file: any
  `docker run -v … :/data ghcr.io/suro4ek/burrow` exited immediately with a
  permission error. The directory now ships owned by `nonroot`, and Docker
  seeds a fresh named volume from it.

## [0.3.0] - 2026-08-16

### Added

- `-route host=127.0.0.1:port` serves a fixed hostname from a local upstream,
  so a panel can share the TLS listener without a reverse proxy in front.

## [0.2.4] - 2026-08-10

### Fixed

- `burrow ssh` closed the pty while two goroutines were still using it — the
  client-to-shell copy and the window-resize loop. macOS scheduling hid it;
  Linux caught it as a data race. Both are now waited on before the close.

## [0.2.3] - 2026-08-10

### Added

- `burrow ssh` greets an interactive session with where it landed — host,
  directory and user — and names the terminal tab. Written only when a
  terminal was requested, so scp and rsync are untouched.

### Fixed

- The container image built against a Go older than `go.mod` requires. CI now
  compares the two, since a dependency bump can raise the minimum without
  anyone touching the Dockerfile and the only symptom is a failed release.

## [0.2.2] - 2026-08-09

### Added

- The handshake carries the authorized keys the server holds for that
  identity, so a key added centrally opens a `burrow ssh` session with no
  password. Delivered on every handshake, so revoking a key takes effect on
  the next reconnect without restarting anything.

## [0.2.1] - 2026-08-09

### Fixed

- `burrow ssh` assigned its embedded server from `Serve` while `Close` read
  it. Besides the data race, a `Close` that arrived first saw nil and returned
  without stopping anything. The server is now built in the constructor.

## [0.2.0] - 2026-08-09

### Changed

- **`burrow ssh` now runs its own SSH server** instead of forwarding to the
  system sshd. It serves the directory it was started in, as the user who
  started it, and prints the connect command, a per-run password and the
  known_hosts line that skips the first-connection prompt.

## [0.1.8] - 2026-08-09

### Added

- `-auth-hook-url` and `-usage-hook-url`: delegate agent authentication to an
  external service and report per-tunnel traffic to it, with a `disconnect`
  reply for enforcing quotas. Keeps accounts, plans and billing out of the
  tunnel path entirely.

## [0.1.7] - 2026-08-09

### Fixed

- A handshake that times out is now explained too, not only one the server
  closes outright — the shape a wrong port actually takes.

## [0.1.6] - 2026-08-09

### Fixed

- `burrow login` no longer carries `-no-tls` and `-insecure` over from a
  previous login. A stale `-no-tls` survived every later login and could only
  be cleared with `-no-tls=false`, which surfaced as an unexplained `EOF` once
  the server gained a certificate.
- A handshake that the server closes now explains the two ways it happens —
  wrong port, or disagreeing with the server about TLS — instead of logging a
  bare `EOF`.

## [0.1.5] - 2026-08-08

### Added

- `-https` and `-redirect-https`: burrowd terminates TLS for tunnels and the
  panel itself, so a wildcard certificate removes the need for a reverse proxy.

## [0.1.4] - 2026-08-08

### Fixed

- The `burrowd` archives for 0.1.2 and 0.1.3 contained an executable named
  `burrow`: removing a deprecated GoReleaser key also deleted the `binary:`
  lines under `builds`, and GoReleaser fell back to the project name.
  `install.sh` rejected the archive, which is how it surfaced. CI now unpacks
  every archive and asserts it holds the binary it is named after.

## [0.1.3] - 2026-08-08

### Fixed

- The 0.1.2 release published its binaries and then failed to publish the
  Homebrew cask: GoReleaser accepts only a bare `{{ .Env.VAR }}` in the tap
  token field, and neither `goreleaser check` nor a local snapshot catches the
  form it rejects.

## [0.1.2] - 2026-08-08

### Added

- `install.sh`: one-line install of a prebuilt binary with checksum
  verification, no Go or Docker required.
- Homebrew casks published to `suro4ek/tap` on every release.

## [0.1.1] - 2026-08-08

### Changed

- Dependabot opens one pull request per ecosystem rather than one per action,
  and the pinned actions and frontend toolchain move up to current majors. The
  committed `web/dist` freshness check warns instead of failing on Dependabot
  pull requests, which cannot regenerate the bundle.

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

[Unreleased]: https://github.com/suro4ek/burrow/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/suro4ek/burrow/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/suro4ek/burrow/compare/v0.2.4...v0.3.0
[0.2.4]: https://github.com/suro4ek/burrow/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/suro4ek/burrow/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/suro4ek/burrow/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/suro4ek/burrow/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/suro4ek/burrow/compare/v0.1.8...v0.2.0
[0.1.8]: https://github.com/suro4ek/burrow/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/suro4ek/burrow/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/suro4ek/burrow/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/suro4ek/burrow/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/suro4ek/burrow/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/suro4ek/burrow/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/suro4ek/burrow/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/suro4ek/burrow/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/suro4ek/burrow/releases/tag/v0.1.0
