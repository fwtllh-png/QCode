# AI Coding Agent 指南

本文给 AI Coding Agent 提供在本仓库可靠工作的最小上下文，也可作为人工 Review
检查表。

## 任务目标

保持 QCode 是“一套受治理的本地 Coding Agent Runtime + 单一 Web Host”。优先保证
正确性、证据、安全边界和可维护性，而不是能力数量。

## 开始工作

1. 阅读根目录 `README.md`。
2. 阅读[架构设计](./architecture.md)。
3. 修改实现前先读最近的 Package Test。
4. 从 `Makefile` 查找标准验证命令。
5. 检查 `git status`，不能覆盖无关用户改动。

## 所有权地图

| 变更领域 | 起始路径 |
| --- | --- |
| 跨层公共契约与基础工具 | `internal/common` 下的职责子包 |
| Web Process/Flag、HTTP/WebSocket Transport | `internal/host` |
| Runtime/Web 集成测试、Coding Benchmark | `internal/host/intergration_test`（仅 `_test.go`） |
| Host 共享视图 DTO | `internal/host/view.go` |
| Operation/Event Shape | `internal/runtime/protocol` |
| Turn/Session State | `internal/runtime/app` |
| Model/Tool Loop | `internal/runtime/agent` |
| Dependency Construction | `internal/runtime/app/wire` |
| Skill/MCP/Memory 接入 | `internal/runtime/app/wire`、`internal/adapter/skill`、`internal/adapter/mcp` |
| Model/Provider | `internal/adapter/model`、`internal/adapter/provider` |
| Tool | `internal/adapter/tool` |
| Approval/Sandbox | `internal/security`、`internal/adapter/tool/guard` |
| Subagent/Admission/Chat Merge | `internal/orchestration` |
| Durable Data | `internal/persist` |
| Usage/Trace/Receipt/Diagnostics | `internal/observability` |
| Web | `web` |

## 不可破坏的约束

- Host 不直接执行 Tool。
- `wire` 不实现业务循环。
- 公共契约和基础工具按职责放在 `internal/common` 子包，只依赖标准库、第三方库或
  其他 common 子包；不能反向导入业务层，也不因仅依赖标准库就迁入领域实现。
- Web 不建立第二套 Runtime。
- 不绕过 Guard、Policy、Constitution、Journal 或 Sandbox。
- Subagent 生命周期只写入 Agent Graph，不建立并行的后台任务生命周期。
- 日志、指标或 Trace 持久化失败不能改变 Turn 的业务结果。
- 受 Git 跟踪的 Config、Log、Fixture、Docs 中不保存原始凭证。
- 测试辅助类型和构造函数放在使用包的 `_test.go` 文件中，不为测试复用新增普通生产包。
- 不读取、打印、总结、Patch 或强制添加被忽略的
  `docs/DEEPSEEK-LIVE.zh-CN.md` 本机 Runbook。
- 有结构化 Parser 时，不用脆弱字符串逻辑解析结构化格式。
- 不静默接受未知 Config Field 或 Protocol Variant。
- 未实际执行验证时，不声称“验证通过”。
- 没有明确需求时，不为未发布开发状态增加兼容 Migration。
- 禁止在业务逻辑中私自引入没有契约来源的固定阈值、模型档位或经验常量来决定
  Context、Capacity、Latency 或资源策略。
- 不恢复架构行数、Fanout 或函数长度棘轮，也不为这类预算压缩代码。

### 阈值与容量规范

- 默认阈值必须从当前模型和 Provider 的权威 Capability、实际 Output Reserve、
  协议协商限制以及运行时观测值动态推导，不能按模型名称、是否为某类 Transport
  或经验档位硬编码。
- 用户显式配置可以覆盖动态默认值，但必须经过范围校验并保留 Provenance；不得用
  隐藏常量二次收紧用户配置。
- 必要的绝对安全上限必须定义为公开 Protocol 或 Config Contract，同时提供
  Validation、中文文档和边界测试。仅在实现文件中声明常量不构成合法来源。
