# Coding Benchmark 任务集

每个子目录是一个任务，由 `internal/host/bench` 驱动真实 Runtime 执行。整套用例**完全 hermetic**：不联网、不需要 API key、不调用真实模型，模型行为由 fixture provider 回放。因此结果可跨机器比较，可用作发布门禁（`make bench`）。

## 目录结构

```text
<task>/
  task.json          # 任务定义与断言
  workspace/         # 执行前拷入临时工作区的种子文件
  provider/          # provider fixture（与 testdata/providers/ 同格式）
    fixture.json
    *.sse
```

`workspace/` 会被复制到临时目录，执行完即删除；仓库内的种子文件不会被修改。

## 基线样本

`baseline-v1.json` 是 Report V1 的脱敏基线摘要。它保留任务清单、成功条件覆盖、任务结果、
Token/成本与验证覆盖等可比较字段，不保存完整 Prompt、Tool 参数、工作区路径或模型输出。

生成原始报告：

```bash
make bench BENCH_REPORT=/tmp/qcode-benchmark-report.json
```

Fixture Provider 会监听本机临时回环端口；受限执行环境需要允许 loopback listen。报告文件权限
由 Harness 设为 `0600`。刷新基线时至少运行两次，并仅在以下字段一致后更新摘要：

- 任务名、Category、Status、Terminal；
- 成功/失败工具名集合、Receipt Changes 和 Verification 结论；
- Usage Calls、输出/推理 Token、Cost、Unpriced Calls 与 Retry Attempts；
- 去掉 Latency 后的 Baseline Metrics。

输入、未缓存输入和缓存 Token 仍记录在摘要中，但不进入稳定投影哈希。Fixture 会把真实工具输出
回灌给模型，其中命令耗时等运行期文本可能造成极小的字节计数抖动；刷新时应比较并解释差异，
不能把它用作精确相等门禁。Context 的估算 Token 应在两次运行间保持一致。

`platform`、`generated_at`、任务 `duration_ms`、Latency Percentile、Window ID 及运行期
Route/Context Digest 不属于跨运行稳定字段，不得据此设置发布阈值。当前 Report V1 还不提供
实际 Turn 数和每种工具的调用次数；基线将其分别标记为 `unavailable` 和 `partial`，避免把
缺失数据当成零。

该样本记录的是当前行为而不是目标值。已知失败必须保留为失败，后续修复应在独立变更中更新
对应 Fixture/断言和基线，不能为了得到绿色报告而在生成阶段过滤。

## task.json 字段

| 字段 | 说明 |
| --- | --- |
| `name` | 任务名，缺省取目录名 |
| `category` | 能力维度，用于报告分组 |
| `note` | 任务意图；若断言锁定的是**当前行为**而非期望行为，必须在此说明 |
| `prompt` | 用户 prompt，需与 `fixture.json` 的 `expected_prompt` 一致 |
| `provider_fixture` | 可选；相对任务目录复用另一套 hermetic Provider Fixture |
| `tools` | 是否启用内置工具 |
| `posture` | 权限姿态（`suggest`/`auto`/`bypass`/`never`），缺省 `auto` |
| `approval_decision` | 自动回答停驻 Approval；仅接受 Protocol Decision（如 `approve`） |
| `budget_tokens` | Session Token Budget；用于验证首请求前的硬门禁 |
| `mode` | 固定为 `act`，省略时取默认值 |
| `max_steps` | Agent 步数上限 |
| `timeout_ms` | 单任务超时，缺省 2 分钟 |
| `verify` | 验证门禁配置，对应 `[execution.verify]`：`mode`/`scope`/`on_failure`/`command`/`max_repair_steps`/`timeout_ms`。门禁默认 `off`，任务必须显式开启 |
| `index` | 仓库符号索引配置，对应 `[context.index]`：`enabled`/`max_file_bytes`/`max_files`。索引默认**开**，任务只在要关掉它或压低上限时才写 |
| `context` | 上下文装配配置：`repo_map`（`enabled`/`max_bytes`/`max_directories`）、`working_set`（`enabled`/`max_entries`/`max_bytes`）、`evidence`（`enabled`/`max_entries`/`max_bytes`）三段易变尾块，与稳定前缀里的 `coding_policy`（`enabled`），分别对应 `[context.repo_map]` / `[context.working_set]` / `[context.evidence]` / `[context.coding_policy]`。四者默认**开**，任务只在要关掉某段或压小预算时才写 |

## 断言（`expect`）

