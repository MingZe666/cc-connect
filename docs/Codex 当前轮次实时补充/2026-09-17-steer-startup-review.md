# 当前轮启动期间补充修复

## 行为变更

第一条消息仍通过 Send 启动新轮。启用实时补充且 Agent 声明能力时，在异步启动进程前登记独立启动批次，后续消息沿用实时补充队列暂存。Codex App Server 的 Send 收到 turn/start 响应后返回，此时放行本轮补充，无需等待模型完成任务。

不再把启动期间本地尚无 turnId 直接当作轮次结束。每轮使用独立信号，Send 发起前捕获它；迟到响应不能放行下一批次。占位 state 替换移交启动信号、补充队列和 worker 完成通道。停止、启动失败和一分钟等待超时结算为明确未提交，不自动重新发送补充。轮次确实结束时仍沿用既有明确未接收回退规则，未知 RPC 结果仍不重发。

本修改不实现“收齐所有附件后才开工”。等待超过一分钟的该批次补充会明确失败，即使之后主任务成功启动也不会偷偷重发。

## 测试与复审

修复前运行 TestSteerWaitsForSendReady，失败信息为 steer submitted before turn/start confirmation；修复后通过。

新增覆盖：两轮启动屏障、占位迁移后有序投递、失败/停止不提交、启动超时不被迟到成功覆盖、worker 使用迁移后的拥有者、旧 Send 不放行新批次。CUJ_STEER3 从 ReceiveMessage 驱动首次启动、复用下一轮和 /new 后第三轮，每轮连续补充并断言用户可见成功回复。

最终检查：

- 定向 go test（core/codex/config 的 Steer、STEER、AppServer、WorkspaceAgentOptions、Watermark）：通过。
- go test ./core -run TestCUJ -count=1：通过。
- go build ./...：通过。
- go vet ./core ./agent/codex ./config：通过。
- go test ./...：未通过，仍为先前记录的 Windows CLI、路径、权限等环境失败；日志 %TEMP%/cc-steer-startup-full.log。
- 未声称 race 检测通过，本机缺少此前要求的 CGO/C 编译环境。

独立只读审阅发现并修复了“超时期间 owner 迁移丢队列”和“异步 Send 过晚捕获 gate”两处竞态，最终复审未发现新的确定性问题。

另外发现存量 graceful CancelTurn 失败分支重复解锁问题；Codex 未实现该接口，本轮未扩大修改范围，该遗留问题需单独修复。

## 交付

单独编译 dist/cc-connect-steer-startup.exe；未替换旧文件、未重启运行实例、未更改生产配置。用户完成当前任务并停止旧实例后，可使用同一配置启动新文件，在真实企业微信环境验证连续上传和下一轮补充。
