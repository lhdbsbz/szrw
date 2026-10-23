#!/usr/bin/env bash
# 发布链的项目段：只给框架层四个事实——镜像名、bake 文件、目标名、是否多站点。
# 生成 / 编译 / 打包 / 推送的机制在 <codegen>/scripts/build-host.sh（与 build.ps1 共用）。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 框架仓位置：SZRW_CODEGEN > 「本仓就在框架仓里」（template/ 的情形）> 同级 szrw/（拷出去的新项目）。
if [[ -n "${SZRW_CODEGEN:-}" ]]; then
    CODEGEN="$SZRW_CODEGEN"
elif [[ -f "$(dirname "$SCRIPT_DIR")/scripts/build-host.sh" ]]; then
    CODEGEN="$(dirname "$SCRIPT_DIR")"
else
    CODEGEN="$(dirname "$SCRIPT_DIR")/szrw"
fi
FRAMEWORK="${CODEGEN}/scripts/build-host.sh"
if [[ ! -f "$FRAMEWORK" ]]; then
    echo "框架构建层缺失: $FRAMEWORK（同级要有 szrw 仓，或用 SZRW_CODEGEN 指定）" >&2
    exit 1
fi
# shellcheck source=/dev/null
source "$FRAMEWORK"

# 镜像变量用项目自己的名字：残留的通用 IMAGE 会把镜像推到别的仓。
export SZRW_TEMPLATE_IMAGE="${SZRW_TEMPLATE_IMAGE:-registry.cn-shanghai.aliyuncs.com/lhdshr/szrw-template:latest}"

SKIP_PUSH=false
case "${1:-}" in
    --skip-push) SKIP_PUSH=true ;;
    "") ;;
    *) echo "未知参数: $1（只接受 --skip-push）" >&2; exit 1 ;;
esac

# 容器里：生成（gen.sh 用框架挂载的 CODEGEN）→ 交叉编译出镜像要 COPY 的 main。
BUILD_COMMAND='bash scripts/gen.sh && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o main .'

RUN_ARGS=(--repo-root "$SCRIPT_DIR" --codegen "$CODEGEN" --command "$BUILD_COMMAND"
    --image "$SZRW_TEMPLATE_IMAGE"
    --bake-file "${SCRIPT_DIR}/docker-bake.hcl" --bake-target runtime)
if $SKIP_PUSH; then
    RUN_ARGS+=(--skip-push)
fi

szrw_invoke_build "${RUN_ARGS[@]}"
