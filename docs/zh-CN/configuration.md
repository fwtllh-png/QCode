# 配置说明

## 配置优先级

配置从低到高按以下顺序解析：

```text
内置默认值 < TOML 文件 < QCODE_* 环境变量 < Runtime 启动参数（仅保留数据目录覆盖）
```

启动时通过 `--config` 指定文件；解析或校验失败会显示在 Web Boot Failure Surface：

```bash
qcode --config ./qcode.toml
```

日常使用桌面 App，无需启动参数；工作区与模型在界面中管理。独立 Runtime 只输出
启动 URL，不自动打开浏览器。工具默认启用；TOML 的 `execution.tools` 或
环境变量 `QCODE_TOOLS` 显式设置为 `false` 时仍会禁用工具。

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
deleted_event_retention = "0s"
archive_deleted_events = false

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
mode = "act"                 # 固定为 act，不支持其他值
workspace = "."
tools = true
max_output_tokens = 0           # 0 = 使用当前模型声明的 MaxOutputTokens
max_steps = 64                  # 连续无结构化进展的 Step Lease；0 = 不设置
implement_no_progress_samples = 6  # 同一工作状态且同一工具身份重复时的 finish-only 租约；0 = 继承 max_steps 派生的 2/3
timeout = "2m"                  # 连接、TLS 和响应头阶段
lease_timeout = "2m"            # Guard 授权到 Executor 接管前的 Lease 有效期
workspace_merge_max_diff_bytes = 3145728 # 命令 / Chat 工作区结算 diff 的字节预算；必须为正
approval_timeout = "0s"         # 0 = 审批随 Turn/Session 生命周期，不独立过期
connection_timeout = "0s"       # 0 表示继承 timeout
tls_handshake_timeout = "0s"    # 0 表示继承 timeout
response_header_timeout = "0s"  # 0 表示继承 timeout
idle_timeout = "1m"             # 每个流事件都会续期
max_concurrent = 8
rate_limit = 0                    # 0 = 仅根据 Provider 反馈动态限流
provider_retry_limit = 3          # 每次 Sample 的网络重试/工具参数纠错上限，分别计数；0 = 关闭
rate_limit_retry_limit = 0        # 0 = 有 Retry-After/Cooldown 时不限次数；无等待信号时继承 provider_retry_limit
rate_limit_wait = "10m"           # 累计 429 等待上限；0 = 继承 timeout
tokens_per_minute = 0             # 0 = TPM 未知，不按模型名称发明默认值；只做请求冷却
budget_tokens = 0            # 0 表示不设置累计 Session Token 上限
turn_budget_tokens = 0       # 0 表示不设置累计 Turn Token 上限
budget_usd = 0               # 0 表示不增加成本上限
reasoning_effort = ""        # 空值为自适应；显式值固定 Effort
native_search = false

[execution.environment]
contract = "v1"              # 只接受 v1；准备器是唯一环境权威
profile = "native"           # native | isolated；主 Agent 默认 native，子 Agent 仍 isolated
shared_user_temp = false     # 仅 native 可开；默认关，是安全合同变更
source = ""                  # 空 = 启动进程环境；非空为用户绑定的来源 ID
# [[execution.environment.resources]]  # 精确声明；不依赖语言适配器；仅可信配置


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
recent_tail_turns = 0 # 默认按容量选择；正值限制原文轮数（含当前轮）
keep_recent_tool_results = 0 # 快照字段；已发送 Tool Result 不再按此改写
history_token_ceiling = 0 # 0 表示 Mandatory 分区之后的剩余硬输入容量
digest = "ledger+narrative" # 允许使用有效摘要；ledger 只使用确定性状态、原文和摘录
narrative_mode = "post_turn" # 不阻塞 Sample；仅允许 off 或 post_turn
checkpoint_max_bytes = 0 # 本次可选 Checkpoint 总字节预算；0 使用必要上下文之后的请求余量

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
verified_change_retention_turns = 32 # 沿用已有字段名，现用于普通变更的保留轮数，不要求验证证明
failure_max_entities = 24
handle_max_entities = 32
omission_sample_max_entities = 8
semantic_narrative_max_input_tokens = 0 # 使用 summary Route 窗口、完整封装和作业预算
semantic_narrative_max_output_tokens = 0 # 0 = 使用 summary 模型声明的 MaxOutputTokens
semantic_narrative_max_items = 0 # 不另设条数上限；完整输出仍受总预算限制
semantic_narrative_item_max_bytes = 0 # 不另设单条字节上限
semantic_narrative_timeout = "30s"
semantic_narrative_retry_limit = 1
owner_delta_max_segments = 16
owner_delta_max_bytes = 65536

Tool Result 在首次 `Admit` 时定稿：不超过 ResultStore 合同则保留原文，超限则
写成有界说明 + Handle。未超硬输入时 Sample 不再改写已发送结果，以便保持
append-only 前缀。原文按 `context.view.recent_tail_turns` 的显式轮数上限和
剩余容量选择，完整 transcript 留在 Durable Journal。超窗时沿安全边界缩减
旧 Turn，并重算遗漏提示与完整请求成本；当前 Turn 仍超硬输入则钉住用户请求，
收掉已闭合因果组，并继续降级最新
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

Web 中的每个 Model 必须提交完整模型元数据，包括 Canonical ID、Wire ID、
Context、Max Output、Capabilities 和可用的 Reasoning Efforts；元数据由连接探测
自动填写或手动录入。该元数据以 `operator_config` 来源保存；只返回 Model ID 的
`/models` 接口不能作为容量或能力来源。同名 Model 的 probe 结果按 Provider、
Endpoint、Protocol 和 Adapter 组成的 Connection Identity 隔离。
连接配置文件 `<data-dir>/web-setup/selection.json` 只接受 `version=3` 的显式连接集合：
端点、协议和模型元数据必须完整，连接 ID 与 Provider 必须匹配端点摘要。
缺少版本、旧格式或不完整的记录会明确报错，不自动迁移、丢弃或改写文件。
需要重新配置时，停止 Runtime 后移走该文件，再启动并通过 Connection 设置录入。

