#!/bin/sh
# Chmura installer.
#
#   curl -fsSL https://raw.githubusercontent.com/daropotter/chmura/main/install.sh | sh
#
# Environment:
#   VERSION             pin a release tag (e.g. v0.1.0); default: newest release
#   CHMURA_INSTALL_DIR  install location; default: ~/.local/bin
set -eu

REPO="daropotter/chmura"
INSTALL_DIR="${CHMURA_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERSION:-}"
BINARIES="chmura chmura-server chmura-agent chmura-dev"

# Exit codes follow the Chmura contract: 2 = usage/config, 1 = execution error.
usage_err() { echo "install: $*" >&2; exit 2; }
die() { echo "install: $*" >&2; exit 1; }

# Detect OS.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin | linux) ;;
  *) usage_err "unsupported OS: $os (only darwin and linux are built)" ;;
esac

# Detect architecture.
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) usage_err "unsupported architecture: $arch" ;;
esac

# A pinned VERSION must be a release tag (leading v) so the tag path resolves.
if [ -n "$VERSION" ]; then
  case "$VERSION" in
    v*) ;;
    *) usage_err "VERSION must be a release tag like v0.1.0 (got: $VERSION)" ;;
  esac
else
  # Newest release from the list — not /releases/latest, which skips pre-releases.
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases" \
    | grep '"tag_name":' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
  [ -n "$VERSION" ] || die "could not determine the latest release"
fi

num="${VERSION#v}" # asset names carry the version without the leading v
archive="chmura_${num}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading chmura $VERSION ($os/$arch)…" >&2
curl -fsSL "$base/$archive" -o "$tmp/$archive" || die "download failed: $archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || die "download failed: checksums.txt"

# Verify the archive against the published sha256 — fail hard, this is the point.
if command -v sha256sum >/dev/null 2>&1; then
  sumcmd="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  sumcmd="shasum -a 256"
else
  die "no sha256 tool found (need sha256sum or shasum) — cannot verify the archive"
fi
# checksums.txt lines are "<hash>  <filename>"; -F keeps the dotted name literal.
# shellcheck disable=SC2086 # sumcmd is a command + args; word splitting is intended
( cd "$tmp" && grep -F "  $archive" checksums.txt | $sumcmd -c - >/dev/null 2>&1 ) \
  || die "checksum verification failed for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" || die "could not extract $archive"

mkdir -p "$INSTALL_DIR"
for bin in $BINARIES; do
  [ -f "$tmp/$bin" ] || die "archive is missing $bin"
  install -m 0755 "$tmp/$bin" "$INSTALL_DIR/$bin"
done

echo "Installed to $INSTALL_DIR: $BINARIES" >&2

# The primary binary must actually run — otherwise this is a silent bad install.
"$INSTALL_DIR/chmura" --version >/dev/null 2>&1 \
  || die "installed chmura did not run — check your architecture and the archive"
echo "chmura $("$INSTALL_DIR/chmura" --version) is ready." >&2

# Nudge if the install dir is not on PATH.
case ":${PATH}:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "Note: $INSTALL_DIR is not on your PATH — add it to use 'chmura' directly." >&2 ;;
esac
