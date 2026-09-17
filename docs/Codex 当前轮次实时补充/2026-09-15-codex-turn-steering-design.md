# CC-Connect：Codex 当前轮次实时补充设计

日期：2026-09-15

状态：设计评审稿；未实现、未切换配置、未重启服务

源码基线：`D:\Code\cc-connect-baseline`，提交 `368377b`（初始状态）

## 1. 目标与范围

目标：目标项目启用实时补充后，用户在 Codex 执行期间发送普通消息，优先补充到同一工作区、同一聊天会话的当前轮次。成功补充不创建新轮次、不再次排队、不额外等待一次完成事件。

验收场景：开始生成较长报告 → 中途发送“只保留 2026 年数据” → 同一轮按新要求完成。判断依据同时包含报告内容和协议日志，不能仅凭回复文本推断使用了同一轮。

本期包含可选能力接口、Engine 忙碌分支、Codex App Server 适配、参数继承、消息接收记录、配置接线、测试和上线步骤。不扩展其他 Agent 的实时补充实现，不实现远程 WebSocket 连接，不改变已经执行的上传或其他操作。

本轮产出设计文档，后续代码实施以本稿评审结果为准。

## 2. 已核对的现状

下列位置均来自当前目录，旧的 `D:\SmartImport\tools\cc-connect-auto-workspace` 路径不再作为依据。行号仅用于当前快照，实施时按符号定位。

| 位置 | 当前行为 | 设计影响 |
| --- | --- | --- |
| `core/interfaces.go:409`，`AgentSession` | 只有 `Send` 等基础能力 | 在旁边新增可选接口，不改变所有 Agent 的必选接口 |
| `core/engine.go:3024` 附近，`session.TryLock` 分支 | 忙碌时调用 `queueMessageForBusySession` | 在已有命令、审批、工作区解析之后接入补充路由 |
| `core/engine.go:3152` 附近 | 队列有容量限制、过期消息检查、`OnAccepted` 和孤儿队列恢复 | 保留并复用，不能简单替换整个忙碌分支 |
| `core/engine.go:5878` 附近 | `EventResult` 后直接取队列发下一轮 | 增加补充投递结算屏障，避免旧轮请求跨到下一轮 |
| `agent/codex/appserver_session.go:444`，`Send` | 保存附件、追加文件引用、构造输入、调用 `turn/start` | 提取公共输入构造，`Steer` 复用 |
| 同文件 `handleNotification`、`completeTurn` | 多处没有核对 thread/turn；idle 状态也完成当前轮 | 统一轮次归属校验，消除过期通知影响 |
| 同文件 `Send` 和 `turn/started` | 都设置 `currentTurn` 并清空文本缓冲 | 必须处理重复开始、响应晚于完成的竞态 |
| 同文件 `requestWithTimeout`、`rejectPending` | 服务端错误被压成字符串，断连也伪装成 RPC error | 保留结构化错误和传输错误来源 |
| 同文件 `connect:254` | `exec.CommandContext` 固定使用 `codex` | 传入已解析的 binary 和附加参数 |
| `agent/codex/codex.go:553`，`WorkspaceAgentOptions` | 已传递 backend、stdio URL、cmd 参数副本 | 增补联合用例，不重写已有继承机制 |
| `core/session.go:619`，`Save` | 原子文件写入，但调用方拿不到错误 | 补充记录需要可观察的保存结果，并复用现有快照逻辑 |

已存在的复用点：`core.ParseCmdOpts`、`core.SaveFilesToDisk`、`core.AppendFileRefs`、`stageImages`、`buildSenderPrompt`、`runMessageAccepted`、`Session.AddHistory`、`SessionManager`、`AtomicWriteFile`、现有队列和 CUJ 测试桩。

当前 shell 解析到的 Codex 为 `0.154.0-alpha.6.2`。这只是本次只读检查结果，不代表正在运行的桥接服务也使用该 binary。部署前必须通过实际启动命令确认。

`LOCAL-AUTO-WORKSPACE.md` 记录过 Windows 全量测试失败及部分基线问题；本次没有执行测试，不能把该历史记录当作当前测试结论。

## 3. 方案比较与选择

