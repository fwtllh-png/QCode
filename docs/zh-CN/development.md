# 本地开发、测试与脚本

## 开发环境

必需依赖是 Go 1.26+、Git 和 Make。重新构建 Web 前端还需要与
`web/package-lock.json` 兼容的 Node.js 与 npm。

```bash
git clone https://github.com/fwtllh-png/QCode.git
cd QCode
go mod download
make web-install
make build
```

## 快速循环

Go 变更：

```bash
gofmt -w path/to/file.go
go test ./path/to/package -count=1
```

Web 变更：

```bash
npm --prefix web run check
npm --prefix web test
npm --prefix web run build
make web-e2e
```

Web 主题 Token 集中在 `web/src/ui/theme/tokens.css`，统一控件外观在
`theme/components.css`，共享交互组件在 `ui/primitives`。主题层只负责呈现，
不从文本或颜色反推 Runtime 状态。圆角按工具表面 8px、控件 12px、菜单 14px、
输入区 20px、弹层 24px 分层；新增样式需使用语义 Token 并同步
`testdata/contracts/web-experience-contract.json`，不要在组件内重复维护色板。
动效时长与曲线只从 CSS Token 读取，不在组件中复制计时常量：
反馈 120ms、菜单进入 160ms、退出 180ms、折叠 220ms、弹层进入 240ms、
骨架呼吸 1600ms。`ui/primitives/Presence.tsx` 统一延迟卸载，退出内容立即
`inert`，嵌套弹层通过 Presence 上下文释放焦点；快速重开取消旧退出，
懒加载表面到达后才开始进入。表面标记 `data-motion-surface`，
遮罩标记 `data-motion-backdrop`，不要再单独叠加入场动画。
`motion.ts` 共享监听系统动态偏好与页面可见性，减少动态效果或隐藏页面时直接结算，
无持续轮询；CSS 时间解析覆盖构建压缩后的 `.18s` 格式。
`Collapse` 共用该生命周期，保留 CSS Grid 高度过渡与闭合后卸载。
`GitTools` 按需加载并显式绑定 Workspace；异步查询支持 AbortSignal，不能把旧请求结果
写入切换后的窗口。Git patch 使用 `diff`（jsdiff）的 unified-diff Parser；
`GitPatchView` 的行号来自 hunk 坐标，DOM 仅保留可见视口与前后各一个视口，
行高取实际 CSS 几何值。二进制或不可解析的 patch 保留原文，不伪造文件前后内容。
浏览器回归覆盖浅深主题、手机抽屉、Git 浮窗、Trajectory 工具栏、嵌套焦点、动态效果、
高对比度与 200% 缩放。视觉基线需在实际检查截图后更新。
对运行中的开发环境进行验证时，可先用 `make build BINARY=.tmp/qcode-material3`
构建独立二进制，再通过 `QCODE_E2E_BINARY` 指定它运行 Playwright；
测试使用临时数据目录和随机端口，不替换当前 Web Owner。

文档和交付检查：

```bash
make docs-check
git diff --check
```

## 主要 Make Target

| Target | 作用 |
| --- | --- |
| `make build` | 构建包含嵌入式 Web 资源的 `bin/qcode` |
| `make desktop-app` | 构建 macOS 桌面壳 `dist/QCode.app`（依赖 `make build`） |
| `make package-app` | 打包桌面应用发布产物（含签名与公证门控，见[桌面应用](./desktop.md)） |
| `make test` | 执行串行 Hermetic Go Test Lane |
| `make test-platform-capability` | 验证真实宿主机 Sandbox |
| `make test-integration` | 验证真实 Binary 与 Web Transport |
| `make test-release` | 执行 Race、Cross-build、脱敏与发布门禁 |
| `make web-install` | 使用 Lockfile 安装前端依赖 |
| `make web-check` | TypeScript 静态检查 |
| `make web-test` | Web Unit Test |
| `make web-build` | 构建无 Source Map 的生产资源 |
| `make web-e2e` | 启动真实 Binary，以 Playwright 验证 Web 主流程与响应式约束 |
| `make protocol-schema` | 生成 Runtime Protocol Schema |
| `make web-experience-check` | 校验 Web 体验契约 |
| `make host-journey-contract` | 校验 Runtime 与 Web 主旅程 |
| `make hotspot-baseline` | 校验热点职责归属 |
| `make security-side-effect-check` | 校验生产副作用入口 Inventory 与 Owner Allowlist |

