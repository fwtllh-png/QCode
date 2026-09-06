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
| Orchestration | `internal/orchestration` | Subagent、Admission/Budget、Chat Merge |
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
拥有 Subagent、Admission/Budget、Child Worktree/Toolset 与 Chat Merge 构造。
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
`tool.result.execution`。Artifact/Process Broker 接管 Process Smoke 与 stdio
MCP 生命周期；File/VCS Broker 接管模型文件工具、Agent/Chat Merge、生成型 Workspace
输出和 Git Metadata Mutation。`workspacebroker.Runtime` 只组合窄能力，不把 Broker
业务逻辑放入 `wire`。

Builtin、Skill、Memory 与 MCP Tool 共享同一个 Registry 实例。Composition Root
按固定顺序直接构造 Skill Catalog、Memory Store 和 MCP Pool，并只向后续模块发布
必要结果。Subagent 工具由 Orchestration Module 单独装配。

Registry 分别冻结模型可见的 `ExternalDescriptor` 与执行权威
`TrustedBinding`。External Requested Effects 只用于呈现和审计；Guard、Policy、
Authority、Journal、Sandbox 和验证证据接纳只消费 Trusted Binding。外部 Source 必须
由可信 Host Policy 显式绑定权限，Deferred Loader 不能改变冻结的 Binding。

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
非敏感 Setup Record。只有这些事实持久化成功后，Host 才调用 `wire.NewExec` 并
`Activate` 完整 Web Runtime。

Web 进程持有一个全局 Owner Lease 和持久化 Workspace Registry。首次启动创建
Supervisor；其他目录再次执行 `qcode` 时，通过 Lease 中仅对当前用户可读的
Capability Token 调用已有 Host 的 `workspace/add`，不会启动第二套控制面。每个
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

Web Unary Route 以 `internal/host/runtimeapi/web/contract.go` 为唯一清单。
`webprotocolgen` 从该清单生成公开 Transport Manifest、TypeScript Route Union 和
Go Handler Table；Handler 方法名由路由分段确定，例如 `session/create` 对应
`sessionCreate`。服务端不得再维护平行的字符串 Dispatch Switch。

Web Client 使用 Runtime Snapshot 完成 Hydration，再按当前 Workspace 的 Cursor 消费
Event。持久层 Sequence 在 Supervisor 内全局严格单调，浏览器则按 Workspace 分别保存
Cursor；只有对应 Runtime 明确报告 Retention Gap 时才进入 Desync。
浏览器 Conversation Projection 对高频 Delta 按动画帧合并发布，并保持未变化业务节点
的引用稳定。Trajectory Event Ledger 与 Chat 复用该事件窗口；`trace/query` 只补充
经过 Session/Turn 归属校验和字段白名单投影的时序，不返回任意 Span Attribute。
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
    Wait，Tool Call 继续同一个 Turn。未执行工具、没有 Mutation、Pending Tool 或 Workspace Change 的
    只读 Answer/Plan，可以由带非空正文的 Provider `end_turn` 完成，避免纯文本直答仅为形式化声明再次
    采样；执行过工具的 Turn 必须显式 `turn_complete`，防止中间叙述被误判为答案。其他 Turn
    仍只有被接受的 `turn_complete` 才能结束，Provider `message_stop` 只结束一次 Sample，
    普通模型正文保持 Provisional。    对于 `status=complete`，声明中的 `summary` 是精确的用户可见 Final Output，
    Runtime 无需额外 Model Sample 即可发布。仅交付计划、未产生 Workspace Mutation
    的 Answer/Plan Turn 可以在 Plan 步骤仍为 pending 时完成，把后续实现留给用户；
    已开始执行计划或发生 Mutation 时，未完成步骤仍拒绝 `status=complete`，
    `required_action` 是完成剩余步骤或 `status=incomplete`，而不是再次 `update_plan`。
    步骤签名未变的 `update_plan` 会被拒绝，不写 `plan.delta`，也不续期进展 Lease。
    被拒绝的 `turn_complete` 调用身份不算结构化进展；同类拒绝耗尽 Declaration Repair
    后进入 Convergence Finalization。Convergence Finalization 也可以使用
    `output_mode=preserve_provisional` 保留已捕获正文并追加简短收尾。Runtime 不根据
    正文措辞推断必需输入。Child Executor 没有 Input Host，不能等待用户
    输入，但仍必须通过 Tool Call 继续或通过 `turn_complete` 完成。
