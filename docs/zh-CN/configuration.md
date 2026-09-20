# 配置说明

## 配置优先级

配置从低到高按以下顺序解析：

```text
内置默认值 < TOML 文件 < QCODE_* 环境变量 < Web 启动参数
```

启动时通过 `--config` 指定文件；解析或校验失败会显示在 Web Boot Failure Surface：

```bash
qcode --config ./qcode.toml --workspace . --open
```

MCP Server 定义使用独立、严格且带版本的 JSON 文件，不属于 Runtime TOML 控制面。
Web 通过 `--mcp-config` 传入，并在 Settings 中展示加载状态。该文件必须位于
`[state].data_dir` 下；启用 stdio Server 还必须显式声明 `host_trusted: true`，因为
Runtime 会将 Server 生命周期绑定到受信配置、Execution Lease 和 Process Broker。

## 完整实用示例

```toml
[runtime]
operation_buffer = 64
event_history = 256
subscriber_buffer = 64

[state]
data_dir = "/absolute/path/outside/workspace/qcode-state"
busy_timeout = "5s"
event_retention = 1000000

[memory]
enabled = false
path = ".qcode/memory"
max_candidates = 32
max_prompt_bytes = 16384
semantic_rerank = false

[telemetry]
log_level = "info"

[credential]
kind = "env"                 # env | file | keyring
name = "OPENAI_API_KEY"      # 只保存引用，不能填写密钥值

[execution]
provider = "openai"
model = "gpt-4.1"
protocol = "openai_chat"
mode = "act"                 # plan | act | operate
workspace = "."
tools = true
max_output_tokens = 0           # 0 = 使用当前模型声明的 MaxOutputTokens
max_steps = 64                  # 连续无结构化进展的 Step Lease；0 = 不设置
implement_no_progress_samples = 6  # 同一工作状态且同一工具身份重复时的 finish-only 租约；0 = 继承 max_steps 派生的 2/3
timeout = "2m"                  # 连接、TLS 和响应头阶段
lease_timeout = "2m"            # Guard 授权到 Executor 接管前的 Lease 有效期
approval_timeout = "0s"         # 0 = 审批随 Turn/Session 生命周期，不独立过期
connection_timeout = "0s"       # 0 表示继承 timeout
tls_handshake_timeout = "0s"    # 0 表示继承 timeout
response_header_timeout = "0s"  # 0 表示继承 timeout
idle_timeout = "1m"             # 每个流事件都会续期
max_concurrent = 8
rate_limit = 0                    # 0 = 仅根据 Provider 反馈动态限流
provider_retry_limit = 3          # 每次 Model Sample 的瞬时故障重试预算；0 = 不重试
rate_limit_retry_limit = 0        # 0 = 有 Retry-After/Cooldown 时不限次数；无等待信号时继承 provider_retry_limit
rate_limit_wait = "10m"           # 累计 429 等待上限；0 = 继承 timeout
tokens_per_minute = 0             # 0 = TPM 未知，不按模型名称发明默认值；只做请求冷却
budget_tokens = 0            # 0 表示不设置累计 Session Token 上限
turn_budget_tokens = 0       # 0 表示不设置累计 Turn Token 上限
budget_usd = 0               # 0 表示不增加成本上限
reasoning_effort = ""        # 空值为自适应；显式值固定 Effort
native_search = false


`turn_budget_tokens` 统计一个 Turn 内所有模型调用的累计输入与输出。它不是模型的
Context Window：后者只约束单次请求。默认值 `0` 不设置累计上限，单次请求仍受模型
能力约束；连续无结构化进展时仍受 `max_steps` 约束。相邻 Sample 在同一工作状态
（Workspace 内容版本、Work Item 签名、工具结果语义 digest）上重复同一工具身份时改用
`implement_no_progress_samples`（默认 6）进入 finish-only；`0` 表示继承
`max_steps` 派生的 2/3 租约。新的内容版本或结构化结果 digest 同时续期长短租约；已见
观察换身份只走长租约。需要控制成本时应显式设置
`turn_budget_tokens`、`budget_tokens` 或 `budget_usd`。
结果语义摘要不使用原始正文、耗时、临时路径日志、结果 Handle、进程 ID 或输出
游标；仅结构化事实变化可以续期。没有结构化事实的新输出不会单独清零计数。
已有租约阈值保持不变；长时间进程仅持续输出日志也不能无限续期。
[execution.verify]
mode = "soft"                # off | soft | hard
scope = "diagnostics"        # diagnostics | repository | affected
on_failure = "fail"          # fail | revert
command = ""                 # 可选的显式仓库验证命令
max_repair_steps = 1
timeout = "2m"

[execution.journal]
durable = true
recover_on_start = true

[execution.subagent]
delegation = "adaptive"      # disabled | explicit | adaptive
max_depth = 5
max_parallel = 4
max_resident = 8
max_total = 16
max_steps = 0                   # 0 = 不设置子 Agent Sample 数量上限
max_tokens = 0                  # 0 表示按 Turn 上限和 max_parallel 派生树预算
max_cost_usd = 0
wall_time = "0s"                # 0 = 不设置子 Agent 执行 Lease
workspace = "auto"           # auto | read_only | worktree | same_workspace_serialized

[context.index]
enabled = true
max_file_bytes = 1048576
max_files = 20000
# 以下三项约束每个符号记录的细节量级，防止病态文件把索引变成源码副本。
# 默认值分别容纳一条长参数列表、一个完整注释块和一个大型生成文件的
# 去重标识符集合；调整后 IndexerVersion 语义不变，下次刷新按新界重写。
signature_max_bytes = 512
docstring_max_bytes = 2048
reference_max_count = 4096
# 引用图排名（Repo Map 目录排序的数据源）参数。damping 取 PageRank
# 原论文（Brin & Page, 1998）的标准值 0.85；迭代上限与收敛阈值限定
# 精化循环规模。排名失败时 Repo Map 自动回退按声明数排序。
rank_damping_factor = 0.85
rank_iteration_limit = 100
rank_convergence_threshold = 0.000001
[context.lsp]
# 常驻 language server 会话池。常驻 server 是宿主进程，因此默认关闭，
# 必须显式启用（与 stdio MCP 的 host_trusted 治理语义一致）。关闭时
# 语义查询保持逐次起停的现状，行为无漂移。
resident_enabled = false
idle_timeout = "10m"     # 空闲会话回收窗口（1s 到 1h）
max_servers = 2          # 单 workspace 并发 server 上限
cache_capacity = 256     # 语义查询结果缓存条目上限

# 受影响测试分析的反向依赖闭包边界：跳数上限与单次回答的文件数上限。
# 三跳覆盖直接依赖方、其依赖方与再一层；默认值的依据是词法图的
# 同名误报随跳数累积快于召回收益。
impact_max_depth = 3
impact_max_results = 200

[context.repo_map]
enabled = true
max_bytes = 8192
max_directories = 24

[context.working_set]
enabled = true
max_entries = 16
max_bytes = 8192

[context.evidence]
enabled = true
max_entries = 24
max_bytes = 4096

[context.coding_policy]
enabled = true

[context.view]
recent_tail_turns = 2
keep_recent_tool_results = 0 # 快照字段；已发送 Tool Result 不再按此改写
history_token_ceiling = 0 # 0 表示 Mandatory 分区之后的剩余硬输入容量
digest = "ledger" # 或 "ledger+narrative"；session_state 始终 mandatory
narrative_mode = "post_turn" # 不阻塞 Sample；仅允许 off 或 post_turn
checkpoint_max_bytes = 0 # 0 继承 summary_max_bytes，再继承 semantic_narrative_item_max_bytes

[context.compact]
prepare_tokens = 0 # 0 表示不设提前压缩档；非 0 为 Operator Ceiling
auto_compact_tokens = 0 # 0 表示不设提前压缩档；非 0 为 Operator Ceiling
emergency_tokens = 0 # 0 表示不设提前压缩档；非 0 为 Operator Ceiling
scope = "total" # 或 "body_after_prefix"
summary_max_bytes = 0 # 0 表示使用当前 Turn 的硬输入容量作为渲染 Ceiling
max_digest_entries = 120
truth_max_bytes = 0 # 0 表示根据当前 Route 的硬输入 Token 容量动态计算
truth_max_entities = 256
mandatory_max_entities = 128
fact_max_entities = 96
verified_change_retention_turns = 32
failure_max_entities = 24
handle_max_entities = 32
omission_sample_max_entities = 8
semantic_narrative_max_input_tokens = 4096
semantic_narrative_max_output_tokens = 0 # 0 = 使用 summary 模型声明的 MaxOutputTokens
semantic_narrative_max_items = 32
semantic_narrative_item_max_bytes = 512
semantic_narrative_timeout = "30s"
semantic_narrative_retry_limit = 1
owner_delta_max_segments = 16
owner_delta_max_bytes = 65536

Tool Result 在首次 `Admit` 时定稿：不超过 ResultStore 合同则保留原文，超限则
写成有界说明 + Handle。未超硬输入时 Sample 不再改写已发送结果，以便保持
append-only 前缀。再只把最近 `context.view.recent_tail_turns` 个 Turn 投影给
模型。完整 transcript 留在 Durable Journal。超窗时对可见 Tail 做一次旧 Turn
折叠；当前 Turn 仍超硬输入则钉住用户请求，收掉已闭合因果组，并继续降级最新
一批结果、调用参数、reasoning 和过长的闭合轮次分析正文。伴随工具调用的判断
默认保留。只有不可再缩前缀（Mandatory 分区 + 当前用户
请求 + output reserve）仍超硬输入，才 `resource_exhausted`。
`context.compact.prepare_tokens` / `auto_compact_tokens` / `emergency_tokens`
为 `0` 时不设提前压缩档位，也不会出现在默认 Context Budget 快照里。History
Replacement 留给显式 `thread.compact` 与 Turn 终态维护。
`max_digest_entries` 限制压缩摘要里的逐条 Removed History；超出的条数写入
omitted 计数，二次压缩会先并入上一份 digest 再套同一上限，而不是整段丢掉。
窗口压力下工具调用参数会收成 identity-only JSON。`Descriptor.identity_keys`
声明保留字段：`exec_command` / `shell_read` 保留截断后的 `command`（及 `cwd`），
文件工具保留 `path`，搜索保留 `query` / `pattern`。未声明时沿用
path / query / handle 等公开白名单，并并入 InputSchema 的 required 标量；
`command` / `content` / `patch` 只有显式声明才保留。声明的 `command` 按与
digest 相同的 160 字节 UTF-8 预算截断，不回灌整段脚本。
显式非零值属于 Operator 成本或 SLA Ceiling，仍须满足顺序和模型窗口范围校验。

Web 中的自定义 Endpoint 和内置目录之外的 Model 必须提交完整模型元数据，包括
Canonical ID、Wire ID、Context、Max Output、Capabilities 和可用的 Reasoning
Efforts。该元数据以 `operator_config` 来源保存；只返回 Model ID 的 `/models` 接口
不能作为容量或能力来源。同名 Model 的 probe 结果按 Provider、Endpoint、Protocol 和
Adapter 组成的 Connection Identity 隔离。旧版缺少元数据来源的自定义 Setup Record
不会迁移为猜测值，而会重新进入 Setup Required。

`context.view.recent_tail_turns` 是模型可见原文的主边界，默认 2。更早 Turn 的
消息会被投影裁掉，但不改写 Durable History 里已发送的 Tool Result。
`keep_recent_tool_results` 仍出现在快照里，不再在后续 Sample 把已消费结果收成
Handle。体积由首次准入决定，需要更多内容时用 `result_get`。Goal、未完成
Todo 和未验证 Change 是每轮必带的 `session_state` 分区，从 Plan / Evidence
Ledger 确定性生成，不依赖 compact 事件。Working Set 与 Evidence 仍按各自分区
预算投影。`truth_max_bytes` 约束该 Mandatory 分区；放不下时在 Sample 前拒绝，
而不是丢掉 Goal。

`context.view.history_token_ceiling` 为 `0` 时，原文 Tail 的 token 上限等于当前
Turn 冻结的硬输入容量减去 Stable / `session_state` 等 Mandatory 分区，而不是
窗口百分比。投影从最新闭合因果组向前填充，直到 `recent_tail_turns` 或该剩余
容量先到达，且不拆 Tool Pair、不隐藏当前用户请求。Operator 显式正值是更紧的
SLA Ceiling，仍不能超过剩余硬输入。若当前 Turn 本身超过该上限，溢出路径先
折叠一次可见 Tail，再对当前 Turn 做钉死用户的 working-set 降级；只有不可再缩
前缀仍超硬输入才 `resource_exhausted`。

`digest` 只允许 `ledger` 或 `ledger+narrative`。`ledger` 是 Mandatory Session
State；`narrative_mode=post_turn` 另加非阻塞 Narrative 分区。`digest=off` 非法，
因为 Session State 不能关掉。`digest=ledger+narrative` 时 `narrative_mode` 必须
是 `post_turn`。Context Budget 快照报告这些 view 字段；只有 Operator 显式设置
时才报告 `prepare_tokens` / `emergency_tokens`。

闭合 Turn 后，带 `source_message_ids` 的 `unresolved` / `pending_job` /
`next_step` 会提升为未完成 Plan Todo，进入每轮 `session_state`，不自动完成已有
条目。同时在 History 之后的 Dynamic 区追加一块 write-once Turn Checkpoint，不写
Stable、不插到 last-2 前面、不改写旧块。`checkpoint_max_bytes=0` 继承
`summary_max_bytes`，再继承 `semantic_narrative_item_max_bytes`（默认 512）。
超限只保留标题与检索指针。完整旧 Turn 原文用 `turn_history`（按 turn id）；
首次投影是该 Turn 尾部结论，全文进 Handle。继续分页用 `result_get` 的
`mode=tail` 或 `mode=query`，不要用默认 `summary`。首次写入后不再改写。被裁掉的旧 Turn 在 `session_state`
给出确定性 `turn_history` 指针；升级前缺失的 Checkpoint 只回封 turn id，不
猜测会话清单。继续原 Session 即可，不必开新会话。当 Plan 已有完成步骤或
Working Set 已有已读路径时，`session_state` 还给出 Resume Fact：不要重复已
完成步骤，下一项未完成工作取第一项 outstanding Plan 标题，并列出全部已读路径。
Prompt 工作集仍按 `context.working_set.max_entries` 取 top-N，两层不要混用。
已读列表超过 `session_state` 分区预算（`context.compact.truth_max_bytes`）时截断
并写 `(N more already-read paths omitted)`。有行号命中时 Resume Fact 还列出
`Located sites`。`working_set` 只列路径；不要再次 `file_read`，除非即将编辑
具体窗口或先前正文已不在当前 Sample。`search_text` / `search_definition` 命中
某路径后优先读该窗口。脏的 `git_status` / `git_diff` 不是重读理由。覆盖范围内
的已知读回放原结果；无法回放时放行必要重读。取消 Checkpoint 保留下一项 Plan
与全部已读路径指针，超 `checkpoint_max_bytes` 时写 omitted；失败仍不带半开 Tool
链。Paused Continue 恢复短 Work Item 胶囊；
源 Turn 已读路径在开局写入 KnownReads，覆盖读回放，git 巡视放行。

[route]
lock = false


[route.plan]
provider = "openai"
model = "gpt-4.1-mini"

[route.vision]
provider = "openai-responses"
model = "gpt-4.1"

[route.summary]
provider = "openai"
model = "gpt-4.1-mini"

[web]
search_backend = "duckduckgo"
```

