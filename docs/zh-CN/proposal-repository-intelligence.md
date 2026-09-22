# 仓库智能设计规划（提案）

> 本文是设计提案，描述目标与实施路径，不作为能力已交付的证明。当前行为以
> [架构设计](./architecture.md)、[Agent 指南](./agent-guide.md)和代码为准。
> 本提案对应 [后续规划](./roadmap.md) 中期 "Coding Intelligence" 与近期
> "正确性" 条目的具体化。

## 1. 背景与问题

QCode 当前的仓库理解是纯词法路线：

| 能力 | 现状 | 位置 |
| --- | --- | --- |
| 符号提取 | 主要语言优先语法树，失败时回退词法或启发式；见[代码结构提取](./code-structure.md) | `internal/platform/symbols/symbols.go` |
| 符号索引 | path/name/kind/container/line/exported，无签名、无引用、无 docstring | `internal/persist/repoindex` |
| Repo Map | 目录 + 符号计数摘要，目录按符号数排名，入口点为硬编码文件名表 | `internal/runtime/agent/repository/map.go` |
| 语义查询 | LSP 一次性进程（每次查询冷启动），否则词法回退 | `internal/adapter/lsp` |
| 受影响测试 | 纯命名约定（Rust 不支持），注释自认不完整 | `internal/persist/repoindex/related.go` |
| 验证命令 | 无自动发现，单条配置命令 + 模型自报 `covered_paths` 对账 | `internal/observability/verify` |
| 跨会话仓库知识 | 无。探索结论止于 Session 级 Resume Fact | `internal/adapter/memory` 仅用户记忆 |

由此产生的用户可见问题：

1. **定位不准**：模型在大型仓库依赖 `search_text` 多轮探索，重复读取与
   无关读取消耗上下文预算；
2. **影响分析不可信**：改一个符号无法可靠找到跨文件调用者与受影响测试，
   验证门禁只能退回模型自报证据；
3. **每个会话从零开始**：上次会话已验证过的构建/测试拓扑、已读结论
   无法在新会话复用；
4. **验证命令配置负担**：`verify.command` 是单条手工配置，多数用户
   从未配置，验证门禁形同虚设。

业界对照（截至 2026-09）：tree-sitter 级结构提取与引用图排名
（Aider repo map 范式、RepoGraph ICLR 2025 报告 SWE-bench +2.3~2.7%
绝对提升）已是验证过的最低成本高收益改进；语义检索/embedding 仍有争议
（Cursor 报告 +12.5%，Sourcegraph Cody 已移除 embedding），维持 roadmap
"只在显著改善证据时引入"的承诺，不在本提案范围内。

## 2. 设计原则

以下原则贯穿全部工作流，违反任何一条的设计在评审时直接拒绝：

1. **置信度分级，不冒充精确**。仓库理解结果始终携带
   `Resolution` 声明，按可信度分级：
   `lexical`（词法规则）→ `syntax`（语法树提取，本提案新增）→
   `lsp`（语言服务器）。任何消费方（工具结果、Repo Map、Receipt）
   都能区分近似与精确。图缺失或提取失败时逐级降级，不失败、不静默。
2. **索引是可重建的派生状态**。repoindex 不属于权威持久化
   （Event Log / Session / Journal），schema 演进通过
   `IndexerVersion` 提升触发整体重建完成，不进入 SQLite
   `user_version` 迁移链。
3. **无未文档化阈值**。新增数值参数全部是公开配置字段，默认值带
   provenance（算法标准值、显式配置策略或派生规则），有校验、
   文档和边界测试。仓库现有内部上限（如索引文件数上限）随本提案
   升级为公开配置字段并补齐 provenance。
4. **证据带来源**。Repo Map 条目、影响分析边、验证命令候选、派生
   记忆都携带来源（文件、行号、digest 或 Receipt 引用），能被
   Execution Receipt 与审计解释。
5. **不扩大攻击面**。新增常驻进程（LSP）沿用现有进程治理
   （Process Broker、沙箱边界、默认关闭）；发现的验证命令不自动执行，
   一律经 `exec_command` 的 Guard、审批与沙箱。
6. **词法层保留为兜底**。语法提取器与 LSP 的任何失败都回退到现有
   词法行为，Resolution 降级可见；不删除现有提取器。

## 3. 目标架构

```text
置信度阶梯（每级可降级到上一级）
  lexical  ── 现有词法规则表（兜底，永不出来）
  syntax   ── R1 tree-sitter 提取器（签名/docstring/引用标识符）
  lsp      ── R3 常驻语言服务器（definition/references/诊断）

派生数据面（全部可重建，键控 workspace root）
  repoindex ┬ 符号表（R1 扩展 signature/docstring）
            ├ 引用边表（R2：import 边 + reference 边，带 Resolution）
            └ 发现缓存（R5：manifest/CI 派生命令候选，digest 键控）

消费面
  Repo Map（R2 引用排名）──> prompt repo_map 分区
  工具面（search_symbol/definition/references/related_tests 升级返回证据）
  验证门禁（R4 图驱动受影响测试 + R5 命令候选）
  Truth Capsule / session_state（R6 派生事实注入，跨会话）
```

