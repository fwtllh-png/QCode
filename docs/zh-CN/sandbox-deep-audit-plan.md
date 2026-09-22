# Sandbox 深度审计与优化方案（2026-09-22）

状态：审计结论与分阶段修复合同。日期：2026-09-22。
范围：`internal/security/sandbox`、`internal/security/egress`、
`internal/security/goproxy`、`internal/platform/process`、
`internal/platform/environment`、`internal/orchestration/execsettle`、
`internal/adapter/tool/shell`、`internal/runtime/app/wire`（认证绑定）。
基线：上述包 `go test` 全部通过——即本文所有缺口都在**未覆盖的路径**上，
其中两处测试还把错误行为锁定为预期。

本文是 [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)
与[审计修订方案](./sandbox-refactor-plan.md)的后续深度审计：不改变两份
合同的冻结决策，只修订实现与合同的偏离，并给既有延后项排期。红线
沿用：不做 TLS MITM；真实凭据不进沙箱；不绕过 guard/policy/approval/
journal；`(deny default)` 姿态不变；能力不足必须报告，不得静默扩大权限。

## 1. 总览

按严重度分七组（编号在本文内唯一，引用格式 `A1`、`B2`……）：

| 组 | 主题 | 最高严重度 | 条目 |
| --- | --- | --- | --- |
| A | 网络通道与认证服务正确性 | **致命（功能）** | 9（Phase A 已修 A1–A3） |
| B | 授权范围与升级防护 | 高 | 7（Phase A 已修 B1–B6；B7 Phase C 已落地/结案） |
| C | 影子 / 隔离执行契约 | 高 | 6（Phase A 已修 C1） |
| D | 沙箱编译器边界（darwin） | 中高 | 9（D4/D8 退役；D5、D7 已修/实证不存在；其余 Phase B 已落地） |
| E | 进程与会话生命周期 | 中 | 7（Phase B 已落地；E2 合同化结案） |
| F | 契约一致性与环境注入 | 中 | 7（Phase C 已落地） |
| G | 常量治理（AGENTS.md 硬规则） | 规范 | 清单 |
| H | 既有延后项排期 | — | 5 |

一句话结论：**"稳定 workspace 通道"改造（refactor-plan 3a-E）与
"协议服务只挂 Session 通道"（P1b/P3）两条已落地决策互相矛盾，产物是
认证服务在唯一支持它的平台上死路（A1）**；其余问题集中在授权升级
（B 组）、隔离契约 fail-open（C1）、以及大量未测路径上的生命周期泄漏。

## 2. 审计方法与证据边界

- 三条并行深审（sandbox 编译器 / egress+goproxy / 进程执行+结算链），
  全量读取非测试源码并交叉核对测试意图。
- 致命与高危发现（A1、C1、B1、B3、B4、B7、F1）由主审计逐条在源码
  复核，包括两条"测试锁定错误行为"的证据。
- 中低发现附 file:line 与代码引文，实施时以"先写失败测试再修"复核，
  不得凭本文直接改代码。
- 本文不改任何冻结决策（D1–D13）；若修复需要触碰第 5.2 节之外的
  语义，先改合同文档再改代码。
- 审计基线是提交 `a87ad9ea`（refactor sandbox）；文内行号为该基线
  （平台收窄后的工作区已使部分行号漂移，复核时以函数名定位）。
  用户已确认随后的工作区变更为**移除 Linux/Windows 平台支持**：
  非 darwin 现在由 `runAttackProbe` 的 `default` 分支如实返回
  `Available=false`（"no supported platform sandbox backend"），且
  探针所有失败路径都置 `Available=false`。据此 D4、D8 与 Landlock/
  seccomp 相关常量行**退役**，D5 **已随平台收窄修复**；darwin / egress
  / process / execsettle 条目经锚点复核全部仍然成立（含 A1 的
  `protocol.go` 稳定通道改写与 C1 的 `validateSettleMode`，平台收窄
  未触碰这两处）。

## 3. A 组：网络通道与认证服务（P0）

### A1【致命】GOPROXY 稳定通道死路：认证服务在 darwin 上不可用

证据链（全部复核）：

1. `internal/security/egress/proxy.go:26` — workspace 通道创建时
   `listenProxyChannel(gate, nil)`，`protocol` 恒为 nil。
2. `proxy.go:36-43` — `BindProtocolHandler` 只写 `p.protocol`；
   唯一读取点在 `OpenSession`（`proxy.go:126-129`），只影响**之后新建**
   的 Session 通道。workspace 通道永不重绑。
3. `internal/runtime/app/wire/auth_service.go:65,134` — wire 把
   goproxy 服务 `BindProtocolHandler` 到 backend，落到上述无效路径。
4. `internal/adapter/tool/shell/protocol.go:610-611` — darwin 上
   `BackendManagedProxyPort(sandboxBackend) != 0` 恒成立
   （`NewManagedBackend` 总是设置，`policy.go:21` 限定 darwin），
   于是 `GOPROXY=http://127.0.0.1:<workspace port>`。
5. go 命令对 GOPROXY 是**直接 origin-form GET**（不走 HTTP_PROXY 语义；
   且 Go 的 `httpproxy` 对 loopback 一律绕过）。
   `session.go:150-163`：workspace 通道 `protocol == nil` →
   `serveForward` → `session.go:268-270` 对 origin-form 请求返回
   `400 absolute proxy URL is required`。

结果：绑定认证服务后，darwin 上**每一次模块获取都 400**。Linux/
Windows 不绑定认证服务，所以该服务在所有平台上都不可用——P3 验收
（空缓存 GOPROXY 闭环）被 3a-E 改造悄然打破。

两条测试锁定现状：
`egress/session_test.go:272-281` 断言"workspace 共享端口不得服务协议
处理器"；`shell/environment_receipt_test.go` 的路由测试只断言 env 值，
从不真的 fetch。两条都要改。

修复方向（保留 3a-E 的降噪意图）：

- `ManagedNetworkProxy.BindProtocolHandler` 同时重绑 workspace 通道；
  `proxyChannel.protocol` 需改为原子访问（`atomic.Pointer` 或锁），
  因为 workspace 通道已在服务中。scope 语义不变：origin-form 仍由
  goproxy 服务自身强制（前缀、固定上游、凭据注入）；workspace 通道
  的 Gate 仍为空（deny-all），外部 CONNECT 继续走逐命令 Session 闸门，
  `denyBoundOrigin` 不变。
- 或者退回"认证服务绑定时空 `network_targets` 必开 Session 端口"
  （3a-E 之前的形态），代价是恢复每命令 loopback 审批噪声。
  推荐前者。
