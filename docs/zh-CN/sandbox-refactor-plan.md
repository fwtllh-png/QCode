# Sandbox 审计修订与重构方案

状态：审计结论与分阶段重构合同。日期：2026-09-21。
依据：对最近真实 session（`thread_f28d0a4e…`，2026-09-21）的实机故障复盘，
以及对 `internal/security`、`internal/platform/process`、
`internal/adapter/tool/shell`、`internal/orchestration/execsettle`、
`internal/runtime/agent/turnkernel` 的代码审计。

本文是 [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)
（下称"环境契约"）的实施修订：环境契约确立的
"资源声明 + 授权 + 生命周期"方向正确，但当前实现与契约存在四处
结构性偏离，导致实机 session 中出现连续受难链。本文给出根因、
目标架构与分阶段修复合同。实机证据见
[内部依赖诊断](./sandbox-dependency-diagnosis.md)。

## 1. 审计结论（总览）

问题不是零散 bug，而是四个架构级错位的叠加：

1. **策略决策点选在"模型事前申报"而不是"运行时事实"。**
   网络目标要求事前枚举精确 `host:port:method`，且 HTTPS 必须声明
   `CONNECT`（`internal/adapter/tool/network_targets.go`）；verification
   用模型自报字段做事前一票否决（`internal/adapter/tool/shell/verification.go`）。
   事前无法可靠判定的性质被做成了事前硬闸门。
2. **语言生态知识硬编码进了通用层。**
   环境变量 allow-list 硬编码 16 个 `GO*`（`internal/platform/process/environment.go`）；
   `ensureGoToolchain` 兜底只护 Go；`internal/security/goproxy` 整包是
   GOPROXY 协议专用。与"适配器是加速器，不是语言准入门槛"的契约原则相悖。
3. **授权是一次性快照，无失效、无刷新、无继承。**
   `BuildPolicy` 进程生命周期只构建一次，`Lifecycle: "source_version"`
   是死字段；隔离结算后端重建 policy 时丢弃工具链与环境注入
   （`internal/orchestration/execsettle/service.go` 的 `SkipPATHReadRoots`）。
4. **拒绝不可见、不可恢复、不可归因。**
   OS 级 EPERM 以裸 errno 落在 stderr；goproxy 认证服务绑定失败静默
   返回 nil（`internal/runtime/app/wire/auth_service.go`）；上游 410 透传
   不记 Fact；`request_user_input` 超时后无人调用 `ResolveInput`，
   turn 状态机以非法迁移报废整轮。

### 1.1 实机故障链（最近 session 还原）

| # | 现象 | 根因（见第 3 节工作流） |
|---|------|--------------------------|
| 1 | `git: unable to access '~/.gitconfig': Operation not permitted` | HOME 重定向 sandbox-home，真实 HOME 不在 readRoots |
| 2 | `environment variable is not in the child-process allow-list` | 环境变量硬编码白名单，无补救通道 |
| 3 | `sh: go: command not found` | 隔离后端跳过 PATH 授权与工具链注入 |
| 4 | `/opt/homebrew/bin/go: Operation not permitted` | seatbelt readRoots 快照缺失/过期 |
| 5 | `ls: /opt/homebrew/Cellar/go/…/bin: Operation not permitted` | 工具链授权快照不完整（brew 升级漂移） |
| 6 | `goproxy.byted.org → 401 Unauthorized` | 凭据注入通道绑定失败且不可见 |
| 7 | `GOPROXY=http://127.0.0.1:<port>` 每命令漂移 | 出口代理生命周期绑定单条命令 |
| 8 | `410 Gone`（内部 goproxy 透传） | 上游状态透传无 Fact，负面缓存污染 workspace 会话 |
| 9 | `https network target requires method CONNECT` | 代理实现细节泄漏成参数校验 |
| 10 | `verification must not write workspace files` | verification 与 write_paths 事前互斥 |
| 11 | `submit a structured Plan before consequential actions` | plan 状态每 turn 重置 |
| 12 | `turn.failed: illegal turn transition (awaiting_input)` | input 超时无 resolve 回调 |

同 session 的 turn 指标 `approval_wait_ms=701300`：一轮 24 分钟的 turn
有 11.7 分钟在等待人工批准，且只读探测命令也触发审批。

## 2. 目标架构与原则

