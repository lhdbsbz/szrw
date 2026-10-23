# 发布链的项目段：只给框架层四个事实——镜像名、bake 文件、目标名、是否多站点。
# 生成 / 编译 / 打包 / 推送的机制在 <codegen>/scripts/build-host.ps1（与 build.sh 共用）。
[CmdletBinding()]
param(
    [switch]$SkipPush
)

$ErrorActionPreference = 'Stop'

$RepoRoot = $PSScriptRoot
# 框架仓位置：SZRW_CODEGEN > 「本仓就在框架仓里」（template/ 的情形）> 同级 szrw/（拷出去的新项目）。
$Codegen = if ($env:SZRW_CODEGEN) {
    $env:SZRW_CODEGEN
} elseif (Test-Path -LiteralPath (Join-Path (Split-Path $RepoRoot -Parent) 'scripts/build-host.ps1')) {
    Split-Path $RepoRoot -Parent
} else {
    Join-Path (Split-Path $RepoRoot -Parent) 'szrw'
}
$Framework = Join-Path $Codegen 'scripts/build-host.ps1'
if (-not (Test-Path -LiteralPath $Framework)) {
    throw "框架构建层缺失: $Framework（同级要有 szrw 仓，或用 SZRW_CODEGEN 指定）"
}
. $Framework

# 镜像变量用项目自己的名字：残留的通用 IMAGE 会把镜像推到别的仓。
$Image = if ($env:SZRW_TEMPLATE_IMAGE) { $env:SZRW_TEMPLATE_IMAGE } else { 'registry.lmcl.xyz/lhdshr/szrw-template:latest' }
$env:SZRW_TEMPLATE_IMAGE = $Image

# 容器里：生成（gen.sh 用框架挂载的 CODEGEN）→ 交叉编译出镜像要 COPY 的 main。
$BuildCommand = 'bash scripts/gen.sh && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o main .'

Invoke-SzrwBuild -RepoRoot $RepoRoot -Codegen $Codegen `
    -Command $BuildCommand `
    -Image $Image `
    -BakeFile (Join-Path $RepoRoot 'docker-bake.hcl') `
    -BakeTarget 'runtime' `
    -SkipPush:$SkipPush
