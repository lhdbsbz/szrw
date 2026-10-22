#!/usr/bin/env bash
# 生成全部 Go 代码。
#
# protoc 调用层在框架仓（插件、固定选项、proto 契约校验、gen/ 原子替换都在那里），
# 本仓只需要说清楚「这一次是生成还是补骨架」。
#
# 前置：szrw-builder 镜像里跑（推荐 ./scripts/gen.ps1），或本机有 protoc + 五个插件。
# 用法：
#   ./scripts/gen.sh              # 生成；缺实现 = 失败
#   ./scripts/gen.sh --scaffold   # 加 RPC 时补出骨架文件
set -euo pipefail
cd "$(dirname "$0")/.."

CODEGEN="${CODEGEN:-../szrw-codegen}"
[[ -x "${CODEGEN}/scripts/generate.sh" ]] || {
    echo "找不到框架生成脚本: ${CODEGEN}/scripts/generate.sh（用 CODEGEN=... 指定 szrw-codegen 路径）" >&2
    exit 1
}

"${CODEGEN}/scripts/generate.sh" "$@"
