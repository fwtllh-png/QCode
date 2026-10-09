# 安全策略模型收敛方案

状态：Phase 0–5 已完成并通过验收。日期：2026-09-29。

第 1–3 节保留方案起草时的问题与目标；现行交付边界以第 4 节实施记录和
[安全文档](./security.md)为准。Phase 5 在 Phase 0–4 验收后继续实施，记录见下文。
范围：`internal/security/{policy,authority,controlmatrix,controlplane,constitution,permissions,egress,sandbox}`
与其唯一调用方 `internal/adapter/tool/guard`（生产代码约 3,750 行）。
依据：对上述包的逐文件代码审计；所有"重复定义""调用点"论断均经全仓 `rg` 验证。

本文只处理**决策模型**：一次工具调用"是什么、风险多大、允许做什么、由谁执行"
如何被表示和计算。Sandbox 执行环境、网络通道生命周期、凭据代理与进程生命周期的
实现缺口已由以下合同覆盖，本文引用而不重复：

- [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)：环境契约总纲；
- [执行环境通用化方案](./environment-language-neutral-plan.md)：资源绑定与实机验收；
- [安全模型与运维](./security.md)：代理、审批、Lease 和 Broker 的现行合同。

## 1. 结论

问题不是零散 bug，而是**同一个安全事实被多个包各自推导，且各自使用不同词汇**。
补丁式代码正是在这些推导之间的缝隙里累积的：

1. **推导重复**。Effect 在 `policy.NormalizeEffect` 中由资源推导，Guard 在审批事件、
   观测、Operation 构建处各调用一次；网络控制要求由 `authority.compileSandboxCeiling`、
   `guard.requiredControls`、`sandbox.CommandControls` 三处独立计算；决策由
   `policy.evaluate` 给出后，Guard 又在 `authorize` 中按 `forceEditPlanApproval`、
   `ApprovalPolicyOnce`、控制面写入检查再改写。
2. **词汇不统一**。资源类型是字符串（`file`/`directory`/`repo`/`workspace`/`host`/
   `url`/`process`/`agent`/`plan`/`parallel`/`sandbox`/`env`/`session`），每个包只解释
   其中一部分；Effect 词汇在 `tool/contract.go` 与 `policy/effect.go` 各有一份；
   `Enforcement` 用 `"strong"`/`"none"`，`NetworkAuthority.Mode` 用
   `"managed"`/`"direct"`/`"loopback"`，`controlmatrix.Network` 用
   `"proxy_targets"`/`"direct"`/`"loopback_any"`，`profile.go` 在无法执行时把后者直接
   写进前者。
3. **工具与生态知识进入安全核心**。`policy/planning.go` 硬编码 `exec_command`、
   `git_push` 与 `spawn_agent` 的 `review`/`explore`/`awaiter` 角色；`granular.go`
   保留 `legacy:skills_read:` 前缀；`egress` 硬编码 GOPROXY 拒绝文案；Seatbelt
   编译器内嵌 `/bin/sh` here-doc 特判。
4. **分层倒置**。`policy`、`authority`、`filebroker` 依赖 `internal/adapter/tool`，
   即安全词汇由适配层定义；`plandrift` 依赖 `internal/runtime/protocol`；
   `egress` 以鸭子类型 `InnerBackend()` 逐层解开 `sandbox.Backend` 装饰器。
5. **存在已确认的安全缺陷**（第 2.3 节），其中 `allow_loopback` 一项为高危。

目标是一条单向管线：每个阶段产出一个不可变值，后续阶段只消费、不重算；安全核心
不认识任何具体工具名或语言生态名。

## 2. 现状诊断

### 2.1 当前决策链

```text
Guard.resolveResources        按 ResourceResolver 字段把参数解析为 []tool.Resource（字符串 Kind）
  → Guard.checkControlPlaneWrites / preflightFileWrites   策略之前的硬拒绝
  → policyInput → policy.Evaluate
       NormalizeEffect        位掩码 + 工具名特判 + EffectFixed 覆盖
       strongestMatch × 3     managed / repository / user 规则
       planningDecision       解析 exec_command / spawn_agent 参数
       permissionDecision     posture × risk
       auto-review            内联复合条件
       ApplySurfaceTightening 按 Source 前缀分类
  → Guard.authorize 改写      forceEditPlanApproval、ApprovalPolicyOnce
  → authority.Compile         compileResources 再解释资源 Kind；compileSandboxCeiling 再算网络模式
  → Guard.requiredControls    第三次解释资源 Kind 得到 Required Controls
  → authority.BuildExecutionOperation   再调 NormalizeEffect；normalizeResource 第四次解释 Kind
  → sandbox.CommandControls   第五次计算网络与写控制
  → seatbeltProfileForCommand 按布尔参数生成 SBPL
```

### 2.2 重复定义清单

| 定义 | 位置 | 备注 |
| --- | --- | --- |
| 受保护控制面目录名 | `controlplane/classifier.go`、`filebroker/broker.go`、`authority/profile.go`、`adapter/mcp/runtime_authority.go` | `profile.go` 缺 `.codex` |
| Effect 词汇 | `adapter/tool/contract.go`、`security/policy/effect.go` | 值相同，类型不同 |
| 网络目标解析与默认端口 | `policy.ParseNetworkTarget`/`defaultNetworkPort`、`egress.normalizeTarget`/`requestPort`/`defaultPort`/`splitAuthority`、`authority.normalizeNetworkResource`、`goproxy.effectivePort` | 至少 5 处 |
| 本机/私网地址判定 | `policy.NamesHostLocal`、`egress.hostLocalIP`、`egress.nonPublicIP` | 三者定义不一致 |
| 允许缺失的路径规范化 | `authority.canonicalPathAllowMissing`、`sandbox.evalSymlinksAllowMissing`、Guard `canonicalMissing` | |
| 敏感凭据路径 | `sandbox.sensitiveCredentialSegments`、`validateInjectedRoot` 的 home 列表、Seatbelt profile 的 home 拒绝列表 | 三份内容不同 |
| loopback 伪资源 | `policy/effect.go`、`policy/network.go`、`authority/operation.go`、`authority/profile.go`、`sandbox/authority.go`、Guard 两处 | `host`+`protocol=loopback`+`loopback://localhost:0` |

