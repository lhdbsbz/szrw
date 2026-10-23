# 在框架工具链镜像里跑生成：宿主只需 Docker。
#
# protoc 调用层在框架仓（插件、固定选项、proto 契约校验、gen/ 原子替换），
# 这里只负责把仓库和框架源码挂进容器。
#
# 用法：
#   .\scripts\gen.ps1                # 生成；缺实现 = 失败
#   .\scripts\gen.ps1 -Scaffold      # 加 RPC 时补出骨架文件
param(
    [string]$Codegen = (Join-Path (Split-Path -Parent $PSScriptRoot) '..'),
    [string]$BuilderImage = 'szrw-builder:latest',
    [switch]$Scaffold
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent $PSScriptRoot
$Codegen = (Resolve-Path $Codegen).Path

$GenArgs = if ($Scaffold) { '--scaffold' } else { '' }

# 单行 bash 命令 + 参数数组：多行 here-string 或未加引号的变量会被 PowerShell
# 按空格拆成多个参数，docker 收到后 bash 只会看到 -c 后面缺参数。
# 用 `bash <脚本>` 而不是直接执行：Windows 检出的 generate.sh 没有 exec bit。
$Cmd = "set -e; cd /work; bash /workspace/szrw/scripts/generate.sh $GenArgs".Trim()

# 注意：不能叫 $Args，那是 PowerShell 的自动变量。
$DockerArgs = @(
    'run', '--rm', '--entrypoint', 'bash',
    # 插件安装要联网拉依赖；容器里没有本机 go env，所以显式传进去。
    '-e', "GOPROXY=$(if ($env:GOPROXY) { $env:GOPROXY } else { 'https://goproxy.cn,direct' })",
    '-v', "${RepoRoot}:/work",
    '-v', "${Codegen}:/workspace/szrw",
    '-w', '/work',
    $BuilderImage, '-c', $Cmd
)
& docker @DockerArgs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