```text
L4 审批与 UX        前缀 grant · 风险分层放行 · plan 漂移闸门 · 审批指标
L3 凭据代理         协议无关 loopback broker（git helper / netrc 类 / goproxy adapter）
L2 生态准备器       manifest 驱动的 Discover（探测→ResourceRequest/Fact→env 投影）
L1 事实仲裁         exec-time(工具链可用性) · connect-time(RuntimeApprover) · settle-time(evidence)
L0 进程边界         seatbelt / landlock / seccomp：只管 fs / 进程 / 网络 / 资源
```

设计原则：

1. 决策点在事实发生时（exec / connect / settle），不做事前意图申报硬闸门。
2. 语言知识是数据不是代码：`internal/security` 与 `internal/platform`
   通用层零 `GO*`/`CARGO*` 字面量，生态差异只存在于准备器 manifest
   与协议 adapter。
3. 授权有生命周期：绑定 session、按 turn 重验，`Lifecycle` 字段生效。
4. 拒绝必须结构化：`{原因, 缺什么, 补救动作}` 三元组，escalation 可接管。
5. 红线不动：不做 TLS MITM；真实凭据不进沙箱；不绕过
   guard/policy/approval/journal；`(deny default)` 姿态不变。

## 3. 工作流（Workstream）

### WS1 沙箱核心：继承与生命周期

- **1.1 隔离后端继承父 policy**：`sandbox.Options` 增加工具链暴露与环境值
  直传字段；`PrepareBackend` 从父 policy 原样透传。`SkipPATHReadRoots`
  语义收窄为"只跳过 PATH 目录扫描，不丢弃已发现的暴露"。授权不超出
  父 policy 已有范围，不违反"不得静默扩大权限"。
- **1.2 授权快照会话化**：policy 所有权移到 workspace session；每 turn
  对 `source_version`/`live_host_file` 类请求做 lstat 级重验，漂移则增量
  重发现。
- **1.3 PATH 来源统一**：收敛到 `PlatformPATHDirectories()` 单一实现，
  darwin 平台清单常量化并文档化。

### WS2 环境构造：三轨制 → 单一契约

- **2.1 allow-list 变准备器投影**：准入集合 =
  基础安全集 ∪ 准备器申报投影；模型传入非集合变量降级为一次性
  approval 扩围；secret 名单继续硬拒。
- **2.2 准备器 manifest 化**：探测命令、env 投影、缓存目录、网络需求
  声明进 manifest；Go 是第一个实例，新增生态只加数据。

### WS3 网络层：申报 → 仲裁

- **3.1 删除 CONNECT-only 参数校验**：method 从申报格式降为提示；
  代理见到 https 自动走 CONNECT。与环境契约"CONNECT 不得包装成
  '只允许 GET'"的原则一致。
- **3.2 通道生命周期**：环境改写指向 workspace 级稳定通道；per-session
  通道只承担授权 scope；workspace 通道增加带 TTL 的负面缓存与可配置
  超时。
- **3.3 RuntimeApprover 扩展到 web 工具**：重定向链批量授权。
- **3.4 goproxy 可观测性**：非 2xx 记 Fact（区分上游永久否定/不可用/
  凭据被拒）；恢复 `/sumdb/` 处理。

### WS4 凭据代理：通用化

- **4.1 绑定失败可见**：宿主自动绑定每个失败分支产出
  `environment.Fact`（`credential_unavailable` + 补救动作）。
- **4.2 git 通用凭据通道**：沙箱内 `GIT_CONFIG_GLOBAL` 指向临时配置，
  credential helper 指向 loopback broker；真实凭据只在宿主侧。
- **4.3 netrc 类生态**：sandbox-home 生成仅含 loopback 身份的 netrc。
- **4.4 goproxy 降级为 adapter**：迁移为 broker 的第一个协议 adapter。

### WS5 验证与计划闸门：事前否决 → 事后观测

- **5.1 verification 事后判定**：删除入口互斥；settle 观测到
  `covered_paths` 被写时把 evidence 标记
  `invalidated_by_workspace_write`。
- **5.2 PlanSubmitted 持久化**：仅基线漂移或 profile 变更时重置；
  declared-verification 纳入豁免。

### WS6 拒绝结构化

后端收尾扫描 stderr 的 `Operation not permitted` 模式，结合本次
profile 生成结构化 Denial（含请求二进制 vs readRoots 快照差集），
escalation 自动生成扩权重试或 approval 请求。

### WS7 审批降噪

| 项 | 现状 | 目标 |
|----|------|------|
| grant 身份 | 完整 argv 指纹 | 静态 argv 前缀（复合/元字符命令退回完整指纹） |
| surface ask | 一刀切压所有 process 工具 | 只压中高风险，`RiskLow` 只读放行 |
| ask 规则匹配 | 复合命令永不匹配 | 前缀命中复合命令首段 |
| once-policy | 每次必问 | 只读形态允许 session scope |
| 指标 | 只有总量 | per-approval 明细入 turn 指标 |

