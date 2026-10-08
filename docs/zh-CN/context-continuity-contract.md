**上下文连续性契约与阶段回归**

本文是[上下文连续性与压缩优化方案](./context-continuity-optimization-plan.md)的验收契约。
C1–C12 的编号在后续阶段保持稳定。P0 交付契约、中文 Fixture 和可重复的缺陷基线；
P1 交付统一选择与最终准入，P2 交付稳定来源、引用选择和 Plan 关联，P3 交付完整范围摘要及异步安装，P4 交付压力恢复与生命周期保证。
P5 已实现默认切换、观测、Web 诊断及固定会话对照；真实模型评测尚待测试连接和预算，不能把脚本回归等同于线上效果验收。
当前配置语义以[配置说明](./configuration.md)为准。

**P5：观测、默认切换与本地验收（2026-10-08）**

`recent_tail_turns` 默认从 2 改为 0，`digest` 从 `ledger` 改为 `ledger+narrative`；
摘要输入 token、条数及单条字节上限默认均为 0。配置层、Engine 和摘要校验器不再补回
4096 tokens、32 条、512 字节的隐含额度。`ledger+narrative + post_turn` 自动调度；
`ledger+narrative + off` 复用仍有效的缓存；`ledger` 不自动生成或使用语义表示。
人工 `thread.compact` 的来源按实际替换前后差集捕获，不再依赖已经关闭的轮数上限。

每个 Provider attempt 在调用前持久化无正文选择目录，包含来源历史 digest、实际选中消息、
精确遗漏及读取轮号、问题 source/item ID、UTF-8 字节范围、表示与覆盖状态，以及最终投影
digest。终态回执保存最后一次选择和计量；即使 Provider 没返回 usage，也保留请求诊断。
遗漏区分轮数、容量、显式 ceiling、重复表示、来源失效/替代及本次未选中的来源。
摘要按整个已验证候选准入；候选包含多个来源且部分与原文/摘录重叠时，整体省略，
不截断自然语言条目，也不删除必要原文。无法核对来源的缓存省略并记录原因。

前台恢复统计按已完成的 `turn_history` / `result_get` 调用累计，字节数为成功返回的
UTF-8 工具表面（含元数据），失败调用单独计数。它不推断调用是否必要。后台摘要以同一
作业 ID 报告 started/prepared/completed/fallback；损坏或覆盖失败在生成结束时即可观察，
等待安全边界不会被标成失败，迟到安装仍归属原 Turn。作业耗时包括生成排队与重试；
`narrative_foreground_wait_ms` 表示等待可选模型生成的时间，当前路径为 0，安装事务耗时
不冒充该指标。诊断费用不参与二次结算；每次真实调用的 usage 仍独立保留，含失败尝试。

Web 在运行统计中提供简单状态和 `Context selection details`，展开后查看来源、范围、
预算与后台摘要事件。Host/Web 只投影 Runtime 数据；晚到维护结果不改写任务结果、
验证状态或原 Turn 用量。完整来源目录属于会话诊断，不作为高基数监控标签，不存原文正文。

固定中文四轮回放使用同一脚本 Provider、131072-token 模型能力和 128-token 输出预留。
参考组是当前代码上的显式两轮原文上限，不是重新运行旧二进制；两组都包含 P2–P4 的来源保证。
检查直接针对实际 ModelRequest 和已冻结 Route，结果见
[P5 回放原始记录](./context-continuity-p5-replay.json)。

| 任务指标 | 参考配置：两轮原文上限 | P5 默认：容量选择 |
| --- | --- | --- |
| 所需定义保留 | 3/3 | 3/3 |
| 原始问题编号保留 | 3/3 | 3/3 |
| 上一轮进度保留 | 3/3 | 3/3 |
| 来源 ID | 四个来源保持一致 | 四个来源保持一致 |
| Provider 请求 | 4 | 4 |
| 前台恢复 / 文件读取 | 0 / 0 | 0 / 0 |
| 后台摘要作业 | 0 | 0 |
| 累计估算输入 tokens | 6416 | 6353 |
| 累计估算输出 tokens | 121 | 121 |
| 非单调前缀次数 | 2 | 2 |
| 真实费用 / 语义忠实度 | 未测量 | 未测量 |

短会话的新默认减少了原文遗漏指引开销；该结果不构成所有长任务都会省 tokens 的结论。
记录中的耗时仅是本地脚本执行时间；非单调前缀不等同于 Provider 实际缓存失效。
不从缺失的 Provider 用量生成零费用结论。真实模型的首响应、缓存、语义质量及总成本
仍待提供测试连接、模型和费用或 token 上限后，以相同会话和配置进行有预算的对照。

可重复运行：