工作流编号：R1 语法级符号提取、R2 引用图与 Repo Map 排名、
R3 常驻 LSP 会话、R4 依赖图驱动影响分析、R5 验证命令发现、
R6 仓库工作记忆。每个工作流按 roadmap 提案格式给出六要素。

## 4. 工作流设计

### R1 通用符号提取层（任何语言基线 + 主流语言增强）

**用户问题与可衡量结果**。两个问题：其一，符号索引缺少签名与
docstring，模型拿到符号列表后仍需整文件读取确认形态；其二，
现有提取器只覆盖六种语言，真实仓库的其余语言（包括 C/C++）
完全没有符号层，这是面向真实仓库的硬伤。完成后：**任何语言的
文件都进入符号索引**（至少基线质量），主流语言拿到高质量词法
提取；`search_symbol` 与 Repo Map outline 返回签名；索引命中率
（按符号找到目标后无需立即读文件的比例）在基准仓库上可测量、
可对比。

**语言覆盖模型（置信度分级即覆盖分级）**：

| 层 | 覆盖 | 能力 | Resolution |
| --- | --- | --- | --- |
| Tier 0 通用启发式 | 任何文本语言，含未知语言与 DSL | 通用声明形态（`name(...)` + 块开始 / 缩进块）、前置注释块即 docstring、文件级标识符 | `heuristic`（新增档） |
| Tier 1 语言精调包 | 主流语言：现有六种 + C/C++、C#、Ruby、PHP、Kotlin、Swift 渐进加入 | 精调规则：准确容器归属、语言特定 docstring 形态、模板/预处理等声明变体 | `lexical` |
| Tier 2 语法级（已交付基础提取） | Go、JS/TS、Python、Rust、Java、C/C++、C#、Ruby、PHP，含 JSX/TSX | 语法声明、签名、文档与文件级标识符；详见[覆盖边界](./code-structure.md) | `syntax` |
| Tier 3 LSP（已有，R3 增强） | 项目级 | 权威 definition/references | `lsp` |

Tier 0 是覆盖保证：语言不在 Tier 1/2 清单中时自动落到
Tier 0，**不存在"不支持的语言"**，只有质量分级。C++ 列入
Tier 1 首批（词法级可覆盖函数/类/命名空间/模板声明与
`#include` 的主流形态；模板重载等精确语义归 Tier 2/3）。

**所属 Package 与 Protocol 影响**：

- `internal/platform/symbols`：现有逐行规则表泛化为
  "通用提取器 + 语言参数包"架构：Tier 0 通用引擎（声明形态 +
  块结构追踪）无配置即可运行于任何语言；Tier 1 语言包在
  M1 内交付现有六语言的迁移与 C/C++ 增补。`Symbol` 结构增加
  `signature`、`docstring`（有界，长度上限为公开配置字段）与
  `references []ReferenceSpan`（本文件内对其他符号名的引用
  位置）；`Resolution` 取值扩展为
  `heuristic/lexical/syntax/lsp` 四档。语言识别从扩展名扩展为
  扩展名 + shebang + 内容启发，未知语言返回通用语言标识并走
  Tier 0。
- `internal/persist/repoindex`：Symbol 存储扩展上述字段；
  `IndexerVersion` 提升一位触发重建；增量刷新逻辑
  （size + mtime + digest）不变。
- Tier 2 基础提取已通过纯 Go 的 gotreesitter 运行时接入，grammar 随依赖内嵌。
  它增强现有索引，不引入外部解析进程或运行时下载。具体语言、解析失败降级、
  索引版本与限制见[代码结构提取](./code-structure.md)。后续已为 Go、JS/TS
  接入作用域感知的跨文件引用候选，并建立[专项评测](./repository-understanding-evaluation.md)；
  其他语言继续使用近似标识符关系，不宣称编译器级语义绑定。
- 协议影响：`search_symbol` 等工具返回体属于模型可见文本，
  新字段向后兼容，不修改 Operation/Event 协议。

**Security 与 Persistence 影响**。提取在进程内完成，无新进程、
无网络。索引仍为可重建派生状态，无权威持久化变更。

**Cancel/Failure/Recovery**。单文件提取失败（二进制、生成代码、
超限文件）跳过该文件符号并在索引元数据记录降级计数；Tier 1
语言包缺陷不吞掉结果——质量可疑时该文件降级 Tier 0 而非空
结果，`Resolution` 如实标注。

**Test 与 Rollout**。Tier 0 通用引擎 fixture：覆盖无语言配置的
未知语言样本、声明形态变体（C 风格块、Python 缩进块）、
docstring 形态（`//`、`#`、`/* */`、字符串 docstring）；
Tier 1 fixture：现有六语言迁移等价性（重构前后符号集一致）+
C/C++ 声明变体（模板、运算符重载、预处理块）；降级路径
（损坏输入、语言包错误）。先以 Shadow 模式并行运行新旧提取器
对比输出（配置开关），数据达标后切换默认。

### R2 引用图与 Repo Map 排名

