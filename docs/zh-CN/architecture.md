# 架构与安全设计

## 架构目标

QCode 保持一个本机 Web Supervisor，并为每个已注册 Workspace 构造一个权威执行
Runtime。Host 按 Workspace 路由 Operation 并观察 Event，不复制 Agent 循环，也不
直接执行特权工具。

受支持的产品 Host 只有本机 Web。它只绑定
`127.0.0.1`，使用同源 HTTP RPC 与下行 WebSocket；项目不提供 LAN、公网部署、
Pairing/QR Flow、通用 REST/SSE Host 或 MCP Server 入口。外部 MCP 只作为受治理的
Tool Source 接入。

```text
Web
                 |
          Web Supervisor
         /       |        \
 Runtime A   Runtime B   Runtime C
     |           |           |
 Operation / Event + Agent Engine
     |           |           |
 Context + Provider + Guarded Tool
                 |
       Policy -> Approval -> Journal -> Sandbox
                 |
       Shared Persistence + Observability
```

## 包分层

| 层 | 路径 | 职责 |
| --- | --- | --- |
| 入口 | `cmd/qcode` | 进程上下文和 Web 启动入口 |
| Host | `internal/host` | 用户/客户端 I/O 与呈现 |
| Runtime | `internal/runtime` | 协议、应用状态、Agent 循环、装配 |
| Adapter | `internal/adapter` | 模型、Provider、Tool、MCP、Skill |
| Security | `internal/security` | Policy、Permission、Constitution、Sandbox |
| Orchestration | `internal/orchestration` | Subagent、Admission/Budget、Chat Merge、Exec Settle |
| Persistence | `internal/persist` | 关系状态、Event、CAS、Session、Snapshot、Journal |
| Observability | `internal/observability` | Usage、Trace、Receipt、Diagnostics、Verify、Telemetry |
| Platform | `internal/platform` | 进程、PTY、操作系统差异 |
| Configuration | `internal/config` | 默认值、TOML、环境变量、校验、Provenance |

## 硬依赖规则

1. `runtime/protocol` 不依赖其他实现包。
2. Host 不直接 Import 并调用 Provider、Tool、Sandbox 或 Agent Engine 实现。
3. Model/Tool/Security 的构造属于 `runtime/app/wire`。
4. Turn 业务循环属于 `runtime/agent`。
5. 所有有副作用工具都经过 `adapter/tool/guard`。
6. UI State 是 Projection，不是 Runtime 事实来源。
7. 持久化写入在所属边界内使用事务或 Journal。
8. Guard 授权后的副作用尝试必须绑定规范化 Execution Operation 和单次 Lease。

Architecture Test 会检查重要 Import 限制。需要违反这些规则的设计必须先进行显式架构
调整，不能用局部捷径绕过。

## Runtime 组合根

`runtime/app/wire.NewExec` 是装配入口，不是 Service Locator 或业务 Workflow。它创建
仅用于构造期的 `buildState`，并执行封闭的 Module 序列：

```text
config -> provider -> persistence -> platform -> builtin tools
       -> capability tools -> security
       -> orchestration -> observability -> agent -> runtime
       -> background services
```

每个 Module 只拥有一个构造边界，并仅向后续 Module 暴露必要结果。模块序列的数据依赖
由显式 Module Contract 声明并在任何构造前校验：每个 buildState 域只有一个写入者，
读取必须由序列中更早的模块写入；违反声明的重排会在构造期以模块名和域名失败。
Runtime、Engine 和
Session Service 都不得持有 `buildState`。Persistence 拥有 Content、Job Log 和
SQLite 基础；Platform 拥有 Process、Sandbox 与 Repository Index；Orchestration
拥有 Subagent、Admission/Budget、Child Worktree/Toolset、Chat Merge
与 Exec Settle 构造。
Provider 显式输出 Provider/Model Catalog，Security 显式输出 Permission Store 与
Guard Factory。

Agent Module 只计算一次 Runtime Core Seed（Provider、Context、Tool、Security 与
Lifecycle Options）。Concrete `runtimeCoreBuilder` 从该 Seed 构造 Main Engine 和
Child Engine，统一完成 Security Clone、Guard Factory 绑定、Workspace Identity
适配和构造失败回滚；Child 的 Role、Budget、Toolset 与 Worktree Authority 仍以显式
`ChildSpec` 覆盖，不能扩大 Parent Authority。

`internal/security/authority` 拥有执行授权数据模型。它把已验证的 Tool Invocation
规范化为带 Resource Namespace、Root Generation、Subject、Effect Contract 和
Required Controls 的 `ExecutionOperation`，并由 Runtime 共享的 `LeaseAuthority`
签发单次 `ExecutionLease`。Guard 保留 `ExecuteBound` 作为兼容 Facade，但每个实际
Attempt 都会先签发和消费 Lease，再把 Operation/Lease/Settlement 证据投影到
`tool.result.execution`。Process Broker 接管 stdio MCP 生命周期，模型侧不再开放
宿主进程冒烟工具；File/VCS Broker 接管模型文件工具、Agent/Chat Merge、生成型 Workspace
输出和 Git Metadata Mutation。`workspacebroker.Runtime` 只组合窄能力，不把 Broker
业务逻辑放入 `wire`。

Builtin、Skill、Memory 与 MCP Tool 共享同一个 Registry 实例。Composition Root
按固定顺序直接构造 Skill Catalog、Memory Store 和 MCP Pool，并只向后续模块发布
必要结果。Subagent 工具由 Orchestration Module 单独装配。

Registry 分别冻结模型可见的 `ExternalDescriptor` 与执行权威
`TrustedBinding`。External Requested Effects 只用于呈现和审计；Guard、Policy、
Authority、Journal、Sandbox 和验证证据接纳只消费 Trusted Binding。外部 Source 必须
由可信 Host Policy 显式绑定权限，Deferred Loader 不能改变冻结的 Binding。
工具 `Descriptor` 可声明 `identity_keys`；窗口压力把调用参数收成这些身份字段。
未声明则用公开白名单并入 InputSchema 的 required 标量，`command` / `content` /
`patch` 只有显式声明才保留。

Trusted Binding 以十维 Required Controls 描述操作需求，Sandbox Probe、Policy 与具体
Command 共同产出 Effective Controls。Authority 在 Lease 签发时执行逐维集合比较，
Process Owner 在 Backend `Prepare` 后再次校验本次命令的 Prepared Controls。
旧 `Strength` 能力字段已删除；展示和缓存身份直接由 Effective Controls 派生。
十维的枚举、校验、满足关系、投影与 Identity 顺序由 `controlmatrix` 的单一规格表
声明；新增维度必须同步扩展该表，字段级完备性测试会锁定遗漏。

Broker 副作用统一经过 `authority` 的 `RunSettled` 事务骨架：消费租约、以失败为
初始结算执行副作用、恰好一次带时间戳的结算，结算错误与业务错误合并返回；
新增 Broker 不再各自展开 Consume/defer-Settle 样板。

Runtime 构造具有 Prepared 状态。`RuntimeModule` 只构造 Facade 并恢复静态 Durable
State，不接受 Operation；`BackgroundModule` 依次执行 MCP 初次 Refresh、启动 Runtime
的 Terminal Outbox/Pending Turn Recovery，再启动 MCP Prewarm。任一步失败都会终止
构造并由 ResourceStack 回滚；Runtime Recovery 成功前不会接受 Operation。

当 Web 启动时没有显式或已保存的 Provider/Model，Host 先进入受限 Setup 状态，不构造
默认 Runtime。该状态只暴露受同源 Capability Token 保护的 `setup/apply`；用户提交的
API Key 写入操作系统 Keyring，Provider、Model、Endpoint 与协议写入 Runtime 管理的
非敏感 Setup Record。只有这些事实持久化成功后，Host 才为已添加的 Workspace 调用
`wire.NewExec` 并激活 Runtime；没有 Workspace 时仅激活 Supervisor 的目录管理界面。

Web 进程持有一个全局 Owner Lease 和持久化 Workspace Registry。首次启动创建
Supervisor；普通 `qcode` 启动不把当前目录、安装目录或源码目录注册为 Workspace，
仅恢复 Registry，空列表也是合法状态。启动器按配置 Provenance 区分 Runtime 内部默认值
与用户显式指定的 Workspace。只有 `--workspace`、显式配置或用户添加目录时，
才通过 Lease 中仅对当前用户可读的 Capability Token 调用已有 Host 的 `workspace/add`，
不会启动第二套控制面。连接配置和凭证生命周期属于 Supervisor，零 Workspace 时
不构造替代 Runtime 或授予目录访问权限；每次添加目录仍校验其与 State Root 不重叠。每个
Workspace 单独拥有 `wire.Session`、Sandbox、Tool Registry、Repository Index 和
MCP 生命周期。共享 SQLite 中的 Session、Event Recovery 与 Terminal Outbox
按规范化 Workspace Root 过滤；关闭一个 Runtime 不能关闭 Supervisor 持有的共享
Store。

构造与关闭共享 `wire.ResourceStack`。Session 只注册一次资源关闭函数；部分构造
失败回滚与正常关闭都按注册逆序关闭同一 Stack。每项资源最多关闭一次，单项关闭失败
不会跳过后续资源，调用方会收到带资源标识的聚合错误。因此 Runtime 等后段构造失败
也不会泄漏已创建资源。

## Runtime 所有权图

```text
Web
        | Operation / Event
        v
OperationService -> TurnService -> TurnCoordinator -> TurnScope
        |                |               |
        v                v               v
 operationDispatcher  ActiveTurnRegistry  Context Authority Snapshot
        |
        v
    EventService -> eventhub.Hub <-------- Event Projection
        |
        +-> TerminalPublisher -> app/persistence -> SQLite / Event Log / CAS
        |
        +-> SessionService / ArtifactService -> Host Query

wire.NewExec -> 仅负责构造 Module
orchestration/chatmerge.Service -> 隔离 Chat Preview / Journal Apply / Git Baseline
eventview + Web Projection -> 仅负责 Host Presentation
```

