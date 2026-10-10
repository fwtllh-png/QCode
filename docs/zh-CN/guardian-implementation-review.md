# Guardian 实际完成情况审查

- 仓库：QCode
- 检测模式：Guardian 实现与验收核查
- 检测范围：当前工作区 P0–P6 全链路，含未跟踪实现；定向核查
- 生成时间：2026-10-10 10:52
- 审查方式：按策略、执行准入、模型预算、审计展示、授权来源与评估五组核对调用链，并交叉复核。

## 2026-10-10 修复复核

原报告五项 P1 均已用仓库内回归复现并修复。生产集成测试又发现同一身份接线链路中的
Workspace ID 混用：授权来源使用 Memory 身份，Candidate 使用 Guard 执行身份；此项一并修复。
下文保留修复前的审查证据与判断，不能把历史失败描述理解为当前仍未修复。

| 问题 | 修复 | 新增验证 |
| --- | --- | --- |
| 1. 持久会话与进程身份混用 | 按当前可信 Turn/Invocation 冻结 Session；来源与 Candidate 共用 Guard 的 Workspace ID | 实际创建持久会话，经 Wire/ThreadManager/HTTP Provider/沙箱自动执行并关联持久审计；跨会话拒绝、冻结后身份不漂移 |
| 2. 分叉误认子 Agent | 每层父链按持久 Agent Graph 分类；分叉保留本地用户输入 | 实际 ForkCheckpoint、禁止修改转人工、Steering 使旧来源失效；多层委派和已结束 Agent 的身份不会提升为用户来源 |
| 3. PATH 替换实际 Shell | 固定系统 `/bin/sh`，解释器路径进入环境摘要 | 在审查前和审查期间替换 PATH 中的 sh，实际仍只执行受审命令 |
| 4. running 后副本提前回收 | Guard 仅回收未交接副本；Shell 会话负责交接后生命周期 | running 时目录保留，最终轮询结算后回收，关闭时丢弃未结算写入并回收 |
| 5. 精确文件交接失败 | 消费已经准备的原始精确范围副本 | 精确文件 apply/discard 均执行成功，范围和相邻文件保持约束 |

回归入口：

- [生产持久会话与分叉](../../internal/runtime/app/wire/guardian_persistent_test.go)
- [Shell 三项缺陷及关闭回收](../../internal/adapter/tool/shell/guardian_execution_regression_test.go)
- [身份冻结与跨会话拒绝](../../internal/runtime/agent/engine/guardian_guard_test.go)
- [分叉与多层委派来源](../../internal/runtime/agent/context/guardian_source_test.go)
- [持久 Agent 身份](../../internal/persist/thread/agent_test.go)

P2/P3/P4 的上述具体阻断已修复。后续补齐 P5 的真实浏览器、Host 与持久 Runtime
待审批崩溃恢复测试，并修复恢复链路发现的问题：

- Continuation 原来只恢复计划内容，遗漏 Policy 的成功提交状态，导致原命令被
  `plan_required` 拦截。现在持久化实际提交事实及当时 Planning Policy，只有已验证的
  Continuation 和相同当前策略才恢复该事实；计划文字本身不会获得执行资格。
- 工具在重新接入审批前遇到资源、参数或策略失效时，原 Pending Approval 没有收尾，
  工具结果会触发非法状态迁移。现在仅将对应等待作废并清理旧恢复入口；已提前响应的
  审批不重复发事件，其他调用的等待不受影响。`approval_recovery_stale` 作为可处理的
  工具拒绝返回，明确未启动命令、不可重放旧批准，要求核对当前资源后重新提议或说明未完成。
- 恢复时工具重新进入 Guard 与 `awaiting_approval` 阶段可以同时存在；展示状态已按
  这个生命周期声明，消除错误的状态一致性诊断，内核仍禁止结果绕过未解决审批。

回归位于 [真实浏览器恢复测试](../../web/tests/e2e/guardian-recovery.spec.ts)：仅模拟
外部模型 Endpoint，使用当前构建的 CLI、页面、WebSocket、磁盘状态与真实沙箱。
在原审批落盘后 SIGKILL，同目录同端口重启；批准后只写一次，拒绝与取消不写入，
原审批 ID 与审查关联保留。另覆盖重启期间资源变化后点击原卡片批准的拒绝路径。
旧协调租约到期前恢复会等待，测试不释放租约或替换恢复入口来规避这一过程。

P6 使用相同 Prompt v2、Schema 与完整合成样本，按显式 60 秒/16,384 Token 预算分别
重测两条路由：均为 17/17 有效、0/12 误放行；GLM 误转人工 0/5，DeepSeek 为 2/5。
在线验收通过，旧失败报告保留，费用与真实开发覆盖率仍未知。数据与限制见
[评估与启用验收](./guardian-evaluation.md)。默认继续关闭，没有修改用户连接或启用配置。