**用户问题与可衡量结果**。目录按符号数排名无法反映重要性，
入口点靠硬编码文件名表。完成后：Repo Map 在相同 token 预算内
包含更高比例的"实际被引用"符号；map 分区的定位辅助效果用
基准任务的首次定位轮次对比。

**所属 Package 与 Protocol 影响**：

- `internal/persist/repoindex`：新增两张派生表
  `import_edges`（语言特定解析：Go import path × module path 映射、
  TS/JS 相对说明符、Python 相对/包内导入、Rust `crate::` 内部引用、
  Java 包目录惯例；仓库外目标记为 external，不建边）与
  `reference_edges`（文件级标识符 ∩ 全局符号名的近似边，
  `Resolution=lexical`；R1 的作用域信息可用于将局部绑定排除出
  近似边）。边表随 `IndexerVersion` 重建。
- `internal/runtime/agent/repository`：目录排名从符号数改为
  符号重要度聚合；符号重要度由 Personalized PageRank 计算，
  damping 取 0.85（Brin & Page 1998 标准值，文档记录出处），
  迭代上限与收敛阈值为公开配置字段。种子集合 = 构建清单
  （现有 `IsBuildManifest`）+ 入口文件检测（现有硬编码表降级为
  回退）+ 用户 Pin 文件。token 预算二分适配沿用现有
  `repo_map` 分区裁剪，不新增预算机制。
- 协议影响：仅 prompt 内渲染变化，无 Operation/Event 变更。

**Security 与 Persistence 影响**。纯派生计算，无副作用工具、
无新权限。

**Cancel/Failure/Recovery**。图构建失败（语言解析不完整、文件超限）
时 Repo Map 回退现有符号数排名；排名计算有迭代与结果上限，
保证大仓库构建时间有界。

**Test 与 Rollout**。fixture 仓库验证 import 解析正确率；
排名稳定性测试（同输入同排名）；map token 预算裁剪边界测试；
回退路径测试。Shadow 模式先输出新 map 供对比，不直接替换。

### R3 常驻 LSP 会话

**用户问题与可衡量结果**。每次 `search_definition` /
`search_references` 冷启动一个 language server，秒级延迟使语义
查询在实践中很少被触发。完成后：同语言连续语义查询延迟降到
交互可用；`Resolution=lsp` 的使用率可在 Trace 中统计。

**所属 Package 与 Protocol 影响**：

- `internal/adapter/lsp`：从一次一进程改为按
  （workspace × language）的会话池；生命周期由现有 Process Broker
  托管（启动、健康检查、空闲回收、Runtime 关闭时终止）。
- 服务器发现沿用 `servers.go` 清单（gopls/clangd/rust-analyzer/
  pyright/typescript-language-server/jdtls），升级为配置字段
  （显式 server 路径或禁用名单），默认关闭常驻模式，
  显式配置启用——与 stdio MCP 的 `host_trusted` 治理语义一致：
  常驻 server 是宿主进程，必须显式授权。
- 结果缓存：definition/references 按
  （查询、文件 digest）缓存跨 Turn 复用；诊断经 didOpen/didChange
  推送，接入现有 `Authority.ObserveToolResult` 的 diagnostic
  来源（Working Set 的 `SourceDiagnostic` 权重已存在）。
- 资源上限：单 workspace 并发 language server 数、空闲回收超时、
  缓存容量均为公开配置字段（默认值以资源回收策略记录 provenance，
  如"空闲回收默认 10 分钟"写入配置文档并附理由）。
- 协议影响：`search_definition`/`search_references`/
  `lsp_diagnostics` 返回体增加 `resolution` 与缓存命中说明；
  无新 Operation/Event。

**Security 与 Persistence 影响**。常驻进程运行在 workspace 沙箱
边界内，与 `exec_command` 同一进程治理；无网络授权（语言服务器
本地工作）；凭证目录不开放（沿用现有沙箱投影）。缓存为进程内
派生状态，不持久化。

**Cancel/Failure/Recovery**。server 崩溃自动重启一次，再失败则
该语言本 session 降级 `Resolution=lexical/syntax` 并在
Diagnostics 可见；Turn 取消不终止会话（会话生命周期绑定
Runtime，不绑定 Turn）；Runtime 关闭时随 ResourceStack 终止。

**Test 与 Rollout**。会话生命周期测试（启动/复用/空闲回收/关闭）；
崩溃降级测试；缓存一致性测试（文件变更后缓存失效）；未启用配置时
行为与现状完全一致（默认关闭，无行为漂移）。

### R4 依赖图驱动影响分析

**用户问题与可衡量结果**。`search_related_tests` 是命名约定，
改一个符号找不全跨文件调用者与测试，Hard 验证门禁的覆盖判断
不可靠。完成后：`search_related_tests` 返回基于引用闭包的结果
并附边证据；Hard + MustPass 模式下受影响测试集由图派生。

**所属 Package 与 Protocol 影响**：

- `internal/persist/repoindex/related.go`：`RelatedTests` 升级为
  两级实现——引用图可用时按"改动符号 → 反向引用闭包（BFS，
  深度与结果上限为公开配置字段）→ 命中文件 → 测试文件判定
  （复用现有惯例表 + manifest 测试目录）"计算，每条结果附
  边证据（跳数、引用位置、Resolution）；图不可用时回退现有
  命名约定并显式标记 `fallback=convention`。Rust 首次获得
  受影响测试能力（依赖 import 边而非文件名）。