- 验收新增**真实 fetch 集成测试**：起 workspace 代理 + 绑定假上游，
  通过 `GOPROXY=http://127.0.0.1:<workspace port>` 的 origin-form GET
  返回模块元信息；并更新上述两条测试的断言语义
  （"共享端口不得把授权写进共享 Gate"仍要测，方式改为断言 Gate 内容
  而非断言协议处理器缺席）。

### A2【高】超过 32 MiB 的上游响应被截断成短 200

`goproxy/cache.go:107-110` 读 `CacheBudgetBytes+1` 后对超限/读错误
`return buffer`（前缀）；`goproxy/service.go:246-249` 写完前缀直接
`return`——`Content-Length` 复制自上游（`service.go:241`），剩余体
永不流出。模块 zip 常超 32 MiB：客户端拿到必错的长度与短体。
修复：非可缓存结局先写前缀再 `io.Copy(writer, response.Body)` 续流；
读错误时中断连接而不是给一个干净的短 200。

### A3【中高】负缓存重放保留 Content-Length 却没有 body

`cache.go:111-118` 克隆 404/410 的全部响应头（含 `Content-Length: N`）
但 `entry.body = nil`；`service.go:304-310` 照写。非空 404 体（真实
代理的常态）重放为短体 → 客户端 `unexpected EOF`，且 5 分钟内每次
命中都错。修复：缓存小体积否定体，或剥除长度/分帧头靠连接关闭表达
结束。测试目前只覆盖无体 410。

### A4【中】可变端点缓存 30 分钟、忽略上游缓存指令与凭证轮换

`cache.go:16-20,126-128`：任意 200 与 404/410 都缓存；`@v/list`、
`@latest`（`protocol.go` KindList/KindLatest）与不可变 zip 同等待遇；
`Cache-Control`/`ETag`/`Last-Modified` 从不读取；缓存键不含凭证指纹
（`service.go:179-185`），凭证轮换后最长 30 分钟内继续提供旧授权内容。
修复：可变 Kind 不缓存或用负 TTL；尊重 `no-store/no-cache`；键中加入
凭证指纹（哈希，不存原文）。

### A5【中】`serveForward` 每请求新建 `http.Transport` 且从不关闭

`session.go:302-307`：transport 是请求局部的，`response.Body.Close()`
后池化的 keep-alive 连接留在无人关闭的 transport 里。长寿命 workspace
代理持续累积空闲 fd。修复：请求结束 `CloseIdleConnections()`，或共享
transport 并在自定义 `DialContext` 中消费审批产出的 IP 集合。

### A6【中】hijack/close 竞态 + `close()` 非幂等

`session.go:129-148`：`close()` 先 `server.Close()` 再快照关闭
`c.conns` 并清空 map；竞态窗口内 `Hijack()` 已成功但 `track()` 未执行
的 CONNECT（`session.go:242-255`）会被登记进**已清空的新 map**，从此
无人回收，隧道活到对端关闭。`close()` 二次调用返回
`http.ErrServerClosed`（`ProcessSession.Close` 与
`ManagedNetworkProxy.Close` 都会调它）。修复：`closed` 标志下
`track()` 立即关闭迟到连接；`close()` 用 `sync.Once` 幂等。

### A7【中】`ManagedNetworkProxy.Close` 忽略 ctx、串行等待、不拒绝新会话

`proxy.go:154-180`：ctx 只在收尾做非阻塞检查；每个 channel 串行等
自己的 5 秒，N 个卡死会话就是 N×5s；无 `closed` 标志，Close 后
`OpenSession` 仍能注册没人再关的会话。修复：`closed` 标志拒绝
`OpenSession`；并发关闭并受 ctx 约束。

### A8【低中】goproxy `s.facts` 无界增长

`service.go:354-358`：只增不减；生产侧只调 `Facts()`（只读），
`TakeFacts` 无人调用，workspace 生命周期内事实列表无限膨胀。修复：
环形上限或消费后清理。

### A9【低】`dialResolved` 空 IP 列表返回 `(nil, nil)`

`proxy.go:195-208`：零失败 `errors.Join` 为 nil，`dialAuthorized`
（`session.go:323-333`）无长度检查，`serveConnect` 会写出
`200 Connection Established` 后 `relay` 对 nil conn panic。默认解析器
会报错所以难以触达，但自定义 `LookupIP` 返回空即触发。修复：空列表
返回错误。

## 4. B 组：授权范围与升级防护（P0）

### B1【高】运行时审批缓存只按 origin 键控：GET 批准静默升级为 CONNECT + 私网

`egress/runtime_approval.go:67,95`：`decided`/`pending` 的键是
`key(request)` = `protocol://host:port`，不含方法、不含
`AllowPrivate`。时序：批准 `GET host:443` → `decided[origin]=nil`；
随后 `CONNECT host:443` 到来 → 缓存命中直接放行（`runtime_approval.go:69-72`）；
`gate.go:271-284` 把**当前请求**（含 CONNECT 方法）连同
`AllowPrivate=true` 写入授权。并发不同方法的等待者也搭同一班车的
结论（`runtime_approval.go:73-76`）。审批提示里展示的方法范围与
实际授予不一致——这是审批语义的静默扩大。
修复：`decided`/`pending` 键扩为 origin+methods+AllowPrivate；仅当
请求范围 ⊆ 已批范围时复用，否则重新询问。

### B2【中高】获批即 `AllowPrivate=true` 且不固定 IP：DNS 重绑定可进私网

`gate.go:271-284`（`Allow` 同理 `:125-131`）：批准结果统一
`granted.AllowPrivate = true`；后续授权重解析 DNS（`gate.go:296`）后
`nonPublicIP` 检查因 `allowPrivate=true` 失效。批准时解析为公网、
之后重绑到 `169.254.169.254`/`10.x` 的源会被直接拨号。
修复：授权记录批准时刻解析的 IP 集并钉住（与 `pinnedDialer` 对齐），
或至少把私网许可按批准决策单独记录、拨号前重校验。

### B3【中】`MatchPrefix` 无路径边界：前缀 `foo` 授权 `foobar`

`goproxy/protocol.go:146-162`：`strings.HasPrefix(module, prefix)`
无边界判断；`ValidateBinding`（`service.go:132-138`）不强制尾斜杠。
配置 `corp.io/team` 会把 `corp.io/team-secrets` 一并纳入同一凭证。
修复：归一后要求 `module == prefix || strings.HasPrefix(module, prefix+"/")`，
验证时拒绝无边界前缀（`*` 除外）；补 `foo/foobar` 边界测试。

### B4【中】重定向定界只比 hostname：同主机 https→http 或换端口时重发凭证

`goproxy/service.go:368-372`（CheckRedirect）与 `:201`（构造请求）：
只比较 `Hostname()`；`https://proxy.example` 302 到
`http://proxy.example:8080` 通过校验并重新注入 `Authorization`，明文
跳段泄露。修复：比较完整 `Host`（host+port）并要求 https（loopback
上游除外）。