当前路由允许模型切换，且目录中存在其他连接的可用、可热切换模型时，Session 同时
开放 `provider` 与 `model` 修改。切换目标必须命中已配置的完整路由；固定路由、
不可用模型和要求重启的条目不开放跨连接热切换。

模型设置中的 `Context tokens (K)` 和 `Max output tokens (K)` 输入框以 K 为单位，
约定 **1 K = 1024 tokens**。例如输入 `128` 表示 131072 tokens。允许小数，但换算后
必须是正的安全整数，且输出上限不能超过上下文容量；编辑已有值时不进行舍入。
API、配置文件、持久化与探测结果摘要继续使用原始 token 数量。

Chat Completions 与 Responses 能力探测使用 `tool_choice=auto`，通过提示词请求
调用探测工具，保留服务端默认思考模式，避免强制工具选择与思考模式冲突。
Streaming、Reasoning、Tool Calls 只根据实际响应事件填写；未观察到工具调用时
不会自动勾选 Tool Calls，可重试探测或根据服务商文档手动确认能力。

模型能力探测与 `/models` 列表请求失败时，错误保留 HTTP 状态码，并展示服务端
JSON `error.message` 中的原因；消息经过凭证脱敏。错误正文沿用 Provider HTTP
诊断上限 `httpclient.MaxErrorBodyBytes`（16 KiB），超过上限、读取失败、非 JSON
或缺少有效消息时仅显示状态码，不展示原始或截断正文。

`context.view.recent_tail_turns` 接受 `0..128`，默认 `0`。零值
不按固定轮数提前裁剪，改由容量选择；正值是普通原文的轮数上限，包含当前轮。
文件配置或环境变量中的显式 `0` 保留其 provenance，不会被默认值覆盖。
更早 Turn 的消息可以退出模型视图，但不改写 Durable History 里已发送的 Tool Result。
P2 的稳定问题定义通过独立来源依赖投影保留，普通原文的轮数和 token ceiling
不限制该分区，但它完整计入模型总窗口和经济准入。
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
SLA Ceiling，仍不能超过剩余硬输入。轮数和原文 ceiling 不授权隐藏当前用户请求。
最终准入在规范化后计入工具定义、动态分区、续写、遗漏提示、协议封装估算与
输出预留；超限时继续缩减并重新计量。若移除一个短轮不足以抵消新增提示，
会继续检查后续安全边界，只有完整成本净下降才接受，否则回滚。当前 Turn 仍超
硬输入时再做钉死用户的 working-set 降级；不可再缩前缀超限才 `resource_exhausted`。

容量、显式输入 ceiling 或原文容量选择迫使历史前缀改变时，会在同一次整理中
尽量留出下一轮工作余量，避免只移除一个短轮、紧接着又改前缀而重复损失缓存。
余量取当前 Turn 消息、最近一个历史 Turn 消息的校准估算，以及有观测基线时
本次 Pending 输入三者最大值，再加最近一次已校准请求的正向预测误差。
首次采样的全部输入不算 Pending 增长；World 状态不重复计入轮次工作量。
余量受有效输入容量限制，目标为有效输入容量减余量；显式输入、body-only 或
原文 ceiling 仍分别作用于各自分区。没有新增固定百分比或提前触发档位。
仅为这个软目标移除已闭合的历史组，达到目标或无可用历史组即停止；硬预算已
满足时，软目标不足不会触发当前用户请求降级或阻断。多组折叠合并为一次回执。
已接受的轮次边界以 `WindowLedger.history_floor_turn` 持久化，后续采样、下一轮及
恢复时不会自动填回已释放的原文空间；显式替换 History 会重置边界。
原文仍可经 `turn_history` 回读，活动来源定义继续独立投影。
采样诊断的 `compaction_headroom_tokens`、`compaction_target_tokens` 记录当次整理目标，
它们不是每次采样都要维持的硬限制；后续新增内容可以消耗这份余量。

`digest` 只允许 `ledger` 或 `ledger+narrative`，默认后者。`ledger` 使用确定性状态、原文和无损摘录，不自动生成摘要。`narrative_mode` 控制新生成：只有 `ledger+narrative` 与 `post_turn` 同时成立才自动调度；`ledger+narrative` 与 `off` 可以复用仍有效的缓存。`digest=off` 非法，Session State 与稳定来源不能关闭。摘要逐次检查来源、过期、Route/Window、重复表示和完整请求余量，不复制已在原文或摘录中的来源；不可拆分的多来源摘要存在部分重叠时整体省略。Context Budget 快照报告这些 view 字段；只有 Operator 显式设置时才报告 `prepare_tokens` / `emergency_tokens`。

闭合 Turn 的摘要条目仅作为解释保存；`unresolved` / `pending_job` / `next_step`
不再自动提升为 Plan Todo。执行义务只由现有计划入口更新；报告或 deliverable
本身不会创建 pending 步骤。闭合时持久化 write-once Turn Checkpoint；每次采样仅把
当前来源引用轮及最近闭合轮的可选块放入 Dynamic 区，不改写旧块。Dynamic 在实际
请求中位于当前 Turn 首条用户请求之前，历史引用与遗漏指引也在该边界提供；同一
工具循环内未变化的资料保持原位置，不反复附在最新工具结果后面。来源选择、
原文覆盖和容量改变时重新投影，恢复会话后仍完整提供本次所需资料。
`checkpoint_max_bytes` 是本次所有可选块的总字节上限；正值仍为 256–1048576，
0 表示只受必要上下文之后的请求余量约束。完整规范化请求（含 Schema、提示、续写、
运行时观测校准和输出预留）还必须满足硬窗口及经济预算。新块渲染仍用公开的摘要
预算（显式 Checkpoint 上限、`summary_max_bytes`、`semantic_narrative_item_max_bytes`
依次生效）；无法装入的可选块跳过，持久内容保留可查询。

