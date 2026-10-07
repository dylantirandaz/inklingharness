#!/bin/sh
# Builds `think` for every supported platform and publishes a GitHub release.
# Usage: scripts/release.sh v0.1.0
set -eu

[ $# -eq 1 ] || { echo "usage: scripts/release.sh VERSION" >&2; exit 2; }
version="$1"
cd "$(dirname "$0")/.."

go vet ./...
go test -count=1 ./...

dist="$(mktemp -d)"
trap 'rm -rf "$dist"' EXIT
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
	os="${target%/*}"
	arch="${target#*/}"
	mkdir "$dist/$os-$arch"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$dist/$os-$arch/think" ./cmd/think
	# macOS tar adds extended attributes that GNU tar warns about on Linux.
	COPYFILE_DISABLE=1 tar --no-mac-metadata --no-xattrs -czf "$dist/think_${os}_${arch}.tar.gz" -C "$dist/$os-$arch" think
done
(cd "$dist" && shasum -a 256 think_*.tar.gz >checksums.txt)

# Measure the binary of this host in a real terminal. A crossed startup,
# idle CPU, or size limit stops the release before anything is published.
go run ./scripts/limits "$dist/$(go env GOOS)-$(go env GOARCH)/think"

gh release create "$version" --title "$version" --notes "Install: curl -fsSL https://raw.githubusercontent.com/dylantirandaz/inklingharness/main/install.sh | sh" \
	"$dist"/think_*.tar.gz "$dist/checksums.txt"