```bash
make context-continuity-replay
QCODE_CONTEXT_REPLAY_REPORT=/tmp/qcode-context-replay.json make context-continuity-replay
```

该回放已加入 Benchmark V2 的 `context_continuity` 旅程。零值与正值边界、四种开关组合、
关闭生成时的缓存复用、过期/重复/超容量省略、无 usage 回执、UTF-8 恢复计数、
迟到安装与失败事件、前台不等待均有专门回归。

**P5 验证记录（2026-10-08）**

| 检查 | 结果 |
| --- | --- |
| `config`、`context`、`contextview`、`prompt`、`protocol`、`scripts/benchmarkv2` 全包测试 | 通过 |
| `engine`、`app` 全包测试 | 通过；最终一轮分别约 105 秒、29 秒 |
| P5 专项及固定会话对照 `-race` | 通过 |
| 上下文、摘要、来源、Fork、Continuation、撤回的 Engine 聚焦 `-race` | 通过；约 6 秒 |
| `app`、`persist/contextstate` 全包 `-race` | 通过 |
| Engine 全包 `-race` | 达到 Go 默认 10 分钟总时限；不能记为通过。超时时正在运行 `TestReadOnlyFinishOnlyCompletesCurrentProcess`，该用例随后单独 `-race -count=1` 通过，约 139 秒 |
| Web TypeScript 检查与测试 | 通过；29 个文件、388 项测试 |
| `make protocol-schema`、`make web-protocol-check`、`make capacity-policy-check` | 通过；协议生成文件已同步 |
| `make docs-check` | 通过；包含 11 项 Benchmark V2 旅程清单校验和 10 项脚本测试 |
| `make context-continuity-replay`、`git diff --check` | 通过；原始对照记录已保存 |
| 真实模型的语义质量、首响应、缓存与费用对照 | 未执行；仍缺测试连接、模型及费用或 token 预算 |

本地回归支持 P5 的实现与默认切换；完整 Engine 竞态检查和真实模型效果评测的限制
如上保留，不将部分通过汇总成全部验收通过。

**契约适用边界**

契约覆盖每次实际 `ModelRequest`，包括普通采样、重试、压缩后续写及摘要请求，
也覆盖模型视图所依赖的来源保存、恢复和后台维护。普通采样的任务接续与摘要输入的
来源覆盖分别检查，不能把“摘要请求符合 JSON Schema”当成“主任务上下文完整”。

“当前依赖”指解释本次请求所必需的定义、有效约束及控制状态；完整 Transcript
不自动成为当前依赖。引用有歧义时必须保留候选并澄清，不把同编号的另一份报告当作来源。
容量不足时可以按来源分段读取并保留阶段性结论；不可再缩的请求与控制状态仍超限时，
必须返回明确的容量原因。不得在缺少当前问题定义时直接开始修改代码。

| 编号 | 必须满足的行为与判定边界 | 主要验收阶段 |
| --- | --- | --- |
| C1 | 实际请求保留当前用户请求、有效用户约束、未决审批/输入及不可中断的 Provider 续写状态；无法容纳时显式失败，不能静默丢弃 | P1、P4 |
| C2 | 正在处理的引用有可解释的定义与有效来源；仅有“问题 2”、轮号或 Handle 不算完整。来源已撤回、缺失或有歧义时，不得猜测定义 | P1、P2、P4 |
| C3 | 来源和条目身份由持久来源确定；列表重排、重复压缩、裁剪前缀或切换模型均不改变同一条目的身份；不同来源的相同文本不混同 | P2、P4 |
| C4 | 进度关联具体条目；普通完成消息不能覆盖仍相关的原始报告。完成一项不改变其他项的定义和状态，用户纠正通过明确替代关系生效 | P2、P4 |
| C5 | 淘汰仍有效的必要语义前，已选替代表示必须可用且覆盖所需来源；摘要排队、失败、部分截取或校验通过均不能单独证明完整替代 | P1、P3、P4 |
| C6 | 摘要属于有来源的解释；不能生成测试通过、文件已修改、审批已通过等权威事实。完成与验证状态分别来自既有 Plan、执行和验证证据 | P2、P3 |
| C7 | 可见来源、遗漏原因及恢复指针来自同一次选择；遗漏集合与实际来源差集一致，读取参数符合工具 Schema，不能用轮数重新猜测 token 裁剪结果 | P1、P4 |
| C8 | 每次实际请求均按冻结 Route 准入；工具定义、协议封装、续写状态和输出预留完整计入。缓存/增量传输不能减少逻辑窗口用量，显式 ceiling 不能被无声突破 | P1、P3、P4 |
| C9 | 选择和缩减保持工具调用配对，不引入孤立调用或结果；逻辑历史变化后，Provider 增量状态必须重建或失效，不能隐含保留已删除前缀 | P1、P4 |
| C10 | 可选后台摘要不阻塞下一轮采样，不改写正在采样的快照，不覆盖后续用户纠正或进度。迟到候选只在来源、权限和版本有效时安装 | P3、P4 |
| C11 | 来源、选择与用量可回放；序列化/恢复保持身份。撤回、Revert、Fork、Subagent 与 CAS 回收保持同一来源权限和存活规则，已撤回内容不能由摘要重新注入 | P2、P4、P5 |
| C12 | 资源决策有公开配置、Provider/模型能力或运行时观测来源；不得引入隐藏比例、语言关键词权重或模型档位。绝对安全上限具备公开字段、来源、校验、文档及边界测试 | P1–P5 |

