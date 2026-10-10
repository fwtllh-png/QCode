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
| OS Sandbox | 强制进程、文件系统和网络边界 |
| Egress Control | 约束远程 Endpoint 与出网 Client |
| Observability | 通过 Privacy、Retention 与有界 Export Policy 接收版本化证据 |

任何一层都不能被描述为另一层的替代品。

## 资源评估与 Effect 判定表

Guard 解析并校验参数、路径和可信 Binding 后，通过 `tool.AssessResources` 映射到
类型化安全资源，并仅调用一次纯函数 `model.Assess`。`model.PreparedInvocation` 携带
不可变 Assessment；其资源、网络目标、方法和固定 Effect 都做深复制。Policy、Grant、
审批展示、Authority 和回执读取同一快照，未评估的调用在 L0 拒绝。
参数替换、运行时新出网目标、追加权限属于新的授权输入，必须显式重新评估。

安全核心不依赖工具适配、Runtime 协议或持久化实现。`security/model` 定义
来源、Subject 和 Prepared 输入；适配层校验 Catalog 引用并投影身份，安全层只消费
快照。Subject 摘要绑定目录来源、代次、修订与内部授权身份；参数使用独立副本。
默认内置工具来源使用 `builtin:`，不保留旧来源前缀兼容路径。

共享契约与评估位于 `model`；规则加载、Constitution 和持久 Grant 位于 `policy`，
其中 `LoadConstitution`、`OpenWorkspacePermissions` 负责来源加载，`Runtime.Decide`
只消费规则与 Assessment 快照。控制面路径保护归 `pathpolicy.ControlPlane`，
凭据引用与系统 Keyring 归 `credential`。合包不改变审批层级、Grant Key 或 Lease 合同。

按下表自上而下命中第一行；除前两行外，“已声明”要求 Access 与 Sandbox 声明齐全。

| 顺序 | 规则 | 条件 | Effect / 风险 / 可逆性 |
| --- | --- | --- | --- |
| 1 | `declared_read_only` | 固定效果且命中 Binding 的只读参数声明 | 固定 Kind / low / reversible |
| 2 | `fixed` | 固定效果 | 使用可信 Binding 的完整效果 |
| 3 | `undeclared_read` | 声明不全，Read 能力 | workspace.read / low / reversible |
| 4 | `undeclared_write` | 声明不全，Write 能力 | external.mutation / medium / bounded |
| 5 | `undeclared_effectful` | 声明不全，Process、Network 或 External 能力 | external.mutation / high / irreversible |
| 6 | `undeclared_unknown` | 声明不全，未知能力 | external.mutation / critical / irreversible |
| 7 | `read` | Read 能力 | workspace.read / low / reversible |
| 8 | `plan_only` | Write 能力且唯一副作用是会话计划 | session.mutation / low / reversible |
| 9 | `agent` | Agent 资源 | agent.lifecycle / high / bounded |
| 10 | `process_host` | 可信内置命令明确请求宿主执行，由单次审批或 Full Access 预授权 | process.mutating / high / irreversible |
| 11 | `process_full_access` | Guard 从 Full Access 会话与可信命令 Binding 绑定的进程权限 | process.mutating / high / irreversible |
| 12 | `loopback_only` | Strong Sandbox 进程，仅 loopback、无工作区写 | network.read / medium / bounded |
| 13 | `network_read` | 有网络能力或目标，Access=Read、无工作区写；进程还须出网仅安全读 | network.read / medium / bounded |
| 14 | `network_mutating` | 其余网络能力或目标 | network.mutating / high / irreversible |
| 15 | `process_read_only` | Strong Sandbox 进程且无工作区写 | process.read_only / low / reversible |
| 16 | `process_mutating` | 其余进程能力或资源 | process.mutating / high / bounded |
| 17 | `journaled_edit` | Write 能力、工作区写且有 Journal | workspace.edit / low / reversible |
| 18 | `external` | 其余已声明操作 | external.mutation / high / irreversible |

loopback 是独立资源类，作用域为本机任意端口，不是端口为零的网络端点。
Profile 用 Loopback 标志，Operation 的 Network Intent 用 `loopback_any` 标志表达；
网络 Targets 仅列真实端点。控制名为 `loopback_any`，绝不自动审查。
精确 Grant Key 保持资源与命令身份，避免 Effect 变化使已有 deny 失效；
可复用的命令前缀 Scope 额外绑定 Effect/Facets 摘要，不能跨写权限或网络效果扩大授权。

## 决策分层

`policy.Runtime.Decide` 采样一次策略快照，依序执行 L0–L8；先出现的终止拒绝不可由
后层覆盖。Guard 只消费 Decision，不改写 Action。