| Owner | 路径 | 独占职责 |
| --- | --- | --- |
| Composition Root | `internal/runtime/app/wire` | Concrete Construction 与 Resource Registration |
| Durable Runtime Assembly | `internal/runtime/app/persistence` | Repository、Lifecycle Recovery、Persistent Runtime Options |
| Chat Merge Service | `internal/orchestration/chatmerge` | Isolated Baseline、Three-way Preview、Journaled Apply |
| Exec Settle | `internal/orchestration/execsettle` | 命令级隔离工作区、写树三方结算 |
| Operation Service | `internal/runtime/app` | Queue、Idempotency、Typed Dispatch 与 Operation Commit/Reject |
| Turn Service | `internal/runtime/app` | Active Lease、Control、Cancel Provenance 与 Turn goroutine 生命周期 |
| Event/Recovery Service | `internal/runtime/app` | Event Projection 索引、Observer 与 Durable Recovery |
| Turn Coordinator/Scope | `internal/runtime/agent` | Reducer Authority、Effect、Control 与 Turn-local State |
| Event Hub/Terminal Publisher | `internal/runtime/app/eventhub`、`internal/runtime/app` | Sequence/Fanout 与 Atomic Terminal Publication |
| Subagent Control | `internal/orchestration/subagent`、`internal/orchestration/admission` | Agent Graph、Budget、Concurrency 与 Worktree Authority |
| Skill Control | `internal/runtime/app/extension`、`internal/adapter/skill` | Skill 状态、Lock、控制操作与 Receipt |
| Trace/Usage Plane | `internal/observability/trace`、`internal/observability/usage` | Span、Latency、Token、Cost 与查询投影 |
| Session/Artifact/Trace Service | `internal/runtime/app` | Runtime-owned Port 上的 Host-facing Query 行为 |
| Agent Preset Service | `internal/runtime/app`、`internal/persist/agentpreset` | Workspace 范围的版本化 Preset 校验、原子持久化与 Session 应用 |
| Benchmark Projection | `internal/runtime/eventview` | Go Benchmark 的 Typed Event Interpretation |
| Web Projection | `web/src` | 浏览器端 Event Projection 与交互状态 |

Web 直接调用 Runtime 的窄化 Session、Operation、History 与 Artifact Service。
浏览器 Transport 不复制 Agent 循环，也不存在第二条兼容执行路径。

`Runtime` 是这些 Service 的兼容 Facade，不直接拥有 Operation Map 或 Active Turn Map。
Session 生命周期写操作由 `SessionService` 的 mutation lock 串行化。`Engine.Execute`
是唯一生产 Turn 入口；它冻结 `TurnRequest` 和 Context Authority，并为每个 Turn
创建隔离 Scope。`turnkernel.Reducer.Apply` 是唯一状态转换入口，其实现按 Command
Family 分布在 `reducer_sampling.go`、`reducer_tool.go`、
`reducer_interaction.go`、`reducer_context.go`、
`reducer_verification.go` 和 `reducer_terminal.go`。

Engine 对外发布的 `State` 是 Kernel `Phase` 的呈现细化，不是第二套权威状态机：
采样拆分为 Compaction 与传输细节，工具执行拆分为准备、运行与回填。两套词汇的
合法共存关系由 `internal/runtime/agent/engine` 中的单一映射表声明；映射表必须
覆盖两侧全部常量（源码级完整性测试锁定），事件发射点按该表校验，违规记录为
Terminal Secondary Issue 而不改写业务终态。

## Runtime 协议

协议定义位于 `internal/runtime/protocol`，生成后的公开 Schema 位于
`docs/protocol/runtime-protocol.schema.json`。

概念模型：

- **Operation**：请求的状态转换，例如开始或取消 Turn。
- **Event**：状态转换产生的不可变事实。
- **Receipt**：上下文、工具、变更、审批、验证或成本的结构化证据。
- **Projection**：由 Event 和关系记录重建的查询状态。

Web Transport 只负责鉴权、序列化和事件投影。除非是有意的 Host 呈现差异，只在一种
Host 中存在的 Runtime 能力都不完整。

Event 分类是 Protocol 数据，而不是 Host Policy。`event_traits.json` 是唯一生成源，
生成 Go Trait Table、Protocol Schema、TypeScript Table 与 Golden；新增 Event 缺少
Class、Item Owner、Durability、Correlation 或 Terminal Trait 时生成直接失败。
Go Benchmark 消费 `eventview` 的 Typed Semantic Update，不再分类 `Event.Data`。

Provider Delta 按消费进度合并：`Recv` 立即交付已有片段，不等待字节阈值或定时窗口；
只有下游忙碌、上一批尚未取走时才合并同类型、同索引且工具身份兼容的片段。公开合同
`assembly.MaxCoalescedDeltaBytes` 沿用原有 1 KiB 批次预算，作为待消费批次的载荷上限，
不作为交付条件；追加片段会超过预算时停止合并。单个超预算 Provider Event 原样交付，
不拆分协议事件。每条流最多保留一个待消费批次和一个正在读取或等待交接的源事件，
满批与类型、索引、工具身份、用量、终止或错误边界均向上游施加背压。
关闭会唤醒消费者和等待交接的生产者，并关闭源流以终止阻塞读取。

阶段说明使用独立的 `commentary.completed` 持久事件，包含稳定 Message ID、Sample ID、
正文与关联 Call ID；Thread/Turn 归属来自事件信封。普通协议在完整逻辑采样成功后，
将伴随常规工具调用的普通文本分类为阶段说明，并随采样结果原子写入 Kernel 状态，
在执行工具前发布。纯文本、单独的 `turn_complete` 和收敛收尾仍使用现有暂存输出及完成
门禁；阶段说明不进入最终答案、不计为工作进展、不提供审批或验证权威。
重试与续传按逻辑 Sample 归并，未确认响应不发布。恢复时从已接受状态补发；终态 Outbox
再次补发遗漏消息，与实时发布共用稳定 Event ID 去重，不新增消息数据库或后台摘要任务。
Web 按 Message ID 保留独立节点，迟到补发的说明依据关联工具恢复 Chat 位置，不修改
审计事件的 Sequence。Child Thread 复用同一链路，阶段说明留在对应 Subagent 执行块。
原始 assistant history 保留正文。无状态投影不再因为正文伴随已闭合工具调用就删除
它；可见 Tail 里的阶段说明仍作为普通 assistant 正文发给模型。超硬输入时，这些
正文随已闭合因果组进入 Truth Capsule，或被收成带 tool-call 来源的非权威摘要。
不把阶段说明重复注入 System 分区或升级为权威事实。

正文 Delta 以瞬态 `output.draft` 事件实时下发，载荷携带逻辑 Sample ID，不进入
持久事件日志；`output.delta` 语义不变，仍只在 Terminal Envelope 提交后由终态
Outbox 发布权威正文投影，提交失败零泄漏的基线契约保持不变。采样正文是否为最终
答案只有在样本闭合时才可知：样本最终产出常规工具调用时，其正文按阶段说明处理，
引擎在进入工具阶段前发布瞬态 `output.discarded`（携带 Sample ID 与原因）撤销该
样本已流出的草稿，说明本身仍由 `commentary.completed` 持久补发。输出中断走既有
续写语义，已流出草稿保持追加。Web 对同一输出节点按 Sample 分段累积草稿，撤销仅
移除被命名样本的分段；终态事件到达后以完整权威正文覆盖，历史重放只看到
`output.delta`，与实时路径一致。

会话自动命名是独立的非权威元数据操作：`SessionService` 在未命名会话的 Turn 被接受后
立即调度无工具 `summary` 采样，不等待 Turn 成功结束，也不把 Prompt 截取为临时标题。
结果返回前保留 `New Chat`。标题来源与独立命名版本放在
既有 Session 生命周期元数据中，持久层以来源、版本及活动 Thread 做事务比较写入，
不受置顶等无关修订干扰，不覆盖手动标题或复活已删除会话。`session.title.updated`
通知 Web 刷新权威会话列表，而不是往 Chat 追加答案。同一会话单飞、同一 Turn 不重复命名，
失败后允许后续 Turn 再尝试；旧 temporary 来源也可由真实模型结果替换。请求受 Runtime
关闭取消，取得共享 Provider 并发许可后才开始计算摘要超时，避免排队耗尽实际采样时间。
命名与主采样共享并发上限和 Retry-After，不因前台 Turn 活动而整轮禁用。Engine 在构造、
Profile 和 Turn 边界发布配置快照，命名不获取整轮 Engine 锁；独立锁记账供后续预算检查使用。
标题采样使用现有会话预算检查和 Usage 事件，既不修改业务终态，也不加入模型对话历史。

Web 将正常采样、工具准备与执行、工具回填投影到单一运行状态栏，不为正常的
`stop_reason=tool_use` 或成功工具结果创建聊天状态卡片。限流、重试、输出不完整和真正的
终态错误仍保留可检查的提示；终态清除运行状态，历史重放使用同一投影规则。

Web Unary Route 以 `internal/host/runtimeapi/web/contract.go` 为唯一清单。
`webprotocolgen` 从该清单生成公开 Transport Manifest、TypeScript Route Union 和
Go Handler Table；Handler 方法名由路由分段确定，例如 `session/create` 对应
`sessionCreate`。服务端不得再维护平行的字符串 Dispatch Switch。

Web Client 使用 Runtime Snapshot 完成 Hydration，再按当前 Workspace 的 Cursor 消费
Event。持久层 Sequence 在 Supervisor 内全局严格单调，浏览器则按 Workspace 分别保存
Cursor；只有对应 Runtime 明确报告 Retention Gap 时才进入 Desync。
Snapshot 与加载期间缓冲的 Live Event 合并后立即显示正文，Profile、队列与辅助面板
独立更新；Profile 和队列未成功就绪前仍禁止提交会话操作。切换 Session 时取消旧加载，
并用选择代次拒绝迟到结果，旧辅助查询不能覆盖已经由实时事件刷新的面板。
计划与 Agent 进度的初次加载和事件刷新共用一条单次在途查询链，两个查询并行执行。
查询期间收到的进度或终态事件合并为后续刷新，已失效的响应不写回面板；
单项失败保留该项最后成功的结果，其他成功结果仍可更新。切换 Session、Workspace、
重置投影或停止客户端时取消查询，迟到响应不能覆盖新选择的进度。
Composer 草稿在输入事件中立即写入当前 Workspace/Session 的浏览器内存，
仅持久化层合并写入 IndexedDB；切换 Session、Workspace 或离开页面不等待 UI 防抖。
草稿读取同步使用内存，Workspace 的存储 Scope、草稿和选中身份一起切换；
加载目标 Workspace 期间继续输入的文本仍保存到原 Workspace。发送成功清空草稿，
提交失败保留原文。
浏览器 Conversation Projection 对高频 Delta 按动画帧合并发布，并保持未变化业务节点
的引用稳定。Chat 的终态、用量和刷新水位使用独立的非 Delta 事件视图，追加流式文本
不重建这些统计；切换 Session、替换 Snapshot 或加载较早历史时重新建立该视图。
历史消息的反馈回调与 Markdown 渲染保持稳定引用，避免每帧重复解析已完成回答。
流式 Markdown 使用 React Deferred 更新让出高优先级输入，终态直接使用完整权威正文，
不裁剪文本、不人为添加逐字延迟，也不按不可靠的字符串边界拆分 Markdown。
对话区的阅读、文本选择及控件交互优先于底部跟随：在按下、键盘激活或获得焦点时
保存实际交互目标的位置，流式更新与详情展开期间保持该锚点。用户滚动、主动导航、
切换会话/视图或返回底部时解除交互锚定。程序恢复位置所产生的重复滚动事件不重新
判断跟随意图；内容收起后仍遵守浏览器合法滚动范围，不人为增加空白空间。
用户滚动事件同步保存阅读位置和窗口边界，导航统计与历史加载检查再按动画帧合并，
避免已排队的流式渲染恢复旧位置。相邻执行细节始终使用稳定的阶段组件，
首条阶段说明只增加折叠入口，不重建工具内容。运行中的 Turn 到达终态时，
若用户已停止跟随底部，则保留原有执行布局；回到底部后再转入默认折叠的历史布局。
初次加载的历史 Turn 仍默认折叠，之后上翻不会重新展开已经收起的执行过程。
弹窗关闭只恢复焦点，不隐式滚回原触发控件；该焦点恢复也不能覆盖正在执行的显式导航。
Trajectory Event Ledger 与 Chat 复用完整事件窗口；`trace/query` 只补充
经过 Session/Turn 归属校验和字段白名单投影的时序，不返回任意 Span Attribute。
Trace 按已加载 Turn 维护查询集合，终态到达时只更新受影响及尚未完整的 Turn，
加载较早历史时补充新增 Turn，返回结果按 Turn 合并。只有活动 Recorder 已释放、
持久 Trace 根与公开 Span 均已结束时，服务端才返回 `complete=true`，客户端才缓存
该终态结果；未完整或不可用的结果在后续刷新时继续查询。`through_sequence` 保持
已确认的事件水位语义，不代表 Trace 的增量游标。
Runtime 已验证并实际传给模型的图片输入会编码进 `turn.started`，使用户消息图片能够从
持久化 Event 恢复；Presentation Snapshot 预算覆盖一个完整的最大图片输入。

