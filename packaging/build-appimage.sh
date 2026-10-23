#!/bin/sh
# Builds the desktop bundled AppImage for $GOARCH into dist/.
set -eu

. ./packaging/release-env.sh
. ./packaging/appimage-core.sh

appimage_begin

CGO_ENABLED=1 go build -tags tagger -trimpath \
  -ldflags="-s -w $LDFLAGS -X '$MOD.Package=appimage' -X 'main.defaultDesktop=true'" \
  -o "$app/usr/bin/monbooru" ./cmd/monbooru

cp tools/ffmpeg tools/ffprobe tools/libonnxruntime.so "$app/usr/bin/"
cp -r tools/licenses "$app/usr/share/"

appimage_finish