**P0 可执行基线**

保留的缺陷现状断言使用 `TestContextContinuityP0Baseline` 前缀。
其 `PASS` 仅表示复现条件和观察值仍成立。P1 已升级 B3，并增加 B1 的容量模式
正向回放；P2 将 B1 的正值轮数上限对照也升级为正向回归。P3 将 B2 升级为完整范围正向回归，
不以基线通过宣称全部契约达标。
测试不 `Skip` 已知缺陷，也不依赖真实模型、网络、访问凭证或模型随机回答。

| 基线 | 入口 | 当前断言 | 必须升级为的目标 |
| --- | --- | --- | --- |
| B1：有容量仍丢引用（P2 已升级） | [engine 回归](../../internal/runtime/agent/engine/context_continuity_baseline_test.go)中的 `TestContextContinuityP2FollowupWithoutPressure` | 原文轮数仍为 2，前三轮 `post_turn` 与四轮 `off` 每次实际 Provider 请求均包含所引用问题的完整定义；无额外模型调用或容量压缩。最近进度不覆盖第一轮报告 | 已验证 C2/C4/C5 的普通短会话接续；同轮焦点、歧义、计划重排和来源恢复见 P2 测试 |
| B2：摘要前丢报告尾部 | [context 基线](../../internal/runtime/agent/context/context_continuity_baseline_test.go)中的 `TestContextContinuityP3NarrativeRangeCoverage` | P3 按结构与 UTF-8 范围分块，重建完整原文字节；第 2、3 项进入实际模型请求，多块共享同一总时限 | C2/C5：按结构分块并记录输入范围、覆盖项及遗漏范围；需要的尾部项进入可校验的替代表示，不能用截断前缀宣称完整摘要 |
| B3：token 裁剪遗漏提示（P1 已升级） | [engine 回归](../../internal/runtime/agent/engine/context_continuity_baseline_test.go)中的 `TestContextContinuityP1TokenCeilingOmission` | 无 ceiling 时两轮均可见；设定 ceiling 后仅当前轮可见、原历史不变；遗漏来源精确指向第 1 轮，原因 `history_token_ceiling`，参数 `turn=1` | 已验证普通历史投影的 C7；实际 Provider 输入由 `TestContextSelectionTokenOmissionReachesActualProviderRequest` 再次检查 |

既有旧会话测试已改名为
`TestContextContinuityP0BaselineOmittedTurnHintFromOldSession`，位于
[session_state_test.go](../../internal/runtime/agent/engine/session_state_test.go)。
它仍记录旧策略，并保留回封检查点和恢复提示检查；错误消息不再把保留真实来源正文称为
“捏造或泄漏”。后续应区分“有来源且仍被引用时保留”与“无来源或已撤回时不能当作事实”，
不能通过删除恢复和来源检查来让测试通过。

**Fixture 与测量规则**

[中文多问题会话](../../internal/runtime/agent/engine/testdata/context_continuity_zh.json)
保存完整问题报告、三次逐项请求、固定回答、每轮引用的定义和来源轮号。
Fixture 格式 2 移除 P0 的缺失定义观察值，所有非空 `required_definition` 都必须可见。
`source_turn` 只表示 Fixture 中的出处；稳定 item ID 由实际终答捕获生成，另有 P2 测试
校验身份、范围与恢复。

`post_turn` 回放前三轮，在第三轮请求处取证；`off` 回放四轮。每个需要接续的完成轮
都通过既有闭合处理保存 Findings，即使可选摘要生成关闭也执行闭合。Fixture 的
“已处理”回答仅驱动后续对话，不代表真实修改、测试通过或计划状态；不能用它评价模型
是否会正确修复问题或是否会重复执行已完成项。