Workspace Runtime 固定 Provider Connection、Endpoint、Credential Reference 与 Egress
边界，Session Profile 则独立持久化 Model ID。Engine 只允许在同一 Provider
Connection 内从当前 Ready Route 派生新的 Model Route，并在 Turn 开始时冻结到
`TurnSpec`；它不能借模型切换改变 Endpoint 或 Credential。Web Model Catalog 将内置
目录与当前 Workspace 已持久化 Session Profile 中的模型合并，因此用户输入的新 Model
ID 在刷新、重启和其他 Session 中仍可选择。Active Turn 期间拒绝 Profile 修改。
Connection 设置通过 Host 控制面切换 Provider：先拒绝新的 Runtime 操作并确认全部
Workspace 空闲，再构造新 Runtime、事务化迁移 Session Route，最后替换旧 Runtime；
任一步失败都会保留或恢复原连接。
首次配置与 Connection 设置的模型探测使用同一 Provider 边界：内置 Provider 的端点和
协议来自目录，自定义连接采用用户选定的 Chat Completions 或 Responses 协议。
探测分别请求 `/chat/completions`、`/responses` 或 `/messages`，并复用对应流解析器；
Messages 必需的输出上限只取服务广告或已知目录值，缺失时不猜测。目录外模型可探测后
补充缺失容量，也可显式填写元数据；探测失败仍可重试或手动填写。提交前继续校验正整数
容量、输出不超过窗口以及工具调用能力，探测操作本身不修改连接或重启 Runtime。

### Application Ownership

Application Runtime 是显式 Owner 组成的 Facade：

- `operationDispatcher` 将 8 类 Operation Payload 映射到强类型 Handler。Handler
  返回包含 Events、Async Turn Identity、Typed Problem 与显式 Commit Mode 的
  Outcome；只有 Dispatcher 执行同步 Commit 与 Rejection。
- `ActiveTurnRegistry` 原子预留 Thread 与 Turn，绑定 Control、Cancel Provenance、
  Profile Revision，并通过内存 Token 拒绝 Stale Release；持久 Lease ID 直接使用已
  持久化的 Start Operation ID。Pending-work Phase 来自权威 Turn Kernel Snapshot。
- `eventhub.Hub` 独占 Sequence 分配、Append、Replay、Subscriber Fanout、Slow
  Consumer Policy 与 Close。
- `TerminalPublisher` 独占 Atomic Terminal Commit、Deterministic Outbox Publish
  Identity、Event Hub Projection 与 Restart Recovery。
- `SessionService` 拥有 Lifecycle、Profile 与 Tool Catalog；`ArtifactService` 拥有
  Checkpoint、Plan、Turn Recovery 与 Persistence。Runtime 直接暴露窄化的 Host
  Query Method，不再保留平行的 Interface-only Package。

## Turn 数据流

执行前，Engine 构造不可变 `TurnSpec`，冻结 Identity、Request、Session Profile、
Route、Policy、Limit、Prompt Prefix、Tool Catalog、Skill 与 MCP Health。Engine 内的
Scope Factory 从该 Spec 打开单 Turn `engine.Scope`；Scope 运行期间 Sampling
不得重新读取这些可变来源。

Scope 独占 Turn 级 Kernel、Trace、Diagnostics、Verification、Tool Spend、Diff 与
Control State。Cancel、Steer、Approval、Input 统一进入 `ControlPort`；有界 Mailbox
拒绝溢出，Request Ledger 拒绝 Late、Duplicate 与 Kind Mismatch Resolution。

1. Host 校验用户输入并提交 Operation。
2. Application 解析 Session、Thread、Workspace 和 Policy。
3. Prompt Context 组装 Repo Map、Pin 文件、Working Set、Evidence、Policy 与压缩历史。
   `internal/runtime/agent/contextview` 将权威状态编译成 Provider 可见的有界视图，并集中
   实现 Economic Admission、Stateless 投影和内容安全 Prefix Manifest；它不拥有
   Durable History，也不执行 Provider 或 Tool。
4. Coordinator 请求 Provider Sample Effect；`DurableEffectDispatcher` 在 Engine
   调用 Provider 前持久化 `EffectStarted`。
5. 模型 Text、Usage 与 Tool Proposal 通过 `ModelSampleResultReceived` 一次返回。
6. Reducer 持久化 Sample Result，并在 Executor 投影前将 Tool Proposal 转换为 Tool Effect。
7. Tool Executor 进入 Registry 和 Guard；Guard 评估 Mode、Posture、Permission、
   Constitution、Approval 与 Sandbox。
8. Tool、Approval、Input Result 以一个可保留重试的 Result Command 返回；Coordinator
   在 Host Projection 前持久化逻辑闭合。
9. 修改型 Tool 先生成不可变 File Plan；File Broker 消费单次 Lease，在 Journal
   Before/After 与 descriptor-relative Workspace API 之间提交或回滚。Git Metadata
   Mutation 由独立 VCS Broker 执行。
10. 交互式主 Turn 必须选择合法 Runtime 状态：`request_user_input` 创建可持久化的 Input
    Wait，Tool Call 继续同一个 Turn。停轮信号是模型停止调用工具并写出非空用户可见正文；
    `turn_complete` 是可选的：`status=complete` 且 `output_mode=exact` 时用 `summary`
    精确替换已捕获正文，`status=incomplete` 记录可恢复的 Pending Actions。
    Provider `message_stop` 只结束一次 Sample。后续工具批次不再清空已捕获正文。
    `submit_plan` 的 `purpose=deliverable` 保存未来实施方案，不替换当前执行清单、
    不授予执行权限，也不参与完成门禁；因此仅交付计划的 Turn 即使写入了计划文件，
    也可在该文件验证完成后结束，方案中的未来步骤仍为 pending。
    `purpose=execution`（省略时的默认值）和 `update_plan` 管理当前执行清单，
    但不构成完成门禁：未完成步骤留在 Session Plan 上供后续 Turn 继续。
    步骤签名未变的 `update_plan` 会被拒绝，不写 `plan.delta`，也不续期进展 Lease。
    被拒绝的 `turn_complete` 调用身份不算结构化进展。已捕获正文非空时，
    `turn_complete` 可以使用 `output_mode=preserve_provisional` 保留正文并追加
    简短收尾；正文为空时该模式被拒绝。Runtime 不根据正文措辞推断必需输入。
    Child Executor 没有 Input Host，不能等待用户输入，但同样可以靠无工具正文停轮。
11. `EvaluateTurnStep` 由 Reducer 选择 Repair、Verification、Finalize、Block 或
    Complete。Repair 与连续 No-progress 只会请求类型化 Kernel Convergence，不会由
    Engine 或 Provider 局部循环直接决定终态错误。Provider 输出不完整时没有默认累计
    Sample 上限，只要 Context 与显式 Token/Cost Budget 允许且持续产生结构化进展就继续。
    非零 `MaxSteps` 是连续无进展的 Progress Lease。进展签名仍来自 Kernel Work Item
    的路径集合（Goal、已读路径、已改路径、验证覆盖、Plan 完成步、接受的
    Completion、未关闭进程数量），用于续期长任务；停轮不看「同一路径是否
    又改过一次」。No-progress 计数看 Turn 内已见工作状态：观察键未出现才清零。
    换工具身份但内容版本与结果语义 digest 已见时继续累加长租约。同一观察且同一身份
    才走短租约。翻页、新内容版本或新结构化结果 digest 都算仍在工作，把
    「模型停止发工具 / 提交 complete」当作正常结束，把「同一工作状态空转或循环」当作循环。
    结果语义摘要仅投影结构化的文件版本、搜索命中位置、诊断位置/代码、验证状态与
    输入摘要、失败类别和进程运行/退出状态；集合顺序和重复项不影响摘要。正文、
    Admission 内容摘要、结果 Handle、Call ID、进程 ID、输出游标、诊断自由文本
    不构成进展。没有结构化事实的输出保守地消耗现有租约；不从原始日志猜测失败
    测试集合。进程会话在进展签名中只投影未关闭数量，保留原 ID 用于执行与恢复。
    已知路径的 `file_read` 仅在规范化请求参数一致且文件摘要未变时回放原结果；
    回放保留原始分页信息、截断状态和结果句柄，不把有限窗口扩展为已读到 EOF。
    翻页、扩大读取窗口或更换 PDF 页码时放行必要重读。Continue 上的
    `git_status` / `git_diff` 放行，不因巡视失败消耗采样。约三分之一时提示收敛；
    Finish-only 与 Token/Cost 预算只建议收尾，不再收窄工具目录。完整 Lease 耗尽后
    进入一次只保留 Terminal/Input 能力的 Finalization。工作状态
    （Workspace 内容版本、Work Item 签名、工具结果语义 digest）首次出现才算进展；
    身份切换本身不清零。回到已见观察且调用身份相同，改用
    `execution.implement_no_progress_samples`（默认 6）作为三个阶段的一致权威：
    一半时提示收敛、全值进入 Finish-only、再保留与 Step Lease 相同构造的 Repair
    预算（默认 2+1+1+1=5）后强制 Finalization。回到已见观察但换了身份，继续走
    `max_steps` 派生的长租约。该值为 `0` 仍继承 `max_steps` 的 2/3。
    该策略完全由调用方显式预算与公开合同字段派生，不使用模型档位或绝对经验阈值。
    默认 Verify 失败记录风险，不撤销已接受的 complete；仅 Hard + MustPass
    才使声明失效。
    Tool Result 明确声明 `retry_original=false` 时，同一 Turn、同一 Workspace Revision
    下的完全相同调用会直接回放该失败事实；Workspace 发生变更后缓存失效，允许根据新状态重试。
    Kernel 允许一次只保留 Terminal/Input 能力的 Finalization Sample。Complete 进入
    正常 Commit；Incomplete 记录用于恢复的摘要与 Pending Actions。