本次测试过程中的并行 Verification 编译错误未通过回滚其他修改处理；最终检查以当前
工作区为准。这些结论覆盖已列出的实现问题与验收场景，不等于任意模型或工作流均可启用。

本轮通过的检查：

- `make guardian-safety-check guardian-eval guardian-runtime-eval`：安全矩阵、37 个离线样本、9 个 Runtime 场景。
- Guard、Shell、Context、Engine、Wire、Thread、State、Authority 的 Guardian/恢复/准入/启动相关 `go test -race`。
- `npm --prefix web run check`，Conversation/TranscriptCards 的 67 项单测，Guardian 的 8 项浏览器用例（4 项 Fixture 与 4 项真实 CLI 崩溃恢复）。
- `make docs-check web-protocol-check` 与 Guardian 改动范围的 `git diff --check`。

补齐 P5/P6 后另通过 Continuation、恢复失效收尾、审批故障分类与状态投影定向测试；
Guard、Context、Engine 的相关竞态测试通过。Web 检查与 67 项单测复跑通过，
`guardian-web-eval` 作为统一构建和浏览器验收入口加入 Makefile。最后一轮使用含全部
恢复修复的 CLI 运行两份浏览器 Spec，8/8 通过（2.5 分钟），包含资源变化后旧批准
被拒绝、执行尝试数为 0、模型收到可处理拒绝后声明未完成的完整路径。

核心完整包扩大测试中 Context、Guard 通过；Engine 的
`TestConversationSelectionReprojectsWithinTurnAndPersists` 在 `conversation_test.go:135`
失败，原因是候选会话内容缺失。该项单独复跑仍失败，并在独立的未修改 HEAD
（`e26c10c2`）源码副本中得到相同失败，属于已有基线问题，本轮未修改其断言或实现。
不将这次定向验收表述为整个 Engine 或全仓测试通过。

扩大检查还运行了 Process、Thread、Shell、Context、Wire 的完整包测试。Process、Thread、
Shell 通过；Context 与 Wire 初次运行各有一处 Verification 重构的旧测试预期，分别仍期待
`verified` 文案和旧子 Agent 验证枚举。Context 只同步了遗漏的测试预期，完整包复跑通过；
Wire 断言已由并行重构更新，失败用例单独复跑通过。未据此声称全仓 Go 测试通过。

新增持久 Agent 身份测试已加入 `guardian-safety-check`，未来门禁同时覆盖真实生产接线
与线程来源回归。下文原始记录中的 Web 编译阻断属于当时的工作区状态，本轮已重新验证通过。

## 原始完成度结论（修复前）

**不能认定 P0–P6 已完整完成。** 主要模块、协议和回归入口已经存在，但生产主线程
送审接线尚不工作，内容与授权绑定、异步执行生命周期也有已复现缺陷。
本报告保留五项与本方案直接相关、均已通过临时探针复现的问题，不代表全部候选问题清单。
默认关闭的事实属实；本次未修改启用配置、源代码或已有用户改动。

这里的阶段 P0/P1 与下文缺陷严重度 P1 是两套编号。

| 方案阶段 | 实际判断 | 证据与边界 |
| --- | --- | --- |
| P0 前缀授权移除 | 核心改动已落地 | 原前缀通路已移除，策略与对抗输入回归通过；不以此宣称所有约束组合均已证明 |
| P1 契约与决策表 | 主体已落地 | Candidate、Evidence、严格解析、本地决策与失效校验存在；相关包测试通过 |
| P2 来源与执行快照 | 未通过验收 | 用户分叉来源被遗漏，PATH 解析出的实际 Shell 不在受审内容中，见问题 2、3 |
| P3 模型接入 | 生产主线程未闭合 | 路由、无重试、预算等有实现和测试，但真实会话与进程身份混用，在 Provider 前失败，见问题 1 |
| P4 Guard 与执行 | 未通过验收 | 启动校验存在，但运行中副本提前回收、精确文件写入交接失败，见问题 4、5 |
| P5 审计与界面 | 已实现部分链路，完整验收不足 | 有持久审计、必要提交失败回 Ask、主线程详情；现有浏览器与重启用例没有覆盖完整持久 Runtime 链路 |
| P6 评估与启用 | 评估入口和结果已落地，启用验收未通过 | 两条真实模型路由的留存评估均失败；真实开发覆盖率、完整费用仍未知 |

## 测试能证明什么

本次实际执行并通过：