### B5【中】`/sumdb/<name>/<anything>` 完全绕过模块前缀 scope

`service.go:166-178`：`KindSumdb` 不做 `MatchPrefix`，能触达该服务的
进程可经认证上游拉取任意 sumdb 路径（私有模块的存在性/哈希预言机）。
修复：sumdb 路由限制为配置的 checksum 数据库主机（默认
`sum.golang.org` + 显式声明的私有 sumdb）。

### B6【中】`denyBoundOrigin` 尾点绕过

`session.go:183`：`CONNECT proxy.example.:443`（尾点）不等于
bound host，逃过该防御；`gate.go:397` 归一化尾点后与已授权的 bound
上游同源，隧道直达认证上游——正是该防御要拦的形态。
修复：比较前对 host 做与 Gate 相同的归一化（去尾点）。

### B7【低中】网络边界杂项

- `gate.go:490-494` `nonPublicIP` 漏 CGNAT `100.64.0.0/10`、基准网
  `198.18.0.0/15`、保留 `240.0.0.0/4`。
- `gate.go:438-441` 显式 `:0` 端口被静默归一为默认端口，应判非法。
- 无认证的 loopback 监听（`session.go:76-98`）：进程间隔离完全依赖
  OS 沙箱画像；任何未沙箱化的宿主进程可拨任意会话端口消费其授权。
  短期记录为已知边界（同 UID 宿主进程本就不可信），中期给会话通道
  加 per-session 随机 token（首请求校验）。

## 5. C 组：影子 / 隔离执行契约（P0–P1）

### C1【高】`settle=discard` 静默降级为真实工作区写入

复核成立：`shell/protocol.go:360-373` `validateSettleMode` 只要求
`write_paths` 非空；`shell/isolate.go:28-35` `beginIsolatedCommand`
在 `existingWriteTrees` 为空（write_paths 全是普通文件、不存在的路径）
或 `IsolatorFrom(ctx)==nil` 时返回**零值且无错误**；命令随后在父工作区
以 exact-file 写授权原地执行；结算侧 `settleIsolated`（`session==nil`）
直接跳过——结果里连降级痕迹都没有（无 `workspace_settlement` 元数据）。
工具描述承诺"writes are summarized and dropped… never touch the
workspace"，与实现相反。这正是环境合同"不得静默改回原地写"的违反。
修复：discard 模式 fail-closed——无可隔离树或无 isolator 时返回
`workspace_isolation_unavailable` / `require_write_tree`（结构化、
`retry_original=false`）；`apply` 模式的降级保留但必须在结果元数据中
明示 `workspace_settlement=in_place_degraded` 与原因。

### C2【中】超过 yield 窗口的 verification evidence 永不结算

`protocol.go:691,739` 的 `attachVerification`/`invalidate...` 只在
`execCommand` 调用；`writeStdin`（`protocol.go:920-1042`）没有任何
evidence 处理。构建/测试类命令普遍超过默认 10s yield（上限 30s），
其 evidence 永远停在 `StatusRunning`，即便模型随后 poll 到干净零退出。
修复：evidence 随 isolate/session 一并保存，最终 `write_stdin` 结果上
完成判定。

### C3【中】默认 `check` 验证没有 `set -e`

`protocol.go:551-553` 以 `input.Verification != ""` 决定是否前缀
`set -e`，而 `applyVerificationDefaults`（`verification.go:20-23`）在
`covered_paths` 非空时补 `Verification="check"`——发生在判断之后。
描述承诺"Declared verification uses POSIX set -e"（`protocol.go:261`）。
多语句命令中间步失败仍可 exit 0 → 假 `StatusPassed`。修复：先默认化
再拼命令。

### C4【中】gitignored 文件：git 模式 isolate 不带、写被静默丢弃

isolate 内容 = HEAD + `git diff HEAD` + `ls-files --others
--exclude-standard`（`chatmerge/service.go:320-360`）；结算变更检测同样
`--exclude-standard`（`:369-406`）。命令对树内 ignored 路径的构建产物/
缓存的写入，在"结算成功"后随 `Close` 蒸发，无任何报错或元数据；
shadow 验证的 InputDigest 却按父工作区（含 ignored）计算。
修复：要么显式声明该边界（工具描述 + 结果元数据 `ignored_writes_discarded`），
要么结算时检测树内 ignored 路径的写入并报告为丢弃项。

### C5【中】被放弃的运行中会话泄漏 isolate（worktree / backend / 副本树）

`protocol.go:740-743` 把 isolate 存进 `p.isolates`，唯一出口是
`write_stdin` 的结算路径；`process.SessionManager` 的
`CloseAll/CloseByThread/CloseByTurn` 与会话超时（`session.go:292-302`）
都不触及。模型拿了 `session_id` 不再 poll，就留下活 worktree
（`.git/worktrees/<id>`）、打开的 backend 和整棵副本树，逐次累积。
修复：isolate 生命周期挂到 session manager（随
`closeSession` 结算/丢弃），或注册进 CloseBy* 清理。

### C6【低中】隔离副本保真与容量

`execsettle/service.go:297-367`：symlink 直接跳过（依赖它的构建在
副本里看到缺失）；只保 `Perm()`，xattr/flags 丢失、硬链接拆分、稀疏
文件展开；无磁盘配额（大仓库×并发隔离可填满卷，表现为费解的
git/copy 错误）；worktree 删除失败回退 `os.RemoveAll`（`:247-256`）
在父仓留下 stale `.git/worktrees` 元数据。修复：symlink 至少按链接
重建；清理失败时回收收据里报告残留；容量不足以结构化错误浮出。

## 6. D 组：沙箱编译器边界（P1，darwin）

平台收窄后本组只保留 darwin 侧条目；D4、D8 已随 Landlock/seccomp
助手删除而退役，D5 已由收窄变更修复（见 §2）。

### D1【中高】exact 写路径类型三处独立观察、不固定：file→dir 变宽竞态

校验（`sandbox/backend.go` `validateExactWorkspaceWritePaths`，基线
`:744-785`，现 `:526+`）按缺失叶子按**文件**批准；profile 生成
（`seatbeltWriteGrant`，基线 `:690-696`，现 `:494+`）执行时**再次独立**
`os.Stat`，是目录就发**子树**写授权；物化（`materializeMissingExactWritePaths`，
基线 `:806-812`，现 `:599+`）对已存在目录只查符号链接后 `continue`，
不重跑 `classifier.CheckWrite`。校验与执行之间路径被（仍有工作区写
权限的）并发进程换成目录，子级即可写 `w/generated/new.txt/.git/...`
——保护名只在校验时检查。
修复：校验时固定 file/dir 类型并贯穿传递，profile/物化两端 fail-closed
比对；`materializeMissingExactWritePaths` 对目录重跑
`classifier.CheckWrite(resolved, true)`。