闭合完成轮另存 Findings（终答与工具位点），不写入模型可见 Checkpoint 正文。
`turn_history` 互斥接受 `turn`、`source_id`、`item_id` 或 `catalog=true`。
`source_id` 可配 `index_only=true` 读取条目索引；来源/条目/目录默认读头，
`turn` 默认读完整归档尾部及 Findings。`max_bytes` 严格限制返回内容字节数，
0 使用既有工具结果准入；显式分页不足以容纳一个 UTF-8 字符时返回错误。
继续向后读取须保留选择器，传 `offset=next_offset` 与返回的 `content_digest`；
读取初始尾页之前的内容用 `from=head`，或用同 digest 和 `offset=previous_offset`
从头开始。游标针对本次渲染内容的 UTF-8 字节范围，内容改变时拒绝旧游标。
`result_get` 只能取回已保存的工具页；该页因工具准入再次缩短时先取回页内遗漏，
源分页遗漏仍用 `turn_history`。归档优先于内存残片，无完整归档时标明完整性未知。

未绑定报告过大时，请求改为目录恢复提示，使用会话状态预算建议分页大小；
恢复期间只开放 `turn_history`、`result_get`、`update_plan`、`request_user_input`。
实际工具执行入口也检查该状态，同批次绑定焦点不会提前开放业务工具。
下一次采样装入已绑定的完整定义后恢复正常；显式焦点或未完成 Plan 所需定义
本身超限时仍明确失败，需要缩小选择或拆分任务。

被裁掉的旧 Turn 由本次
`ProjectionResult` 生成临时 `[context_selection]` 提示，使用合法参数
`turn_history {"turn":1}`。提示同时覆盖轮数、原文 token ceiling、模型容量、
operator ceiling、经济预算和吞吐/溢出恢复等原因，不再写入轮内冻结的 World。
连续轮号按原因聚合，稀疏轮号不虚构区间。提示复用 `truth_max_bytes` 派生的
会话状态预算；放不下时省去说明并汇总未展示组数，最小提示仍超限则拒绝采样。
完整遗漏元数据不随提示缩短而丢弃。升级前缺失的 Checkpoint 只回封 turn id，
不猜测会话清单。已建立索引的终答由即时 `[conversation_references]` 提供，
不再在冻结的 Continuity 胶囊重复正文；没有索引的旧 Findings 和会话工具位点沿用
原有 Continuity/检索路径。继续原 Session 即可，不必开新会话。当 Plan 已有完成步骤或
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


[route.vision]
provider = "openai-responses"
model = "gpt-4.1"

[route.summary]
provider = "openai"
model = "gpt-4.1-mini"

[route.judge]
provider = "openai"
model = "gpt-4.1-mini"