窗口 131,072 tokens、输出预留 128 tokens、原文轮数 2 是显式测试输入，沿用已确认复现条件，
不是新的模型档位或建议默认值。B1 记录实际请求、未投影历史加固定提示的估算及硬输入上限，
断言估算低于上限且压缩事件为零；不把精确 token 估算数字写成 Golden，也不代替 C8 的完整计量验收。
B3 的 256 tokens ceiling 和长消息同样只是隔离 token 裁剪路径的测试输入。

[中文长报告](../../internal/runtime/agent/context/testdata/context_continuity_long_report_zh.txt)
将第 2、3 项放在原先 1,024 字节逐消息上限之后。P3 的 B2 验证每段 UTF-8 范围连续，
可重建完整原文；生成回归进一步检查每次实际请求的 token ceiling 与相同总 deadline。

**P1 已实施结果与边界**

- `TestContextContinuityP1CapacityFollowupWithoutPressure` 复用 B1 同一中文会话、
  模型窗口和输出预留，显式设置 `recent_tail_turns=0`。`post_turn` 前三轮与
  `off` 四轮中，每次首次 Provider 请求都含对应问题原文定义且没有遗漏提示。
  默认仍为 2；P2 已完成正值上限下的来源引用，P5 负责默认切换。
- [选择测试](../../internal/runtime/agent/contextview/selection_test.go)检查原文差集、
  多种遗漏原因、源历史不变、确定性、元数据 JSON 往返、容量扩展单调性与跨轮工具配对。
  `ProjectionSource.Index` 只在 `SourceHistoryDigest` 内有效，不充当持久问题 ID。
- [提示测试](../../internal/runtime/agent/prompt/context_selection_test.go)检查稀疏轮号、
  合法 `turn_history {"turn":N}` 参数、预算内聚合、未展示组数和最小提示超限拒绝。
  提示不写入冻结 World，每次裁剪后重建；缩短提示不删除完整遗漏元数据。
- [最终准入测试](../../internal/runtime/agent/engine/context_selection_test.go)比较实际
  Provider 消息、工具 Schema 与最终规范化 Snapshot，检查完整输入与输出预留。
  覆盖不同输出预留、大 Schema、规范化后成本增长、续写保留、恰好达到显式 ceiling、
  operator/经济原因区分，以及移除短轮不足以抵消提示时继续找净缩减或回滚。
- 配置测试覆盖文件/env 显式 0 及 provenance、`-1/0/1/128/129` 边界；
  零值由配置层解释，Engine 不再把它重新置为默认 2。

`SampleContextData.context_projection_digest` 绑定最终选择、源历史、冻结窗口、
Route、Context digest、输入成本和输出预留。当前仍使用既有 TokenEstimator、
封装估算和 Provider 观测校准，未实现精确 wire tokenizer。普通原文的遗漏与
规范化中的孤立工具块/图片等变化分别由 ProjectionResult、NormalizationReceipt 记录。
P2 已将稳定来源索引、正文和选择纳入 Context Snapshot/Manifest；来源目录及有界查询见下文 P4；完整采样投影历史的观测仍按 P5 范围推进。P1 本身不修改持久化格式。摘要请求覆盖与后台不等待已由 P3
实现；长期 Checkpoint 总量控制见下文 P4。

**P1 验证记录（2026-10-08）**

| 检查 | 结果 |
| --- | --- |
| `context`、`contextview`、`prompt`、`engine`、`config`、`turnhistory` 包测试 | 通过；Engine 全包约 133 秒 |
| P1 选择/连续性、容量门禁、吞吐/溢出恢复、World 提示等聚焦竞态回归 | 通过；Engine 聚焦组约 144 秒，另补 Provider 溢出后即时提示重建测试通过 |
| `app`、`app/eventhub`、`app/persistence`、`app/workspacequery`、`persist/contextstate`、`protocol` 竞态检查 | 通过 |
| 协议生成、`make web-protocol-check`、Web TypeScript 检查 | 通过；协议漂移检查首次遇到 Go 缓存沙箱权限限制，获准重试后通过 |
| `make docs-check`、`git diff --check` | 通过 |
| Engine 全量 `-race` | 达到默认 10 分钟总时限；超时时正在运行的 `TestReadOnlyFinishOnlyCompletesCurrentProcess` 随后在聚焦竞态组通过，不能将全量竞态记为通过 |
| `app/wire` 扩展集成检查 | `TestChildAgentRunsRealEngineTurn` 失败，普通模式单独复现相同结果：测试期望 `not_evaluated`，当前工作树已有验证逻辑返回 `not_required`。该验证状态逻辑属于 P1 之外的未提交改动，本阶段未修改该断言 |

这些结果证明上述 P1 范围，不代表整个工作树或后续阶段已通过验收。测试使用脚本
Provider，未进行真实模型的效果与费用评测。

**P2 已实施结果与边界**