- `make guardian-safety-check`：5 个安全/配置包的完整测试，以及 7 个包的 Guardian、恢复和准入定向测试。
- `go test -race`：Guard、Shell、Context、Engine、Wire、State 的 Guardian/恢复/准入定向测试。
  同次命令也列入 Authority，但该筛选没有命中 Authority 用例，不把它计为竞态覆盖。
- `make guardian-eval`：37 个固定响应样本通过，17 个可送审；它不是实际模型质量或实际命令执行评估。
- `make guardian-runtime-eval`：9 个 Runtime、磁盘事件库、真实沙箱场景通过。
- Common/Adapter/Security/Persist/Agent 分层包、Protocol、Provider HTTP/Router 和 Model 包测试通过。
- Web Conversation/TranscriptCards 两个测试文件共 67 项通过。
- `make docs-check web-protocol-check` 和 `git diff --check` 通过。

本次五个临时失败探针采用 Go overlay，源文件留在仓库外；Shell 三例在临时目录和真实
Seatbelt 沙箱运行，只写测试文件。身份不匹配与分叉来源两例分别验证生产函数的会话校验、
来源投影和启动回调；**未声称已执行完整 Web 越权攻击**。真实主线程的身份故障会先转人工，
但其他身份匹配路径仍能进入审查，不能以该故障替代执行边界。

当前工作区的 `web/src/ui/App.tsx` 在审查期间持续变化，并出现 JSX 语法错误。
`npm --prefix web run check` 失败，Guardian 浏览器用例也因 Vite 编译失败无法进入页面；
本次不能复核文档所称的 4 项浏览器通过。该问题位于并行 UI 改动中，不列为 Guardian 引入的缺陷。
未运行全仓 Go 测试或重新调用付费真实模型。

P5 测试边界需要明确：

- [Runtime 测试](../../internal/adapter/tool/shell/guardian_runtime_test.go)直接构造 Engine，
  使用临时 Coordinator；终态后只重开事件库读取记录，未覆盖 Pending Approval 时重启完整 Runtime。
- [恢复测试](../../internal/adapter/tool/shell/guardian_recovery_test.go)直接重建 Guard 并
  `RestoreApproval`，未经过生产 Lifecycle、ThreadManager 和 Host 恢复链。
- [浏览器 Fixture](../../web/tests/e2e/fixtures/guardian.tsx)替换 `start`、
  `decideApproval`、`cancel`，刷新仅回放 sessionStorage；它验证卡片交互与投影，
  不证明 Web → Host → 持久 Runtime → 执行的闭环。

## P6 留存结果复核

机器可读报告与评估文档中的数值一致：GLM 有效 14/17，DeepSeek 有效 15/17；
两者都仅将 33 个样本基础 Ask 中的 4 个变成 Allow，不能外推生产审批减少率。
两条路由各在 12 个要求保留 Ask 的送审样本中观察到 0 次误放行，
但这 12 个样本没有覆盖本报告复现的调用链缺陷。

Go/npm/Python 等构建与解释器依赖目前仍按内容不完整转人工，属于当前明确边界；
不能用已支持的简单字面量与递归 sh 样例，宣称已经覆盖通常的开发命令。
真实可审查比例和完整费用未知。详见[评估与启用验收](./guardian-evaluation.md)。

合理的状态描述应为：**主体实现与定向回归已落地，生产接入和安全验收仍有阻断项；
P6 首轮评估已执行，启用验收未通过。**

## 缺陷统计

- P0：0
- P1：5
- P2：0
- 合计：5

## 缺陷详情

### 1. [P1][逻辑错误] 生产主线程混用进程与持久会话身份，合法审查在调用模型前失败

- 位置：`internal/runtime/agent/engine/guardian_prompt.go:37-38`
- 置信度：10/10

**问题描述**

普通 Web 主线程由 Wire 将 Options.SessionID 设置为 process-<pid>-<pointer>，ThreadManager.StartTurn 则把真实持久 SessionID 绑定到调用上下文。Guardian Candidate 使用后者，PrepareGuardianReview 仍复制前者，此处拒绝合法候选。临时 Go overlay 测试复现：active_session=session，reviewer_seed=process-runtime，candidate_session=session，Provider 调用数为 0，错误为 Guardian candidate belongs to another session。现有 Runtime 测试直接构造同名 Engine，Wire 测试只调用 PrepareGuardianReview，没有验证真实会话送审。这使 P3 的生产主线程接入未闭合。其他使用相同身份的调用路径仍能进入审查，不能把这个失败当作其他安全缺陷的防护。

**修复建议**