[web]
search_backend = "duckduckgo"
```

模型目录声明了默认 Reasoning Effort 时，空的 `reasoning_effort` 使用该默认值；
DeepSeek 的默认值为 High，可选档位为 Off、Low、High、Max。未声明 Effort 集合时，
Runtime 不发送 `reasoning_effort`；声明了集合但没有默认值时，自适应策略只在声明的
集合内选择。显式 Effort 始终固定，且必须由 act、vision、summary Route 广告；不支持的值会在
Provider I/O 前失败。Reasoning Effort 不再改变输出容量。
Guardian 的 judge 路由按自身能力选择 Reasoning Effort，不继承主 Agent 的强制值。

执行模式固定为 `act`；`execution.mode`、`QCODE_MODE` 及 Session/Preset 的 `mode`
仅接受该值，Profile Patch 不再接受模式变更。旧的非 act 配置和 `[route.plan]`
会明确报错，需移除旧路由并将模式改为 `act`。主 Turn 使用 Act 路由，辅助路由仅有
`vision`、`summary` 和 `judge`。未配置的辅助路由在 `route.lock=false` 时回退到
当前 act；锁定时必须显式配置，错误配置不会自动换模型。

Guardian 模型服务配置如下，默认关闭，仅接受 Operator 配置或显式信任的仓库配置：

```toml
[security.guardian]
enabled = false
timeout = "10s"
max_output_tokens = 0
```

`timeout` 没有隐藏默认值，启用时必须显式指定正时长；示例 10 秒是 Operator 选择的
总预算，覆盖排队、速率等待、请求和读流，并受调用方更早的期限约束。格式错误或负值
始终报错。`max_output_tokens=0` 从 judge 的权威输出能力、完整输入后的剩余窗口和
共享预算推导；正值是上限，超过模型能力会报错。必需来源超出窗口时审查失败，不删掉限制。

每次审查只调用一次 Provider，不重试、不带工具或原生搜索；Token 和费用与前台、标题、
摘要共享预留和实际结算，失败、无效及取消的已观测 Usage 也计费。统一
`QCODE_DISABLE_APPROVAL_AUTO_REVIEW=1` 会关闭 Guardian。显式启用后，
只有持久审计提交成功、当前 Policy 允许且启动前授权仍有效的调用才自动放行。
审查、审计或证据校验失败保留人工审批；成功审查在工具详情展示，故障原因进入原审批卡。
重启不会把历史模型评估当作授权。P6 已提供脱敏评估与启用/回退流程，默认仍关闭；
首轮结果包含真实模型故障、未知价格和生产覆盖率缺口，不能作为默认启用的依据。
详见 [Guardian 评估](./guardian-evaluation.md) 和 [Guardian 设计](./guardian-auto-review-design.md)。

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
Capsule 使用统一文本 token 估算，独立检查 token 和显式字节上限，不按固定
bytes/token 比例换算。Token 上限取已配置上限、父 Turn 剩余容量和 Child Agent
Token Budget 中有效限制的最小值；字节上限未配置时回执为 0，不生成隐含字节上限。
除 `fresh` 外，各模式独立携带父侧已绑定及未完成 Plan 必需的来源定义，包含稳定
source/item ID、原始 digest/字节范围及脱敏标记；未绑定目录需父侧先消歧并绑定。
历史裁剪先删可选轮次、证据和文件提示，不裁切任务目标、当前请求、角色/工作区约束
或所选来源定义。必要材料本身放不下时拒绝委派，由父侧缩小任务或调整预算。

Agent Tree、Mailbox、Result 和 Budget Ledger 持久化在 Workspace State Store。
每个 Agent 具有 Canonical Path 和 CAS Revision；终态 Result 与 Completion Outbox
原子提交。Completion 自动通知 Parent，`wait_agent` 只是对同一事实的主动同步方式。
Mailbox 使用稳定 Message ID 和 `Receive/Ack`，未确认消息在重启后重投。
以下上限都按 Session 的 Agent Tree 计算，同一 Workspace Runtime 中的其他 Session
互不占用。`max_parallel` 限制活跃 Child 数，`max_resident` 还计入仍保留 Result 或
Worktree 的已完成 Child，`max_total` 则限制整棵 Durable Tree 的累计 Spawn 数，包括
已关闭 Agent；在第一个 Turn 获准之前就被关闭的委派（例如因预算投影被拒）不计入。
正在创建 Worktree 的 Spawn 已计入 `max_total`，但创建 Worktree 期间不持有 Manager
锁，不会阻塞其他 Child 的 Wait、Settle 或工具调用。Depth、Token 和 Cost Admission
同样作用于 Nested Agent；Child 只能收窄，不能扩大 Parent Budget。这些准入、预留和
结算只由 `subagent.Manager` 执行，账本在重启后由持久化的 Agent 状态重建。

Child Authority 只能收紧当前 Session Profile。有效 Posture 遵循
`never < suggest < auto < bypass`；写工具权限是 Parent Tool Catalog 与 Child Role
Contract 的交集，Read-only Role 固定使用 `never`；`suggest` 仅保留为内部收紧级别。
在 `Auto` 下需要用户审批的 Child Approval
会在 Host 中显示 Agent Path 与 Role。Host 通过 Parent Session 提交原 Request ID，
Runtime 将决定路由到权威 Child Thread，并在重启后保留 Pending Approval。Deny 会向
Child 返回结构化 Problem 与 `approval_denied` Tool Result。

Skill 从文件系统目录发现，同名 Skill 按以下顺序取首个匹配：Workspace 目录、
显式配置目录、当前 Workspace 的沙箱 Home、宿主 User 目录。
Skill 的启用状态由 Skill Control 管理，版本、来源和内容摘要进入 Catalog 与 Receipt。

沙箱中执行 `npx skills add <package> -g` 时，`-g` 指向当前 Workspace 的私有 HOME，
不会写入宿主用户目录或其他 Workspace。Skill 发现会扫描该 HOME 下的
`.agents/skills`、`.claude/skills` 和 `.qcode/skills`，来源标记为 `workspace`，
设置页与模型工具使用相同的沙箱 Home。目录不能通过符号链接逃出该 Home；
Manifest、Lock 和内容摘要校验保持不变。Catalog 在 Runtime 构造时加载，安装完成后
需重启 Runtime 才会发现新 Skill，浏览器刷新不会重新扫描磁盘。

包含 `skill.toml` 的 Skill 必须通过 Skill Control 显式锁定后才能加载内容。
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
内存中，并在失败或 Idle Timeout 后删除。采样流在 Idle Timeout 后被关闭时，其所在
连接随之退役：读取不再占用会话，下一次 Sample 在新连接上发送完整请求，旧连接迟到
的帧不会串入新响应。Usage Event 仅持久化 Request Bytes
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
它不判断内容是否有效。工具参数另按 JSON 对象成员唯一性进行增量校验：同一对象的
重复成员一经确认即关闭当前响应流并记录 Provider 响应错误，随后按
`execution.provider_retry_limit` 重新生成完整调用；保留已有工作，不续写无效参数。
这类错误不需要调整超时。普通文本或字符串值的重复不据此判为错误。

`execution.max_concurrent` 是运维侧声明的 Provider 并发合同。同一 Session 内
主 Agent 与全部 Subagent 的并发模型采样都受它约束：Runtime 在两个层面执行同一
声明值——Provider HTTP 客户端的在途请求上限，以及会话级采样门的并发槽。声明为
`1` 时保持严格单飞。任一采样收到 429 后，共享的 Retry-After 冷却会冻结全部并发
槽，冷却结束后按槽位继续。该字段可由 `QCODE_MAX_CONCURRENT` 覆盖。

`execution.rate_limit` 是运维侧声明的初始请求速率上限。无论该值是否为零，Runtime
都会按 Provider、Endpoint、Credential 引用和 Model 共享动态限流状态：优先采用
`Retry-After`，其次采用 `RateLimit-Reset`/`X-RateLimit-Reset`；Provider 未返回时间
提示时，根据实际请求耗时和连续限流反馈逐步延长冷却。冷却等待可取消且不会占用
Provider 并发槽。不同 Provider 的 `X-RateLimit-Reset` 可能是相对秒数、Unix 秒或
Unix 毫秒，Runtime 取三种读法中最接近当前时间的一种；已过去的重置时间视为无需等待，
不会把毫秒时间戳误当成数十年的冷却。

`execution.tokens_per_minute` 是运维侧声明的 Provider Throughput 合同，单位为每分钟
Token。它与模型 Context Window、`budget_tokens` / `turn_budget_tokens` 经济预算是三个
独立容量平面。默认 `0` 表示未知：Runtime 不按模型名称发明 TPM，也不在发送前按 Token
做 Admission；请求冷却仍然生效。非零值或 Provider 返回的 Token 专用 Header
（`X-RateLimit-*-Tokens`）成为已知 Burst 后，准入按
`投影输入 + 输出保留` 计算，缓存 Token 在合同未声明免费前计入全量。超过已知 Burst
的合法工作集先按安全因果组边界找到包含提示成本的净缩减，再重新准入；仍超过 Burst 才拒绝
（`resource_exhausted` / `provider_throughput` / `exceeds_route_burst`），
不会静默重探同一 Digest，也不会改写 Durable History。滚动窗口不足时等待，累计
等待将超过 `execution.rate_limit_wait` 时同样先尝试净缩减，仍不足则拒绝
（`wait_exceeds_budget`）。通用 `RateLimit-Remaining` 不当作 Token 合同。Host
不实现 Governor。该字段可由 `QCODE_TOKENS_PER_MINUTE` 覆盖。

`context.view.digest=ledger+narrative` 且 `narrative_mode=post_turn` 时，仅在 `turn.completed` 后为实际投影拟淘汰的、尚未处理的来源生成可选解释性摘要；显式 `thread.compact` 也可发起生成。编号结构、原文来源和 write-once Checkpoint 在终态确定性封存，不依赖模型。`off` 关闭摘要生成，保留这些状态；`inline` 不再合法。

`semantic_narrative_max_input_tokens` 默认 `0`，由 summary Route 容量和完整封装导出，实际调用继续受作业的会话预算约束；正值是每次完整摘要请求的额外 token ceiling，包括提示、Truth 和来源元数据。输入先按消息、Markdown 标题/列表/段落/代码块划分，放不下的块继续按 UTF-8 范围拆分，保留父结构、原编号和全部尾部。已移除默认每消息 1024 字节截断、英文关键词优先级和固定字节/token 换算。计量使用冻结 summary Route 的规范化请求与 TokenEstimator。若元数据加最小范围也放不下，生成失败并保留原文。

`semantic_narrative_max_output_tokens = 0` 从 summary 模型声明的 `MaxOutputTokens` 和本次输入后的剩余窗口导出；正值是额外的 token 上限。`semantic_narrative_max_items` 和 `semantic_narrative_item_max_bytes` 默认 `0`，在总输出预算内分配，底层不会补回旧的 32 条或 512 字节限制；正值分别限制单次输出条目数和条目字节数。引用必须覆盖本次全部输入范围，聚合后再次核对完整源范围与 digest；覆盖失败或没有净压缩收益的候选不安装。机械引用覆盖不代表自然语言语义忠实，活动问题定义仍保留原文锚点。

后台生成复用共享 Provider 并发许可与限流，前台到来时取消可选采样；前台不等待摘要作业结算，实际 Provider 调用仍遵守配置的并发上限。结果只在空闲或下一轮开始的安全边界安装，不修改已冻结 Sample。安装检查来源、撤回、epoch、权限、Route 与显式来源替代，只合并解释性表示，不回写 Plan、选择或验证状态。持久化通过现有 Context Manifest/CAS 维护提交，在同一事务内以最新业务终态与维护快照中较新的 Context revision 比较 BaseRevision；版本冲突时保留当前状态，损坏终态不能作为缺省版本跳过。

5xx、网络错误和 Timeout 的重试复用 `semantic_narrative_retry_limit`（默认 1）与 `execution.provider_retry_limit`；429 使用既有 Rate Limit Recovery Budget，硬配额不重试。`semantic_narrative_timeout`（默认 30s）覆盖一次作业的分块、排队、请求与重试，总时限不会逐块重置。每次调用计入共享 session token/费用预算，校验失败、超时和过期候选仍结算已观测用量。相同来源、Route、规则与配置的已处理输入持久化去重，避免每轮重复失败重试；显式 compact 可重新生成。默认容量选择不会仅因为完成了一轮就生成摘要；没有实际遗漏来源时不调度作业。

`execution.provider_retry_limit` 是单次 Model Sample 对 5xx、网络中断、Timeout
等普通瞬时故障的重试预算。`0` 表示这次 Sample 不重试这些故障。空响应仍允许 1
次恢复。明确分类为 `rate_limit` 的 429 不消耗该次数预算，改由
Rate Limit Recovery Budget 约束。本地推导的退避在现有 20% 幅度内按 Session、
Route 与 Sample 做确定性 jitter；有 `Retry-After` / Reset / Route Cooldown 时仍
等满剩余窗口。普通故障次数、429 次数和累计等待预留随 Sample 持久化，重启不会
把 429 算成网络重试，也不会清空已预留的等待预算。等待开始前落盘；进程在等待中
退出时，恢复仅等待原截止时间的剩余部分。吞吐准入等待共用此预算，不增加重试次数。

同一配置也约束工具参数的自动纠错次数，和网络重试分别计数。重复 JSON 成员或正常
停止时无效的工具调用会被整批拒绝，模型在同一 Turn 的新响应片段中重新生成；默认
最多 3 次，`0` 表示直接保留草稿并报告。已完成的工具不重放，拒绝的参数不续写。
纠错授权先写入 Durable Assembly，重启不会清零。耗尽后提供 Continue 入口，并说明
应检查 Provider 或选择其他模型。`max_tokens` 等正常截断仍走续写机制，不占参数纠错额度。
续写内容使完整请求超窗时，Runtime 先保存完整内容，再按实际窗口余量投影可分页读取的
摘录；保留原始 Assembly 和用户可见正文。过大的未执行工具片段会要求重新生成较小的
完整调用，不引入额外重试或压缩大小阈值。

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
（`Tree / max_parallel`），不会再让第一个 child 预留整棵 Tree。真实用量只有在
回执中才出现，因此一个 Turn 的实际消耗可以超过它的预留；这不会把已完成的 Child 改成
失败，超出部分照常记账，之后的 Turn 或 Spawn 会以 `resource_exhausted` 拒绝
（Scope 为 `agent:<id>` 或 `agent_tree:<session>`）。并发槽位耗尽返回可重试的
`resource_exhausted`（reason `concurrency_capacity_exhausted`），`max_total` 耗尽返回
不可重试的 `resource_exhausted`（reason `spawn_capacity_exhausted`）。

未知 TOML 字段会被拒绝。这是有意设计：拼错的安全或预算字段不能“看起来已配置但
实际没有生效”。

`context.compact` 先为 Mandatory Truth 和未闭合因果组分配空间，再保留
Protected/Refreshable Truth、Raw Tail 和可选 Narrative。新增计划如果会超过
Mandatory 上界，仍拒绝状态提交。工具批次的 Pending Input 或写入预留超额时，
执行前向所有待执行调用返回 `context_reservation_exceeded`，并提示拆批或验证已有变更；
模型可在同一 Turn 继续。原有 Mandatory 上界不变，也不缓存该批拒绝来阻止后续拆批。
`post_turn` Narrative 仅在 `turn.completed` 之后生成滚动
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

主路由由 `[execution].provider`、`[execution].model` 与 `execution.base_url` 决定；
`base_url` 是 Provider 的 OpenAI-Compatible 端点（HTTPS 或回环地址），没有内置
Provider 目录可以省略。`protocol` 描述 Wire 格式，例如：

- `openai_chat`
- `openai_responses`

模型元数据通过 `execution.model_metadata` 指向一份 JSON 文件声明（Canonical ID、
Wire ID、Context、Max Output 与完整 Capability 声明）；Runtime 不猜测模型限制。
首次 Setup 在 Web 中完成：填写 Base URL、Protocol、Model ID 与 API Key 四项要素，
元数据由连接探测自动填写或手动录入，不存在内置 Provider 预设。

OpenAI-compatible Chat 请求统一同时发送 `stream=true` 与 `tool_stream=true`：
兼容网关在流式模式下默认缓冲工具参数，`tool_stream` 让参数逐段接收并续期
`execution.idle_timeout`，完整采样成功后才交给 Guard 执行。该字段对每条连接
一致发送，不按 Provider 名称区分；Responses 协议不发送 `tool_stream`。

不要猜测标识符。Web Settings 展示 Runtime 发布的 Provider/Model Catalog；即使
Model ID 相同，Provider ID 也可能不同，存在歧义时必须在 TOML 中显式指定 Provider。

Web Settings 的 Connection 页展示并管理 Runtime Provider、Endpoint、Protocol 和凭据；
Models 页展示当前 Session 的 Model、能力来源与 Reasoning 档位。Connection 中的
Provider 变更会在没有活动或待处理工作的前提下重建已注册的 Workspace Runtime，并把
现有 Session Profile 迁移到新 Provider 和初始 Model；构造失败时继续保留旧 Runtime。
Composer 可直接切换历史 Model。同一 Provider 内标记为 `hot` 的 Model 可作为 Session
Profile 在 Turn 之间切换；运行中的 Turn 继续使用启动时冻结的 Route。

用途路由支持 `vision`、`summary` 和 `judge`。设置 `route.lock=true` 后，缺失用途路由
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

Mode 固定为 `act`；新 Session 的 Posture 默认为 `auto`，通过界面的 Permissions
或 Session 设置修改，不提供启动参数。二者互不替代。
界面提供 `Read only`（`never`）、`Auto`（`auto`）和 `Full Access`（`bypass`）；
Full Access 授予普通命令宿主文件读写、直接联网和浏览器/PTY 等系统能力，并预授权
Git 推送等工具声明的单次审批；显式 ask、deny、hold、Surface 限制和强制编辑审阅仍生效。
显式 `write_paths` 只限制文件写入，`network_targets` / `allow_loopback` 只限制网络，
其余维度保持原授权。`settle=discard` 使用隔离范围沙箱。
凭据、工作区控制目录、QCode 状态和显式策略仍受保护。旧 `suggest` 会话加载时合并到 Auto。
`wire.ExecOptions.ProfilePermissionCeiling` 由可信 Host 声明可选上限；空值使用启动姿态，
未知值拒绝构造。Web 声明 `bypass` 上限但保持 `auto` 默认值，Session 参数不能更改 Host 上限。

通用 Verify Gate 已移除。`execution.verify` 和 `QCODE_VERIFY_*` 不再是配置入口；
旧配置中的这部分不会生效。测试或 CI 由用户要求、仓库约定与任务需要驱动，通过普通
命令执行并记录客观结果，不再要求模型声明覆盖路径，也不以覆盖账本阻止完成。
权限、审批、沙箱及 Journal 结算规则保持生效。

## 编辑后诊断

`[diagnostics.commands]` 将小写文件扩展名映射到可信的、通过 PATH 解析的可执行程序。
每个命令的有界参数列表必须包含 `{path}`。这些命令会在受 Guard 管理的文件编辑后
执行，因此仓库本地配置只有在被显式信任后才能定义它们。

## 状态与持久化

`execution.workspace_merge_max_diff_bytes` 是命令隔离与 Chat 工作区合并共享的
实际结算 diff 上限，默认 `3145728`（3 MiB），来源为原有结算预览内存预算。
它独立于授权路径条目数，也不限制目录内文件总数。支持 TOML 和环境变量
`QCODE_WORKSPACE_MERGE_MAX_DIFF_BYTES`；环境变量覆盖文件值，来源记录在
配置 provenance，零和负数拒绝加载。超出预算会在应用变更前拒绝结算并提示
收窄写入或调整配置；不会仅应用一部分文件。显式调高不会再被隐藏的 512 个
变更文件上限收紧。目录扫描与隔离复制受调用取消控制。

默认用户数据目录为 `~/.qcode/v1`。工作区可通过 `--data-dir` 或
`[state].data_dir` 使用独立目录。State Directory 不能位于 Workspace 内部，也不能
包含 Workspace；启用 Durable Journal 时缺少外部 State Store 会导致 Runtime
Fail Closed。

每个 Workspace 在 `<data-dir>/workspaces/<workspace-id>/` 下拥有彼此隔离的
`control`、`sandbox-home` 和 `artifacts` 目录。只有 `sandbox-home` 可映射为 Sandbox
写目录；`control` 和 `artifacts` 不会暴露给 Workspace 进程。

持久化内容包括 Runtime Projection、Event、CAS、Session Metadata、Usage 和 Journal。
项目仍处于公开发布前，不应依赖旧开发提交产生的数据库兼容性。

当前状态 schema 为 5，CAS 内容及引用归属保存在 SQLite；旧版本数据库直接拒绝，
不自动迁移或删除。切换前应停止 Runtime，再自行移走旧数据或选择新的空数据目录。

`state.busy_timeout`（环境变量 `QCODE_STATE_BUSY_TIMEOUT`）是 SQLite 等待锁的上限，
默认 `"5s"`，接受 1ns 到 5m 的 Go duration。持久化 Runtime 同时用它作为结算重试
间隔：Operation 的拒绝事件、Outcome 事件或 Commit Receipt 写入失败，以及已提交
终态的 Outbox 投影失败后，Runtime 每隔该时长在后台重试，直到写入成功；这一窗口内
写入持续失败才会进入重试，因此不另设独立阈值。

`state.deleted_event_retention` 控制已删除会话的审计日志保留时长，默认 `"0s"`，
接受非负 Go duration（如 `"24h"`）。到期日志在启动、删除会话后的维护或显式
`Store.Maintain` 时清理；活跃会话和仅归档的会话不受影响。
`state.archive_deleted_events` 默认 `false`；设为 `true` 时，清理前将目标日志写入
`<data-dir>/event-archives/<sha256>.jsonl.gz`，归档独立保留，不进入在线重放。
对应环境变量为 `QCODE_STATE_DELETED_EVENT_RETENTION` 和
`QCODE_STATE_ARCHIVE_DELETED_EVENTS`，来源进入配置 provenance。无效或负时长拒绝加载。
清理不重排事件序号或降低高水位，旧游标会跳过已清理事件并继续读取保留记录。

`execution.journal.durable=true` 会保留中断 Turn 恢复所需的编辑证据，真实仓库应保持
开启。

`[execution.environment]` 是 Sandbox 执行环境契约的公开开关，默认已切换为
`v1` + `native`：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `contract` | `v1` | 唯一合同。启用准备器；没有适配器时仍接受精确资源声明。配置拒绝其它值 |
| `profile` | `native` | `native` 保留宿主 HOME 变量但仍按资源授权；`isolated` 使用私有 `sandbox-home`。子 Agent 运行时仍 isolated |
| `shared_user_temp` | `false` | 允许当前用户系统临时区。仅 `profile=native` 可开；打开后不再承诺 Agent/Workspace 临时文件隔离 |
| `source` | 空 | 空表示使用启动 QCode 的进程环境；非空是用户显式绑定的来源 ID |
| `resources` | 空 | 用户声明的精确环境资源（`host_config` / `host_toolchain` / `cache` / `network` / `credential`+`use` / `shared_user_temp`）。不经适配器即可准备。`namespace=network` 可被空 `network_targets` 继承到 Session Gate。最多 64 条。`workspace` 仍走命令 `write_paths`。仅可信配置可设；未信任仓库文件忽略。准备器还会合并平台 PATH 源并把这些目录列为 `host_toolchain`。`native` 下 Git 适配器还会加入已存在的用户 git 配置文件，不加入凭证文件 |

默认准备链不调用 `go env`、不读取 `go.mod`，不自动配置 Go 缓存或代理变量。
语言工具需要的变量、配置文件与缓存目录使用 `resources` 显式声明；任务需要查询
工具版本时通过普通受控工具执行。宿主已有私有模块认证不会自动迁入沙箱，需要
使用用户显式接入且已经授权的外部服务。

PATH 可执行文件的实际动态库依赖由平台按精确文件只读绑定；这不会自动开放
工具的配置目录。受控环境在没有显式设置时提供 `OPENSSL_CONF=`：依据 OpenSSL
`config(5)`，空字符串表示不加载配置文件，普通 Node/npm 命令不必先读取宿主
`openssl.cnf`。它不关闭 TLS 证书校验，也不替换证书文件和信任库设置。
若需要自定义 OpenSSL Provider、FIPS 或其它宿主配置，使用 `host_config` 声明
文件并指定 `env = "OPENSSL_CONF"`；来源环境、资源声明和单次命令的显式值均可
覆盖缺省值，读取权限仍需独立授权。缓存使用 `cache` 声明。共享临时区的写权限包含
创建条目所需的元数据读取，已有共享文件的内容读取仍需单独授权。

环境在准备时确定，后续受控命令不重新继承宿主环境变化。普通变量的优先级为
受控环境缺省值 < 平台与来源基线 < `resources` 声明 < 单次命令 `env`；同一层同名异值报错，
重复资源名也会报错，空字符串可以显式覆盖已有值。HOME、TMPDIR、TMP、TEMP
由 Profile 与沙箱目录决定，不能通过普通声明改写；HTTP 代理变量由当前执行的
受管通道生成。变量本身不授予路径或网络访问，相关资源仍需独立声明和授权。
秘密名与解释器预加载变量在可信声明和命令声明中使用同一套拒绝规则。
只读命令不自动注入 `PYTHONDONTWRITEBYTECODE`；如工具需要该设置，可显式声明。

对应环境变量为 `QCODE_ENVIRONMENT_CONTRACT`、`QCODE_ENVIRONMENT_PROFILE`、
`QCODE_ENVIRONMENT_SHARED_USER_TEMP` 和 `QCODE_ENVIRONMENT_SOURCE`，来源进入
configuration provenance。资源声明列表没有环境变量入口，只能写在可信配置文件里。
`isolated` 与 `shared_user_temp=true` 组合拒绝加载，声明 `shared_user_temp` 资源时
必须同时打开该开关。`credential/use` 只描述资源身份；当前没有通用凭证绑定器，
这类资源标记为未绑定，必需资源产生 `credential_binder_unavailable` 准备事实。
声明凭证不会交付秘密或授予网络访问。

旧 `execution.environment.auth_services` 字段已删除，可信和未信任配置中的该字段
均报未知字段错误，空列表也不例外。没有迁移或兼容回退。私有制品源认证可使用用户
显式接入的受限外部服务；目标仍须通过现有网络授权，QCode 不自动创建该服务。

声明示例：

```toml
[execution.environment]
contract = "v1"
profile = "native"