### D2【中】写树内的保护子路径：OS 层允许写、结算层跳过

`validateExactWorkspaceWritePaths` 内的 `classifier.CheckWrite` 只查
树根路径组件；`(allow file-write* (subpath .../generated))` 放行树内
`generated/.git/config`；而 `write_trees.go:46-53` 枚举结算时又**跳过**
保护名。授权面比审批模型宽。修复：profile 对树内保护名显式 deny
（沿用隐藏路径 deny 机制），或收集时 fail。

### D3【中】Seatbelt `(allow signal)` 无 target + 未收窄的 `process-info*`

`backend.go`（基线 `:572`，现 `:376`）：可向内核允许的同 UID 任意进程
（包括 QCode runtime 自身与无关用户应用）发 SIGKILL/SIGSTOP；平台
收窄后 macOS 没有任何命名空间兜底。修复：`(allow signal (target self))`
（含子进程），按工具需要收窄 `process-info*`。

### D4【退役】Linux：命令级网络收窄未传入 syscall 策略；`ManagedProxyPort` 未交付洞

随 Linux Landlock/seccomp 助手删除而退役。若未来恢复非 darwin
后端，按原文重新验收：命令级收窄必须到达 syscall 策略；
`ManagedProxyPort != 0` 未交付即拒绝。

### D5【已修复】探针先置 `Available=true`，后续失败仍报可用

平台收窄变更已修复：非 darwin 走 `default` 分支返回
`Available=false`；darwin 探针的 Prepare/exec 失败路径现在都置
`candidate.Available = false` + Reason。保留一条回归断言即可。

### D6【中】凭据路径黑名单不全；注入根与 PrivateTemp 无包含检查

- `policy.go`（基线 `:642-655`，现 `:640+`）：文件名单缺 `.npmrc`、
  `.wgetrc` 等；目录名单缺 `~/.kube`、`~/.docker`、`~/.azure`、
  `~/.gcloud`、`~/.config/gh`。`$HOME` 直下文件默认放行
  （`validateInjectedHostFile`，基线 `:603-618`，现 `:601+`）。
- `policy.go:144-146` 只查 workspace↔PrivateTemp 交叉；HostReadRoot/
  WriteRoot/AdditionalReadPath/PATH 根包含私有临时区（或 `/tmp` 同级
  会话目录）时被接受——私有临时区同 UID，等于读写其他活跃会话的
  临时区。
修复：扩名单（或家目录邻接路径改允许清单）；任何注入根与
PrivateTemp 的包含/被包含一律拒绝。

### D7【中】硬链接逃逸未纳入攻击探针（macOS 未知数）

`validateWorkspaceLinks` 只在 Prepare 前跑；命令执行期间
`ln <只读根文件> ./pwn && echo x >> ./pwn` 无任何复检，攻击探针的
脚本也没有该 case。平台收窄后 macOS 是唯一后端，Seatbelt
`file-link` 是否查源路径成为唯一屏障——**用探针回答，不要假设**。
修复：探针脚本加 hardlink case；若平台放行，profile 显式 deny
只读根上的 `file-link*`。

### D8【退役】seccomp 否决表缺 mount 族与 `CLONE_NEWTIME`

随 seccomp 删除而退役。

### D9【低】编译器杂项

- `AllowNetwork+AllowLoopback` 组合静默取 loopback 分支（外部出网
  丢失）——矩阵与 profile 一致但无测试锁定语义。
- HostWriteRoots 无祖先 `file-read-metadata` 授权，运行期 EPERM
  （fail-closed 但费解）。
- 敏感 home deny 用 lexical home；home 解析错误被吞后家目录保护
  静默消失（`validateInjectedHostFile` 的正确写法应统一到
  `validateInjectedRoot`）。
- lexical 别名未过敏感路径校验（影响有限，破坏"每个授予串都过
  验证"的不变式）。

## 7. E 组：进程与会话生命周期（P1）

### E1【中】交互会话自然退出不关闭网络通道

`process/session.go:748-762` `waitLoop` 观察到退出只标 done，不关
`s.network`；只有 `closeWithReason`（manager Close/超时）会关。exec
路径会补关（`shell/protocol.go:727-731`），但退出后不再 poll 的交互
会话把 listener、Gate 授权、已批 origin 拖到 workspace 拆卸。
修复：`waitLoop` 首次观察到 `!running` 即关闭网络通道。

### E2【中】Prepare 物化的空文件在命令未跑时残留

`backend.go:795-851` Prepare 时为缺失叶子创建 `0600` 空文件；命令
未启动或立即失败，空文件留在用户工作区（出现在 `git status`）。
修复：跟踪"已创建未写入"的叶子并在执行失败路径清理，或在合同中
明示并写入结果元数据。

### E3【低中】两处清理路径泄漏

- `policy.go` `policyBinding.Close` 内层 backend Close 失败时跳过
  `closePolicyTemp`（0700 私有临时区留在盘上）——改 `errors.Join`。
- `backend.go` `NewWorkspace` 失败返回不清理已建 policy temp
  （姊妹路径有清理）。
- （原 Landlock 请求目录累积一条已随平台收窄退役。）

### E4【中】容量驱逐不 `closeSession`、不重写 journal

`process/jobs.go:136-146`：直接 `delete(m.sessions, ...)`，盘上
journal 行与内存 map 失步直到下次重写。修复：驱逐走 closeSession
语义（同锁内重写 journal）。

### E5【中】PTY master 先于读侧排空而关闭：尾部输出丢失

`process/process.go:672-677`、`process/session.go:748-753`：leader
回收（进程组 SIGKILL）后立即 `Close()` master，内核缓冲的最后一段
输出（测试摘要行、最后 printf）随 EIO 丢弃——间歇性、同时污染
`StreamReceipt` 游标。非 PTY 路径安全（`exec.Cmd.Wait` 先排水）。
修复：先等 `copyDone`（EOF/EIO）再关 master，或加有界排水宽限。

### E6【中】任意信号都把会话标成 `Terminated`

`process/session.go:534-537`：`Signal` 一律置 `terminated=true`，
包括交互式 ctrl-c（INT）与 `WINCH`；此后每次读取都按取消报告
（`protocol.go:1130-1148`），验证 evidence 被翻成 `StatusFailed`
（`verification.go:101-106`）。修复：仅 KILL/close 驱动置位，或从
wait 结果推导。

### E7【低】错误吞没杂项

- `session.go:739-744` `readLoop` 两个分支都 `return`——瞬时读错误
  无差别静默 EOF，截断不可检测（死代码条件）。
- `shell/protocol.go:729` 自然退出路径丢弃 `manager.Close` 的
  journal 重写错误。