- [来源索引测试](../../internal/runtime/agent/context/conversation_test.go)覆盖原始有序编号、
  嵌套列表、标题/Setext、多组列表、空条目、首项代码围栏、空围栏与围栏内数字，
  直接比较原文字节范围；正文保持完整，不按摘要字数上限截尾。
- 来源组绑定 Thread/Turn/终答消息槽/正文摘要，条目绑定来源组与字节范围。
  相同标题、相同文本但不同轮次或线程不会合并。无效外部 ID 无法读取其他会话。
  无绑定时保留有效结构化报告及最近终答，多报告相同编号不自动选最近一个。
- [Engine 接续测试](../../internal/runtime/agent/engine/conversation_test.go)实际执行
  `update_plan.context_selection`，验证同轮下一次 Provider 请求立即改变引用范围；
  只切换焦点不创建 Plan 或 PlanDelta。运行时记录用户请求出处。
- Plan 的稳定 ID 与 `reference_item_ids` 贯通工具、Engine、提示和持久化；
  同名步骤互不合并，改名重排仍关联原定义，PlanTruth 使用同一稳定身份。
  无效引用整次拒绝，工具回调失败不污染本地进度。摘要不再自动追加执行步骤。
- Snapshot、Delta、Manifest 往返保留来源与选择。正文独立存 CAS，选择更新
  复用正文引用，所有引用进入 ContentIDs；正文缺失或范围损坏时拒绝恢复。
  可选字段缺省不改变已有编码，无预发布迁移框架。
- 恢复、Fork 与[撤回基线测试](../../internal/runtime/agent/engine/turn_withdrawal_test.go)
  检查来源保留/回退；活动步骤不能引用被替代来源，已完成步骤允许保留历史出处。
- [依赖选择测试](../../internal/runtime/agent/contextview/conversation_test.go)检查原文
  已可见时只补元数据，以及子项保留父项前提。ProjectionResult 记录精确覆盖范围和
  表示类型，参与最终 projection digest；普通原文 ceiling 不会抹掉引用定义。
- 清空焦点后，`turn_history` 仍返回该轮的稳定来源/条目索引，支持重新绑定。
  仅剩来源正文时明确说明完整 Transcript 不可用；来源回读继续检查撤回状态。
- [会话身份回归](../../internal/runtime/agent/engine/conversation_session_test.go)覆盖
  进程默认 ID 与持久会话 ID 不同的连续两轮、换进程恢复、来源原文与稳定 ID 保留，
  以及跨会话、缺失归属、撤回和工具回调身份冲突时的拦截。
  [Runtime 集成回归](../../internal/runtime/app/thread_manager_test.go)中的
  `TestThreadManagerConversationSourcesUseDurableSession` 验证真实会话绑定经过
  ThreadManager 和 EngineAdapter 后，两轮均产生完成来源。
  [摘要身份回归](../../internal/runtime/agent/engine/narrative_session_test.go)验证
  后台候选保存真实会话身份，安装前来源转属、删除或撤回时不会替换当前表示。
- 容量测试使用显式 4,096-token 窗口：整组必要来源过大时在 Provider 调用前返回
  `resource_exhausted`，明确选择可装入的尾项后完整提供该定义。没有用截断正文
  或越过总预算让测试通过。

P2 不自动重建旧会话缺失的完成来源；原有 Findings/`turn_history` 继续可用。
P2 当时的持久化保证以已提交 Context Snapshot 为边界；候选目录、按项恢复、
长期压力、Subagent 继承和未闭合轮次崩溃后的焦点/计划恢复，现由下文 P4 补齐。
P3 的摘要分块和异步安装结果见下文；P2 交付时 `recent_tail_turns` 默认仍为 2，
P5 已切换为容量选择。

**P2 验证记录（2026-10-08）**

| 检查 | 结果 |
| --- | --- |
| `context`、`contextview`、`prompt`、`interact`、`engine` 全包测试 | 通过；最终一轮 Engine 全包约 107 秒；后续原文字节保留调整由聚焦竞态回归验证 |
| P2 来源、选择、计划身份、中文接续、撤回等聚焦 `-race` | 通过 |
| `app`、`app/persistence`、`persist/contextstate`、`turnkernel`、`protocol` 全包测试 | 通过 |
| 协议生成、`make web-protocol-check`、Web TypeScript 检查 | 通过 |
| `make docs-check`、`git diff --check` | 通过 |

上述验证使用脚本 Provider，证明请求与持久化合同；不等同于真实模型的修复效果、
费用评测或 P3–P5 已完成。

**C1–C12 的现有支撑与待补门禁**

