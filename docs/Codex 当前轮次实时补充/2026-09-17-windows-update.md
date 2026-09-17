# Windows 一键更新

目标：用户执行一个 PowerShell 命令，构建前端与 Go 程序、备份、替换当前路径程序、后台重启并检查启动日志；失败恢复旧程序。现状是直接进程，不是 Windows 服务。

实现：根目录 update-cc-connect.ps1，默认目标 dist/cc-connect.exe，配置 ~/.cc-connect/config.toml；BuildOnly 只生成候选文件。沿用 npm build / go build，Go 支持 PATH、显式路径和本机已安装路径。部署锁避免并发更新，路径与配置匹配才停止目标 PID。只支持直接进程，检测到匹配 Windows 服务时拒绝。

验证：PowerShell 语法检查、隔离目录模拟替换成功/启动失败恢复/无旧程序清理；BuildOnly 真实构建并验证 --version。不实际重启生产任务。

限制：停止使用 Stop-Process，可能中断任务；用户应在空闲时执行。启动检查是本地程序 ready 标记，不代表外部平台或模型可用。不创建开机自启，不管理 npm 更新或写死的旧二进制路径。