12. 跨层故障统一使用协议 `Fault` 契约：Error Code、Origin、Disposition、
    Retryability、Side-effect State 和 Recovery Action。未分类的边界错误默认是
    `unavailable/resume_turn`；只有显式不变量故障才能以
    `internal/fail_turn` 终止。
13. Journal Commit、Suspend 和 Rollback 是幂等 Durable Effect。持久化失败时
    Effect 保持 Requested，Turn 保持 `committing`；Runtime 拒绝当前 Operation，
    但不会伪造 Failed Turn Terminal。恢复流程重试同一个 Effect。
14. 业务 Terminal Decision 在 Turn 后 Context 维护之前冻结。Compaction、
    Session Delta 应用和非控制事件投影失败只能成为 Secondary Issue 或可重放
    Outbox 工作，不能把已完成 Turn 改写为失败。
15. Verification Executor 通过 `VerificationFinished` 返回证据；Reducer 选择 Passed、
    Repair、Reported、Blocked、Failed 或 Reverted，并独占 Repair Budget。
16. Engine 提交 `TerminalRequested`；Reducer 选择 Completed、Failed 或 Canceled。
    随后 Journal Commit/Suspend/Rollback 作为 Durable Effect 执行，并返回
    `JournalResultReceived`。Suspend 会为结构化绑定的 Continue Turn 保留
    Verification-blocked 或 Convergence-blocked 修改。
17. Scope 准备带 Revision 与 Digest 的 `SessionDelta`，包含 History、Usage、Cost、
    Working Set、Evidence、Failures 与 Compaction State。
18. Runtime 为 Usage 与 Latency 冻结同一份带 Digest 的
    `TerminalMeasurementSnapshot`。Receipt、由 Measurement 投影的 Trace 与 Terminal
    Envelope 都引用该 Snapshot，不会再次读取可变 Counter。
19. Persistent Runtime 在同一 SQLite 事务原子提交 Frozen State、Measurement、
    Session Delta、Final Output、Receipt、Terminal Event、Outbox 与真实 Operation
    Receipt。
20. Engine 只在该 Commit 成功后幂等 Apply Session Delta；Commit 失败不修改 Session
    内存。
21. 重启时 Runtime 扫描 Pending Terminal Projection，以稳定 Event ID 逐条 Append，
    成功后再将对应 Entry 标记为 Published。
22. accepted StartTurn 仅在存在对应非终态 Domain Fact 时自动恢复；Coordinator requeue
    Running Effect，Engine 从 Durable Payload 接续 Provider、Tool 或 Journal 执行。
23. Approval/Input 恢复在接续执行前预装原 Request ID，Host 只回放一个 Wait，不会收到
    替代请求。

Engine 始终提交完整逻辑模型请求。只有模型显式广告能力、请求属性不变且输入严格扩展
已提交 Response Chain 时，Provider Adapter 才能将该请求投影为 Incremental
Responses WebSocket。Response ID 与连接状态不会进入 Host 或 Runtime Authority；
Reset、Retry、Compaction、Resume 或任意不确定状态都会回退完整请求。Usage 分别保留
Logical/Transport Digest 与序列化 Request Bytes，传输收益不会被报告为 Token 收益。

每条 Route 都携带显式 `AdapterID`，不可变 Provider Router 是生产环境唯一采样路径。
Composition Root 安装 OpenAI Adapter，以及一个参数化的 OpenAI-compatible
Adapter。DeepSeek 与 GLM 通过后者接入；它们不广告 Incremental Responses，因此
Chat Route 始终使用完整 HTTP/SSE 请求，不发送 `previous_response_id`。

Turn 开始时冻结 `ContextCapacity`：模型 Context Window 扣除模型能力、Operator
Ceiling 和 Turn/Session Budget 共同确定的 Output Reserve 后，得到硬输入容量。
默认 Prepare、Auto Compact 与 Emergency 都等于该容量，不再按百分比提前触发；
Operator 可显式配置更小的成本或延迟 Ceiling。Transport 类型不得暗中套用固定档位。
Token 估算默认使用字符数启发式：拉丁文本按约四字符一 Token，CJK 表意文字、假名、
谚文及其标点按每字符一 Token（主流分词器对密集文字的实际密度；旧的统一四字符
除法会把中文低估约四倍，导致会话首样本即溢出测量窗口）。没有原文可依据的
字节↔Token 预算换算按密集文字的 UTF-8 密度（三字节一 Token）取安全侧。Provider
首次上报真实 Input Tokens 后，Runtime
按同请求的 `真实值 / 原始启发式估算` 比率校准后续估算。反馈先除去当前倍率还原
原始基准再学习，因此连续反馈收敛到真实倍率，而不会因分母随倍率变化在真实
倍率与 1 之间震荡（比率限定在记录于源码并有边界测试锁定的可信区间内，区间外
的上报视为记账异常不予学习）。倍率变化时，窗口账本把已记录的估算基线重定到
新口径，后续增量始终在同一计量基准上比较，不会把倍率变化误读为内容增减，
使会话早期的窗口与吞吐准入贴合真实分词压力，而不是通过反复 Prune/Fold 事后发现。
Provider Throughput 是第三条独立容量平面：`execution.tokens_per_minute` 或 Token
专用限流 Header 给出已知 Burst 时，Runtime 在发送前按 `投影输入 + 输出保留` 准入；
未知则跳过 Token Admission，不按模型名称发明 TPM。超过已知 Burst 或等待将超过
预算时，先对可见 Tail 做一次因果组折叠再重新准入；仍超则拒绝或等待，不会静默
重探，也不会改写 Durable History。

Tool Result 在执行边界按硬输入容量、并行 Batch 大小与 ResultStore Capacity 取得
本次 Token Budget；完整原文保存在 Durable Content Store，模型只接收带稳定
`result_get` Handle 的有界投影。若 `输入 + Output Reserve` 仍超出模型窗口，Gate
先进一步缩减可重新获取的 Surface，再选择保持 Goal 和 Tool Pair 闭合的最小 History
Replacement。Surface 缩减对结构化与 Raw（非 Tool Result JSON）结果一视同仁：原文
落盘，保留头尾摘录并显式标注省略的中间部分与取回句柄，模型始终能区分"被裁剪"与
"本来就没有"。新 Turn 投影只保留每个历史 World Section 的最新有效版本；当前 Turn
的 World 前缀保持追加语义，删除分区不会在下一轮重新出现。已闭合 Tool Round 的
冗余 Assistant Text 可以投影掉，但工具调用所需的 Reasoning 与 Provider Replay
保持不变。Durable History、World Patch 链和原始 Tool Result 不被改写。

每个 Turn 在首次 Provider Sample 前冻结一份 Provider 可见的 World Snapshot。Turn
内后续 Tool Result、Plan 更新、Evidence 与重复调用提醒仍写入权威状态和 Journal，
但不重复生成 World Patch；它们在下一 Turn、恢复或 Compaction 边界重新投影。该冻结
不改变 Authority、Workspace Binding、审批、Sandbox 或 Verification 事实。

OpenAI-compatible Adapter 的回归测试在最终 JSON 序列化后逐消息进行字节比较；
Trace 与 Receipt 保留逻辑公共前缀指标和最终 Transport Payload Digest，不记录消息
内容。`exec_command` 的首次 Sample 只等待到 `yield_time_ms`（默认 10s，上限
30s）。进程仍在运行时返回 `session_id`，由 `write_stdin` 继续收输出或关闭；TTY
与非 TTY 相同，Runtime 不再把非 TTY 命令挂到进程自行退出。`timeout_ms` 只杀进程
组，不延长第一次等待。Cancel 会终止等待并回收进程组。
`command.execution` 对尚未退出的命令使用 `started`，不附带退出码；
`write_stdin` 续读或关闭后按原始 Call ID 和 Session ID 更新同一条命令，
终态区分 `completed`、`failed`、`canceled` 和 `timed_out`。
一次等待窗口结束不代表进程超时，Web 实时视图与历史回放采用同一状态投影。

`exec_command` 可声明验证用途与精确覆盖路径。Runtime 将真实命令、退出码、启动与
结算调用、声明输入摘要、Workspace Revision 和 Mutation Revision 绑定为证据；
长命令在 `write_stdin` 结束时结算。ReceiptRunner 只汇总证据和已有 diagnostics，
不启动进程。Soft 模式如实报告未验证状态，Hard 策略才要求完成前满足覆盖约束。
验证状态、缺失覆盖和调用 ID 来自同一次筛选与输入复核；相同命令针对不同路径的证据
分别保留。重复失败不会因新的调用 ID 重置 Repair Budget，失败进程已经产生的文件
变更也必须进入 Turn Diff。诊断只覆盖部分修改文件时不能声明整体通过。
Tool Result 在首次准入时定稿，后续 Sample 不改写已发送内容，以便 Provider
前缀缓存保持 append-only。超限结果第一次就带 Handle，全文留在 ResultStore，
需要时用 `result_get` 取回。增量 Route 保持严格追加投影，不执行这些会破坏
Response Chain 前缀的转换。

`TurnCoordinator` 是生产环境唯一 `Reducer.Apply` 入口。Engine Event 只用于投影，
不会反向生成 Command 写回状态机。Durable Runtime 构造必须显式提供 Event、Content、
Terminal Store；Memory Store 仅由显式 `NewRuntime` Ephemeral 构造选择。

Cancel 和 Failure 是明确终态，不是“没有返回数据”。

