# 安全模型与运维

## 安全目标

QCode 会根据模型选择在源码上执行工具。目标不是让任意代码变得安全，而是让权限
显式、影响有界、凭证不进入模型可见状态，并让所有关键动作可检查。

## 威胁模型

以下输入都应视为不可信：

- 用户 Prompt 与粘贴内容；
- 仓库文件、生成代码、测试和构建脚本；
- 模型输出与 Tool Argument；
- Provider Response 与 Native Search Result；
- MCP Server 与 Skill Content；
- HTTP/Web Transport Client Message；
- 从其他 Workspace 复制的持久化状态；
- Archive Path、Symlink、Environment 与 Process Output。

本地 Operator 和可信 Release Key 是 Authority Root，但 Operator 误操作和依赖被攻陷
仍在威胁范围内。

## 分层控制

| 层 | 作用 |
| --- | --- |
| Mode | 限制请求的工作类型 |
| Posture | 决定拒绝、审批或自动处理 |
| Workspace Permission | 把已记忆权限绑定到单一 Workspace |
| Constitution | 普通配置不能绕过的硬约束 |
| Tool Guard | Identity、Risk、Resource、Approval 与 Evidence 的统一决策 |
| Execution Authority | 将授权结果绑定为单次 Operation Lease，并校验 Generation 与 Controls |
| Edit Journal | 记录 Before Image 与中断工作 |
| Verify Gate | Commit 前收集正确性证据 |
| OS Sandbox | 强制进程、文件系统和网络边界 |
| Egress Control | 约束远程 Endpoint 与出网 Client |
| Observability | 通过 Privacy、Retention 与有界 Export Policy 接收版本化证据 |

任何一层都不能被描述为另一层的替代品。

## Posture 建议

- `never`：首次检查仓库和不可信 Workspace 最安全。
- `suggest`：日常交互开发推荐。
- `auto`：适合已知 Policy 和确定性 Fixture；被拒绝的操作可能不弹审批。

Web 不接受 `bypass`；该内部 Posture 仅用于受控测试与隔离执行，并仍受硬约束和
Sandbox Availability 限制。

Web Markdown 不执行原始 HTML 或危险 URL。同源图片可以直接显示；跨域图片只有在
用户点击加载后才会请求，并且只允许 HTTPS、使用 `no-referrer`，避免模型输出静默
泄露页面来源或触发明文媒体请求。

## Workspace 与文件安全

- 相对配置 Workspace 解析并校验路径。
- 拒绝 Traversal、不安全 Symlink 和 Archive Escape。
- Durable Workspace Journal、Process Job Journal 和 Job Log 位于
  `<data-dir>/workspaces/<workspace-id>/control`，不再从 Workspace 内的旧
  Runtime 外部状态目录中的 Journal 恢复。
- Workspace State 分为互不重叠的 `control`、`sandbox-home` 和 `artifacts`；
  在这三个状态域中，Sandbox 只获得 `sandbox-home` 写权限。
- Tool Contract 要求时先读后写。
- Tool Catalog 将模型可见的 `ExternalDescriptor` 与 Registry 可信的
  `TrustedBinding` 分开冻结。MCP 等外部来源只能提交
  Requested Effects；Capability、Resource Resolver、Access、Sandbox、Effect、
  Required Controls、Journal 和验证证据资格由可信 Binding 决定。
- Guard、Policy 和 Authority 不从工具名或 External Requested Effects 推导授权。
  Deferred Loader 改变 Trusted Binding、Schema 或 Alias 会 Fail Closed；替换 Binding
  会更换 Revision/Authority，使采样时的旧 Catalog Binding 失效。
- Trusted Binding 和 ExecutionOperation 使用十维 Required Controls：
  Filesystem Read/Write、Network、Process Tree、Cross Process、Syscall、IPC、
  Path Identity、Artifact Origin 与 Durable Recovery。Sandbox Probe、Policy 和具体
  Command 共同产生 Effective Controls，Lease 只在每个要求都被满足时签发。