- `internal/adapter/tool/search/symbol.go`：`search_related_tests`
  返回体升级；`TestMapper` 接入验证门禁：Hard 模式下 Receipt
  Runner 以图派生受影响测试集核对 `covered_paths`。
- `internal/observability/verify`：Execution Receipt 的验证
  尝试记录影响来源（graph/convention）与图版本
  （IndexerVersion + digest）。
- 协议影响：Receipt 结构性扩展（新增字段），走生成流程与
  兼容检查。

**Security 与 Persistence 影响**。只读图查询，无新副作用。
Receipt 字段扩展属于可观测平面。

**Cancel/Failure/Recovery**。图查询超时或超上限时按部分结果
返回并标记截断；验证门禁在图不可用时维持现状（模型自报证据
对账），不因图故障阻塞 Turn。

**Test 与 Rollout**。fixture：跨包调用、间接调用（两跳）、
测试文件命名不规则（命名约定会漏而图能找到）的对照用例；
闭包上限与截断标记边界测试；fallback 路径测试；Rust 用例。

### R5 验证命令发现（未排期）

> 2026-09 评审决定不排期：显式 `verify.command` 配置已覆盖主要
> 场景，候选发现的价值不足以抵消新增来源解析面与呈现接线的
> 成本。以下设计保留作为重启时的参考；重启以独立提案为准。

**用户问题与可衡量结果**。`verify.command` 需要手工配置且只有
一条，未配置时验证门禁完全依赖模型自报。完成后：Runtime 从
仓库事实派生命令候选列表，用户一键确认或显式配置覆盖；
`verify.command` 未配置的 workspace 中验证参与率可测量提升。

**所属 Package 与 Protocol 影响**：

- 新增发现器（建议落位 `internal/persist/repoindex/discovery.go`
  或独立 `internal/platform/verifydiscovery`，实施时按
  "平台能力 vs 持久化"归属评审）：输入为已有 `IsBuildManifest`
  检测的 manifest 集合与 CI 配置，来源包括
  Makefile target（名称语义：test/vet/lint/check）、
  package.json scripts、Cargo target、`go test` 惯例展开、
  CI workflow 的 run 步骤（`.github/workflows` 等）。
  每个候选携带 provenance（来源文件、行、digest）。
- 候选缓存键控来源文件 digest，文件变更即失效。
- 优先级：显式 `verify.command` 配置 > 用户在 Web 确认的候选 >
  模型在 Turn 内选择（仍走 `exec_command` 全部治理）。
  发现的命令**不自动执行**，不存在"发现即运行"路径。
- 配置：`verify.discovery.enabled`（默认开启只读发现，
  执行仍需确认）、来源类型开关。
- 协议影响：候选呈现经现有 Web 查询面（如 Session/Artifact
  Service 的 Host-facing Query），若需新增 Unary Route 则走
  `contract.go` 清单 + `webprotocolgen` 生成。

**Security 与 Persistence 影响**。发现是只读解析；CI 配置与
Makefile 视为不可信文本，解析结果仅作候选呈现，不进入指令流
（防注入：候选执行前经 Guard 审批，与任意 exec 相同）。
持久化为派生缓存。

**Cancel/Failure/Recovery**。解析失败跳过该来源；候选列表为空时
维持现状；缓存失效重算有界（来源文件数上限为配置字段）。

**Test 与 Rollout**。各来源类型解析 fixture（含恶意命令样例仅
呈现不执行的断言）；缓存失效测试；优先级测试。

### R6 仓库工作记忆（跨会话派生事实）

**用户问题与可衡量结果**。新会话对同一仓库从零探索；已验证的
构建/测试拓扑、已读结论无法复用。完成后：新会话首个 Turn 的
Repo Map 与验证候选包含上次会话的已验证事实；重复读取率
（Event Log 可统计）跨会话下降。

**所属 Package 与 Protocol 影响**：

- `internal/adapter/memory`：记录结构扩展 `kind=derived_fact` 与
  `provenance{origin, workspace_revision, digest}`。派生事实
  与用户记忆（`remember` 工具写入）同库不同源：
  - 结构事实（origin=repoindex）：入口点、测试拓扑、命令候选，
    随索引重建刷新，scope=repository；
  - 验证事实（origin=verify_receipt）：某命令在某 workspace
    revision 验证过某路径集，scope=workspace，revision 变更后
    标记 stale（不删除，保留历史可查）；
  - 探索结论（origin=exploration）：已读路径与结论摘要，
    scope=workspace，复用现有过期时间语义。
- 写入路径全部为系统事件驱动（索引完成、Receipt 提交），
  **模型无直接写工具**——`remember` 仍只写用户记忆，防止模型
  污染派生事实面。
- 注入：经现有 Turn Admission 进入 Truth Capsule / `session_state`
  分区（Runtime 生成，非模型写入）；准入按结构键（符号/路径）+
  新鲜度 + 显式 Generation 冻结，词法相关性仅作补充排序。