模型目录声明了默认 Reasoning Effort 时，空的 `reasoning_effort` 使用该默认值；
DeepSeek 的默认值为 High，可选档位为 Off、Low、High、Max。未声明 Effort 集合时，
Runtime 不发送 `reasoning_effort`；声明了集合但没有默认值时，自适应策略只在声明的
集合内选择。显式 Effort 始终固定，且必须由所有已配置 Route 广告；不支持的值会在
Provider I/O 前失败。Reasoning Effort 不再改变输出容量。

`max_output_tokens = 0` 会根据当前 Model Catalog 能力和输入投影后剩余的 Context
空间，为每次请求动态计算上限。初始 Ceiling 来自模型声明的 `MaxOutputTokens`；
正值表示 Operator 显式上限。实际请求还会被 Turn/Session Token Budget、USD Budget
和本次输入后的剩余窗口继续收窄。

默认的 `delegation = "adaptive"` 允许模型在并行收益高于 Spawn 与协调成本时主动委派
独立工作；简单任务、线性依赖任务和写入范围重叠的任务仍由 Parent 完成。`explicit`
只允许 User、Developer、Skill 或内部 System 明确授权的委派，`disabled` 对模型隐藏
Agent Lifecycle Tool。

`spawn_agent` 从当前 Runtime Turn 自动捕获 Parent Context。`context_mode` 默认是
`task_capsule`；`fresh` 不继承 Parent Context，`last_n_turns` 最多加入
`context_turns` 个包含完整 Tool Call/Result 配对的最近 Turn，`full` 需要明确授权或
Role Policy。`task_capsule` 中的每个 Relevant File 附带不超过 2048 字节的当前文件
内容前缀（Excerpt）：它只共享 Workspace 事实、经过脱敏，且 Capsule 预算不足时先
剥离 Excerpt 再丢弃文件路径，Child 需要完整内容时仍应自行读取窗口。Tool 返回
`context_receipt`，记录来源、包含/排除原因、字节和 Token
预算及 SHA-256 Digest。旧的 `fork_context` 和 `parent_context` 参数不再接受。
Capsule 未显式配置容量时，使用父 Turn 硬输入容量扣除当前模型可见 Context 后的余量，
再由 Child Agent Token Budget 收窄；不再套用固定的字节或 Token 档位。