11. `EvaluateTurnStep` 由 Reducer 选择 Repair、Verification、Finalize、Block 或
    Complete。Repair 与连续 No-progress 只会请求类型化 Kernel Convergence，不会由
    Engine 或 Provider 局部循环直接决定终态错误。Provider 输出不完整时没有默认累计
    Sample 上限，只要 Context 与显式 Token/Cost Budget 允许且持续产生结构化进展就继续。
    非零 `MaxSteps` 是连续无进展的 Progress Lease：进展签名来自 Kernel Work Item
    的路径集合（Goal、已读路径、已改路径、验证覆盖、Plan 完成步、接受的
    Completion、未关闭 Process Session）。同一路径再次 `file_edit`、成功调用次数
    或单纯 Mutation Revision 增长不会续期。Answer/Plan 首次纳入新已读路径仍算研究
    进展；Open Implement（Plan 已有完成步骤且仍有 outstanding）时新 `file_read`
    不续期。已知路径的整文件 `file_read` 与 Continue 上的 `git_status` /
    `git_diff` 由准入拒绝，且不改变签名。约三分之一时提示收敛，约三分之二时
    收窄为完成相关能力，完整 Lease 耗尽后进入一次受限 Finalization。Turn 一旦
    具有 Known 或 Open Work Item，改用 `execution.implement_no_progress_samples`
    （默认 6）进入 Finish-only；`0` 仍继承 `max_steps` 的 2/3。该策略完全由调用方
    显式预算与公开合同字段派生，不使用模型档位或绝对经验阈值。
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
Composition Root 安装 OpenAI、Anthropic Adapter，以及一个参数化的 OpenAI-compatible
Adapter。DeepSeek 与 GLM 通过后者接入；它们不广告 Incremental Responses，因此
Chat Route 始终使用完整 HTTP/SSE 请求，不发送 `previous_response_id`。

Turn 开始时冻结 `ContextCapacity`：模型 Context Window 扣除模型能力、Operator
Ceiling 和 Turn/Session Budget 共同确定的 Output Reserve 后，得到硬输入容量。
默认 Prepare、Auto Compact 与 Emergency 都等于该容量，不再按百分比提前触发；
Operator 可显式配置更小的成本或延迟 Ceiling。Transport 类型不得暗中套用固定档位。
Provider Throughput 是第三条独立容量平面：`execution.tokens_per_minute` 或 Token
专用限流 Header 给出已知 Burst 时，Runtime 在发送前按 `投影输入 + 输出保留` 准入；
未知则跳过 Token Admission，不按模型名称发明 TPM。超过已知 Burst 或等待将超过
预算时，先对可见 Tail 做一次因果组折叠再重新准入；仍超则拒绝或等待，不会静默
重探，也不会改写 Durable History。

Tool Result 在执行边界按硬输入容量、并行 Batch 大小与 ResultStore Capacity 取得
本次 Token Budget；完整原文保存在 Durable Content Store，模型只接收带稳定
`result_get` Handle 的有界投影。若 `输入 + Output Reserve` 仍超出模型窗口，Gate
先进一步缩减可重新获取的 Surface，再选择保持 Goal 和 Tool Pair 闭合的最小 History
Replacement。Provider 请求只物化每个 World Section 的最新版本，并移除已闭合 Tool
Round 中的旧状态文本和 Reasoning；Durable History、World Patch 链和原始 Tool
Result 不被改写。

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

