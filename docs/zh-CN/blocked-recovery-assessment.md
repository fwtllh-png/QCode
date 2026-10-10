# Turn 受阻与恢复评估

本文区分真正需要外部条件变化的受阻、运行时可以自动纠正的问题，以及仅在界面上显示
为 Blocked 的失败。评估覆盖主 Agent 的采样、工具、验证、上下文、持久化、恢复及
Web 投影入口；Subagent 使用相同 Engine，父级收到的子任务受阻不必终止父级 Turn。
这是代码路径审计，不代表这些情况都已在真实会话中发生。

## Blocked 的实际含义

Kernel 的终态是 Completed、Failed、Canceled；Blocked 主要由 Web 根据终态携带的
恢复事实推导。[conversation.ts](../../web/src/projection/conversation.ts) 将以下情况保留为
可恢复受阻状态：

- 有 Convergence 原因，例如无进展、修复预算耗尽、模型声明未完成；
- Fault 的 Disposition 是 `retry_step`、`retry_turn` 或 `resume_turn`。

因此 Blocked 表示“没有完成且存在恢复路径”，并不等于权限不足，也不表示已经用尽
自动恢复能力。对话卡片通过
[failurePresentation.ts](../../web/src/projection/failurePresentation.ts) 展示具体原因；
Session 的 Blocked 状态和 Continue/Retry 协议保持不变。[fault.go](../../internal/common/fault/fault.go) 还将未分类错误默认映射
为 `unavailable/resume_turn`，扩大了这一标签覆盖的范围。AwaitingApproval、
AwaitingInput 和 Journal 的 AwaitingRecovery 是独立等待状态；单个工具结果报错
也不等于整个 Turn 受阻。

## 工具参数纠错契约

[stream_consumer.go](../../internal/adapter/provider/assembly/stream_consumer.go) 在
发现重复成员时立即关闭流；正常结束时仍无效的调用也被拒绝。整个响应中的工具批次
均未执行，因此允许重新生成该批次。

[tool_argument_repair.go](../../internal/adapter/provider/assembly/tool_argument_repair.go)
保存追加式纠错授权；失败片段、已确认正文和 Usage 保持可审计。Engine 在新响应片段
中重新生成，不续写坏 JSON，不重放之前已完成的工具。纠错与网络重试分别计数，均
采用 `execution.provider_retry_limit`，没有额外隐藏阈值。授权先持久化再请求模型，
重启复用已授权尝试。额度耗尽才返回 `resume_turn` 并保留草稿。

事件序号倒退、同一事件身份对应不同内容等流协议错误不属于参数纠错，继续拒绝。
正常 `max_tokens` 截断保留续写语义。

`exec_command` 的 host 准备与普通执行共用参数校验；非法
timeout、输出限制、终端尺寸组合等均以 `invalid_arguments` 返回模型，在当前 Turn
内纠正，拒绝的命令不会启动。同批次已完成的调用仍保留，环境准备故障不改判为参数错误。

## 受阻入口与合理性