- Backend 完成 `Prepare` 后，Process Owner 再次核对本次命令的 Prepared Controls。
  旧 `Strength` 能力与 Receipt 字段已删除，不能单独证明或授予执行权限。
- 副作用 Inventory 同时检查系统进程 API 和 `internal/platform/process` 的构造入口；
  新调用方必须登记为 Guard 授权执行、Lease-consuming Broker 或可信 Runtime/Host Owner。
- Web 分支切换不直接执行 `git switch`，由 VCS Broker 校验固定参数、仓库身份和
  Execution Lease；未注入 Broker 时 fail closed。
- `file_write`、`file_edit`、`file_apply`、`file_patch`、`integrate_agent`、隔离
  Chat Merge 和 `document_convert` 的最终 Workspace 输出统一生成不可变 File Plan。
  Guard 或 Runtime Authority 签发绑定 Plan Digest、Workspace Generation 和精确
  Path Resource 的单次 Lease，File Broker 是提交这些 Plan 的唯一 Owner。
- File Broker 在 Journal Before Image 后再次校验文件内容、身份和父目录，使用
  descriptor-relative API 先写后删。写入、最终快照或 Journal Settlement 失败时
  逆序恢复；恢复冲突或失败明确报告 Partial Change，不伪造原子成功。
- `file_edit` 以及 `file_apply` 中首个落盘操作为精确替换的路径，可由受信文件工具
  提交绑定当前内容摘要的 Exact Edit Proof，等价满足该路径的 Read-before-write。
  全量覆盖、删除、移动或先覆盖后编辑仍要求显式 `file_read`。
- File Broker 拒绝 Symlink、Hardlink、Device Boundary、Root/Parent Replacement，
  并在自身边界拒绝 `.git`、`.qcode`、`.qcode-worktree`、`.agents` 和
  `.codex`。Unified Diff 先解析为 File Plan，不调用 `git apply` 修改 Workspace。
- `exec_command` 的写权限只授予显式 `write_paths`。目标可以是现有普通文件，或位于
  已存在父目录下的待创建文件；Guard 在执行前完成 Preflight，Strong Sandbox 只为
  这些精确路径物化最小占位。目录、Symlink、重复路径和执行前发生的身份漂移均拒绝。
- 配置后，写入型 Subagent 使用 Worktree。
- 隔离 Worktree 仅可只读访问经过校验的自身 Git Administration Directory，以及
  Repository Common Git Directory 中必要的 Object、Ref 与配置路径；这不会授予
  Parent Worktree 或 Git Metadata 写权限。
- Git Worktree Registration、Index 和 Ref 不属于普通 Workspace 文件。Child
  Worktree Add/Remove/Prune、Chat Baseline，以及模型发起的 `add`、`commit`、
  `switch`、`fetch`、fast-forward `pull` 和非 force `push` 只能由 VCS Broker
  执行。每次白名单 Mutation 绑定 Common Git Directory Identity、目标 Worktree
  HEAD/Ref、Index Digest 和 Worktree Registration Digest；执行接管前发生漂移即拒绝。
  远端写入使用不可逆高风险 Effect，并要求单次审批。
- 使用 `apply --dry-run` 检查生成计划。
- 重要仓库必须纳入版本控制并维护备份。

## 进程执行

- Guard 在现有 Policy 和人工审批完成后，把冻结的 Tool Invocation
  规范化为 `ExecutionOperation`。Operation 绑定 Workspace/Subject Generation、
  Resource Namespace、Effect Contract、Required Controls、参数摘要和 Artifact
  Provenance；资源排序、去重后计算稳定 Digest。
- 每次实际 Attempt 使用共享 `LeaseAuthority` 签发并消费一个不可伪造、单次使用的
  Execution Lease。Lease 绑定 Operation Digest、Permission Profile Digest、Policy
  Revision、Sandbox Policy、Workspace/Subject Generation、Artifact Digest 和 Attempt。
  过期、撤销、重复消费或任一 Generation 漂移都会 Fail Closed。