- `protocol.go:537-542` `directoryFile` 重赋值造成首个句柄双重 Close。

## 8. F 组：契约一致性与环境注入（P1–P2）

### F1【中】env 白名单：工具描述承诺的合同不存在

`shell/protocol.go:303-307` 向模型声明"Only allow-listed names are
accepted (PATH, HOME, TMPDIR, LANG/LC_*, TERM, Go toolchain and proxy
variables)"；实现只查 NAME 合法性与 secret 名单
（`protocol.go:1062-1079`、`process/environment.go:40-49`），`LD_PRELOAD`
`BASH_ENV` `NODE_OPTIONS` 等一律放行（`environment.go:26-30` 的注释
明确这是有意设计：模型本可在命令文本里 export）。安全上等价，但
**向模型与评审者陈述的控制不存在**，且解释器预载名会改变被审命令
文本的语义。修复（二选一，推荐前者）：

1. 实现承诺的白名单 = 基础安全集 ∪ 绑定 policy 的
   `EnvironmentValues`/`Toolchains.Environment` 名；白名单外降级为
   一次性审批扩围（对齐 refactor-plan WS2.1 的原设计）。
2. 修正工具描述与文档，如实陈述；同时硬拒解释器预载名
   （`LD_PRELOAD`、`LD_LIBRARY_PATH`、`DYLD_*`、`BASH_ENV`、`ENV`、
   `NODE_OPTIONS`、`PYTHONSTARTUP`）。

### F2【中】secret 名单启发式缺口

`environment.go:81-92`：`OPENAI_KEY`/`ANTHROPIC_KEY`/`SIGNING_KEY`
等裸 `*_KEY` 不含任何 marker 通过；`ToUpper` 不做 Unicode 折叠
（全角 `ＡＰＩ＿ＫＥＹ` 通过，可利用性受限）。修复：加 `KEY` 后缀
匹配或受管后缀清单；匹配前 NFKC 归一。

### F3【中】PATH 构造：git 目录排在工具链目录之前 + 空项（cwd）

`process/process.go:204-208`：`ensureGitToolchain` 在工具链前置
**再**前置，子进程 PATH = `gitDir:toolchainBins:hostPATH`；`:537-577`
`prependPATH` 对缺失 PATH 产出尾随空项（POSIX 解释为 cwd，工作区内
相对可执行文件捡拾），不去重 `/var` vs `/private/var` 别名。
修复：统一构造顺序；空项丢弃；去重键用解析路径。

### F4【中】preflight 的 PATH 模型与子进程实际 PATH 不一致

`shell/preflight.go:42-45` 用 `Toolchains.BinDirs` + 原始
`os.Getenv("PATH")` 搜索；同名二进制可命中不同 inode——结构化拒绝
可能指向子进程根本不会执行的文件（反之亦然）。修复：preflight 用
与子进程完全相同的 `prependPATH`+`ensureGitToolchain` 管线构造搜索表。

### F5【低】`authServiceFactsSince` 用 len 差做游标

`shell/environment_receipt.go:69-74`：事实列表一旦截断/重置，新事实
静默丢失。修复：单调序号游标。

### F6【低】结果元数据杂项

- `protocol.go:1149-1153` 每次 write_stdin poll 报告的 `duration_ms`
  是自会话创建以来的全程时长，不是本次窗口。
- `protocol.go:689+1110` 会话输出先经 accumulator 截断（自带标记）再
  二次截断，嵌套标记与 `omitted_bytes` 双重计入。

### F7【低】TMPDIR/HOME 覆盖规则在两种 Posture 下不一致

`process.go:194-203`（私有临时区强制覆盖模型值）vs
`environment.go:146-158`（SharedUserTemp 下模型值优先于平台解析值）。
影响受写策略约束，但"模型能否移动 TMPDIR"应有单一答案。
修复：`HOME`/`TMPDIR`/`TMP`/`TEMP` 一律 policy 所有，全 Posture 生效。

## 9. G 组：常量治理（AGENTS.md 硬规则）

AGENTS.md：不得引入无文档固定阈值；必要绝对安全限制必须是带出处、
校验、文档与边界测试的公开合同或配置字段。以下内联字面量需治理
（"状态"列：✅=已合规，⚠=需命名/文档/测试）：

| 常量 | 位置 | 状态 |
| --- | --- | --- |
| `MaxExactWorkspaceWritePaths=512` | `sandbox/backend.go` | ✅（注意 `shell/write_globs.go:68` 错误串硬编码"512"需改为引用常量） |
| `ToolchainProbeTimeout`/`MaxOutputBytes` | `sandbox/certificates.go:19,23` | ✅ |
| wrapper 解包深度 `8` | `sandbox/backend.go`、`egress/session.go`（`BindProtocolHandler`/`LookupProcessSessionOpener`） | ⚠ 命名+边界测试 |
| system.sb 审计上限 `1<<20` | `sandbox/backend.go` | ⚠ 出处注释+边界测试 |
| 探测 exec 超时 `5s`、`nc -w 1` | `sandbox/backend.go`（`runAttackProbe`） | ⚠ 复用命名常量 |
| 临时名重试 `32` 次 | `workspace_fs_unix.go` | ⚠ |
| `maxReceipts=256` | `egress/gate.go:115` | ⚠ |
| 通道关闭 5s、读头 10s、空闲 30s | `egress/session.go:90-91,133` | ⚠ |
| goproxy Dial/TLS/Client 10/10/30s | `goproxy/service.go:29-33` | ✅（文档已注明非配置项；`UpstreamTimeout` 可配） |
| 缓存 32MiB/30m/5m | `goproxy/cache.go:16-20` | ✅（公开常量，需补边界测试与本文 A3/A4 修正） |
| `defaultSessionLimit=128` | `process/session.go:22` | ⚠ 不可配置，需公开或接线 |
| `command.WaitDelay=2s` | `process/process.go:372` | ⚠ |
| `maxArchiveReplay=256KiB`、读缓冲 32KiB | `process/session.go:51,721` | ⚠ |
| poll 等待 30s、tail 4KiB | `process/jobs.go:302,369-370` | ⚠ |
| exec yield 10s/5s/30s、tokens 4096/10000、24h | `shell/protocol.go:27-37` | ⚠（部分有测试） |
| `shell_read timeout_ms` 无上限 | `shell/shell.go:311-316` | ⚠ 与 exec 的 24h 上限不一致 |
| `×4` bytes/token、tail 2KiB | `shell/protocol.go:1110`、`shell.go:263-265` | ⚠ |
| `shadowSummaryMaxPaths=20` | `shell/isolate.go:185` | ✅（注释声明公开合同，有边界测试） |
| `gitCommandTimeout=2min` | `execsettle/service.go:451` | ⚠ |