### WS8 turn 状态机修复

- **8.1 时序修复**：`interact.Host` 增加与 guard 对称的 expiry handler；
  TTL 触发或取消时先执行拒绝式 `ResolveInput` 再返回错误。
- **8.2 防御修复**：`applyToolResult` 接受 `awaiting_input`（前提：该
  call 绑定到 pending input，`InputState` 记 `CallID`）。

## 3a. Turn 复盘驱动的增补项（2026-09-22）

对修复后首个成功 turn（`turn_5b52bc22`，约 18 分钟、37 次工具调用、
14 次审批）的复盘结论：Phase 1-3 修复的各类错误均未复现；剩余摩擦
集中在三处，其中两项已落地：

- **已实现 A · 失败命令摘要**：`exec_command` exit≠0 且无
  `error_category` 时，Web 摘要不再输出"no structured error details"
  空壳，改为带 `output: "<尾行>"` 的输出上下文（标注为证据而非
  归因，符合"不从输出文本推断原因"的既有约定）。
- **已实现 B · 多仓工作区的 Git 定位**：git 读工具从 `path`
  （或工作区根）向上寻找 `.git`（不越出工作区，支持 worktree 的
  `.git` 文件），在发现的仓库根执行；`git_show`/`git_blame` 的
  pathspec 重写为相对仓库根；根目录不是仓库时失败结果附带子仓库
  清单提示。工具描述同步说明 `path` 的定位语义。
- **已实现 C · 工具链适配事实化**（2026-09-22）：Go 准备器读取
  工作区 go.mod 的 `go`/`toolchain` 指令（有界遍历，多模块取最严，
  跳过 vendor/testdata 等目录）并与宿主 GOVERSION 比对，模块要求
  更高时产出 Fact：说明 GOTOOLCHAIN=auto 会经 Session 代理自动
  拉取所需工具链（GOTOOLCHAIN=local 时附 `approve_host_config`
  动作）。同时修复 prepared.Facts 在 wire 绑定时被整体丢弃的缺口：
  准备器 Facts（含既有的"go 不在 PATH"、"`|direct` 不自动授予"）
  现在与认证绑定报告共用同一通道，在失败的进程结果上归因；
  Fact 的 Detail 也以 `environment_detail` 元数据透出（此前只透出
  category/action，指引文本丢失）。
- **候选 D · 整模块上游不可用指引**：goproxy 记录 per-module 的
  410，当 `@v/list` 也 410 时 Fact 的 RequiredAction 直接给出
  "使用 replace / vendor / 替代源"。
- **已实现 E · 复合命令审批粒度（2026-09-22，根因方案）**：该 turn
  14 次审批的真正根因不是复合命令指纹，而是 GOPROXY 被改写到
  **每命令随机会话端口**——只有 `allow_loopback` 的 seatbelt 通配
  授权能到达，于是每条模块探测命令都背上 loopback 资源与审批。
  落地改为：GOPROXY 指向 workspace 级稳定通道（`ManagedProxyPort`，
  seatbelt 已无条件预授权，Phase 2 的响应缓存跨命令复用率同步提
  高）；认证服务绑定时空 `network_targets` 不再视为离线（对齐环境
  合同"未声明、无认证服务、无 allow_loopback 才是离线"）；
  fail-closed 检查认可 workspace 通道交付环境网络。执行面不变：
  origin-form 请求仍由认证服务自身强制（前缀 scope、固定上游、
  凭据注入），外部 CONNECT 仍走逐命令会话闸门，`denyBoundOrigin`
  仍禁止绕过。两个被否决的方向与理由存档：复合命令首段前缀降级
  ——批准 `A && B` 的前缀会覆盖未批准的 B 段，不安全；纯 loopback
  风险降级——`localhost:*` 可达宿主本地服务，审批是真实边界。
  另在 exec_command 描述中引导"相关探测合并为一条链式命令，
  共享一次审批"。

## 3b. Phase 4 · 影子验证（Shadow Verification）详细设计

**痛点证据**：`turn_5b52bc22` 中模型为避免把 `replace` 临时指令
结算回工作区，手工编排了"可丢弃验证副本"——复制整个模块到
`$TMPDIR`、构造 kitc_stub、在副本内构建与跑测、再把改动的测试文件
手动同步回工作区并二次 gofmt 确认，耗时约 10 分钟（占 turn 一半）。
现有 execsettle 隔离后端只覆盖"要结算的写"，没有"验证后丢弃的写"。

