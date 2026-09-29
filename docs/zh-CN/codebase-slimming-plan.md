# 代码瘦身方案

状态：分析结论与分阶段实施合同。日期：2026-09-25。
依据：对全仓的两轮静态审计（覆盖 `internal/` 全部 16 个顶层包、`web/`、`desktop/`），
所有"零引用"论断均经全仓 grep 验证；行数基线取自 2026-09-25 的
`wc -l` 实测（生产/测试拆分）。产品判据来自
[overview](./overview.md)、[roadmap](./roadmap.md) 与 [usage](./usage.md)。

## 1. 背景与基线

### 1.1 规模

| 目录 | 总行数 | 其中测试 |
| --- | --- | --- |
| internal/runtime | 126,156 | — |
| internal/adapter | 82,366 | — |
| internal/persist | 34,401 | 13,614 |
| web/src | ~30,000 | ~3,200 |
| internal/security | 25,372 | 9,919 |
| internal/host | 19,487 | 7,175 |
| internal/platform | 13,631 | 5,848 |
| 其余（orchestration/observability/config/desktop 等） | ~24,000 | — |

Go 合计 327,990 行：生产 183,064、测试 144,861（44%）。近 90 天改动热度
集中在 `internal/runtime/agent`（2,201 次文件改动）、`internal/runtime/app`
（1,517）、`internal/adapter/tool`（1,179）——这三个目录恰好也是本方案
收益最大的区域，同时是测试覆盖最厚的区域（如 engine 测试 23.5k 行），
属于"可以动、但要小步动"。

### 1.2 核心判断

审计没有发现可整体删除的死子系统：全部 internal 顶层包都有活跃调用方，
`persist` 的 CAS/contentstore 是分层而非重复，sandbox 只有 seatbelt 一个
真实后端，测试占比 44% 是这个 Runtime 的资产而非负担。维护成本来自三件事：

1. **同一能力多处实现**：分页投影、head/tail 截断、glob 匹配、UTF-8 切边、
   token 估算、路径包含性判断，各有 2~6 处独立实现；
2. **功能只增不减**：工具面约 30 个族中存在整族零文档、零引用、且与
   `exec_command` 重叠的遗留工具；另有大量"仅测试引用的生产符号"；
3. **巨型文件与巨型函数**：`server.go` 2,797 行、`tool.go` 2,195 行、
   `Scope.Run` 单函数约 1,167 行、`App.tsx` 单组件约 2,240 行。

因此本方案的重心是**清理死代码、裁剪边缘功能、收敛真实重复**，而不是
压缩代码。这与 [agent-guide](./agent-guide.md) 的约束一致：不引入行数、
扇出或函数长度棘轮，不为满足体积预算而压缩或拆分代码；下文每一项动作
都附带独立于行数的理由（死代码、产品决策、真实重复、正确性）。

## 2. 功能价值分级（裁剪判据）

分级判据取自产品文档自身：[roadmap](./roadmap.md) 北极星七条
（仓库定位 → 有界计划 → 最小受治理变更 → 自动诊断/相关测试 →
修复或回滚 → Durable Receipt → 主/子 Agent 语义一致），
[usage](./usage.md) 明示的工具用法与产品边界（"GitLab、内部平台和企业
认证继续通过 MCP 或 Skill 提供"；"测试、构建和静态检查统一使用
exec_command，不再提供独立 quality 工具"），以及 roadmap 明声的
"不追求内置工具数量最大化"。

### 2.1 工具面分级

| 工具族 | 规模（非测试） | 分级 | 结论 |
| --- | --- | --- | --- |
| file / shell / search / git / guard / interact（前三者）/ agent / completion / handle+result / turnhistory / revert / toolsearch | 14,000+ | 核心 | 保留，北极星与 usage 主线 |
| lsp / memory / skill / mcp bridge / document_convert | 3,800+ | 重要 | 保留，生态与验证承诺 |
| content 族 4 工具（image_ocr/speech_transcribe/data_validate/content_capabilities） | ~350 | 边缘 | 删除（见 4.1） |
| dev 族（format_code/debug_run/dependency_resolve） | 780 | 边缘 | 删 2~3 个（见 4.1） |
| repohost（GitHub 4 工具） | 259 | 边缘 | 删除（见 4.1） |
| project_map / image_analyze / web_scrape / agent.send_message | ~350 | 边缘 | 合并或降级（见 4.2/4.3） |