- 协议影响：Memory 记录结构扩展需同步 `docs/protocol` 生成物
  （若记忆结构在公开契约中）与 Web 投影。

**Security 与 Persistence 影响**。派生事实为非权威数据面，
不进入 Authority Digest；写入经 Memory Store 现有事务边界。
stale 事实不注入上下文。审计可解释每条注入事实的来源。

**Cancel/Failure/Recovery**。写入失败不影响触发它的系统事件
（索引完成、Turn 提交）结果；记忆损坏或版本不识别时整体忽略
派生分区，回退无记忆行为。

**Test 与 Rollout**。跨会话复用集成测试（两个 Session 同仓库）；
stale 失效测试；模型不可写入派生事实的负向测试；准入预算测试。

## 5. 交付顺序

```text
M1   R1 通用提取层：Tier 0 通用引擎 + Tier 1 六语言迁移与 C/C++ 增补
M2   R2 引用图 + Repo Map 排名          （依赖 M1 的引用标识符）
M3   R3 常驻 LSP                        （独立，可与 M2 并行）
M4   R4 影响分析 + 验证门禁             （依赖 M2 的边表）
M6   R6 仓库工作记忆                    （依赖 M1/M4 的 provenance 链）
```

每个里程碑独立可验收、可回退（配置开关关闭后行为与现状一致），
不允许跨里程碑的大提交。M1/M2/M4 是主线；M3 提供独立价值可并行。
Tier 2 的基础语法提取已独立接入现有索引，见[交付范围](./code-structure.md)。
R5（验证命令发现）仍未排期。

## 6. 配置字段清单（新增）

所有字段进入 [配置说明](./configuration.md)，默认值附 provenance，
校验与边界测试随里程碑交付：

| 字段 | 默认值 | 默认值来源 |
| --- | --- | --- |
| `repoindex.max_files`（现有内部值公开化） | 20000 | 公开化时以现有内部上限为默认并补文档 |
| `repoindex.signature_max_bytes` | 512 | 显式资源策略，边界测试锁定 |
| `rank.damping_factor` | 0.85 | Brin & Page 1998 标准值 |
| `rank.iteration_limit` / `rank.convergence_threshold` | 100 / 1e-6 | 数值稳定标准值，文档记录 |
| `lsp.resident_enabled` | false | 宿主进程默认关闭，显式启用 |
| `lsp.idle_timeout` | 10m | 资源回收策略，文档记录理由 |
| `lsp.max_servers_per_workspace` | 2 | 资源上限，边界测试锁定 |
| `impact.max_depth` / `impact.max_results` | 3 / 200 | 公开合同字段，文档记录 |
| `verify.discovery.enabled` | true（只读发现） | 执行仍需确认，发现无害 |
| `memory.derived_expiry`（探索结论） | 7d | 复用记忆过期语义 |

## 7. 风险与明确不做

**风险**：

1. Tier 0 通用启发式在声明形态不规则的语言上产生噪音符号——
   噪音符号携带 `Resolution=heuristic` 如实分级，下游（Repo Map
   排名、影响分析）按置信度加权；用户可按语言关闭 Tier 0。
   Tier 2 使用固定版本的纯 Go 解析器增强主要语言；解析器维护和 grammar
   正确性风险仍存在。通过内部适配、语言级回归与解析失败降级约束影响，
   不把第三方宣称的全部语言自动加入支持范围。
2. 词法近似引用边假阳性（同名符号）——边携带 Resolution，
   下游（排名、影响分析）按置信度加权，不冒充精确；R3 LSP 命中时
   可校正。
3. 常驻 LSP 资源占用——默认关闭 + 空闲回收 + 并发上限配置。
4. 大仓库索引耗时上升——沿用现有分批与限速机制，语法解析失败
   逐文件降级，不阻塞首查询。
5. 派生记忆污染上下文——注入预算受现有 Economic Admission 约束，
   stale 即不注入，模型无写路径。

**明确不做**：

1. 不引入 embedding / 向量检索（维持 roadmap 承诺；待
   Event Log 评估数据证明显著收益后另立提案）；
2. 不做需要类型系统/编译器的精确引用解析（LSP 补位，不冒充）；
3. 不替换词法提取器（永久保留为兜底层）；
4. 不为索引引入网络或远程服务依赖。

## 8. 验收清单（映射 roadmap 验收规则）

| 规则 | 本提案的满足方式 |
| --- | --- |
| 可从受支持 Host 到达 | 工具返回升级、Repo Map、验证候选、记忆注入均在本机 Web 会话内可见 |
| 已定义安全与失败行为 | 每个工作流的 Failure/Recovery 小节；常驻进程默认关闭；发现命令不自动执行 |
| 测试覆盖契约 | 每个工作流的 Test 小节 + `testdata` fixture；置信度分级有锁定测试 |
| Observability 或 Receipt 可检查 | Resolution 随工具结果返回；Receipt 记录影响来源与图版本；Trace 统计 Resolution 使用率 |
| 中文文档已更新 | 见第 9 节 |
| Release/Support 声明与动态证据一致 | 里程碑验收记录对比数据（Shadow 模式基准）写入实施记录 |