`turn/withdraw` 是独立的用户上下文操作，仅接受当前 Session 主 Thread 的最近一个
Turn。Engine 在执行前通过既有 Context Manifest/CAS 保存不可变基线；恢复中的同一
Turn 不覆盖基线。Runtime 对执行准入加栅栏，停止并等待所属活动 Turn 和 Child Thread
结算，随后以新 Epoch、Revision 和 Token Window 恢复基线。实际文件变更保留为未验证
证据，不保留被撤回的 Goal、Plan、摘要或工具恢复句柄。
撤回使用 `context_rebases` 的稳定提交身份作为 tombstone，与 `context_current` 在同一
事务提交，不修改原终态或费用。`turn.withdrawn` 只负责 Web 审计投影；即使事件发布失败，
Session 查询与恢复校验仍以持久 tombstone 为准，重试可以补发同一稳定事件。
Journal 草稿在上下文提交后按用户确认保留文件并结算；中断后重复撤回或下一 Turn 会
幂等完成结算。Checkpoint 和 Plan 恢复入口拒绝已撤回来源。

## 持久化

Durable State 由多个明确组件组合：

| 组件 | 用途 |
| --- | --- |
| SQLite | Session、Turn、Agent、Usage、Trace 与 Repository 关系 Projection |
| Event Log | 有序 Runtime 事实 |
| CAS | 不可变内容寻址 Payload |
| Session Metadata | 面向用户的 Session/Thread 组织 |
| Workspace Journal | Before Image 与编辑恢复 |
| Snapshot | 显式 Thread 状态检查点 |

SQLite Schema 版本记录在 `PRAGMA user_version`，当前为版本 5。只接受空数据库和
当前版本；其他版本在启用 WAL 或修改 Schema 前拒绝打开，不执行开发期数据迁移。
切换到当前版本时须使用新的空数据目录，或由用户自行移走旧数据。

Session 删除与 Discard 在同一 SQLite 事务中清理所有所属 Thread/Turn 的
Domain Fact、Terminal Envelope 和 Terminal Outbox，再级联删除 Session 关系记录。
这些 Kernel 表不依赖生命周期外键；清理失败必须回滚整个删除事务。删除触发器在同一
事务释放事实、终态、快照和 Context Rebase 的 CAS 根引用，并记录已删除 Thread 的
日志归属。共享内容在最后一个可达引用消失后回收，活跃暂存批次仍受保护。
审计日志按 `state.deleted_event_retention` 保留；默认 `0s`，删除后即可清理，
也可通过 `state.archive_deleted_events` 在清理前导出 gzip JSONL。

Event Log 与 SQLite 投影之间的一致性以事件日志为准：启动时仍完整校验日志，并批量
读取已提交的事件投影。只有事件身份、归属、类型、时间、偏移、长度和摘要全部匹配，
才跳过写入；缺失或不一致的投影仍按日志修复。事件索引、预留提交状态与 Agent Graph
原子提交，因此健康启动不再逐条重复写投影。运行期
追加遇到预留冲突或投影失败时，也会先按日志修复一次再重试同一事件。不确定的落盘
结果保留预留状态，交由下一次对账裁决，不会写入重复记录；此前的干净失败则允许
重试诚实地补写日志。

非持久流事件只在 SQLite 更新单行 `event_watermark`，不写 JSONL 或逐事件预留。
重启仍保留全局序号高水位，已占用序号及水位以下的空洞不能分配给新事件。
流事件重试返回序号冲突，由 Hub 重新分配；持久事件继续使用预留、
日志追加与投影提交的原有恢复流程。流事件不触发生命周期投影：usage 与关系条目
投影本就只关心持久事件类型，`threads.updated_at` 仅随持久事件推进，流事件的
高水位是其唯一 SQLite 写入。

并发追加按组提交落盘：追加请求在存储内部排队，与在途刷新并发到达的事件合并为
一个批次，整批共享一次序号预留事务、一次 JSONL 追加 fsync 和一次投影事务。批内
按序号升序写入，保持日志严格递增；跨批的序号倒挂仍按高水位拒绝，由事件 Hub 的
既有重试（读水位、重建事件、重试）收敛。序号冲突、幂等重试或批中途失败时，整批
回退到逐事件的既有语义（含自愈对账），单事件结果不变。生产者在入队后不可再因
自身上下文取消中止追加——批次会完整落盘并回传结果。

SQLite 连接使用 WAL + `synchronous=FULL`：CAS、领域事实、内容归属、高水位和
日志清理意图均是事务事实，不能仅依赖 JSONL 重建。事务提交必须先持久化，才能进行
后续日志文件替换；事件追加仍通过组提交合并 fsync。

日志清理在启动、会话删除后及显式 `Store.Maintain` 时运行。保留时长从 Thread 删除
时间计算，到期内容在下一次维护时清理。先提交 `event_prune_queue`，再持有读生命周期
锁原子替换日志，最后在同一事务更新保留事件偏移、删除目标索引和预留、清空队列。
崩溃后先完成队列再对账，避免被删除日志复活或误报损坏。可选归档在日志替换前 fsync，
按内容摘要命名，重复维护不生成重复归档；归档文件不参与在线回放，独立保留。
保留事件序号不重排，高水位不降低；旧游标继续返回其后的保留事件，删除形成的空洞
直接跳过，超过高水位的游标仍报错。会话归档（archive）本身不视为删除。

事件日志与状态存储的读路径与追加并发执行：锁内固定已提交高水位和不可变的偏移
证据，锁外读取、校验和解码日志；独立的读取生命周期锁保证关闭文件前等待读取完成，
追加不获取这把锁。日常已提交区域只追加，维护时独占读取生命周期锁；失败回滚只影响上一个已提交末尾之后
的字节。`EventByID` 经 `event_index` 的偏移证据直达读取日志记录，不重放日志前缀。
Turn 恢复、终态 Checkpoint 与 Plan 绑定经 `event_index_turn_sequence` 按 `turn_id`
定位后直达读取对应记录，空或未知 Turn 如实返回空，不回退成全量日志重放。
Continue 解包嵌套恢复 Prompt 时，对提示中声明的祖先 Turn 逐个做同样的索引读取。
Checkpoint 副作用中的 fork 归属另按 `kind=checkpoint.forked` 从 `event_index` 读取，
因为 fork 事件的 `turn_id` 属于源 Turn，而 `NewThreadID` 才指向新 Thread。
无持久索引的内存 Runtime 在已缓存事件上按 TurnID / Kind 过滤。
EventHub 只在固定读取栅栏和注册订阅时持发布锁，历史回放在锁外执行。订阅使用配置
的 Buffer 容量缓存栅栏后的实时事件，回放完成后依次交接历史与缓存；栅栏内稳定事件
的重试不重复推送。缓存溢出则取消本次回放、移除订阅并返回错误，调用方可从已消费
游标重新连接。Workspace 分页沿用同一栅栏，不追随并发追加。
倒序 Session 历史与 Presentation Snapshot 使用日志归属索引定位目标记录。
索引在启动日志校验及成功追加时建立，显式 Session 归属优先于 Thread 归属；
Workspace 过滤及日志字节摘要校验仍执行。分页只解码目标页，Snapshot 从读取栅栏
向前取尾部，达到既有正文预算即停止，保持截断游标及至少保留最新事件的语义。
无持久索引的内存 Runtime 继续使用原有重放路径。

会话内容搜索使用可重建的 `session_event_search` 缓存，记录用户可见请求、最终
回复、变更路径与编辑器符号，并与事件索引在同一事务内更新。启动复用已校验的日志
重建缺失或损坏的缓存；缓存不是独立事实来源，不改变公开 Schema 版本。
查询按 Thread 索引限定候选会话，验证 Workspace 和 Session 归属，再从预先执行
Unicode 小写转换的文本中进行字面子串匹配，只解码各会话最新命中的搜索字段，
不回放全局 Event Log。中文单字、大小写不敏感匹配及既有 240 字符摘要窗口保持不变。
Web 将搜索词、筛选结果与会话列表分开保存，后台活动刷新保留搜索词，输入新查询
取消旧请求并拒绝迟到结果；筛选和清空筛选不触发会话切换。

侧栏列表先按既有排序选出候选页，再按页批量读取最新 Turn、事件水位与用量汇总。
Runtime 批量读取线程归属、检查点数量及最新检查点摘要、Turn 撤回状态；搜索复用
同一份线程归属。SQLite 查询次数不随页内会话数增长，检查点元数据仍执行完整校验；
未提供批量能力的存储实现沿用单会话读取。Web 活动事件仅刷新其事件流绑定的
Workspace，保留其他 Workspace 的列表和筛选结果。请求期间的新事件合并为一次
后续刷新，不用定时阈值，也不反复取消进行中的查询。

Persistent Runtime Wiring 在创建 Engine 前注入 SQLite Turn Coordinator Store。每个
已接受 Transition 都在 State Commit 或 Effect Dispatch 前追加 Domain Fact。热路径恢复
只从最近一份 Snapshot 加重放后续 Delta，不把整条 Fact 历史读进内存。启动恢复
使用可续租的 Active Turn Lease；无法还原的 Active Turn 被隔离为 failed，不阻断
Runtime 启动。重复恢复同一已跟踪 Coordinator 仍 Fail Closed。

Session Checkpoint 与 Plan Artifact 复用 Snapshot Index 和 CAS。Checkpoint 只保存
经过校验的 Context Manifest 与 Profile Snapshot；Manifest 将 History 拆为 Base/Tail，
并将 Working Set、Evidence、Failures 和 Plan 拆为有界 Owner Segment。Restore 不能
执行历史 Event，也不回退独立的 Usage/Cost Accounting。Checkpoint Restore/Fork 创建
新的 State Epoch 和 Token Window，并用 Sparse Workspace Binding 重新核对文件相关
Evidence；不匹配的验证声明会失效为 stale。持久化 Restore/Fork Event 保证重启重建
结果确定；事件显式引用已提交的 Context Commit ID、Digest、Revision 和 Epoch。
Workspace 对账同时重写 History 中已压缩的 Truth Capsule，不能让旧
`verified/current` 声明继续进入下一次采样。Fork 血缘、子 Thread Context 基线与当前
Active Session Thread 属于关系型 Lifecycle State，而不是 Host-local State。