### 2.2 看似可疑、实则核心（禁止误删清单）

- `search_symbol/definition/references/related_tests`：repository-intelligence
  提案 M1–M4 主线，有独立索引与 LSP 支撑，不是 `search_text` 可替代的；
- `revert_turn`（北极星第 5 条）、`turn_history`（agent-guide 合同）、
  verify/trace/usage/receipt（差异化卖点）、repoindex/symbols（规划核心）；
- bench（`release-gate` 依赖）、desktop 壳（文档化交付物，活跃投入）；
- `persist` 的 profile 在线迁移与 turnstate 双写：活路径，删除破坏存量数据。

## 3. 主题一：死代码清扫（约 1,300 行，风险极低）

全部经全仓 grep 验证零调用方或仅测试引用，可在一个 PR 内完成：

| 位置 | 内容 | 行数 |
| --- | --- | --- |
| `internal/evaluation/protocol/` | 整包零导入（含测试 305） | 305 |
| `internal/persist/state/history_search.go` | `SearchHistory`/`HistoryHit` 整文件零调用 | 209 |
| `internal/persist/state/store.go:418` | `PatchThreadMeta`+`ThreadMetaPatch`+`ErrEmptyMetaPatch` | ~65 |
| `internal/persist/repoindex/related.go` | `TestMapper`（零实例化）、`Paths`（仅测试） | ~40 |
| `internal/security/sandbox/backend.go:330` | `WithClose`/`closeBinding`/`IsUnavailable` | ~21 |
| `internal/runtime/agent` 各处 | `BufferOutput`、`RequiredActionOr`、`HistoryHasSessionStateHint`、死配置字段 `Authorize`/`MaxToolConcurrent`/`MaxToolStreamBytes`、仅测试引用的 `RebindNarrativeInput` 等包装、`command_matrix.go` 移入 `_test.go` | ~200 |
| `internal/runtime/app` | `engine_contract.go` 22 个无引用别名（67→10 行）、`Runtime.RouteMailbox`、`NoopEngine` 移入测试、`extension.AdaptEngine` | ~90 |
| `web/src/ui/backgroundActivity.ts:59` | `sessionStatusPresentation` | 24 |

注意：`contextview.BuildPrefixManifest`（估算版）虽仅测试引用，但其测试
验证"估算≈实测"的等价性，属安全网，保留。

## 4. 主题二：边缘功能裁剪（约 2,500~3,300 行，需逐项决策）

### 4.1 建议直接删除

| 功能 | 位置 | 行数 | 依据 |
| --- | --- | --- | --- |
| content 族 4 工具 + `platform/contentdeps` | `adapter/tool/content` | ~350 | 零文档、零包外引用；OCR/转写依赖 tesseract/whisper 均未进配置文档；data_validate 可由 exec_command+jq 替代；保留 document_convert（security.md 在用） |
| repohost GitHub 4 工具 | `adapter/tool/repohost/` | ~260（+测试） | 被 usage.md 自己声明的"MCP/Skill 承接平台集成"边界否定；依赖宿主 gh CLI；git log 仅 2 次提交 |
| dev 族 debug_run + dependency_resolve | `adapter/tool/dev/` | ~545 | usage.md 已确立"统一走 exec_command"的 quality 工具边界；debug_run（LLDB 批处理）极小众；激进方案含 format_code 共 -780 |
| MCP OAuth（PKCE 全流程） | `adapter/mcp/oauth.go` | ~417 | 零文档的 speculative 生态面；roadmap 中期只承诺 MCP Provenance/Risk，未承诺 OAuth。**需负责人确认** |
| legacy setup 目录迁移 | `host/web/legacy_setup_catalog.json` + setup.go 迁移段 | ~400 | 注释自述"一次性物化迁移"，服务未发布开发状态，违反 agent-guide 的 no-compat-migration 约束 |
| providerdump | `observability/providerdump/` | ~179 | 环境变量门控的调试后门，单点消费（provider/httpclient/response.go）。可选项 |
| Mermaid 前端渲染 | `web/src/ui/MermaidDiagram.tsx` + mermaid 依赖 | ~340 | usage.md 的 Markdown 支持清单不含 mermaid；web 最重依赖之一，虽已懒加载 |