## 9. 文档变更清单

实施时同步更新：

- [配置说明](./configuration.md)：第 6 节全部新字段；
- [使用指南](./usage.md)：`search_symbol`/`search_related_tests`
  返回证据、验证候选确认流程；
- [架构设计](./architecture.md)：仓库智能章节（置信度阶梯、
  派生数据面、常驻进程治理）；
- [Agent 指南](./agent-guide.md)：仓库理解工具的 Resolution 语义；
- [后续规划](./roadmap.md)：中期条目链接本提案；
- `docs/protocol` 生成物：仅当 Receipt/Memory 结构进入公开契约时
  按仓库命令重新生成。

## 10. 实施记录（随里程碑追加）

- [x] M0 依赖选型记录（含当前实现选择）：

  | 候选 | 版本 | 结论 |
  | --- | --- | --- |
  | `github.com/tree-sitter/go-tree-sitter`（官方） | v0.25.0 | 权威维护；**cgo**，且不带 grammar，每语言需单独绑定包；增加跨平台构建与分发成本，本次未采用 |
  | `github.com/kreuzberg-dev/tree-sitter-language-pack` | v1.18.0 | 语言覆盖广、迭代活跃；cgo 且**首次使用需联网下载预编译解析器**，违反本地无网络边界。排除 |
  | `github.com/odvcencio/gotreesitter` | v0.52.0 | 纯 Go、grammar 内嵌。早期评估因公开 API 边界和维护集中度风险否决；当前实现为满足 Go 多语言提取选择固定此版本，相关风险仍保留，由内部适配与回归测试限制接入范围 |

  附加事实与修正：提案调研阶段引用的 "wasilibs/go-tree-sitter"
  经核实不存在，已从 R1 移除。R1 原定的"六语言"范围为现状继承
  （`internal/platform/symbols/symbols.go` 语言常量表），
  2026-09 评审否定了"按需渐进扩展语言包"作为语言策略——
  面向真实仓库的 Runtime 必须任何语言都有符号层基线，
  **语言广度取代"纯 Go 构建"成为 Tier 2 选型的第一决策标准**
  （纯 Go 构建降为第二标准，macOS 自包含发布仍为约束）。
  R1 已据此重写为"Tier 0 通用启发式（任何语言基线）+
  Tier 1 语言精调包（C/C++ 列入首批）+ Tier 2 语法级（M1.5
  独立决策）"的分层覆盖模型；wazero 包装官方 WASM grammar
  因同时满足语言广度与纯 Go 构建，升入 M1.5 必查路线。

  当前状态：M1 的词法与启发式基础继续保留；Tier 2 的主要语言基础提取已接入。
  本次实现采用固定版本的 gotreesitter，限定为经过仓库样例验证的 11 种语言及
  JSX/TSX，不引入 CGO、额外进程或运行时 grammar 下载。早期对依赖维护集中度
  和公开 API 过宽的疑虑仍成立；QCode 只通过 `internal/platform/symbols`
  的内部适配使用它，升级必须重新运行多语言、取消、竞态与 macOS 双架构构建检查。
  行为与限制以[代码结构提取](./code-structure.md)为准。

- [x] M1 通用提取层（2026-09-12 交付）：

  按分层模型实现，零新依赖。交付内容：

  - `internal/platform/symbols` 分层重构：Tier 0 通用引擎
    （`generic.go`，泛语言类型关键词 + 声明形态 + 前置注释
    docstring，`Resolution=heuristic`）、Tier 1 语言包
    （`languages.go` 六语言迁移 + `cpp.go` C/C++ 增补，
    `Resolution=lexical`）、共享 detail 层（`detail.go`：
    签名拼接、docstring、文件级引用标识符收集，全部受
    Options 字节/数量上界约束）；`DetectLanguage`（扩展名 +
    shebang，未知语言进 generic 档）；扩展名表从 12 项扩到
    60+ 项（C/C++/C#/Ruby/PHP/Kotlin/Swift/Scala/ObjC/
    Shell/Lua/Elixir/Zig/Dart 等，无规则表的语言走
    Tier 0 并保留语言名）。
  - `internal/persist/repoindex`：`IndexerVersion` 1→2；
    Symbol 行新增 signature/docstring/resolution；新表
    `repo_index_references`（文件级标识符计数，R2 建边的
    原料）；`WeakestResolution` 聚合。
  - `internal/persist/state/sqlite`：`ensureRepositoryIndexShape`
    按列集检测旧形状并 DROP 重建（cache 表形状演进不进
    user_version 迁移链，测试锁定）。
  - 配置：`context.index.signature_max_bytes`（默认 512）、
    `docstring_max_bytes`（2048）、`reference_max_count`
    （4096），全套 provenance/env/TOML/override/校验接入，
    见[配置说明](./configuration.md)。
  - 消费面：`search_symbol` 返回 signature 与逐行
    resolution；Repo Map outline 渲染签名，heuristic 行
    显式标注。
  - 测试：六语言迁移等价性（身份字段逐项锁定）、Tier 0
    （Kotlin/未知缩进语言/控制流排除）、C/C++（类/命名空间/
    模板/析构/Allman/访问标签/原型索引）、detail（多行签名/
    docstring 边界/引用计数与上界）、repoindex round-trip、
    sqlite 旧库重建。

  实施取舍（与提案的偏差，均已按更诚实的建模执行）：

  1. references 是文件属性而非符号属性，落在
     `ExtractResult.References` 与 `repo_index_references`
     表，不挂在 Symbol 结构上；
  2. C++ 头文件原型与 `.cpp` 定义同时索引（两条行、签名可
     分辨），放弃提案初稿"原型跳过"的设定——搜索语境下多一
     条优于漏一条；
  3. 未做运行时 Shadow 并行对比开关：六语言等价性由测试
     锁定，回退路径是 IndexerVersion 再提升（cache 语义），
     配置开关判定为冗余；
  4. docstring 已入索引但消费面（工具结果/Repo Map）暂不
     返回正文，只返回签名——token 预算归 M2 渲染策略统一
     决定。

  验收数据（QCode 自举冒烟）：`languages.go` 45 符号 /
  `store.go` 33 符号，身份与签名正确；C++/Kotlin fixture
  见 `tiers_test.go`。跨会话重复读取率等指标依赖 M6 记忆
  注入后才有意义，移至 M6 验收。