Plan Mode 的 Workspace 只读性由 Policy Effect 强制：普通 Write、写入型 Process
与 Network 继续拒绝；Strong Sandbox 中最终归类为 `process.read_only` 的命令可执行，
Resource 为 Session Plan 的低风险状态更新也可通过。`submit_plan`
生成版本化 JSON Artifact，并在 Artifact Body 内记录 Revision、Supersedes Identity、
步骤依赖、验证证据、用途与文件摘要。执行计划（`purpose=execution`）提交后自动批准
并继续当前 Turn，不经过独立的用户审批或执行按钮。交付计划
（`purpose=deliverable`）只保存为方案产物，不更新 Context 中的执行清单、
不清空工具失败缓存，也不改变已有执行计划的授权；其 Artifact 不提供直接执行转换。
用户后续要求实施时，模型须根据当前请求与 Workspace 状态提交执行计划，
不能通过重标记旧 Artifact 绕过规划、审批或验证。
Runtime 在恢复时重新校验 Session/Thread/Profile 和文件摘要。
Plan Artifact 同时保存执行配置摘要；摘要覆盖 Mode、模型、工具集、审批姿态、执行目标
和步骤预算，但不包含 Planning Policy。Planning Policy 变化不会让已提交计划失效，
执行能力发生变化时仍会 Fail Closed 并要求重新规划。

产品只暴露 `plan`、`act`、`operate` 三种 Mode。`act` 与 `operate` 固定采用
`adaptive` Planning Policy；Guard 在 Capability、Resource、Effect 和 Risk 已规范化后，
拦截高风险、不可逆、网络写、外部写和 Agent Lifecycle 操作；非高风险且非不可逆的
批量 Workspace 操作不因资源数量单独升级。成功的
执行用途的 `submit_plan` Tool Result 才能推进 Turn-scoped `submitted/approved` 状态；
文本声明和交付计划不能解锁工具。每个 Turn 结束时授权状态归零。Continue 恢复自动批准
的执行 Plan 时，必须由同一源 Turn 的执行用途 `plan.delta`、Execution Receipt Plan 与匹配的
Profile Revision 共同证明，不能仅凭恢复 Prompt 中的 Plan 文本重新授权。
交付计划不进入 Context Checkpoint 的执行清单，Continue/Retry 不会自动绑定其执行权限；
同一 Turn 后提交的交付计划也不能替换先前执行计划的恢复绑定。

Turn 的 Model Route 继续在 Scope 创建时冻结。独立 Plan Mode 选择 `PurposePlan`；
Act 内规划选择 `PurposeAct`，因此 Auto 流程可以在同一 Turn 中从规划继续执行，而不会
发生中途换模型或重建 Context 的隐式状态变化。

Workspace Git 状态由 `internal/platform/workspacequery` 在已绑定沙箱中查询。Web Host
只路由显式、带幂等键的分支切换请求；服务只接受已存在的本地分支，并在 Runtime 有活动
Turn 或待处理 Operation 时拒绝切换。Session 列表聚合同时保存
`session_id -> workspace_id` 来源映射，所有 Session 请求固定使用 Owner Workspace，
不从切换中的 UI 状态临时推断。

Git 悬浮窗通过只读 `workspace/git-status` 与 `workspace/git-diff` 查询当前 Workspace：
使用 NUL 分隔的 porcelain/numstat 记录，路径按字面值匹配，不以显示文本解析文件名；
Diff 禁用外部驱动和 textconv。忽略与跳过路径不进入可预览清单，未跟踪文件内容沿用
Workspace Resource 的常规文件和编码检查。每条 Git 查询的公开字节上限
`workspacequery.GitInspectMaxBytes` 继承 Resource 的 1 MiB 限制，截断直接报告错误，
不把部分统计当作完整结果。查询显式绑定 Workspace，切换或关闭窗口时取消旧请求，
不写入 Git Index、Session 或 Turn 历史。`workspace/git-action` 将结构化 Git 命令交给
Runtime `GitControl`；Host 不执行工具，Runtime 不启动 Turn 或调用模型。
`wire` 为每次显式操作构造独立 Guard，沿用策略、仓库规则、Constitution、Journal 和
Lease Authority，仅将按钮确认映射为该次固定工具调用的 once 授权，不复用 Agent 的审批缓存。
Git 操作与 Turn 共享 Workspace 排他准入，有待处理 Operation 或排队 Prompt 时拒绝执行；
占用只计入 Workspace，不把 Session 投影为运行中的 Turn，期间拒绝冲突的 Session 生命周期
与 Profile 修改。关闭 Runtime 会取消并等待正在执行的 Git 操作。
HEAD、完整暂存差异与本地 Git 配置共同形成状态版本，旧版本提交在执行前拒绝；
提交前重新核对全部暂存路径，Git 路径使用 literal pathspec，不让路径表达式扩大暂存范围。
提交与推送分别返回完成事实；推送失败不能回滚已经完成的提交，也不自动重放。
这些显式操作不进入 Turn 恢复队列，网络中断后由用户刷新 Git 状态再决定下一步。

Provider Replay State 同时绑定 Adapter、Provider 和 Model。Router 在目标 Route 改变时
清除不兼容的原生 Replay，仅保留可见 Assistant 内容，避免同 Adapter 跨模型切换后因
Provenance 不匹配导致下一 Turn 失败。

状态数据库使用 schema 5，只创建新格式；旧 schema 在任何写入前拒绝，不提供迁移。
领域事实格式 2 保留每 16 条快照的现有恢复间隔，所有 JSON 对象字段按成员增改/删除
记录增量；成员值 `null` 与成员删除分开编码，不按字段名决定增量算法。
模型响应 Assembly 和 Effect / Outbox payload 压缩后按 SHA-256 存入
`content_objects`，事实和终态只存内容引用，读取时还原原文再验证 State Digest。
引用替换只修改对应值，保留原始 JSON 字节顺序，避免 Operation Receipt 等原始
JSON 在恢复后改变终态摘要；重复写入已存在内容也验证其字节摘要。
TurnContinuation 格式 2 使用独立消息 CAS 引用列表，后续续跑点复用未变化消息，
读取逐条验证摘要、长度和内容结构。

Terminal Envelope 引用 Context Manifest。状态 CAS 的字节、根引用
`content_roots`、子引用 `content_edges` 和暂存批次 `content_staging` 都存于同一
SQLite 数据库。先暂存后提交根引用；事实正文与所属事实在同一事务写入，失败全量回滚。
Turn Checkpoint 的可选 Findings 在结论、定位和来源均为空时不写入 JSON，
恢复时保持字段缺省，避免空对象改变已保存的 Context Snapshot、Manifest 和 Envelope 摘要；
非空 Findings 仍完整参与摘要校验。
Context Rebase、基线快照、终态和续跑批次在成功、失败及部分准备失败后都释放暂存。
启动只释放已不属于本进程活跃批次的遗留暂存；回收从根引用、暂存及显式保留引用遍历
可达图，保护跨会话复用内容，删除不可达边和对象，重试幂等。
单独使用的文件 CAS 保留原有接口；`cas.Release` 本身不删盘，状态 CAS 的回收与写入
通过 SQLite 事务串行，避免检查引用后被并发保留的竞态。
采样路径按公开合同 `context.view.recent_tail_turns` 和剩余硬输入（或显式
`context.view.history_token_ceiling`）投影原文，超窗时再用一次 Visible Tail
Fold。Fold 后仍超硬输入时，对当前 Turn 做钉死用户请求的 working-set 替换：
已闭合因果组收成一条 Truth Capsule，当前 Turn 的 world patch 收成最新基线；
若仍超限，已闭合及最新一批工具结果、调用参数、已消费 reasoning 和过长的闭合轮次
分析正文都可降级为有界投影（正文收成带来源的非权威摘要；工具原文留在
ResultStore / Journal，可通过 `result_get` 回读），并推进 Token Window。进行中的 Turn 继续降级到不可再缩前缀——Stable、`session_state`、
工具定义、当前用户请求原文和 output reserve。只有该前缀仍超硬输入，或部分
Provider 续写无法放入窗口时，才以 `resource_exhausted` 失败。
跨 Turn 的完整 History Replacement 仍留给显式 `thread.compact` 与 Turn 终态维护。
压缩 digest 二次折叠时并入上一份 Removed History；超出 `max_digest_entries`
的条数写入 omitted 计数，不静默丢掉。
`context.view.narrative_mode=post_turn` 写独立 Digest 分区，不阻塞下一轮 Sample。
带出处的未完成工作提升为 Plan Todo 后进入 `session_state`；每个闭合 Turn 在
Dynamic（History 之后）追加一块 write-once Checkpoint。旧 Turn 原文通过
`turn_history` / `result_get` 有界回读，不恢复整段旧 History。Turn 开始时冻结归档
句柄与 Turn ID 映射，工具回读使用该快照，不获取覆盖整轮执行的 Engine 锁；
归档读取继续传递取消信号并检查撤回状态，未知 Turn 或缺少归档时如实返回未命中。
被裁掉的旧 Turn
在 `session_state` 给出检索指针，并指向最近带 Findings 的 omitted turn
（`preferred_turn`）；升级前缺失的 Checkpoint 只回封 turn id。
最近闭合且已完成的用户可见终答，以及工具定位的 `path:line` / 符号，写入
mandatory Continuity 胶囊，不依赖可见 Tail，也不把旧轮对话清单写进 Checkpoint
正文。Plan 已有完成步骤或已读路径时，`session_state` 另带 Resume Fact，避免
Continue / Retry / 新 prompt 把已读文件再读一遍；Resume 使用全部已读路径，
不继承 `context.working_set.max_entries`，超 `session_state` 分区预算时写
omitted 计数。有行号命中时 Continuity / Resume 列出
`Located sites`。搜索命中后优先按 `start_line` 读取相关窗口，根据当前问题扩展，
不要求读取后必须编辑；Plan 和只读分析同样可补读未覆盖窗口。已有 Continuity
或 Located sites 时，不要用 `turn_history` 或搜索做开场恢复。
相邻 Sample 在同一工作状态上重复同一工具身份达到
`execution.implement_no_progress_samples`（默认 6）即进入 Finish-only；该阶段
不允许 `git_status` / `git_diff` 或整文件读取。新的内容版本、翻页窗口或新的
工具结果语义 digest 会同时续期长短租约；A/B 交替但观察键已见时只走 `max_steps`
长租约。重复 `file_read` 仅在请求参数一致且文件摘要未变时回放原结果；
未覆盖窗口、版本变化或原结果不可恢复时允许重新读取。脏的 `git_status` /
`git_diff` 或可见 Tail 没有那次读取都不是重读理由，应走 `turn_history` /
`result_get`。取消和失败 Checkpoint 均保留下一项 Plan 与全部已读路径指针，超 Checkpoint 预算时写 omitted，但不带未提交的半开
Tool 链。缺少终态证据的旧 Checkpoint 只回封检索指针，状态为 `unknown`，
不推断为 completed。Continue 恢复短 Work Item 胶囊（源 Turn、terminal、Known/Open、工具
结论），Goal 是当前用户句，并在开局写入源 Turn 的 KnownReads。

## 可观测性架构

