# 项目介绍、价值与定位

## 项目定义

QCode 是本地 Coding Agent Runtime，而不仅是一个聊天界面。用户表达工程目标后，
Runtime 会收集仓库证据、调用模型、执行受治理工具、在需要时请求审批、验证结果，并
记录可审计事件流。

“终端优先”意味着它优先保证可移植和可自动化，并不意味着“只有终端”。Web 与
Web Transport 都是同一 Runtime 之上的 Host。

## 要解决的问题

当 AI 编程工具存在以下问题时，很难获得长期信任：

- 不说明使用了哪些上下文；
- 文件修改和命令执行不经过统一策略边界；
- 每个编辑器插件都复制一套业务逻辑；
- 进程重启后状态丢失；
- 未运行测试或诊断就声称任务成功；
- 成本、审批与副作用不可追踪。

QCode 通过协议中心的 Runtime，以及显式安全、持久化和证据层解决这些问题。

## 核心价值

### 对个人开发者

- 用一个本地工具完成分析、实现、Review 和仓库操作；
- 高风险操作前有交互审批和可见变更；
- 会话可恢复，工作区上下文可持续；
- Provider 可替换，仓库逻辑不绑定单一模型供应商。

### 对团队与平台工程

- 编辑器与 Agent 客户端可嵌入的 Web Transport Runtime；
- 用于观测、评估和策略治理的结构化事件；
- Hermetic Fixture 与契约测试支持可重复集成；
- 通过 MCP 和 Skill 扩展。

### 对 AI Agent

- 明确的仓库边界和包职责；
- 机器可读的协议与配置；
- 确定性的构建命令和聚焦测试命令；
- 关于历史读取、编辑、失败与验证的持久证据。

## 能力模型

| 领域 | 当前能力 |
| --- | --- |
| 仓库上下文 | 文件搜索、符号索引、仓库地图、Working Set、Evidence Ledger |
| Agent 循环 | 流式模型调用、工具调用、步数/预算限制、上下文压缩 |
| 编辑 | 受控文件操作、Edit Plan、Journal、Revert |
| 验证 | diagnostics/repository/affected 范围，soft/hard Gate，修复预算 |
| 安全 | Posture、审批、工作区权限、Constitution、OS Sandbox |
| 持久化 | SQLite Projection、Domain Fact、Terminal Envelope、Event Log、CAS、Snapshot、Journal |
| 可观测性 | 冻结终态 Measurement、Trace、Usage、Receipt、Diagnostics、结构化日志与指标 |
| 多 Agent 协作 | Agent Graph、Subagent Admission/Budget、Worktree 隔离与 Chat Merge |
| 生态 | Model Catalog、MCP、Skill、Memory |
| Host | 本机 Web |

## 产品边界

QCode 不是：

- 托管用户源码的 SaaS；
- 无限制 Shell 包装器；
- 对模型输出正确性的保证；
- 仓库 CI 或人工 Code Review 的替代品；
- 对所有平台沙箱能力完全一致的承诺；
- 对公开发布前开发数据格式的长期兼容承诺。

安全控制用于降低风险，不能让任意代码执行变得绝对安全。用户仍需负责凭证管理、
仓库备份、审批和关键变更复核。

## 差异化定位

核心差异不是“工具数量”，而是执行路径的一致性：

```text
用户或 Host
  -> Operation
  -> Runtime / Agent Engine
  -> 受治理 Adapter
  -> Policy + Approval + Journal + Sandbox
  -> 副作用
  -> Event + Receipt + Verify Evidence
  -> Trace / Usage / Receipt + 本地 Telemetry
```

主 Agent 与 Subagent 都观察相同语义。浏览器不能拥有私有文件写路径，Child Runtime
也不能绕过交互 Turn 使用的 Guard。

## 当前成熟度

当前仓库是首个完成历史收敛的实现基线。首次公开稳定发布前，优先事项是正确性、
文档、macOS 验证、可重复发布和持续降低偶然复杂度。后续目标见
[后续规划](./roadmap.md)。