（原 Landlock 请求上限与 `"sandbox-v2-"` 前缀两行已随平台收窄退役。）

## 10. H 组：既有延后项排期（沿用两份合同，不重复设计）

| 项 | 出处 | 建议 |
| --- | --- | --- |
| 凭据 broker 通用化（git helper / netrc / goproxy 降级 adapter） | refactor-plan WS4.2-4.4 | 保持延后；实施前单独安全评审。B4/B5 修复先收敛 goproxy 自身边界 |
| 准备器 manifest 化 | WS2.2 | 保持"待第二个生态"触发 |
| policy 所有权迁 workspace session + 自动重发现 | WS1.2 余项 | 与 D6/D9 的"host 根重验"合并为一个 Prepare 期重验工作项 |
| per-module 410 指引（候选 D） | 3a | 小项，随 A3/A4 一并做 |
| Host `unattached` 补声明 UI | 环境合同 §7.2 | 不变，Host 侧独立排期 |

（原"Linux 命名空间 Session 网络通道"一条已随平台收窄退役。）

## 11. 实施顺序与验收

每阶段先写失败测试再修；禁止"先做一个能跑的版本"触碰冻结决策。

### Phase A · 止血（A 组核心 + B 组全部 + C1）

落地状态（2026-09-22）：已实现。

- **A1**：`ManagedNetworkProxy.BindProtocolHandler` 现在同时重绑 workspace
  稳定通道；`proxyChannel.protocol` 改为 `atomic.Pointer`（通道启动后
  重绑是生产时序）。锁定旧行为的测试改写为
  `TestProtocolHandlerServesOriginFormOnWorkspaceAndSessionChannels`
  （共享端口必须服务协议处理器，且授权不落共享 Gate），并新增端到端
  复现 `TestGoproxyServiceServesStableWorkspaceChannel`（真实
  goproxy.Service + 真实稳定通道 + origin-form GET 返回模块元信息）。
- **A2/A3**：`rememberUpstreamBody` 返回四态（passthrough/cached/
  overflow/truncated）；超预算响应写前缀后续流，中途读错误以连接
  中止取代短 200；负缓存条目带体重放（受同一预算约束）。
  `TestOversizeUpstreamBodyStreamsPastCacheBudget`、
  `TestNegationReplayKeepsUpstreamBody` 锁定。
- **B1**：审批缓存键扩为 origin+方法（`askKey`）；瞬时错误（取消/
  超时）不写入 `decided`，同源下次重新询问。
  `TestApprovalDecisionIsMethodScoped`：GET 批准不复用为 CONNECT。
- **B2**：批准后不再无条件 `AllowPrivate=true`；批准路径只解析一次，
  该解析同时决定授权的私网标志并复用为本次请求的 IP 集——批准时
  解析为公网的源之后重绑到私网会被逐请求解析检查拒绝。
  `TestApprovedPublicOriginCannotRebindToPrivate`、
  `TestApprovedPrivateTargetGrantsPrivateDialing` 锁定。
- **B3**：`MatchPrefix` 归一化后仅边界匹配（`module == prefix ||
  HasPrefix(module, prefix+"/")`）；`TestMatchPrefixRequiresPathBoundary`
  锁定 `foo`/`foobar`。
- **B4**：`sameUpstreamEndpoint` 比较 host+有效端口+TLS 类别，用于
  重定向与构造请求两处；同主机 https→http 降级被拒且凭证不出现在
  明文跳段（`TestSameHostSchemeDowngradeRedirectIsRejected`），同端点
  路径重定向照常跟随并携带凭证。
- **B5**：`/sumdb/` 限定为 `sum.golang.org`（命名常量）与绑定上游自身
  数据库名；越权名 403 + `trust_validation_failed` Fact，不触达认证
  上游（`TestSumdbScopeRejectsUndeclaredDatabases`）。
- **B6**：`denyBoundOrigin` 比较前做与 Gate 相同的归一化（去尾点、
  折叠大小写）；`TestProcessSessionDeniesTrailingDotConnectToBoundHost`。
- **C1**：`settle=discard` fail-closed——无可隔离树或无 isolator 时
  返回 `workspace_isolation_unavailable` + `declare_write_tree_or_apply`
  （`retry_original=false`），工作区零触碰；apply 模式降级时结果元数据
  显式 `workspace_settlement=in_place_degraded` +
  `degradation_reason=workspace_isolator_unavailable`。三个测试锁定
  （含攻击形态：降级路径不得写入真实工作区）。
- **F1（最小）**：工具描述改为如实陈述（任意合法 NAME、secret 名拒绝、
  `declared_env` 审计、代理/沙箱控制变量 policy 所有）；白名单实现
  按 Phase C 立项。

验证：`go test -race ./internal/security/egress
./internal/security/goproxy ./internal/adapter/tool/shell
./internal/runtime/app/wire`、`make sandbox-attack-test`、
`make docs-check`、`git diff --check`、`go vet ./...`、广义回归
（process/execsettle/environment/security 全量）全部通过。
B7（会话端口 token 认证、netblock 补齐、`:0` 拒绝）不属 Phase A，
按计划留待 Phase C。

验收：

- **绑定认证服务 + workspace 稳定通道下，origin-form 模块获取集成测试
  返回真实元信息**（A1 的复现测试转绿；两条锁定测试按新语义更新）；
- 超预算与负缓存响应不再产生短体（>32MiB 流式、404 带体重放完整）；
- 攻击测试：`GET 批准 → CONNECT 请求`必须重新询问；`foo` 前缀拒绝
  `foobar`；`host.:443` 被拒；同主机 http 重定向不携带凭证；
  `sumdb/` 越权路径被拒；
- `settle=discard` 无可隔离树时结构化拒绝（`retry_original=false`），
  攻击测试证明 discard 无法借降级触碰工作区。

验证：`go test ./internal/security/egress ./internal/security/goproxy
./internal/adapter/tool/shell ./internal/runtime/app/wire`；
`make sandbox-attack-test`；`make docs-check`。

### Phase B · 契约与边界（C 组余项 + D 组 + E 组）

落地状态（2026-09-22）：已实现。

- **C2**：验证 evidence 与 isolate 一并存入 `pendingExecution`，随会话
  OnClose 注册回收；最终 write_stdin poll（含显式 close 路径）经
  `settleTakenPending` 使 evidence 到达终态。超窗验证命令现在可以
  `passed`。`TestVerificationEvidenceFinalizesOnFinalPoll`。
- **C3**：`applyVerificationDefaults` 移到 `set -e` 判定之前；
  `TestDefaultedCheckRunsUnderSetE`（`false; printf masked` +
  covered_paths → 必失败）。
- **C4**：按"显式声明边界"落地——write_paths 工具描述声明
  gitignored 路径写入随隔离副本丢弃、不结算；实现级检测留待需要时。