Provider 配额耗尽与瞬时限流分开处理。OpenAI-compatible 的明确
`insufficient_quota` 和 GLM 官方定义的配额业务码不会进入短期退避；GLM 数字码仅在
对应 Provider 下解释，未知 429 不按文本推断为 TPM 或配额。配额终态使用
`resource_exhausted`，保留 Provider 原因和重置时间文案，恢复动作为额度恢复后 Continue。
`provider_retry_after_ms` 只保存 Provider Header 值；本地推导退避留在 Route Cooldown
与实际等待字段，并在 20% 幅度内用 Session / Route / Sample 的确定性 jitter 错开
同序号重试。`execution.provider_retry_limit=0` 对 5xx / 网络 / Timeout 不重试；
空响应仍允许 1 次，429 走独立等待预算。没有 `Retry-After` / Route Cooldown 的
429 在 `rate_limit_retry_limit=0` 时继承 `provider_retry_limit`，避免本地短退避
空转。每次请求都有 started 和闭合事件，回执仅按 started 计数。

模型侧不再提供 quality 工具、专项环境探针或“修改文件后才允许重试”的额外门禁。
命令统一经过 exec_command/write_stdin 的 Guard、审批和 Sandbox；退出码来自真实
进程，不通过输出关键词推断语言、环境故障或测试成功。Go 安装根目录与 macOS SDK
准备属于沙箱能力建设，不属于验证工具。失败摘要优先展示诊断正文，去重身份来自
完整原因摘要而非截断前缀。

Runtime Event 是 Host Protocol，也是生命周期回放的权威记录。Terminal Envelope
原子保存冻结 Measurement、Receipt、Session Delta 与 Projection Outbox。除此之外，
系统只维护面向 Coding 主线的轻量可观测数据：

- **Trace**：Turn 内存 Span Tree 在结束时写入 SQLite，用于 Phase Latency 与查询；
- **Usage**：按 Provider、Model、Session、Thread 和 Turn 聚合 Token 与 Cost；
- **Receipt**：记录最终变更、验证、预算、缓存和 Measurement Digest；
- **Telemetry**：本地结构化日志与低基数指标；
- **Diagnostics/Verification**：提供环境诊断和完成门禁证据。

QCode 不维护第二份 Durable Observation Journal、CAS Payload、Retention Policy
或 OTLP Exporter。W3C Trace Context 仍跨 Provider HTTP、MCP HTTP/stdio、Process 与
Subagent 传播，用于关联调用；它不获得执行权威。故障分析以 Runtime Event、Terminal
Envelope、Trace、Usage、Receipt、Job Log 与 Workspace Journal 交叉核对。

## 上下文架构

仓库符号查询由实际索引查询方法执行一次刷新，仍检查文件列表与修改时间。并发到达
的查询共享同一次刷新：在刷新等待队列中排队的调用者，若刷新在其进入后完成，直接
复用该结果，不各自重走全仓枚举；刷新完成后新到达的调用仍会刷新——这不是时间
窗口，而是重叠去重。同一条调用链内的后续查询（如 Repo Map 构建中的大纲查询）
复用链上已确认的快照，不重复刷新。进程启动后在后台预热首次构建；状态快照独立
原子发布，工具注册和状态展示不等待构建锁。首次构建完成前返回 Pending，后续刷新
期间返回上一次完整发布的状态；成功与降级状态均整体替换，取消不发布未完成结果。
实际索引查询仍等待刷新完成，预热期间到达的查询经重叠去重与预热共享同一次构建。
依赖图与排名在后台构建，不在刷新的同步路径上：刷新以确认的文件行完成，图随后
跟上，期间的排名与影响面回答描述最近一次成功构建的文件集——这是质量边界而非
正确性边界，与图构建失败时回退声明计数排序的既有降级一致。内容级编辑走增量
更新：受影响源集合是变更文件加上引用了这些文件前后所声明符号的文件，重命名因
此会重新绑定引用者；文件集合增删、包名迁移或索引器版本变化回退全量重建，增量
结果与全量重建逐边、逐排名一致（测试锁定）。只变更文件时间戳不重建图。图构建
失败不缓存为成功，下次刷新重试；进程重新打开索引后重新确认图。
符号声明查询在 SQL 中先按名称、类型、文件路径、字面路径前缀及导出属性过滤，
再排序和限制返回条数。路径前缀区分大小写，`%`、`_` 和反斜杠不作为通配符。
`search_symbol` 与索引回退的 `search_definition` 从同一 SQL 快照取得匹配总数，
`total` 不受返回条数限制；超过 `max_results` 或存储层现有 2000 条上限时，
`truncated` 如实标记，不能把被截断的候选集当作完整查询结果。

上下文按稳定性和用途拆分：

- 稳定 Coding Policy 与系统约束；
- Repo Map 与 Symbol Index；
- 用户 Pin 文件；
- 持续演进的 Working Set；
- Evidence 与未解决风险；
- 最近 Event History 或结构化 Compact Summary。

上限是正确性的一部分。无界上下文最终会变贵、变慢并降低一致性。

长期 Session 的模型可见 Context 分为三层：

1. Runtime 生成、经过 Retention 和 Admission 的 Truth Capsule；
2. 可选、非权威、带 Source Message Fence 的 Semantic Narrative；
3. 保持原始 Role/Block 和 Tool Pair 完整性的 Recent Raw Tail。

Authority Digest 只覆盖 Mandatory Truth。Protected 与 Refreshable Entity 按稳定优先级
和类型配额保留，淘汰通过聚合 Omission 解释。Narrative 只能走 `summary` Route，禁用
Tool 和 Native Search；Provider、解析、超时或 Staleness 失败都回退到 Truth + Tail，
不能改写业务 Turn 终态。

Execution Receipt 会逐条解释入选的 Working Set 文件或测试，包括选择来源、支撑
Evidence、相关性分数和单条预算结果。`included=false` 加截断原因表示 Selector 选择了
该路径，但渲染后的上下文预算裁掉了对应行。各 Host 投影同一份 Receipt，不自行反推
选择原因。

用户 Memory 是独立的非权威数据面。记录具有稳定 ID、Generation、来源、过期时间以及
`user`、`workspace`、`repository` Scope；Workspace 和 Repository 使用规范化身份隔离。
Turn Admission 冻结当前 Generation，按显式 Pin、精确 Scope、词法相关性、新鲜度和
稳定 ID 选择记录。Memory 写入只影响下一 Turn，且 CRUD 工具全部经过统一 Guard。

## 安全模型

安全采用分层结构，因为单一控制无法回答所有问题。

### 1. Mode 与 Posture

描述当前 Host/Session 的意图和审批行为。

### 2. Workspace Permission

记录用户对特定工作区允许的操作，必须绑定工作区，不能变成全局授权。

### 3. Constitution

普通 Session 配置不能绕过的硬约束。

### 4. Tool Guard

Tool Identity、Risk、Approval、Repository Policy 与 Edit Evidence 的统一决策点。

### 5. Edit Journal 与 Verify

授权写入后提供可恢复性和正确性证据。
Durable Workspace Journal、Process Job Journal 与 Job Log 位于
`<data-dir>/workspaces/<workspace-id>/control`，不读取 Workspace 内的旧 Journal。
Journal 按 Workspace 加锁，不按 Session 隔离；删除 Session 必须先回滚该 Session
拥有的 retained draft，否则其他 Session 无法 `Begin`。已删除 Session 留下的孤儿
草稿可由任意剩余 Session 的 Continue 接管或 Retry 回滚。
同一 Workspace Identity 下的 `control`、`sandbox-home` 和 `artifacts` 是互不重叠的
状态域；只有 `sandbox-home` 可以作为 Sandbox 写目录。
Execution Receipt 会保留每次 Verification Attempt、命令推导原因、失败分类、Repair
次数、最终 Gate Action 和最终 Workspace Outcome。Rollback 会区分已恢复路径、冲突和
无法回滚的非文件副作用；原有 Pass/Fail 聚合字段只作为兼容摘要保留。

### 6. OS Sandbox

在操作系统边界限制进程、文件系统和网络。不同平台 Backend 强度不同；所需边界不可用
时，执行必须 fail closed。

每个 Workspace 使用位于 State Data Directory 下、权限为 `0700` 的持久私有 Home。
它跨 Turn 和进程重启保留编译缓存与 Agent 安装的工具，但不与其他 Workspace 或宿主
Home 混用。Sandbox 只继承 PATH 目录、Darwin `/etc/paths` 与存在的 Homebrew bin，以及平台
SDK（`SDKROOT`），不再按语言名探测安装根。PATH 目录里指向目录外的可执行符号
链接会把解析后的 `bin` 及其父级（若目录名为 `bin`）只读加入，以覆盖 Homebrew
Cellar 这类布局；证书发现走准备链。平台适配器还会解析可执行文件的传递运行时依赖；
例如 macOS 会读取 Mach-O 依赖和 RPATH，并将经过校验的动态库、包版本根、共享资源
目录和顶层配置文件精确地只读暴露，而不是开放整个包管理器配置目录或包内私有
子目录。凭证目录和整个宿主 Home 始终不开放。主 Agent 与子 Agent 使用同一模型，
但各自拥有独立的 Workspace 范围私有 Home。

将私有 Home 过滤模型替换为可授权环境契约的设计见
[Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)。
产品默认现为 `execution.environment.contract=v1` 与 `profile=native`：
准备器按通用 `host_config` / `cache` / `shared_user_temp` / `credential`+`use`
资源物化环境；主 Agent 保留宿主 HOME 变量，但不开放整个 Home。
Git 适配器只翻译文档中的用户配置文件位置；`.git-credentials` 不进入策略。
`shared_user_temp` 仍须显式打开。GOPROXY 认证在宿主已有凭证时由 runtime
绑定。`contract` 只接受 `v1`。
用户或可信配置的 `[[execution.environment.resources]]` 即可接入，
Go 适配器只是可选翻译器。`exec_command` 的 `write_paths` 还可指向已存在的
工作区子目录，授予有界树写而不打开整个工作区。带写树的命令在隔离执行工作区
运行，真实 cwd 写入 `isolated_cwd`，退出后经 File Broker / Journal 三方结算；
用户并发修改不自动算 Agent 修改。进程不再无条件重写 HOME / 缓存，
证书发现改走准备链，v1 shell 使用非登录 `sh -c`。
`exec_command` / `write_stdin` 失败时，`error_category` 与 `required_action`
只来自 Gate、代理或 OS 后端的结构化事实；没有事实时标记 `unknown`，
不从命令输出中的 `401` 或 `permission denied` 改判。
Darwin 进程出网走 Workspace 代理进程上的 Session 端口与 Session Gate，
Seatbelt 只放行该端口；`contract=v1` 时空 `network_targets` 继承用户声明
的环境网络资源，不继承适配器发现的 GOPROXY 主机。Linux/Windows 在命名空间
助手交付前对需要 Session 通道的进程报告 `backend_capability_unsupported`。
主/子 Agent、PTY 与后台共用该契约；子 Agent 固定 `isolated`，Skill 仍在
`sandbox-home`。
GOPROXY 认证服务挂在同一 Session loopback 上处理 origin-form 模块请求，
不写共享 Gate，也不把长期凭证放进进程环境。对已绑定上游主机的 CONNECT
或绝对形式请求会被拒绝并记 `trust_validation_failed`，避免绕过认证服务。
可信配置 `[[execution.environment.auth_services]]` 可覆盖绑定；否则 runtime
使用宿主 `go env GOPROXY` 的 userinfo 或宿主 `~/.netrc`。第二种制品协议尚未开放。
运行中新发现的出网目标在拨号前走现有 Approval；同一执行内同一 origin 不重复
询问。获批只修订当前 Session / Call Scope。已开始的进程命令不会因出网缺失
被整段重放。