- `execution.lease_timeout` 是授权到执行接管之间的显式配置上限；更早的调用 Context
  Deadline 会收紧它。Lease 消费后，运行中资源的回收不受 Lease 到期影响。
- `execution.approval_timeout` 控制人工审批等待；默认 `0`，表示只随 Turn/Session
  生命周期结束。非零值启用独立过期，过期请求继续 Fail Closed。
- “始终允许”先写入工作区权限文件，成功后发布共享的版本化规则。同工作区已有
  会话和新建会话在下一次工具授权时读取最新规则，无需重启；单次和会话级审批缓存
  仍各自隔离。持久权限不覆盖 Managed/Repository Deny、只读模式或子 Agent 的工具
  限制；已冻结的执行快照不被追溯修改，其他工作区不订阅该规则源。
- Attempt Receipt 持久记录 Operation Digest、Lease ID/State、Effect、Workspace、
  Subject、Policy 和 Sandbox 绑定。当前兼容 Facade 保持原有 Policy Decision、
  Approval Scope、Typed Denial 与 Amendment 语义不变。
- Artifact Broker 只接受 Workspace 或 Sandbox Home 内的常规可执行文件，拒绝
  Symlink、Hardlink、特殊文件与 Device Boundary 变化，并复制到 Broker-only
  Artifact Staging。复制前后复核源身份，Manifest 绑定 Workspace Generation、
  Producer Operation 与内容摘要。
- Process Broker 验证 Artifact Manifest 和最终 Operation，单次消费 Execution Lease，
  签发绑定 Session/Thread/Turn 与 Process Generation 的 Process Handle，并独占
  Start、Cancel、Wait、Reap 和 Settlement。Runner Failure 与提前退出分别记录为
  `runner_failure` 和 `command_exited_early`。
- Command 使用 Sanitized Environment。
- Working Directory 与 Executable Path 必须显式。
- 必须支持 Timeout、Cancel 和 Process Group Cleanup。
- PTY 与非 PTY 共享 Policy Boundary。
- `shell_read` 是检查类 Pipeline 的自动执行路径。Strong Sandbox 将 Workspace
  强制挂载为只读、禁用网络，只允许写入 Private Temporary Directory，并且绝不进行
  Unsandboxed Retry。
- 可增权的 Typed Sandbox Denial 可通过 Critical One-shot Approval 申请一个精确
  Path、Host/Port 或 Process Capability。重试使用递增 Revision 的 Permission
  Profile，并保持在同一 Strong Sandbox；Untyped 或重复 Denial 均 Fail Closed。
- macOS 上 `exec_command` 的进程出口仅允许
  通过 Runtime-owned loopback proxy，并要求用 `network_targets` 显式声明 Host、
  Port、Protocol、传输 Method 和私网权限。HTTPS 目标必须使用 `CONNECT`，HTTP
  目标使用普通 HTTP Method。已声明的 Process Network Resource 会先于 Process
  Effect 被归类：`suggest` 必须经过人工 Network Approval，`auto` 才可以自动
  Review 精确的只读目标。Sandbox 只能连接代理端口，直连和未声明目标均 Fail
  Closed。该 Loopback Proxy 返回 CONNECT 403 表示目标未声明或未授权，并不表示
  远端服务不可达。Linux 在 namespace proxy bridge 交付前保持进程全禁网。
  macOS 的代理能力通过启动时的精确端口允许/拒绝探测单独确认，不从默认禁网状态
  推断。获批目标的 Effective Profile 保留代理端口，进程环境注入 Runtime 代理；
  没有声明目标的命令仍禁网，也不注入代理变量。