[[execution.environment.resources]]
name = "tool-config"
namespace = "host_config"
access = "read"
path = "/opt/tool/config.toml"

[[execution.environment.resources]]
name = "tool-cache"
namespace = "cache"
access = "write"
path = "sandbox-home/cache/tool"
env = "TOOL_CACHE"
tree = true

```

探测超时和输出上限继续使用已有的 `ToolchainProbeTimeout` 与
`ToolchainProbeMaxOutputBytes`，不另设隐藏阈值。进程失败的
`error_category` / `required_action` 只来自 Gate 或后端事实，
不从命令文本推断。准备时未绑定资源作为独立的
`environment_preparation_facts` 工具 Metadata 保留，成功和失败回执均可携带；
这些事实本身不证明当前命令的失败原因，不覆盖 `error_category`。完整设计见
[Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)。

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


**P2 会话来源与计划引用**

完成轮的终答在终态压缩之前建立 Markdown 来源索引，和终态 Context Snapshot
一起提交；`narrative_mode=off` 不关闭来源保存。标题/列表保留原始编号、嵌套父项
及 UTF-8 字节范围，代码围栏里的数字不作为列表项。解析范围无法可靠表达时保留
整个原文块，不用摘要截断替代定义。失败、取消或状态未知的轮次不推断为完成报告。

`update_plan` 的 `steps[].id` 是稳定步骤身份。新增步骤可以省略，由工具生成并在结果
中返回；后续更新、改名、重排须复用它。`steps[].reference_item_ids` 引用当前会话
索引中的条目 ID。相同标题可属于不同步骤；已完成步骤可以保留被替代的历史来源，
未完成步骤必须在来源替代时显式重绑。计划完成状态与验证证据仍由各自账本维护。

`update_plan.context_selection` 接受 `group_ids`、`item_ids` 和
`replacements=[{old_group_id,new_group_id}]`。可只传该对象切换材料焦点，不创建
Plan 或 PlanDelta；空对象清空焦点。运行时附加当前用户请求的轮次、Turn ID 与
摘要作为出处，模型只能依据用户选择或纠正提交绑定。引用只在当前 Context 内解析，
无效、重复、已替代的活动引用会拒绝整个更新。`submit_plan` 不接受此字段。

没有显式焦点时，提供尚有效的结构化报告和最近终答，多个报告的相同编号保持歧义，
由主模型依据用户上下文判断或澄清。显式焦点及未完成 Plan 的关联定义参与每次
即时投影；嵌套条目保留父项前提。原文已经可见时只补 ID/范围元数据，避免复制正文。
来源依赖同样计入最终请求，放不下时以容量错误拒绝采样，不静默丢弃报告尾部。
P2 尚不自动分页大量候选；目录分页、压力下分段与更广生命周期验收在 P4 完成。

清空焦点后，可通过 `turn_history` 重新取回旧报告的稳定来源组和条目 ID，再提交
新的绑定。完整 Transcript 不可用而终答来源仍在时，工具只返回已保存终答并明确
标注范围；来源回读同样检查撤回状态，不利用 CAS 正文绕过撤回。

ConversationState 与来源索引参与 Snapshot、Session Delta、Manifest 的摘要校验。
Manifest 为正文维护独立 CAS 引用，更新焦点复用正文；正文引用进入 ContentIDs，
由既有根/边回收机制保护。可选字段缺省时保持原编码；旧会话没有来源索引时继续用
已有 Findings/`turn_history` 恢复，不在读取时伪造完成来源或重写旧摘要。