| 场景 | 当前路径与处理 | 评估 |
| --- | --- | --- |
| 重复字段、正常停止时调用不完整 | Assembly 拒绝；Engine 按持久化额度重新生成；耗尽后 Continue | 自动纠错后才停止合理；不能执行有歧义的参数 |
| 5xx、网络失败、Timeout、空响应 | [provider_retry.go](../../internal/runtime/agent/engine/provider_retry.go) 与 Provider RetryPolicy 先重试；耗尽后可恢复失败 | 有上限合理；恢复时应准确区分请求、响应和网络故障 |
| 429、Provider 配额耗尽 | 瞬时限流先按 Retry-After/Cooldown 等待；额度耗尽立即停止 | 合理，充值或等待额度恢复是外部条件；不能通过重试额度错误无限空转 |
| Auth、模型/请求配置错误、内容过滤、流事件契约冲突 | Provider 分类后停止，通常交给 Host 恢复 | 拒绝继续发送同样无效请求合理；具体提示应指向连接设置或请求修正，统一“重试 Turn”不够精确 |
| token/费用预算与 TPM/Burst 不足 | [budget.go](../../internal/runtime/agent/engine/budget.go)、[throughput.go](../../internal/runtime/agent/engine/throughput.go) 先计算余量、折叠或等待，仍不足则停止 | 硬预算应遵守；提示需区分运行预算、模型窗口和 Provider 吞吐，盲目 Continue 未必有用 |
| 必要上下文超过窗口 | [current_turn_compact.go](../../internal/runtime/agent/engine/current_turn_compact.go) 尝试工具结果和参数引用化、正文压缩等后停止 | 当前请求与必要事实确实装不下时合理；Continuation 已支持原文保存与实测容量投影 |
| 工具批次的写入事实预留超额 | [context_admission.go](../../internal/runtime/agent/engine/context_admission.go) 拒绝预留；[tool_effect.go](../../internal/runtime/agent/turnkernel/tool_effect.go) 持久化全部待执行调用的拒绝结果 | 拦截写入合理；模型收到拆批或先验证已有变更的反馈，继续同一 Turn |
| 只读模式、显式策略拒绝、审批拒绝、资源缺失、工具失败 | [result/recovery.go](../../internal/adapter/tool/result/recovery.go) 返回结构化工具错误及修正动作；模型可选替代方案、提示切换权限或声明未完成 | 安全边界合理，通常不会直接终止 Turn；只有必要条件无法满足时才应声明受阻 |
| 缺少历史定义或来源不可用 | 缺定义时只允许恢复/绑定；失效的未绑定候选从请求移除；已绑定必要来源不可用则拒绝采样 | 恢复门禁及来源校验合理；已区分必要来源与可选候选，原文、检查点与摘要同步过滤 |
| 无进展、修复预算耗尽、模型声明 incomplete | [reducer_common.go](../../internal/runtime/agent/turnkernel/reducer_common.go) 先给修正或最终收尾机会，[turn_run.go](../../internal/runtime/agent/engine/turn_run.go) 再结算受阻 | 防止空转合理；模型可能过早声明未完成，需检查 Pending Actions 是否真的依赖外部条件 |
| 通用验证门禁 | 已移除；不再因覆盖声明、验证器不可用或验证修复预算耗尽阻止完成 | 用户要求的测试仍执行，实际失败保留在工具结果中 |
| 草稿冲突、恢复环境不一致、撤回来源、损坏的事实链 | Journal/恢复入口验证身份、Revision、Digest 与副作用状态 | 合理，避免丢改动或重复副作用；保留草稿、重新选择来源等恢复动作必须具体 |
| Domain Fact、必需交互投影、Journal 或终态存储失败 | [coordinator.go](../../internal/runtime/agent/turnkernel/coordinator.go) 停止推进未落盘事实；Journal/Outbox 保留同一幂等工作 | 持久化安全边界合理；能自动结算的工作应维持等待与重试，不能让用户重复执行业务工具 |
| 普通阶段说明、日志、用量投影或完成后的上下文维护失败 | 记录 Secondary Issue 或交给 Outbox 重放 | 不应改变已决定的业务结果；当前主路径已有隔离，应继续保持回归 |
| 未分类的普通 error | `fault.Of` 默认转成可恢复 unavailable | 保留工作合理，但不能据此证明故障是可由用户解决的；需要补齐来源、原因和恢复责任 |

## 三处过早终止路径的修复

1. **部分输出导致窗口溢出。** History 降级后若 Continuation 仍使完整请求超窗，
   先保存完整续写消息到 ResultStore，再按实际请求成本选择可分页恢复的首尾摘录。
   用户当前请求、必要事实和原始 Assembly 不改写。已确认正文仍保留为对应的进度或
   答案；未执行工具片段只能作为资料引用，必须重新生成较小的完整调用。存储失败
   保留原表示，最小恢复投影仍不适配必要上下文时才停止。
2. **批次预留失败。** 超额批次的每个待执行调用均进入既有持久化生命周期，以
   `context_reservation_exceeded` 拒绝结果闭合。整批拒绝决定先持久化，再逐个保存结果；
   进程在中途退出后，恢复只结算剩余拒绝，不重新执行这些调用。模型收到
   `split_batch_or_resolve_obligations`，可拆批或先验证已有变更。已经完成的调用
   不重放；批次拒绝不进入单调用失败缓存，防止拆批后仍被旧错误挡住。存储或生命周期
   错误继续终止推进，不能当成普通容量反馈吞掉。
