# 框架侧宿主编排层：项目 build.ps1 用 `. <path>` 引入，然后调 Invoke-SzrwBuild。
# 不是独立脚本——参数化代价比 dot-source 大：项目只需要说四个事实。
#
# 与 scripts/build-host.sh 行为一致（同一组函数、同一组挂载与环境变量）；
# 函数对应：Invoke-SzrwBuild ↔ szrw_invoke_build，Invoke-SzrwBake ↔ szrw_bake，
#           Build-SzrwBuilderImage ↔ szrw_ensure_builder，Invoke-SzrwContainer ↔ szrw_run_container。
# 改这里必须同一次改那边。
#
# 这里只有「所有项目必须一致」的机制：
#   ① 关掉 BuildKit attestation（ACR 个人版拒绝 OCI artifact，push 会在层上传完后失败）
#   ② 工具链镜像存在性检查与构建（构建也关 attestation；裸 docker build 会漂移）
#   ③ docker run 的三个固定挂载：repo / szrw / 缓存（+ 可选的多站点 node_modules）
#   ④ --load / --push 的选择
#
# 容器内的项目脚本看到的是固定路径与环境变量：
#   REPO_ROOT=/workspace/repo  WORKSPACE_DIR=/workspace  CODEGEN=/workspace/szrw
#   npm_config_cache / GOMODCACHE / GOCACHE 都在 /cache 卷里（一次构建只挂一个缓存卷）

$script:SzrwRepoMount = '/workspace/repo'
$script:SzrwCodegenMount = '/workspace/szrw'
$script:SzrwCacheMount = '/cache'
$script:SzrwScriptsDir = $PSScriptRoot

function Get-SzrwBuildHelp {
    @'
用法（被 build.ps1 dot-source，然后调用）：
  Invoke-SzrwBuild -RepoRoot <dir> [-Codegen <szrw 仓>] [-BuilderImage <tag>] [-Image <tag>]
                   (-Entrypoint <容器内脚本相对路径> | -Command <bash -c 命令>)
                   [-BakeFile <hcl>] [-BakeTarget <name>] [-StageSites] [-SkipPush]
                   [-ContainerArgs <传给容器入口的参数...>]

  -Entrypoint   容器内入口（相对仓库根，如 scripts/ci-build.sh）；-Command 是它的替代
  -BakeFile     给了才打包镜像；没给就只跑容器（生成/校验/编译）
  -Image        只用于日志的镜像名（真正的 tag 在 bake 文件里），项目自己的 *_IMAGE
  -StageSites   多站点：为每个站点挂 <prefix>-nm-<site>:/workspace/repo/web/<site>/node_modules
'@
}

function Enable-SzrwAttestationOff {
    # Bake HCL 无法可靠关掉默认 provenance，只能每次调用显式带上。
    $env:BUILDX_NO_DEFAULT_ATTESTATIONS = '1'
    $env:BUILDX_BAKE_FILE_RELATIVE_PATHS = '1'
}

function Test-SzrwBuildx {
    & docker buildx version *> $null
    return $LASTEXITCODE -eq 0
}

function Get-SzrwVolumePrefix {
    param([Parameter(Mandatory = $true)][string]$RepoRoot)
    $name = (Split-Path $RepoRoot -Leaf).ToLowerInvariant() -replace '[^a-z0-9_.-]', '-'
    if (-not $name) { $name = 'szrw-project' }
    return $name
}