| 方案 | 优点 | 代价 | 选择 |
| --- | --- | --- | --- |
| 可选 `TurnSteerer` + Engine 会话内有序投递 | 不破坏 Agent 边界；错误和队列行为可独立测试 | 需要补齐轮次与投递状态 | **采用** |
| 让 `Send` 自动决定 start 或 steer | 表面改动少 | 调用方无法知道是否新增轮次，现有完成事件等待容易失配 | 不采用 |
| 中断当前轮再启动新轮 | 可利用部分现有停止流程 | 无法实现同轮补充，且不能撤回已执行操作 | 不采用 |

策略由 Engine 负责；协议细节由 Codex 适配层负责。`core` 不根据 Agent 名称或平台名称判断能力，也不解析 Codex 错误文本。

## 4. 配置设计

沿用用户提出的配置位置，在目标项目已有的 `[projects.agent.options]` 中增加以下字段。示例只展示相关字段，不替换整个项目配置。

```toml
[projects.agent.options]
# 使用本地 App Server 子进程，通过标准输入输出通信。
backend = "app_server"
app_server_url = "stdio://"

# queue 为默认值；steer 表示忙碌时优先补充当前轮。
busy_message_mode = "steer"

# 保留现有自动执行模式及工作区网络访问参数。
mode = "full-auto"
cmd = "codex -c sandbox_workspace_write.network_access=true"
```

- `busy_message_mode` 允许省略、`queue`、`steer`；省略和空字符串按 `queue`。字符串去空白并按大小写不敏感规则解析；其他类型或取值在配置加载时明确报错。
- 它是项目消息策略，虽然为了保持约定放在 Agent options 中，仍由配置层解析、启动组装层调用 `Engine.SetBusyMessageMode`，不由 Codex Agent 决定路由。
- 同一项目的所有工作区使用该 Engine 策略；无需在 `WorkspaceAgentOptions` 重复传递策略。各工作区 Agent 仍各自继承 backend、cmd、参数、model、effort 和 URL。
- 配置 `steer` 但 AgentSession 没有实现能力时，正常使用原队列；不使旧 Agent 初始化失败。
- 不增加本期开关热更新入口；切换通过正常配置加载和重启生效。模型、provider、网络、`codex_home`、多工作区及环境变量配置原样保留。
- 本次目标固定 `stdio://`。当前代码实际从管道读写，即使 URL 为 ws 也没有对应 socket 客户端；本期不顺带扩展此能力。

## 5. Core 能力契约

在 `AgentSession` 旁定义可选接口，方法签名保持用户提出的形式：

```go
// TurnSteerer 表示会话支持向当前活跃轮次追加用户输入。
type TurnSteerer interface {
    // Steer 只提交到调用时捕获的活跃轮次；返回 nil 表示服务端已确认接收。
    // 它不开始新轮次，不等待轮次完成，也不自动重试。
    Steer(prompt string, messageID string, images []ImageAttachment, files []FileAttachment) error
}
```

该接口由具体 AgentSession 实现，而不是 Agent 工厂。取消与关闭沿用 session context，RPC 的有限等待由适配层保证。

在 `core/turn_steering.go` 定义通用 `SteerError` 和结果类别：

| 类别 | 契约 | Engine 行为 |
| --- | --- | --- |
| nil | 收到匹配目标轮次的成功响应 | 确认接收、记录历史、回复补充成功 |
| `SteerUnavailable` | 确定未接收：没有活跃轮次，或目标轮次已经结束/变更 | 同一原会话排队，按下一轮处理 |
| `SteerRejected` | 确定拒绝：权限、参数、附件准备、方法不支持等错误 | 回复失败，不自动入队 |
| `SteerUnknown` | 可能已经接收：超时、提交后断连、响应无法解码或响应轮次不匹配 | 标记待核实，不自动重发或入队 |

错误包含底层 cause，支持 `Unwrap`；Engine 使用 `errors.As`，不依赖错误字符串。未分类错误保守按 Unknown 处理。没有调用过 RPC 的本地附件错误可以明确归为 Rejected。

## 6. 消息路由与有序投递

### 6.1 接入位置