Verification Runner 将节点结果绑定到声明输入的内容摘要、Workspace Revision 与
Mutation Revision。只复用输入摘要未变的通过节点；失败或 unavailable 节点必须重跑，
完成门禁仍要求当前 Mutation 的完整覆盖。Tool Result 在首次准入时定稿，后续 Sample 不改写已发送内容，以便 Provider
前缀缓存保持 append-only。超限结果第一次就带 Handle，全文留在 ResultStore，
需要时用 `result_get` 取回。增量 Route 保持严格追加投影，不执行这些会破坏
Response Chain 前缀的转换。

`TurnCoordinator` 是生产环境唯一 `Reducer.Apply` 入口。Engine Event 只用于投影，
不会反向生成 Command 写回状态机。Durable Runtime 构造必须显式提供 Event、Content、
Terminal Store；Memory Store 仅由显式 `NewRuntime` Ephemeral 构造选择。

Cancel 和 Failure 是明确终态，不是“没有返回数据”。

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

SQLite Schema 版本记录在 `PRAGMA user_version`，当前为版本 4。版本演进以显式
迁移链登记：每一步恰好前进一个版本并在自己的事务内记录新版本号，链必须连续且
终点等于当前版本（源码级测试锁定）；更高版本拒绝打开，没有登记步骤的版本
不做自动迁移。首次稳定基线前的开发迁移历史已有意压缩；公开版本后的 Schema
变更必须继续在迁移链上追加显式步骤。

Event Log 与 SQLite 投影之间的一致性以事件日志为准：启动时执行 reconcile；运行期
追加遇到预留冲突或投影失败时，也会先按日志修复一次再重试同一事件。不确定的落盘
结果保留预留状态，交由下一次对账裁决，不会写入重复记录；此前的干净失败则允许
重试诚实地补写日志。

事件日志与状态存储的读路径与追加并发执行：已提交区域只追加不收缩，失败回滚只
影响上一个已提交末尾之后的字节，因此重放、单条读取和高水位查询都持读锁，慢消费
者的重放不再阻塞写入。`EventByID` 经 `event_index` 的偏移证据直达读取日志记录，
不重放日志前缀。

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
步骤依赖、验证证据与文件摘要。Plan 在提交后自动批准并继续当前 Turn，不经过独立的
用户审批或执行按钮。Runtime 在恢复时重新校验 Session/Thread/Profile 和文件摘要。
Plan Artifact 同时保存执行配置摘要；摘要覆盖 Mode、模型、工具集、审批姿态、执行目标
和步骤预算，但不包含 Planning Policy。Planning Policy 变化不会让已提交计划失效，
执行能力发生变化时仍会 Fail Closed 并要求重新规划。

产品只暴露 `plan`、`act`、`operate` 三种 Mode。`act` 与 `operate` 固定采用
`adaptive` Planning Policy；Guard 在 Capability、Resource、Effect 和 Risk 已规范化后，
拦截高风险、不可逆、网络写、外部写和 Agent Lifecycle 操作；非高风险且非不可逆的
批量 Workspace 操作不因资源数量单独升级。成功的
`submit_plan` Tool Result 才能推进 Turn-scoped `submitted/approved` 状态；文本声明不能
解锁工具。Plan 只采用自动执行语义，每个 Turn 结束时状态归零。Continue 恢复自动批准
的 Plan 时，必须由同一源 Turn 的 `plan.delta`、Execution Receipt Plan 与匹配的
Profile Revision 共同证明，不能仅凭恢复 Prompt 中的 Plan 文本重新授权。

Turn 的 Model Route 继续在 Scope 创建时冻结。独立 Plan Mode 选择 `PurposePlan`；
Act 内规划选择 `PurposeAct`，因此 Auto 流程可以在同一 Turn 中从规划继续执行，而不会
发生中途换模型或重建 Context 的隐式状态变化。