| 字段 | 含义 |
| --- | --- |
| `terminal` | 终态：`completed` / `failed` / `canceled`，缺省 `completed` |
| `terminal_contains` | 失败消息必须包含的片段，用于确认"因正确的原因失败" |
| `files` | 路径 → 执行后必须完全相等的内容 |
| `unchanged` | 必须与种子逐字节一致的路径 |
| `absent` | 执行后必须不存在的路径 |
| `tools_used` | 必须成功执行过的工具 |
| `tools_failed` | 必须返回错误结果的工具 |
| `receipt_changes` | `turn.receipt` 必须报告的改动路径全集（校验收据与真实效果一致） |
| `output_contains` | 模型可见输出必须包含的片段 |
| `verify_status` | 最后一次门禁结论：`passed` / `failed` / `unavailable` / `not_evaluated` |
| `verify_action` | 门禁对结论的处置：`passed` / `repair` / `reported` / `failed` / `reverted` / `skipped` |
| `verify_repairs` | 门禁实际消耗的修复轮数，用来证明"确实修了"而不是"恰好通过" |
| `approvals` | 必须停驻并由 Harness 回答的 Approval 数量 |
| `approval_decision` | Harness 实际提交的 Approval Decision |
| `context_sections` | `turn.receipt` 必须报告的上下文分区（如 `repo_map` / `working_set_ledger`），用来证明尾块真的装配了而不是被静默跳过 |
| `context_truncated` | 必须报告被预算截断的分区 |
| `context_selections` | 路径 → 必须报告的 Kind、进入原因、Evidence Kind 与逐条截断状态 |
| `receipt_read_paths` | `turn.receipt` 必须报告的读取路径全集 |
| `receipt_evidence_kinds` | 收据的 `evidence.facts` 必须至少各出现一次的分类（`definition` / `reference` / `test` / `config` / `text_match`），用来证明命中被**分了类**而不只是被计了数 |
| `receipt_evidence_risks` | 收据的 `evidence.risks` 必须报告的风险类型（如 `changed_without_verification`） |
| `receipt_evidence_reminders` | 收据的 `evidence.reminders` 里必须出现的片段，用来锁定模型真正收到的那句提醒 |
| `receipt_not_collected_excludes` | 收据的 `not_collected` **不得**再包含的分区，随分区实现逐个划掉，保证覆盖清单不说谎 |

## 新增任务注意

- **诊断可复现性**：`.go` 文件的 post-edit 诊断默认调用 `gopls check`，结果依赖机器上是否装了 gopls。除非就是要测诊断，否则用不会触发诊断命令的扩展名（如 `.py`），保证 hermetic。
- **stream 数量**：fixture 按顺序为每次 provider 请求返回一个 stream，数量必须与 Agent 实际请求次数一致。
- **expected_request_fragments**：写上关键片段（暴露的工具名、tool_call_id、工具结果特征），这样工具暴露或回灌格式回归时会直接报错。
- **门禁任务的 hermetic 要求**：`scope = "repository"` 时显式写 `command`，不要依赖 `verify.Detect` 探测出的 `make verify` / `go test ./...`——那会把结果绑到机器上装了什么工具链。`grep`/`test` 这类 POSIX 命令足够表达"改对了没有"。
- **修复轮要先重读**：一次成功写入会作废该文件的读指纹，所以修复轮的 stream 必须先 `file_read` 再 `file_edit`，否则第二次编辑会以 `read-before-edit` 失败（见 `verify-gate-repair`）。`file_apply` 的一次调用内部不受此限（同一文件可连续改多次），但调用**之间**仍需重读。
- **符号工具的结果依赖索引就绪**：索引在第一次符号调用时惰性构建，写 `expected_request_fragments` 时只断言路径与 kind 这类稳定字段。注意工具结果内容在请求里是 JSON 字符串，内嵌引号会被转义，所以片段里不要写 `"kind":"function"` 这种带引号的形式。
- **`affected` scope 的 hermetic 写法**：配 `command` 并用 `{packages}` / `{paths}` 占位（见 `related-tests-affected`），不要指望真跑 `go test`——基准不能依赖机器上的工具链。
- **尾块片段怎么断言**：易变尾块以 `RoleSystem` 追加在 history 之后，段头是 `[repo_map turn=N index=<status>]` 与 `[working_set turn=N]`，可直接写进 `expected_request_fragments`。`turn` 从 1 开始；同一 turn 内每次采样都会重建尾块，所以「第一步读的文件出现在第二个请求的工作集里」是可断言的（见 `working-set-evolution`）。仓库地图每 turn 只取一次索引快照，因此**同 turn 内新读文件的符号轮廓不会立刻出现**。
- **证据尾块与 coding policy 怎么断言**：段头是 `[evidence turn=N]`，内部顺序固定为 `wasted effort:` → `unproved, and yours to close:` → `what lookups established:`（截断保前缀，最该被读到的排最前）。事实行形如 `auth/token.go:3 definition Verify (search_definition, turn 1)`，写片段时只取前半段更抗改动。coding policy 是**稳定前缀**，从第一个请求起就在，可用 `Coding method:` 断言。证据只在非空时才成段，所以第一个请求不会有 `evidence` 分区。
- **`file_patch` 不进基准**：它 shell out `git apply` 且要求强沙箱，macOS 沙箱会拦住 git 对临时文件的写入，任务只会超时。多文件事务的等价证明由 `edit-transaction-*` 三个任务承担——`file_apply` 不依赖 git，参数里也同样没有单一 `path` 字段。

## Benchmark V2

`testdata/contracts/benchmark-v2.json` 是六条用户旅程及其能力分层的权威清单：
Cross-file Edit、Test Selection、Crash Recovery、Approval、Budget 和 Host Replay。
`make benchmark-v2` 先校验清单与 Evidence，再运行 Fixture Benchmark、Workspace
Journal Recovery 和 Web Event Replay。需要 Strong Sandbox 的任务明确归入
`platform-capability`，不会在 Hermetic Lane 中静默伪装为绿色。