### 4.2 建议合并

- `web_scrape` 并入 `web_fetch`（`extract_text` 参数，约 -80，省一个 catalog 位）；
- `project_map` 删除，模型改用 `file_list` 与 prompt repo_map 分区（功能被双向覆盖，
  约含 projection 核心集项 -100）；
- `agent.send_message` 并入 `followup_task`（usage.md 只点名其余 6 个 agent 工具，约 -60）。

### 4.3 建议降级（保留代码、移出默认投影，收紧 prompt 预算）

`image_analyze`（去掉 screenshot 关键字特判，保留给显式配置 `[route.vision]`
的用户）、`git_tag`/`git_amend`、`memory_update`/`forget`、`format_code`
（若不删）、web_search 的 Bocha/SearXNG 后端——统一改为仅 `tool_search`
可发现。降级不减行数，但直接降低每 turn 的 catalog 投影成本。

### 4.4 决策记录要求

4.1 中的 MCP OAuth、providerdump、Mermaid 与 dev 族删减范围需要产品
负责人逐项确认后执行；确认结论记录于本文档的变更历史。

## 5. 主题三：抽象与复用收敛（约 2,400~3,100 行 Go，另 ~600 行 TS 生成化）

### 5.1 横切关注点收敛

| 模式 | 现状 | 共享抽象 | 预计净减 | 风险 |
| --- | --- | --- | --- | --- |
| 分页投影引擎 | `result_get`（tool/tool.go:1735）与 `handle_read`（tool/handle/handle.go:239）两套完整七模式实现；skill/turnhistory 各有游标残片 | `tool` 包内 `page[T]` 投影器 + 统一游标 | 150~300 | 中低（两套游标语义有微差，需对齐+测试） |
| guard 收尾/回滚序列 | `pipeline_attempt.go` 中 9 处 terminal 序列 + 14 处 attemptReceipt 早退；guard.go:615 附近有字面 no-op | `finishAttempt`/`rejectAttempt` 助手 | 150~250 | 中（安全关键路径，靠 guard_test.go 1974 行锁定后动） |
| sandbox 绑定样板 | 11 处 `BindPolicy+NewWorkspace` 三连 + builtin 双重绑定 | `sandbox.BindWorkspace(root, backend)` | 80~120 | 低 |
| 工具 Descriptor 冗余 | `agent/ops.go:89` 8 个 Descriptor 六字段同值；file/git/interact 同型 | `baseDescriptor(name, desc)` + 差异字段 | 80~180 | 低（纯数据） |
| 引擎重试收尾重复 | `engine/model_handler.go:532` 与 `:725` 两条路径的 ~70 行重试序列近乎逐行相同 | `handleSampleFailure` + 状态小结构体 | 55~70 | 中（provider_retry_test 651 行 + stream_recovery_test 710 行覆盖） |
| turnkernel Approval/Input 平行 | `runtime_kernel.go:559-700` 六方法两两对应 | 只共享 `resolveEffect` 序列（3 处重复） | 40~60 | 中（需过 turn-kernel-convergence 门禁） |
| turn_handler 终局/修复分支 | 两份终局 switch、三个同构 repair case、complete/block 尾部结算重复 | 表驱动 + 结算提取 | ~60 | 中 |
| 路径包含性判断 | 6 个异构实现（Rel-based 与 prefix-based 两种语义并存） | `platform/pathutil.Contains/CleanUnder` | 60~80 | **非零**：安全边界函数，逐处确认语义后再替换 |
| UTF-8 截断 | 5 处，其中 2 处按字节切边（见 5.4） | `platform` `TruncateRunes/TailRunes` | 30~40 | 极低 |
| head/tail 截断 | 4 处（shell accumulator、summarizeResult、summarize、compact_failures） | 并入分页投影包 | ~80 | 中低 |
| glob 匹配 | search 的 `globRegex` 与 shell 的 `write_globs` 两套 | 统一支持 `**` 的 matcher | ~60 | 中 |
| childruntime 结算链 | 5 层函数约 100 行，中间层无独立职责；StartTurn 回滚三件套 5 分支手写 | 收敛结算链 + defer 回滚栈 | 75~85 | 中 |
| 前端 overlay 三份 CSS + 8 处 outside-close JSX | 三个对话框各写一份 `.contextDialogOverlay` | `primitives/Dialog.tsx` | 100~130 | 低 |

