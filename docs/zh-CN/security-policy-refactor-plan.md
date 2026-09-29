# 安全策略模型收敛方案

状态：审计结论与分阶段重构合同。日期：2026-09-29。
范围：`internal/security/{policy,authority,controlmatrix,controlplane,constitution,permissions,egress,sandbox}`
与其唯一调用方 `internal/adapter/tool/guard`（生产代码约 3,750 行）。
依据：对上述包的逐文件代码审计；所有"重复定义""调用点"论断均经全仓 `rg` 验证。

本文只处理**决策模型**：一次工具调用"是什么、风险多大、允许做什么、由谁执行"
如何被表示和计算。Sandbox 执行环境、网络通道生命周期、凭据代理与进程生命周期的
实现缺口已由以下合同覆盖，本文引用而不重复：

- [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)：环境契约总纲；
- [Sandbox 审计修订与重构方案](./sandbox-refactor-plan.md)：WS1-WS8；
- [Sandbox 深度审计与优化方案](./sandbox-deep-audit-plan.md)：A-H 组缺陷。

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
   `"proxy_targets"`/`"direct"`/`"loopback_exact"`，`profile.go` 在无法执行时把后者直接
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
`NetworkLoopbackExact` 及 [安全文档](./security.md) 的"精确 Localhost Grant"不符。
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
L7 Binding 审批策略（once）与强制编辑审批 → ask
L8 自动审查资格                           → allow（显式谓词表）
```

- 控制面写入检查（现 `Guard.checkControlPlaneWrites`）并入 L1，使用 `pathpolicy`。
- `forceEditPlanApproval` 成为 Snapshot 字段，并入 L7。
- L8 的资格是一张谓词表：Effect 种类、风险、Posture、`HostLocalTarget` 与
  `LoopbackReach` 均为假。S1 由此从结构上关闭。
- 规则资源匹配改用编译后的匹配器：路径规则支持单段 `*`、`?`、`[...]` 与整段 `**`，
  其他语法在加载时报错（修复 S2）；`RequireWrite` 改用 `Access.Writes()`（修复 S3）。

**审批 Grant 的合同**：沿用 [审计修订方案 WS7](./sandbox-refactor-plan.md) 的静态
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
  服务端校验 `Proxy-Authorization`（origin-form 认证服务同样校验）。本机端口可达
  不再等于授权。
- `boundOrigin` 与 GOPROXY 文案抽象为 `UpstreamAuthService` 接口
  （`BoundOrigins()` + `ServeHTTP`）。`goproxy` 迁出 `internal/security` 仍按
  WS4.4 的单独安全评审排期。

**Sandbox**：

- 以显式组合结构（Backend、Policy、SessionOpener、ProtocolBinder 作为字段）替代
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
  与 absolute-form 为 407，origin-form（GOPROXY）为 401。Seatbelt 攻击测试
  `TestRealSessionProxyIsolatesSiblingPorts` 覆盖 loopback 命令访问兄弟 Session
  端口与 Workspace 通道，结果均为 407。
- 0-1 残留：Chromium `--proxy-server` 无法携带凭据，浏览器通道改用显式的
  `StartUnauthenticatedNetworkProxy`；浏览器运行期间，获批 loopback 的命令仍可借用
  浏览器 Gate。后续阶段需改为浏览器侧应答 CDP `Fetch.authRequired`（需要异步 CDP
  事件通道），或改用 Unix Socket 代理。
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

### Phase 3 · 唯一决策入口

- 实现 `policy.Decide` 与第 3.6 节分层；
- 迁入 Guard 的控制面检查、`forceEditPlanApproval`、`ApprovalPolicyOnce` 改写；
- `Decision.Layer` 写入 Attempt Receipt；
- 规则匹配器支持第 3.6 节路径语法，Phase 0-2 的加载拒绝放宽为"仅拒绝不支持的语法"。

验收：Guard 中不再对 `decision.Action` 赋值；`policy.Evaluate` 删除；
每个决策层有独立测试，覆盖 Allow、Deny、Malformed Input 与层间优先级。

### Phase 4 · 一次编译与执行层收敛

- `authority.Compile` 合并 Profile、Required Controls 与 Operation；
- `sandbox.CommandControls` 改为一致性校验；
- Egress Gate 构造函数化、`UpstreamAuthService` 接口；
- Sandbox 显式组合替代装饰器解包。

验收：资源 Kind 的解释点只剩 `resource` 与 `assess` 两个包；
`rg 'InnerBackend' internal` 零命中；`make sandbox-attack-test` 与
`make security-side-effect-check` 通过。

### Phase 5 · 清理与文档

- 删除 `legacy:` 前缀、产品名注释与 Phase 1-4 遗留的适配函数；
- 架构测试允许清单清空；
- 更新 [架构](./architecture.md)、[安全](./security.md)、[阅读指南](./reading-guide.md)
  中的包职责与决策层描述。

## 5. 数据与兼容影响

按 AGENTS.md，不做预发布兼容迁移；以下影响一律 fail closed：

- **持久 Grant 失效**。Grant Key 由规范化资源计算，Phase 2 后旧
  `permissions.toml` 的 `grant_key` 不再命中，用户需重新批准一次。`command_prefix`
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
| D4 | `goproxy` 迁出时机 | 随 Phase 4 迁出，或保持 WS4.4 的单独评审排期 | 保持 WS4.4；Phase 4 只落接口 |

## 7. 测试清单

- **Effect 决策表**：表驱动，每行一个用例，外加"未命中任何行"的拒绝用例。
- **决策分层**：每层的 Allow/Deny/Malformed，及相邻层冲突时的优先级。
- **攻击测试**（沿用 `managed_egress_attack_test.go` 模式）：
  loopback 访问兄弟 Session 与 Workspace 通道；NAT64/6to4/`0.0.0.0/8`/IPv4 映射地址；
  DNS 重绑定到内嵌 IPv4 形式；Constitution glob 与 `AccessTree` 写入；
  argv 前缀扩展引入网络或写入后不再命中 Grant。
- **分层架构测试**：`internal/security` 的导入方向。

## 8. 验收度量

| 指标 | 方案起草时 | Phase 1 后 | 目标 |
| --- | --- | --- | --- |
| `internal/security` 中具体工具名字面量 | 3 处 | 3 处 | 0 |
| `internal/security` 对 `adapter`/`runtime`/`persist` 的导入 | 5 个包 | 4 个包 | 0 |
| 受保护目录名定义处 | 4 | 1 | 1 |
| 网络目标解析实现 | ≥5 | 1 | 1 |
| 地址分类实现 | 3 | 1 | 1 |
| 资源 Kind 的解释点（生产代码文件） | ≥10 | ≥10 | 2（`resource`、`assess`） |
| Guard 中绕过或改写策略决策的位置 | 3（2 处改写 `decision.Action`，1 处策略前硬拒绝） | 3 | 0 |

## 9. 与既有文档的关系

- [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)：冻结决策不变；
  本文的 `resource.Kind` 需覆盖其第 5.2 节的 Resource Namespace 与 Access。
- [Sandbox 审计修订与重构方案](./sandbox-refactor-plan.md)：WS7 的前缀 grant 保留，
  本文第 3.6 节只收紧其 scope；WS4.4 的 goproxy 迁移排期不变。
- [Sandbox 深度审计与优化方案](./sandbox-deep-audit-plan.md)：A5 并入本文 Phase 0-5；
  D6 的名单扩充在 `pathpolicy` 中完成；其余各组独立推进。
- [架构](./architecture.md)、[安全](./security.md)：产品语义以它们为准；Phase 0-1
  已修正 loopback 描述以匹配实际授予范围，并补充代理通道凭据与浏览器通道残留风险。