- **C5**：`SessionOptions.OnClose` 钩子（每会话恰一次，覆盖 Close/
  turn 释放/超时/容量驱逐/CloseAll）；shell 侧 `reclaimAbandonedExecution`
  回收被放弃 isolate（worktree + 副本），write_stdin 正常路径先结算
  后关会话避免与回收竞态。`TestAbandonedIsolatedSessionIsReclaimedOnThreadClose`。
- **C6**：`copyWorkspace` 现按原样重建符号链接（相对/绝对目标、
  悬挂链接同样保留）；worktree 清理失败时错误信息显式报告 scratch
  已删、父仓可能残留可经 `git worktree prune` 清理的元数据。
  `TestCopyWorkspaceRecreatesSymlinks`。
- **D1**：`workspaceWritePath{path, kind}` 在校验时固定类型，贯穿
  profile 生成（不再二次 Stat）与物化（类型漂移 fail-closed，树重跑
  classifier）。`TestWritePathTypeSwapFailsClosed`、
  `TestWriteTreeDriftFailsClosedAndStaysClassified`。
- **D2**：写树内 `controlplane.ProtectedNames()`（单一来源）显式
  `(deny file-write* (subpath tree/<name>))`，位于 allow 之后。
  `TestWriteTreeProfileDeniesProtectedSubpaths`。
- **D3**：**经验测试后按平台边界结案**——Seatbelt 无 self+后代
  target（`(target self)` 下 kill 自己的子进程也 EPERM，`child`/
  `descendant` 语法不存在），收窄会破坏构建工具链的基础 kill/reap
  模式。保留未收窄授权，profile 生成处注释记录证据；同 UID 信号
  属于环境合同已声明的平台边界。
- **D6**：敏感名单扩为 `sensitiveCredentialFiles`/`Segments` 变量
  （新增 `.npmrc`、`.wgetrc`、`/.kube`、`/.docker`、`/.azure`、
  `/.gcloud`、`/.config/gh`）；BuildPolicy 收尾处对全部注入根
  （声明/PATH/工具链/证书/写根，含 lexical 别名）做 PrivateTemp
  包含/被包含集中拒绝；home 解析失败保留 lexical（与
  host-file 路径统一）；lexical 别名过敏感校验。
  `TestInjectedRootsMayNotTouchPrivateTemp`、
  `TestSensitiveCredentialDenylistCoversCommonStores`。
- **D7**：**实证结论：逃逸不存在**。真实生产 profile 下 `ln`（工作区
  内部与 /etc/hosts 逃逸两种形态）全部 EPERM——profile 从未授权
  `file-link`，deny-default 已封死全部硬链接创建。两条断言写入常驻
  攻击探针，防止未来为缓存分区加 file-link 授权时静默打开
  inode-identity 逃逸。（`file-link*` 带 `*` 不是合法操作名。）
- **D9**：`AllowNetwork+AllowLoopback` 组合取窄分支并以
  `TestAllowNetworkWithLoopbackChoosesLoopbackBranch` 锁定语义；
  home 统一随 D6；HostWriteRoots 祖先 metadata 未动（fail-closed，
  影响为可诊断性，Phase C 顺手项）。
- **E1**：`waitLoop` 观察到进程退出即 `closeNetwork()`（`sync.Once`
  防与 closeWithReason 双关）。授权/端口不再拖到 workspace 拆卸。
- **E2**：按合同化结案——Prepare 物化的 0600 空文件是"已声明写目标
  的预创建"，属结算可见行为；不引入跨层清理机制。此处记录，不再
  另立实现。
- **E3**：`policyBinding.Close` errors.Join（内层失败不再遗留私有
  临时区）；`NewPlatformBackend` 在 `NewWorkspace` 失败时清理已建
  policy temp。
- **E4**：容量驱逐改走 `closeSessionLocked`（删表 + journal 重写 +
  stale 记录），不再裸 delete。
- **E5**：PTY 排水修复——session `waitLoop` 与一次性 `Run` 的
  `runPTY` 都先等读侧 EOF/EIO（有界 `ptyDrainTimeout=2s` 公开常量）
  再关 master；期间发现并修复 runPTY 排水 select 吞掉唯一 channel
  值导致永久阻塞的缺陷（改为单次接收结构）。
  `TestPTYSessionFinalLineSurvivesExit`。
- **E6**：仅 `SIGKILL`（及 close 驱动终止）置 `terminated`；
  INT/TERM/HUP/WINCH 不再把会话标成取消、不再污染验证 evidence。
  `TestSessionInterruptIsNotTerminationButKillIs`。
- **E7**：`readLoop` 非正常读错误记录为 `Session.ReadError()`
  （截断可观测）；自然退出路径的 `manager.Close` 错误以
  `session_close_error` 元数据保留；`directoryFile` 重赋值后的
  双重 Close 移除。

验证：`go test ./internal/security/sandbox ./internal/orchestration/execsettle
./internal/orchestration/chatmerge ./internal/platform/process
./internal/adapter/tool/shell ./internal/adapter/tool/guard`、
`make sandbox-attack-test`（探针含新硬链接断言）、
`-race`（process/shell/sandbox）、广义回归（egress/goproxy/wire/
environment/orchestration 全量）、`go vet ./...` 全部通过。
非 darwin `Available=false` 回归断言随平台收窄由 `runAttackProbe`
的 `default` 分支承担（探测非 darwin 已无构建）。

验收：

- 超过 yield 窗口的验证命令经 write_stdin poll 到零退出后 evidence
  可达 `passed`；默认 check 命令带 `set -e`（中间步失败必非零）；
- 被放弃的运行中会话在 CloseByThread/超时路径回收 worktree 与副本
  （泄漏计数为零）；
- file→dir 类型竞态、写树内 `.git` 写入、树内 ignored 写入、硬链接
  逃逸进入攻击探针/测试并有 fail-closed 结果；
- 非 darwin 构建（若保留）`Capability` 如实报 `Available=false`
  （平台收窄回归断言）；
- PTY 尾行不再丢失（复现脚本转绿）；INT/WINCH 不再把会话标成取消。

验证：`go test ./internal/security/sandbox ./internal/orchestration/execsettle
./internal/orchestration/chatmerge ./internal/platform/process
./internal/adapter/tool/shell ./internal/adapter/tool/guard`；
`make sandbox-attack-test`。

### Phase C · 一致性与治理（F 组 + G 组 + H 组排期）

落地状态（2026-09-22）：已实现。

