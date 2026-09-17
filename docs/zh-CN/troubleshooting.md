# 排障指南

## 首轮检查

先确认 Binary 与启动参数：

```bash
./bin/qcode --version
./bin/qcode --help
```

随后启动 Web，并保留终端中的 Boot、Runtime 和 Shutdown 日志：

```bash
./bin/qcode --config ./qcode.toml --workspace . --no-open
```

Web Settings 中的 Runtime Diagnostics 是当前能力、配置和恢复状态的权威展示。

## Runtime 无法就绪

页面在 Runtime 构造失败时仍会保留 Boot Failure Surface。优先检查：

1. TOML 是否可解析，字段是否属于当前 Schema；
2. Workspace 与 Data Directory 是否可访问；
3. Provider、Model 和 Credential Reference 是否匹配；
4. 是否已有同一 Workspace 的 QCode 进程持有 Owner Lease；
5. 平台是否具备当前 Posture 所需的 Sandbox 能力。

修正配置后重启进程。不要通过禁用 Guard、Constitution 或 Sandbox 绕过错误。

## Provider 或凭证失败

Web Settings 会显示 Provider、Model、Credential 状态与校验结果。确认：

- Credential 只以 `env`、`file` 或 `keyring` 引用存在；
- 启动进程能够读取对应引用；
- Provider 与 Model 都存在于 Runtime 发布的 Catalog；
- 自定义 Endpoint 使用受支持的协议和完整 Model Metadata；
- 网络出口没有被 Egress Policy 拒绝。

不要把原始 Secret 放进日志、Issue、Fixture 或受 Git 跟踪文件。

## 工具调用后反复出现“消息被截断”

不要根据模型 reasoning 中的 “Your message was cut off” 判断 Runtime 已截断输出。先核对
目标 Sample 的结构化 `stop_reason`：

- `tool_use` 是正常工具边界；
- `max_tokens`、`incomplete` 或 `content_filter` 才属于不完整输出分类；
- Runtime 只有在真实不完整时才会产生结构化 `[continue_after_incomplete]` Feedback；
- HTTP 429 `rate_limit` 是 Provider 请求失败与等待重试，不是输出截断；
- 同一逻辑 Sample 的 Attempt 以 `provider.attempt` 事件公开，不要从 Usage Sample
  编号跳跃反推重试次数；
- 429 等待受 `execution.rate_limit_wait`（默认 `10m`）和
  `execution.rate_limit_retry_limit`（默认不限次数）约束。同一 Session 的 Parent
  与 Child 不会并行发送 Provider Sample；冷却未解除时也不再并行 spawn。超出预算时
  Turn 进入可恢复 Blocked，消息为 `provider rate limit retry budget exhausted`，
  不会无限透明重试。Continue 会刷新等待预算，但仍先等满剩余 `Retry-After`。
- `provider request failed during response_headers` 是等待响应头超时
  （默认继承 `execution.timeout`，通常 `2m`），不是上下文溢出。429 恢复不消耗
  `execution.provider_retry_limit`；随后的 Timeout 仍按该预算重试。Provider
  持续排队时可提高 `execution.response_header_timeout` 或
  `execution.rate_limit_wait`。
- 合法工作集超过已知 TPM（`execution.tokens_per_minute` 或 Token 专用 Header）时，
  Runtime 先对可见 Tail 做一次因果组折叠再重新准入；仍超则拒绝或等待，公开原因为
  `resource_exhausted` / `provider_throughput`，不会把同一 Digest 再探一次
  Provider，也不会改写 Durable History。

若请求达到数十万 Token 且 Provider Projection 为 `complete_http_sse`，每次工具调用后的
新 Sample 和每次 429 Attempt 都可能重新发送完整 Context。

## Tool 被拒绝

检查 Approval 中展示的 Tool、Resource、Workspace、Mode、Posture 与拒绝原因。

- `never` 只允许只读行为；
- `suggest` 对高风险操作请求批准；
- `auto` 按策略执行或拒绝；
- Web 不接受 `bypass`；
- Constitution、Workspace Fence 和强 Sandbox 要求始终生效。

批准只绑定当前 Request/Turn/Item/Edit Plan Identity，旧批准不能复用到新请求。

本地 Fixture 或 Socket 测试被拒绝且原因为
`managed_proxy` / `authority_unverified` 时：保持 `allow_loopback=true`，省略
`network_targets`，通过 `exec_command` 执行测试并声明验证用途，不要把服务存活当作测试通过。该错误在审批
已生效后仍出现，表示执行权与托管代理未对齐；按工具结果里的 `required_action`
处理，而不是改端口 `0`。

Bash 卡片长时间停在「执行中」且没有 `tool.result` 时：旧 Runtime 会把非 TTY
`exec_command` 挂到进程自己退出。重启带修复的 Binary 后，第一次 Sample 最多等到
`yield_time_ms` 就返回；仍在运行则跟 `write_stdin`，或设 `timeout_ms`。当前 Turn
仍卡住时先 Cancel，避免残留已死锁的测试进程。

## Session 无法恢复或删除

Browser State 不是事实来源。刷新后 Web 会重新获取 Session Snapshot，并从
`through_sequence` 之后继续 Event Replay。

删除 Session 时：

- 活跃内存执行者或恢复中的 Operation 会阻止删除；
- 已失去执行者的未完成 Turn 可在明确确认后连同隔离 Worktree 和该 Session 的
  Workspace Journal 草稿一并丢弃（草稿对应的文件会按 Journal 回滚）；
- 若删除后新 Session 仍报 `workspace journal has a retained draft`，说明旧草稿已成
  孤儿：在黄卡上 `Continue` 保留这些文件，`Retry` 回滚后才能重新执行；
- `source Turn is unavailable or not terminal` 出现在 Journal 准入失败、尚未写出
  `turn.started` 时；重启带修复的 Runtime 后再点黄卡 `Retry` 或 `Continue`；
- 归档 Session 需要先在侧栏显示归档项，再执行恢复或删除。

不要手工改写 SQLite、Event Log、CAS 或 Journal 来伪造终态。

## MCP 或 Extension 不健康

通过 `--mcp-config` 提供版本化 MCP 配置，在 Web Settings 中检查 Source、
Generation、Capability、Health 和 Control Receipt。单个扩展失败应保持隔离，不能
污染 Runtime Terminal Outcome。

配置修正后重启 Runtime；不要直接编辑 Lifecycle Store 或绕过 Trust/Generation Fence。

## Web 连接失败

QCode 只监听 `127.0.0.1`。确认浏览器访问终端输出的完整 URL，并检查：

- Host 与 Origin 没有被代理或重写；
- Capability Token 来自当前进程的 Bootstrap；
- WebSocket 没有被浏览器扩展拦截；
- 当前端资源更新后，`web/dist` 与 Manifest 已重新构建；
- Retention Gap 是否触发了明确的重新 Hydration。

本地验证：

```bash
make web-check
make web-test
make web-assets-check
make web-e2e
```

## 并发或持久化问题

使用聚焦 Race Test 和 Runtime 测试定位：

```bash
go test -race ./internal/runtime/app/... ./internal/runtime/agent/engine/...
go test ./internal/persist/... ./internal/orchestration/...
```

报告问题时提供脱敏后的 Session/Turn/Operation ID、错误码、时间范围和最小复现，不要
提供 Secret 或私有源码。

## 文档检查失败

```bash
make docs-check
git diff --check
```

根据错误定位失效链接、缺失文档或脚本回归；删除文档时同步更新导航与引用。