Agent Tree、Mailbox、Result 和 Budget Ledger 持久化在 Workspace State Store。
每个 Agent 具有 Canonical Path 和 CAS Revision；终态 Result 与 Completion Outbox
原子提交。Completion 自动通知 Parent，`wait_agent` 只是对同一事实的主动同步方式。
Mailbox 使用稳定 Message ID 和 `Receive/Ack`，未确认消息在重启后重投。
`max_parallel` 限制活跃 Child 数，`max_resident` 还计入仍保留 Result 或 Worktree 的
已完成 Child，`max_total` 则限制整棵 Durable Tree 的累计 Spawn 数，包括已关闭
Agent。Depth、Token 和 Cost Admission 同样作用于 Nested Agent；Child 只能收窄，
不能扩大 Parent Budget。

Child Authority 只能收紧当前 Session Profile。有效 Posture 遵循
`never < suggest < auto < bypass`；写工具权限是 Parent Tool Catalog 与 Child Role
Contract 的交集，Read-only Role 固定使用 `never`。在 `suggest` 下，Child Approval
会在 Host 中显示 Agent Path 与 Role。Host 通过 Parent Session 提交原 Request ID，
Runtime 将决定路由到权威 Child Thread，并在重启后保留 Pending Approval。Deny 会向
Child 返回结构化 Problem 与 `approval_denied` Tool Result。

