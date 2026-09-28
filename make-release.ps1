# ============================================================
# EdgeLite Gateway (Go Edition) 发布打包脚本
# 参考: DBBridge/make-release.ps1
# 用法: powershell -ExecutionPolicy Bypass -File make-release.ps1
#       powershell -ExecutionPolicy Bypass -File make-release.ps1 -Version 2.0.0
#       powershell -ExecutionPolicy Bypass -File make-release.ps1 -Targets arm7,amd64,arm64
# 产出: release/edgelite-<version>-linux-<arch>.tar.gz
#       (二进制 + 前端资源 + 默认配置 + install.sh + uninstall.sh + README)
#
# 流程:
#   1. 构建前端 (Vue3 -> frontend/dist/)
#   2. 交叉编译 Linux 多平台二进制 (CGO_ENABLED=0 纯 Go, 无 C 依赖)
#   3. 打包发布 tar.gz (嵌入板无 unzip 也有 tar)
#   4. 生成 SHA256 校验文件
# ============================================================

param(
    [string]$Version = "1.0.0",
    [string]$Targets = "arm7,amd64,arm64"
)

$ErrorActionPreference = "Stop"
$ProjectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $ProjectRoot

Write-Host ""
Write-Host "==========================================" -ForegroundColor Cyan
Write-Host "  EdgeLite Gateway v$Version Release Builder" -ForegroundColor Cyan
Write-Host "  工业物联网网关 (Go Edition)" -ForegroundColor Cyan
Write-Host "==========================================" -ForegroundColor Cyan
Write-Host ""

# ============================================================
# 1. 构建前端
# ============================================================
Write-Host "[1/4] Building frontend..." -ForegroundColor Green
Push-Location frontend

if (-not (Test-Path "node_modules")) {
    Write-Host "  Installing npm dependencies..." -ForegroundColor DarkGray
    $npmInstall = Start-Process -FilePath "cmd.exe" -ArgumentList "/c npm install" -NoNewWindow -Wait -PassThru
    if ($npmInstall.ExitCode -ne 0) {
        Write-Host "ERROR: npm install failed (exit $($npmInstall.ExitCode))" -ForegroundColor Red
        Pop-Location; exit 1
    }
}

Write-Host "  Building Vue3 frontend..." -ForegroundColor DarkGray
$viteBuild = Start-Process -FilePath "cmd.exe" -ArgumentList "/c npx vite build" -NoNewWindow -Wait -PassThru
if ($viteBuild.ExitCode -ne 0 -or -not (Test-Path "dist/index.html")) {
    Write-Host "  First vite build failed (exit $($viteBuild.ExitCode)), retrying..." -ForegroundColor Yellow
    Start-Sleep -Seconds 1
    $viteBuild = Start-Process -FilePath "cmd.exe" -ArgumentList "/c npx vite build" -NoNewWindow -Wait -PassThru
}
if (-not (Test-Path "dist/index.html")) {
    Write-Host "ERROR: Frontend build failed - dist/index.html not found" -ForegroundColor Red
    Write-Host "  Try running manually: cd frontend && npm run build" -ForegroundColor Yellow
    Pop-Location; exit 1
}
Pop-Location
Write-Host "  Frontend built OK" -ForegroundColor DarkGray

# ============================================================
# 2. 交叉编译 Linux 二进制
#    - 全部使用 CGO_ENABLED=0 纯 Go 静态编译（modernc.org/sqlite 纯 Go 驱动）
#    - arm7  = linux/arm GOARM=7，适用 Cortex-A7/A8/A9 等 ARMv7 设备
#    - amd64 = x86_64 服务器/工控机
#    - arm64 = Cortex-A53/A72 等64位设备
# ============================================================
Write-Host "[2/4] Building Linux binaries..." -ForegroundColor Green

$distDir = "dist"
New-Item -ItemType Directory -Path $distDir -Force | Out-Null

$buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$gitCommit = "unknown"
try { $gitCommit = (git rev-parse --short HEAD 2>$null).Trim() } catch {}
$ldflags = "-s -w -X main.Version=$Version"

$binaries = @{}
$targetList = $Targets.Split(",") | ForEach-Object { $_.Trim().ToLower() }

