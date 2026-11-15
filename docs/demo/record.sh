#!/usr/bin/env bash
# Records docs/demo.gif in Docker (the machine only needs Docker and Go).
#   ./docs/demo/record.sh
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
demo=$root/docs/demo

# A Linux binary for the same architecture as Docker.
arch=$(docker info --format '{{.Architecture}}')
case $arch in aarch64 | arm64) goarch=arm64 ;; *) goarch=amd64 ;; esac
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$demo/holdfast" ./cmd/holdfast)
trap 'rm -f "$demo/holdfast"' EXIT

docker build -q -t holdfast-demo "$demo"
docker run --rm -v "$root:/vhs" -w /vhs holdfast-demo docs/demo.tape
echo "Done: $root/docs/demo.gif"
