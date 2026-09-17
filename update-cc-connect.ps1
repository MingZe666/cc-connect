#requires -Version 5.1
<#
.SYNOPSIS
编译、备份并重启直接运行的 cc-connect；请在当前任务结束后执行。
.EXAMPLE
powershell -ExecutionPolicy Bypass -File .\update-cc-connect.ps1
.EXAMPLE
powershell -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 -BuildOnly
#>
[CmdletBinding()]
param(
    [string]$ConfigPath = "$env:USERPROFILE\.cc-connect\config.toml",
    [string]$TargetPath,
    [string]$GoPath,
    [switch]$BuildOnly
)

$ErrorActionPreference = 'Stop'
# Windows PowerShell 在参数默认表达式阶段可能尚未设置 PSScriptRoot，进入脚本后再定位。
if (!$TargetPath) { $TargetPath = Join-Path $PSScriptRoot 'dist\cc-connect.exe' }

# 查找已有 Go 工具链，不自动下载安装或修改全局 PATH。
function Find-CcGo([string]$ExplicitPath) {
    if ($ExplicitPath) { return (Resolve-Path -LiteralPath $ExplicitPath).Path }
    $command = Get-Command go.exe -ErrorAction SilentlyContinue
    if ($command) { return $command.Source }
    $toolsRoot = Join-Path $env:LOCALAPPDATA 'Programs\cc-connect-tools'
    $versions = @(Get-ChildItem -LiteralPath $toolsRoot -Directory -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match '^go\d+\.\d+\.\d+$' } |
        Sort-Object { [version]$_.Name.Substring(2) } -Descending)
    foreach ($version in $versions) {
        $installed = Join-Path $version.FullName 'go\bin\go.exe'
        if (Test-Path -LiteralPath $installed -PathType Leaf) { return $installed }
    }
    throw '未找到 Go，请通过 -GoPath 指定 go.exe。'
}

# 调用原生构建命令，退出码非零时立即中止，绝不继续替换程序。
function Invoke-CcCommand([string]$File, [string[]]$Arguments) {
    & $File @Arguments | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "命令失败：$File，退出码 $LASTEXITCODE" }
}

function Install-CcWebDependencies([string]$Npm) {
    if (Test-Path -LiteralPath 'node_modules') { return }
    $operation = if (Test-Path -LiteralPath 'package-lock.json') { 'ci' } else { 'install' }
    Invoke-CcCommand $Npm @($operation)
}

# 仅选择目标绝对路径的进程；不按程序名称批量停止其他实例。
function Get-CcTargetProcess([string]$Target) {
    @(Get-CimInstance Win32_Process |
        Where-Object { $_.ExecutablePath -and [IO.Path]::GetFullPath($_.ExecutablePath) -eq $Target })
}

# 校验进程使用同一配置，避免把其他配置的实例误当成本次更新目标。
function Assert-CcConfig($Process, [string]$Config) {
    $configMatches = [regex]::Matches($Process.CommandLine, '(?:^|\s)--?config(?:=|\s+)(?:"([^"]+)"|(\S+))')
    # Go 同时支持 -config 和 --config，且重复参数最后一个生效；拒绝歧义。
    if ($configMatches.Count -ne 1) { throw '运行进程必须显式且仅指定一次 --config，请核对启动参数。' }
    $match = $configMatches[0]
    $value = $match.Groups[1].Value
    if (!$value) { $value = $match.Groups[2].Value }
    if (![IO.Path]::IsPathRooted($value) -or [IO.Path]::GetFullPath($value) -ne $Config) {
        throw '目标进程的配置与 -ConfigPath 不一致，请核对后再执行。'
    }
}

# 停止前重新核对 PID 和创建时间，防止 PID 被其他进程复用。
function Stop-CcProcess($Process) {
    $current = Get-CimInstance Win32_Process -Filter "ProcessId=$($Process.ProcessId)"
    if (!$current) { return }
    if ($current.CreationDate -ne $Process.CreationDate -or $current.ExecutablePath -ne $Process.ExecutablePath) {
        throw '目标 PID 已变化，中止更新。'
    }
    Stop-Process -Id $Process.ProcessId -Force
    # Windows 释放可执行文件句柄可能稍晚于停止请求，最多等待十秒。
    Wait-Process -Id $Process.ProcessId -Timeout 10 -ErrorAction SilentlyContinue
    if (Get-Process -Id $Process.ProcessId -ErrorAction SilentlyContinue) { throw '目标进程未停止。' }
}