下表是测试追踪，不是完整合规证明。既有测试只支撑各自检查到的局部性质，
不得因为某个测试名包含 `Stable`、`NonAuthoritative` 或 `DoesNotBlock` 就判定整项契约通过。

| 契约 | 当前支撑或缺陷证据 | 后续必须补齐的验收 |
| --- | --- | --- |
| C1 | B1/B3 保留当前请求；[current_turn_test.go](../../internal/runtime/agent/context/current_turn_test.go)检查用户消息和闭合工具对 | 约束、审批、续写和极小窗口的联合保留及准确容量失败 |
| C2 | P2 的 B1 正值上限及同轮引用回归通过；B2 已验证完整范围与模型请求覆盖 | 真实请求中定义与 source/item 绑定；多份报告、歧义、来源缺失和撤回 |
| C3 | [message_ledger_test.go](../../internal/runtime/agent/context/message_ledger_test.go)的 `TestItemIdentitySurvivesUnrelatedHistoryPrefixRemoval` 只覆盖账本消息身份 | 报告条目身份跨编号重排、压缩、模型切换、持久化和 Fork 保持稳定 |
| C4 | P2 的 B1、计划改名重排、来源替代及同轮焦点切换 | 多项逐轮推进、用户纠正和显式任务切换；状态来自正确的权威所有者 |
| C5 | P3 覆盖失败、无收益与 Provider 失败均保留已有表示 | 摘要关闭、失败、排队和覆盖不足时保留有效表示；淘汰前校验覆盖 |
| C6 | [compact_truth_test.go](../../internal/runtime/agent/context/compact_truth_test.go)的 `TestTruthCapsuleRejectsInventedVerification`；[compact_narrative_test.go](../../internal/runtime/agent/context/compact_narrative_test.go)拒绝未知来源和字段 | 新 Representation、来源索引及语义关系不能改写执行/审批/验证事实 |
| C7 | P1 已升级 B3，并验证精确差集与实际请求中的提示；[turnhistory_test.go](../../internal/adapter/tool/turnhistory/turnhistory_test.go)覆盖既有轮次读取和 Findings 首页 | 对所有遗漏原因验证实际差集、来源与真实 Schema 参数；包括部分归档和范围恢复 |
| C8 | P1 最终请求/Schema/续写/提示净成本回归；[message_ledger_test.go](../../internal/runtime/agent/context/message_ledger_test.go)测量投影请求；[context_policy_test.go](../../internal/runtime/agent/engine/context_policy_test.go)检查能力派生和显式 ceiling | 最终 Provider 封装、缓存/增量逻辑窗口、输出配置变化和摘要分块请求的边界 |
| C9 | [view_test.go](../../internal/runtime/agent/contextview/view_test.go)检查工具对；[context_ledger_test.go](../../internal/runtime/agent/engine/context_ledger_test.go)检查请求前缀和账本 | 统一投影后的工具配对与 Provider replay 失效/重建同时成立 |
| C10 | [narrative_test.go](../../internal/runtime/agent/engine/narrative_test.go)的 `TestPreparePostTurnNarrativeSnapshotExcludesLaterTurns` 覆盖输入快照；P3 的 `TestNarrativeP3ForegroundDoesNotJoinProviderCleanup` 用独立 channel 控制后台结束，证明前台先完成 | 用 channel/barrier 证明后台未结束时下一轮已进入采样，迟到候选不会覆盖新状态；不能以固定 sleep 或超时上限代替不阻塞证明 |
| C11 | [session_manifest_test.go](../../internal/runtime/agent/context/session_manifest_test.go)、[生命周期基线](../../internal/runtime/app/context_engineering_baseline_test.go)支撑既有保存与回放路径 | 新来源/条目的序列化、恢复、撤回、Fork、子任务继承、费用结算和 CAS 根引用/回收 |
| C12 | P1 显式零值和配置边界回归；[context_policy_test.go](../../internal/runtime/agent/engine/context_policy_test.go)已有能力派生与显式覆盖测试 | 新预算字段的 provenance、校验和边界；移除摘要逐消息隐含上限及语言关键词保留权重 |

P0 不新增生产序列化类型或改变现有 Snapshot/Manifest，因此不引入持久化格式迁移。
Fixture 的 `schema_version` 仅约束测试输入。P2 的可选字段已验证缺省编码、digest
及恢复行为，沿用格式版本 1；不能因为本次 Fixture 带版本就建设生产迁移框架。

**运行与升级规则**

同时运行保留的 P0 对照与 P1/P2/P3 正向回归：

```bash
go test ./internal/runtime/agent/engine ./internal/runtime/agent/context -run '^TestContextContinuity(P0Baseline|P1|P2|P3)' -count=1 -v
```

修改这些测试或对应实现后，按影响范围运行包测试及文档检查：

