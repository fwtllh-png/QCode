# 快速开始

## 1. 前置条件

| 依赖 | 说明 |
| --- | --- |
| Go 1.26+ | Runtime 必需 |
| Git | 仓库工作流和 Worktree 隔离必需 |
| Make | 推荐的统一构建入口 |
| Node.js + npm | 仅重新构建 Web 前端时需要 |
| macOS | 唯一支持的运行平台 |

Linux 和 Windows 不再提供构建、发布与运行支持。发布产物仅为 macOS amd64/arm64。

## 2. 安装并启动

```bash
git clone https://github.com/fwtllh-png/QCode.git
cd QCode
make install
qcode
```

`make install` 会依次安装 Web 依赖、构建静态资源和 Go 二进制，再原子安装到
`~/.local/bin/qcode`。二进制包含 Web 静态资源，运行期间不依赖源码目录或独立
前端服务。安装后在任意目录执行 `qcode`，只打开本机页面，不自动注册当前目录；无配置
启动默认启用受 Guard 管理的内置工具，并使用 `auto` 审批姿态。

同一用户只运行一个本机 Web Supervisor。通过页面 `Add workspace` 添加目录，
或显式执行 `qcode --workspace /path/to/project`，命令会注册该目录并打开带 Workspace 定位参数的已有
页面并正常退出。每个 Workspace 拥有独立 Runtime、Sandbox、Tool Registry、索引、
后台调度器和事件投影；页面侧栏会同时展示所有已注册 Workspace 及其 Session。

如果 `~/.local/bin` 不在 `PATH`，安装命令会输出需要加入 Shell 配置的路径。也可指定
标准安装前缀：

```bash
make install PREFIX=/usr/local
```

源码开发和调试使用 `make start` 时同样没有默认 Workspace；需要指定项目时使用
`make start START_WORKSPACE=/path/to/project`。该命令完成
Web 和二进制构建后，会比较 Owner Lease 中的构建身份；若已有 Supervisor 来自旧构建，
先等待其安全退出再启动新进程，避免继续提供旧的嵌入式 Web 资源。卸载使用
`make uninstall`，并可通过相同的 `PREFIX` 指定安装位置。

## 3. 首次引导

首次进入且尚未配置模型连接时，Web 直接展示主界面，并在会话区顶部显示一条
引导横幅；模型配置在 Settings 的 Connection 页面完成，配置前主界面可正常浏览
与管理工作区。所有连接统一为 OpenAI-Compatible 形态，完成配置需要填写四项要素：

1. Base URL（OpenAI-Compatible 端点，HTTPS 或回环地址）；
2. Protocol（`openai_chat` 或 `openai_responses`）；
3. 准确的 Model ID；
4. API Key。

模型元数据（Canonical/Wire ID、Context、Max Output 与完整 Capability 声明）
通过连接探测自动填写，或手动录入。没有默认 Provider 或 Model，也不通过内置枚举
限制 Model ID。API Key 由操作系统
Keyring 加密保存，不写入仓库、浏览器存储或 Setup Record；非敏感选择由 Runtime
管理。连接设置可在零 Workspace 状态下完成，此时不会构造具有目录访问能力的 Runtime。
配置模型之前添加的 Workspace 会处于待激活状态，配置完成后由 Runtime 自动激活。
Setup 完成后，页面依次引导添加或选择 Workspace、创建 Session，再进入 Composer，
不会代替用户自动创建 Session。Runtime 不从 Model ID 或 `/models` 列表猜测容量与
布尔能力。对于探测到 Reasoning 但未声明 Effort 档位的模型，提供
`low`、`medium`、`high`、`xhigh`、`max`，默认 `medium`，
用户可在提交前修改。
每个 Session 可从 Composer 快速切换模型，选择跨全部已配置连接的可用
Model（不同 Base URL 连接的模型以 `provider · model` 标注来源），切换在回合
之间热生效，不重建 Runtime。Settings 的 Connection 页面维护连接集合：
`Save connection` 新增或更新一条连接（不改变默认连接），`Apply and
restart` 保存并设为默认连接（新 Session 的基线），列表中可切换默认或移
除非默认连接（仍被 Session 使用的连接会拒绝移除）。添加、移除连接或切
换默认会重建空闲 Workspace Runtime；已有 Session 保留各自选定的
(provider, model)，不会被重置。Web 默认监听
`127.0.0.1:6732`；同一用户重复执行 `qcode` 时复用已有 Supervisor。

## 4. 直接运行二进制

已有安装产物时可显式打开目标项目：

```bash
qcode --workspace /path/to/project
```

不传 `--workspace`，且未显式配置 `execution.workspace` 或 `QCODE_WORKSPACE` 时，
不会添加任何目录，也不会自动选中已有 Workspace。普通启动只恢复已添加的列表；
移除最后一个 Workspace 后重启仍保持空列表。没有已保存的连接且未显式指定 Provider/Model
时进入首次引导，不会选择默认路由。支持的启动参数见[Web 使用指南](./usage.md)；
`qcode --help` 是当前 Binary 的参数事实。

## 5. 使用 Fixture

无需凭证和网络即可启动真实 Runtime 与 Web Transport：

```bash
./bin/qcode \
  --provider-fixture ./testdata/providers/openai \
  --provider openai \
  --model gpt-fixture \
  --workspace . \
  --no-open
```

Fixture 使用确定性的已记录响应，但仍经过真实 Session、Operation、Event、Guard 和
Persistence 路径。

## 6. 首次检查

进入 Web 后确认：

1. Settings 中 Runtime 状态为 Ready；
2. Model 与 Credential 状态符合引导阶段的选择；
3. 创建 Session 后 Composer 可用；
4. `suggest` Posture 下修改型 Tool 会进入 Approval；
5. 通过侧栏 Workspace 按钮可添加目录，并在不同 Workspace 的 Session 间切换；
6. 完成 Turn 后可查看 Receipt、Usage 与 Trajectory。

Runtime 无法启动时，页面保留 Boot Failure Surface，并展示结构化修复信息。

## 7. 开发验证

```bash
make web-check
make web-test
make web-e2e
make test
```

完整发布前门禁使用 `make verify`。