- 性能优化的软预算与正确性保护的硬边界必须分离。模型可见上下文是有界工作集：
  Goal、约束、未验证变更、当前请求和最近因果链。这些 mandatory 事实由每轮
  `session_state` 分区从 Ledger 投影，不依赖 compact 事件。`post_turn` Narrative
  只在 `turn.completed` 之后写可选 Digest 分区；用户暂停不得再调 summary 或
  发出 fallback Compaction 卡片。对话投影也不把 `post_turn` Narrative fallback
  显示成压缩失败。`semantic_narrative_max_output_tokens = 0` 从 summary 模型
  声明的 `MaxOutputTokens` 与剩余窗口推导，正值才是 Operator Ceiling。Timeout 不得挡住下一轮 Sample；`route.summary` 的瞬时
  429/5xx 走与主采样相同的 RetryPolicy，硬配额立即 fallback。完整 transcript 保留在
  Durable Journal，不作为模型上下文。TTFT、成本或缓存优化不得丢掉 mandatory
  用户语义，也不得用隐藏百分比替代公开的 `context.view.recent_tail_turns` 与
  剩余硬输入 residual 契约。`context.view.history_token_ceiling=0` 表示
  Mandatory 分区之后的剩余容量，不是窗口百分比。`recent_tail_turns=0` 表示
  容量选择，也是默认值；正值是包含当前轮的显式上限。默认
  `digest=ledger+narrative`，与 `narrative_mode=post_turn` 同时启用才自动生成摘要；
  `off` 可继续复用有效缓存。摘要输入、条数和单条字节数的零值不能被替换为隐藏上限。
  原文选择和遗漏指引必须使用同一 `ProjectionResult`；最终 Normalize 后计入
  Schema、动态内容、续写、提示和输出预留再准入。只有完整成本净下降才接受
  折叠；短轮不足以抵消提示开销时检查后续安全边界，找不到则回滚。
  Tool Result 首次准入后，未超硬输入时不再改写；超硬输入时采样路径可把已
  发送结果和调用参数收成 Handle / 身份投影，不得用隐藏 N 代替 ResultStore
  合同或 `result_get`。伴随已闭合工具调用的分析正文默认保留；只有超硬输入
  时才收成带来源的非权威摘要，不能仅因为文字和工具在同一条消息就丢掉判断。
  进行中的 Turn 不得因窗口失败，除非用户请求加
  Mandatory 分区已超过硬输入。闭合 Turn 的 Checkpoint 持久化仍 write-once，
  采样仅选当前来源引用轮和最近闭合轮，放在 Dynamic。Dynamic 与历史引用、遗漏
  指引都插入当前 Turn 首条用户请求之前；资料不变时保留工具循环前缀，选择或
  容量改变时重新投影，不写入 Durable History。预算与续写反馈仍在末尾。
  模型静默使用内部背景，不确认接收或播报注入过程；`checkpoint_max_bytes`
  约束可选块总投影，0 用必要上下文之后的请求余量。摘要不得自动提升执行 Todo。
  历史恢复走 `turn_history` 的 turn/source_id/item_id/catalog 互斥选择器，
  source_id 可读 index_only；max_bytes 有界，继续源分页用 offset/content_digest。
  `result_get` 只恢复已存工具页，不代替读取源分页遗漏。完整归档优先于内存残片。
  未绑定定义过大时先目录恢复；实际执行入口只允许恢复、绑定与澄清，
  同批次绑定不能提前执行业务工具。下一次采样装入必要定义后再继续。
  被投影裁掉的旧 Turn 必须由本次
  选择生成临时 `[context_selection]`，给出实际原因与合法 `turn_history {"turn":N}`
  参数，不写入冻结 World。稀疏轮号不虚构区间，提示缩短时声明未展示组数；
  缺失 Checkpoint 只回封
  turn id，不把旧审计猜进 Plan。最近完成轮的用户可见终答与工具定位位点必须
  进入 mandatory Continuity 胶囊，不能只留在被裁掉的原文 Tail 里。Plan 已有完成步骤或 Working Set 已有已读路径时，`session_state`
  必须带 Resume Fact：不要重复已完成步骤，下一项未完成工作取第一项
  outstanding Plan 标题，并列出全部已读路径。Prompt 工作集仍按
  `context.working_set.max_entries` 取 top-N。已读列表超过 `session_state`
  分区预算时截断并写 `(N more already-read paths omitted)`。
  有行号命中时 Continuity / Resume 还列出 `Located sites`。已有信息覆盖当前
  请求时直接复用；缺少必要定义或证据时，按遗漏指针或未覆盖窗口恢复。
  `working_set` 只列路径；
  不要再次 `file_read`，除非即将编辑具体窗口。`search_text` /
  `search_definition` 命中后优先读该窗口。
  预期命中面很广的 `search_text` / `search_project` 先用 `output=files` 或
  `output=count` 拿文件级汇总（省结果 token），再收窄查询或回到
  `output=content` 取行级命中；两档汇总仍向 Evidence 提供 `Located sites`。
  已知缺陷用 `search_text` / `search_definition` 定位。单文件 `path` 仍按公开
  walk 字节上限搜索；空命中带 `skipped.large` 不表示符号不存在。已有行号命中
  后只读将编辑的窗口并立刻改，不要整文件翻页。取消或失败且未改文件的 Turn
  已记在 Checkpoint 里，不要用 `git_diff` 再确认。
  脏的 `git_status` / `git_diff` 不是重读理由。覆盖范围内的已知读回放原结果；
  无法回放或先前正文已不在当前 Sample 时放行必要重读。取消 Checkpoint 保留下一项
  Plan 与全部已读路径指针，超 Checkpoint 预算时写 omitted。Paused Continue 恢复 Work Item（当前用户句为 Goal，源
  Turn KnownReads 开局写入）；覆盖读回放，git 巡视放行。相邻 Sample 重复同一
  工作状态（内容版本 + 结果语义 digest）且同一工具身份达到
  `execution.implement_no_progress_samples`（默认 6）进入 Finish-only，但此时
  不再收窄工具目录。新窗口、新内容版本或新结构化结果 digest 同时续期；A/B 循环只走
  `max_steps` 长租约。结果语义摘要只使用结构化的文件版本、命中位置、诊断代码/位置、
  验证状态与输入摘要、失败类别和进程状态；正文、Admission 内容摘要、诊断自由文本、
  Handle、进程 ID 与输出游标不续期。未知输出保守处理，原始结果仍完整保留。
