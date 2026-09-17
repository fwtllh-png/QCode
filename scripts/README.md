# 仓库脚本

本目录保存稳定的验证、冒烟、本地配置和发布打包入口。除非脚本明确说明，否则从仓库
Root 运行。

| 脚本 | 网络 | 输出或副作用 |
| --- | --- | --- |
| `check-docs.sh` | 无 | 检查 Markdown Link 与中文单一文档树 |
| `check-brand.sh` | 无 | 扫描已跟踪源码中的历史品牌 |
| `test-brand-check.sh` | 无 | Brand Scanner 自测 |
| `run-test-lane.py` | 取决于被测命令 | 写入 Passed、Failed 或 Unavailable JSON Lane 证据 |
| `check-hotspot-baseline.go` | 无 | 校验当前热点职责归属与体积预算 |
| `securityeffects` | 无 | 使用 Go AST 校验生产副作用入口及其 Owner Allowlist |
| `webexperiencecheck` | 无 | 校验 Web 布局、Token、Motion、Viewport 和 CSS 静态契约 |
| `webassetmanifest` | 无 | 生成并核验嵌入 Web 产物的 SHA-256、大小和 MIME Manifest |
| `webprotocolgen` | 无 | 从 Web Route Registry 生成或校验 Web Host 传输契约 |
| `package-release.sh` | 无 | `dist/release` Binary、Checksum、SBOM、Manifest |
| `web-release-drill.py` | 无 | Web RC Data Dir 备份恢复与上一 Binary 降级证据 |

## 约定

脚本必须：

- 解析 Repository Root，不假设调用目录；
- 使用严格错误处理并保留失败退出码；
- 通过环境变量暴露机器相关路径；
- 不打印 Secret，也不从受 Git 跟踪的仓库文档读取 Secret；
- 只向约定的 Build Directory 写生成产物；
- 使用 Trap 清理临时文件；
- 接受参数时提供 `--help`。

## 常用命令

```bash
make docs-check
make script-test
make test
make hotspot-baseline
make architecture-freeze
make host-journey-contract
make web-experience-check
make test-platform-capability
make test-integration
make brand-check
make secret-leak-test
make security-side-effect-check
make web-release-drill PREVIOUS_RELEASE_REF=<上一发布提交>
make web-supply-chain-check
make web-vulnerability-check
VERSION=0.1.0 make package
```

完整开发与发布背景见
[docs/zh-CN/development.md](../docs/zh-CN/development.md)。
