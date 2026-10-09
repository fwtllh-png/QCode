# QCode

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](./LICENSE)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](./go.mod)
[![Release](https://img.shields.io/github/v/release/fwtllh-png/QCode?display_name=tag&sort=semver)](https://github.com/fwtllh-png/QCode/releases)
[![Discussions](https://img.shields.io/github/discussions/fwtllh-png/QCode)](https://github.com/fwtllh-png/QCode/discussions)

**一个使用 Go 实现的、本地运行、受控执行的 AI Coding Agent Runtime。**

QCode 将仓库理解、模型调用、受治理工具、审批、验证、持久化会话与 Subagent
协作统一放在一套 Runtime 协议之后，以 macOS 桌面应用为主要入口，加载本机 Web 界面。

> 项目状态：初始开发版本。首次公开稳定发布前，接口和持久化格式仍可能调整。

`docs/zh-CN` 下的中文产品手册描述已交付行为；
[源码阅读指南](./docs/zh-CN/reading-guide.md)提供架构、实现与测试的阅读路径。

## 为什么建设 QCode

多数 Coding Agent 原型优先追求演示效果，QCode 更关注产品长期运行后必须具备
的工程属性：

- **本地控制权**：源码和执行仍在用户工作区中。
- **一个 Web 入口，一套 Runtime**：主 Agent 与 Subagent 共享同一套
  Operation/Event 和安全语义。
- **受控执行**：所有修改型工具都经过 policy、permission、constitution、journal
  与操作系统沙箱检查。
- **证据优先**：搜索、编辑、审批、验证、用量和 trace 都形成可检查的运行事实。
- **扩展但不分叉控制面**：MCP、Skill 和 Subagent 都通过受治理的
  Adapter 接入。
- **默认关闭而不是假装安全**：安全能力不可用时明确报告，不静默降级为“看起来已隔离”。

## 快速开始

环境要求：

- Go 1.26 或更高版本
- Git
- Node.js 和 npm（`make build` 会先生成并嵌入 Web 前端）
- Xcode Command Line Tools（编译 Swift 桌面壳）

目前仅支持 macOS（amd64/arm64）；Linux 和 Windows 不再提供构建、发布与运行支持。

| 平台 | Runtime | 沙箱边界 |
| --- | --- | --- |
| macOS | 支持 | Seatbelt Backend 可用时为 Strong |

```bash
git clone https://github.com/fwtllh-png/QCode.git
cd QCode
make start
```

`make start` 构建并打开 `dist/QCode.app`；之后可直接打开该 App，也可复制到
`/Applications`。桌面壳无参数启动内嵌 Runtime，不自动添加或选中当前目录。
没有默认 Workspace，用户通过 `Add workspace` 选择目录。
已有 Web Supervisor 运行时，桌面壳复用该进程，无需启动第二个 Web 服务。
普通启动只恢复已添加的列表，删除最后一个 Workspace 后重启仍保持空列表。
首次进入时不会预选 Provider 或 Model。所有连接统一为 OpenAI-Compatible 形态，
用户必须在页面中填写 Base URL、Protocol、Model ID 与 API Key 四项要素；模型
Context、Output 和 Capability 元数据通过连接探测自动填写或显式录入，
Runtime 不猜测模型限制。API Key 由操作系统 Keyring 加密保存，非敏感选择与
元数据由 Runtime 管理；无需创建或编辑配置文件。Session 可跨全部已配置连接
（不同 Base URL）切换已验证模型；新增未知模型需要从 Connection 设置提交其元数据。

重新构建后，退出 App 并重新打开以使用新 Runtime；若复用的是终端启动的进程，
需先在原终端停止它。内置工具默认启用并受 Guard 管理，新 Session 默认使用
`auto` 审批姿态；显式的 `execution.tools` / `QCODE_TOOLS` 配置仍生效。

Web 默认监听 `127.0.0.1:6732`。调试时可通过 `make install` 安装独立 Runtime
到 `~/.local/bin/qcode`；直接运行只输出启动 URL，不自动打开浏览器。

安装、初始配置、凭证、持久化和 Web 使用方式见
[快速开始](./docs/zh-CN/getting-started.md)。

## 产品入口

| 入口 | 命令或路径 | 主要用途 |
| --- | --- | --- |
| macOS 桌面应用 | `make start` 或打开 `dist/QCode.app` | 日常入口，覆盖会话、审批、变更、Subagent 与运行状态 |
| 独立 Runtime | `qcode` | 开发调试，输出本机 Web 地址供手动访问 |

## 一分钟理解安全模型

Agent 固定使用 `act`，按需规划并执行用户请求，不提供模式切换。

界面中的 `Permissions` 描述工具权限：

- `Read only`：只读
- `Auto`：普通操作自动执行，需要时请求审批
- `Full Access`：普通命令可读写宿主文件并直接访问网络，无需常规审批；仍保护凭据、工作区控制目录和 QCode 状态，并遵守显式策略

默认使用 `Auto`。凭证应保存为环境变量、文件或系统 Keyring 的引用，TOML 中
不应出现原始密钥。

## 仓库结构

```text
cmd/qcode/          进程入口
internal/common/         按职责组织的公共契约与基础工具
internal/host/           Web Host 与 Runtime Transport
internal/runtime/        Operation/Event Runtime 与 Agent Engine
internal/adapter/        Provider、Model、Tool、MCP、Skill
internal/security/       Policy、Permission、Constitution、Sandbox
internal/orchestration/  Subagent、Admission/Budget、Chat Merge
internal/persist/        SQLite、Event Log、Session、Snapshot、Journal
internal/observability/  Usage、Trace、Verify、Diagnostics、Telemetry
internal/platform/       进程和操作系统集成
web/                     React/TypeScript 本机 Web 前端
desktop/                 macOS 桌面壳（WKWebView）与 .app 构建
docs/                    持续维护的中文文档
scripts/                 构建、验证、配置和发布脚本
testdata/                Hermetic Provider 与 Benchmark Fixture
```

## 文档

| 主题 | 文档 |
| --- | --- |
| 文档总览 | [docs/zh-CN](./docs/zh-CN/README.md) |
| 产品介绍与定位 | [项目介绍](./docs/zh-CN/overview.md) |
| 安装与上手 | [快速开始](./docs/zh-CN/getting-started.md) |
| 配置 | [配置说明](./docs/zh-CN/configuration.md) |
| Web 与工作流 | [使用指南](./docs/zh-CN/usage.md) |
| 架构 | [架构设计](./docs/zh-CN/architecture.md) |
| 安全 | [安全指南](./docs/zh-CN/security.md) |
| 本地开发 | [本地开发](./docs/zh-CN/development.md) |
| Agent 上下文 | [Agent 指南](./docs/zh-CN/agent-guide.md) |
| 源码导读 | [源码阅读指南](./docs/zh-CN/reading-guide.md) |
| 产品现状 | [能力概览](./docs/zh-CN/overview.md) |

## 开发

```bash
make build
make test
make docs-check
make verify
```

`make verify` 覆盖面很广，部分测试依赖平台安全能力。修改单个子系统时，应优先使用
[本地开发指南](./docs/zh-CN/development.md)列出的聚焦命令。

修改 Runtime 契约、安全边界、持久化状态或生成协议文件前，请阅读
[CONTRIBUTING.md](./CONTRIBUTING.md)。

疑似安全漏洞必须按照 [SECURITY.md](./SECURITY.md) 私下报告，不应创建
公开 Issue。

QCode 使用 [Apache License 2.0](./LICENSE)。