# 后台启动并等待本地 ready 标记；失败时只终止本函数创建的进程。
function Start-CcProcess([string]$Target, [string]$Config, [string]$LogPrefix) {
    $stdout = "$LogPrefix.stdout.log"
    $stderr = "$LogPrefix.stderr.log"
    $process = Start-Process -FilePath $Target -ArgumentList @('--config', ('"{0}"' -f $Config)) `
        -WorkingDirectory $PSScriptRoot -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    # 只验证本地启动，最多等待三十秒；不把它等同于平台连接或模型调用成功。
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    try {
        while ([DateTime]::UtcNow -lt $deadline) {
            $process.Refresh()
            if ($process.HasExited) { throw "程序启动后退出，退出码 $($process.ExitCode)；查看 $stderr" }
            foreach ($log in @($stdout, $stderr)) {
                if ((Test-Path -LiteralPath $log) -and (Select-String -LiteralPath $log -SimpleMatch 'cc-connect is running' -Quiet)) {
                    Write-Host "启动成功，PID=$($process.Id)；日志：$stderr"
                    return
                }
            }
            Start-Sleep -Milliseconds 250 # 本地日志轮询间隔，避免忙等。
        }
        throw "等待启动就绪超时；查看 $stderr"
    } catch {
        $process.Refresh()
        if (!$process.HasExited) {
            Stop-Process -Id $process.Id -Force
            $process.WaitForExit()
        }
        throw
    } finally {
        $process.Dispose()
    }
}

# 执行可回滚替换：候选文件已验证后才触碰运行程序，备份始终保留。
function Install-CcBinary([string]$Candidate, [string]$Target, [string]$Config, [string]$RunDirectory) {
    $processes = @(Get-CcTargetProcess $Target)
    if ($processes.Count -gt 1) { throw '目标路径有多个运行实例，请先确定要更新的实例。' }
    foreach ($process in $processes) { Assert-CcConfig $process $Config }
    $hadBinary = Test-Path -LiteralPath $Target
    $backup = Join-Path $RunDirectory 'cc-connect.previous.exe'
    if ($hadBinary) { Copy-Item -LiteralPath $Target -Destination $backup }
    # 停止失败不进入替换，避免覆盖仍在运行或身份不符的进程。
    foreach ($process in $processes) { Stop-CcProcess $process }
    try {
        Copy-Item -LiteralPath $Candidate -Destination $Target -Force
        Start-CcProcess $Target $Config (Join-Path $RunDirectory 'new')
    } catch {
        $failure = $_
        try {
            if ($hadBinary) {
                Copy-Item -LiteralPath $backup -Destination $Target -Force
                if ($processes.Count -gt 0) { Start-CcProcess $Target $Config (Join-Path $RunDirectory 'rollback') }
            } elseif (Test-Path -LiteralPath $Target) {
                Remove-Item -LiteralPath $Target
            }
        } catch {
            throw "更新失败：$failure；回滚也失败：$_。旧程序备份：$backup"
        }
        throw "更新失败，已恢复更新前的程序和运行状态：$failure"
    }
}

# 主流程复用项目 npm/Go 构建入口；部署锁覆盖编译到启动，避免同时更新。
function Invoke-CcUpdate {
    $target = [IO.Path]::GetFullPath($TargetPath)
    $config = (Resolve-Path -LiteralPath $ConfigPath).Path
    $directory = Split-Path -Parent $target
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
    $lock = [IO.File]::Open((Join-Path $directory '.update.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
    try {
        if (!$BuildOnly) {
            # 本脚本管理直接进程，服务管理器重启策略需要使用服务专用部署流程。
            $service = Get-CimInstance Win32_Service | Where-Object { $_.PathName -and $_.PathName.IndexOf($target, [StringComparison]::OrdinalIgnoreCase) -ge 0 }
            if ($service) { throw '目标程序已注册 Windows 服务，本脚本只支持直接运行的进程。' }
        }
        $go = Find-CcGo $GoPath
        $npm = (Get-Command npm.cmd -ErrorAction Stop).Source
        $runId = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [Guid]::NewGuid().ToString('N')
        $runDirectory = Join-Path $directory "deployments\$runId"
        New-Item -ItemType Directory -Path $runDirectory -Force | Out-Null
        $candidate = Join-Path $runDirectory 'cc-connect.candidate.exe'
        Push-Location (Join-Path $PSScriptRoot 'web')
        try {
            Install-CcWebDependencies $npm
            Invoke-CcCommand $npm @('run', 'build')
        } finally { Pop-Location }
        Push-Location $PSScriptRoot
        try {
            Invoke-CcCommand $go @('build', '-o', $candidate, './cmd/cc-connect')
            Invoke-CcCommand $candidate @('--version')
        } finally { Pop-Location }
        Write-Host "编译完成：$candidate"
        if ($BuildOnly) { return }
        Write-Host '开始替换并重启；目标进程尚未完成的任务会被中断。'
        Install-CcBinary $candidate $target $config $runDirectory
        Write-Host "更新完成：$target；备份与日志：$runDirectory"
    } finally { $lock.Dispose() }
}

# 点加载仅提供函数，便于隔离测试；作为脚本执行时运行完整流程。
if ($MyInvocation.InvocationName -ne '.') { Invoke-CcUpdate }