3. **可选历史候选失效。** 未绑定候选被撤回或不属于本会话时，只移除当前请求中的
   相关投影，Durable 来源索引不变。其原文、检查点及依赖原文的可选摘要同步排除；
   诊断标记 `source_unavailable`，不广告无效的 `turn_history` 恢复入口。显式选择和
   未完成计划步骤的必要来源仍严格检查，来源存储不可用也不会静默省略。

## 恢复分类和产品提示（原建议第 4、5 项，已实施）

4. **重启后的重试预算归类。** `ProviderRetries` 仅作为单次 Sample 的单调顺序号；
   `RetryBudget` 独立保存普通瞬时故障次数、429 次数、累计等待预留及等待截止时间。
   429 授权与预算预留在同一个 Domain Fact 中提交，等待中退出不会退还次数和预算；
   恢复只等待原截止时间的剩余部分，不重置完整延迟。吞吐准入的等待通过
   `provider_wait_reserved` 进入同一持久化预算，不消耗重试次数。
   普通网络重试、限流与工具参数纠错仍分别受既有配置约束，没有增加隐藏额度。
   存储失败不得开始新的等待或 Provider 请求；已完成工具继续复用已有结果。
   新增预算字段不改写旧审计记录；旧版本活动 Sample 若缺少分类事实，明确要求通过
   Continue 创建新的恢复 Turn，保留已有工作，不猜测旧次数或自动清零。
5. **展示具体受阻原因与恢复建议。** Engine 为 Provider 配额、限流、网络重试耗尽、
   参数纠错耗尽、认证、请求配置、内容限制和无效响应提供稳定的 `fault.reason`。
   验证不可用与已执行检查失败分别标记。Web 使用这些结构化事实以及 Convergence
   原因展示具体标题；没有分类的错误显示“需要恢复”或“模型请求失败”，不按报错
   正文猜测权限或配额。持久化、Kernel 和投影故障优先展示运行时恢复要求，
   不能被早先的模型失败或未完成声明覆盖。
   卡片展示 Runtime 给出的推荐动作，或未完成声明中的 Pending Actions；
   `provider.attempt` 中已有的等待安排、网络重试、未完成输出与参数重新生成事实
   汇总为已记录的恢复尝试，按 Sample、Attempt 和状态去重，缺少事件不虚构次数。
   等待安排不宣称请求已经完成；终态存储故障也不宣称正在后台自动修复。
   Continue/Retry、草稿和副作用语义保持不变，权限切换仍需用户显式点击。

## 验证入口

```bash
go test ./internal/adapter/provider/assembly -count=1
go test ./internal/runtime/agent/engine -run 'Test(DuplicateToolArgument|RejectedToolArgument|R3|MaxTokensCreates)' -count=1
go test ./internal/runtime/agent/turnkernel ./internal/runtime/agent/prompt -count=1
go test ./internal/runtime/agent/engine ./internal/runtime/agent/turnkernel ./internal/runtime/agent/contextview -run 'Test(ContinuationPressure|ContextReservation|ToolAdmission|ConversationAvailability|UnavailableSelection|NarrativeSelection)' -count=1
go test ./internal/runtime/agent/engine ./internal/runtime/agent/turnkernel -run 'Test(ProviderRetry|ProviderWait|ProviderBudget|ProviderFailure|AdmitThroughput|VerifyGate)' -count=1
npm --prefix web test -- src/projection/conversation.test.ts src/projection/failurePresentation.test.ts src/ui/App.test.tsx
make docs-check
git diff --check
```

参数纠错测试覆盖关闭/单次/多次额度、失败片段不可改写、授权持久化、完整新调用、
已完成编辑不重放、耗尽后草稿保留，以及重启后额度不清零。Fixture 测试验证恢复机制，
不能证明任何真实模型都能在给定次数内纠正自身输出。

三处恢复回归还覆盖不同模型窗口、超大正文与未执行参数、原文分页与存储失败、重启后
不重放已完成工具、整批拒绝后真实文件编辑成功、拒绝结果持久化、运行中来源撤回、
跨会话来源、必要来源和存储错误继续拒绝，以及摘要不能重新引入被排除来源。

重试分类回归覆盖多次 429 后重启再遇到 5xx、反向混合故障、次数与等待预算耗尽、
等待中重启、存储提交失败、等待取消和非法计数。Web 回归覆盖结构化原因、无分类错误、
来源优先级、Pending Actions、恢复事件去重与跨 Turn 隔离、撤回清理和实际卡片渲染。