function Build-LinuxBinary($tag, $envVars) {
    foreach ($kv in $envVars.GetEnumerator()) { Set-Item -Path "env:$($kv.Key)" -Value $kv.Value }
    Set-Item -Path "env:CGO_ENABLED" -Value "0"
    Set-Item -Path "env:GOPROXY" -Value "https://goproxy.cn,direct"
    $out = "$distDir/edgelite-linux-$tag"
    Write-Host "  Building linux/$tag..." -ForegroundColor DarkGray
    $prevEAP = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    & go build -ldflags="$ldflags" -o $out ./cmd/edgelite 2>&1 | Out-Null
    $buildExit = $LASTEXITCODE; $ErrorActionPreference = $prevEAP
    if ($buildExit -ne 0 -or -not (Test-Path $out)) {
        Write-Host "ERROR: Build failed for linux-$tag (exit $buildExit)" -ForegroundColor Red
        exit 1
    }
    $script:binaries["edgelite-linux-$tag"] = $out
    Write-Host ("    -> edgelite-linux-{0} ({1} MB)" -f $tag, [math]::Round((Get-Item $out).Length / 1MB, 1)) -ForegroundColor Yellow
}

$allTargets = @{
    "arm7" = @{ GOOS = "linux"; GOARCH = "arm"; GOARM = "7" }
    "amd64" = @{ GOOS = "linux"; GOARCH = "amd64" }
    "arm64" = @{ GOOS = "linux"; GOARCH = "arm64" }
}
foreach ($t in $targetList) {
    if (-not $allTargets.ContainsKey($t)) {
        Write-Host "ERROR: Unknown target '$t' (valid: arm7, amd64, arm64)" -ForegroundColor Red
        exit 1
    }
    Build-LinuxBinary -tag $t -envVars $allTargets[$t]
}
# 清理交叉编译环境变量
$env:GOOS = ""; $env:GOARCH = ""; $env:GOARM = ""; $env:CGO_ENABLED = ""
Write-Host "  All binaries built OK" -ForegroundColor DarkGray

# ============================================================
# 3. 打包发布 tar.gz
# ============================================================
Write-Host "[3/4] Creating release packages..." -ForegroundColor Green

$releaseDir = "release"
New-Item -ItemType Directory -Path $releaseDir -Force | Out-Null

foreach ($t in $targetList) {
    $binKey = "edgelite-linux-$t"
    $stageDir = "$releaseDir/edgelite-$t-stage"
    # 包内顶层目录：解压后不会散落文件，且条目名不带 ./ 前缀（部分解压工具不识别）
    $topDir = "edgelite-$Version"
    $pkgRoot = "$stageDir/$topDir"
    New-Item -ItemType Directory -Path $pkgRoot -Force | Out-Null

    # 二进制（tar 内统一命名 edgelite）
    Copy-Item $binaries[$binKey] "$pkgRoot/edgelite" -Force

    # 前端资源：robocopy /MIR 镜像复制（幂等，重跑安全；/XD 排除开发残留）
    # robocopy 退出码 0-7 均为成功，>=8 才是失败
    robocopy "frontend/dist" "$pkgRoot/frontend" /MIR /XD logo-proposals node_modules .vite /NFL /NDL /NJH /NJS /NP | Out-Null
    if ($LASTEXITCODE -ge 8) {
        Write-Host "ERROR: robocopy failed for frontend/dist (exit $LASTEXITCODE)" -ForegroundColor Red
        exit 1
    }

    # 默认配置
    if (Test-Path "configs/config.yaml") {
        New-Item -ItemType Directory -Path "$pkgRoot/configs" -Force | Out-Null
        Copy-Item "configs/config.yaml" "$pkgRoot/configs/config.yaml" -Force
    }

    # 安装/卸载脚本与中文部署说明（替代面向开发的仓库 README）
    Copy-Item "deploy/install.sh" "$pkgRoot/install.sh" -Force
    Copy-Item "deploy/uninstall.sh" "$pkgRoot/uninstall.sh" -Force
    if (Test-Path "deploy/README-release.md") {
        Copy-Item "deploy/README-release.md" "$pkgRoot/README.md" -Force
    } elseif (Test-Path "README.md") {
        Copy-Item "README.md" "$pkgRoot/README.md" -Force
    }

    # tar.gz：必须用 GNU tar 并显式指定 Unix 权限。
    # Windows 自带的 bsdtar 会把所有条目打成 rw-rw-rw-（无可执行位），
    # 客户解压后 ./edgelite 无法直接运行，属于不合格交付。
    $tarball = "$releaseDir/edgelite-$Version-linux-$t.tar.gz"
    if (Test-Path $tarball) { Remove-Item $tarball -Force }
    Write-Host "  Creating $tarball..." -ForegroundColor DarkGray
    $gnuTar = $null
    $tarCandidates = @(
        (Get-Command tar -ErrorAction SilentlyContinue).Source,
        "$env:ProgramW6432\Git\usr\bin\tar.exe",
        "$env:ProgramFiles\Git\usr\bin\tar.exe",
        "D:\Program Files\Git\usr\bin\tar.exe"
    ) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -Unique
    foreach ($tc in $tarCandidates) {
        $verOut = & $tc --version 2>$null | Select-Object -First 1
        if ($verOut -match "GNU tar") { $gnuTar = $tc; break }
    }
    if ($gnuTar) {
        # --mode=755: 二进制/脚本可执行，目录/静态资源 755 也无害。
        # 注意: GNU tar -z 需要派生 gzip 子进程，其所在目录必须临时加入 PATH，
        # 否则在纯 PowerShell 环境下 gzip 找不到会导致 status 127 / Broken pipe。
        $gnuDir = Split-Path $gnuTar
        $prevPath = $env:PATH
        $env:PATH = "$gnuDir;$env:PATH"
        & $gnuTar -czf $tarball --mode=755 --owner=0 --group=0 -C $stageDir $topDir
        $tarExit = $LASTEXITCODE
        $env:PATH = $prevPath
    } else {
        Write-Host "  WARNING: 未找到 GNU tar，回退 bsdtar（包内无可执行位，依赖 install.sh 内 install -m 755 兜底）" -ForegroundColor Yellow
        & tar -czf $tarball -C $stageDir $topDir
        $tarExit = $LASTEXITCODE
    }
    if ($tarExit -ne 0 -or -not (Test-Path $tarball)) {
        Write-Host "ERROR: tar failed for $t" -ForegroundColor Red
        exit 1
    }
    Write-Host ("    -> {0} ({1} MB)" -f $tarball, [math]::Round((Get-Item $tarball).Length / 1MB, 1)) -ForegroundColor Yellow
}