```bash
go test ./internal/runtime/agent/context ./internal/runtime/agent/contextview ./internal/runtime/agent/engine ./internal/adapter/tool/turnhistory
make docs-check
git diff --check
```

后续阶段修复导致基线失败时，应在同一变更中：

1. 对照 C1–C12 判断改变是否满足目标，保留同一会话和有效容量前提。
2. 将对应缺陷断言提升为正向回归，去掉该测试的 `P0Baseline` 名称；更新 Fixture 的观察字段及此表状态，不能只是刷新为新缺陷值。
3. 保留当前请求、来源完整性、预算、工具配对和已有恢复能力等控制断言；针对新来源类型补上来源及状态检查。
4. 附上升级后真实请求或覆盖范围的验证结果。不得 `Skip`、放宽到只检查工具名，或仅增加轮数来宣称验收完成。

P0 结果只回答“问题是否能稳定复现、目标如何判定”。P1 的已验证范围见上文，后续阶段仍按方案中的阶段标准和
完整验收矩阵判断，不能用本组基线通过代替连续性修复完成。


**P3 已实现范围**

- 摘要输入保留源消息的完整文本投影，按 Markdown 结构与 UTF-8 子范围拆分；来源、父结构、原编号和 digest 随每块传递。逐请求按实际 TokenEstimator 和 summary Route 准入，字节限制不再代替 token ceiling。
- 输出要求覆盖全部输入 ID；聚合验证连续范围和来源 digest。覆盖失败、无净缩减、超时和校验错误均保留原文/旧有效表示。
- 确定性终态检查点先提交；后台不再调用当前轮封存函数。来源快照固定，下一轮不 join，候选只在空闲或下一轮开始时安装；新计划不被旧候选覆盖。
- 复用 Context Manifest/CAS 的独立维护提交，新增可选 BaseRevision 做比较。事务内读取最新业务终态和维护根，以较新的 Context revision 校验；有效交错提交可成功，过期、跳版本、跨线程和损坏终态被拒绝。显式压缩使用相同版本口径。处理记录、范围覆盖进入原 Compaction Owner。每个物理请求的费用独立保留，失败和丢弃同样结算；关闭 Runtime 时先取消作业并结算已观测用量，再关闭事件存储。
- 回归位于 [范围校验](../../internal/runtime/agent/context/narrative_ranges_test.go) 与 [P3 生成和并发回归](../../internal/runtime/agent/engine/narrative_p3_test.go)，覆盖中文尾部、实际请求 ceiling、总 deadline、缺失/伪造范围、去重、前台优先、计划更新、epoch 失效及提交失败。
- [持久化交错回归](../../internal/persist/contextstate/context_rebase_test.go)使用真实 SQLite 终态提交，验证 Manifest 与 SessionDelta 两种表示；维护版本 2 后业务终态推进到 3，再以 base 3 提交维护版本 4，恢复保留最新表示和去重记录。

P3 使用脚本 Provider 验证机制与用量，不声称已完成真实模型语义忠实度评测。完整生命周期压力回归见下文 P4，默认配置切换仍属于 P5。Provider 真正占用的并发许可仍受公开配置约束，取消请求不会虚构已经释放的物理连接。

**P3 验证记录（2026-10-08）**

| 检查 | 结果 |
| --- | --- |
| `context`、`contextview`、`prompt`、`engine`、`app`、`app/persistence`、`persist/contextstate`、`persist/state/turnstate`、`turnkernel`、`protocol` 包测试 | 通过；Engine 全包约 112 秒，最终 Runtime 全包约 35 秒；持久化交错提交修复后全包复跑通过 |
| 摘要生成、中文连续性、候选安装、前台不等待、Restore 取消、关闭结算等聚焦 `-race` | 通过；覆盖 `context`、`engine`、`app` 及终态存储，`persist/contextstate` 最终全包 `-race` 通过 |
| `app/wire` 的 P3 摘要 token 配置传递测试 | 通过；没有把聚焦结果记为 wire 全包验收 |
| `make protocol-schema`、`make web-protocol-check` | 通过；使用仓库命令生成和校验协议 |
| `make docs-check`、`make capacity-policy-check`、`git diff --check` | 通过 |

`TestDeliverablePlanDoesNotBypassVerification` 的请求 Fixture 本身约 4,103 tokens，超出原先 4,096-token Route；移除 P3 终态封存也可复现。该测试仅将显式 Route 调整为 8,192 tokens，保留验证门禁与完成行为断言，不修改产品默认容量。


**P4 已实施范围**

