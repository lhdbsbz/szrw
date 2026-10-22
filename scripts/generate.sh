#!/usr/bin/env bash
# 框架统一的 protoc 调用层。
#
# 项目不需要各写一遍：proto 根、输出目录、五个插件与它们的固定选项、gen/ 的
# 原子替换，全在这里。项目侧只剩「什么时候跑」（构建脚本一行）。
#
# 用法（cwd = 项目根，必须有 proto/ 与 codegen.yaml）：
#   <codegen>/scripts/generate.sh                    # 生成
#   <codegen>/scripts/generate.sh --check             # 只校验：跑一遍然后丢弃产物（CI 用）
#   <codegen>/scripts/generate.sh --scaffold          # 顺带补出缺失的实现骨架（本地加 RPC）
#
# 固定不变的部分（写死在这里，不可配）：
#   proto 根 = proto/          第三方 include = proto_third/
#   Go 产物   = gen/           前端/接线 = 项目根（codegen.yaml 决定具体目录）
#   plugin  = protoc-gen-go / -go-grpc / -go-json / -grpc-gateway / -szrw
set -euo pipefail

CODEGEN_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(pwd)"

CHECK=false
SCAFFOLD=false
for arg in "$@"; do
    case "$arg" in
        --check)    CHECK=true ;;
        --scaffold) SCAFFOLD=true ;;
        *) echo "未知参数: $arg（只接受 --check / --scaffold）" >&2; exit 1 ;;
    esac
done

fail() { echo "ERROR: $*" >&2; exit 1; }

[[ -f "$ROOT/codegen.yaml" ]] || fail "项目根缺少 codegen.yaml（cwd=$ROOT）"
[[ -d "$ROOT/proto" ]]        || fail "项目根缺少 proto/（cwd=$ROOT）"
for tool in go protoc protoc-gen-go protoc-gen-go-grpc protoc-gen-go-json protoc-gen-grpc-gateway; do
    command -v "$tool" >/dev/null 2>&1 || fail "缺少 $tool：请用 szrw-builder 镜像跑生成（见 README）"
done

# 框架插件从本仓源码安装：改生成器不必先发布，项目也不用自己 go install。
# GOPROXY 只在未设置时给个默认（国内直连 proxy.golang.org 会挂，跟各项目 build 脚本一致）。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
( cd "$CODEGEN_SRC" && go install ./cmd/protoc-gen-szrw-codegen )
export PATH="$(go env GOPATH)/bin:${PATH}"

PROTO_FILES=()
while IFS= read -r f; do PROTO_FILES+=("$f"); done \
    < <(find "$ROOT/proto" -name '*.proto' -type f | LC_ALL=C sort)
[[ ${#PROTO_FILES[@]} -eq 0 ]] && fail "proto/ 下没有 .proto 文件"

INCLUDES=(--proto_path="$ROOT")
[[ -d "$ROOT/proto_third" ]] && INCLUDES+=(--proto_path="$ROOT/proto_third")

OPT="config=$ROOT/codegen.yaml"
$SCAFFOLD && OPT="$OPT,scaffold=write"

# gen/ 原子替换：先挪到 gen.prev，成功才丢弃；失败原样换回。
# 为什么不是「先删再生成」：生成失败时仓库看起来像被删空了，真正的错误会被
# 一堆「找不到符号」的编译错误埋掉。
GEN="$ROOT/gen"
PREV="$ROOT/gen.prev"
begin_gen()   { rm -rf "$PREV"; [[ -d "$GEN" ]] && mv "$GEN" "$PREV"; mkdir -p "$GEN"; }
commit_gen()  { rm -rf "$PREV"; }
restore_gen() { rm -rf "$GEN"; [[ -d "$PREV" ]] && mv "$PREV" "$GEN"; return 0; }
trap '[[ -d "$PREV" ]] && restore_gen || true' EXIT

begin_gen
if ! protoc \
    "${INCLUDES[@]}" \
    "--go_out=$GEN"            "--go_opt=paths=source_relative" \
    "--go-grpc_out=$GEN"       "--go-grpc_opt=paths=source_relative" \
    "--go-json_out=$GEN"       "--go-json_opt=paths=source_relative,allow_unknown=true,emit_defaults=true" \
    "--grpc-gateway_out=$GEN"  "--grpc-gateway_opt=paths=source_relative" \
    "--szrw-codegen_out=$ROOT" "--szrw-codegen_opt=$OPT" \
    "${PROTO_FILES[@]}"
then
    restore_gen
    fail "生成失败；gen/ 已还原（proto 规则见 szrw/internal/validate）"
fi

if $CHECK; then
    restore_gen
    echo "OK: proto 契约与生成均通过（--check 丢弃产物）"
    exit 0
fi

commit_gen
echo "generated: gen/（以及 codegen.yaml 声明的 frontend_api.targets）"