**语义**：

- `exec_command` 声明 `verification` + `write_paths` 之外新增执行
  模式 `settle: "discard"`（或 verification kind 扩展 shadow 值）：
  命令在隔离副本中执行，结束后**不结算任何写**，只回收证。
- Evidence 语义与 WS5.1 一致：covered_paths 的输入摘要仍取自工作区
  原文件（副本由工作区复制，保证一致）；副本内改写 covered_paths
  不影响工作区，因此 evidence 判定看命令退出状态即可。
- 丢弃前产出**变更摘要**（"影子副本产生了 N 个文件变更，已丢弃"），
  给模型诊断信息而不污染工作区。

**安全边界**：

- 写只发生在隔离副本（沙箱 workspace root 即副本根）；HostWriteRoots
  与命令级网络/凭据声明不变；journal 记录 shadow 执行并标记副作用
  不可重放；副本构建缓存（sandbox-home 的 GOMODCACHE/GOCACHE）按
  workspace 生命周期照常复用。

**实现锚点**：

- `beginIsolatedCommand`（shell/isolate.go）已创建副本，
  Phase 1 已让隔离后端继承工具链与环境注入——影子模式复用同一路径。
- `execsettle.Service` 增加 `BeginShadow`（或 Begin 带模式）：Settle
  侧跳过 `merger.ApplyPaths`，改为 `PlanPaths` 的 diff 摘要 +
  `Close` 清理。
- Guard 侧效果分级：影子命令的写全部丢弃，效果按 process + 网络
  授权，不产生 workspace mutation revision。

**验收**：复现 `turn_5b52bc22` 的 kitc_stub 场景——一条命令完成
"副本内 replace + 构建 + 测试"，工作区零污染、evidence 通过、无
手动文件同步。配套攻击测试：影子模式不得向工作区结算任何路径、
不得把副本内新建的敏感路径（凭据文件名）带出摘要之外的内容。

落地状态（2026-09-22）：已实现。`exec_command` 新增 `settle` 字段
（`apply` 默认 / `discard`，discard 必须声明 `write_paths`）；
`tool.Isolator` 新增 `BeginShadow`；execsettle 影子会话的 Settle
只做 `PlanPaths` 计划性摘要、跳过 `ApplyPaths`（不触 journal 与
workspace gate）；shell 侧影子结算只进元数据
（`workspace_settlement=shadow_discarded`、`discarded_changes` 与
上限 20 条的路径摘要），**不进** settlement facts 与 observed
changes——turn 不产生未落地变更的 mutation revision。验收测试
`TestExecCommandShadowDiscardKeepsWorkspaceUntouched` 复现 kitc_stub
形态（副本内改 go.mod + 建 stub + verification），断言工作区字节
不变、stub 不泄漏、evidence 通过、facts 零污染；execsettle 单测
断言影子 Settle 返回计划变更且父工作区零写入。

## 4. 分期实施与验收

### Phase 1 · 止血

范围：WS1.1、WS3.1、WS4.1、WS8。
验收：

- 带 write_paths 的构建命令在隔离后端直接找到工具链（不再出现
  `command not found` → 绝对路径 → EPERM 链）；
- HTTPS 目标申报不再被参数校验拒绝；
- goproxy 未绑定时模型首轮收到 credential Fact 而非反复试探；
- `request_user_input` 超时/取消不再报废 turn。

### Phase 2 · 疲劳与观测

范围：WS5、WS7、WS3.2、WS3.4。
验收：`approval_wait_ms / turn_ms` < 10%；只读探测零审批；
`go build -o` 类验证命令可执行（evidence 事后判定）；跨命令缓存命中，
410 不再污染整个会话；`turn.failed(illegal_transition)` 归零。

落地状态（2026-09-21）：WS5.1 已实现——verification 入口互斥删除，
执行后按 covered_paths 重算 InputDigest，变化即标记 `invalidated`
（新增 evidence 状态与 `invalidation_reason` 字段）；WS5.2 已实现——
`PlanSubmitted` 会话内持久（每 turn 重置已移除，profile 变更仍重置），
声明式 verification 的 plan 闸门从 Hold 降级为单次 Ask
（`plan_verification`）；WS7 已实现——surface ask 不再作用于
`process.read_only + risk_low`（deny 姿态不受影响），shell grant 增加
静态 argv 前缀身份（同 cwd 与资源 scope 内匹配，复合/动态命令退回
完整指纹），ask 前缀规则可命中复合命令任意段（allow 仍限单段）；
WS3.2/3.4 已实现——goproxy 增加带 TTL 的响应缓存（成功 30 分钟、
404/410 负面 5 分钟、总预算 32 MiB 公开常量），上游 404/410/5xx
记录 Fact（区分上游否定与沙箱故障），恢复 `/sumdb/` 路由
（自校验载荷，信任边界为固定上游主机），上游超时可经
`upstream_timeout_ms` 配置（0 保持 30s 默认，负值拒绝）。