- [有界 Checkpoint 选择](../../internal/runtime/agent/contextview/checkpoints_test.go)：仅选本次来源引用轮与最近闭合轮，按总字节和完整请求准入；千轮压力不把全部持久块装入模型，选择不修改 write-once 内容。
- [实际请求与执行回归](../../internal/runtime/agent/engine/context_p4_test.go)：小窗口、经济预算、观测窗口用量和摘要关闭时保留当前约束、稳定 ID 与完整定义。未绑定候选过大时进入目录恢复；业务工具在执行前被拒绝，同批次 `update_plan` 或空焦点不能提前解除限制。精确恢复并绑定后，下一次采样完整装入定义再恢复执行。
- [来源范围回归](../../internal/runtime/agent/context/conversation_ranges_test.go)：选择子项时保留完整条目及祖先自身的前提、结尾约束，排除无关兄弟定义；带总标题的长报告不会因父章节展开重新装入全文。模型投影、按项回读和子任务继承共用范围计算，来源 ID 与原文字节偏移不变。
- [分页回归](../../internal/adapter/tool/turnhistory/recovery_test.go)：稳定 source/item、目录、单来源索引，保留 turn 形式；offset/content_digest 拒绝陈旧游标，中文小页逐字节重建原文。Findings 同样计入 max_bytes，超长索引不突破上限。完整归档优先于内存残片；未知/跨会话和撤回来源不能被工具恢复或再次采样。
- 运行中续跑点持久化已接受的来源、焦点、Plan 与目录恢复状态；正文仍经 ConversationManifest/CAS。真实 `update_plan` 批次接受后模拟重启，首次请求保留焦点定义和步骤引用。环境漂移、revision/epoch 变化或来源正文缺失时失败，Provider 不采样。
- Fork 保留来源身份与 lineage，独立更新焦点；真实工具调用使用当前 Engine 的来源查询与 Plan 回调，共享注册表不能回调父线程或广播更新；模型和输出上限变化后重算请求。模型 Profile 切换回归同时检查旧窗口/预备压缩失效、来源和焦点保留。
- [子任务继承回归](../../internal/orchestration/subagent/context_fork_test.go)：`fresh` 无父定义，其他模式独立携带所选来源；历史裁剪不删定义，脱敏保留原始 digest/范围并标记。统一文本 token 估算与显式字节上限分别检查；任务请求、角色/工作区约束和必要定义本身放不下时明确拒绝，不截断后执行。
- [共享所有权回收](../../internal/persist/contextstate/content_lifecycle_test.go)及[续跑暂存提交](../../internal/persist/state/turnstate/content_test.go)：共享来源在最后 owner 删除前可恢复；成功事实接管全部正文引用，提交失败或 owner 删除后回收对象、边和暂存，无泄漏。

完整依赖本身超过模型容量时，P4 不用残缺正文代替定义。目录与分页支持先恢复索引、
缩小至可装入条目；条目和父级前提本身仍超限时需要拆分任务或调整预算。
工具页若因通用准入再次缩短，页内剩余用 `result_get`，源中尚未读取的页仍用
`turn_history`。本阶段没有改变 `recent_tail_turns=2`；默认切换、效果指标与真实
模型费用对照留给 P5。


**P4 验证记录（2026-10-08）**

| 检查 | 结果 |
| --- | --- |
| `context`、`contextview`、`prompt`、`engine`、`tool`、`turnhistory`、`interact`、`subagent`、`persist/contextstate`、`persist/state/turnstate`、`turnkernel`、`app`、`app/persistence`、`protocol`、`config` 全包 | 通过；Engine 一轮约 125 秒，App 约 42 秒；最终依赖范围收敛后 Context/Contextview/Prompt/Engine 全包再次通过，Engine 约 113 秒 |
| P4 压力、目录恢复、Fork、重启、来源/Plan、撤回、模型切换与 CAS 聚焦 `-race` | 通过；并补最终范围选择与续跑所有权回归；`interact`、`turnhistory`、`subagent`、`persist/contextstate` 全包竞态通过 |
| `make protocol-schema`、`make web-protocol-check`、`make capacity-policy-check` | 通过 |
| Web TypeScript 检查与 `npm --prefix web test` | 通过；28 个文件、385 个测试 |
| `make docs-check`、`git diff --check` | 通过 |
| `app/wire` 的 Context/Fork 聚焦检查 | 通过 |
| `app/wire` 扩展检查 | 复现已有 `TestChildAgentRunsRealEngineTurn` 断言差异：期待 `not_evaluated`，当前验证逻辑返回 `not_required`；与 P1 记录一致，本阶段未修改验证判定或放宽断言 |

验证使用脚本 Provider 和真实持久化组件，没有进行真实模型效果/费用评测，
也没有将聚焦竞态结果记成 Engine 全量竞态通过。