### 2.3 已确认的安全缺陷

以下缺陷独立于重构，进入第 4 节 Phase 0 优先修复。

**S1【高】`allow_loopback` 实际授予本机任意端口，且 `auto` 下自动批准。**
`sandbox/backend.go` 为 loopback 生成 `(remote ip "localhost:*")`，与控制名
`NetworkLoopbackAny` 及 [安全文档](./security.md) 的"精确 Localhost Grant"不符。
`NormalizeEffect` 将其归为 `network.read`/`medium`，`targetsHostLocal` 又显式跳过
`protocol == "loopback"`，因此 `auto` 走 `auto_review_allowed`——与同函数注释
"Loopback services ... are never auto-reviewed"自相矛盾。进程代理通道
（`egress/session.go`）不认证客户端，连接即授权，于是一个自动批准的 loopback 命令可以：
使用兄弟 Session 通道上已获批的目标（推翻"兄弟命令不能消费该 Session 的目标"）；
访问 Workspace 通道上的 GOPROXY 认证服务并借用宿主凭据；访问本机数据库、Docker 等服务。

**S2【中高】Constitution `deny_write_globs` 不做 glob 匹配。**
`constitution.normalizeGlob` 只剥离尾部 `/**`、`/*`、`/`，规则按字面相等或前缀匹配。
`*.pem`、`**/.env` 永不命中；模板默认的 `.env` 只保护根目录一个文件。安全配置遇到
不支持的语法应拒绝加载，而非静默失效。

**S3【中】写判定只认 `AccessWrite`。**
`policy.ruleMatches` 的 `RequireWrite` 分支用 `resource.Access != tool.AccessWrite`
跳过资源，`AccessTree` 不被视为写；`filebroker.validateOperation` 却把两者都当写。
当前 Guard 恰好都产出 `AccessWrite`，缺陷未被触发，但判定必须收敛到单一谓词。

**S4【中低】地址分类不完整。**
`egress.nonPublicIP` 未覆盖 NAT64 `64:ff9b::/96`（NAT64 网络中可映射到 `10.0.0.0/8`）、
6to4 `2002::/16`、`0.0.0.0/8` 中非 `0.0.0.0` 的地址及其他 IANA 特殊用途段。

**S5【中低】Egress Gate 默认放行。**
`Gate{Enforce:false}` 与 nil `*Gate` 直接放行，注释说明是为单测。生产代码全部设置
`Enforce: true`，该开关只剩默认放行风险。`transport.RoundTrip` 仅在 base 为
`*http.Transport` 时做 DNS 钉住，否则静默退化。`serveForward` 的 Transport 泄漏见
深度审计 A5。

敏感凭据路径名单的缺口（子串匹配漏掉 `~/.config/gcloud`，同时误伤
`~/.config/ghostty`、`secrets-manager` 一类路径）由深度审计 D6 继续跟踪，本文第 3.5 节
只改变其表示方式。

## 3. 目标模型

### 3.1 原则

1. **一个事实只算一次**。Resolve、Assess、Decide、Compile 各产出不可变值，
   下游只消费，禁止回头重新解释资源或参数。
2. **安全核心不认识工具名与生态名**。工具差异通过 `TrustedBinding` 上的声明式属性
   进入；`internal/security` 不出现具体工具名、语言变量名或产品名。
3. **词汇类型化且归 `internal/security` 所有**。适配层把自己的表示映射进来，
   `internal/security` 不导入 `internal/adapter`、`internal/runtime`、`internal/persist`。
4. **未知即拒绝**。不存在关闭执行的开关；配置语法不支持即加载失败；
   新增枚举值在所有 `switch` 中默认拒绝。
5. **授权范围即执行范围**。OS 层实际授予的范围必须与审批展示、Receipt 记录的范围
   一致；做不到精确时，控制名与风险分类必须如实表达"宽"。
6. **表有出处**。地址段、受保护路径、凭据位置等名单是带出处的公开数据表，
   满足 AGENTS.md 对安全常量的要求。

### 3.2 目标管线

```text
TrustedBinding + Arguments
  → Resolve   Guard：按 Binding 声明解析参数 → []resource.Resource（类型化）
  → Assess    security/assess：Assessment{Resources, Facets, Effect}，纯函数
  → Decide    security/policy：Decision{Action, Code, Layer}，唯一决策入口
  → Compile   security/authority：Authority{Profile, Required, Operation}，一次编译
  → Enforce   sandbox / egress / 各 Broker 只消费 Authority
```

Guard 保留编排职责（解析、审批等待、租约、执行、结算），不再持有任何分类或决策逻辑。

### 3.3 Resource 类型化

新增叶子包 `internal/security/resource`，不依赖任何 QCode 包：

```go
type Kind uint8     // Path, Network, Loopback, Process, Agent, Plan, Session, Credential, HostConfig
type Access uint8   // Read, Write, Tree, Use

func (a Access) Writes() bool // Write 与 Tree 为真；S3 的唯一判定

type Resource struct {
	Kind     Kind
	Access   Access
	Path     string        // Kind == Path：Guard 已规范化的绝对路径
	Tree     bool
	Network  *netpolicy.Target
	Loopback *LoopbackGrant
	ID       string        // Agent / Plan / Session / Credential / HostConfig 的标识
}
```

- `repo`/`workspace`/`directory`/`file` 合并为 `Path`，目录性由 `Tree` 表达；
  `parallel` 不是安全资源，从安全资源列表移除。
- `host`/`url` 合并为 `Network`，携带统一的 `netpolicy.Target`（第 3.5 节）。
- loopback 成为独立 `Kind`，不再伪装成 `host`，彻底删除 `loopback://localhost:0`。
- `tool.Resource` 保留为适配层表示；Guard 提供唯一映射函数。Web 协议使用独立的
  `protocol.CanonicalResource`，不受影响。

### 3.4 Assessment：Effect 只推导一次

新增 `internal/security/assess`，把 Effect 词汇从 `adapter/tool/contract.go` 移入
`internal/security/effect`（叶子包，`adapter/tool` 反向引用）。