QCode 在二进制中内置版本化的 `system-code-review`、`system-debugging`、
`system-refactor` 和 `system-test-expansion` Skill。它们提供领域工作流及 Subagent
拆分建议，不承载安全或委派授权。Skill 同名覆盖顺序为 Workspace 目录、显式配置目录、
当前 Workspace 的沙箱 Home、宿主 User、Builtin；因此项目可以替换默认工作流。Builtin Skill 可通过现有 Skill Control
禁用，其版本、来源和内容摘要会进入 Catalog 与 Receipt；由于内容随二进制固定，
单独使用 Builtin Skill 不要求 Workspace Lock。

沙箱中执行 `npx skills add <package> -g` 时，`-g` 指向当前 Workspace 的私有 HOME，
不会写入宿主用户目录或其他 Workspace。Skill 发现会扫描该 HOME 下的
`.agents/skills`、`.claude/skills` 和 `.qcode/skills`，来源标记为 `workspace`，
设置页与模型工具使用相同的沙箱 Home。目录不能通过符号链接逃出该 Home；
Manifest、Lock 和内容摘要校验保持不变。Catalog 在 Runtime 构造时加载，安装完成后
需重启 Runtime 才会发现新 Skill，浏览器刷新不会重新扫描磁盘。

包含 `skill.toml` 的外部 Skill 必须通过 Skill Control 显式锁定后才能加载内容。
Lock 缺失或与新 Catalog 不一致时，Runtime 和管理入口仍可启动；可先执行 `verify`
查看错误，再执行 `lock` 接受当前已发现的内容和依赖。启动不会自动接受新内容。
Lock 覆盖已发现的受治理包及其依赖，包括禁用项；启停 Skill 不修改锁定集合。
禁用独立 Skill 不会影响其他已锁定 Skill，依赖被禁用 Skill 的加载则仍会拒绝。

Bundled `openai-responses` 路由只有在显式广告 Incremental Transport 时，才会
按 Sticky Session Key 复用 Provider 所有的 WebSocket。第一个 Sample 发送完整
逻辑输入；后续 Sample 仅当所有非输入属性不变，且逻辑输入严格扩展已提交链时，
才发送 `previous_response_id` 与新增输入。Route、属性、Compaction、Retry、
Resume、连接或 Response State 存在任何不确定性时，都发送完整请求。其他
Provider 继续使用原有 HTTP Transport。

Incremental Transport 固定使用 `store=false`。Response State 只保留在活动连接
内存中，并在失败或 Idle Timeout 后删除。Usage Event 仅持久化 Request Bytes
以及 Logical/Transport 的 SHA-256 Digest，不保存 Prompt 内容。Request Byte
下降只属于传输证据，不会被报告为 Token 降幅。

`execution.max_steps` 是连续无结构化进展的显式执行 Lease，默认值为 `64`；
显式配置为 `0` 表示不设置 Sample 数量上限。Work Item 路径集合签名变化（新已读或
已改路径、验证覆盖、Plan 完成步、接受的 Completion、未关闭进程数量）会续期 Lease，
因此跨文件的正常长任务不会因为累计 Sample 数达到 64 而中断。Lease 耗尽后，Kernel 会在预算之外保留一次
Finalization Sample；它只能请求必需输入，或声明 Complete/Incomplete 状态，不能继续
探索或修改。Kernel 授权的 Repair Steps 拥有独立预算。

