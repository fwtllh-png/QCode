# 桌面应用

QCode 提供 macOS 桌面应用（QCode.app）：一个原生 WKWebView 壳，加载本机
Web Supervisor 的页面。桌面应用与本机 Web 共享同一 Runtime、同一数据目录与
同一套安全语义，不引入第二条产品入口逻辑。

## 构建与安装

```bash
make desktop-app        # 开发构建，产出 dist/QCode.app（adhoc 签名）
VERSION=0.1.0 RELEASE_STAGE=experimental make package-app   # 发布打包
```

`make desktop-app` 依赖 `make build`，会先构建并内嵌 Web 前端。产物结构：

```text
QCode.app/Contents/MacOS/QCode          桌面壳（Swift，universal）
QCode.app/Contents/MacOS/qcode-runtime  内嵌的 qcode Runtime 二进制
```

> 壳与 Runtime 的文件名不能只差大小写：macOS 默认 APFS 卷大小写不敏感，
> `QCode` 与 `qcode` 会冲突为同一个文件。

## 启动与进程语义

桌面壳的启动流程与 CLI 复用语义一致：

1. 先扫描默认数据目录（`~/.qcode/v1`）下的 Supervisor lease 文件，逐个用
   `/healthz` 探活；存在可用 Supervisor 时直接**收养**（不新起进程）。
2. 收养失败时拉起内嵌的 `qcode-runtime --no-open`，等待 `/healthz` 返回
   `ready` 或 `setup_required` 后在窗口中加载页面。
3. 若拉起失败但 `127.0.0.1:6732` 上已有合法 QCode 服务（例如 CLI 使用了
   自定义 `--data-dir` 启动），兜底收养该进程。

退出语义：

- 壳**自己拉起**的 Runtime：退出 App（⌘Q）时发送 SIGINT 并等待 drain 完成
  （最多约 35 秒，超时升级为 SIGKILL）。
- 壳**收养**的 Runtime：退出 App 不影响它，原属主（通常是终端里的 `qcode`）
  继续持有。
- Runtime 意外退出时，壳会自动重新走收养/拉起流程并重载窗口。
- 关闭最后一个窗口不退出 App；点 Dock 图标可重新打开窗口。

页面始终从 `http://127.0.0.1:6732` 加载（不使用自定义 URL scheme）：
前端 WebSocket、服务端 Origin 栅栏与本地存储都要求稳定的本机 origin。

## 通知与角标

WKWebView 不支持网页版 Notification API。桌面壳在页面加载前注入一个
Notification 兼容层，把前端的后台活动通知（审批请求、失败、完成等）转发为
原生 macOS 通知：

- 通知权限在首次于设置中启用通知时向系统申请；
- 前端的通知开关（localStorage `ch.notifications.enabled`）语义不变；
- 点击通知会聚焦窗口并跳转到对应会话；
- 前端写入 `document.title` 的活动状态（如 `(2) Action required · QCode`）
  会同步为窗口标题，计数部分映射为 Dock 角标。

## 签名与分发

`make desktop-app` 产出 adhoc 签名，仅限本机开发使用。对外分发需要
Developer ID 签名与公证，`make package-app` 按环境变量门控启用：

| 环境变量 | 用途 |
| --- | --- |
| `APPLE_DEVELOPER_IDENTITY` | Developer ID 证书名（codesign） |
| `APPLE_NOTARY_PROFILE` | notarytool 的 keychain profile（推荐） |
| `APPLE_ID` + `APPLE_PASSWORD` + `APPLE_TEAM_ID` | notarytool 备选凭据 |

凭据齐备时自动执行 codesign（Hardened Runtime）→ notarytool 提交 → staple；
凭据缺失时明确输出"未签名/未公证"并继续产出，不会静默降级。

QCode 需要在用户工作区执行任意 CLI 工具与 `sandbox-exec`，因此桌面应用
不启用 macOS App Sandbox，也不上架 Mac App Store，仅通过 Developer ID
门外分发。

发布产物位于 `dist/release/`：`QCode.app`、`QCode-<VERSION>-macos.zip`、
`SHA256SUMS` 追加条目与 `package-manifest.json` 的 `desktop_artifacts` 记录。

## 已知边界

- **目录选择器**：添加 Workspace 时弹出的目录选择面板由 Runtime 进程
  （osascript）呈现，属于独立进程窗口，不附着在 App 窗口上；功能不受影响。
- **刷新**：应用菜单刻意不提供 Reload 项；如需刷新页面，使用页面内的
  重连入口或重启 App。误刷新不会丢数据（状态由服务端恢复），但会重置滚动
  位置等内存态。
- **深链与自动更新**：`qcode://` URL 注册与 Sparkle 自动更新尚未实现，
  计划见后续规划。
- **外部链接**：会话内的外部 http(s) 链接一律交给系统默认浏览器打开。

## 验证清单

发布前的人工回归项：

1. 冷启动：无 Supervisor 时窗口出现并进入引导/工作区页面。
2. 先 `qcode` 后开 App：App 收养现有 Supervisor，原进程不退出。
3. App 先启动，终端再执行 `qcode --workspace <路径>`：CLI 注册成功，
   App 内出现该 Workspace。
4. ⌘Q：App 退出且自己拉起的 Runtime 完成 drain 后退出。
5. 通知：后台会话触发审批时收到系统通知，点击后聚焦并跳转。
6. WebKit 兼容性：流式渲染与打字光标动效、剪贴板复制、附件选择、删除/重命名/
   归档会话的应用内对话框、中文输入法输入、暗色模式。