`Assess(binding, resources) Assessment` 分两步：

1. **计算 Facets**：从类型化资源得出一组布尔或枚举事实，例如
   `WritesWorkspace`、`WritesHost`、`Egress{None, SafeRead, Mutating}`、
   `LoopbackReach`、`HostLocalTarget`、`SpawnsAgent{ReadOnly, Mutating}`、
   `PlanOnly`、`StrongSandbox`、`Journaled`、`DeclaredVerification`。
   `processEgressCanWrite` 的逻辑（HTTPS CONNECT 无法约束方法）下沉为
   `netpolicy.Target.CanCarryData()`，在资源构造时确定。
2. **查表得 Effect**：`EffectFixed` 的 Binding 直接使用声明值；`EffectDerived`
   按一张有序决策表取第一条命中行得到 `(Kind, Risk, Reversibility)`。
   该表是公开合同，文档与表驱动测试一一对应，取代 `NormalizeEffect` 中的位掩码。

工具特判改为 Binding 声明：

| 现状特判 | 目标 |
| --- | --- |
| `readOnlySpawn` 解析 `spawn_agent` 的 `role` | agent 适配器的 ResourceResolver 对只读角色产出 `Access=Read` 的 Agent 资源 |
| `declaredVerification` 解析 `exec_command` 参数 | Binding 已有的 verification 字段由 Guard 解析为 `DeclaredVerification` Facet |
| `planningExemptTool("git_push")` | `EffectContract` 增加 `Planning` 属性（`default`/`exempt`），由 git 适配器声明 |
| `ClassifySurface` 的 `legacy:` 前缀 | 删除；Surface 由 Binding 的来源类型决定 |

### 3.5 公共数据表

| 新包 | 内容 | 取代 |
| --- | --- | --- |
| `internal/security/netpolicy` | `Target{Scheme, Host, Port, Methods}` 与唯一解析器 `ParseTarget`；`Classify(ip) Reach`，`Reach ∈ {Public, Private, HostLocal, Reserved}`，数据来自 IANA IPv4/IPv6 Special-Purpose Address Registry，内嵌 IPv4 的 IPv6 形式（映射、NAT64、6to4）先提取再分类；唯一的 `NamesHostLocal` | 第 2.2 节网络目标与地址判定各项，修复 S4 |
| `internal/security/pathpolicy` | 受保护控制面目录名；凭据存储位置按"根（HOME / XDG_CONFIG_HOME）+ 路径段"结构化表示，按路径段匹配而非子串 | 受保护目录名与敏感凭据路径各项 |

所有消费方（controlplane、filebroker、authority、Seatbelt 编译器、MCP authority）
只引用这两张表。

### 3.6 Decide：唯一决策入口

`policy.Decide(assessment, snapshot) Decision` 吸收 Guard 当前的所有改写，
按固定层次求值，`Decision.Layer` 记录作出决定的层，写入 Attempt Receipt：

```text
L0 输入无效或未校验                       → deny
L1 硬约束：控制面写入、Constitution、Managed deny/hold → deny/hold
L2 Repository 规则                        → deny/hold/ask
L3 User 规则                              → deny/ask/allow
L4 Mode 与 Planning                        → hold/ask
L5 Posture × Effect Risk                  → allow/ask/deny
L6 Surface 收紧                            → ask/deny
L7 Binding 审批策略（once；Full Access 预授权）与显式强制编辑审批 → ask
L8 自动审查资格                           → allow（显式谓词表）
```

- 控制面写入检查（现 `Guard.checkControlPlaneWrites`）并入 L1，使用 `pathpolicy`。
- `forceEditPlanApproval` 成为 Snapshot 字段，并入 L7。
- L8 的资格是一张谓词表：Effect 种类、风险、Posture、`HostLocalTarget` 与
  `LoopbackReach` 均为假。S1 由此从结构上关闭。
- 规则资源匹配改用编译后的匹配器：路径规则支持单段 `*`、`?`、`[...]` 与整段 `**`，
  其他语法在加载时报错（修复 S2）；`RequireWrite` 改用 `Access.Writes()`（修复 S3）。

