# QCode 文档

这里是面向当前代码树持续维护的文档集合。历史实现 RFC 不再作为产品文档保留；仍然
有效的架构决策会以“当前约束”的形式写入对应指南，而不是要求读者重放开发过程。
产品手册只维护中文版本，`docs/en` 和 `docs/book/en` 不属于允许的仓库结构。

## 按目标阅读

### 我要系统学习 Agent 工程

1. [项目与系统全景](./overview.md)
2. [架构设计](./architecture.md)
3. [安全模型](./security.md)
4. [源码阅读路线指南](./reading-guide.md)
5. [本地开发与脚本](./development.md)

### 我要使用 QCode

1. [项目介绍与定位](./overview.md)
2. [快速开始](./getting-started.md)
3. [配置说明](./configuration.md)
4. [Web 使用与工作流](./usage.md)
5. [桌面应用](./desktop.md)
6. [安全模型](./security.md)
7. [排障指南](./troubleshooting.md)

### 我要使用 Web 工作区

1. [快速开始](./getting-started.md)
2. [配置说明](./configuration.md)
3. [排障指南](./troubleshooting.md)

### 我要参与开发

1. [架构设计](./architecture.md)
2. [安全模型](./security.md)
3. [本地开发与脚本](./development.md)
4. [源码阅读路线指南](./reading-guide.md)
5. [Agent 指南](./agent-guide.md)
6. [CONTRIBUTING.md](../../CONTRIBUTING.md)
7. [后续规划](./roadmap.md)

## 文档事实来源

| 文档内容 | 代码事实来源 |
| --- | --- |
| Web 启动参数 | `internal/host/web` 与 `qcode --help` |
| TOML、环境变量与默认值 | `internal/config/schema.go`、`defaults.go`、`environment.go` |
| Runtime 协议 | `docs/protocol/runtime-protocol.schema.json` |
| 架构边界 | Import 图和 Architecture Test |
| 构建测试命令 | `Makefile` 与 `web/package.json` |
| Web 体验语义 | `testdata/contracts/web-experience-contract.json` |
| Runtime 所有权与主流程 | `docs/zh-CN/architecture.md` 与 Architecture Test |
| 可靠性不变量 | `testdata/contracts/reliability-matrix.json` |
| 路线图 | 只描述目标，不作为“已交付”证明 |

实现与文档不一致时，应先核对实现，在同一变更中修正文档；适合自动化的内容应补充
漂移检查。
