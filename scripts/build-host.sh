#!/usr/bin/env bash
# 框架侧宿主编排层：项目 build.sh 用 `source` 引入，然后调 szrw_invoke_build。
# （对应 build-host.ps1：szrw_invoke_build ↔ Invoke-SzrwBuild，同一组挂载/环境变量。）
# 不是独立脚本——参数化代价比 dot-source 大：项目只需要说四个事实。
#
# 这里只有「所有项目必须一致」的机制：
#   ① 关掉 BuildKit attestation（不需要 provenance/SBOM，省一层 manifest，也不依赖目标 registry 的 OCI artifact 支持）
#   ② 工具链镜像存在性检查与构建（构建也关 attestation；裸 docker build 会漂移）
#   ③ docker run 的三个固定挂载：repo / szrw / 缓存（+ 可选的多站点 node_modules）
#   ④ --load / --push 的选择，以及推送前对私有 registry 的登录（REGISTRY_USER / REGISTRY_PASSWORD）
#
# 项目只提供：镜像名、bake 文件、目标名、是否多站点 staging。
# 容器内的项目脚本看到的是固定路径与环境变量：
#   REPO_ROOT=/workspace/repo  WORKSPACE_DIR=/workspace  CODEGEN=/workspace/szrw
#   npm_config_cache / GOMODCACHE / GOCACHE 都在 /cache 卷里（一次构建只挂一个缓存卷）

SZRW_BUILDHOST_SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=frontends.sh
source "${SZRW_BUILDHOST_SCRIPTS_DIR}/frontends.sh"

SZRW_REPO_MOUNT='/workspace/repo'
SZRW_CODEGEN_MOUNT='/workspace/szrw'
SZRW_CACHE_MOUNT='/cache'

szrw_build_usage() {
    cat <<'EOF'
用法（被 build.sh source，然后调用）：
  szrw_invoke_build --repo-root <dir> [--codegen <szrw 仓>] [--builder-image <tag>] [--image <tag>]
                    (--entrypoint <容器内脚本相对路径> | --command <bash -c 命令>)
                    [--bake-file <hcl>] [--bake-target <name>] [--stage-sites] [--skip-push]
                    [-- <传给容器入口的参数...>]

  --entrypoint  容器内入口（相对仓库根，如 scripts/ci-build.sh）；--command 是它的替代
  --bake-file   给了才打包镜像；没给就只跑容器（生成/校验/编译）
  --image       只用于日志的镜像名（真正的 tag 在 bake 文件里），项目自己的 *_IMAGE
  --stage-sites 多站点：为每个站点挂 <prefix>-nm-<site>:/workspace/repo/web/<site>/node_modules
EOF
}

szrw_attestation_off() {
    # Bake HCL 无法可靠关掉默认 provenance，只能每次调用显式带上。
    export BUILDX_NO_DEFAULT_ATTESTATIONS=1
    export BUILDX_BAKE_FILE_RELATIVE_PATHS=1
}

szrw_buildx_available() {
    docker buildx version >/dev/null 2>&1
}

szrw_volume_prefix() {
    local repo="${1%/}" name
    name="$(basename "$repo" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9_.-]/-/g')"
    [[ -n "$name" ]] || name='szrw-project'
    printf '%s\n' "$name"
}

# 工具链镜像属于框架仓（<codegen>/Dockerfile.builder），项目不定义它，只在使用前确保存在。
# 注意这里必须是 buildx + 显式关 attestation：普通 docker build 在 BuildKit 下同样会追加
# provenance，构建出来的镜像一旦被 push 就会比 bake 出来的多一层 manifest。
szrw_ensure_builder() {
    local codegen="${1%/}" image="${2:-szrw-builder:latest}" dockerfile
    if docker image inspect "$image" >/dev/null 2>&1; then
        return 0
    fi
    dockerfile="${codegen}/Dockerfile.builder"
    [[ -f "$dockerfile" ]] || {
        echo "ERROR: 工具链镜像定义不存在: $dockerfile" >&2
        return 1
    }
    echo "Building framework toolchain image: $image"
    szrw_attestation_off
    if szrw_buildx_available; then
        docker buildx build --load --provenance=false --sbom=false \
            --platform linux/amd64 -f "$dockerfile" -t "$image" "$codegen"
    else
        docker build --platform linux/amd64 -f "$dockerfile" -t "$image" "$codegen"
    fi
}

szrw_run_container() {
    local repo="${1%/}" codegen="${2%/}" image="$3" entrypoint="$4" command="$5" stage_sites="$6"
    shift 6

    local prefix entry_args=() tail_args=() volume_args=()
    prefix="$(szrw_volume_prefix "$repo")"

    if [[ -n "$entrypoint" ]]; then
        entry_args=(--entrypoint "${SZRW_REPO_MOUNT}/${entrypoint#/}")
    elif [[ -n "$command" ]]; then
        # `-c` 是容器入口 bash 的参数，必须跟在镜像名后面，不能混进 docker run 的选项。
        entry_args=(--entrypoint bash)
        tail_args=(-c "$command")
    else
        echo "ERROR: 需要 entrypoint 或 command 之一" >&2
        return 1
    fi

    if [[ "$stage_sites" == true ]]; then
        szrw_frontends_verify "$repo"
        local name
        while read -r name _; do
            [[ -n "$name" ]] || continue
            # 卷挂在工程目录（web/<name>），不是产物目录：npm ci 要写的是前者。
            volume_args+=(-v "${prefix}-nm-${name}:${SZRW_REPO_MOUNT}/${SZRW_FRONTENDS_WEB_ROOT}/${name}/node_modules")
        done < <(szrw_frontends_list "$repo")
    fi

    echo "Running build in container: $image"
    docker run --rm \
        -e "REPO_ROOT=${SZRW_REPO_MOUNT}" \
        -e 'WORKSPACE_DIR=/workspace' \
        -e "CODEGEN=${SZRW_CODEGEN_MOUNT}" \
        -e "GOPROXY=${GOPROXY:-https://goproxy.cn,direct}" \
        -e "npm_config_cache=${SZRW_CACHE_MOUNT}/npm" \
        -e "GOMODCACHE=${SZRW_CACHE_MOUNT}/go-mod" \
        -e "GOCACHE=${SZRW_CACHE_MOUNT}/go-build" \
        -v "${repo}:${SZRW_REPO_MOUNT}" \
        -v "${codegen}:${SZRW_CODEGEN_MOUNT}" \
        -v "${prefix}-cache:${SZRW_CACHE_MOUNT}" \
        ${volume_args[@]+"${volume_args[@]}"} \
        "${entry_args[@]}" \
        "$image" ${tail_args[@]+"${tail_args[@]}"} "$@"
}

