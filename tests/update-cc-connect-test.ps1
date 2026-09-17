#requires -Version 5.1
# 隔离测试替换与回滚，不查询或停止真实 cc-connect 进程。
$ErrorActionPreference = 'Stop'
. (Join-Path (Split-Path -Parent $PSScriptRoot) 'update-cc-connect.ps1')
# 回归检查 PowerShell 5 参数默认值求值时机，目标不得退化成盘符根目录。
$expectedTarget = Join-Path (Split-Path -Parent $PSScriptRoot) 'dist\cc-connect.exe'
if ($TargetPath -ne $expectedTarget) { throw "默认目标路径错误：$TargetPath" }
Write-Host 'PASS default-target'


# 使用虚拟进程边界测试部署事务，文件复制仍执行真实实现。
function Get-CcTargetProcess([string]$Target) {
    if ($script:WasRunning) {
        [pscustomobject]@{ CommandLine = ('cc-connect.exe --config "{0}"' -f $script:TestConfig) }
    }
}

# 记录停止调用，确保失败构建之外的部署只停止选定实例。
function Stop-CcProcess($Process) { $script:StopCount++; if ($script:FailStop) { throw '模拟停止失败' } }

# 模拟启动探测失败，使真实替换流程进入回滚分支。
function Start-CcProcess([string]$Target, [string]$Config, [string]$LogPrefix) {
    $content = [IO.File]::ReadAllText($Target)
    $script:Starts.Add($content)
    if ($script:FailNew -and $content -eq 'new') { throw '模拟新程序启动失败' }
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('cc-update-test-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $testRoot | Out-Null
try {
    & {
        function Invoke-CcCommand([string]$File, [string[]]$Arguments) {
            $script:InstallArguments = $Arguments -join ' '
        }
        Push-Location $testRoot
        try {
            Install-CcWebDependencies 'npm.cmd'
            if ($script:InstallArguments -ne 'install') { throw '无 npm 锁文件时应使用 install' }
            [IO.File]::WriteAllText((Join-Path $testRoot 'package-lock.json'), '{}')
            Install-CcWebDependencies 'npm.cmd'
            if ($script:InstallArguments -ne 'ci') { throw '存在 npm 锁文件时应使用 ci' }
            Write-Host 'PASS npm-install-without-lockfile'
        } finally { Pop-Location }
    }
    # PATH 中无 Go 时，必须识别用户工具目录中的不同 Go 版本。
    & {
        $savedLocalAppData = $env:LOCALAPPDATA
        function Get-Command { param($Name, $ErrorAction) return $null }
        try {
            $env:LOCALAPPDATA = $testRoot
            $expectedGo = Join-Path $testRoot 'Programs\cc-connect-tools\go1.26.8\go\bin\go.exe'
            New-Item -ItemType Directory -Path (Split-Path $expectedGo) -Force | Out-Null
            [IO.File]::WriteAllText($expectedGo, '')
            if ((Find-CcGo) -ne $expectedGo) { throw '未找到用户目录中的 Go 工具链' }
            Write-Host 'PASS go-discovery-versioned-user-install'
        } finally { $env:LOCALAPPDATA = $savedLocalAppData }
    }
    foreach ($scenario in @('success', 'rollback-running', 'rollback-stopped', 'first-install-failed', 'stop-failed')) {
        $caseDir = Join-Path $testRoot $scenario
        New-Item -ItemType Directory -Path $caseDir | Out-Null
        $target = Join-Path $caseDir 'cc-connect.exe'
        $candidate = Join-Path $caseDir 'candidate.exe'
        $script:TestConfig = Join-Path $caseDir 'config.toml'
        $script:WasRunning = $scenario -in @('success', 'rollback-running', 'stop-failed')
        $script:FailStop = $scenario -eq 'stop-failed'
        $script:FailNew = $scenario -notin @('success', 'stop-failed')
        $script:StopCount = 0
        $script:Starts = [Collections.Generic.List[string]]::new()
        if ($scenario -ne 'first-install-failed') { [IO.File]::WriteAllText($target, 'old') }
        [IO.File]::WriteAllText($candidate, 'new')
        $failed = $false
        try { Install-CcBinary $candidate $target $script:TestConfig $caseDir } catch { $failed = $true }
        if ($failed -ne ($script:FailNew -or $script:FailStop)) { throw "$scenario：返回结果错误" }
        if ($scenario -eq 'first-install-failed') {
            if (Test-Path -LiteralPath $target) { throw '首次启动失败遗留了目标程序' }
        } else {
            $expected = if ($script:FailNew -or $script:FailStop) { 'old' } else { 'new' }
            if ([IO.File]::ReadAllText($target) -ne $expected) { throw "$scenario：恢复文件错误" }
            if ([IO.File]::ReadAllText((Join-Path $caseDir 'cc-connect.previous.exe')) -ne 'old') { throw '旧程序备份丢失' }
        }
        $expectedStarts = if ($scenario -eq 'rollback-running') { 'new,old' } elseif ($script:FailStop) { '' } else { 'new' }
        if (($script:Starts -join ',') -ne $expectedStarts) { throw "$scenario：启动/恢复顺序错误" }
        if ($script:StopCount -ne [int]$script:WasRunning) { throw "$scenario：停止次数错误" }
        Write-Host "PASS $scenario"
    }
    # 配置不同必须在停止进程前拒绝更新。
    $script:TestConfig = Join-Path $testRoot 'other.toml'
    $rejected = $false
    try { Assert-CcConfig ([pscustomobject]@{CommandLine = 'cc-connect --config "C:\wrong.toml"'}) $script:TestConfig } catch { $rejected = $true }
    if (!$rejected) { throw '未拒绝不同配置的进程' }
    Write-Host 'PASS config-mismatch'
    # 重复参数最后值会覆盖前值，必须在停止前拒绝，含单横线混用。
    foreach ($flag in @('--config', '-config')) {
        $rejected = $false
        $command = 'cc-connect --config "{0}" {1} "C:\other.toml"' -f $script:TestConfig, $flag
        try { Assert-CcConfig ([pscustomobject]@{CommandLine = $command}) $script:TestConfig } catch { $rejected = $true }
        if (!$rejected) { throw '未拒绝重复 config 参数' }
    }
    Write-Host 'PASS duplicate-config'
    Assert-CcConfig ([pscustomobject]@{CommandLine = ('cc-connect -config "{0}"' -f $script:TestConfig)}) $script:TestConfig
    Write-Host 'PASS single-dash-config'

    # 映射在临时 profile 内测试，不写真实用户配置；覆盖中文、空格和单引号路径。
    $mappingDir = Join-Path $testRoot "映射 space's"
    New-Item -ItemType Directory -Path $mappingDir | Out-Null
    $mappingTarget = Join-Path $mappingDir 'cc-connect.exe'
    [IO.File]::WriteAllText($mappingTarget, 'fixture')
    $profileFixture = Join-Path $testRoot 'profile.ps1'
    $originalProfile = "# 用户原有中文配置`r`nSet-Alias cc-connect 'C:\old\cc-connect.exe'`r`n"
    [IO.File]::WriteAllText($profileFixture, $originalProfile, [Text.UTF8Encoding]::new($true))
    Install-CcCommandMapping $mappingTarget $profileFixture
    $firstProfile = [IO.File]::ReadAllText($profileFixture)
    if (!$firstProfile.StartsWith($originalProfile)) { throw '修改了用户原有配置' }
    Install-CcCommandMapping $mappingTarget $profileFixture
    if ([IO.File]::ReadAllText($profileFixture) -cne $firstProfile) { throw '重复映射不是幂等操作' }
    if (@(Get-ChildItem -LiteralPath $testRoot -Filter 'profile.ps1.cc-connect-*.bak').Count -ne 1) { throw '重复执行不应创建多余备份' }
    . $profileFixture
    if ((Get-Alias cc-connect).Definition -ne $mappingTarget) { throw '转义后的映射不能生效' }
    Write-Host 'PASS mapping-preserves-profile-and-idempotence'
    $secondTarget = Join-Path $testRoot 'other.exe'
    [IO.File]::WriteAllText($secondTarget, 'fixture')
    Install-CcCommandMapping $secondTarget $profileFixture
    . $profileFixture
    if ((Get-Alias cc-connect).Definition -ne $secondTarget) { throw '更新部署路径后仍指向旧文件' }
    if ([regex]::Matches([IO.File]::ReadAllText($profileFixture), '# BEGIN cc-connect managed alias').Count -ne 1) { throw '托管区块重复' }
    Write-Host 'PASS mapping-retarget'
    $brokenProfile = Join-Path $testRoot 'broken-profile.ps1'
    [IO.File]::WriteAllText($brokenProfile, '# BEGIN cc-connect managed alias')
    $rejected = $false
    try { Install-CcCommandMapping $mappingTarget $brokenProfile } catch { $rejected = $true }
    if (!$rejected) { throw '不完整托管区块未被拒绝' }
    Write-Host 'PASS mapping-incomplete-marker'

} finally {
    # 仅递归清理本测试创建且已确认位于临时目录的独立目录。
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $tempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if (!$resolved.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) { throw '测试清理目录越界' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
