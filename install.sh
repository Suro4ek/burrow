#!/bin/sh
# Installs a burrow binary from the GitHub releases.
#
#   curl -sSLf https://raw.githubusercontent.com/Suro4ek/burrow/main/install.sh | sh
#   curl -sSLf https://raw.githubusercontent.com/Suro4ek/burrow/main/install.sh | sh -s -- burrowd
#
# Environment:
#   BURROW_VERSION      tag to install, e.g. v0.1.1 (default: the latest release)
#   BURROW_INSTALL_DIR  where to put the binary (default: /usr/local/bin, or
#                       ~/.local/bin when /usr/local/bin is not writable)
#
# POSIX sh on purpose: this has to run under dash and busybox ash, not just bash.
set -eu

REPO="Suro4ek/burrow"
BINARY="${1:-burrow}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

case "$BINARY" in
	burrow | burrowd) ;;
	*) die "unknown binary '$BINARY'; expected 'burrow' (agent) or 'burrowd' (server)" ;;
esac

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }
need curl
need tar

# ---------------------------------------------------------------- platform ---

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux | darwin | freebsd) ;;
	msys* | mingw* | cygwin*)
		die "Windows is not supported by this script; download the .zip from
             https://github.com/$REPO/releases" ;;
	*) die "unsupported operating system: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) die "unsupported architecture: $arch (releases cover amd64 and arm64)" ;;
esac

if [ "$BINARY" = "burrowd" ] && [ "$os" = "darwin" ]; then
	say "note: burrowd is a server daemon; you normally want it on a Linux VPS."
fi

# ----------------------------------------------------------------- version ---

version="${BURROW_VERSION:-}"
if [ -z "$version" ]; then
	# Resolve the latest tag from the redirect that /releases/latest performs.
	# This avoids the GitHub API, which rate-limits unauthenticated callers
	# hard enough to break installs behind a shared NAT.
	latest_url=$(curl -sSLf -o /dev/null -w '%{url_effective}' \
		"https://github.com/$REPO/releases/latest") ||
		die "could not reach GitHub to determine the latest version"
	version=${latest_url##*/}
fi
case "$version" in
	v*) ;;
	*) die "version '$version' should start with 'v', e.g. v0.1.1" ;;
esac

# Release archives carry the version without the leading "v".
archive="${BINARY}_${version#v}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

# ---------------------------------------------------------------- download ---

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t burrow)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $archive ($version)"
curl -sSLf -o "$tmp/$archive" "$base/$archive" ||
	die "no release asset '$archive'.
     Check what exists at https://github.com/$REPO/releases/tag/$version"

# Verifying the checksum is the point of publishing one: without it a broken
# proxy or a truncated download installs a corrupt binary silently.
if curl -sSLf -o "$tmp/checksums.txt" "$base/checksums.txt"; then
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
	elif command -v shasum >/dev/null 2>&1; then
		sum=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
	else
		sum=""
		say "warning: no sha256sum or shasum found, skipping checksum verification"
	fi
	if [ -n "$sum" ]; then
		want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
		[ -n "$want" ] || die "$archive is missing from checksums.txt"
		[ "$sum" = "$want" ] || die "checksum mismatch for $archive
     expected $want
     got      $sum"
		say "Checksum verified"
	fi
else
	say "warning: checksums.txt is not available for $version, skipping verification"
fi

tar -xzf "$tmp/$archive" -C "$tmp" ||
	die "could not extract $archive"
[ -f "$tmp/$BINARY" ] || die "$archive did not contain a '$BINARY' executable"

# ----------------------------------------------------------------- install ---

dir="${BURROW_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
	# Prefer /usr/local/bin, but fall back rather than demanding sudo: a user
	# without root should still end up with a working binary.
	if [ -w /usr/local/bin ] 2>/dev/null; then
		dir="/usr/local/bin"
	elif [ "$(id -u)" = "0" ]; then
		dir="/usr/local/bin"
	else
		dir="$HOME/.local/bin"
	fi
fi

mkdir -p "$dir" 2>/dev/null || die "cannot create $dir"
chmod +x "$tmp/$BINARY"

if [ -w "$dir" ]; then
	mv -f "$tmp/$BINARY" "$dir/$BINARY"
elif command -v sudo >/dev/null 2>&1; then
	say "Installing to $dir (needs sudo)"
	sudo mv -f "$tmp/$BINARY" "$dir/$BINARY"
else
	die "$dir is not writable and sudo is unavailable.
     Retry with: BURROW_INSTALL_DIR=\$HOME/.local/bin sh install.sh $BINARY"
fi

say "Installed $BINARY $version to $dir/$BINARY"

# A binary in a directory that is not on PATH looks like a failed install, so
# say it plainly instead of letting the next command be "command not found".
case ":${PATH}:" in
	*":$dir:"*) ;;
	*)
		say ""
		say "warning: $dir is not on your PATH. Add it with:"
		say "    echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile"
		;;
esac

say ""
if [ "$BINARY" = "burrow" ]; then
	say "Next: burrow login -server YOUR-SERVER:7000 -token YOUR-TOKEN"
	say "Then: burrow http 3000    or    burrow ssh"
else
	say "Next: burrowd -domain tun.example.com -tokens /etc/burrowd/tokens.json"
	say "See https://github.com/$REPO#setting-up-a-real-server"
fi
