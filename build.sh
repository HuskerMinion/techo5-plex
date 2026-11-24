#!/bin/sh
# Builds techo5-plex for the devices: static, no cgo.
#   bin/techo5-plex-arm    32-bit ARM (GOARM=7): every Echo Show build, the Show 5 included
#   bin/techo5-plex-arm64  64-bit ARM: the boards with a 64-bit kernel (Show 8 2nd gen, Skylight)
#   bin/addon.json         the manifest TECHO5's add-on manager installs it by: addon.json, as it is
# VERSION=1.2.3 ./build.sh stamps a version; otherwise it is the git description.
set -eu
cd "$(dirname "$0")"
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
# A release's manifest has to name the release: TECHO5's catalog pins it by its hash, and a device refuses
# one whose version is not the catalog entry's. So a stamped build with another version in addon.json
# stops here, rather than publishing a manifest no catalog entry could use.
manifest_version=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' addon.json)
if [ -n "${VERSION:-}" ] && [ "${VERSION#v}" != "$manifest_version" ]; then
	echo "build.sh: addon.json says version $manifest_version, not ${VERSION#v}: update it in the release's commit" >&2
	exit 1
fi
ldflags="-s -w -X main.version=$version"
mkdir -p bin
export CGO_ENABLED=0 GOOS=linux
GOARCH=arm GOARM=7 nice -n 10 go build -trimpath -ldflags "$ldflags" -o bin/techo5-plex-arm .
GOARCH=arm64 nice -n 10 go build -trimpath -ldflags "$ldflags" -o bin/techo5-plex-arm64 .
cp addon.json bin/addon.json
(cd bin && sha256sum techo5-plex-arm techo5-plex-arm64 addon.json > SHA256SUMS)
echo "built $version:"
ls -l bin/techo5-plex-arm bin/techo5-plex-arm64 bin/addon.json