Agent 还会跟踪连续没有结构化进展的 Sample；对于正在执行 Workspace 工作的 Turn，
No-progress 阶段由显式 `execution.max_steps` 派生：约三分之一时要求收敛，约三分之二
时建议收尾，但不再收窄工具目录。直到完整 Lease 耗尽才进入只保留 Terminal/Input
的结构化 Finalization。Complete 声明仍可选提交；Incomplete 声明记录可恢复的摘要与
具体 Pending Actions。Work Item 签名变化（新已读/已改路径、验证覆盖、Plan 完成
步、接受的 Completion、未关闭进程数量）会立即清零计数。真正的进展是 Turn 内首次
出现的工作状态：Workspace 内容版本、Work Item 签名或工具结果语义 digest。身份切换
本身不清零。回到已见观察且调用身份相同才累加短租约，达到
`execution.implement_no_progress_samples`（默认 6，公开合同字段）进入
Finish-only；回到已见观察但换了身份则累加 `max_steps` 长租约。该值为 `0` 时
短租约继承 `max_steps` 派生的 2/3。
停轮信号是模型停止调用工具并写出用户可见正文；`turn_complete(status=complete)`
可选，未完成的 execution Plan 步骤不拒绝停轮。
已知路径的覆盖重读回放原结果，无法回放时放行；Continue 上的 git 巡视放行。
Progress 与 Convergence 状态都会持久化并在 Runtime 恢复后延续。
`execution.max_steps=0` 且 `implement_no_progress_samples=0` 时不启用基于 Sample
数量的 No-progress 上限，持续工作仍受模型 Context Window 和显式 Token/Cost Budget
约束。

`execution.subagent.max_steps` 同样使用 `0 = 未设置` 语义。可选的
`execution.subagent.wall_time` 是可续期执行 Lease：可观测的子 Runtime 进展会续期；
空闲到期时子 Agent 进入可恢复的 Interrupted 状态，而不是记录为永久失败。

`execution.timeout` 不再是 Provider 调用的总墙钟上限，而是连接建立、TLS 协商和
等待响应头的兼容默认值。非零的 `execution.connection_timeout`、
`execution.tls_handshake_timeout` 和 `execution.response_header_timeout` 可分别覆盖
对应阶段；响应体开始后，生命周期只由 Turn Context 或显式执行 Lease 决定。
`execution.idle_timeout` 约束相邻流事件之间的空闲时间，每收到一个事件就重新计时，
因此持续产出进展的长流不会在固定两分钟后被中断。

`execution.max_concurrent` 是运维侧声明的 Provider 并发合同。同一 Session 内
主 Agent 与全部 Subagent 的并发模型采样都受它约束：Runtime 在两个层面执行同一
声明值——Provider HTTP 客户端的在途请求上限，以及会话级采样门的并发槽。声明为
`1` 时保持严格单飞。任一采样收到 429 后，共享的 Retry-After 冷却会冻结全部并发
槽，冷却结束后按槽位继续。该字段可由 `QCODE_MAX_CONCURRENT` 覆盖。

`execution.rate_limit` 是运维侧声明的初始请求速率上限。无论该值是否为零，Runtime
都会按 Provider、Endpoint、Credential 引用和 Model 共享动态限流状态：优先采用
`Retry-After`，其次采用 `RateLimit-Reset`/`X-RateLimit-Reset`；Provider 未返回时间
提示时，根据实际请求耗时和连续限流反馈逐步延长冷却。冷却等待可取消且不会占用
Provider 并发槽。

`execution.tokens_per_minute` 是运维侧声明的 Provider Throughput 合同，单位为每分钟
Token。它与模型 Context Window、`budget_tokens` / `turn_budget_tokens` 经济预算是三个
独立容量平面。默认 `0` 表示未知：Runtime 不按模型名称发明 TPM，也不在发送前按 Token
做 Admission；请求冷却仍然生效。非零值或 Provider 返回的 Token 专用 Header
（`X-RateLimit-*-Tokens`）成为已知 Burst 后，准入按
`投影输入 + 输出保留` 计算，缓存 Token 在合同未声明免费前计入全量。超过已知 Burst
的合法工作集先对可见 Tail 做一次因果组折叠再重新准入；仍超过 Burst 才拒绝
（`resource_exhausted` / `provider_throughput` / `exceeds_route_burst`），
不会静默重探同一 Digest，也不会改写 Durable History。滚动窗口不足时等待，累计
等待将超过 `execution.rate_limit_wait` 时同样先折叠一次，仍不足则拒绝
（`wait_exceeds_budget`）。通用 `RateLimit-Remaining` 不当作 Token 合同。Host
不实现 Governor。该字段可由 `QCODE_TOKENS_PER_MINUTE` 覆盖。

`context.view.narrative_mode=post_turn` 仅在 `turn.completed` 之后通过 `route.summary` 生成独立的
结构化 Continuation Checkpoint，不阻塞下一轮 Sample。Checkpoint 保留文件与
代码接口、当前工作和下一步，并要求每项引用输入消息。`off` 只保留 Truth Capsule
与原始 Tail。`inline` 不再合法。
`semantic_narrative_max_output_tokens = 0` 与主采样的 `max_output_tokens = 0` 相同：
初始 Ceiling 来自 summary 模型声明的 `MaxOutputTokens`，再被本次输入后的剩余窗口
收窄。正值是 Operator 显式上限。失败时保留 Ledger Session State；对话投影不把
`post_turn` Narrative fallback 显示成 Compaction 卡片。
语义压缩与主采样共用同一套失败分类：兼容提供商上的 HTTP 429（含
`insufficient_quota` 这类瞬时配额文案）走 Rate Limit Recovery Budget；5xx /
Timeout 走 `semantic_narrative_retry_limit`（默认 1）和
`execution.provider_retry_limit` 的较小值。账户硬配额（`FailureQuota`）立即
`fallback=ledger`，不重试。全部 Attempt 与等待仍受
`semantic_narrative_timeout`（默认 30s）约束，超时或预算耗尽后保留 Ledger
Session State 与 write-once Checkpoint，不改写业务 Turn。

