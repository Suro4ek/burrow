# Contributing

Thanks for taking a look. Bug reports, questions and patches are all welcome.

## Getting set up

You need Go (the version in `go.mod`) and Node 24 for the admin panel.

```sh
git clone https://github.com/suro4ek/burrow
cd burrow
make web      # builds the admin panel into web/dist
make build    # produces bin/burrow and bin/burrowd
```

## Running it locally

`lvh.me` and all of its subdomains resolve to `127.0.0.1`, which makes it a
convenient stand-in for a real wildcard domain:

```sh
BURROWD_ADMIN_PASSWORD=devpassword ./bin/burrowd \
  -domain lvh.me -http 127.0.0.1:8080 -control 127.0.0.1:7000 \
  -tokens tokens.json -admin-addr 127.0.0.1:7002
```

Open <http://127.0.0.1:7002/_admin/>, create a token, and the panel hands you
the `burrow login` line to paste.

For panel work, run Vite instead of rebuilding on every change:

```sh
make web-dev    # http://127.0.0.1:5173/_admin/
```

Vite proxies `/_api` to `127.0.0.1:7002`. That extra listener answers on any
`Host`; the main one only serves the panel for the configured base domain.

## Before you open a pull request

```sh
make lint     # go vet + gofmt
make race     # the test run that matters — this code is nearly all concurrent
```

**If you changed anything under `web/src`, run `make web` and commit
`web/dist`.** The panel is embedded with `go:embed`, so the committed build is
what makes `go install` produce a working binary. CI fails if `web/dist` does
not match the source.

## What the tests cover

`e2e_test.go` starts a real server and a real agent on ephemeral ports and
pushes traffic through them. If you touch the proxy, the registry or the
protocol, that is the file to extend — a unit test on its own tends to miss the
interesting failures here.

`internal/server/store_test.go` covers the token store, including rollback on
conflict. `internal/server/admin_test.go` covers the admin API, including the
authorization rules; please keep adding cases there for anything that touches
access control.

## Style

- Match the surrounding code. Comments explain *why*, not *what*.
- Errors get context: `fmt.Errorf("listen control %s: %w", addr, err)`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org)
  (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`) — the release
  changelog is generated from them.

## Releasing

Maintainers only. Push an annotated tag and GitHub Actions does the rest:

```sh
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

That builds the binaries with GoReleaser, publishes the GitHub Release, and
pushes a multi-arch image to `ghcr.io/suro4ek/burrow`.