- [x] M2 引用图与 Repo Map 排名（2026-09-12 交付）：

  交付内容：

  - `internal/persist/repoindex/imports.go`：六种 Tier 1 语言的
    import 说明符解析（Go 单行/分组、TS/JS from/require/副作用
    导入、Python 绝对/相对、Rust `crate::`、Java 含 static、
    C/C++ 引号 include；尖括号系统包含是 external 不建边）。
    说明符在 scan 时**原样记录**（`repo_index_imports` 表），
    图构建期在全量文件集上解析——并发刷新中途的文件集不完整，
    原样记录是唯一诚实的时机分离。
  - `internal/persist/repoindex/graph.go`：`repo_index_edges`
    边表（import 边 + reference 近似边，后者由标识符计数 JOIN
    符号表面成，权重为使用次数）与文件级 Personalized
    PageRank（写入 `repo_index_file_rank`）。Go 包导入展开到
    包目录下全部已索引文件。**确定性是合同的一部分**：节点
    按路径序、边按固定序、浮点累加随之有序，同仓库必然同
    排名（测试锁定八次重算逐位相等）。悬挂节点质量按
    personalization 分布回流，总质量守恒为 1（测试锁定）。
  - 入口分类从 Repo Map 的本地表移入索引（`repoindex.EntryPoint`），
    与 `IsBuildManifest` 同模式——map、图种子和未来消费方对同一
    问题只有一个答案。
  - `internal/runtime/agent/repository`：目录排序从声明数改为
    聚合 rank（rank 相同才落到声明数、文件数、路径），聚合按
    路径序累加保证确定性；rank 全零（图未构建/失败）时自然
    回退旧排序（测试锁定）。
  - 配置：`context.index.rank_damping_factor`（0.85，Brin &
    Page 1998）、`rank_iteration_limit`（100）、
    `rank_convergence_threshold`（1e-6），全套 provenance 接入。
  - `IndexerVersion` 2→3；cache 形状清单覆盖三个新表。

  实施取舍（与提案的偏差）：

  1. PageRank 在**文件级**而非提案原文的符号级图上运行：图的
     消费方（目录排名、R4 影响分析、未来边查询）都是文件/
     目录粒度，符号级图在 2 万文件上限下成本不成比例；R2
     验收指标（"更高比例的被引用符号进入 map"）由文件 rank
     + outline 的既有行序共同达成。
  2. 种子不含构建清单：manifest 文件没有出边，作为种子无法
     传导（随机游走从它出发立即悬挂）；实现取入口文件为种
     子，无入口的库项目退化为均匀 PageRank——被依赖最多的
     文件最高，语义等价于"没有起点的同一问题"。Pin 文件在
     Build 消费端已由 outline 独立呈现，不参与排名。
  3. 提案初稿列出的 import 边语言未含 C/C++（M1 后补的 Tier 1
     语言）；`#include` 引号形态是最可靠的说明符之一，已一并
     交付。

  验收数据：解析/解析候选/确定性/种子行为/均匀回退/端到端
  （imports→edges→rank→Files 回填）/消费面（rank 选择目录、
  无 rank 回退声明数）全部由 `graph_test.go` 与 `map_test.go`
  锁定；QCode 自举的 map 定位轮次对比依赖真实模型会话，归入
  M6 前的独立评测（提案 P2 评估框架）。