- **F1/F7**：模型 env 声明新增两类硬拒——解释器预载名
  （`LD_PRELOAD`、`LD_LIBRARY_PATH`、`DYLD_*`、`BASH_ENV`、`ENV`、
  `NODE_OPTIONS`、`PYTHONSTARTUP`、`PERL5OPT`、`RUBYOPT`：改变被审
  命令文本的解释方式而非携带数据）与 policy 所有名（`HOME`/
  `TMPDIR`/`TMP`/`TEMP`：全 Posture 由沙箱与准备器决定，模型声明
  无法移动）。工具描述同步。
  `TestSanitizedEnvironmentRefusesPreloadAndPolicyOwnedNames`。
- **F2**：`SecretEnvironmentName` 先做 NFKC 归一（全角形
  `ＡＰＩ＿ＫＥＹ` 折叠后命中），并补裸 `_KEY` 后缀
  （`OPENAI_KEY`/`SIGNING_KEY`）；`KEYBOARD` 等不误伤。
  `TestSecretEnvironmentNameCoversKeySuffixAndLookalikes`。
- **F3/F4**：PATH 构造收敛为单一顺序（工具链 bin → 平台 git 目录 →
  宿主 PATH），`ToolchainSearchPath` 与子进程 PATH 构造共用该管线；
  空项丢弃（消除 cwd 拾取）、别名（`/var` vs `/private/var`）去重。
  preflight 改用同一管线——结构化拒绝命名的就是子进程将执行的
  文件。`TestToolchainSearchPathOrdersAndDedupes`、
  `TestSandboxedChildPATHMatchesPreflightSearchOrder`（端到端同序）。
- **F5**：`serviceFactsSinceCursor` 以前缀相等取代长度差——服务
  事实列表截断/重置时重报全部而不是静默丢新事实。
  `TestServiceFactsSinceCursorSurvivesRotation`。
- **F6**：`duration_ms` 报告本次结果覆盖的窗口（首次 exec 为命令
  至今、write_stdin 为该轮 poll/关闭窗口）；截断单点化——
  accumulator 路径已有界带标记，WaitNext 路径在调用点截断一次，
  `sessionResult` 不再二次截断（嵌套标记与 omitted 双计消除）。
- **B7（网络边界）**：`nonPublicIP` 补 RFC 6598 CGNAT（100.64/10）、
  RFC 2544 基准（198.18/15）、保留段（240/4）；URL 显式 `:0` 拒绝
  而非静默归一为默认端口。
  `TestNonPublicIPCoversCGNATBenchmarkAndReservedRanges`、
  `TestRequestPortRejectsExplicitZero`。
  **会话 token 结论**：GOPROXY 改写通道无法要求客户端认证——go
  工具链对 `GOPROXY=http://127.0.0.1:<port>` 的 origin-form GET 不
  带任何可校验凭据，CONNECT 客户端同样无法携带；进程间隔离继续由
  OS 沙箱画像承担（与 E1 的通道生命周期收口配合）。可行替代是
  per-session 目录权限的 UNIX domain socket 通道，属后端级变更，
  移入 H 组随"跨平台 Session 通道"一并评估。
- **shell_read timeout**：显式 `timeout_ms` 与 exec_command 共用
  24h 公共上限（负值仍按既有合同回退默认）。
  `TestForegroundTimeoutSharesExecCeiling`。
- **G 表**：内联字面量全部具名并带出处注释——
  `maxBackendWrapperDepth`、`maxSystemProfileBytes`（边界测试
  `TestSystemProfileAuditRejectsOversizedProfile`）、探针超时复用
  `ToolchainProbeTimeout`、`maxTempNameAttempts`、egress 通道
  `channelReadHeaderTimeout`/`channelIdleTimeout`/
  `channelCloseTimeout`/`maxChannelWrapperDepth`、`maxReceipts`
  （边界测试 `TestReceiptLogIsBounded`）、`DefaultSessionLimit`
  （公开导出）、`processWaitDelay`、`sessionReadBufferSize`、
  `maxArchiveReplay`（已有文档）、`jobsListWait`、
  `jobsOutputTailBytes`、`gitCommandTimeout`。exec yield/tokens
  系列本就是命名常量。
- **H 组确认**：五项排期不变（凭据 broker 通用化、准备器
  manifest 化、policy 会话化+重验、per-module 410 指引、Host 补
  声明 UI）；会话 token 按 B7 结论并入"跨平台 Session 通道"评估。

验证：sandbox/egress/goproxy/process/shell/orchestration 全量、
`-race`（egress/process/shell）、wire/environment/guard 回归、
`make sandbox-attack-test`、`go vet ./...`、`go mod tidy`
（landlock/psx 随平台收窄移除正式化，x/text 转直接依赖）、
`git diff --check` 全部通过。

验收：

- 每个治理常量具名/文档/边界测试三件套齐全；
- preflight 与子进程 PATH 解析一致（同 inode 断言）；
- `make docs-check`、`git diff --check`、`go vet ./...` 干净。

## 12. 新增攻击与回归测试清单

1. workspace 通道 origin-form 模块获取（A1，真实 fetch）。
2. 审批方法升级（GET→CONNECT）与私网升级（B1/B2）。
3. 前缀边界 `foo/foobar`（B3）、尾点 CONNECT（B6）、同主机降级重定向
   凭证（B4）、`/sumdb/` 越权（B5）。
4. discard 无树降级攻击（C1）：结果工作区字节不变。
5. 写路径 file→dir 竞态（D1）、树内保护名写（D2）、硬链接逃逸探针（D7）。
6. 会话生命周期：自然退出关网络（E1）、孤立 isolate 回收（C5）、
   hijack 后关闭回收（A6）。
7. PTY 尾行保留（E5）、INT/WINCH 状态（E6）。
8. 大响应流式与负缓存完整重放（A2/A3）。

## 13. 度量（沿用 refactor-plan 第 6 节，增补）

- 模块获取成功率（workspace 通道，目标 100%——A1 修复前为 0）；
- 审批范围升级次数（B1/B2，目标 0）；
- discard/apply 降级误报率（C1，目标 0——fail-closed 后无静默降级）；
- 孤立 isolate 与泄漏 fd/worktree 计数（C5/A5，长会话压测归零）；
- evidence 终态达成率（C2：超窗验证命令最终有终态，目标 100%）；
- 既有指标（approval_wait_ms/turn_ms <10%、EPERM 结构化 Denial 趋零、
  `illegal_transition` 归零、401 首轮归因 100%）不回退。

## 14. 与既有文档的关系

- [Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)：
  冻结决策不变；本文修订 3a-E 与 P1b/P3 的落地矛盾（A1），其余为实现
  层缺口。
- [审计修订方案](./sandbox-refactor-plan.md)：Phase 1-3 结论不变；
  3a-E 的"每命令审批噪声根因"结论仍成立，但其落地物 A1 必须修复后
  该项才算交付。
- [架构](./architecture.md)、[安全](./security.md)：产品语义以它们为准；
  F1 的工具描述修正与 B 组修复不得改变对用户的授权承诺。