- 测试 Fixture 或本地开发服务必须绑定并连接临时 Localhost 端口时，
  `exec_command` 可声明 `allow_loopback`。
  该能力默认关闭；Strong Sandbox 内仅包含精确 Localhost Grant 且没有 Workspace
  写入的调用按有界 Network Read 评估，`suggest` 要求审批，`auto` 自动 Review。
  macOS Profile 只增加 Localhost Inbound/Outbound Seatbelt Rule；非 Loopback
  流量仍必须声明精确 Proxy Target。Loopback-only Effective Profile 不绑定托管
  代理端口；执行器不得因为 enclosing sandbox 仍持有 Runtime Proxy 而拒绝已批准的
  Localhost Grant。该调用的命令策略与环境均移除代理配置，Prepared Controls 根据
  后端已探测能力声明 `loopback_exact`；同时获批 Proxy Target 与 Loopback 的调用
  仍使用 `proxy_targets` 并保留代理端口。共享 Workspace Policy 不随单次调用改变。
  Guard 在汇总全部网络资源后确定租约的 Required Controls，与 Effective Profile
  保持一致；主机排序和参数顺序不改变混合调用的控制要求，也不隐式授予 Loopback。
  若代理端口仍与 Profile 错位，工具结果必须带
  `required_action=keep_allow_loopback_omit_network_targets`，不能把临时端口
  写进 `network_targets`。Effective Profile 与 Attempt Receipt 都会记录该
  Loopback Grant。
- 声明 `verification` 的 `exec_command` 使用 POSIX `set -e`，并要求精确的
  `covered_paths`。声明不能扩大执行权限；验证命令不能声明 Workspace 写入，
  仍经过相同 Guard、审批、Journal 和 Sandbox。证据在启动前绑定输入摘要，
  结束时检查摘要和 Mutation Revision；运行中或被终止的进程不提供通过证据。
  Verifier 子代理只允许带验证声明的进程启动，不因入口统一取得写权限。
- Language Server 按文件类型选择实际安装的 Server，进程在 Workspace Read-only、
  Network Denied 的 Strong Sandbox 中运行。format、code action 和 rename 只返回
  edits，不直接取得文件写权限。
- `format_code` 的写权限限定到请求中的精确文件，并使用 before-image Transaction；
  `debug_run` 只接受经过校验的 Symbol 或 `file:line` 断点，不接受任意 LLDB Command；
  `dependency_resolve` 禁用安装脚本并保持 Workspace Read-only。
- `web_run` 使用独立临时 Chromium Profile，不复用用户浏览器 Profile。浏览器交互和
  通用 `http_request` 都按不可逆 External Mutation 要求单次审批；Loopback 导航必须
  显式声明。`http_request` 拒绝 Authorization、Cookie 和 API Key Header，并从返回
  Metadata 中删除 Set-Cookie 与认证挑战 Header。
- 权限规则保留原始 Resource。文件资源使用 Guard 解析出的规范化路径匹配，主机、
  URL 等 ID 资源使用原值匹配；通配工具或同时涉及文件和网络的工具也遵循该区分。
  相对文件规则继续拒绝 `..` 逃逸并解析符号链接，不根据名称是否含点猜测资源类型。
- Web HTTP 工具的动态网络许可仅属于当前 Guard 调用，覆盖该调用内的重试，结束或
  取消后失效。每个 HTTP 请求与重定向都检查当前调用的目标许可；新目标仍须通过
  Guard 策略和审批，不能复用其他工具、会话或已完成调用的传输许可。会话级审批
  缓存仍可按原策略复用，但必须重新绑定本次调用。配置的搜索后端继续作为声明资源
  接受审批，固定 Gate 许可不能代替调用许可；Provider 与进程代理使用独立 Gate。
  重定向拒绝信号保留实际目标的协议、主机、端口和 HTTP Method，审批展示与本次
  传输放行使用同一目标；例如批准 `https://cdn.example:8443` 后按 8443 重试，
  不退回默认 443。303/307 等重定向使用跳转后的实际 Method；缺少结构化目标的
  网络拒绝不会根据原始 URL 猜测目标并追加审批。
- Git merge、rebase、cherry-pick、restore、stash、tag 和 amend 均通过 VCS Broker
  的固定 argv 白名单执行；不提供任意 Git 参数、force push 或隐式远端。可能改写历史、
  产生冲突或丢弃内容的操作要求单次审批。
- 不提供绕出 OS Sandbox 的模型侧宿主进程冒烟入口。开发服务和 Fixture 使用
  `exec_command` 及显式 `allow_loopback`；观察到服务存活不能当作测试通过。