# ============================================================
# 4. 生成 SHA256 校验文件
# ============================================================
Write-Host "[4/4] Generating checksums..." -ForegroundColor Green
$hashLines = Get-ChildItem $releaseDir -Filter "edgelite-$Version-*.tar.gz" | ForEach-Object {
    $hash = (Get-FileHash $_.FullName -Algorithm SHA256).Hash
    "$hash  $($_.Name)"
}
$hashLines | Out-File -Encoding ASCII -FilePath "$releaseDir/checksums.txt"

# ============================================================
# 输出结果
# ============================================================
Write-Host ""
Write-Host "==========================================" -ForegroundColor Green
Write-Host "  Release packages created:" -ForegroundColor Green
Write-Host "==========================================" -ForegroundColor Green
Get-ChildItem $releaseDir -Filter "edgelite-$Version-*.tar.gz" | ForEach-Object {
    $size = "$([math]::Round($_.Length/1MB,1)) MB"
    Write-Host "  release/$($_.Name)  ($size)" -ForegroundColor White
}
Write-Host ""
Write-Host "Each package contains:" -ForegroundColor Cyan
Write-Host "  $topDir/           <- 顶层目录" -ForegroundColor DarkGray
Write-Host "    edgelite       <- binary (静态编译, 无运行时依赖)" -ForegroundColor DarkGray
Write-Host "  frontend/      <- Web 控制台 (Vue3)" -ForegroundColor DarkGray
Write-Host "  configs/       <- 默认配置 config.yaml" -ForegroundColor DarkGray
Write-Host "  install.sh     <- 一键安装 (含 systemd/sysvinit 服务注册)" -ForegroundColor DarkGray
Write-Host "  uninstall.sh   <- 卸载脚本" -ForegroundColor DarkGray
Write-Host "  README.md      <- 文档" -ForegroundColor DarkGray
Write-Host ""
Write-Host "设备端部署 (Cortex-A7 / ARMv7):" -ForegroundColor Yellow
Write-Host "  1. 上传 edgelite-$Version-linux-arm7.tar.gz 到设备" -ForegroundColor Yellow
Write-Host "  2. tar -xzf edgelite-$Version-linux-arm7.tar.gz -C /tmp/edgelite-pkg" -ForegroundColor Yellow
Write-Host "  3. cd /tmp/edgelite-pkg/edgelite-$Version; sudo bash install.sh --port 8080" -ForegroundColor Yellow
