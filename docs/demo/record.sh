#!/usr/bin/env bash
# Записывает docs/demo.gif в Docker (на машине нужен только Docker).
#   ./docs/demo/record.sh
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
demo=$root/docs/demo

# Бинарник под Linux той же архитектуры, что и Docker.
arch=$(docker info --format '{{.Architecture}}')
case $arch in aarch64 | arm64) goarch=arm64 ;; *) goarch=amd64 ;; esac
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$demo/holdfast" ./cmd/holdfast)
trap 'rm -f "$demo/holdfast"' EXIT

docker build -q -t holdfast-demo "$demo"
docker run --rm -v "$root:/vhs" -w /vhs holdfast-demo docs/demo.tape
echo "Готово: $root/docs/demo.gif"