- Linux Strong Sandbox 将 Landlock、`no_new_privs`、seccomp 与 `execve` 固定在
  同一个 OS Thread。Seccomp 拒绝 Tracing、跨进程内存访问、Namespace 创建、
  `clone3` 与 `io_uring`；Restricted Network Mode 只保留 AF_UNIX 进程内 IPC。
- Command Policy 使用 Bash AST 与 Static argv Segment。Managed Authority 定义
  Ceiling，Repository 只能收紧，User Approval 不能覆盖高权 Deny/Ask。Policy Reload
  原子发布新 Revision，并绑定到 Profile Provenance。
- 每次实际执行的 Tool Attempt 都记录准确的 Effective Permission Profile
  Revision/Digest、Enforcement Backend、Filesystem Root、Network Mode、Grant
  Provenance，以及 Typed Denial 或 One-shot Amendment。Amendment Receipt 将 Base
  Digest 与获批后的 Replacement Digest 绑定，重试不会覆盖前一次 Attempt 的证据。
  `tool.result.execution` 将这条证据链持久投影到 Runtime Event；对话历史重建只消费
  Tool Output，不把该审计字段送回 Model Context。
- `exec_command` 与 `write_stdin` 保留 Process Capability 和原有 Approval
  行为。`exec_command` 是唯一通用 Command Start 路径；首次 Sample 只等到
  `yield_time_ms`，进程未退出则返回 `session_id`。`write_stdin` 在每次
  Session 交互前校验当前 Thread Lease；`timeout_ms` 只杀进程组。
  macOS/Linux 下父 shell 正常退出后，Session 先终止同组残留后台进程，再发布
  完成状态及释放 Session/Turn 记录；后台进程是否关闭输出不影响回收。终止与最终
  Reap 同步，父进程身份在组清理前保持有效，避免延迟按已复用的 PID/PGID 发信号。
- Process Tool 通过有界 Fair Budget 与精确 Resource Claim Admission。不同 Session
  与无关 Path 可并发，冲突 Claim 保持顺序。
- Cancellation Terminal Ownership 遵循声明的 Execution Disposition；Process
  Teardown 必须在释放 Consequential Claim 前终止并回收完整 Process Group。
- 缺少所需 Strong Sandbox 是失败，不是允许 Unsandboxed Execution。

## 凭证

配置只允许 Reference：

```toml
[credential]
kind = "env"
name = "OPENAI_API_KEY"
```

运维规则：

- 不提交 Secret Value；
- 不在 Prompt 或 Command Argument 中传 Secret；
- 桌面端优先 OS Keyring；
- CI 优先由 Secret Manager 注入 Environment；
- Secret File 使用限制性权限；
- 怀疑泄漏后立即 Rotation；
- 即使开启 Redaction，Log、Receipt、Crash Dump 与导出的诊断材料仍视为敏感。

Web 写入凭证时会创建 Workspace/Provider 隔离的新 Keyring Entry，并以不含 Secret 的
`prepared`、`config_committed`、`completed` Intent 和 Generation CAS 提交 Reference。
切换后当前 Runtime 继续使用 Turn 已冻结的旧 Route，页面显示需要重启；下次启动会清理
未提交的新 Orphan，并只在扫描 data-dir 内全部托管 Reference 后删除无引用的旧托管
Entry。用户自定义 Keyring Name 无法完成全局引用证明，因此不会被自动删除。

运行：

```bash
make secret-leak-test
```

## 网络与服务暴露

- 服务默认监听 `127.0.0.1`。
- 非 Loopback 部署必须使用经过 Review 的认证网关。
- Provider Base URL 与 Redirect 属于安全敏感配置。
- Provider、Web 工具和进程代理分别使用独立 Egress Gate。固定 Provider Endpoint
  与 Web Search Backend 的授权不会授予进程代理；Guard 按可信 Tool Capability
  将获批目标交给对应的 Web 或 Process Gate，工具审批不修改 Provider Gate。
