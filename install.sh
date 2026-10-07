#!/bin/sh
# Installs the latest `think` release for macOS or Linux:
#   curl -fsSL https://raw.githubusercontent.com/dylantirandaz/inklingharness/main/install.sh | sh
# THINK_INSTALL_DIR overrides the target directory (default: ~/.local/bin).
set -eu

repository="dylantirandaz/inklingharness"
install_dir="${THINK_INSTALL_DIR:-$HOME/.local/bin}"

fail() {
	echo "think install: $*" >&2
	exit 1
}

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported operating system $(uname -s); macOS and Linux are supported" ;;
esac

case "$(uname -m)" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*) fail "unsupported CPU architecture $(uname -m)" ;;
esac
# A shell under Rosetta reports x86_64 on Apple silicon; use the native build.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ]; then
	arch=arm64
fi

command -v curl >/dev/null 2>&1 || fail "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
	checksum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	checksum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail "sha256sum or shasum is required"
fi

asset="think_${os}_${arch}.tar.gz"
base="https://github.com/${repository}/releases/latest/download"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "Downloading $asset"
curl -fsSL -o "$work/$asset" "$base/$asset" || fail "download failed: $base/$asset"
curl -fsSL -o "$work/checksums.txt" "$base/checksums.txt" || fail "download failed: $base/checksums.txt"

expected="$(awk -v name="$asset" '$2 == name { print $1 }' "$work/checksums.txt")"
[ -n "$expected" ] || fail "checksums.txt has no entry for $asset"
actual="$(checksum "$work/$asset")"
[ "$expected" = "$actual" ] || fail "checksum mismatch for $asset: expected $expected, got $actual"

tar -xzf "$work/$asset" -C "$work" think
mkdir -p "$install_dir"
install -m 0755 "$work/think" "$install_dir/think"
echo "Installed $install_dir/think"

case ":$PATH:" in
*":$install_dir:"*) echo "Run: think login, then think" ;;
*)
	echo "$install_dir is not on PATH. Add this line to your shell profile (~/.zshrc or ~/.bashrc):"
	echo "  export PATH=\"$install_dir:\$PATH\""
	;;
esac