- [x] M3 常驻 LSP 会话（2026-09-12 交付）：

  交付内容：

  - `internal/adapter/lsp/resident.go`：按 server 二进制的常驻会话池，
    实现 `symbols.Provider`。会话复用（initialize/didOpen 一次），
    文档增量同步（digest 未变跳过、变更发 didChange），查询结果
    按（server、方法、位置、文件 digest）缓存（容量上限 FIFO
    淘汰），空闲 janitor 回收，`Close` 幂等并清理全部会话。
  - **查询串行化**是正确性前提：rpcClient 丢弃 id 不符的响应，
    同一会话上的并发调用会互吞答案——会话锁覆盖从 didOpen 到
    call 的完整交换（并发测试锁定 8 路突发全数成功）。
  - 崩溃语义：会话死亡或查询失败计一次崩溃，**成功应答**才清零；
    连续两次失败后该 server 在池生命周期内禁用，调用方落回
    词法索引（search 工具既有 semanticFallback 链路）。
  - 治理：默认关闭（`context.lsp.resident_enabled=false`），
    显式启用——常驻 server 是宿主进程，与 stdio MCP 的
    host_trusted 同一信任姿态；进程创建沿用
    `process.NewCommand`（沙箱内、workspace 只读、拒绝网络），
    关闭经 `Session.RegisterResource` 进入逆序资源栈。
  - 配置：`idle_timeout`（10m，1s..1h）、`max_servers`（2）、
    `cache_capacity`（256），全套 provenance 接入。
  - 测试：复用（initialize/didOpen 各一次）、缓存（三次同查询
    一次 exchange）、didChange（编辑后增量）、单次崩溃重启、
    连续失败禁用、Close 幂等、并发串行化、config 默认关闭。

  实施取舍与裁剪：

  1. **诊断（lsp_diagnostics）不接池**：Analyze 的正确性依赖
     每次批量 didOpen 的完整状态，复用常驻会话需要跨查询的
     版本追踪设计；一次性进程的诊断路径保持现状，常驻化留作
     独立变更。提案中"诊断经 didChange 推送接
     ObserveToolResult"随之推迟。
  2. 提案所述"Resolution=lsp 使用率可在 Trace 中统计"未接线：
     会话池在 adapter 层，Trace span 归 turn 可观测平面，
     接线需要 engine 侧配合，留待有真实使用后再评估。
  3. `Session.RegisterResource` 是本次新增的模块资源注册入口
     （buildState 无资源栈，模块构造的宿主进程此前没有统一
     关闭路径）；签名与 ResourceStack.Add 对齐，逆序关闭。

  验收数据：交互延迟的量化对比依赖真实 provider 会话，归入
  P2 评估框架；fixture 测试以"第二次查询零 initialize/didOpen"
  锁定冷启动成本的消除。
- [x] M4 依赖图驱动影响分析（2026-09-12 交付）：

  交付内容：

  - `internal/persist/repoindex/impact.go`：`Impact` 反向依赖闭包
    （BFS，跳数即最短路径；返回逐文件跳数与末跳边类型，
    结果上限截断显式报告）；`RelatedTests` 升级为两级合并——
    图路线（闭包内测试样文件，带跳数与边证据）优先，
    命名惯例补充图未覆盖的部分，双路命中保留图证据
    （更具体的声明）。`TestMapper` 随之返回带证据的行；
    `Paths` 提供纯路径投影。
  - 图路线的测试判定是 `looksLikeTest`：命名惯例（IsTestPath）
    **加**目录约定（tests/、test/、__tests__/、spec/）——图路线
    的意义正在于到达命名惯例配不上的测试文件（对照用例：
    tests/odd_name.py 引用 core 的符号即被命中），代价是
    tests/ 下的 helper 也会入列，由 Resolution=graph 公开可辨。
  - Rust 经图路线首次获得受影响测试能力（命名惯例不支持）。
  - 工具 `search_related_tests`：tests 逐条带 path/hops/via/
    resolution，答案级 `impact_source`（graph / convention /
    graph+convention）。
  - 配置：`context.index.impact_max_depth`（3）与
    `impact_max_results`（200），全套 provenance 接入。
  - 测试：闭包跳数与边类型、双路合并与图证据优先、
    命名不规则对照、Rust、截断、图静默时惯例独立工作、
    同名词法噪音的公开携带（断言其 Via=reference）。

  实施取舍与裁剪：

  1. **验证门禁接入裁剪**：提案承诺的"Hard + MustPass 下
     Receipt Runner 以图派生测试集核对 covered_paths"未交付。
     现状核查发现 `TestMapper` 并无生产消费方，且门禁的
     affected scope 语义是"改动路径的验证覆盖"而非"测试集
     覆盖"——引入测试集对账是**新的门禁行为**，需要独立的
     turn 行为变更评审（该区域另有进行中的工作），不应在
     M4 内默认引入。RelatedTests 的质量提升已使未来接线
     即受益。Receipt 影响来源字段随门禁裁剪一并推迟。
  2. 现已补充最短文件依赖链；作用域候选边携带一个代表性的引用位置，
     工具核对 digest 后附源码片段。普通 import 和旧同名边仍不虚构行号。
     多个改动来源分别归因，深度/数量截断及证据缺失显式报告，
     详见[代码结构提取](./code-structure.md)。这仍是文件级推荐，不是函数级覆盖证明。
  3. `IndexerVersion` 不变：边表结构未动，RelatedTests 是
     查询时计算。

  验收数据：`impact_test.go` 的对照断言即召回对比（命名惯例
  漏掉而图找到的 odd_name.py/integration.rs 用例）；真实仓库
  的召回率对比归入 P2 评估框架。
- [ ] M6 验收数据（跨会话重复读取率）