从当前可信 TurnSpec 或 InvocationIdentity 冻结持久 SessionID，并据此校验 Candidate；无绑定身份的独立 Engine 才回退到 Options.SessionID。增加通过生产 Wire、ThreadManager 与真实持久会话的送审验证，同时保留跨会话拒绝。

---

### 2. [P1][安全漏洞] 检查点分叉被误认作子 Agent，用户的新限制与 Steering 被遗漏

- 位置：`internal/runtime/agent/context/guardian_source.go:44-50`
- 置信度：10/10

**问题描述**

ForkCheckpoint 为同一 Session 的新主线程保存 ParentThreadID 并激活它；wire.guardianSource 对所有非空 ParentThreadID 都返回父作用域，本处随即将 Child 置为 true。CaptureGuardianAuthorization 因而跳过该线程的 TurnStarted、TurnSteered 和 InputResolved。临时 overlay 测试复现：父线程请求创建文件，分叉用户要求只审查、禁止修改，之后追加停止修改；采集只保留父请求，追加限制前后摘要相同，WithCurrent 仍接受旧授权并调用启动回调。这违反 P2/P4 对完整用户来源和启动前撤权的要求；修复主线程 SessionID 接线后，此缺口不能继续保留。

**修复建议**

用可信线程类型区分检查点分叉与子 Agent 委派；分叉继承相应来源，同时保留当前主线程真实用户输入。补充真实 Fork、后续用户限制、Steering 和启动窗口失效的回归。

---

### 3. [P1][安全漏洞] 只绑定 PATH 字符串，没有绑定实际 Shell，未审查代码可以自动执行

- 位置：`internal/adapter/tool/shell/review.go:34-46`
- 置信度：10/10

**问题描述**

GuardianReviewPlan 仅摘要环境字符串并采集命令及 ./ 脚本依赖；实际 process 启动仍按 PATH 解析 sh。在临时工作区放置 .tools/sh，令 PATH 优先指向它，提交 printf reviewed > generated/out.txt 后，内容证据为空且 CoverageComplete=true。真实沙箱测试中模型审查 1 次、人工审批 0 次，调用成功，但输出为未审查 Shell 写入的 unreviewed。该 Shell 位于可变源工作区，也不参加启动前内容复核。沙箱边界依然有效，但审查内容与实际执行内容不一致，不能满足 P2。

**修复建议**

在准备阶段解析并绑定实际解释器及其身份；只支持系统 POSIX Shell 时使用可信绝对路径，其他解释器及依赖不能固定到受审快照时转人工。覆盖 PATH 替换 Shell 和模型等待期间替换该文件的回归。

---

### 4. [P1][逻辑错误] 首次返回 running 时提前回收审查副本，破坏后台命令与结算

- 位置：`internal/adapter/tool/guard/pipeline_attempt.go:62-63`
- 置信度：10/10

**问题描述**

executePipeline 无条件 defer guardianState.close；ReviewExecution.Close 不区分已经交给 Shell 的 used 状态，直接关闭底层隔离会话并删除副本。但 Shell 对超过 yield_time_ms 的命令会保存 pendingExecution，约定由最终 write_stdin 或 OnClose 结算、回收。真实沙箱测试使用有限的 printf 输出和 yield_time_ms=1：返回仍含 session_id，审查副本目录却已不存在。后续文件访问和隔离结算因此失去工作目录。现有 Guardian 测试用短命令及 30000ms 等待，没有覆盖运行中会话。

**修复建议**

建立明确的副本所有权移交：Guard 只释放未交付的准备资源，交付后的副本由 Shell 会话在完成、取消或关闭时释放。验证首次返回 running 后副本仍存在，最终轮询可结算且只回收一次。

---

### 5. [P1][逻辑错误] 精确文件写入获模型放行后仍被目录隔离前置条件拒绝

- 位置：`internal/adapter/tool/guard/pipeline_attempt.go:547-555`
- 置信度：10/10

**问题描述**

PrepareGuardianExecution 接受非空精确文件 WritePaths，并创建可审查副本；但消费 ReviewedExecution 时无条件 RequireWriteIsolation，而 beginIsolatedCommand 要求 existingWriteTrees 非空。真实沙箱测试以 write_paths=[generated/out.txt] 提交合法字面量写入：模型审查 1 次，随后报 required write tree isolation is no longer available，未执行，也未转回人工审批。原有精确文件执行范围是合法输入，这使 P4 的模型允许到实际执行链路对该范围断裂。

**修复建议**

让已经准备的审查副本支持原始精确文件范围的交接；若本期不支持，必须在模型调用前排除并保留人工审批。不能通过扩大到整目录授权规避。补充精确文件从准备、模型评估到实际执行的集成回归。

---