### 5.2 生成代码扩展（沿用现有挂载点）

仓库已有 `make web-protocol`（webprotocolgen）与 `make protocol-schema`
（eventtraitgen）两条生成通道，扩展方向与现有约定同向：

1. **TS 类型生成**（预计 -600 行 TS 手工维护面）：`web/src/protocol.ts`
   902 行手写镜像 Go protocol 类型，contract JSON 已含每条路由的请求/响应
   类型名，扩展 webprotocolgen 发射 `*.types.generated.ts`（optionality 从
   omitempty 推导），挂到 `web-protocol-check` 漂移门。主要价值是消除
   "Go 加字段、TS 忘同步"这一整类漂移。风险中，需逐接口核对可选性。
2. **web host 泛型路由表**（-250）：server.go 及配套文件里 67 个
   decode+delegate handler，改 `map[route]func(ctx, Deps, *Req)` 泛型表；
   命名约定与 unary_routes.generated.go 一致，可顺带生成表项。
3. `engine_adapter.go` 的 ~420 行字段拷贝：**只做**低成本档（每个分支提为
   具名转换函数 + 纯拷贝结构体直接类型别名），不做生成器——该文件 40%
   是投影逻辑而非拷贝，生成器表达不了。

### 5.3 测试样板收敛

- **fixture stream builder**（-500~650 测试行）：全仓 136 处手写
  `SliceStream{...Start/Stop 包装}`；在 `provider/fixture` 加
  `Reply/ToolCall/Streams` 三个 builder。只适用于完整流，断流/错误流
  测试保留手写。
- 微型测试 helper（mustJSON×3、writeFile×3、runGit×4 等）提升到
  `internal/testutil` 约 -100~150，但会造成测试对公共包耦合：**只在新增
  时复用，不批量迁移**。

### 5.4 顺带修复的正确性问题（价值大于行数）

1. `observability/providerdump/dump.go:178` 与
   `orchestration/subagent/result.go:110` 按 `value[:limit]` 字节切边，
   会切断多字节 rune（mojibake 输出点）；
2. `orchestration/subagent/context_fork.go:264,664` 用 rune/4 估算 token，
   而 `platform/tokenestimate` 文档明确该启发式对 CJK 低估约 4 倍——
   子代理 fork 上下文预算存在系统性偏差，应改调 platform 实现。

### 5.5 明确不做的抽象（防止为行数压缩）

- 分层场景测试（withdrawal 等在 engine/app/persistence/web 五层各有
  ~130 行）不合并：断言对象不同，runtimecontract 已是跨 transport 的
  共享层，合并会降低失败定位能力；
- 3,100 个测试函数不批量 table-driven 化；
- `persist/state` 与 `runtime/app` 的 EventStore 双实现不统一（内存环形
  vs SQLite 持久，接口本就分离）；
- 364 处 JSON schema 手写 map 不引入公共 builder（异构 anyOf/别名逻辑
  的抽象税高于 ~200 行收益）；
- config 四表面（schema/loader/env/override/provenance，新字段 8 处编辑）
  只做**增量**声明式治理：新字段写描述表，旧字段不动——provenance 与
  校验是 contract 测试锁死的公共契约，禁止回头改造；
- 错误分类/重试决策各层（protocol/provider/engine/httpclient/persist）
  不强行统一成上帝函数。

## 6. 主题四：测试与构建基础设施归位

roadmap 已将"可重复的 Web Release Pipeline"列为近期目标，因此
bench（`release-gate` 依赖）与 runtimecontract（不进二进制）**保留**。
归位动作：

