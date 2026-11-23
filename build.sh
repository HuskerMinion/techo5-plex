#!/bin/sh
# Builds techo5-plex for the devices: static, no cgo.
#   bin/techo5-plex-arm    32-bit ARM (GOARM=7): every Echo Show build, the Show 5 included
#   bin/techo5-plex-arm64  64-bit ARM: the boards with a 64-bit kernel (Show 8 2nd gen, Skylight)
# VERSION=1.2.3 ./build.sh stamps a version; otherwise it is the git description.
set -eu
cd "$(dirname "$0")"
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
ldflags="-s -w -X main.version=$version"
mkdir -p bin
export CGO_ENABLED=0 GOOS=linux
GOARCH=arm GOARM=7 nice -n 10 go build -trimpath -ldflags "$ldflags" -o bin/techo5-plex-arm .
GOARCH=arm64 nice -n 10 go build -trimpath -ldflags "$ldflags" -o bin/techo5-plex-arm64 .
(cd bin && sha256sum techo5-plex-arm techo5-plex-arm64 > SHA256SUMS)
echo "built $version:"
ls -l bin/techo5-plex-arm bin/techo5-plex-arm64