Workspace Git 状态由 `internal/platform/workspacequery` 在已绑定沙箱中查询。Web Host
只路由显式、带幂等键的分支切换请求；服务只接受已存在的本地分支，并在 Runtime 有活动
Turn 或待处理 Operation 时拒绝切换。Session 列表聚合同时保存
`session_id -> workspace_id` 来源映射，所有 Session 请求固定使用 Owner Workspace，
不从切换中的 UI 状态临时推断。

Provider Replay State 同时绑定 Adapter、Provider 和 Model。Router 在目标 Route 改变时
清除不兼容的原生 Replay，仅保留可见 Assistant 内容，避免同 Adapter 跨模型切换后因
Provenance 不匹配导致下一 Turn 失败。

Terminal Envelope 不再重复写入完整 Session Snapshot，而是引用 CAS 中的 Context
Manifest。CAS 先按 Digest 幂等 Stage，SQLite 再提交 Manifest 可达性和 Terminal
事实。采样路径按公开合同 `context.view.recent_tail_turns` 和剩余硬输入（或显式
`context.view.history_token_ceiling`）投影原文，超窗时再用一次 Visible Tail
Fold；History Replacement 只发生在显式 `thread.compact` 或 Turn 终态维护。
`context.view.narrative_mode=post_turn` 写独立 Digest 分区，不阻塞下一轮 Sample。
带出处的未完成工作提升为 Plan Todo 后进入 `session_state`；每个闭合 Turn 在
Dynamic（History 之后）追加一块 write-once Checkpoint。旧 Turn 原文通过
`turn_history` / `result_get` 有界回读，不恢复整段旧 History。被裁掉的旧 Turn
在 `session_state` 给出检索指针；升级前缺失的 Checkpoint 只回封 turn id。
Plan 已有完成步骤或已读路径时，`session_state` 另带 Resume Fact，避免
Continue / Retry / 新 prompt 把已读文件再读一遍；有行号命中时列出
`Located sites`。搜索命中后对该路径的 `file_read` 必须带 `start_line`。
Turn 的 Work Item 一旦有 Known 或 Open，无签名变化的 Sample 达到
`execution.implement_no_progress_samples`（默认 6）即进入 Finish-only；该阶段
不允许 `git_status` / `git_diff` 或整文件读取。已知路径整文件 `file_read` 与
Continue 巡视 git 在工具执行前被拒绝，不续租。脏的 `git_status` /
`git_diff` 或可见 Tail 没有那次读取都不是重读理由，应走 `turn_history` /
`result_get`。取消 Checkpoint 保留下一项 Plan 与已读路径指针，失败仍不带半开
Tool 链。Continue 恢复短 Work Item 胶囊（源 Turn、terminal、Known/Open、工具
结论），Goal 是当前用户句，并在开局写入源 Turn 的 KnownReads。

## 可观测性架构

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
Home 混用。Sandbox 还通过统一的 Toolchain Exposure 发现宿主 PATH 中的 Go、Rust、
Node.js、Python 等安装，将已校验的可执行目录和依赖根只读挂载，并只投影运行所需的
环境变量。平台适配器还会解析可执行文件的传递运行时依赖；例如 macOS 会读取 Mach-O
依赖和 RPATH，并将经过校验的动态库、包版本根、共享资源目录和顶层配置文件精确地
只读暴露，而不是开放整个包管理器配置目录或包内私有子目录。凭证目录和整个宿主 Home
始终不开放。主 Agent 与子 Agent 使用同一模型，但各自拥有独立的 Workspace 范围私有
Home。

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
Turn Selection 会先保留被精确点名、Required 以及此前使用过的 Skill，再应用有界词法
候选上限。Turn 会冻结 Name-to-handle Binding；加载时重新校验 Content Digest、
Dependency Plan 与 Lock。`skills_read` 接受该冻结条目广告的
任一精确 Handle（Skill、Package 或 Resource），并在结果中返回规范化 Skill Handle。
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
  `interrupted`，并带稳定 `reason_code`。
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