`execution.provider_retry_limit` 是单次 Model Sample 对 5xx、网络中断、Timeout
等普通瞬时故障的重试预算。`0` 表示这次 Sample 不重试这些故障。空响应仍允许 1
次恢复。明确分类为 `rate_limit` 的 429 不消耗该次数预算，改由
Rate Limit Recovery Budget 约束。本地推导的退避在现有 20% 幅度内按 Session、
Route 与 Sample 做确定性 jitter；有 `Retry-After` / Reset / Route Cooldown 时仍
等满剩余窗口。

Rate Limit Recovery Budget：

- `execution.rate_limit_retry_limit` 是单次 Sample 允许的 429 恢复次数。默认 `0`
  在 Provider 给出等待信号（`Retry-After` 或 Route Cooldown）时不按次数封顶，
  仍受累计等待预算约束。没有等待信号时继承 `execution.provider_retry_limit`，
  避免本地 10ms 退避在等待预算里空转。同一 Session 内 Parent 与 Child 的并发
  Sample 共用这份次数观察值来计算退避，但 Turn Kernel 的 Retry 序号仍按 Sample
  单调递增。
- `execution.rate_limit_wait` 是 Session 内并发 Sample 共用的累计 429 等待上限。
  默认 `10m`，与连接阶段的 `execution.timeout`（默认 `2m`）分开：后者约束建连、
  TLS 和响应头，前者覆盖 `Retry-After` 冷却和 Parent/Child 串行排队。`0` 仍继承
  `execution.timeout`。一次成功 Sample 或用户发起的新 Parent Turn 会刷新该预算；
  未结束的 `Retry-After` 仍然生效。
- 达到任一边界时，Runtime 返回可恢复的 `provider rate limit retry budget exhausted`，
  不把 429 伪装成模型截断，也不丢失已完成 Tool Side Effect。用户可从 Durable
  Checkpoint 继续或取消。
- 已知 `Retry-After`、Reset 或 Route Cooldown 时，下一次 Attempt 必须等满剩余窗口，
  不会被瞬时故障的单次 Delay Cap 截短后立即重探同一请求。等待可取消。
  Session 内同一时刻只发送一个 Provider Sample，避免 Parent 与多个 Child 同时
  打热限流的模型。

上述字段可通过 `QCODE_PROVIDER_RETRY_LIMIT`、
`QCODE_RATE_LIMIT_RETRY_LIMIT`、`QCODE_RATE_LIMIT_WAIT` 和
`QCODE_TOKENS_PER_MINUTE` 覆盖。账户硬配额
错误仍立即停止。等待由 Runtime 调度，不要求模型或用户轮询。

`execution.lease_timeout` 是 Guard 完成授权到 Executor 消费 Execution Lease 之间的
公开上限，可由 `QCODE_LEASE_TIMEOUT` 或受信配置覆盖。调用 Context 的 Deadline
更早时使用更早值。Lease 被消费后，运行中进程的 Timeout、Cancel、Wait 和 Reap 由
Executor/Broker 生命周期负责，不能因为 Lease 到期而放弃回收。

`execution.approval_timeout` 是等待人工审批的可选墙钟上限。默认 `0`，审批请求随
Turn 或 Session 的取消而结束，不会因用户暂时离开页面而自动阻断 Turn。受监管环境
可设置非零时长，或通过 `QCODE_APPROVAL_TIMEOUT` 覆盖；到期请求仍按
`approval_expired` Fail Closed，不能在过期后执行。

这些 Convergence Budget 不等于物理边界或用户配置的硬上限。Runtime 不会越过
Token/Cost Ceiling，不会猜测半截 Tool Call，不会绕过 Content Filter，也不会在安全
Compaction 后请求仍无法放入 Context 的 Sample。

显式 `budget_tokens`、`budget_usd` 或 Subagent Token/Cost Budget 耗尽时，
Runtime 返回包含 Scope、资源类型和 Used/Limit 的结构化 `resource_exhausted` Fault。
准入检查改用 Projected/Limit，避免把预留容量误报为已提交消耗。`resume_turn` 保留
继续入口。Main Turn 保留可恢复状态，Child 在提交新 Turn 前拒绝准入。预算未提高或
补充前不会自动重试；恢复后不会重跑已闭合 Tool Effect。

`execution.subagent.max_tokens` 与 `max_cost_usd` 是整棵 Agent Tree 的共享上限。
`spawn_agent` 中的 `max_tokens` 或 `max_cost_usd` 是 resident Agent 跨初始 Turn 与
follow-up 的生命周期上限；每次 follow-up 只预留该 Agent 的剩余额度。child 未显式
填写 Token/Cost 上限时，Runtime 按并发度分配一份 Tree 额度
（`Tree / max_parallel`），不会再让第一个 child 预留整棵 Tree。

未知 TOML 字段会被拒绝。这是有意设计：拼错的安全或预算字段不能“看起来已配置但
实际没有生效”。

`context.compact` 先为 Mandatory Truth 和未闭合因果组分配空间，再保留
Protected/Refreshable Truth、Raw Tail 和可选 Narrative。新增计划、Pending Input
或写工具预留如果会超过 Mandatory 上界，会在状态或副作用提交前返回
`resource_exhausted`。`post_turn` Narrative 仅在 `turn.completed` 之后生成滚动
Digest 分区，不得持锁或挡住下一轮 Sample；用户暂停 / 取消 / 失败不调用
summary 模型。Timeout 或 Provider 失败只记录 `fallback=ledger`，Session State
继续从 Ledger 投影。`thread.compact` 可带可选
`focus`（最长 4096 字节）引导这次 Digest，但立即应用确定性 History Replacement，
不把窗口留给 Narrative。默认 `truth_max_bytes=0`，Runtime 按当前 Route 扣除 Output
Reserve 后的硬输入 Token 容量动态派生字节上限，并受公开的 1 MiB 安全上限及
显式 `summary_max_bytes` 约束；显式非零配置仍优先。Narrative 使用
`route.summary`，禁用工具和原生搜索。
Tool 执行前从当前 Turn 的硬输入容量申请 Result Budget，并按并行 Batch 数量分配，
同时受 ResultStore Capacity 收窄。超过预算的模型可见内容替换为带 Handle 与 Digest
的投影，完整原文仍由 Content Store 持有。后续 Sample 只有在
`输入 + Output Reserve` 超过硬窗口时才进一步缩减可重新获取的 Tool Surface。

