# Windows 一键编译更新

适用于直接运行 cc-connect.exe 的 Windows PowerShell 5.1 或更高版本。本机目前采用这种运行方式，没有注册 Windows 服务。

## 一条命令完成更新、重启和映射

请先将更新后的 `update-cc-connect.ps1` 同步到实际使用的源码仓库。下面以你的部署目录 `D:\SmartImport\tools\cc-connect` 为例：

```powershell
cd D:\SmartImport\tools\cc-connect
```

在仓库根目录执行原来的命令即可，不必额外加映射参数：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1
```

执行顺序：**编译 → 备份 → 替换 → 后台重启 → 保存命令映射**。

以上目录对应的默认目标是 `D:\SmartImport\tools\cc-connect\dist\cc-connect.exe`。脚本直接启动这个 EXE，不经过 npm，也不依赖已有别名。默认配置是执行用户的 `$env:USERPROFILE\.cc-connect\config.toml`，用户名不会固定为 gaomi 或 jinti。

更新完成后，当前窗口执行：

```powershell
. $PROFILE
Get-Command cc-connect
```

应显示 `Alias`，目标为上述 EXE；如果仍显示 npm 目录里的 `ExternalScript cc-connect.ps1`，说明当前窗口尚未加载映射或使用的不是新版脚本。新开的 Windows PowerShell 窗口正常加载 profile 后自动生效。

## 参数速查

| 参数 | 用途 |
| --- | --- |
| 不带参数 | 编译、替换、重启，并保存命令映射 |
| `-BuildOnly` | 只生成并验证候选程序，不替换、不重启、不修改映射 |
| `-MapOnly` | 仅为已存在的目标 EXE 保存映射，不编译、不重启 |
| `-SkipCommandMapping` | 完整更新重启，但跳过命令映射 |
| `-TargetPath` | 指定部署 EXE 路径，映射也使用此路径 |
| `-ConfigPath` | 指定启动配置文件 |
| `-GoPath` | 指定 Go 编译器路径 |

`-MapOnly` 不能与 `-BuildOnly` 或 `-SkipCommandMapping` 同时使用。目前没有 `-Version` 参数。

## 构建要求与其他用法

Go 查找顺序为 `-GoPath`、PATH、`%LOCALAPPDATA%\Programs\cc-connect-tools\go版本号\go\bin\go.exe`（优先最高版本）。需预先安装符合 `go.mod` 要求的 Go；脚本本身不会下载工具链。

请在导账任务结束后执行。脚本使用 Stop-Process 终止旧主进程，不能保证正在执行的任务完整退出；它不会等待业务任务自动完成。外部工具已经执行的操作也不会被撤销。

只编译验证、不替换或重启：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 -BuildOnly
```

指定配置或 Go 路径：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 `
  -ConfigPath "$env:USERPROFILE\.cc-connect\config.toml" `
  -GoPath "C:\Program Files\Go\bin\go.exe"
```

如果源码目录与部署目录不同，可以指定目标：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 `
  -TargetPath "D:\SmartImport\tools\cc-connect\dist\cc-connect.exe"
```

编译的始终是脚本所在仓库的源码，`-TargetPath` 只改变部署和映射位置。

## 脚本流程

1. 取得目标目录的更新锁，检查配置与工具链。
2. 执行 npm run build；无 node_modules 时，有 package-lock.json 则先 npm ci，否则 npm install。
3. Go 编译候选文件并运行 --version。编译失败不停止旧实例。
4. 按完整可执行文件路径定位进程，核对其显式 --config。不同配置、多实例或目标已注册 Windows 服务时拒绝部署。
5. 备份旧文件，核验 PID/创建时间后停止目标进程，再复制候选程序。
6. 以仓库根目录为工作目录、指定配置后台启动。日志写到独立部署目录。
7. 最多三十秒等待本地 cc-connect is running 标记。失败恢复旧文件；原先在运行则重新启动旧文件，原先未运行则保持停止。

8. 启动成功后保存当前用户的命令映射；映射失败单独警告，不回滚程序。

备份、候选文件与日志保存在目标目录 `deployments\日期时间-唯一ID\`，不自动删除历史备份。若回滚本身失败，脚本报错并显示备份位置，需手动处理。

这个启动检查证明本地初始化就绪，不保证企业微信订阅、模型网络及真实业务任务成功。脚本不会注册 Windows 服务、任务计划或开机自启。

## cc-connect 命令入口

完整更新成功后，脚本默认把 `cc-connect` 别名写入执行用户的 Windows PowerShell 当前主机配置 `$PROFILE`，指向实际部署目标（包含 `-TargetPath`）。保留原配置，修改前备份，只维护自己的标记区块；重复执行不会重复追加。npm 启动器和安装目录不变。

只设置映射，不编译、不重启，也不需要配置文件：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 -MapOnly
# 当前 PowerShell 窗口加载新映射；检查应显示 Alias 和目标 EXE。
. $PROFILE
Get-Command cc-connect
```

子进程无法改变父窗口的别名。新开的 Windows PowerShell 窗口正常加载 profile 后自动生效，已有窗口需执行 `. $PROFILE`。通过 `-NoProfile` 启动的窗口不加载别名。这不会设置 CMD、Git Bash、PowerShell 7 或其他用户的命令入口；使用 `powershell -File` 时写入的是 Windows PowerShell 的 profile。

完整更新时可加 `-SkipCommandMapping` 跳过映射，`-BuildOnly` 始终不写映射。程序更新成功但映射写入失败时会单独警告，可用 `-MapOnly` 重试，不会因此回滚已启动的程序。如旧代码/技能写死了 npm 目录下的 EXE 路径，别名不会改变这些调用。

脚本已经后台启动后，不要再启动同一配置的第二个实例。

## 关闭进程后再次启动

更新脚本已经后台启动程序时，不要重复启动同一配置。确认旧进程已停止后，可以直接用已生效的别名启动，无需重新编译：

```powershell
cc-connect --config "$env:USERPROFILE\.cc-connect\config.toml"
```

这次是前台运行。若映射未生效，可直接使用完整路径：

```powershell
& "D:\SmartImport\tools\cc-connect\dist\cc-connect.exe" `
  --config "$env:USERPROFILE\.cc-connect\config.toml"
```

## 验证

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\tests\update-cc-connect-test.ps1
```

隔离测试覆盖默认路径、正常替换、旧实例回滚恢复、原先停止状态、首次启动失败清理、停止失败不替换、配置不匹配拒绝，以及映射保留用户配置、重复执行、目标路径更新和残缺标记保护。真实 BuildOnly 已验证前端、Go 构建及版本检查；交付时未执行生产替换/重启。