# 站点清单：清单优先（生成器写的 gen/frontends.json），缺失时按同一谓词探测。
# 与 scripts/frontends.sh 同一规则，改规则要同时改 internal/frontendgen。
function Get-SzrwFrontendSites {
    param([Parameter(Mandatory = $true)][string]$RepoRoot)

    $manifest = Join-Path $RepoRoot 'gen/frontends.json'
    if (Test-Path -LiteralPath $manifest) {
        try {
            $data = Get-Content -LiteralPath $manifest -Raw -Encoding UTF8 | ConvertFrom-Json
            if ($data.version -eq 1) {
                return @($data.sites | ForEach-Object {
                        [pscustomobject]@{ Name = $_.name; Out = $_.out }
                    })
            }
        } catch {
            # 清单坏了就当没有：下面用探测结果，构建不会因为生成物损坏而停。
        }
    }

    $webRoot = Join-Path $RepoRoot 'web'
    if (-not (Test-Path -LiteralPath $webRoot)) { return @() }

    return @(
        Get-ChildItem -LiteralPath $webRoot -Directory |
            Where-Object {
                $_.Name -cmatch '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$' -and
                (Test-Path -LiteralPath (Join-Path $_.FullName 'package.json')) -and
                (
                    (Test-Path -LiteralPath (Join-Path $_.FullName 'vite.config.ts')) -or
                    (Test-Path -LiteralPath (Join-Path $_.FullName 'vite.config.js')) -or
                    @(
                        Get-ChildItem -LiteralPath (Join-Path $_.FullName 'apps') -Directory -ErrorAction SilentlyContinue |
                            Where-Object {
                                (Test-Path -LiteralPath (Join-Path $_.FullName 'vite.config.ts')) -or
                                (Test-Path -LiteralPath (Join-Path $_.FullName 'vite.config.js'))
                            }
                    ).Count -gt 0
                )
            } |
            Sort-Object Name |
            ForEach-Object {
                [pscustomobject]@{ Name = $_.Name; Out = "web/$($_.Name)/$($_.Name)" }
            }
    )
}

# 工具链镜像属于框架仓（<Codegen>/Dockerfile.builder），项目不定义它，只在使用前确保存在。
# 注意这里必须是 buildx + 显式关 attestation：普通 docker build 在 BuildKit 下同样会追加
# provenance，构建出来的镜像一旦被 push 就会踩 ACR 的同一个坑。
function Build-SzrwBuilderImage {
    param(
        [Parameter(Mandatory = $true)][string]$Codegen,
        [string]$BuilderImage = 'szrw-builder:latest'
    )
    & docker image inspect $BuilderImage *> $null
    if ($LASTEXITCODE -eq 0) { return }

    $dockerfile = Join-Path $Codegen 'Dockerfile.builder'
    if (-not (Test-Path -LiteralPath $dockerfile)) {
        throw "工具链镜像定义不存在: $dockerfile"
    }
    Write-Host "Building framework toolchain image: $BuilderImage" -ForegroundColor Cyan
    Enable-SzrwAttestationOff
    if (Test-SzrwBuildx) {
        & docker buildx build --load --provenance=false --sbom=false `
            --platform linux/amd64 -f $dockerfile -t $BuilderImage $Codegen
    } else {
        & docker build --platform linux/amd64 -f $dockerfile -t $BuilderImage $Codegen
    }
    if ($LASTEXITCODE -ne 0) { throw "docker build failed (exit $LASTEXITCODE)" }
}

function Invoke-SzrwContainer {
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot,
        [Parameter(Mandatory = $true)][string]$Codegen,
        [Parameter(Mandatory = $true)][string]$BuilderImage,
        [string]$Entrypoint,
        [string]$Command,
        [switch]$StageSites,
        [string[]]$ContainerArgs = @()
    )

    $prefix = Get-SzrwVolumePrefix -RepoRoot $RepoRoot
    $volumeArgs = @(
        '-v', "${RepoRoot}:$script:SzrwRepoMount",
        '-v', "${Codegen}:$script:SzrwCodegenMount",
        '-v', "${prefix}-cache:$script:SzrwCacheMount"
    )
    if ($StageSites) {
        Get-SzrwFrontendSites -RepoRoot $RepoRoot | ForEach-Object {
            # 卷挂在工程目录（web/<name>），不是产物目录：npm ci 要写的是前者。
            $volumeArgs += @('-v', "${prefix}-nm-$($_.Name):$script:SzrwRepoMount/web/$($_.Name)/node_modules")
        }
    }

    $goproxy = if ($env:GOPROXY) { $env:GOPROXY } else { 'https://goproxy.cn,direct' }
    $envArgs = @(
        '-e', "REPO_ROOT=$script:SzrwRepoMount",
        '-e', 'WORKSPACE_DIR=/workspace',
        '-e', "CODEGEN=$script:SzrwCodegenMount",
        '-e', "GOPROXY=$goproxy",
        '-e', "npm_config_cache=$script:SzrwCacheMount/npm",
        '-e', "GOMODCACHE=$script:SzrwCacheMount/go-mod",
        '-e', "GOCACHE=$script:SzrwCacheMount/go-build"
    )

    $entryArgs = @()
    $tailArgs = @()
    if ($Entrypoint) {
        $entryArgs = @('--entrypoint', "$script:SzrwRepoMount/$($Entrypoint -replace '^/', '')")
    } elseif ($Command) {
        # `-c` 是容器入口 bash 的参数，必须跟在镜像名后面，不能混进 docker run 的选项。
        $entryArgs = @('--entrypoint', 'bash')
        $tailArgs = @('-c', $Command)
    } else {
        throw '需要 Entrypoint 或 Command 之一'
    }

    Write-Host "Running build in container: $BuilderImage" -ForegroundColor Cyan
    $dockerArgs = @('run', '--rm') + $envArgs + $volumeArgs + $entryArgs + @($BuilderImage) + $tailArgs + $ContainerArgs
    & docker @dockerArgs
    if ($LASTEXITCODE -ne 0) { throw "Container build failed (exit $LASTEXITCODE)" }
}

# 在 bake 文件所在目录执行：HCL 里的 context = "." 相对它自己。
function Invoke-SzrwBake {
    param(
        [Parameter(Mandatory = $true)][string]$BakeFile,
        [string]$BakeTarget = 'runtime',
        [switch]$SkipPush
    )
    if (-not (Test-Path -LiteralPath $BakeFile)) { throw "bake 文件不存在: $BakeFile" }

    $bakeArgs = @('--load')
    if (-not $SkipPush) { $bakeArgs += '--push' }
    if ($BakeTarget) { $bakeArgs += $BakeTarget }

    Enable-SzrwAttestationOff
    Push-Location (Split-Path $BakeFile -Parent)
    try {
        & docker buildx bake --file (Split-Path $BakeFile -Leaf) `
            --provenance=false --sbom=false @bakeArgs
        if ($LASTEXITCODE -ne 0) { throw "docker buildx bake failed (exit $LASTEXITCODE)" }
    } finally {
        Pop-Location
    }
}