### Phase 3 · 架构收敛

范围：WS2、WS3.3、WS4.2-4.4、WS1.2-1.3、WS6。
验收：通用层 `grep` 语言前缀零命中；新增生态支持 = 加 manifest；
git 私有仓库拉取免凭据配置走通；EPERM 结构化 Denial 覆盖 escalation。

落地状态（2026-09-21）：

- **WS2.1 已实现**——模型声明的环境变量放开为"合法 NAME + 非 secret
  名单"（`CGO_ENABLED`/`CARGO_HOME` 等直接可用，声明名单进入结果
  `declared_env` 审计回执）；宿主隐式继承列表去掉语言变量（GO\* 不再
  隐式透传，准备器与显式声明是唯一来源）；无 policy 路径的
  `ensureGoToolchain` 特判替换为语言无关的 `ensurePlatformToolchainPATH`
  （平台 PATH 目录引导，go/git/cargo/python 同等对待）。
- **WS6 已实现**——`exec_command` 增加执行前可执行文件可读性预检
  （`Policy.ExecutableReadable` 忠实建模 profile 读根集合，含符号链接
  解析），不可读的二进制在启动前以结构化
  `filesystem_access_denied` + `approve_host_config` + 具体路径拒绝，
  取代子进程里的裸 EPERM。判定来自后端事实，不解析命令输出，
  符合"error_category 只来自 Gate 或后端事实"的合同。
- **WS3.3 已实现**——guard 把 RuntimeApprover 绑定扩展到网络能力工具：
  重定向链与运行时发现的目标在连接时单次询问并继续抓取，仓库 deny
  规则在连接时结算为结构化 `egress_denied`（带 host 与
  required_action），不再整次失败后重放。
- **WS1.3 已实现**——darwin 开发者工具 git 布局知识移入
  `toolchain_darwin.go` 平台文件；PATH 引导收敛到
  `PlatformPATHDirectories()` 单一来源。
- **WS1.2 部分实现**——执行前预检即是 turn 级的快照失效检测（工具链
  漂移立即产生可行动的结构化错误）；policy 所有权迁移到 workspace
  session 与自动重发现仍延后。
- **WS4.2-4.4（凭据 broker 通用化）延后**：设计要点已定——git 走
  `GIT_CONFIG_GLOBAL` 指向 sandbox-home 临时配置、credential helper
  指向 loopback broker（真实凭据只在宿主解析，须以显式
  `credential`+`use` 资源声明为门）；netrc 类生态用仅含 loopback 身份
  的临时 netrc；goproxy 降级为 broker 的第一个协议 adapter。因涉及
  凭据面安全姿态变更，需单独评审后实施。
- **WS2.2（准备器 manifest 化）延后**：Go 准备器已是契约唯一实例，
  manifest 化是纯重构，待第二个生态准备器出现时一并做，避免无实例
  的抽象。

## 5. 安全边界与攻击测试

- 新配置字段（缓存 TTL、代理超时、manifest 清单）带
  provenance/validation/文档/boundary tests；不引入 pre-release
  兼容迁移。
- 攻击测试沿用 `managed_egress_attack_test.go` 模式扩面：假 netrc
  提权、缓存投毒（伪造 etag/410）、loopback broker 跨 workspace
  隔离、前缀 grant 前缀逃逸（复合命令不折算前缀）。
- 凭据面：真实凭据宿主侧解析；沙箱内文件 0600、只含 loopback 身份、
  会话结束销毁；audit log 记录凭据使用。

## 6. 度量

- `approval_wait_ms / turn_ms`（目标 <10%）
- sandbox EPERM 结构化 Denial 计数（目标趋零）
- `turn.failed` 中 `illegal_transition` 归零
- 凭据类 401 的首轮归因率（目标 100%）
- 跨命令缓存命中率（goproxy/HTTP 层）

## 7. 与现有文档的关系

- [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)：
  环境契约总纲，本文不改变其冻结决策，只修订实施偏离。
- [内部依赖诊断](./sandbox-dependency-diagnosis.md)：实机证据。
- [架构](./architecture.md)、[安全](./security.md)：产品语义以它们为准。