| 层 | 标识 | 规则 |
| --- | --- | --- |
| L0 | `input` | 校验调用、Schema 验证状态、Assessment、可信来源、能力、Stage 与路径写根 |
| L1 | `hard_constraint` | 控制面写保护、Constitution、Managed Grant；无 Grant 即拒绝 |
| L2 | `repository` | 仓库只能 deny、hold、ask，不能 allow |
| L3 | `user` | 用户权限规则；不能覆盖前层拒绝或仓库 ask |
| L4 | `mode` | 工作模式和计划门；计划 ask 直接进入绑定审批 |
| L5 | `posture` | suggest、auto、bypass、never 与评估效果 |
| L6 | `surface` | Sandbox、Rules、Skills、MCP 的进一步收紧 |
| L7 | `binding` | Full Access 预授权工具声明的一次审批；显式 ask 保留来源与单次作用域，强制新编辑计划仍生效；运行时出网目标阶段跳过绑定审批 |
| L8 | `auto_review` | 满足下表全部条件时，允许可自动审查的有界操作 |

| 条件 | 含义 |
| --- | --- |
| `auto_review_enabled` | 自动审查总开关启用 |
| `posture_requested_approval` | ask 来自 Posture，非 Managed 或 Repository ask |
| `medium_risk` | 风险为 medium |
| `bounded_effect` | 效果为 agent.lifecycle 或 network.read |
| `network_read_under_auto` | network.read 仅在 auto Posture 下自动审查 |
| `public_network_target` | 网络读不是本机目标或 loopback 权限 |
| `reusable_binding_approval` | 绑定未要求新的一次审批或新编辑计划 |
| `exact_typed_grant` | 存在可计算的精确类型化 Grant |

`fresh` 与 `fresh_once` 均不读取旧审批缓存；新的同意仅完成本次调用，不写缓存。
二者仅允许 once scope 并禁止参数替换。普通可复用审批替换参数后，先重新解析、评估
并检查策略。Journal 写还必须重建 Edit Plan 校验内容漂移。
工具绑定的单次审批使用 `tool_approval_required`，原因中包含工具名；已有的显式
策略 ask 保留原来的 Code 与 Layer，不被工具绑定改写成通用进程审批。

路径规则支持段内 `*`、`?`、`[...]`、独占一段的 `**` 和反斜杠转义。
未转义花括号、非法字符类、混合 `**` 拒绝加载；工作区字面花括号须正确转义。
restrictive 目录写规则按“授权子树与保护模式是否相交”判定，包含未来可创建的文件，
因此对 `app` 的树写不能绕过 `**/.env`。allow 必须覆盖被授权资源，不能用子路径的
允许扩大到整个目录。最终规范化 ResourcePath 也必须通过语法验证。
Guard 在独立的策略快照上解析规则路径，不改写共享 Runtime；不同工作区使用各自的
路径基准。新授权会采样最新规则与权限，已用于执行的快照保持不变。

## 一次授权编译

`authority.Compile` 验证 Prepared 与 Policy 共享同一 Assessment 和参数，然后一次
产生 Profile、Required Controls 和 ExecutionOperation。网络 Reach 从 Assessment
Facets 得到一次；Command Prepare 验证实际控制不宽于编译的控制，不重新解释资源。
独立后端能力探针没有执行 Lease，只能如实报告实测控制。
普通工具执行只通过 Compile 构造 Operation；File/Process Broker 的内部受管操作
使用各自的显式构造入口。工作区 Broker 与 Journal 的组合位于 orchestration，
底层租约消费和结算仍由安全 Broker 负责。

`Authority.Bind` 只附加 Artifact 与 File Mutation 证据、更新必要控制和摘要，
不读取文件或重新解析路径；`WithProfile` 使用已冻结绝对路径验证新根并重绑身份。
文件内容身份仍是初次编译时的 SHA-256；后续执行漂移由 Broker/Lease 校验。
Profile Schema 为 5，Operation Schema 为 3；旧回执不参与执行校验，不增加兼容迁移。
Darwin `/bin/sh` here-document 的平台例外由
`internal/security/sandbox/executable_contract.go` 中的包内数据表维护，包含来源说明；
沙箱编译器查询该表，不按具体可执行文件名分支。

## 权限模式

- `Read only`（`never`）：检查仓库，不允许修改。
- `Auto`（`auto`）：日常开发默认选择；普通操作自动执行，需要时请求审批。
- `Full Access`（`bypass`）：普通命令可读写宿主文件并直接联网，预授权工具声明的单次审批，包括 Git 推送；跳过常规 Posture 和验证计划审批。

Full Access 由可信 Binding 声明能力，Guard 在每次授权时从会话权限绑定事实，不能由模型
参数开启。Authority 将该事实编译进不可变 Profile、租约与执行回执的 `full_access`，
同一 Seatbelt 后端应用逐维授权。Full Access 使用允许普通 OS 操作的独立基线，再叠加
明确保护与资源限制；支持浏览器使用的 Mach/IOKit、PTY 和本地测试服务。普通测试与构建无需枚举输出目录、缓存
或网络目标；原有环境契约仍负责 HOME、临时目录、PATH 和选定的环境变量。