function Invoke-SzrwBuild {
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot,
        [string]$Codegen,
        [string]$BuilderImage = 'szrw-builder:latest',
        [string]$Image,
        [string]$Entrypoint,
        [string]$Command,
        [string[]]$ContainerArgs = @(),
        [string]$BakeFile,
        [string]$BakeTarget = 'runtime',
        [switch]$StageSites,
        [switch]$SkipPush
    )

    if (-not (Test-Path -LiteralPath $RepoRoot)) { throw "仓库不存在: $RepoRoot" }
    $RepoRoot = (Resolve-Path -LiteralPath $RepoRoot).Path
    if (-not $Codegen) { $Codegen = Join-Path (Split-Path $RepoRoot -Parent) 'szrw' }
    if (-not (Test-Path -LiteralPath $Codegen)) {
        throw "框架仓不存在: $Codegen（同级要有 szrw 仓，或用 -Codegen 指定）"
    }
    $Codegen = (Resolve-Path -LiteralPath $Codegen).Path

    Enable-SzrwAttestationOff
    Build-SzrwBuilderImage -Codegen $Codegen -BuilderImage $BuilderImage

    if ($Entrypoint -or $Command) {
        Invoke-SzrwContainer -RepoRoot $RepoRoot -Codegen $Codegen -BuilderImage $BuilderImage `
            -Entrypoint $Entrypoint -Command $Command -StageSites:$StageSites `
            -ContainerArgs $ContainerArgs
    }

    if ($BakeFile) {
        $label = if ($Image) { $Image } else { '<bake 变量>' }
        Write-Host "Building image: $label (target: $BakeTarget)" -ForegroundColor Cyan
        Invoke-SzrwBake -BakeFile $BakeFile -BakeTarget $BakeTarget -SkipPush:$SkipPush
        if ($SkipPush) {
            Write-Host "Completed (push skipped): $label" -ForegroundColor Green
        } else {
            Write-Host "Completed! Image pushed: $label" -ForegroundColor Green
        }
    }
}