Memory 使用带稳定 ID 和 Generation 的记录存储。`user`、`workspace` 和
`repository` Scope 按规范化身份隔离；`remember`、`memory_list`、`memory_get`、
`memory_update` 和 `forget` 都经过 Tool Guard。检索首先匹配精确 Scope，再按词法相关性、
更新时间和稳定 ID 排序；`semantic_rerank` 目前必须保持关闭。

## Provider 与模型

主路由由 `[execution].provider` 和 `[execution].model` 决定。`protocol` 描述 Wire
格式，例如：

- `openai_chat`
- `openai_responses`

首次 Setup 的一级 Provider 包括 OpenAI、DeepSeek、GLM 和自定义
OpenAI-Compatible。GLM 内置 `glm-5.3`、`glm-5.3-flash`，固定使用
`https://open.bigmodel.cn/api/coding/paas/v4` 与 `openai_chat`。

GLM Chat 请求默认同时发送 `stream=true` 与 `tool_stream=true`，遵循
[智谱工具流式输出协议](https://docs.bigmodel.cn/cn/guide/capabilities/stream-tool)。
工具参数逐段接收并续期 `execution.idle_timeout`，完整采样成功后才交给 Guard 执行；
其他 Provider 和 Responses 协议不发送 `tool_stream`，也不按模型名称猜测是否支持。

不要猜测标识符。Web Settings 展示 Runtime 发布的 Provider/Model Catalog；即使
Model ID 相同，Provider ID 也可能不同，存在歧义时必须在 TOML 中显式指定 Provider。

Web Settings 的 Connection 页展示并管理 Runtime Provider、Endpoint、Protocol 和凭据；
Models 页展示当前 Session 的 Model、能力来源与 Reasoning 档位。Connection 中的
Provider 变更会在没有活动或待处理工作的前提下重建已注册的 Workspace Runtime，并把
现有 Session Profile 迁移到新 Provider 和初始 Model；构造失败时继续保留旧 Runtime。
Composer 可直接切换历史 Model。同一 Provider 内标记为 `hot` 的 Model 可作为 Session
Profile 在 Turn 之间切换；运行中的 Turn 继续使用启动时冻结的 Route。

用途路由支持 `plan`、`vision` 和 `summary`。设置 `route.lock=true` 后，缺失用途路由
会直接报错，不再静默回落到主执行路由。

## 凭证

`[credential]` 只能保存引用：

| Kind | 含义 | 建议 |
| --- | --- | --- |
| `env` | `name` 是环境变量名 | 本地与 CI 最简单 |
| `file` | `name` 是受保护文件路径 | 使用 `0600` 并交给外部 Secret Manager 管理 |
| `keyring` | `name` 是系统 Keyring Key | 桌面交互环境优先 |

本机 Web 的 Settings 可以把 API Key 只写入系统 Keychain；浏览器和响应只接收
`configured`、`validation` 与引用类型，不会读回密钥值。Web Provider 每次请求解析
Credential Control 的最新引用，因此页面完成 Keychain 轮换后无需重启。

## Mode、Posture 与验证

Mode 写入 TOML；Posture 是 Web Host 启动决策，通过参数提供。二者互不替代。

验证模式：

- `off`：不运行 Verify Gate；
- `soft`：收集并报告失败或未验证状态，不强制增加修复轮次，也不阻止完成；
- `hard`：修复预算耗尽后强制执行结论。

`workspace_change` 和 Completion Contract 不再隐式覆盖验证模式。只有显式 `hard`
策略要求阻止未验证的完成；失败后遵循配置的恢复或回滚策略。Soft 完成不会把
`unavailable`、`failed` 改写成 `passed`。

验证范围：

- `diagnostics`：语言或编辑器诊断；
- `repository`：收集仓库验证命令的实际执行证据；
- `affected`：收集覆盖变更路径的实际执行证据。

命令统一由模型通过 `exec_command` 执行并声明 `verification`、`covered_paths`；
长命令通过 `write_stdin` 结算。Runtime 不再自动探测语言或另起进程重复执行检查。
若设置 `execution.verify.command`，现有 `{paths}`、`{packages}` 显式模板仍可使用，
但该命令现在是所需证据的约束和执行提示；只有匹配命令的当前输入证据能够满足它。
普通命令没有验证声明时不自动提供覆盖证明。

只有仓库验证命令在目标沙箱内稳定可复现时，才应使用 `hard`。

## 编辑后诊断

`[diagnostics.commands]` 将小写文件扩展名映射到可信的、通过 PATH 解析的可执行程序。
每个命令的有界参数列表必须包含 `{path}`。这些命令会在受 Guard 管理的文件编辑后
执行，因此仓库本地配置只有在被显式信任后才能定义它们。

## 状态与持久化

默认用户数据目录为 `~/.qcode/v1`。工作区可通过 `--data-dir` 或
`[state].data_dir` 使用独立目录。State Directory 不能位于 Workspace 内部，也不能
包含 Workspace；启用 Durable Journal 时缺少外部 State Store 会导致 Runtime
Fail Closed。

每个 Workspace 在 `<data-dir>/workspaces/<workspace-id>/` 下拥有彼此隔离的
`control`、`sandbox-home` 和 `artifacts` 目录。只有 `sandbox-home` 可映射为 Sandbox
写目录；`control` 和 `artifacts` 不会暴露给 Workspace 进程。

持久化内容包括 Runtime Projection、Event、CAS、Session Metadata、Usage 和 Journal。
项目仍处于公开发布前，不应依赖旧开发提交产生的数据库兼容性。

`execution.journal.durable=true` 会保留中断 Turn 恢复所需的编辑证据，真实仓库应保持
开启。

stdio MCP 配置示例：

```json
{
  "version": 1,
  "servers": {
    "local-service": {
      "transport": "stdio",
      "host_trusted": true,
      "command": "/absolute/path/to/server",
      "tools": {
        "lookup": {
          "capability": "read",
          "access_mode": "read",
          "parallel_policy": "concurrent",
          "sandbox_requirement": "none"
        }
      }
    }
  }
}
```

`host_trusted` 只承认位于外部 State Directory 的 Operator 配置。它表示 MCP Server
当前以宿主进程权限运行，不表示 MCP Tool Call 绕过 Guard。

## Telemetry

`[telemetry].log_level` 控制本地结构化 Runtime Log。Trace Span、Usage 与 Receipt
使用 Runtime 自身的 SQLite 和终态存储，不需要独立 Capture 或 OTLP 配置。

## 上下文控制

- `execution.base_system`：覆盖默认系统提示词人格。留空使用内置人格加
  探测到的环境指纹（操作系统与架构、登录 Shell、PATH 上 git/go/node/python3
  的版本，进程内缓存、单次探测 2 秒上限）；配置后仍受 `base_system` 分区
  预算约束；
- 指令文件按三层发现：用户全局层（`~/.qcode/AGENTS.md`，回退
  `~/.claude/CLAUDE.md`，取第一个存在者）、仓库根（`AGENTS.md` 与
  `.qcode/instructions.md` 都注入；两者皆无时回退根 `CLAUDE.md`，避免双份）、
  目录层（工作集触达路径的最近祖先目录下的 `AGENTS.md`，回退 `CLAUDE.md`，
  最近者胜出、按目录去重，随工作集逐 Turn 重投影）。仓库根先于全局层注入，
  共享预算紧张时更具体的规则优先存活；
- `index`：有界符号提取；
- `repo_map`：有界仓库结构与入口概览；
- `working_set`：会话触达或 Pin 的路径；只列路径，不放正文。已读路径由
  `session_state` Resume Fact 复述，不要默认再 `file_read`；
- `evidence`：已证明事实、风险和未验证变更；
- `coding_policy`：稳定工作方法；
- `compact`：长历史何时以及如何压缩。

`search_definition` 和 `search_references` 可接收 `path`、`line`、`character`。提供
具体位置时优先使用注入的 Language Provider；未提供位置或 Provider 不可用时，使用
Lexical Repository Index。结果始终标注 `resolution`、`source`、`version` 和
`confidence`；语义调用失败时还会记录降级原因。

关闭上下文段可以减少输入，但通常会增加重复搜索并削弱连续性。优先调整上限，而不是
直接关闭。

## 环境变量

常用覆盖项：

| 变量族 | 字段 |
| --- | --- |
| `QCODE_PROVIDER`、`QCODE_MODEL`、`QCODE_PROTOCOL` | 主模型路由 |
| `QCODE_MODE`、`QCODE_WORKSPACE`、`QCODE_TOOLS` | 执行行为 |
| `QCODE_MAX_*`、`QCODE_TIMEOUT`、`QCODE_LEASE_TIMEOUT`、`QCODE_CONNECTION_TIMEOUT`、`QCODE_TLS_HANDSHAKE_TIMEOUT`、`QCODE_RESPONSE_HEADER_TIMEOUT`、`QCODE_IDLE_TIMEOUT`、`QCODE_PROVIDER_RETRY_LIMIT`、`QCODE_RATE_LIMIT_RETRY_LIMIT`、`QCODE_RATE_LIMIT_WAIT`、`QCODE_TOKENS_PER_MINUTE` | 限制 |
| `QCODE_BUDGET_TOKENS`、`QCODE_BUDGET_USD` | 会话预算 |
| `QCODE_SUBAGENT_*` | 委派模式、Tree 限制、Child 预算、Wall Time 与 Workspace 策略 |
| `QCODE_VERIFY_*` | 验证行为 |
| `QCODE_STATE_*` | 持久化 |
| `QCODE_LOG_LEVEL` | 结构化 Runtime Log |
| `QCODE_CREDENTIAL_KIND`、`QCODE_CREDENTIAL_NAME` | Secret 引用 |
| `QCODE_INDEX_*`、`QCODE_REPO_MAP_*` | 仓库上下文 |
| `QCODE_WORKING_SET_*`、`QCODE_EVIDENCE_*` | 会话上下文 |
| `QCODE_VIEW_*` | 模型可见工作集（tail / residual / digest / narrative） |
| `QCODE_COMPACT_*` | 显式 Replacement 与 Digest 生成上限 |
| `QCODE_VISION_*`、`QCODE_WEB_SEARCH_BACKEND` | 专用 Adapter |

权威列表位于 `internal/config/environment.go` 的环境变量应用逻辑。

## 配置卫生

- 提交安全示例，不提交个人凭证配置。
- 共享示例优先使用工作区相对路径。
- 生产凭证必须位于仓库外。
- 每次修改配置后重启 Web，并检查 Boot/Settings 中的结构化状态。
- 启动参数、环境变量和 TOML 行为不一致时，以 Runtime 发布的有效配置为准。
- Hard Verify、启用写能力和自定义 Shell Command 都应经过 Review。