# 目标 registry 是自建的私有仓（registry.lmcl.xyz）：推送前需要凭据。
# 本机 docker login 一次即可；CI 用 REGISTRY_USER / REGISTRY_PASSWORD 自动登录。
szrw_registry_login() {
    local image="$1" skip_push="$2" registry_host
    [[ "$skip_push" == true || -z "$image" ]] && return 0
    # 没有 namespace 的短名（如 myimg:latest）走 Docker Hub，不需要登录
    [[ "$image" == */* ]] || return 0
    registry_host="${image%%/*}"
    # 首段必须是真正的主机名（带 . / : 或 localhost）
    [[ "$registry_host" == *.* || "$registry_host" == *:* || "$registry_host" == localhost ]] || return 0
    if [[ -n "${REGISTRY_USER:-}" && -n "${REGISTRY_PASSWORD:-}" ]]; then
        printf '%s' "$REGISTRY_PASSWORD" | docker login "$registry_host" -u "$REGISTRY_USER" --password-stdin >/dev/null || {
            echo "ERROR: docker login $registry_host 失败" >&2
            return 1
        }
    else
        echo "提示: 推送需要已登录 $registry_host（docker login $registry_host）"
    fi
}

# 在 bake 文件所在目录执行：HCL 里的 context = "." 相对它自己。
szrw_bake() {
    local bake_file="$1" target="${2:-runtime}" skip_push="$3"
    [[ -f "$bake_file" ]] || {
        echo "ERROR: bake 文件不存在: $bake_file" >&2
        return 1
    }
    local bake_args=(--load)
    if [[ "$skip_push" != true ]]; then
        bake_args+=(--push)
    fi
    if [[ -n "$target" ]]; then
        bake_args+=("$target")
    fi

    szrw_attestation_off
    (
        cd "$(dirname "$bake_file")" || exit 1
        docker buildx bake --file "$(basename "$bake_file")" \
            --provenance=false --sbom=false "${bake_args[@]}"
    )
}

szrw_invoke_build() {
    local repo_root='' codegen='' builder_image='szrw-builder:latest'
    local entrypoint='' command='' bake_file='' bake_target='runtime' image=''
    local stage_sites=false skip_push=false
    local container_args=()

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --repo-root)     repo_root="$2"; shift 2 ;;
            --codegen)       codegen="$2"; shift 2 ;;
            --builder-image) builder_image="$2"; shift 2 ;;
            --image)         image="$2"; shift 2 ;;
            --entrypoint)    entrypoint="$2"; shift 2 ;;
            --command)       command="$2"; shift 2 ;;
            --bake-file)     bake_file="$2"; shift 2 ;;
            --bake-target)   bake_target="$2"; shift 2 ;;
            --stage-sites)   stage_sites=true; shift ;;
            --skip-push)     skip_push=true; shift ;;
            -h|--help)       szrw_build_usage; return 0 ;;
            --)              shift; container_args=("$@"); break ;;
            *)
                echo "ERROR: 未知参数: $1（--help 看用法）" >&2
                return 1
                ;;
        esac
    done

    [[ -n "$repo_root" ]] || {
        echo "ERROR: 缺少 --repo-root" >&2
        return 1
    }
    repo_root="$(cd "$repo_root" && pwd)" || return 1
    if [[ -z "$codegen" ]]; then
        codegen="$(dirname "$repo_root")/szrw"
    fi
    [[ -d "$codegen" ]] || {
        echo "ERROR: 框架仓不存在: $codegen（同级要有 szrw 仓，或用 --codegen 指定）" >&2
        return 1
    }
    codegen="$(cd "$codegen" && pwd)"

    szrw_attestation_off
    szrw_ensure_builder "$codegen" "$builder_image" || return 1

    if [[ -n "$entrypoint" || -n "$command" ]]; then
        szrw_run_container "$repo_root" "$codegen" "$builder_image" \
            "$entrypoint" "$command" "$stage_sites" \
            ${container_args[@]+"${container_args[@]}"} || return 1
    fi

    if [[ -n "$bake_file" ]]; then
        szrw_registry_login "$image" "$skip_push" || return 1
        echo "Building image: ${image:-<bake 变量>} (target: ${bake_target})"
        szrw_bake "$bake_file" "$bake_target" "$skip_push" || return 1
        if [[ "$skip_push" == true ]]; then
            echo "Completed (push skipped): ${image:-<bake 变量>}"
        else
            echo "Completed! Image pushed: ${image:-<bake 变量>}"
        fi
    fi
    return 0
}
