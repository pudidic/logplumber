#!/bin/sh
# release.sh vX.Y.Z — builds plumb for Linux, macOS and Windows on x86-64 and
# ARM64 and packs each into dist/ with README.md and LICENSE, plus SHA256SUMS.
set -eu
tag=${1:?usage: release.sh vX.Y.Z}
ver=${tag#v}
rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
	os=${target%/*} arch=${target#*/}
	name=plumb_${ver}_${os}_${arch}
	exe=plumb
	[ "$os" = windows ] && exe=plumb.exe
	mkdir -p "dist/$name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$ver" -o "dist/$name/$exe" ./cmd/plumb
	cp README.md LICENSE "dist/$name/"
	if [ "$os" = windows ]; then
		(cd dist && zip -qr "$name.zip" "$name")
	else
		tar -C dist -czf "dist/$name.tar.gz" "$name"
	fi
	rm -rf "dist/$name"
done
(cd dist && sha256sum plumb_* > SHA256SUMS)
ls -l dist