`web/dist` 是被 Git 忽略的本地构建目录。`make build` 先执行 `web-build`，再使用
`webbundle` Build Tag 将生成资源嵌入 Go Binary；不要直接用裸 `go build` 产出发布
Binary。普通 Go Test 不依赖该目录，因而干净 Checkout 可以直接执行。

`make verify` 是完整门禁。Make 负责串行化 Web Build 和依赖嵌入 Go Binary 的步骤，
避免 Vite 清空 `web/dist` 时 Go 编译器正在读取资源。

## 测试分层

| Lane | 命令 | 契约 |
| --- | --- | --- |
| Hermetic | `make test` | 无网络、凭证、GUI 或宿主机 Sandbox 依赖 |
| Platform Capability | `make test-platform-capability` | 真实 OS Sandbox 行为 |
| Integration | `make test-integration` | 真实 Binary、HTTP/WebSocket 与 Runtime 生命周期 |
| Release | `make test-release` | Race、Benchmark、Cross-build、脱敏和打包 |

测试证据写入 `.tmp/test-lanes/`，状态是 `passed`、`failed` 或 `unavailable`。缺失平台
前置条件不能伪装为通过。

Web 入口发布还会运行 `make web-release-drill`。该门禁用当前 Binary 创建真实
Session 和已完成 Turn，在进程停止后复制 Data Dir 并逐文件校验 SHA-256，再让
`PREVIOUS_RELEASE_REF` 构建出的上一正式发布 Binary 完成 Session List、Load、History
和 Turn Recovery。该参数没有分支或上一提交回退值：运行者必须通过
`PREVIOUS_RELEASE_REF` 配置不可变的上一正式发布 Tag 或 Commit，或通过
`PREVIOUS_BINARY` 指向保留的上一发布产物。报告写入
`.tmp/release/web-downgrade-drill.json`。

Release Lane 还会执行 `make web-streaming-soak`，持续一小时验证 WebSocket Event
完整性及 Heap、Goroutine、文件描述符收敛。`make test-release` 必须从 clean Commit
运行；最终工作树不 clean 时门禁失败，Parity Report 不能以 `qualified_dirty` 代替
`verified`。Release Lane 同时执行 `make web-supply-chain-check` 和
`make web-vulnerability-check`，校验前端依赖许可证 allowlist、raw/gzip/brotli
Bundle Budget，并拒绝 npm Audit 报告中的 High 或 Critical 漏洞。

## 生成文件

不要手工编辑：

- `docs/protocol/runtime-protocol.schema.json`
- `web/dist/**`（本地生成且不提交）

使用：

```bash
make protocol-schema
```

## 架构约束

- Host 只提交 Operation、消费 Event 和查询 Read Model。
- Runtime 业务循环位于 `internal/runtime/agent`。
- 构造位于 `internal/runtime/app/wire`。
- 修改型工具必须经过 Guard、Approval、Journal 与 Sandbox。
- Web 只监听 `127.0.0.1`，不得增加通用公网 HTTP Host。
- Credential Secret 只进入环境变量、受保护文件或 OS Keyring。

热点职责位于 `testdata/contracts/hotspot-baseline.json`。新增职责应先拆分 Owner，
而不是把无关符号堆进同一热点文件。不要用行数、Fanout 或函数长度棘轮否决合理改动。

## 发布

```bash
VERSION=0.1.0 RELEASE_STAGE=experimental make package
make test-release
```

发布产物是独立 QCode Binary，其中包含 `web/dist`。Web 不单独发布，也不在运行中
替换当前进程。发布前必须通过 Web Parity、Cross-build、Secret Leak 和文档治理门禁。
