#!/usr/bin/env bash
# 构建框架工具链镜像。产物：szrw-builder:latest（本地，不入 ACR）。
# 用法：./scripts/build-builder.sh [tag]
set -euo pipefail

cd "$(dirname "$0")/.."
TAG="${1:-szrw-builder:latest}"

# 阿里云 ACR 个人版拒绝 BuildKit 的 OCI attestation；这个镜像是本地用的，
# 但保持和产品镜像一致的开关，避免有人 push 时踩同一个坑。
BUILDX_NO_DEFAULT_ATTESTATIONS=1 \
    docker buildx build --load \
    --provenance=false --sbom=false \
    --platform linux/amd64 \
    -f Dockerfile.builder \
    -t "$TAG" \
    .

echo "built: $TAG"