- 模型窗口、经济预算和 Provider Throughput 是三个独立容量平面。Operator 通过
  `execution.tokens_per_minute` 声明 TPM；`0` 表示未知，不发明按模型名称的默认值。
  合法工作集超过已知 Burst 或等待将超过预算时，先寻找包含遗漏提示成本的
  Visible Tail 净缩减再重新准入；仍超则延迟或拒绝，不得静默重探同一 Digest，也不得因此改写 Durable
  History。
- 动态策略必须覆盖不同 Context Window、Output Reserve、Provider Projection Mode
  和缓存状态的参数化测试，禁止只针对某个模型规格编写固定期望值测试。

## 工作方法

### 探索

使用 `rg` 和定向读取，确认：

- 所属 Package；
- 当前测试；
- Public/Persisted Contract；
- 跨平台文件；
- 生成文件；
- 用户已修改文件。

### 计划

明确行为变化、不变量、文件和验证。变更应留在既有所有权边界内。

### 实现

- 遵循本地模式；
- 做最小且完整的变更；
- 使用领域语义名称；
- 只为不明显约束添加注释；
- 中文文档与代码事实同步更新；
- 使用仓库命令重新生成 Artifact。
- 不要为行数、Fanout 或函数长度预算牺牲可读性；这类棘轮已删除，不要恢复。

### 验证

先运行最窄测试，再按影响面扩大：

```bash
go test ./path/to/package
make docs-check
cd web && npm run check && npm test -- relevant-area
```

结束前运行 `git diff --check`。按影响面扩大到 Race、Playwright 或 `make verify`。

## 契约变更

### Protocol

修改 `internal/runtime/protocol` 时：

1. 更新 Validation 与 Test；
2. 重新生成 JSON Schema；
3. 重新生成 Web Protocol Type；
4. 运行 Web Transport 与 HTTP Contract Test；
5. 更新中文文档。

### Persistence

修改 Durable Schema 时：

1. 明确兼容预期；
2. 保证初始化就是最新 Schema；
3. 公开发布后使用事务 Migration 与 Migration Test；
4. 验证 Foreign Key、Index、Rollback 与 Corruption Handling；
5. 更新运维文档。

### Security

修改 Guard、Policy、Permission、Constitution、Sandbox、Egress、Credential 或 MCP
Trust 时：

1. 枚举攻击者可控输入；
2. 保持 Fail-closed；
3. 除成功路径外，测试拒绝与清理；
4. 运行聚焦 Race/Security Test；
5. 不弱化平台能力声明。
6. 环境契约实现对照
   [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)，
   不要在核心增加语言变量白名单或语言名称分支，也不要建立第二套授权模型。
   产品默认是 `v1` + `native`；不要静默打开 `shared_user_temp`、凭证目录或
   任意出网。子 Agent 保持 isolated。
   新工具走 `ResourceRequest` 声明。准备器不维护语言发现器；Git 集成负责生成自己的
   配置声明，由 wire 组合。准备后使用策略中的环境值，不在命令启动时读取宿主环境。
   环境安全规则复用 `security/envpolicy`，普通变量按来源、可信声明、命令声明覆盖，
   同层冲突报错，HOME/临时目录/受管代理继续由策略决定。
   默认准备链不探测语言工具或自动绑定宿主认证；语言工具的配置和缓存使用可信资源
   声明。内置 GOPROXY 服务与 `auth_services` 已删除，不在 Runtime/Guard/Shell
   中增加语言认证分支。`credential/use` 无实际绑定器时必须明确未绑定。
   准备事实经不可变快照传递，并与当前执行失败分类分开；子环境使用自己的事实。
   详见[执行环境通用化优化方案](./environment-language-neutral-plan.md)。
   进程失败类别只接受权威组件事实，禁止从 stderr 扫描 `401` /
   `permission denied` 改判。运行中新主机必须先批准再连接；同一缺失不要
   再生成一遍审批，也不要重放已有副作用的安装/构建命令。