显式 `write_paths` 只将文件写入收窄到声明路径；读取与系统能力保持 Full Access。
`network_targets` 只将网络收窄到受管代理目标，`allow_loopback` 只将网络收窄到本地地址；
两者合用时保留代理目标和本地连接。网络参数不会取消文件写入权限，写入参数也不会取消
直接联网权限。`settle=discard` 使用原有隔离范围沙箱，`shell_read` 始终只读。
Full Access 保留 Guard、Constitution、Managed
Grant、仓库和用户 deny/hold/ask、Surface 收紧、显式强制编辑审阅、执行租约、审计以及 Sandbox。
工具的 `ApprovalPolicyOnce` 不再单独触发审批；如果上述显式策略仍要求 ask，继续
使用本次有效、不可替换参数的审批，不能复用历史授权。Auto 下工具的单次审批保持不变。
由于无范围命令可能访问任何资源，带资源条件的限制规则会保守匹配；需要限制到其他范围
时可显式声明资源；范围受限的 Managed Allow 也不能授予无范围命令权限。
凭据文件与目录、工作区各层控制目录和可信 Host 指定的 Runtime 状态根仍
受保护；状态根内仅保留已有环境明确选定的目录授权，子 Agent 继承这些保护。
Keychain 的保护同时覆盖文件与 Mach 凭据服务（SecurityServer、securityd、secd 和
systemkeychain），普通应用的其他 Mach/IOKit 访问保持可用。

回执的 `full_access` 表示采用 Full Access 基线，具体限制以 `effective_controls`、
`write_paths` 和 `network_mode` 为准。默认沙箱的 Syscall/IPC 回执为
`platform_filtered`，不声称没有过滤或具备独立命名空间；Full Access 的 Syscall 为
`unrestricted`，IPC 因凭据服务保护仍为 `platform_filtered`。这些值描述 QCode 的
控制，宿主系统权限依然有效。Prepare 后必须与冻结的 Full Access 各维授权一致。

macOS 的沙箱重入不能按权限包含关系推断：同一 Profile 重用成功不代表全新 Runtime
能应用另一个 Profile；实测更严格的子 Profile 也可能被内核拒绝。Full Access 下默认
命令仍保留 Seatbelt。需要创建子沙箱的测试可以明确请求下述宿主执行路径。

`exec_command.execution_target` 默认 `sandbox`；模型明确传入 `host` 时，复用现有
Permissions 和工具审批流程，不增加独立会话开关。Auto 要求当前命令的单次用户审批，
不接受缓存审批、永久放行或参数替换；Full Access 预授权该请求；Read only 拒绝。
审批卡展示命令及当前系统账户的文件、网络访问范围，明确不使用 QCode 沙箱。
宿主命令使用这一次审批，不再重复要求计划审批；Constitution、Managed Grant、
仓库/用户 deny、hold、ask、Surface 收紧、执行租约与审计继续生效。显式 ask 在
Full Access 下仍要求单次审批。子 Agent 保留隔离边界，不能请求宿主执行。

已经保存旧 `allow_host_execution` 字段的会话会在加载时清除该字段，保留原有
Permissions；旧字段的 true/false 值都不产生授权。清理通过会话版本校验写回，
不会重复增加版本；该字段不再属于公开协议或可编辑设置。

宿主执行使用 Runtime 的 Process Broker 和现有 SessionManager。租约绑定命令、固定环境、
cwd、所属线程和超时等具体启动参数；启动租约只结算一次，后续输入、轮询、关闭与回收
沿用进程会话的归属检查。回执为 `execution_target=host`、`enforcement=none`，文件与
网络无 QCode 隔离，IPC、系统调用及跨进程访问也不声称受限；保留进程组回收和 cwd
描述符身份校验。不得声称保护凭据、Runtime 状态或控制目录。只传递既有环境策略
选出的环境变量，不向命令注入服务凭据。

宿主执行拒绝 `write_paths`/展开的 `write_globs`、`network_targets`、`allow_loopback=true`
和 `settle=discard`，避免承诺不可执行的范围限制或回滚。任意宿主写入不可逆，无法保证
完整 Turn Diff；验证证据继续校验覆盖文件指纹。审批等待期间权限变更会重新校验，
切换 Read only 后尚未启动的宿主命令不能继续执行。
不会在沙箱失败后自动改为宿主重跑。如果 Runtime 自己已处于外层沙箱中，`host` 仍继承
该外层限制，必须从外层之外启动 Runtime 或测试；本路径不提供沙箱逃逸。

