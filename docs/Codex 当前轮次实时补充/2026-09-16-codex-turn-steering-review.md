# Codex 当前轮补充：实施与审阅记录

日期：2026-09-16。审阅基线：`368377b`。范围：工作区未提交代码及新增测试。

## 结果

已实现可选 TurnSteerer、默认 queue / 可选 steer 配置、同会话串行补充、接收记录持久化和重投去重、明确失败分类、附件公共构造、自定义 App Server 命令和参数传递，以及五语言反馈和历史待核实提示。

不确定结果不会自动重试；明确无活跃轮次才回退同会话队列。补充不能撤销到达前已执行的操作。生产配置、运行程序和现有导账任务未切换。

## 审阅及修复

采用独立只读审阅，修改后复审，重要发现均已修复：

- start 响应晚于完成通知时，不能复活已结束轮次；先到的完成通知等待本地启动身份关联。
- 回退消息必须排在后来普通消息前，且已接收消息不能被容量竞争丢弃。
- 完成屏障只等待投递分类、历史与落盘，不能等待平台网络或回调。
- /new、/switch 后同聊天旧 ID 仍去重；不同工作区和聊天记录隔离。
- 回退发送前保存失败、会话消失、停止和撤回必须结算为明确未提交。
- error 通知过滤归属；仅作诊断，由 turn/completed 唯一结算，避免双终态及提前启动下一轮。
- 关闭与事件发送协同，关键终态事件不因队列满静默丢弃。

最终复审确认最后的错误通知问题已闭环。审阅不替代真实平台验收。

## 验证证据

使用官方 Go 1.25.0 Windows 工具链。

| 检查 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./core ./agent/codex ./config` | 通过 |
| `go test ./core ./agent/codex ./config -run 'Steer\|STEER\|AppServer\|WorkspaceAgentOptions\|Watermark' -count=1` | 三个包通过 |
| `go test ./core/ -run TestCUJ -count=1` | 新增 CUJ 通过；现有 A3/A5 因 Windows TempDir 清理失败未全绿 |
| `go test ./...` | 未通过，存在基线环境失败，见下节 |
| `go test -race ...` | 未执行成功：CGO 未启用，环境缺少可用 C 编译器 |

新增覆盖：正常及连续补充、完成屏障、结束竞态、超时不重发、重启后去重、/new 后旧消息重投、两个工作区同 ID 不串消息、回退顺序与容量、保存失败、附件唯一文件、通知归属、自定义命令实际启动及参数继承。

回归证据：`TestAppServerStartResponseAfterCompletion` 在基线独立工作树失败（已结束轮次复活），修复后通过。新增 CUJ 的 STEER 分组是本地新增名称；未更新仓库外的权威 CUJ 清单。

### 基线对照

在独立基线工作树执行全量测试，已复现多 Agent CLI/Unix 路径假设、配置 HOME 路径、core 文件路径、daemon 文件权限等失败。当前全量测试另显示 cmd/cc-connect 的 Windows 测试引用 Unix 专有类型失败；基线 cmd 测试先被缺少 web/dist 阻挡，未把该项声称为已完整复现。

A3/A5 临时目录清理失败已在基线执行 `go test ./core -run 'TestCUJ_A[35]_' -count=10` 复现。因此不将完整 CUJ 或全量测试标为通过。

原始日志位于本机临时目录：`cc-steer-final-full.log`、`cc-steer-final-cuj.log`、`cc-steer-baseline-full.log`、`cc-steer-baseline-cuj-cleanup.log`。本报告保留结论，临时日志可能被系统清理。

## 隔离协议检查与上线前条件

使用本机 `codex-cli 0.154.0-alpha.6.2`、临时 CODEX_HOME 和临时工作目录检查 App Server；生成本机 JSON schema 核对协议字段，并验证无活跃轮次及 expectedTurnId 不匹配的实际错误格式。错误白名单只匹配验证过的完整形式，其他 RPC 错误作为明确拒绝，不随意回退。

`-c sandbox_workspace_write.network_access=true` 已传递，config/read 确認网络配置为 true；但隔离实例 thread/start 返回的实际 sandbox 是 readOnly、networkAccess=false。原因尚未确认，可能涉及独立 Windows 沙箱初始化；不能把参数传递测试当作网络实际可用的证明，也未擅自扩大权限。

上线前仍需：

1. 在具备正确身份和 Windows 沙箱设置的独立实例确认实际 workspace-write 与网络行为。
2. 在真实消息平台开始长报告，中途补充“只保留 2026 年数据”，确认同一 turn 按要求完成；单元/CUJ 使用模拟外部边界，不能替代此项。
3. 在具备 CGO 的环境运行竞态检测，并通过项目发布测试门禁。
4. 当前导账任务结束后，再为目标项目切换 backend/app_server_url/busy_message_mode，保留现有模型、full-auto、网络和多工作区设置，替换程序并重启。

当前交付为可审阅的实现；尚未满足生产切换条件。