### Subagent

修改 Subagent Lifecycle、Admission、Budget 或 Worktree 时：

1. 保持 Agent Graph 是子 Agent 生命周期的唯一事实来源；
2. Child 执行继续提交普通 Runtime Turn，不创建后台 Task 或 WorkGraph 镜像；
3. Budget、Depth、Concurrency 和 Workspace Authority 只能相对 Parent 收窄；
4. Host View 保持只读，所有控制动作经 Runtime；
5. 测试 Restart、Duplicate Settlement、Budget Fence、Cancel 与 Worktree Cleanup。

### Observability

修改 Trace、Usage、Receipt、Diagnostics 或 Metrics 时：

1. 保持 Terminal Envelope 是终态 Measurement 的唯一事实来源；
2. Trace 只记录有界 Span 与低基数 Attribute；
3. Usage 保留 Provider、Model 和 Pricing Provenance；
4. 日志、指标与 Trace 写入失败不能改写业务结果；
5. 敏感诊断材料继续经过现有日志和 Provider Dump 脱敏边界。

## 测试预期

| 风险 | 预期测试 |
| --- | --- |
| 局部逻辑 | Unit Test |
| 共享组件 | Unit + Integration Consumer |
| Protocol | Golden/Schema + Transport Contract |
| Observability | Trace/Usage/Receipt Contract + Failure Isolation Test |
| Persistence | Create/Read/Update + Failure/Reopen |
| Concurrency | 确定性同步 + Race |
| Security | Allow、Deny、Malformed Input、Cleanup |
| UI | State Projection + Message Validation |
| Script | Happy Path、Invalid Input、Exit Status |

仓库已有 Target 或 Fixture Framework 时，不创建脱离框架的私有测试脚本。
纯测试用途的场景、驱动和包内辅助代码必须放在 `_test.go` 中，不放入生产 Go 文件。
确需跨包复用的测试辅助代码集中在根目录 `testutil`；为支持 Go 跨包导入使用
普通 `.go` 文件，仅允许测试引用，生产代码禁止导入。
`internal/host` 的实现、同名测试、视图和协议文件全部平铺，统一使用 `host` 包。
该目录仅保留 `intergration_test` 一个子目录，集中 Runtime/Web 共享行为测试与
Coding Benchmark；全部使用 `_test.go`，类型和辅助函数保持私有，
不作为其他包可导入的测试框架。

## 文档预期

文档属于功能的一部分：

- 描述当前行为，不描述开发编年史；
- 区分已交付能力与 Roadmap；
- 示例命令必须存在于 `--help`；
- 受 Git 跟踪的文档不嵌入真实凭证；
- 保持本地链接有效；
- 更新 `docs/zh-CN`，不创建英文镜像；
- 删除过时材料，不保留互相矛盾的副本。

在 PR Documentation Impact 区块列出受影响的中文文档路径，并运行
`make docs-check`；不再要求已删除书籍的 Catalog、章节元数据或导航。

## 完成报告

说明：

- 修改内容；
- 关键文件或子系统；
- 执行的测试；
- 环境失败或跳过测试；
- 兼容或 Migration 影响；
- 未纳入任务的剩余 Untracked File。


### 稳定引用报告与计划

模型输入中的 `conversation_references` 保存早先报告的原始编号和来源条目 ID。
继续处理某项时使用已经随请求提供的定义；多个报告有同一编号时先根据用户上下文
确定来源，不能自行选最近报告。无需因原文轮次被裁剪而重复读取已覆盖的定义。

执行计划更新时复用 `steps[].id`，用 `reference_item_ids` 关联报告项；新增步骤
省略 ID 后使用工具返回值继续更新。只改变材料焦点时，向 `update_plan` 传
`context_selection`，可省略 `steps`；空对象清空焦点，不会完成或删除既有计划。
替代来源必须依据用户纠正提交 `replacements`，并同步重绑尚未完成的计划引用。
报告、摘要及交付方案本身不构成执行授权，完成与验证继续依据 Plan 和 Evidence。