- 同一个 Gate 按 Protocol、Host、Port 累计授权，新增 Method 不撤销已有 Method。
  `allow_private` 只作用于同次获批的 Method；例如公网 GET 与私网 POST 合并后，
  私网 GET 仍拒绝。空 Method 授权表示所有方法，后续精确方法授权不能将其收窄；
  请求省略 Method 时则必须具有所有方法的授权。
  当前进程代理的目标授权在 Runtime 会话内共享，并非逐进程身份或租约的独立授权表。
- Native/Web Search Result 仍是不可信内容。
- 可记录 Endpoint Inventory，但不能记录 Credential。

## MCP 与 Skill 供应链

### MCP

Review Executable、Argument、Environment Allowlist、OAuth Config 与 Endpoint，使用
Health Isolation 和有界 Timeout。stdio MCP 默认关闭；启用时配置必须来自外部 State
Directory，并显式声明 `host_trusted=true`。该标记会
进入 Tool Catalog 描述和 Tool Result Metadata，并只允许 Runtime 创建 Lifecycle
Operation，不直接授予进程启动能力。Server 配置摘要和每次启动的 Generation 进入
Subject；Process Broker 消费单次 Lease 并签发绑定 Workspace/Server/Generation 的
Handle。Reload、Disable、Crash 和 Shutdown 会终结 Handle、Settlement 并释放 Lease。

### Skill

锁定最终 Source/Version，并 Review Instruction/Resource。Skill 是 Agent 解释的内容，
存在 Prompt Injection 风险。

## Log 与 Diagnostics

Redaction 降低意外泄漏，但不会让 Log 变成公开数据。应限制访问并设置 Retention。结构化
Error 在 Remote/Filesystem Error 可能含 Secret 时，不应原样输出。
Attempt Receipt 包含 Canonical Path 和 Network Target；即使其中没有 Credential
Value，也必须作为受限 Audit Record 处理。Runtime Event、Receipt、Trace、Usage、
Job Log 与 Workspace Journal 必须采用各自既有的访问控制和 Retention。

Trace Attribute 与 Metric Label 只能使用固定低基数集合，绝不能包含 Prompt、Path、
Argument、Resource ID、Credential 或 Raw Error。Provider Debug Dump 默认关闭；启用
时必须继续经过专用脱敏与本地文件权限边界。QCode 不持久化独立 Observation
Payload，也不提供 OTLP 或 Observation Journal 导出。

## 安全测试

```bash
make security-side-effect-check
make security-test
make sandbox-attack-test
make secret-leak-test
make web-build
```

安全变更应覆盖：

- Allow；
- Deny；
- Malformed Input；
- Cancel 与 Cleanup；
- Concurrent Access；
- Redaction；
- Unsupported Platform。

## 事件处理

Secret 或 Signing Key 可能泄漏时：

1. 停止受影响 Runtime/Update Distribution；
2. Revoke 并 Rotate Credential/Key；
3. 使受影响 Binary Artifact 失效；
4. 保留脱敏证据；
5. 检查 Event/Receipt/Log 影响范围；
6. 适用时发布更高 Sequence 的 Revocation Manifest；
7. 修复控制并增加 Regression Test；
8. 准确通知受影响版本与修复方法。

Workspace Integrity 不确定时，应停止执行，保留 State 与 Journal，检查 Git/Diff，并从
可信 Revision 或 Backup 恢复。

## 报告

公开报告中不能包含 Secret 或私有源码。应提供 Version、Platform、Command Shape、
Sanitized Config Provenance、预期/实际 Security Decision，以及可行时的可复现 Fixture。

当前已交付的执行边界包括 State Domain、Operation/Lease、
Artifact/Process/File/VCS Broker、Process Smoke、stdio MCP Lifecycle、
Workspace Write、Git Metadata Mutation 收口，以及
External Descriptor/Trusted Binding 分离和 Required/Effective Controls 能力矩阵。
后续演进必须继续通过同一 Operation、Lease、Broker 和矩阵契约扩展，不能恢复旁路。
