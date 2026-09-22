# Sandbox 内部依赖失败诊断

调查日期：2026-09-21。范围为原会话执行证据、宿主与 macOS Sandbox 对照。
产品默认已在同日切换为 `v1` + `native`，但本文仍是当时的失败诊断，
不表示原 `/Users/bytedance/eds` 业务项目已重新编译或跑完指定测试。

## 结论

QCode 缺少统一的开发执行环境机制，Go 是本次已复现的样本：
宿主持久配置没有进入私有 Home，内部依赖源的认证没有接通，
macOS 默认临时目录行为也未被私有 TMPDIR 覆盖。
不能以操作系统沙箱成功执行了访问限制为依据，断言整个 Sandbox 功能没有问题。

下文记录具体证据和责任落点，不代表逐工具补丁方案。
可实施改造合同见[Sandbox 执行环境重构方案](./sandbox-execution-environment-plan.md)；
第 8 节把本次失败写成必须对照的 ResourceRequest 与回执夹具。
P1a 已禁止从 stderr 把“沙箱看不见”改判成“宿主不存在”；环境准备链仍待后续阶段。

同一内部依赖源在宿主可用。获批 Sandbox 可以连到该源，但正常凭证发现所得的
认证结果与宿主不同。原 Turn 还主动关闭了 Go 依赖获取，没有申请出网，
并错误地把沙箱不可见当成宿主不存在。

## 原会话证据

工作区为 `/Users/bytedance/eds`，会话为
`session_web_2c91d88e-4d43-4a7b-9add-84739977db8b`。
事件从只读 SQLite 索引定位到事件日志，并校验记录正文的 SHA-256。

| 事件 | 事实 |
| --- | --- |
| 实施 Turn `turn_5c5152a8d27c2876b13c0d393bfa7f75`，序号 49426 | Sandbox 使用私有模块缓存，GOPROXY 为默认公共代理 |
| 序号 58346 | 命令显式设置 `GOPROXY=off`，构建管道后接输出命令；外层退出码 0 不能证明构建成功 |
| 序号 58892–58893 | `TMPD=$(mktemp -d)` 在宿主 `/var/folders/.../T/` 路径报 `Operation not permitted` |
| 序号 59314–59315 | 再次设置 `GOPROXY=off`，模块报 `module lookup disabled by GOPROXY=off` |
| 序号 60692、61251 | 计划 S6 为 `in_progress`；验证为 `failed`，测试为 `not_evaluated`，审批请求数为 0 |
| 后续解释 Turn `turn_6e03998953b82124d7096595e8ddd8dd`，序号 61730、61736 | 在 Sandbox 内查询配置并隐藏部分 stderr，随后错误声称宿主没有内部代理和缓存 |

后续 Turn 的问题是“为什么最后一项任务仍是 pending”，只执行了两次
`shell_read`。`answered` 是回答结束的事实，不代表原计划验证完成。

## 运行时对照

使用当前工作树的真实 Darwin Sandbox、固定工作目录描述符、受控网络代理及
现有执行授权编译链路。网络授权限定到测试目标的 HTTPS 443 / CONNECT，
未开放任意外网。临时工作区、缓存和私有 Home 随探针退出回收。

目标为 `code.byted.org/gopkg/ctxvalues@v0.6.0`，版本元信息 URL 为：

```text
https://goproxy.byted.org/code.byted.org/gopkg/ctxvalues/@v/v0.6.0.info
```

| 项目 | 宿主 | Sandbox |
| --- | --- | --- |
| Go 代理 | `https://goproxy.byted.org\|direct` | `https://proxy.golang.org,direct` |
| Go 校验例外 | `GONOSUMDB=*.byted.org` | 空 |
| 内部模块缓存目录 | 存在 | 对应私有缓存不存在 |
| 不启用 curl 凭证发现的同 URL GET | HTTP 401 | HTTP 401 |
| 同一 `/usr/bin/curl -q -n` 命令，启用各自 Home 下的正常凭证发现 | HTTP 200 | HTTP 401，直接启动和登录 shell 一致 |
| 新建空模块缓存后 `go list -m -json` 查询上述版本 | 成功返回模块路径及 `v0.6.0` | 仅补入宿主非敏感 Go 配置仍失败 |
| `mktemp -d`、`mktemp -d -t qcode-debug` | 原 Turn 的失败发生在 Sandbox | 两者均报 EPERM；登录与非登录 shell 一致 |
| 显式模板 `mktemp -d "$TMPDIR/qcode-debug.XXXXXX"` | — | 成功 |

认证对照由 curl 自身使用宿主已有 `.netrc`，没有输出或复制凭证明文，
没有把宿主凭证文件授权给 Sandbox。宿主 Go 对照使用全新缓存，
因此成功不能归因于命中已有模块缓存。

补入 Go 配置后，Sandbox 仍保留原 `GOPROXY` 的 `|direct` 回退。
最终错误是向 `code.byted.org` 请求 `?go-get=1` 时返回 Forbidden；
该回退目标没有获得本次探针的授权。这与精确代理 URL 已测得的 401 分开记录，
不能把最终 Forbidden 误当成内部代理的响应。

`mktemp` 失败时，四个临时目录/Home 环境变量均正确指向 Sandbox 私有目录。
本机 `mktemp(1)` 手册说明：无模板或 `-t` 优先使用
`_CS_DARWIN_USER_TEMP_DIR`，不可用时才回退到 TMPDIR。
因此已排除“登录 shell 覆盖 TMPDIR”的假设。

探针测试通过表示完成了上述采证，不表示被测缺陷已修复。
本次只验证一个模块的元信息获取，不据此宣称整个业务依赖树、构建或测试成功。

## 责任位置与修复范围

1. **工具链配置准备**：`internal/platform/process/environment.go` 只继承进程环境白名单，
   不加载宿主持久 `go env -w` 配置；`process.go` 中 `sandboxEnvironment` 重设 HOME，
   使 Go 转向私有 Home 下尚不存在的配置文件。应通过工具链配置准备机制投影经过
   校验的非敏感配置，并保留来源；不能把某个内部代理域名写进通用默认值。
2. **依赖源认证**：私有 Home 隔离了宿主认证材料，而已测路径没有提供对应的受控认证能力。
   修复应定义按目标使用凭证的边界和实现，不应靠整体暴露宿主 Home 或复制全部凭证
   来恢复功能。仅开放网络目标或补上 GOPROXY 不足以修好本例。
3. **临时目录适配**：平台层需要考虑工具使用系统临时目录 API 的行为。
   只设置 TMPDIR 不足以支持本机默认 `mktemp`；回归验收必须覆盖真实系统工具，
   不能只检查环境变量。具体实现仍需设计，本次未验证通用修复方案。
4. **Agent 的诊断和恢复行为**：工具结果应保留不存在、无权限、未授权出网和认证失败的区别。
   模型应依据这些事实使用现有网络目标声明与审批流程，不能在 `GOPROXY=off` 下推断
   网络不可达，也不能从受限视图声称宿主没有配置或缓存。

模块缓存隔离本身属于既定边界；这里证实的是宿主缓存存在以及诊断误报。
是否引入受控缓存复用是另一个设计选择，并非恢复本例依赖下载的前置条件。
其他 Coding Agent 的内部实现未经本次对照，不能据此推测其具体隔离策略。
