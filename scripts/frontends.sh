#!/usr/bin/env bash
# 前端站点谓词的唯一 shell 实现（被 build-host.sh source；项目脚本不要自己再探测）。
#
# 规则与 Go 侧 internal/frontendgen/frontends.go 完全一致：
#   web/<name>/package.json 存在，且根目录或 apps/*/ 下有 vite.config.ts|js
#   → 站点名 = <name>，产物 = web/<name>/<name>
#
# 事实来源是生成器写的 gen/frontends.json（一站点一行）。清单缺失（还没跑过生成）
# 才回退到本文件的探测；两者不一致只说明「忘了跑生成」，不是配置项。
#
# 输出格式（名字与产物之间一个空格；目录名被约束成 [a-z0-9-]，不含空格）：
#   admin web/admin/admin
#   story web/story/story

SZRW_FRONTENDS_WEB_ROOT='web'
SZRW_FRONTENDS_MANIFEST='gen/frontends.json'

# 站点名 = 目录名 = 子域名：小写字母/数字，中划线只能夹在中间，≤63 字符。
# 与 Go 的 frontendgen.IsSiteName 同一规则。
szrw_frontends_is_site_name() {
    local name="$1"
    [[ -n "$name" && ${#name} -le 63 ]] || return 1
    [[ "$name" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]
}

# 单个 web/<name> 是否是前端站点。用 vite.config 判、不用 package.json 的 name：
# web/kit 这类库也有 package.json，按 name/scripts 判会把库当站点。
szrw_frontends_is_frontend_dir() {
    local dir="${1%/}"
    [[ -f "${dir}/package.json" ]] || return 1
    [[ -f "${dir}/vite.config.ts" || -f "${dir}/vite.config.js" ]] && return 0
    compgen -G "${dir}/apps/*/vite.config.ts" >/dev/null && return 0
    compgen -G "${dir}/apps/*/vite.config.js" >/dev/null && return 0
    return 1
}

szrw_frontends_manifest_path() {
    printf '%s/%s\n' "${1%/}" "$SZRW_FRONTENDS_MANIFEST"
}

# 读生成器写的清单。清单不存在或版本不认识时返回非零（调用方回退到探测）。
szrw_frontends_from_manifest() {
    local repo="${1%/}" manifest
    manifest="$(szrw_frontends_manifest_path "$repo")"
    [[ -f "$manifest" ]] || return 1
    grep -q '"version": 1,' "$manifest" 2>/dev/null || return 1
    sed -n 's/^ *{"name":"\([a-z0-9-]*\)","dir":"[^"]*","out":"\([^"]*\)"},\{0,1\}$/\1 \2/p' "$manifest"
    return 0
}

# 探测：只在没有清单时用（例如刚 clone、还没跑生成）。
szrw_frontends_probe() {
    local repo="${1%/}" dir name web_root
    web_root="${repo}/${SZRW_FRONTENDS_WEB_ROOT}"
    for dir in "${web_root}"/*; do
        [[ -d "$dir" ]] || continue
        name="$(basename "$dir")"
        szrw_frontends_is_site_name "$name" || continue
        szrw_frontends_is_frontend_dir "$dir" || continue
        printf '%s %s\n' "$name" "${SZRW_FRONTENDS_WEB_ROOT}/${name}/${name}"
    done | LC_ALL=C sort
}

# 站点清单：清单优先，缺失时探测。每行 "<name> <产物相对路径>"，已排序。
szrw_frontends_list() {
    local repo="${1%/}"
    if szrw_frontends_from_manifest "$repo" 2>/dev/null; then
        return 0
    fi
    szrw_frontends_probe "$repo"
}

# 清单与磁盘不一致时警告：宿主侧规划卷发生在生成之前，新增站点会漏卷。
szrw_frontends_verify() {
    local repo="${1%/}" from_manifest from_disk
    szrw_frontends_from_manifest "$repo" >/dev/null 2>&1 || return 0
    from_manifest="$(szrw_frontends_from_manifest "$repo")"
    from_disk="$(szrw_frontends_probe "$repo")"
    if [[ "$from_manifest" != "$from_disk" ]]; then
        echo "WARNING: $(szrw_frontends_manifest_path "$repo") 与磁盘上的站点不一致——先跑一次生成（gen）" >&2
    fi
    return 0
}