证书依赖独立于包管理器布局：环境准备器从 PATH 中的 OpenSSL
查询公开的 `version -d` 元数据，定位 `OPENSSLDIR/cert.pem`。同时支持宿主显式设置的
`SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS`、`REQUESTS_CA_BUNDLE` 和 `CURL_CA_BUNDLE`。
路径必须为绝对路径，符号链接与真实目标均通过普通文件、敏感目录和 Workspace
边界校验后，仅按文件只读暴露；显式配置无效时拒绝构造，不静默丢弃。
其他信任库布局可通过显式证书文件或 Host Read File 配置接入，不猜测安装前缀，
不关闭 TLS 校验。元数据查询沿用沙箱能力探测的 5 秒预算
（`ToolchainProbeTimeout`），单次输出安全上限为公开的 64 KiB
（`ToolchainProbeMaxOutputBytes`）；超时、超限或无法解析的自动发现结果不授予读取权限。

## Secret 与网络边界

- Config 保存 Secret Reference，不保存 Secret Value。
- Provider 与 Web 出网使用受治理 Client 和显式 Endpoint。
- Log 与 Report 会脱敏，但仍属于敏感工程数据。
- MCP 是供应链边界，不是可信文本文件。
- 服务默认监听 Loopback。

## 能力接入

Skill、Memory 与 MCP 不再经过通用 Extension Registry 或 Extension Plan。Wire 在
`capability-tools` 模块中按固定顺序直接构造三类能力：Skill 发布 Catalog 与发现工具，
Memory 打开 Store 并注册受信工具，MCP 创建受 Runtime Authority 约束的 Pool 与
Prewarm。各能力自行管理配置、完整性、生命周期和撤销边界。

Web Transport `extension/list`/`extension/control` 与 Web Extensions View 使用同一
Runtime Control Plane。Mutation 按 Operation ID 幂等，并持久化
Prepare/Commit Receipt；Host 只提交 Operation 与投影 Runtime-owned State。

### MCP

外部 Server 通过协议 Adapter 暴露 Tool。Health、Timeout、Circuit Breaker 和 Tool
Binding 隔离避免单个 Server 故障污染全部工具。当前 stdio Server 仍是宿主进程，
因此默认关闭，只接受外部 State Directory 中显式 `host_trusted=true` 的配置。

### Skill

Skill 打包指令和资源。Discovery、Manifest、Lock 与 Enablement State 让最终内容可见。
Runtime 构造时从实际 Sandbox Policy 取得私有 Home，将其中的标准 Skill 目录以
Workspace 来源加入 Catalog；模型工具和 Web Skill Control 使用同一 Home，不根据
宿主 HOME 或另算的 Workspace ID 推断安装位置。私有 Skill 根不得经符号链接逃出
该 Home。Catalog 固定这些发现范围，在 `skills_list` 首页请求与新 Turn 的
技能选择入口重新扫描；完整扫描结果原子替换目录快照，变化时清理选择缓存。
安装后可在当前 Turn 重新列出，无需重启。后续分页不重新扫描；
若其他请求已刷新并改变目录，旧游标明确失效，不能跨快照混页。
按句柄加载与依赖解析固定在同一目录快照上，旧句柄不能被刷新后的新内容替代。
隔离 Chat 与使用独立工具集的子 Agent 同样构造各自的 Catalog，并将
`skills_list`、`skills_read` 和 Turn Selection 绑定到这一份 Catalog。发现范围使用
子 Workspace、实际子 Sandbox Home 及已配置的公共 Skill 目录，不继承父或兄弟的
私有 Home；启用状态与 Lock 路径沿用所属 Runtime 的配置。Lock 缺失或漂移不会
阻断 Runtime 和 Skill Control 构造，也不会自动改写 Lock；受治理内容加载仍须
通过完整校验。子工具集释放后重建会重新发现其持久 Home 内安装的 Skill；
共享 Workspace 工具集的子 Agent 继续使用父 Catalog。
Lock 记录 Catalog 中已发现的受治理包及其依赖库存，包含禁用项，不随启用状态增删。
禁用独立 Skill 不影响其他已锁定 Skill；加载依赖被禁用项的 Skill 时仍拒绝。
安装或更新后的新 Catalog 可通过 Skill Control 显式锁定，恢复受治理内容加载。
Turn Selection 会先保留被精确点名、Required 以及此前使用过的 Skill，再应用有界词法
候选上限。Turn 会冻结 Name-to-handle Binding；加载时重新校验 Content Digest、
Dependency Plan 与 Lock。`skills_read` 接受该冻结条目广告的
任一精确 Handle（Skill、Package 或 Resource），并在结果中返回规范化 Skill Handle。
候选摘要与 `skills_list` 保留真实 `SKILL.md` 来源路径；`skills_read` 在正文中逐项标明
根 Skill 与依赖的 `source_path` 和 `resource_base`，结构化结果也保留路径。
相对引用、脚本和资源以所属 Skill 的实际目录解析，不从 Skill 名称猜目录，
也不沿用根 Skill 的目录解析依赖。工作区内资源用 `file_read`，外部路径可用
`shell_read`，继续经过现有 Policy 与 Sandbox；来源元数据本身不授予权限。
内置 Skill 的 `builtin://` 来源标记为自包含嵌入内容，不作为文件系统路径使用。
真正无效或过期的 Handle 会返回结构化 `skills_list` 恢复动作，而不会直接终止 Turn。
Execution Receipt 会记录选择规模、显式命中、Token Projection、Cache 使用情况以及
Query/Candidate 截断。

## Subagent 协作架构

QCode 不维护通用后台 Task Queue、Worker Lease、Workflow DAG、Automation、
Lane 或 Fleet。前台工作由 Runtime Turn 承载；多 Agent 协作通过 Subagent
Control Plane 扩展，但不建立第二套执行生命周期：

```text
Parent Turn
  -> DelegationIntent
  -> Supervisor Admit / Bind / Schedule / Settle
  -> Child Runtime Turn
  -> Guarded Tool + Worktree
  -> Typed Result + Mailbox + Journaled Integration
```

- **Supervisor**：编排入口。Tool 只提交 Intent；Admit 在 Takeover 前用与 Child
  Engine 相同的首包窗口投影预算，不足则拒绝 spawn，不创建 running agent。
- **Session Parent**：顶层 Child 的 `ParentID` 固定为 `parent`，completion 与
  context 投递同一收件人；Mailbox Drain 失败不得丢消息。
- **Typed Settlement**：`completed` / `retryable`（预算、rate-limit） / `failed` /
  `interrupted`，并带稳定 `reason_code`。分类只读取终态及其 `Fault`，不扫描正文、
  Receipt Note 或 Secondary Issue 的关键词。可选 `fault.reason` 随终态冻结、持久化和
  投递，明确携带 `token_budget_exhausted`、`cost_budget_exhausted` 或
  `provider_rate_limited`；旧记录缺少该字段时不推断原因。只有来源、错误码、原因和
  恢复 Disposition 一致的预算或瞬时限流失败才标记 `retryable`，表示条件满足后可
  `followup_task`，不表示立即自动重试。恢复建议优先使用该 Fault 的 `recovery_action`，
  区分 Token 与费用预算；供应商硬配额、上下文不足不误归为 Child Token 预算。
- **取消与结算**：`interrupt_agent` 只提交活动 Turn 的取消请求，返回当前真实状态。
  取消结果到达前保留活动状态和 Manager 的预算预留；真实结果经 `Settle` 一次性提交
  终态、结果、用量与 completion 消息。父 Agent 通过 `wait_agent` 等待结算后再
  `followup_task`，重复取消已结算的 Agent 不再提交操作或改写终态。
  `close_agent` 先阻止新任务、工具执行和继续委派，再等待正在提交的 Turn 明确身份，
  请求取消并等待真实结算，最后写入 Closed、清理 Worktree 和 Runtime Thread。
  取消提交失败或等待被取消时保留真实状态与资源，允许重试关闭；重复关闭不重复记账。
  Runtime 在结算提交完成前保留 Turn 跟踪，重复终态不重复扣费，后续任务等待该次
  结算收尾，避免关闭与结算交错留下永久重试或误释放下一轮资源。
- **共享 Provider 限额**：Session 级 limiter 对 Parent 与 Children 的 Provider
  Sample 单飞排队，共用一份 Retry-After 冷却和 429 等待预算；冷却未解除时
  Supervisor 不再并行启动第二个 Child。用户发起的新 Parent Turn 会刷新等待预算，
  但仍遵守未结束的冷却。
- **Agent Graph**：持久化 Spawn、Transition、Result、Mailbox 与 Integration 事实。
- **Admission/Budget**：约束 Depth、Concurrency、Token、Cost 和 Resident Agent。
- **Child Runtime**：执行普通 Runtime Turn，共享 Operation/Event、Guard、Journal
  与恢复语义。
- **Worktree/Chat Merge**：只读 stance 不 provision git worktree；写入隔离并集成，
  不允许 Child 扩大 Parent Authority。
- **Role 工具面**：`process.read_only` 只放行只读 process，不得放行 `exec_command`。
  被 Role 整工具拒绝的条目不得进入该 Child 的模型目录，避免模型反复调用再被
  Guard 拒绝。

外部定时或流水线系统可以调用受支持的 Web 入口，但不进入 Runtime 内部建立后台
Scheduler。

## 架构变更检查表

1. 确认所属层。
2. 判断是否改变协议或持久化状态。
3. 定义 Cancel、Retry 与 Terminal 行为。
4. 保持 Guard 与 Sandbox 路径。
5. 增加 Contract 或 Architecture Test。
6. 同步更新中文文档。
7. 必要时重新生成 Protocol 与 Compatibility Artifact。