沿用当前入口顺序：鉴权/限流、文本与引用合并、工作区解析、控制命令、审批回复、其他交互流程、过期消息检查，然后进入普通消息路径。不得因为开启 steer 而绕过其中任何一步。

目标必须来自本次解析得到的 `agent`、`sessions`、`session`、`interactiveKey` 和工作区目录。多工作区键继续使用当前实现的“已解析工作区 + SessionKey”；投递对象额外捕获 cc-connect session ID 和 state 实例，不能执行时重新选择任意活跃 session。

| 当前状态 | 处理 |
| --- | --- |
| Session 空闲且成功取得逻辑会话锁 | 走原 `Send` 路径 |
| 忙碌但开关为 queue | 原队列 |
| 忙碌但没有 TurnSteerer、session 尚在创建或旧队列已有消息 | 原队列；不让新补充超越已排队消息 |
| 忙碌且当前 state 正在接受补充 | 登记有序投递，再由会话投递 worker 调用 `Steer` |
| 已进入完成结算阶段 | 原队列，等待结算结束后开始下一轮 |
| state 已被停止、移除或替换 | 不投向新 state，给原发送者明确处理结果 |

提取 `core/engine_steering.go` 承载补充路由，不继续向大型 `engine.go` 堆放协议或存储逻辑。

### 6.2 有序性和容量

- 每个 interactiveState 最多一个补充投递 worker。普通补充在 state 锁内登记本地递增顺序，按登记顺序逐条提交；不声称可以恢复平台网络到达前的原始顺序。
- 待提交的补充项复用 `queuedMessage` 的消息载荷或从中提取的公共内部载荷，保存原消息 ID、发送者、附件和 ReplyCtx。它们与 `pendingMessages` 的“下一轮队列”语义不同，不能被原队列消费。
- 复用 `maxQueuedMessages` 作为等待容量：下一轮队列 + 待提交补充数量合计受限；不把已经发出的单个 RPC 计入等待容量。容量为零时允许没有等待项的立即投递，不建立隐藏等待队列。
- 普通消息只在一个容器里出现。Unavailable 回退时原子转移已有容量席位；原在途 RPC 不占等待容量，回退时允许这一项临时超限，不能因并发新消息占满而丢失已登记消息。
- 一旦某条补充回退下一轮，随后尚未提交的补充也转到原队列，并置于后来登记的普通排队消息之前，保持顺序。不同工作区不共用 worker 或投递锁。
- 从主接收处理返回后保留 worker 所需的独立消息副本，避免与平台回调修改同一个 Message 对象。
- `OnAccepted`、磁盘保存、平台回复和 RPC 均在 state/session 数据锁外执行；worker 退出时仅由自身关闭完成 channel，停止路径负责取消 context，不重复关闭 channel。

### 6.3 轮次结束屏障

`EventResult` 到达时，先在 state 锁内关闭该轮补充登记，保存终态，再在锁外等待已登记投递全部分类并完成记录。外部 OnAccepted 回调与平台回复异步执行，不纳入屏障，避免外部阻塞导致轮次无法结算。之后才执行完成水位更新、最终历史落盘和下一轮出队。

这使“成功响应晚于完成事件”仍能把补充写入正确轮次的历史，也保证尚未执行的补充不会在下一轮开始后捕获到新 turn ID。worker 等待的是 RPC 响应，不等待 Engine 处理 EventResult，因此不存在互相等待。

结算过程中保持审批/停止的专用路径可运行；补充提交有截止时间。收到 `/stop` 或 state 替换时使该 state 失效并取消其 session：已发出的请求按证据归类；尚未发出的请求明确通知未提交，不自动转移到新会话。

需要在 `processInteractiveEvents` 的终态处理、普通 drain 和 orphan drain 入口统一使用同一完成结算 helper。异常退出也必须结算，不允许只覆盖正常 EventResult。

## 7. Codex 轮次状态与协议

### 7.1 协议依据

