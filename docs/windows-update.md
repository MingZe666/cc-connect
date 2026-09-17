# Windows 一键编译更新

适用于直接运行 cc-connect.exe 的 Windows PowerShell 5.1 或更高版本。本机目前采用这种运行方式，没有注册 Windows 服务。

## 使用

在仓库根目录执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1
```

默认目标为仓库 `dist\cc-connect.exe`，配置为当前用户 `.cc-connect\config.toml`。

Go 查找顺序为 `-GoPath`、PATH、`%LOCALAPPDATA%\Programs\cc-connect-tools\go版本号\go\bin\go.exe`（优先最高版本）。需预先安装符合 `go.mod` 要求的 Go；脚本本身不会下载工具链。

请在导账任务结束后执行。脚本使用 Stop-Process 终止旧主进程，不能保证正在执行的任务完整退出；它不会等待业务任务自动完成。外部工具已经执行的操作也不会被撤销。

只编译验证、不替换或重启：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 -BuildOnly
```

指定配置或 Go 路径：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\update-cc-connect.ps1 `
  -ConfigPath "C:\Users\gaomi\.cc-connect\config.toml" `
  -GoPath "C:\Program Files\Go\bin\go.exe"
```

## 脚本流程

1. 取得目标目录的更新锁，检查配置与工具链。
2. 执行 npm run build；无 node_modules 时，有 package-lock.json 则先 npm ci，否则 npm install。
3. Go 编译候选文件并运行 --version。编译失败不停止旧实例。
4. 按完整可执行文件路径定位进程，核对其显式 --config。不同配置、多实例或目标已注册 Windows 服务时拒绝部署。
5. 备份旧文件，核验 PID/创建时间后停止目标进程，再复制候选程序。
6. 以仓库根目录为工作目录、指定配置后台启动。日志写到独立部署目录。
7. 最多三十秒等待本地 cc-connect is running 标记。失败恢复旧文件；原先在运行则重新启动旧文件，原先未运行则保持停止。

备份、候选文件与日志保存在目标目录 `deployments\日期时间-唯一ID\`，不自动删除历史备份。若回滚本身失败，脚本报错并显示备份位置，需手动处理。

这个启动检查证明本地初始化就绪，不保证企业微信订阅、模型网络及真实业务任务成功。脚本不会注册 Windows 服务、任务计划或开机自启。

## cc-connect 命令入口

脚本更新 dist 中的 EXE，不修改 npm 启动器。前面配置的 PowerShell 别名继续指向同一路径即可：

```powershell
Set-Alias cc-connect "D:\Code\cc-connect-baseline\dist\cc-connect.exe"
```

需要跨窗口生效时将该行放入 `$PROFILE`。不要在已由脚本后台启动后再运行同一配置的第二个实例。此前任务说明中写死的 npm 安装路径也不会被该别名改变。

## 验证

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\tests\update-cc-connect-test.ps1
```

隔离测试覆盖默认路径、正常替换、旧实例回滚恢复、原先停止状态、首次启动失败清理、停止失败不替换、配置不匹配拒绝。真实 BuildOnly 已验证前端、Go 构建及版本检查；交付时未执行生产替换/重启。