Full Access 的任意宿主副作用按高风险、不可逆进程记录，不生成全宿主 before-image，
也不保证其所有写入出现在文件工具的 Turn Diff 中。需要逐文件变更归因、Journal 回滚或
隔离结算时应使用文件工具或显式写入范围。验证证据在完成时比较覆盖文件指纹，使用旧
证据判定任务完成前再次核验，命令修改自身覆盖文件或之后文件变化均不能沿用通过状态。
切换权限影响后续新授权命令；已启动进程保留启动时冻结的权限，需停止后重新启动才能收紧。

Web 的默认姿态仍为 Auto；Host 显式声明 Session 可选上限为 Full Access，默认值与
权限上限分别传入 Runtime。其他调用方不声明上限时仍以启动姿态为上限，子 Agent
始终以父级当前有效权限收紧，Read-only Role 保持只读。
`suggest` 不再作为用户模式；旧 Session 加载后更新为 Auto。策略引擎仍保留该级别
作为内部权限上限，不能把这种硬上限自动放宽到 Auto。

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
  `[execution.environment]` 使用 `v1` 契约，主 Agent 默认 `native`，子 Agent 为
  `isolated`。HOME 变量不等于整个宿主 Home 的读写权限；工具链、配置、缓存和网络
  依照可信的 `resources` 声明与平台事实绑定。启动时不执行语言探测，不读取包清单。
  来源在准备时冻结，进程启动不重新读取宿主环境。`shared_user_temp` 默认关闭。
  Git 适配器在 native 下声明已存在的精确用户配置，凭证文件继续禁止暴露。
  OpenSSL 未显式配置时使用 `OPENSSL_CONF=`，按其公开空值语义不加载宿主配置；
  普通 Node/npm 启动不因此扩大文件读取权限。显式加密配置继续覆盖缺省值并接受
  资源授权，诊断工具也遵循同一配置。此行为不关闭证书验证；自定义 Provider、FIPS
  与宿主加密策略需要显式配置和文件授权。
  内置 GOPROXY 服务和 `auth_services` 配置已删除；私有制品认证由显式接入的受限
  外部服务负责。`credential/use` 只表达使用身份，无绑定器时报告未绑定。
  详见[执行环境配置](./configuration.md#执行环境)与[执行环境通用化方案](./environment-language-neutral-plan.md)。
  进程连接前审批只在所属工具调用存续期间有效；工具返回或取消后，新目标拒绝，
  未完成审批也取消。所属 Thread 轮询 Session 时，可在这次调用期间重新发起审批。
  代理拒绝使用 `network_target_unapproved` / `approve_network_target`，不把代理
  403 误报为上游故障，也不整段重放已执行命令。
- 受保护的控制面目录名与宿主凭据位置只在 `internal/security/pathpolicy` 定义一次，
  控制面分类、File Broker、Authority 的写拒绝根、MCP 隐藏路径、Seatbelt Profile
  与注入根校验都从这里读取。凭据位置按路径段、大小写不敏感匹配（`~/.config/ghostty`
  不会被当成 `~/.config/gh`）。Seatbelt 对 home 下每个凭据位置同时拒绝读写：
  `~/.ssh`、`~/.gnupg`、`~/.aws`、`~/Library/Keychains`、`~/.kube`、`~/.docker`、
  `~/.azure`、`~/.gcloud`、`~/.config/gh`，以及 home 下的 `credentials`、`secrets`、
  `keychains`。宿主注入根不能是 home 本身，也不能包含上述任一位置（例如
  `~/Library`、`~/.config`）；`.netrc`、`.git-credentials`、`.npmrc`、SSH 私钥等凭据
  文件无论在何处都不能注入。
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
  Path Identity、Artifact Origin 与 Durable Recovery。Sandbox Probe 与授权编译产生 Effective Controls，具体
  Command 只验证控制一致性，Lease 只在每个要求都被满足时签发。
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
  精确替换的字节匹配仍是首要前置条件；当字节匹配未命中时，工具先做两次有界
  恢复再判失败：`old` 的每个非空行都带编号输出前缀（如 `12:`）时剥离前缀重试
  一次；随后按行内空白与易混标点（智能引号、连字符、NBSP 等一对一折叠）在
  折叠视图中定位，并把命中投影回原始字节区间后只替换该区间——折叠文本本身
  永不写回，span 与 `old` 差距失衡或回投影校验失败时按原失配错误 Fail Closed。
  恢复路径写入的替换文本会对齐文件原貌：`new` 各行的缩进按 `old` 与文件命中行
  的缩进对应关系映射到文件实际缩进，span 起点之前的同行文本保留；换行符沿用命中
  位置的行尾（命中处为 CRLF 行时 `new` 中的 LF 转为 CRLF）；精确匹配的 `old` 不含
  `\r` 而命中处为 CRLF 行时同样按 CRLF 写入，不会产生混合行尾。
  出现次数语义在恢复路径下不变，工具结果以 `normalized_match` /
  `line_prefixes_stripped` 元数据如实标注恢复来源。
- `file_read` 的文本窗口按行流式读取：单行超过公开的按 rune 上限时在返回内容中
  原地截断并以 `truncated_lines` 元数据列出被截断行号，丢弃的尾部仍参与二进制
  与 UTF-8 校验；超过旧 1 MiB 扫描器上限的超长行不再使整个读取失败。
- File Broker 拒绝 Symlink、Hardlink、Device Boundary、Root/Parent Replacement，
  并在自身边界拒绝 `.git`、`.qcode`、`.qcode-worktree`、`.agents` 和
  `.codex`。Unified Diff 先解析为 File Plan，不调用 `git apply` 修改 Workspace。
- `exec_command` 的写权限只授予显式 `write_paths`。目标可以是现有普通文件、位于
  已存在父目录下的待创建文件，或**已存在**的工作区子目录（有界树写）。Guard 在
  执行前完成 Preflight；Strong Sandbox 对精确文件物化最小占位，对已存在目录授予
  树写。工作区根、缺失目录、Symlink、受保护元数据、重复路径和执行前发生的身份
  漂移均拒绝。树内新建文件不必再逐条列出。带写树的 `exec_command` 在隔离
  执行工作区运行，退出后经 File Broker / Journal 三方结算；用户并发修改不
  自动算 Agent 修改，重叠冲突拒绝。`Isolator` 不可用时保持原地树写；隔离
  准备失败则拒绝并要求精确 `write_paths`。隔离 cwd 出现在结果的
  `isolated_cwd`。
- `write_paths` 的公开上限为 512 条授权路径，目录算一条；`write_globs` 展开后的
  精确路径也计入该额度。目录内已有文件和实际变更文件不使用这个计数上限。
  因此依赖目录包含超过 512 个文件不会阻止构建；仍应声明所需的最小输出或缓存目录。
  写树失效等执行前拒绝返回可恢复工具结果，带 `check_write_paths` 提示，允许 Agent
  调整调用。审批期间目录被删除也会在执行前重新检查。
- 命令隔离使用私有文件系统副本及独立 Git 基线，保留被忽略的依赖文件、空目录和
  符号链接；不把依赖快照加入父仓库的 Git 对象库。受保护控制目录继续排除，符号链接
  不递归跟随。隔离路径的绝对 `cwd` / `write_paths` 按父工作区相对位置映射。
  Guard 不重复快照隔离写树；实际变更仍由 File Broker / Journal 三方结算，
  忽略文件的新增和修改也必须经过该边界。目录遍历和复制响应调用取消。
  私有基线初始化经 VCS Broker 独立租约执行；命令授权按同一相对路径映射到副本，
  保留网络、进程及必需控制约束，目录失效时禁止退回父工作区执行。
  实际结算 diff 的预览字节预算独立使用
  `execution.workspace_merge_max_diff_bytes`，见[配置说明](./configuration.md)。
- 配置后，写入型 Subagent 使用 Worktree。
- 隔离 Worktree 仅可只读访问经过校验的自身 Git Administration Directory，以及
  Repository Common Git Directory 中必要的 Object、Ref 与配置路径；这不会授予
  Parent Worktree 或 Git Metadata 写权限。
- Git Worktree Registration、Index 和 Ref 不属于普通 Workspace 文件。Child
  Worktree Add/Remove/Prune、Chat Baseline，以及模型发起的 `add`、`commit`、
  `switch`、`fetch`、fast-forward `pull` 和非 force `push` 只能由 VCS Broker
  执行。每次白名单 Mutation 绑定 Common Git Directory Identity、目标 Worktree
  HEAD/Ref、Index Digest 和 Worktree Registration Digest；执行接管前发生漂移即拒绝。
  Broker 只复制已准备策略中的环境值，并将其纳入进程租约摘要；不重新捕获宿主
  环境，不允许命令声明覆盖 HOME 或临时目录。`native` 使用所选用户 Git 配置，
  `isolated` 和私有内容基线禁用全局及系统 Git 配置；环境值不增加文件或网络授权。
  远端写入使用不可逆高风险 Effect；Auto 下要求单次审批，Full Access 预授权该工具审批。
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
  Subject、Policy 和 Sandbox 绑定。Policy Decision 同时记录 Action、Layer 与 Code；审批前拒绝记录 PolicyDenial。
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
  Effect 被归类：`Auto` 可以自动 Review 满足条件的精确只读目标。只读目标仅指限定为 `GET`、`HEAD`、`OPTIONS` 的明文
  HTTP 目标；HTTPS 的 CONNECT 隧道无法约束方法、可以上传任意数据，未限定方法或
  含其他方法的目标同理，都归类为 Network Mutating（高风险），`auto` 下也必须人工
  审批。运行时发现的 CONNECT 使用同一分类。Sandbox 只能连接代理端口，直连和未声明目标均 Fail
  Closed。该 Loopback Proxy 返回 CONNECT 403 表示目标未声明或未授权，并不表示
  远端服务不可达。
  macOS 的代理能力通过启动时的精确端口允许/拒绝探测单独确认，不从默认禁网状态
  推断。获批目标的 Effective Profile 保留代理端口，进程环境注入 Runtime 代理；
  没有声明目标且没有用户声明环境网络时，命令仍禁网，也不注入代理变量。
  用户声明的环境网络资源可被空 `network_targets` 继承到当前 Session Gate；
  工具配置或环境变量中的主机名本身不授予 CONNECT。
- 测试 Fixture 或本地开发服务必须绑定并连接临时 Localhost 端口时，
  `exec_command` 可声明 `allow_loopback`。
  该能力默认关闭。Seatbelt 只能按“本机任意端口”放行，无法限定到 Fixture 端口，
  因此 Loopback Grant 如实建模为可连接本机任意端口：它同样能连到其他本地服务，
  以及其他 Session 和 Workspace 的代理通道。Strong Sandbox 内仅包含 Localhost
  Grant 且没有 Workspace 写入的调用仍归类为 Network Read，但 `Auto`
  必须人工审批，不会自动 Review。代理通道之间的隔离不依赖端口不可达，
  而依赖各通道的独立凭据，见“网络与服务暴露”一节。
  macOS Profile 只增加 Localhost Inbound/Outbound Seatbelt Rule；非 Loopback
  流量仍必须声明精确 Proxy Target。Loopback-only Effective Profile 不绑定托管
  代理端口；执行器不得因为 enclosing sandbox 仍持有 Runtime Proxy 而拒绝已批准的
  Localhost Grant。该调用的命令策略与环境均移除代理配置，Prepared Controls 验证
  编译出的 `loopback_any`；同时获批 Proxy Target 与 Loopback 的调用
  仍使用 `proxy_targets` 并保留代理端口。共享 Workspace Policy 不随单次调用改变。
  Authority 编译时从统一 Assessment 确定租约的 Required Controls，与 Effective Profile
  保持一致；主机排序和参数顺序不改变混合调用的控制要求，也不隐式授予 Loopback。
  若代理端口仍与 Profile 错位，工具结果必须带
  `required_action=keep_allow_loopback_omit_network_targets`，不能把临时端口
  写进 `network_targets`。Effective Profile 与 Attempt Receipt 都会记录该
  Loopback Grant。
- 测试、构建和检查使用普通 `exec_command`，经过相同 Guard、审批、Journal 和
  Sandbox。不存在验证声明带来的特权或覆盖门禁；Verifier 子代理的权限仍由角色
  策略限制，模型可以执行允许的普通检查命令。
- Language Server 按文件类型选择实际安装的 Server，进程在 Workspace Read-only、
  Network Denied 的 Strong Sandbox 中运行。format、code action 和 rename 只返回
  edits，不直接取得文件写权限。
- `format_code` 的写权限限定到请求中的精确文件，并使用 before-image Transaction；
  `debug_run` 只接受经过校验的 Symbol 或 `file:line` 断点，不接受任意 LLDB Command；
  `dependency_resolve` 禁用安装脚本并保持 Workspace Read-only。
- `web_run` 使用独立临时 Chromium Profile，不复用用户浏览器 Profile。浏览器交互和
  通用 `http_request` 都按不可逆 External Mutation 声明单次审批，Auto 下逐次确认，
  Full Access 预授权该工具审批；Loopback 导航必须
  显式声明。`http_request` 拒绝 Authorization、Cookie 和 API Key Header，并从返回
  Metadata 中删除 Set-Cookie 与认证挑战 Header。
- `web_run` 的 Chromium 所有流量都经过 Runtime 自有的 loopback 代理和浏览器专用
  Egress Gate，页面子资源、重定向、脚本 `fetch` 和 WebSocket 都不能绕开：
  - 启动参数指定 `--proxy-server`，并用 `--proxy-bypass-list=<-loopback>` 取消
    Chromium 对 localhost 的默认直连；`--host-resolver-rules` 让浏览器自身的域名
    解析一律失败，WebRTC 禁止非代理 UDP。域名由代理解析，并按解析结果钉住连接地址。
  - 解析结果全部是公网地址的目标直接放行，页面能正常加载 CDN、字体和第三方资源。
  - 内网、回环、链路本地（含云元数据地址）等非公网目标，只有已批准的 `web_run`
    调用授权过的目标才放行，私网权限沿用 Web URL 授权的“授权时解析”规则。授权在
    浏览器会话内有效，浏览器关闭后全部作废。未直接写明回环或链路本地地址的域名，
    即使获授权也不能解析进这类地址。
  - 已知限制：通过代理的 `ws://` 以 CONNECT 发起，本地开发服务的 WebSocket（如 HMR）
    不匹配 `http://` 授权，会被拒绝；页面本身仍可加载。
  - 浏览器代理也使用独立随机凭据。Chrome 启动参数中的代理地址不含 userinfo；
    Runtime 经私有 CDP pipe 响应代理认证，只向本通道端口、指定 realm 的 Basic
    Proxy 挑战提供凭据。源站挑战、其他代理和重复挑战均拒绝。CDP 不监听 TCP 端口。
    持有 `allow_loopback` 的其他命令能连接端口，但无凭据时返回 407。
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
  产生冲突或丢弃内容的操作在 Auto 下要求单次审批，Full Access 预授权该工具审批。
- 开发服务默认通过 `exec_command` 在沙箱内运行，按需声明 `allow_loopback`。
  必须创建独立 OS 沙箱的 Fixture 明确请求 `execution_target=host`，经单次审批或 Full Access 预授权，
  仍走 Guard、Authority 和 Process Broker；观察到服务存活不能当作测试通过。
- Command Policy 使用 Bash AST 与 Static argv Segment。Managed Authority 定义
  Ceiling，Repository 只能收紧，User Approval 不能覆盖高权 Deny/Ask。Policy Reload
  原子发布新 Revision，并绑定到 Profile Provenance。
- `command_prefix` 规则按动作区分匹配方式：
  - `allow` 只匹配单段、静态、非解释器负载的命令，argv 逐词字面比较，不展开路径或
    包装命令，因此 `/tmp/x/git status` 不会命中 `git status` 的放行规则。
  - `deny`、`hold`、`ask` 回答“这条命令是否可能执行该前缀”，并对无法证明的情况
    Fail Closed：
    - 命令无法解析时视为命中。
    - 动态词（命令替换、变量展开）可匹配任意前缀词。
    - 可执行文件按去掉反斜杠后的文件名比较，`/usr/bin/git`、`\git` 都等同 `git`。
    - 前缀词之间允许插入选项及其参数，如 `git -C dir push`。
    - 穿透 `command`、`exec`、`env`、`nice`、`timeout`、`sudo`、`nohup`、
      `xargs`、`find -exec` 等包装命令；`xargs` 和 `find` 从输入补全 argv，前缀
      被截断也算命中。
    - `eval` 与 `sh -c` 的文本递归解析，解析失败视为命中。
    - `python -c` 等解释器程序文本包含全部前缀词时视为命中。
  - `env -S` 等无法静态确定 argv 的写法标记为动态，放行规则不会命中。
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
  macOS 下父 shell 正常退出后，Session 先终止同组残留后台进程，再发布
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
`connection/add` 与 `setup/apply` 使用同一套暂存—激活流程：Setup Record 保存成功后、
Runtime 切换前先把暂存 Intent 推进到已提交，激活失败时恢复原 Setup Record 并清理
暂存。未激活的暂存 Entry 会被后续不带 Key 的 Probe 或重启视为 Orphan 删除，因此新增
连接必须完成激活才算成功。

Web Host 的暂存 → 激活 → 提交只由一个事务类型 `credentialRotation`
（`internal/host/credential_rotation.go`）驱动。Launcher、无 Workspace 配置、
Runtime 重建和 `connection/add` 四处都复用它：

- 阶段只前进，未激活不得提交；
- 暂存或已激活时回滚会恢复旧 Reference，提交后回滚为空操作；
- 暂存 Reference 只写回所属连接，`connection/add` 为非默认连接暂存的 Key 不会成为
  默认连接的凭证，也不会进入默认 Runtime。

`TestCredentialProtocolLivesInRotation` 禁止在该类型之外直接调用 Credential Control
的协议方法。

运行：

```bash
make secret-leak-test
```

## 网络与服务暴露

- 服务默认监听 `127.0.0.1`。
- 非 Loopback 部署必须使用经过 Review 的认证网关。
- Web Host 的浏览器认证使用一次性启动码换取会话 Cookie：
  - 启动器打开或打印的地址带 `?launch=<启动码>`。启动码只能兑换一次，未兑换的
    启动码在 `capacity_defaults.launch_code_ttl_seconds`（默认 300 秒，见
    `docs/protocol/web-host.contract.json`）后失效。
  - 服务端把启动码兑换为 `HttpOnly`、`SameSite=Strict` 的会话 Cookie，Cookie 名带
    端口后缀；随后 303 跳转到去掉启动码的地址。未知或过期的启动码不设置 Cookie。
  - 会话 Cookie 与 Owner Lease 中的 Capability Token 是两个独立的随机值，Token
    不能当作 Cookie 使用。Host 重启后旧 Cookie 失效，需重新运行 `qcode` 或重开
    桌面 App 获取新链接。
  - `/api/v1/bootstrap` 不再下发任何凭证。无有效会话时只返回协议版本、构建和就绪
    状态，不包含 Workspace 路径或目录列表。
  - Bearer Capability Token 只供进程外属主使用（重复运行的 `qcode`、桌面壳），
    从仅当前用户可读的 Owner Lease 读取。只有持有该 Token 的调用方才能通过
    `auth/launch-code` 申请新的启动码，浏览器会话不能申请。
  - 浏览器共享 `127.0.0.1` 所有端口的 Cookie。本机其他端口上的服务若被浏览器
    访问，可能收到该 Cookie；这是本机信任边界内的已知限制。
- Provider Base URL 与 Redirect 属于安全敏感配置。
- Provider、Web 工具和进程代理分别使用独立 Egress Gate。固定 Provider Endpoint
  与 Web Search Backend 的授权不会授予进程代理；Guard 按可信 Tool Capability
  将获批 Web 目标交给 Web Gate，工具审批不修改 Provider Gate，也不写入
  Workspace 共享进程 Gate。
- 同一个 Gate 按 Protocol、Host、Port 累计授权，新增 Method 不撤销已有 Method。
  `allow_private` 只作用于同次获批的 Method；例如公网 GET 与私网 POST 合并后，
  私网 GET 仍拒绝。空 Method 授权表示所有方法，后续精确方法授权不能将其收窄；
  请求省略 Method 时则必须具有所有方法的授权。
- Web 工具的 URL 授权按授权时的解析结果决定私网权限，不再一律授予：
  - URL 直接写回环、链路本地（含云元数据地址 `169.254.169.254`）、未指定地址或
    `localhost` 名称时，`auto` 也不会自动放行，必须人工审批。审批通过后允许访问
    该字面地址。
  - 域名在授权时解析：解析结果含回环或链路本地地址时不授予私网权限；否则只要含
    内网地址（`10/8`、`172.16/12`、`192.168/16`、ULA 等）就授予私网权限，内网域名
    照常可用。解析失败时不授予私网权限。
  - Web Gate 在每次请求时重新解析。未直接写明回环或链路本地地址的主机，即使
    解析到这类地址也会被拒绝，因此 DNS 重绑定不能进入本机服务。
  - 地址分类只在 `internal/security/netpolicy` 实现一次，分为本机（回环、未指定、
    `0.0.0.0/8`、链路本地及链路范围组播）、私网（RFC 1918、`100.64/10`、ULA）、
    其他非公网保留段、公网四类；IPv4 映射、IPv4 兼容、NAT64 `64:ff9b::/96` 与
    6to4 形式按内嵌的 IPv4 地址分类。判断"主机名是否直接指向本机"用的也是这套分类，
    所以 `::ffff:127.0.0.1`、`64:ff9b::7f00:1` 与 `127.0.0.1` 一样需要人工审批。
  - 已知残余风险：在 `auto` 下，解析到内网地址的外部域名仍会获得内网访问权限。
    需要隔离内网时应配置显式网络限制；审批模式不替代网络隔离。
  Darwin 上每个 Process Session 绑定独立 loopback 端口和 Session Gate；兄弟命令、
  子 Agent 和 Workspace 共享端口不能消费该 Session 的目标。
  每个代理通道（Workspace 通道和每个 Session 通道）在创建时生成独立的随机凭据，
  只注入该通道所属命令的 `HTTP_PROXY`/`HTTPS_PROXY` URL userinfo，
  不写入 Receipt 或 Journal，通道关闭即失效。CONNECT 与 absolute-form 请求必须以
  `Proxy-Authorization: Basic` 出示本通道凭据，否则返回 407。认证后的 origin-form
  请求返回 400；代理认证头不传给上游，普通目标的 Authorization 保留原义。
  持有 `allow_loopback` 的命令拿不到其他通道凭据；命令能读取自己的代理 URL，
  该临时凭据仅在自己的通道和 Gate 中有效。
  取消或关闭 Session 会先标记通道关闭、停止监听并回收已 hijack 的 CONNECT。
  登记隧道和关闭共享同一把锁；已关闭通道拒绝新连接，不在 Close 返回后确认隧道。
  Workspace 代理关闭后也不能创建新的 Session。
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

Provider Debug Dump 的实现位于 `internal/adapter/provider/httpclient/debug_dump.go`，
与 HTTP 错误处理同属一个包。当前仅在非 2xx 响应时检查 `QCODE_PROVIDER_DUMP`
开关并生成诊断文件；写入失败不改变原有 Provider 错误。

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