已核对官方 App Server 文档：`turn/steer` 追加当前轮输入，必须携带 `expectedTurnId`；没有活跃轮次时失败；不会产生新的 `turn/started`；成功返回 `turnId`；不能设置 model、cwd、sandboxPolicy 等轮次覆盖项。[官方说明](https://learn.chatgpt.com/docs/app-server#steer-an-active-turn)

请求 ID 复用 `nextID` 分配，不固定为示例中的某个数字。参数只包括 `threadId`、`expectedTurnId` 和公共构造的 `input`。成功响应中的 `turnId` 必须与请求快照一致。

### 7.2 输入复用

从 `Send` 提取 `prepareTurnInput`：文件保存 → `AppendFileRefs` → `stageImages` → text/localImage 输入数组。`Send` 和 `Steer` 都调用它。发送者信息复用 Engine 的 `buildSenderPrompt`；引用和位置等增强内容已在入口合并。

初次 system/append prompt 注入保留在 `Send` 的首次启动逻辑中，不能因每次补充重复追加，也不能由 `Steer` 修改 `preambleSent`。附件保存错误必须能让调用方判定失败；对返回部分成功路径的现有保存函数，要核对实际保存结果，避免把遗漏附件当成完整接收。

附件以当前 workDir 和原 messageID 落盘。已有 `stageImages` 的时间戳命名需要覆盖并发文件名碰撞用例；只有证明可能覆盖时才最小调整为唯一文件创建，继续共享同一实现。

### 7.3 状态所有权

将轮次状态明确为 Idle、Starting、Active、Closed，并为每次 `Send` 分配本地 generation。`stateMu` 保护 generation、currentTurn、轮次文本缓冲和完成标志。

- 发起 `turn/start` 前先登记 Starting generation。
- 把 start 响应的轮次状态应用放到 readLoop 的响应分发路径，在唤醒请求等待者前完成；`Send` 返回后不再二次清空缓冲或设置 currentTurn。
- `turn/started` 可以先于响应建立同 generation 的 active turn；相同 ID 的重复开始是幂等操作，不清空已有内容。
- `turn/completed` 后完成该 generation；晚到的 start 响应或相同轮次开始通知不能把它恢复为 Active。Starting 期间保存已观察到的终态标识，用于响应匹配。
- Active 状态不得被另一个未经本地 start 流程关联的 turn/started 覆盖。应用内部只通过该 session 开始轮次。
- `Steer` 在锁内捕获 thread ID、turn ID、generation，随后释放锁，准备附件并发 RPC；不在返回时恢复或清空任何轮次状态。
- `Send` 与补充调度由 Engine 的结束屏障协调；适配层也拒绝已经 Active 时的重复 start，防止其他调用者绕过路由产生重叠轮次。

### 7.4 通知过滤和完成语义

- turn started/completed：校验 thread ID、turn ID 与本地 generation 的关联。
- item started/completed：校验 thread ID 和 turn ID 后才改文本缓冲、发工具事件。
- tokenUsage：按该通知实际提供的关联字段过滤；账号额度通知仍是账号级事件。
- 审批和 request_user_input：校验可用的 thread/turn 字段；过期请求必须明确拒绝，不能展示成当前任务审批，也不能静默悬挂服务端请求。
- error 通知核对 threadId、turnId，并解析 error.message 和 willRetry；业务错误只记录诊断，由 turn/completed 唯一结算，避免提前启动排队消息或产生双终态。
- session 级传输错误仍结束连接；不得把一个归属其他轮次的业务错误当成当前 session 的传输故障。
- `thread/status/changed = idle` 缺少可靠 turn 关联，不再独自生成完成事件。完成以匹配的 `turn/completed` 为准。目标 CLI 必须通过完整生命周期实测后才能启用此后端。
- 每轮最多发出一次终态通知。完成时在同一锁区摘取该轮缓冲并关闭 active 状态，再在锁外发送最终文本和结果。
- 当前 `emit` 在通道满时会丢事件；终态不得使用可丢弃路径。终态发送需可被 session context 取消，Engine 退出时必须关闭对应 session，避免泄漏。

### 7.5 RPC 分类

扩展当前 RPC 错误结构以保留 code、message、data，并让响应分发分别表达“服务端 JSON-RPC 错误”和“本地传输中断”。`rejectPending` 不再把断连塞进伪造的 rpcError。

明确无轮次和 expectedTurnId 不匹配，只能依据目标 CLI 已验证的结构化信息，或该版本精确错误格式白名单映射为 Unavailable。官方上述页面没有承诺这些失败的固定错误码；不能把所有 invalid params 或含 turn 的文本都当成可回退。未命中白名单的服务端拒绝按 Rejected 处理。

提交进入管道写入后，写入错误、等待超时、EOF、取消以及无法确认的响应均按 Unknown。只有能够证明尚未发出的本地失败才能标成明确未提交。

补充 RPC 建议使用独立命名常量 `appServerSteerTimeout = 10 * time.Second`：这是本设计用于限制补充结算等待的初始值，不是官方限制。测试注入短超时，不依赖真实等待十秒。其他已有 RPC 超时保持原值。

请求成功或失败都不生成额外 EventResult。迟到响应不触发重试，也不能改变另一个请求的状态。

## 8. 接收记录、历史与重复消息

### 8.1 最小持久化记录

在 Session 中增加可选的补充投递记录，键由平台、原聊天键和非空 messageID 构成，归属捕获的 Session ID；同内容、不同 ID 是不同消息。记录至少包含状态、内容摘要/原文、消息创建时间、目标 agent session ID、错误类别和记录时间；协议日志另记录 RPC ID、thread/turn ID。

状态为 Pending、Accepted、Rejected、Unknown、Queued。Pending 在发 RPC 之前持久化；启动恢复时遗留 Pending 视为 Unknown，绝不自动补发。Queued 只表示已转入现有内存队列，不承诺崩溃后恢复执行；重启后残留 Queued 标为待核实并提示原队列可能未执行，不自动重放。

复用 SessionManager 快照、AtomicWriteFile，增加返回 error 的保存入口，原 `Save()` 保持现有调用方式并负责日志。所有保存入口必须共享覆盖“构造快照到落盘”的保存串行化机制，避免较旧快照后写覆盖补充记录；锁顺序统一，不能在持有 session/state 锁时调用保存。

具体保存顺序固定为 SessionManager 管理锁 → 保存串行锁 → 单个 Session 锁；外部入口和已持管理锁的内部入口都复用这一顺序。快照显式复制新增记录及其切片，不能只给 Session 加字段却漏改当前手工构造快照的代码。

写 Pending 失败则不发 RPC，回复本地记录失败；服务端成功但 Accepted 落盘失败时，不重发，内存保留 Accepted 并提示记录保存失败，磁盘 Pending 使重启后的行为保持保守。空存储路径仅允许测试使用，不可将生产环境的 no-op 保存当作持久化成功。

记录至少随当前 Session 保留，未核实项不能自动淘汰；随用户删除 Session 一起删除。本期不引入独立数据库或后台自动核实任务。

### 8.2 各结果的处理

| 结果 | 回调与历史 | 用户反馈 |
| --- | --- | --- |
| Accepted | 原消息 `OnAccepted` 一次；更新 LastUserActivity；user 历史一次；保存 | 已补充到当前任务 |
| Unavailable → Queued | 复用队列接受回调；历史仍在真正发下一轮时添加 | 已加入队列，将在当前任务后处理 |
| Rejected | 不写成成功接收的 user 历史，不触发成功回调 | 补充失败：原因 |
| Unknown | 不触发成功回调，不写成已接收历史；保存待核实记录 | 补充是否收到暂时无法确认，未自动重发；请核对当前任务结果 |

Accepted 的 user 历史与投递状态在同一次 Session 更新中完成，再生成快照；保存后执行回调和回复，最后释放该投递的结算席位。若保存失败，按上一节保留内存 Accepted，并明确提示“已接收，但本地记录保存失败”，不能误报为未接收或再次提交。

重复投递同一 ID：Pending 不再创建 worker；Accepted/Queued 返回对应已有状态；Unknown 返回待核实提示；Rejected 返回原失败，不自动重新尝试。去重范围覆盖同工作区、同聊天的历史 Session，/new 和 /switch 不清除投递证据。当前 Session 使用 admissionMu 串行登记。去重检查先于补充路径的旧消息丢弃检查，避免 Unknown 被静默吞掉。

对没有 messageID 的合成消息，本期保持原队列路径，不用文本哈希假装能提供可靠幂等性。平台本身投递成功的传输确认与 `OnAccepted` 是不同概念，不能依赖不调用后者阻止平台重投。

补充 Accepted 后按最大值更新当前轮用户消息时间；不得改写最初的 currentMessageID、ReplyCtx、流式卡片和 fromVoice。完成水位在补充结算后推进，避免旧消息复投。已有队列未空时不 steer，避免新补充的水位导致先前队列消息被误判过期。

`/history` 的文本和卡片两条渲染路径增加待核实摘要，独立于成功对话历史，显示原消息标识及状态。后续生成新轮次 prompt 不把 Unknown 记录偷偷注入模型。人工核对不能单凭输出缺少关键词认定未接收；明确再次要求发送的新消息视为新的用户指令。

### 8.3 i18n 与日志

在 `core/i18n.go` 为补充成功、补充失败、结果未知、记录失败、会话结束前未提交、历史待核实摘要定义 MsgKey，并补齐 EN、ZH、ZH-TW、JA、ES。普通队列和容量提示复用现有文案。

日志至少记录 project、interactiveKey、cc session ID、message ID、RPC ID、thread ID、expected turn ID、结果类别、耗时；不记录完整 prompt、附件内容、认证环境变量或密钥。对外错误经过已有脱敏函数。

## 9. App Server 命令参数继承

`Agent.StartSession` 已捕获 `cliBin`、`cliExtraArgs`，将它们传入 App Server session，并在构造时复制参数切片。`connect` 使用传入的 binary，默认值继续由 `core.ParseCmdOpts` 提供。

抽取 App Server 命令参数构造 helper，保留 cmd 中的全局选项，把配置覆盖项放到 app-server 子命令作用域，最后加入当前适配已有的 model/effort/provider/baseURL 显式配置。复用现有 exec 参数处理的分组思路；只有确有共同规则才提取共享函数，不调用 `buildExecArgs` 来拼 App Server 命令。

对于当前网络配置，预期有效参数等价于：`codex app-server -c sandbox_workspace_write.network_access=true --listen stdio://`。不追加 `exec`、`resume` 或 exec 专有参数。不通过 shell 重拼命令，带空格的 binary 路径必须保持独立参数。

model 等重复配置遵循现有 Agent 显式设置优先的规则；与启动传输冲突的额外 listen 参数需明确报错，不启动一个管道客户端无法读取的进程。保留 cwd、CODEX_HOME 和环境合并顺序。网络生效必须用独立实例的实际联网结果验证，不能只检查 argv。

## 10. 文件改动清单

| 文件 | 计划职责 |
| --- | --- |
| `core/interfaces.go` | TurnSteerer 可选能力 |
| `core/turn_steering.go`（新增） | 通用失败分类和补充记录类型 |
| `core/engine_steering.go`（新增） | 路由判断、投递 worker、分类与结算 |
| `core/engine.go` | 忙碌入口、结束/drain 接线、历史呈现接线、策略 setter |
| `core/session.go` | 记录持久化、保存结果、快照串行化 |
| `core/i18n.go` | 新提示的五种语言 |
| `agent/codex/appserver_session.go` | Steer、输入共享、轮次通知、RPC 错误、启动参数 |
| `agent/codex/codex.go` | 传递 binary 和参数到 App Server |
| `config/config.go`、`cmd/cc-connect/main.go` | 校验开关并设置 Engine 策略 |
| `config.example.toml`、`README.zh-CN.md` | 默认队列、启用方法与 Unknown 行为 |
| `core/engine_steering_test.go`（新增） | 路由与竞态回归 |
| `core/session_test.go`、`core/user_message_watermark_test.go` | 重启记录与水位回归 |
| `core/cuj_test.go` | 用户视角多步旅程 |
| `agent/codex/appserver_session_test.go` | 协议、通知顺序、传输失败、输入回归 |
| `agent/codex/workspace_command_test.go`、`codex_model_test.go` | 扩展现有命令和 backend 继承用例 |
| `config/config_test.go`、启动组装层对应测试 | 默认/非法配置及策略真正生效 |

仅对本功能涉及的复杂逻辑做提取；不重构其他 Agent、平台或全项目 Engine 架构。新增类型、函数、关键条件和常量添加中文注释，沿用现有 Go 风格。

## 11. 测试与验收矩阵

所有竞态通过 channel/barrier 控制顺序，不用长 sleep 碰运气。Codex 边界复用现有管道、阻塞 writer 和事件测试桩，测试直接经过真实 request/readLoop/通知处理。

| 用例 | 必须断言 |
| --- | --- |
| `SteerActiveTurn` | 一次 turn/steer，thread/expectedTurnId 正确；无第二次 start |
| `SteerMultipleMessages` | 连续多条按登记顺序投递，各确认与历史一次，仅原轮终态 |
| `SteerAttachments` | 文件引用和图片输入齐全、消息目录隔离、准备失败不提交 |
| `StartResponseAfterCompletion` | started → item → completed → start response 不复活、不清空已输出内容 |
| `DuplicateAndForeignNotifications` | 重复 started/completed 幂等；其他 thread/turn 不污染文本、状态或审批 |
| `IdleDoesNotCompleteTurn` | idle 不误终止新轮；真正 completed 仍完成一次 |
| `SteerTurnEndedFallsBack` | 明确无轮或 ID 不匹配只排下一轮，保留容量和顺序 |
| `SteerTimeoutDoesNotResubmit` | 已写入后不回响应，Unknown；同 ID 再来、迟到响应均无第二次 RPC/start |
| `SteerTransportErrors` | 写阻塞、部分写入后失败、EOF、取消、坏响应分类正确，无自动重试 |
| `SteerRejectedDoesNotQueue` | 参数/权限/方法错误显示失败，不隐式排队 |
| `SteerCompletionBarrier` | 完成先于确认时，历史仍属于旧轮；下一轮要等结算，原接收循环不死锁 |
| `SteerStopAndReplace` | 停止、/new 或会话替换不把旧投递送到新 session |
| `SteerKeepsPrimaryReplyContext` | 补充确认回复发送者，最终结果保持原轮卡片/回复上下文 |
| `SteerReceiptsSurviveRestart` | Pending 恢复为 Unknown；已接收/待核实同 ID 均不重放 |
| `SteerReceiptSaveFailure` | 预保存失败时 RPC 为零；确认后保存失败不重发；并发保存不回退状态 |
| `SteerWatermarkAndQueueOrder` | 已有队列不被绕过；成功水位按最大值；同 ID/旧消息不重复历史 |
| `SteerConfigCompatibility` | 缺省 queue；非法值报错；不支持能力正常排队；配置真正传到 Engine |
| `AppServerCommandAndWorkspaceOptions` | string/argv cmd、带空格路径、网络参数、backend、stdio、model、effort 保留且切片不别名 |
| `SteerTwoWorkspaces` | A/B 同时执行，补充仅改变 A；相同平台 messageID 在不同会话不互相去重 |

CUJ 使用真实 Engine + SessionManager，只模拟平台发送和 Agent 外部边界，至少三个用户动作并断言 `getSent()`：

1. 发报告请求 → 发年份补充 → 发格式补充 → 看最终报告 → `/history`：只出现一次最终报告，历史各一次。
2. 发报告请求 → 发补充且模拟确认超时 → 同 ID 重投 → `/history`：展示待核实，没有下一轮重复报告。
3. A/B 分别发请求 → A 补充 → 查询双方结果：只有 A 包含补充。扩展已有 H4 工作区隔离 CUJ 的相关子场景；停止相关场景扩展 C4。其他新增 CUJ 编号在实施时对照权威 inventory 分配，不冒用现有编号。

验证命令：

```powershell
# 定向验证协议、路由、历史与继承功能。
go test ./agent/codex ./core ./config -run 'Steer|AppServer|WorkspaceAgentOptions|Watermark'
# 显式运行用户关键旅程。
go test ./core/ -run TestCUJ
# 执行仓库要求的完整检查。
go build ./...
go test ./...
go vet ./...
# 在支持 Go race detector 的环境检查并发读写。
go test -race ./core ./agent/codex
```

修复已有行为必须保留能在基线失败、修复后通过的回归用例，尤其是命令丢失和 start 响应复活竞态。若 Windows 出现已有失败，用同一命令在基线复现并记录差异；新失败必须修复，历史失败不能被静默忽略，也不能宣称全量通过。

## 12. 实施顺序与发布

### 阶段 A：协议和测试基础

先为启动参数遗漏、响应/通知竞态补失败测试，再实现参数传递、错误分类、轮次状态与公共输入构造。使用目标 CLI 获取无活跃轮次、expectedTurnId 不匹配和方法拒绝的真实错误样本，固定为兼容测试；不从网页示例猜错误码。

### 阶段 B：Core 能力与投递

先补正常补充、失败矩阵、结束屏障、两个工作区不串消息的测试，再实现能力接口、会话投递、持久化记录、历史与 i18n。通用路由测试使用实现 TurnSteerer 的 stub，避免 core 导入 codex。

### 阶段 C：配置接线与完整验证

实现默认 queue 的解析和 Engine 接线，扩展现有 WorkspaceAgentOptions 测试，更新示例文档。执行定向、CUJ、全量和并发验证，记录实际命令、结果与基线差异。

### 阶段 D：独立实例验证

使用独立配置、data_dir、工作区和测试聊天入口，避免同时消费生产聊天消息；测试目录仅放合成报告素材。记录实际 Codex binary、版本和生效参数。

按“较长报告 → 2026 年约束 → 最终报告”执行，并核对同 thread ID、同 turn ID、一次 turn/start、补充 turn/steer、一次终态。再验证多条补充、两工作区、结束时补充、断连不重投以及无业务副作用的联网请求。

合成数据测试不得调用真实导账上传。若无法获得独立聊天入口，可完成协议测试但不能标记真实平台实测已通过。

### 阶段 E：生产切换与回滚

独立实例通过后，按现有 web 构建流程和 Windows `goolm` 构建标签生成候选程序，记录版本和 SHA256。定位真实安装 binary、启动方式和配置路径，不使用失效旧路径或猜测 npm 安装位置。

确认当前导账任务已经完成、没有未结算补充后，备份程序与配置，设置目标项目的 backend、stdio URL、steer 开关，再替换并重启。原有 full-auto、模型、provider、网络参数和工作区绑定继续保留。

重启后确认平台连接、配置加载、工作区绑定、已有 session resume 以及一次合成报告补充。若失败，恢复备份程序和配置；如果只有补充策略异常，可将开关退回 queue。所有回滚均不得自动重放 Unknown 记录。

## 13. 评审结论与明确限制

本稿推荐采用可选能力接口和会话内有序投递。相比最初六点方案，新增的必要约束是：完成结算屏障、结构化 RPC 错误、可持久化的待核实记录，以及有身份校验的轮次状态更新。

“补充成功”表示服务端确认接收输入，不保证模型一定完全遵循，也不表示已完成操作会撤销。协议缺少本业务的端到端幂等键；本期承诺桥接层不自动重复提交结果未知的同一条消息，不宣称任意故障下 exactly-once 执行。

当前已核对源码、官方协议文档与本机 CLI 帮助。真实错误格式、生产 binary/config 路径、独立聊天入口和目标版本完整事件序列属于实施及发布门槛；本稿没有把这些未实测项目写成已通过。


## 12. 实施审阅补充（2026-09-16）

回退到普通队列的记录，在实际 Send 前重新保存 Pending；返回成功后确认为 Accepted，返回不确定错误时标记 Unknown。发送前保存失败、停止或撤回均结算为明确未提交，不永久停留 Pending/Queued。持久化记录与普通消息历史沿用现有 Session 保存机制。

实现与验证结果见 `../reviews/2026-09-16-codex-turn-steering-review.md`。当前状态为代码已实现并审阅，生产启用条件尚未满足。


## 13. 启动阶段补充修订（2026-09-17）

用户批准将“仍在启动时立即排队”修改为等待本轮启动确认：进程创建前使用可选 TurnSteeringAgent 声明能力，补充绑定每次 Send 的独立批次。Codex Send 返回意味着已收到 turn/start 响应。失败、停止或一分钟等待超时明确未提交；已发送 Steer 的未知结果规则不变。详细实现和验证见 `../reviews/2026-09-17-steer-startup-review.md`。