- `provider/fixture` 因 bench 留在生产 import 图：移到 `internal/testutil`
  lane 或加构建标签，让生产构建不再编译测试设施；
- `internal/runtime/eventview`（167 行）是 bench 的唯一生产消费者，随
  fixture 归位一并处理；
- `adapter/mcp/contract/` 目录只有 fixture_test.go，布局归位；
- `extension.NoopEngine`（33 行）移入测试支持文件。

## 7. 主题五：巨型文件拆解（不减行数，降维护成本）

按收益排序，全部是可维护性驱动的行为不变重构：

1. `web/src/ui/App.tsx`（4,232 行，App() 单函数约 2,240 行）：6 个自包含
   对话框/转录组件（ApprovalComposer、InputComposer、三个 Dialog、
   SessionRow、TurnTranscript 族）搬出，顺带消除 props 钻透中转；
2. `host/runtimeapi/web/server.go`（2,797 行）：workspace 校验
   （:1847-2160）、静态资源、中间件各自成文件；
3. `engine` 的 `modelStep`（745 行）与 `Scope.Run`（约 1,167 行）：
   主题三去重后自然减半，再按重试/终局/修复分段；
4. `persist/artifact/service.go`（1,955 行）：四大块（turn 恢复证据、
   检查点、计划执行、工件持久化）本就职责清晰，按块拆文件。

## 8. 实施路线图

| 波次 | 内容 | 预计 | 验证 |
| --- | --- | --- | --- |
| Wave 1（第 1 周） | 主题一全部 + 5.4 正确性修复 + guard no-op 删除 | -1,300 | `go test ./...`、`make docs-check`、`npm --prefix web run check && test` |
| Wave 2（第 2~3 周） | 4.1/4.2 已确认项 + 4.3 降级 + 5.1 低风险行（分页投影、绑定、Descriptor、UTF-8、Dialog） | -1,500~2,200 | 同上 + tool 契约测试 + catalog 快照 |
| Wave 3（第 4~6 周） | 5.1 中风险行（guard、engine/turnkernel 去重、childruntime）+ 5.2 TS 类型生成与路由表 + 5.3 fixture builder + 主题四归位 + 主题五拆解 | -1,200~1,900 + TS 生成化 | 同上 + `make turn-kernel-convergence-baseline` + `make web-protocol-check` + `make verify` |

约束：每项独立提交；动 guard/engine/turnkernel 前先确认对应契约测试
与门禁基线在位；Wave 2 的产品裁剪项按 4.4 完成决策记录后才动手；
与 worktree 中未落地的工作（当前有 12 个未提交修改）不得交叉。

## 9. 防反弹机制

1. Wave 1 合入后在 CI 增加死代码巡检（`deadcode`/`unused` 定期跑，
   只报警不阻塞）；
2. 新增工具必须同步更新 usage.md 的工具表（文档即准入）；
3. 新增 config 字段走 5.5 的增量声明式表；
4. catalog 投影核心集的每次增改需要在 PR 描述中给出 prompt 预算依据。

## 10. 收益总账

| 主题 | 行数 | 备注 |
| --- | --- | --- |
| 一：死代码清扫 | ~1,300 | 风险极低 |
| 二：边缘功能裁剪 | ~2,500~3,300 | 含 4 项待决策；另降级 5 个工具的投影成本 |
| 三：抽象复用 | ~2,400~3,100（Go）+ ~600（TS 生成化）+ ~550（测试） | 含 2 个正确性修复 |
| 四：构建归位 | 0（不删） | 生产构建不再编译测试设施 |
| 五：巨型文件拆解 | 0（不减） | 维护成本下降 |
| 合计 | **~6,700~8,200（约生产代码的 4%）** | 加测试侧 ~550 |

对照 1.2 的判断：本方案不追求行数指标本身。若产品侧确认 4.4 的全部
决策项并完成 Wave 3，实际效果是——工具面从 30 族收敛到约 24 族、
6 类横切重复收敛为单一实现、跨语言 DTO 进入生成通道、5 个巨物文件
拆解、外加 3 处正确性修复。维护成本的降幅将显著大于 4% 的行数降幅。