**审批 Grant 的合同**：沿用 [安全文档](./security.md#进程执行) 的静态
argv 前缀 grant，但明确其安全边界是 **scope**，不是命令语义。scope 指纹在
cwd 与资源之外增加 Assessment 的 Effect 与 Facets 摘要：argv 扩展后若 Effect 类别
或 Facets 改变（例如新增网络或写入），前缀不再命中。

### 3.7 Compile：一次编译

`authority.Compile(assessment, decision, sandboxPolicy, capability) (Authority, error)`
同时产出 `EffectivePermissionProfile`、`RequiredControls` 与 `ExecutionOperation`：

- `guard.requiredControls` 与 `BuildExecutionOperation` 中的 Effect 推导并入此处；
- `Enforcement` 与网络模式改为类型化枚举，删除 `NetworkAuthority.Mode` 字符串，
  直接使用 `controlmatrix.Network`；
- 受保护写根来自 `pathpolicy`；
- `sandbox.CommandControls` 改为校验 Prepare 结果与 `Authority.Required` 一致，
  不再独立计算。

### 3.8 Enforce：执行层只消费

**Egress Gate**：

- 删除 `Enforce` 字段；nil `*Gate` 一律拒绝。
- 用构造函数替代布尔组合：`NewStaticGate(targets)`（Provider、固定端点）、
  `NewCallScopedGate()`（Web 工具）、`NewSessionGate(targets)`（进程 Session）、
  `NewBrowserGate()`（公网直通、非公网需授权）。`modelDriven := scoped || AllowPublic`
  这类从标志反推语义的代码随之消失。
- DNS 钉住由 Gate 自身的 `DialContext` 保证，不依赖 base Transport 的具体类型；
  共享 Transport 同时解决深度审计 A5。
- 代理通道增加每 Session 随机凭据，通过代理 URL 的 userinfo 注入进程环境，
  服务端校验 `Proxy-Authorization`，origin-form 不再承载认证服务。本机端口可达
  不再等于授权。
- 通用化 P3 已删除 `goproxy` 及其唯一消费的 `UpstreamAuthService` /
  `BoundOrigins` 接口，不再迁移同一实现或保留协议服务绑定器。

**Sandbox**：

- 以显式组合结构（Backend、Policy、SessionOpener、关闭函数作为字段）替代
  `InnerBackend()` 链式解包，删除 `maxBackendWrapperDepth` 与 `maxChannelWrapperDepth`。
- `/bin/sh` here-doc 特判迁入 darwin 平台数据表，注明出处；删除 `Options` 中的
  产品名注释。
- `Options` 拆分为隔离策略与环境投影两部分，环境投影的具体形态服从 WS2。

### 3.9 分层约束

新增 `internal/security` 架构测试，只约束导入方向，不涉及行数或函数长度：

- 禁止导入 `internal/adapter/...`、`internal/runtime/...`、`internal/persist/...`、
  `internal/host/...`；
- 初始以允许清单登记现存违规（`policy`、`authority`、`filebroker`→`adapter/tool`，
  `plandrift`→`runtime/protocol`，`workspacebroker`→`persist/workspacejournal`），
  每个 Phase 结束时收缩，Phase 5 结束时清空。Phase 1 后 `filebroker` 已移出清单。`plandrift` 与 `workspacebroker` 是否迁往
  `internal/orchestration` 或 `internal/runtime/app` 在 Phase 5 决定。

## 4. 分阶段实施

每阶段先写失败测试再改实现；除 Phase 0 的缺陷修复外，各阶段不改变对用户的授权承诺。

### Phase 0 · 止血

不依赖后续重构，可立即开始。

| 项 | 内容 | 验收 |
| --- | --- | --- |
| 0-1 | S1：代理通道 Session 凭据；`allow_loopback` 移出自动审查（Facet 未落地前以 `targetsHostLocal` 覆盖 loopback 临时实现） | 攻击测试：loopback 命令连接兄弟 Session 端口与 Workspace 通道均 407；`auto` 下 `allow_loopback` 进入人工审批 |
| 0-2 | S2：Constitution 加载时拒绝含 glob 元字符的规则，报出具体条目 | 含 `*.pem` 的 constitution 加载失败；`secrets/` 行为不变 |
| 0-3 | S3：`RequireWrite` 同时匹配 `AccessWrite` 与 `AccessTree` | `AccessTree` 写入被 constitution hold 拦截 |
| 0-4 | S4：`nonPublicIP` 补齐特殊用途段与内嵌 IPv4 形式 | NAT64、6to4、`0.0.0.1`、IPv4 映射私网地址全部非公网 |
| 0-5 | S5：删除 `Enforce` 与 nil 放行；DNS 钉住不再依赖 base 类型；并做 A5 | 未配置 Gate 的客户端请求失败；自定义 RoundTripper 须实现 `PinnedTransport` 接收获批地址，否则在授权前拒绝 |

同步修改 [安全文档](./security.md) 中 loopback 的描述，使其与实际授予范围一致。

**实施记录（2026-09-29，Phase 0 已完成）**

- 0-1：Workspace 通道与每个 Session 通道各自生成 32 字节随机凭据，经
  `sandbox.ManagedProxyURL` 写进命令的代理 URL userinfo；`Policy.ManagedProxyCredential`
  标记 `json:"-"`，不进入 Policy ID、Receipt 或 Journal。认证失败的返回：CONNECT
  与 absolute-form 为 407；通用化 P3 后 origin-form 也先校验代理凭据，认证后返回 400。
  Seatbelt 攻击测试
  `TestRealSessionProxyIsolatesSiblingPorts` 覆盖 loopback 命令访问兄弟 Session
  端口与 Workspace 通道，结果均为 407。
- 0-1 补齐：浏览器通道与其他代理一样强制认证；Chrome 通过私有 CDP pipe 响应
  `Fetch.authRequired`，只向自有代理端口与指定 realm 提供凭据。删除未认证代理
  构造和 TCP 调试端口。真实 Chrome 导航与外部无凭据 407 回归已覆盖。
- 通道关闭补齐：Hijack 后、确认 CONNECT 前登记两端；关闭与登记共享锁，已关闭
  通道立即回收迟到连接。关闭后的 Workspace 代理拒绝 OpenSession。
- S2/S3 补齐：目录写按授权子树与保护模式相交判定，含未来后代；路径中的字面
  花括号正确转义，最终 ResourcePath 编译校验，错误的 restrictive 模式不再漏匹配。
- 0-5：base 为 `*http.Transport` 时克隆并钉住拨号；实现 `PinnedTransport` 的自定义
  RoundTripper 收到获批地址；其他类型在授权前以 `egress transport cannot pin
  resolved addresses` 拒绝。
- 0-5 顺带修复：原钉住克隆设置了 `DisableKeepAlives`，带请求体的请求在 context
  取消后连接不会关闭，调用方会一直阻塞。现改为响应体关闭时回收该克隆的空闲连接。
- 0-4 测试把"公网"样例从文档保留段（`192.0.2/24`、`198.51.100/24`、
  `203.0.113/24`）改为真实公网地址，因为这些保留段现在按 IANA 归为非公网。

### Phase 1 · 公共词汇与数据表

- 新建 `resource`、`effect`、`netpolicy`、`pathpolicy` 四个叶子包；
- 第 2.2 节各重复定义改为引用，删除副本；
- `adapter/tool/contract.go` 的 Effect 类型改为引用 `security/effect`；
- 落地第 3.9 节架构测试与允许清单。

验收：`rg` 在 `internal/security` 与 Guard 中不再出现受保护目录名字面量
（`pathpolicy` 除外）；Phase 0 攻击测试保持通过；`make security-test` 通过。

**实施记录（2026-09-29，Phase 1 已完成）**

- 四个包位于 `internal/security/{resource,effect,netpolicy,pathpolicy}`，只依赖标准库，
  由 `TestSecurityVocabularyPackagesAreLeaves` 约束。第 2.2 节七类重复定义均已改为引用。
- `pathpolicy`：控制面目录名（`.agents`、`.codex`、`.git`、`.qcode`、
  `.qcode-worktree`）、宿主凭据位置表、凭据文件名表、`CanonicalAllowMissing`。
  删除 `controlplane.ProtectedNames`、`sandbox.evalSymlinksAllowMissing`、
  `authority.canonicalPathAllowMissing`、Guard `canonicalMissing` 与三份凭据列表。
  行为变化：
  - `authority` 的 `DeniedWriteRoots` 补上 `.codex`；
  - Seatbelt home 拒绝列表从 4 项扩为凭据表中的全部位置；
  - 注入根除 home 本身外，还拒绝包含任一 home 凭据位置的目录（如 `~/Library`、
    `~/.config`）；
  - 凭据位置从子串匹配改为大小写不敏感的路径段匹配，`~/.config/ghostty`、
    `secrets-manager` 不再误判；
  - `XDG_CONFIG_HOME` 锚点留给 D6。
- `netpolicy`：`Target{Scheme, Host, Port}`、`ParseTarget`、`URLTarget`、`URLPort`、
  `DefaultPort`、`ParsePort`、`SplitAuthority`、`NormalizeHost`、`NormalizeMethods`；
  `Classify` 返回 `HostLocal`、`Private`、`Reserved`、`Public`，按此顺序判定，无法解析
  的地址按 `HostLocal` 处理；`NamesHostLocal` 以 `Classify` 为准。行为变化：
  - 未知 scheme 且无显式端口的 URL 解析失败，不再默认 443（如 `chrome://settings`）；
  - 裸主机中的 `@`、`\` 被拒绝；
  - Host 统一去掉 IPv6 方括号与末尾根点，Grant 键中 `example.com.` 与
    `example.com` 合并；
  - `NamesHostLocal` 覆盖 `0.0.0.0/8` 与内嵌 IPv4 形式，和 Gate 的本机判定一致。
  `policy.NetworkTarget` 由 `netpolicy.Target` 取代（`Protocol` 改名 `Scheme`）；
  无调用方的 `egress.HostOf` 删除。`egress.Target` 携带 Grant 的 Method 与私网权限，
  保留到 Phase 3。
- `effect`：`Kind`、`Risk`、`Reversibility`、`Effect` 及校验。`adapter/tool` 以类型
  别名引用；`policy.EffectKind`、`RiskLevel`、`Effect` 与 `authority.Reversibility`
  删除。`authority`（除 `profile.go`）、`filebroker`、`vcsbroker` 的生产代码因此不再
  导入 `policy`。
- `resource`：`Access`（仍是字符串类型，JSON 值不变）与 `Writes`、`Valid`，路径与网络
  Kind 常量及 `IsPathKind`，loopback 伪资源常量与 `IsLoopback`、`IsLoopbackTarget`。
  `tool.AccessMode` 改为别名；`policy.WritesResource` 与两份 `isPathKind` 删除。
  Guard 的 5 处写判定（控制面写保护、Strong Sandbox 精确写路径、Journal 写路径与写树、
  `hasConsequentialWrite`）从 `== AccessWrite` 改为 `Writes()`，与 S3 一致；目前没有
  资源模板产出 `AccessTree`，现有工具行为不变。Kind 的完整类型化留在 Phase 2。
- 架构测试 `internal/security/architecture_test.go` 只检查生产文件。`filebroker` 生产代码
  已不导入 `adapter/tool`，从第 3.9 节的初始清单中移除；清单条目不再对应实际导入时
  测试失败，所以清单会随修复同步收缩。
- 验收 `rg` 仅剩 `sandbox/workspace_fs_unix.go` 的临时文件前缀 `.qcode-write-`，
  它不是受保护目录；`goproxy` 的默认 netrc 路径改用 `pathpolicy.NetrcFile`。

### Phase 2 · Resource 类型化与 Assessment

- Guard `resolveResources` 产出 `[]resource.Resource`；
- 实现 `assess.Assess` 与 Effect 决策表，删除 `policy.NormalizeEffect`；
- 第 3.4 节工具特判迁为 Binding 声明；
- Grant 指纹改用类型化资源并纳入 Effect/Facets 摘要。

验收：`rg '"(exec_command|spawn_agent|git_push)"' internal/security` 零命中；
`rg '"loopback"|loopback://' internal` 仅剩 `resource` 包；Effect 决策表的每一行都有
对应测试用例，且与本文或 [安全文档](./security.md) 的表格逐行一致。

**实施记录（2026-09-29，Phase 2 缺口已补齐）**

- `resource.Resource` 提供类型化 Path、Network、Loopback、Process、Agent、Plan、
  Session、Named；Guard 在参数和模板解析后通过 `tool.AssessResources` 一次映射、
  评估。原始 `tool.Resource` 仅保留给适配器、调度和显示；安全消费者读取 Assessment。
- Assessment 的资源、Binding、Facets、Effect 和 Rule 均私有，输入及所有可变返回值
  深复制。`policy.Invocation` 删除适配器资源及重复 Binding 字段，直接持有快照。
  `policy.Assess` 删除；未评估输入不能决策放行、生成 Grant 或审批。
- 参数替换、追加权限、运行时新网络目标显式生成新快照；正常决策、Grant、审批、
  观测与 Authority 编译不触发再次评估。Web CanonicalResource 是 Guard 单向展示投影。
- Effect 的 16 行决策表、L0–L8 和自动审查条件由测试逐行核对[安全文档](./security.md)。
  只读角色、验证覆盖和计划豁免均为可信 Binding 声明，安全核心不判断工具名。
- 精确 Grant Key 不包含 Effect，以防既有 deny 随效果变化失效；前缀 Scope 额外
  绑定 Effect/Facets 摘要。资源身份使用长度前缀字段，网络协议参与端点身份。
- Loopback 是独立资源类，作用域 `all-local-ports`；Profile 与 Network Intent 用
  独立标志，真实目标列表中不再放入端口为零的伪端点。控制名为 `loopback_any`。

### Phase 3 · 唯一决策入口

- 实现 `policy.Decide` 与第 3.6 节分层；
- 迁入 Guard 的控制面检查、`forceEditPlanApproval`、`ApprovalPolicyOnce` 改写；
- `Decision.Layer` 写入 Attempt Receipt；
- 规则匹配器支持第 3.6 节路径语法，Phase 0-2 的加载拒绝放宽为"仅拒绝不支持的语法"。

验收：Guard 中不再对 `decision.Action` 赋值；`policy.Evaluate` 删除；
每个决策层有独立测试，覆盖 Allow、Deny、Malformed Input 与层间优先级。

**实施记录（2026-09-29，Phase 3 缺口已补齐）**

- `fresh` 和 `fresh_once` 同意后直接完成当次授权，不进入缓存重试循环；只提供
  once 并禁用参数替换。连续两个相同调用各审批、各执行一次的回归已覆盖。
- 规则路径只在 Guard 独立采样的策略快照上规范化，构造不改写共享 Runtime。
  多工作区 Guard 共享策略时不会互相覆盖 ResourcePath；后续规则与权限更新在
  下一次采样可见，既有快照保持冻结。该边界由共享策略回归和 Runtime Race 验证。
- `policy.Runtime.Decide` 是唯一入口，`Evaluate` 删除；分层实现在 `decide.go`，
  `Layers()` 顺序即 L0–L8，`Decision.Layer` 标明结论出处。分层表与自动审查条件表见
  [安全文档](./security.md#决策分层)，`TestDecisionLayersMatchSecurityDocument`
  校验文档与代码一致；`decide_test.go` 每层都有 Allow、Deny、Malformed 与层间优先级
  用例。
- `Invocation` 新增 `Workspace`（规范绝对路径，L0 对路径写强制要求）与 `Stage`
  （`egress_target` 阶段跳过 L7）。`Decision` 新增 `Approval`（`""` 可复用缓存、
  `fresh`、`fresh_once`）与 `Resource`（控制面拒绝时的受保护路径）。
- Guard 收敛：`checkControlPlaneWrites` 删除，控制面写保护并入 L1，使用无 I/O 的
  `controlplane.Within(workspace)`；Guard 只按 `Decision.Resource` 为 Git 元数据补
  `use_git_tool` 恢复提示。`forceEditPlanApproval` 从 Guard Options 移到
  `Runtime.ForceEditPlanApproval`（随快照采样），与 `once_required` 一起并入 L7；
  审批流程只读 `Decision.Approval`，不再改写动作。追加权限重授权、替换参数复核与
  网络目标审批都改走 `Decide`。
- Constitution 成为独立来源 `Runtime.Constitution`（`SetConstitution`，只允许
  deny/hold），不再拼接到仓库规则前面；在 L1 先于 Managed Grant 求值，命中时报告
  规则 Code（缺省 `constitution_denied`），此前报 `repository_rule_denied`。
  Authority Provenance 新增 `constitution` 摘要与 `decision_layer`。
- 路径规则语法：`policy.PathPattern` 支持段内 `*`、`?`、`[...]` 与独占一段的 `**`，
  锚定且覆盖子树，`\` 转义；加载只拒绝花括号、非法字符类与混合 `**`。
  `IsPathPattern` 从 Guard 的 `isPathRule` 迁入 policy；Guard 规范化规则时只规范化
  字面前缀并转义。Constitution 模板默认从 `.env` 改为 `**/.env`。
- Receipt：`AttemptReceipt.Policy` 记录放行该尝试的决策（action、layer、code）；
  授权阶段即被拒绝时 `ExecutionReceipt.PolicyDenial` 记录拒绝决策。协议投影
  `ToolPolicyDecision` 同步，`make protocol-schema` 重新生成。
- 与计划的差异及有意的行为变化：
  - Auto-review 原本先于 Surface 收紧判定；为保持授权承诺，L6 额外判断“若 L8 会
    自动放行，Surface 是否收紧该放行”，结论记在 `surface` 层。
  - 计划门的 ask 直接进入 L7，不经 Posture、Surface 与自动审查，与旧行为一致。
  - 追加权限重授权此前不应用 Guard 改写，once/强制编辑计划时基线摘要会与首次授权
    不一致并报 `authorization_changed`；现在同样经过 L7，摘要一致。
  - 文件写 Preflight 现在在策略拒绝之后执行，被拒绝的调用不再先报 Preflight 错误。
  - `tool.Resource.Security()` 把 file 以外的路径资源（directory、repo、workspace）
    都标为 Tree，使控制面与规则匹配按子树判断。

### Phase 4 · 一次编译与执行层收敛

- `authority.Compile` 合并 Profile、Required Controls 与 Operation；
- `sandbox.CommandControls` 改为一致性校验；
- Egress Gate 构造函数化；曾抽出的 `UpstreamAuthService` 接口已随通用化 P3 删除；
- Sandbox 显式组合替代装饰器解包。

验收：资源 Kind 的解释点只剩 `resource` 与 `assess` 两个包；
`rg 'InnerBackend' internal` 零命中；`make sandbox-attack-test` 与
`make security-side-effect-check` 通过。

**实施记录（2026-09-29，Phase 4 缺口已补齐）**

- `authority.Compile` 检查 Prepared 与 Policy 共享同一已评估输入、调用与参数。
  同一类型化资源快照产出 Profile、Required Controls 和 Operation；Network Reach
  从 Facets 取得一次，Profile 与 Required 共用。
- `Authority.Bind` 只复制 Operation、附加晚到证据、更新必要控制与摘要；不再调用
  Operation 构造器，不读取文件。`WithProfile` 按已冻结的绝对路径验证新授权根、
  重绑 Root ID 和相对路径，保留原文件身份。文件变成符号链接后两种操作仍保留
  初次快照的回归已覆盖；执行期身份漂移继续由 Broker 检测。
- `sandbox.CommandControls` 对授权命令校验编译控制、实际命令开关与后端能力，
  不重选授权；报告更宽或未提供编译控制的命令拒绝。独立能力探针无执行 Lease，
  按实际能力报告。Process Owner 继续验证 Prepared 的控制和代理端口。
- `NetworkLoopbackAny` 如实表达任意本机端口；Profile Schema=5、Operation Schema=3。
  不做预发布兼容迁移。Darwin shell 临时文件例外移至带来源的
  数据表（现位于 `internal/security/sandbox/executable_contract.go`），编译器不再识别
  具体 shell 名。
- Egress Gate 保持 `NewStaticGate`、`NewCallScopedGate`、`NewBrowserGate` 固定模式；
  Backend 使用显式组合，无 `InnerBackend` 解包。通用化 P3 删除的认证服务不恢复。
- Phase 5 的架构清理结果见下一阶段实施记录。

**本轮验收（2026-09-29）**

| 检查 | 结果与覆盖 |
| --- | --- |
| 聚焦 Race 回归 | Assessment 深复制与身份、未评估输入拒绝、Constitution 目录后代和字面花括号路径、共享策略快照隔离、Fresh 审批、审批期间一次解析、冻结文件身份、代理关闭竞态、Agent 工具包均通过 |
| 真实 Chrome 回归 | 浏览器通过认证代理导航；私有 CDP pipe；外部无凭据访问返回 407；源站、其他代理和重复挑战不能取得凭据 |
| 全仓编译与静态检查 | 普通及 `-tags=capability` 的 `go test ./internal/... -run '^$'`、`go vet ./internal/...` 全部通过 |
| `make docs-check` | 27 个 Markdown 文档与 10 个脚本测试通过，已修复删除文档后的失效链接 |
| `make protocol-schema`、`make web-protocol-check` | 已按仓库命令生成协议，漂移检查通过 |
| Web 类型检查与测试 | `npm --prefix web run check` 通过；27 个测试文件、370 个测试通过 |
| `GOFLAGS='-timeout=30m -json' make security-test` | 通过，退出码 0；副作用入口检查、全部安全与 Runtime Race 包、Process 定向回归通过。首轮发现并修复共享规则竞争；Engine 完整 Race 用时约 884 秒，使用命令行延长超时，未修改仓库默认值 |
| `make sandbox-attack-test` | 通过；真实文件与 Shell 攻击、直接出网拦截、Session/Workspace 代理隔离、进程组取消清理均通过 |

新增的拒绝与生命周期回归保留在对应包中；浏览器、代理和沙箱验证使用本机服务与
当前 Darwin 后端。Runtime app 的队列用例在并行复跑时一次触及 2 秒事件等待超时；
随后单例连续 25 次与完整包 Race 复跑均通过，未放宽断言或修改队列代码。
当时剩余的四项跨层依赖未计入 Phase 0–4 完成范围，后续由 Phase 5 关闭。

### Phase 5 · 清理与文档

- 删除 `legacy:` 前缀、产品名注释与 Phase 1-4 遗留的适配函数；
- 架构测试允许清单清空；
- 更新 [架构](./architecture.md)、[安全](./security.md)、[阅读指南](./reading-guide.md)
  中的包职责与决策层描述。

**实施记录（2026-09-29，已完成并验收）**

- `policy` 直接引用安全层 Capability、Access 和 ApprovalMode；Invocation 来源为
  `security/invocation.SourceKind`，来源字符串只在工具目录侧解析。未知来源在 L0 拒绝。
- `security/invocation` 拥有 Subject 和 Prepared 输入；工具适配层校验 Catalog Ref，
  复制参数并保留已评估快照后交给 Authority。Compile 不再接收完整工具描述、执行器
  或 `[]tool.Resource`，追加权限直接扩展类型化资源并显式评估。
- 删除仅供旧测试使用的公开 `BuildExecutionOperation`；普通调用统一由 Compile
  构造，包内规范化测试直接验证私有构造函数。Broker 的受管构造合同保持不变。
- 默认工具目录注册使用 `builtin:`，删除旧 `legacy:` 生成入口并迁移测试夹具；
  未加入预发布兼容逻辑。平台路径表保留必要的来源说明，编译器没有产品名特判。
- `plandrift` 保留在安全层，返回 `DriftError{Path}`；Artifact 调用方映射为原有的
  可重试协议冲突。`workspacebroker` 迁到 `internal/orchestration`，组合 Journal、
  File/VCS Broker；Lease 校验、执行和结算仍归底层安全 Broker。
- 架构允许清单及其豁免分支全部删除。生产安全代码对 Adapter、Runtime、Persistence、
  Host 的直接导入为零，导入方向测试通过；架构、安全手册和阅读指南已同步。
- 验收中补齐工具资源夹具的 URL、loopback Methods/AllowPrivate 期望，保留 Phase 4
  的授权语义；新增身份投影、参数副本、目录身份变化和未知来源拒绝回归。
- 测试夹具保留在各使用包的 `_test.go` 中；单次使用的调用构造直接调用现有 API，
  已有 Assessment 快照直接复用，不再维护测试专用的资源反向投影或独立生产包。

验证记录：

- 无豁免的架构导入检查与核心包 Race 回归通过；目录身份绑定、未知来源拒绝、
  Plan 漂移到协议冲突的集成回归通过。删除公开 Operation 构造入口后，Authority、
  File Broker 和 Guard 再次通过完整 Race 回归。
- `go test -tags=capability ./internal/... -run '^$'`、`go vet ./internal/...`、
  `make docs-check`、`make web-protocol-check` 与 Web 类型检查通过；协议按
  `make protocol-schema` 生成，Web 协议形状不变。
- `GOFLAGS='-timeout=30m -json' make security-test` 通过，退出码 0；安全核心、Guard、
  MCP、Runtime app、Host、Engine、wiring 与 Process 定向 Race 回归通过。
  Engine 完整 Race 用时约 910 秒，wiring 约 481 秒；超时仅由命令行延长，仓库默认值不变。
- `make sandbox-attack-test` 通过，退出码 0；真实文件与 Shell 攻击回归分别用时约
  271 秒和 305 秒，直接出网拦截、Session 代理隔离与进程组取消清理通过。
  Process 攻击集合本轮单独运行通过后，完整套件复用了同一测试结果的缓存。

本轮使用当前 Darwin 后端。上述命令保留仓库默认跳过条件，未开启外部模型在线验证、
发布长稳测试或其他方案的专用门禁；这些默认跳过项不计入 Phase 5 已通过项。

### 包结构收敛补充（2026-09-29）

Phase 0–5 的实施记录保留当时包名；当前源码和阅读路径以此表及
[架构设计](./architecture.md)为准。安全子包从 22 个收敛为 13 个，按职责合并：

| 原包 | 当前归属 | 职责边界 |
| --- | --- | --- |
| `effect`、`resource`、`assess`、`invocation`、`controlmatrix` | `model` | 资源、效果、不可变评估、调用身份、控制需求；项目内只依赖 `netpolicy` |
| `constitution`、`permissions` | `policy` | 来源加载与持久规则归入决策域，`Decide` 继续只消费已加载快照 |
| `controlplane` | `pathpolicy` | 控制面目录表与写入检查共同维护 |
| `keyring` | `credential` | 系统 Keyring 与凭据引用、轮换恢复共同维护 |
| `plandrift` | `sandbox` | 使用受控 Workspace 校验 Plan 基线，向调用方返回领域错误 |

Authority、各 Broker、Sandbox 和 Egress 按授权与执行职责保留边界。旧包路径已删除，
调用方直接使用新归属，无转发包；测试辅助代码继续只放在包内 `_test.go`。
序列化字段、Effect/Control 常量值、Grant 与 Operation 摘要材料保持原合同。

验收：全 internal capability 编译、`go vet ./internal/...`、相关包完整 Race、
`make docs-check`、协议生成与漂移检查通过。完整
`GOFLAGS='-timeout=30m -json' make security-test` 和 `make sandbox-attack-test`
均以退出码 0 完成；后者的真实文件、Shell 与 Process 攻击集合分别用时约
264 秒、299 秒和 23 秒。验证使用当前 Darwin 后端，外部模型在线验证与发布长稳
测试保持默认跳过条件。

## 5. 数据与兼容影响

按 AGENTS.md，不做预发布兼容迁移；以下影响一律 fail closed：

- **持久 Grant 失效**。Grant Key 由规范化资源计算，Phase 2 后旧
  `permissions.toml` 的 `grant_key` 不再命中，用户需重新批准一次。运行时只持久化
  `grant_key` 的 allow 规则；手写的 `grant_key` deny 规则同样不再命中，且这一项不是
  fail closed，需要按 `tool`、`resource` 或 `command_prefix` 重写。`command_prefix`
  与 `resource` 规则不受影响。
- **Receipt 版本提升**。`authority.SchemaVersion` 与 `OperationSchemaVersion` 在
  Phase 4 各加一；旧 Receipt 只读展示，不参与校验。
- **待恢复审批**。跨重启恢复的审批请求若资源形态无法映射，按取消处理。
- **Web 协议不变**。`protocol.CanonicalResource` 的映射在 Guard 中维护。

## 6. 待决策事项

| 编号 | 问题 | 选项 | 建议 |
| --- | --- | --- | --- |
| D1 | loopback 授权形态 | A：如实建模为"本机任意端口"（控制名改为 `loopback_any`），永不自动审查，依赖代理凭据隔离通道；B：新增精确端口声明，仅精确端口可自动审查 | 已决定 A（2026-09-29），Phase 0 按 A 交付；B 需要解决测试服务使用临时端口的问题，另行评估 |
| D2 | Operation 中文件身份 | A：保持全文 SHA-256；B：改为 `(dev, inode, size, mtime_ns, ctime_ns)` 元数据身份，内容摘要只由 File Plan 承担 | B；全文哈希在每次构建 Operation 时读取整个文件，成本与文件大小成正比 |
| D3 | `validateWorkspaceLinks` | A：保持每次 Prepare 全树遍历；B：按 Workspace Generation 缓存结果，仅在 Generation 变化时重扫 | B；需确认 Generation 覆盖所有外部修改来源 |
| D4 | `goproxy` 后续处理 | 原迁出方案由通用化 P3 覆盖 | 已删除服务及唯一消费接口，不再迁移 |

## 7. 测试清单

- **Effect 决策表**：表驱动，每行一个用例，外加"未命中任何行"的拒绝用例。
- **决策分层**：每层的 Allow/Deny/Malformed，及相邻层冲突时的优先级。
- **攻击测试**（沿用 `process/process_capability_test.go` 模式）：
  loopback 访问兄弟 Session 与 Workspace 通道；NAT64/6to4/`0.0.0.0/8`/IPv4 映射地址；
  DNS 重绑定到内嵌 IPv4 形式；Constitution glob 与 `AccessTree` 写入；
  argv 前缀扩展引入网络或写入后不再命中 Grant。
- **分层架构测试**：`internal/security` 的导入方向。

## 8. 验收度量

| 指标 | 方案起草时 | Phase 1 后 | Phase 3 后 | Phase 5 |
| --- | --- | --- | --- | --- |
| `internal/security` 中具体工具名字面量 | 3 处 | 3 处 | 0 | 0 |
| `internal/security` 对 `adapter`/`runtime`/`persist` 的导入 | 5 个包 | 4 个包 | 4 个包 | 0 |
| 受保护目录名定义处 | 4 | 1 | 1 | 1 |
| 网络目标解析实现 | ≥5 | 1 | 1 | 1 |
| 地址分类实现 | 3 | 1 | 1 | 1 |
| 适配器资源到安全类型的映射 | 多处解释字符串 Kind | 多处 | 多处 | 单一 `tool.Resource.Security()` 边界 |
| Guard 中绕过或改写策略决策的位置 | 3（2 处改写 `decision.Action`，1 处策略前硬拒绝） | 3 | 0 | 0 |

原“资源 Kind 分支只剩两个文件”指标不适用于类型化模型：Authority 仍须把资源投影到
执行命名空间，Broker 仍须按类型检查执行范围。此处按边界验收：安全核心不接收适配器
资源，不重算 Effect；类型化资源的消费分支按编译与执行职责保留。

## 9. 与既有文档的关系

- [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)：环境契约总纲。
- [执行环境通用化方案](./environment-language-neutral-plan.md)：声明式绑定、凭据服务删除和平台验收。
- [架构](./architecture.md)、[安全](./security.md)：产品语义与当前执行边界。
