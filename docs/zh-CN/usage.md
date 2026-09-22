# Web 使用指南

从源码安装一次：

```bash
make install
```

之后在任意目录直接启动：

```bash
qcode
```

普通启动只打开浏览器，不把当前目录、源码目录或安装目录自动添加为 Workspace，
也不自动选中列表中的目录。服务默认监听 `127.0.0.1:6732`。
显式执行 `qcode --workspace /path/to/project` 才会注册并打开该目录；已有 Supervisor
时复用现有进程。`make start` 同样没有默认目录，只有显式传入 `START_WORKSPACE`
才会添加并打开对应项目。它仅作为源码开发入口保留。
它会使用 `--replace-owner` 比较构建身份并重启旧的开发 Supervisor；直接执行已安装的
`qcode` 仍复用现有 Supervisor。

## 启动参数

| 参数 | 说明 |
| --- | --- |
| `--workspace PATH` | 显式添加并打开目录；也可通过 `execution.workspace` 或 `QCODE_WORKSPACE` 显式指定，无隐式默认值 |
| `--replace-owner` | 构建身份变化时重启已有 Web Owner；仅供源码开发启动使用 |
| `--config PATH` | TOML 配置文件 |
| `--data-dir PATH` | 持久状态目录 |
| `--host 127.0.0.1` | 监听地址；只接受 Loopback |
| `--port PORT` | 监听端口；默认 `6732`，`0` 仅用于测试或临时隔离 |
| `--open` | 启动后打开系统浏览器 |
| `--no-open` | 禁止自动打开浏览器 |
| `--enable-tools` | 启用内置 Workspace Tool |
| `--posture MODE` | `suggest`、`auto` 或 `never` |
| `--mcp-config PATH` | State Directory 内的版本化 MCP 配置；stdio Server 还需显式 `host_trusted=true` |
| `--provider ID` | 覆盖配置中的 Provider |
| `--model ID` | 覆盖配置中的 Model |
| `--api-key-env NAME` | 使用环境变量中的 Provider Credential |
| `--provider-fixture PATH` | 使用 Hermetic Provider Fixture |
| `--version` | 输出构建版本 |

`--host` 固定为 `127.0.0.1`，`bypass` 不允许作为 Web Posture。启动参数只负责构造
Web Host；会话、审批、输入、工具执行和持久化仍由 Runtime 负责。

## 代码结构与符号搜索

仓库索引会自动提取主要编程语言的语法结构，供符号搜索、定义搜索和仓库地图
使用。语言覆盖、提取级别与降级边界见[代码结构提取](./code-structure.md)。

## Web 工作流

页面启动后可完成：

- 添加和切换 Workspace，并创建、搜索、切换、重命名、置顶、归档和删除 Session；
- 提交 Prompt，查看流式 Text、Reasoning 和 Tool Activity；
- 处理 Approval 与结构化 Input；
- 检查 Diff、Plan、Checkpoint、Usage、Diagnostics、Task、Agent 和 Receipt；
- 浏览、搜索和预览 Workspace Resource；
- 查看三泳道 Trajectory 并在 Chat 与 Tool Record 间双向定位；
- 按 Turn、用户问题、Tool 和文件引用搜索长会话；
- 管理 Credential 与受支持的 Extension 状态。

界面采用低饱和 Material 风格，浅色和深色共用语义颜色、控件尺寸与分层圆角。
Settings 中可选择跟随系统或固定主题。按钮与输入使用 120ms 状态反馈，
菜单进入为 160ms，弹层进入为 240ms，退出为 180ms。
关闭时立即禁用退出内容的交互并恢复焦点，过渡结束后再卸载；
快速重开可以中断退出，不会因旧计时器再次消失。嵌套弹窗只由最上层处理焦点，
从命令菜单打开新弹窗后，关闭会返回稳定的菜单按钮或输入框。
骨架屏使用轻微呼吸效果，已有正文刷新时不替换成整页骨架屏。
系统启用减少动态效果或页面进入后台时，停止持续动画并立即完成待卸载过渡；
不对逐 Token 输出重复播放入场效果，也不增加模型请求。
已完成 Turn 仍默认折叠执行过程；若完成时正在上翻阅读，则保留当前展开状态，
回到底部后再折叠。展开和收起使用 220ms 高度过渡，关闭后卸载详情；
搜索和 Trajectory 定位继续自动展开目标执行过程。

对话标题栏保留会话定位和 Git 工具入口，不再提供右侧 Latest turn 面板、会话下载按钮
或 `/export` 命令。历史工具内容与结构化验证记录仍可在 Chat 和 Trajectory 中查看。
Trajectory 顶部的 Prefix 使用紧凑数字，悬停显示完整 token 数；窄屏下搜索栏自动换行。
不超过 720px 时，左侧会话也通过标题栏菜单按需打开。抽屉和设置弹层支持
Escape 关闭、Tab 焦点圈定和关闭后恢复焦点，不允许键盘落入被遮挡的主界面。

右上角 Git 悬浮窗默认展示，不占用对话列宽；标题栏的 `Git tools` 可以关闭或重新打开。
同一 Workspace 内切换 Session 保留开关状态，切换 Workspace 后默认重新展示对应仓库。
进入 Trajectory 时暂时隐藏浮窗，返回 Chat 后恢复，避免遮挡记录检查区。
手机端默认使用非模态紧凑摘要，不抢输入焦点；扩大窗口或进入分支、Diff 详情时使用模态视图。
窗口支持收起、扩大、
查看实时变更、搜索与切换本地分支。`Changes` 按已暂存/未暂存分类展示文件和逐文件 Diff，
行数来自 Git 的结构化统计，二进制单独标注，未跟踪文件不计入行数总计。
Diff 视图使用宽窗口，代码区填满剩余高度；支持展开到近全屏和收起文件列表。
手机端文件列表与代码上下排列，收起列表可为代码腾出空间。调整视图保留所选文件与滚动位置，
长 Diff 仍按可见区域虚拟渲染，不因扩大窗口加载全部代码行。
这些数据属于当前 Workspace，不是最近 Turn 的交付清单。对话不再重复显示 `Produced files`，
Workspace 行也不再显示分支选择器；历史交付记录仍保留在工具 Diff 和 Trajectory 中，
任务进度和子 Agent 保持原有位置。
打开、手动刷新、窗口重新聚焦和活动状态改变时刷新，不持续轮询，也不调用模型来查看 Git。

`Commit or push` 打开操作确认框。默认只提交已暂存文件；包含未暂存文件需要明确勾选，
推送需要选择命名 Remote。提交说明必填，不使用模型生成。
`Commit`、`Commit and push`、`Push` 和创建分支直接调用 Runtime 的受限 Git 操作入口，
由既有 Git Tool、Guard 和 VCS Broker 执行，不创建 Turn、不调用模型，也不写入聊天消息。
按钮确认只授权本次结构化操作对应的精确工具参数，不覆盖策略或仓库规则中的拒绝项。
浮窗展示实际 commit hash 和推送结果；提交成功但推送失败时保留提交结果，可单独重试推送。
网络断开或结果不确定时不自动重试提交，应先刷新 Git 状态。
不必先创建 Session；若存在当前 Session，则同时遵循其只读和隔离限制。
不会清空输入框草稿或挪用附件上下文。执行期间 Workspace 不接受新的 Turn 或 Git 操作，
分支、暂存区或本地 Git 配置已变化的旧请求会被拒绝。操作仅支持仓库根目录的非 detached 分支。
只读、忙碌、暂停待恢复或隔离 Worktree 会话不允许修改主 Workspace 的 Git 状态，
也不提供强制推送、Amend、Reset 或丢弃更改的快捷操作。

Session 侧栏按 Workspace 分组。`Add workspace` 是分组标题旁的明确操作；
每个当前或悬停的 Workspace 行尾提供独立的“对话气泡 +”按钮，新 Session 必须从所属
Workspace 创建。若按钮属于非当前 Workspace，Web 先切换权威 Runtime，再提交创建请求，
不会把 Session 错建到当前 Workspace。分支管理统一位于 Git 悬浮窗，低频删除操作渐进披露；
新 Session 的 Shared/Worktree 默认值在 Settings 中统一配置，不在每个列表项重复显示。
搜索、归档与 Session 行级操作继续渐进披露。未指定标题的新会话
先显示 `New Chat`；`turn.start` 被接受后，Runtime 尽早使用 `summary` 路由异步提炼
“动作 + 核心对象”，并原位更新侧栏，不等待任务结束。模型结果返回前保持 `New Chat`，
不再把第一条 Prompt 或其截断文本当作标题。命名不添加聊天消息，成功命名后不再反复改名。

标题请求只包含本次用户可见文本与命名规则，不携带系统上下文、附件或工具日志。它共享
Provider 并发、冷却和会话预算，可以在主任务工具执行期间完成，不受整轮 Engine 锁阻塞。
主任务失败、Blocked 或取消不取消命名；进程关闭会取消请求。命名失败不回填 Prompt，
同一 Turn 不重复请求，后续 Turn 可为仍未命名的会话再尝试。取得并发许可后，请求超时复用
`context.semantic_narrative_timeout`；这与上下文摘要是否启用无关。
标题以单行、最多 256 个 UTF-8 字节保存，显示宽度由界面省略号处理；不是截取前几个词
充当摘要。标题 JSON 的字节上限由最坏 Unicode 转义与对象外壳推导，即 `6 × 256 + 12`。

Runtime 显式区分默认、临时、自动和手动标题，并保存独立命名版本；置顶等操作不影响
命名，显式改名（即使文字未变）会阻止未完成的自动结果覆盖。缺少来源信息的旧会话按
手动标题保护，不根据 `New Chat` 字样猜测或批量回填。已有 temporary 来源会话可在下一
Turn 用模型生成的标题替换；已有自动或手动标题保持不变。

对话底部的运行状态栏显示思考、工具准备、工具执行与继续处理等当前阶段。正常的工具
衔接不再作为聊天卡片显示；限流、重试、输出不完整和真实错误仍保留独立提示。

侧栏的 `Add workspace` 打开 Workspace 管理界面。点击 `Choose folder` 后由本地 Host 打开
操作系统目录选择器；用户选中的目录由 Supervisor 规范化物理路径、持久化 Registry，
并为该目录构造独立 Runtime，不需要在浏览器中手工输入路径。HTTP RPC 和内容下载通过
`X-QCode-Workspace-ID` 路由，WebSocket 在鉴权帧中携带 `workspace_id`；未知
Workspace、跨 Workspace Session 和内容句柄均拒绝访问。浏览器为每个 Workspace
分别保存事件 Cursor、选中 Session、草稿和反馈。当前 Workspace 使用实时事件流；
Workspace Catalog 和 Session 摘要在页面重新可见时刷新，不持续轮询 Git 状态。
Trajectory 也由新 Runtime Event 驱动增量 Trace 查询。裸 Supervisor URL 不隐式选择
默认 Workspace；用户必须先选择一个 Ready Workspace，页面和 Host 才允许创建 Session。
只有 `qcode --workspace PATH` 或显式 Workspace 配置才会让启动器定位到目录。
首次启动允许零 Workspace，模型连接设置不依赖默认项目；添加目录后才构造其 Runtime。
Workspace 管理界面可以移除任意 Workspace。移除只会注销并关闭对应 Runtime，不会
删除本机目录、Git 内容或持久化 Session。移除当前 Workspace 后，Web 自动切换到另一
个 Ready Workspace；移除最后一个后进入 Workspace 选择空态，重启不会自动补回目录。
已有记录不会依据目录名或路径自动删除，历史 Session 数据保持不变。所有 Runtime HTTP RPC、
内容下载和 WebSocket 鉴权都必须携带显式 Workspace ID，不存在默认 Workspace 回退。
Git Workspace 会在侧栏显示当前本地分支，并可从本地分支列表直接切换。切换在沙箱内
执行，活动 Turn 或待处理 Operation 存在时拒绝；Git 自身仍负责拒绝会覆盖本地修改的
切换。

Agent 不通过 `exec_command` 写 `.git`。提交与同步工作流使用结构化的 `git_add`、
`git_commit`、`git_switch`、`git_fetch`、`git_pull` 和 `git_push`；参数由 VCS
Broker 白名单校验，其中 pull 只允许 fast-forward，push 不允许 force refspec，并且
远端写入要求单次审批。`git_status`、`git_diff`、`git_log`、`git_remote`、
`git_branch`、`git_show` 和 `git_blame` 保持只读。
分支协作使用 `git_merge`、`git_rebase`、`git_cherry_pick`、`git_restore`、
`git_stash`、`git_tag` 和 `git_amend`。这些工具只接受固定参数结构；merge、rebase、
cherry-pick、restore、stash 和 amend 可能改变历史、产生冲突或丢弃未提交内容，因此
要求结构化 Plan 和单次审批。冲突不会被自动掩盖，Git 保留冲突状态供后续检查和处理。
冲突处理使用 `git_conflict`，仅允许 merge/rebase/cherry-pick 的 continue 或 abort；
不开放 `reset --hard`，continue 也不会启动交互式编辑器。
本地 `git_add`、`git_commit` 和 `git_switch` 属于有界本地变更，不会单独触发自适应
Plan；`git_pull` 和 `git_push` 仍按网络或外部变更要求 Plan。

Tool Catalog 的 `discovery_terms` 保存不授予权限的多语言检索词。首轮投影先保留核心与
当前已物化工具，再按相关度排序其他工具，并在模型声明的 Tool Definition 与 Schema
容量内填充，不再使用固定的“相关工具数量”。因此中文的 Git、Web、LSP、格式化、调试
和依赖工作流不需要先失败一次再通过 `tool_search` 补载。
容量裁剪后仍有可发现工具未暴露时，必须保留 `tool_search`；必需工具与搜索入口
无法同时容纳时明确返回容量错误。搜索、下一轮投影与调用使用同一套 Session Profile
和角色可用性规则，禁用项不参与搜索或物化；资源级审批仍在实际调用时执行。

仓库探索优先用 `file_list` 列目录、`file_read` 读文件，并使用工具返回的路径。
独立探测分别调用，保留各自输出、stderr 和真实退出状态；不要用 `2>/dev/null`
隐藏诊断或用 `|| true` 把所有失败转为成功。确认可选路径不存在时明确报告缺失，
继续其他探测；权限、沙箱拒绝和 I/O 故障仍按真实失败处理。
已读路径不代表所有内容都已覆盖。已有文本能回答当前问题且窗口、版本一致时复用；
否则可为只读分析或修改补读必要窗口。搜索命中只提供起点，不要求读后必须编辑。

`lsp_diagnostics`、`lsp_hover`、`lsp_format_edits`、`lsp_code_actions` 和
`lsp_rename_edits` 按文件类型选择已安装的 `gopls`、`clangd`、
`rust-analyzer`、`pyright-langserver`、`typescript-language-server` 或 `jdtls`。
注册状态来自实际二进制探测；同一次调用不能混用不同 Language Server。
format/code-action/rename 只返回结构化 edits，不直接修改文件，应用 edits 仍通过受
Journal 保护的文件工具完成。

测试、构建和静态检查统一使用 `exec_command`，不再提供独立 quality 工具，也不默认
执行 Go 或其他语言的校验。需要记录验证证据时，声明 `verification`（`test`、`build`、
`lint` 或 `check`）和 Workspace 相对路径 `covered_paths`。例如：

```json
{"command":"npm test","verification":"test","covered_paths":["src/parser.ts"]}
```

验证命令保持 Workspace 只读，构建产物放在 `$TMPDIR`；不能同时声明 `write_paths`。
声明验证的命令使用 POSIX `set -e`，组合检查仍应使用 `&&`，不要用管道截断输出来掩盖
退出码。运行中只记录待结算状态，最终退出由 `exec_command` 或 `write_stdin` 返回；
被终止、超时或输入变更的执行不能记录为验证通过。验证只证明实际命令对声明输入的
执行结果，不自动证明测试充分性。

`format_code` 只格式化显式路径且进入 before-image Journal；
`debug_run` 使用 LLDB 的固定批处理参数，Workspace 保持只读；`dependency_resolve`
以禁用脚本、Workspace 只读的方式解析依赖，并要求显式声明网络目标。

安装 Chromium/Chrome 后，`web_run` 使用隔离临时 Profile 和 CDP 提供真实
navigate、DOM snapshot、click 与 fill；`QCODE_BROWSER_BINARY` 可覆盖自动探测。
本地开发地址必须显式传入 `allow_loopback`，不要把 `localhost` 或端口 `0` 写进
`network_targets`。可信配置里声明的环境网络可被空 `network_targets` 继承；
未声明时仍是禁网。进程启动成功或存活不等于测试通过。`exec_command` 第一次只等到 `yield_time_ms`；
进程还在跑时会返回 `session_id`，用 `write_stdin` 继续收输出或关闭，并可用
`timeout_ms` 杀掉进程组（声明值不得超过一个工作日，24 小时；更长的任务应组织为
显式轮询的会话而不是单一无限期限）。`shell_read` 未显式传 `timeout_ms` 时按
60 秒默认前台期限执行并在超时结果中提示改用 `exec_command` 会话，挂起命令不再
占用整个 Turn。`write_paths` 可以指向已存在的工作区子目录，表示该树内的创建与
修改；这类命令在隔离工作区执行，结果里的 `isolated_cwd` 是真实 cwd，结算时不会
把用户当时改过的其它文件算成 Agent 修改。`http_request` 支持结构化
GET/POST/PUT/PATCH/DELETE/HEAD、响应状态断言和有界 Body；它拒绝
Authorization、Cookie、API Key 等会被持久化进 Tool Call 的敏感 Header。

安装并授权 GitHub CLI 后，`github_pr_list`、`github_pr_view`、
`github_ci_status` 和 `github_pr_create` 提供固定参数的 PR/CI 操作。创建 PR 属于
不可逆外部变更，要求 Plan 和单次审批。GitLab、内部代码托管平台和企业认证流程继续
通过 MCP 或 Skill 提供，不把平台凭据写入通用 Tool 参数。

Composer 下方的 Stats 使用一条可整体省略的摘要展示 Turn、Tool、总耗时、模型耗时、
Tool 耗时、TTFT、Token、Cache 和 Cost；完整明细保留在 Tooltip 中，不逐项压缩。

Plan 模式只允许 Workspace Read 与有界的 Session Plan 状态更新。Agent 调研完成后通过
`submit_plan` 提交带步骤、依赖、预期证据和受影响文件的结构化 JSON 计划。`purpose`
区分两种用途：`execution`（默认）是本次执行计划，`deliverable` 是交付给用户的
未来方案。只要求补充计划或设计方案时，Agent 使用 `deliverable`；Plan 模式也使用
该用途。Plan Artifact 不接受 Markdown 或 XML 标签输出。交付方案显示为
`Proposed plan`，不显示为正在执行的 Tasks。提交计划时会记录受影响文件摘要，
执行前若文件已变化，Runtime 拒绝旧 Revision 并要求重新规划。

Mode 只提供 `plan`、`act`、`operate` 三项。`act` 与 `operate` 固定使用自适应规划：
非高风险且非不可逆的 Workspace 操作直接执行，不按文件数量升级；高风险、不可逆、
网络写、外部写或 Agent 生命周期操作先提交计划。界面不再暴露独立的 Planning
Policy，避免用户同时选择模式和规划策略。

执行 Plan 提交后自动批准并继续当前 Turn；用户无需选择 `Implement` 或 `Autopilot`。
交付 Plan 只保存产物，不授权实施、不覆盖当前执行清单，也不能直接转换为执行。
用户后续要求实施时，Agent 核对当前状态后另行提交 `purpose=execution` 的计划。
执行计划的提交状态只属于
当前 Turn，不写回 Session 默认工具审批姿态。独立 Plan 模式仍使用 Plan 模型路由；
Act 内规划保持 Turn 已冻结的 Act 路由，不在一次回答中途切换模型。新 Session 默认
使用 `approval_posture=auto`。Plan Artifact 以执行配置摘要而不是整个 Session
Profile Revision 判断是否过期；模型、工具集、审批姿态或执行目标等执行配置变化仍会
要求重新规划。

活动 Plan 的状态变化通过 `update_plan` 立即生成新的 `plan.delta`。步骤签名未变的
重写会被拒绝，不产生新的 delta。Runtime 不根据文件写入猜测业务步骤是否完成。
执行 Plan 正文进入 Session State，下一 Turn 仍可 `update_plan` 或按步骤继续实现；
`update_plan` 只接受执行用途。交付 Plan 的未来步骤不进入本次完成门禁，
即使本次写入了计划文档，也可在文档交付与验证完成后正常结束，未来步骤保持 pending。
已有执行清单的未完成任务不能通过提交交付 Plan 清除；发生修改后仍须完成这些任务或声明
`incomplete`，而不是反复改同一份计划。Checkpoint 和 Continue/Retry 只恢复原执行清单
及有效执行授权，不会自动启动交付方案。

创建新 Session 时，Web 会继承当前 Session 的 Approval Posture；因此用户选择 `auto`
后，新建 Session 不会重新回到 `suggest`。显式的新建参数仍优先于继承值。

内置 `deepseek-v4-flash-vision-exp` 模型声明 Image Input 与 Vision 能力，并通过
DeepSeek Responses 协议发送图片。支持图片的 Session 会在模型上下文中明确声明该能力，
避免模型仅凭通用身份说明误判为纯文本环境。实际交给模型的图片同时随
`turn.started` 持久化为用户消息附件，因此发送后、刷新页面或重新进入 Session 时仍可
在对话中查看。

模型推理在 Chat 中显示为可折叠的 `Think` 行。运行时摘要跟随最新内容，每次模型
Sample 完成后持久化完整推理，因此重载页面或切换 Session 后仍可恢复多个独立 Think
段。Read、Bash、Grep/Glob 分别使用带行号的文件面板、Terminal 面板和分组搜索面板；
文件名与搜索结果路径仅作为可选取文本显示，不再调用本机编辑器或 VSCode。
页面内文件预览、内容复制和 Git Diff 保留；目录选择器仍用于添加 Workspace。

失败工具卡片的标题保留命令或路径，展开后分别显示失败摘要和原始输出。
摘要优先使用结构化恢复信息与最后一次执行的拒绝原因；只有退出码时明确显示
退出码，超时和取消按实际状态显示，不从普通输出首行或命令字符串推断原因。
原始输出保留供诊断，真实非零退出状态不会被改写为成功。

复杂任务中，模型可以在常规工具调用前输出简短的阶段说明，报告已确认的发现与下一步。
这些说明由主模型生成，在该次完整响应被接纳后显示，不从推理文本中截取，也不额外调用
摘要模型。说明与工具按顺序穿插，`Stage details` 可折叠相邻执行细节，说明本身保持可见。
第一条阶段说明到达时，已打开的工具详情、焦点与阅读位置保持不变。
工具运行较久或模型尚未完成响应时，继续显示原有运行状态，不虚构阶段结论。

Turn 完成后，Chat 默认只保留用户问题和最终结论；阶段说明、推理、Tool、验证和交付记录
收进可展开的 `Execution details`。最终结论不会替换阶段说明。运行中的 Turn 默认展开。
如果完成、失败或取消时已停止跟随底部，则保留当前执行过程，避免正在阅读的内容突然收起；
回到底部后再恢复默认折叠，重新打开会话时仍按历史 Turn 默认折叠。
通过会话搜索或 Trajectory 定位阶段说明、Tool 或文件时，所属执行过程及分组会自动展开。
重载页面、取消或失败后，已确认的阶段说明仍可恢复；它们不代表任务已完成或验证通过。

最终回答支持 GFM 表格、CJK 相邻强调、行内与块级数学公式、引用、嵌套列表、图片和
带语言标识的代码块。宽表格与长代码只在各自区域滚动；Markdown 文件引用显示为静态
路径标识，Host 不再暴露 `workspace/open` 或外部编辑器能力。同源图片可直接显示，跨域图片必须由用户
显式加载且只允许 HTTPS；图片提供尺寸约束、加载失败、重试和下载动作。

Conversation Header 显示当前用户问题位置，并提供上一个、下一个问题和会话内搜索动作。
搜索面板可按 Turn、问题、阶段说明（`Updates`）、Tool 或文件过滤；命中项使用 Runtime 派生的稳定
Entry、Turn、Call 和 Path Identity 定位。Chat 与 Trajectory 往返、切换 Session、
加载更早历史或展开 Tool 时，页面会保留当前语义阅读锚点。Transcript 使用最多
200 个业务节点的重叠滑动窗口，避免长会话无限扩张 DOM。向上滚动时自动显示或读取
更早历史，向下滚动时自动显示后续消息，不再显示 `Earlier messages` / `Newer messages`
分页按钮。加载期间保留当前可见消息及其视口位置；浏览旧消息时，新输出不会强制拉回底部，
滚动与流式更新同时发生时以最新滚动位置为准。`Back to bottom` 会切回最新窗口并继续跟随输出。

同一 Session 的并发历史读取会合并；切换 Session 或 Workspace 后取消请求并丢弃迟到结果。
历史页未推进 Cursor 时停止加载，避免无限请求。读取失败只展示错误与重试图标，
不会自动反复重试；离开 Chat 或页面进入后台时不触发自动历史加载。

Runtime 连接中断时，页面立即停止当前 Turn 的运行计时和操作控件，显示连接中断提示，
并禁止继续提交。自动重连会重新读取 Runtime 的持久化状态，以确认该 Turn 实际为
继续、完成或失败；Browser 不会自行伪造业务终态。

启用 Subagent 时，终态 `turn.receipt` 会记录冻结的委派模式、Spawn 尝试数、成功数和
观测结果。`delegated` 表示至少成功创建一个 Child，`blocked` 表示 Spawn 均失败，
`retained_parent` 表示 Adaptive Turn 已执行模型采样但未尝试 Spawn，
`not_evaluated` 表示没有足够的执行事实。Trajectory 直接显示该结果；它不推测或伪造
模型未委派的自然语言理由。

Chat 会把每个 Child 的状态、阶段说明、推理摘要、Tool 调用和最终结果聚合为可展开的 Subagent
执行块；运行中或失败的执行块默认展开，完成后可折叠。失败卡展示稳定原因
（如 `budget exhausted`、`provider rate limited`）以及输入/输出 token 用量，避免只
留下 `2 unresolved` 这类摘要。刷新或重连时，这些内容从同一组 Runtime Event 恢复。
限流较频繁的模型上，Parent 与 Child 的采样会排队而不是并行打同一 Provider；若
Child 因 `provider rate limited` 失败且标为 `retryable`，应 `wait_agent` 后再
`followup_task`，不要同时再开一批审查。
Trajectory 继续提供完整时序和 Tool Record 检查入口。
Review 子代理只能使用读文件、搜索和 `shell_read` 这类只读 process；`exec_command`
不会出现在它的工具目录里。需要编译或跑测试时应另开 Verifier，而不是让 Review
去调 Bash。

## Session 与恢复

Browser State 是可丢弃 Projection，不是事实来源。页面先为当前 Workspace 建立
WebSocket，再获取带 `through_sequence` 的 Session Snapshot，并合并水位之后的 Live
Event。每个 Workspace 使用独立 Cursor；刷新、重连或切换 Workspace 不会重新提交
Prompt。

删除 Session 时会要求显式确认。对于已失去执行者的未完成 Turn、Workspace Journal
草稿或隔离 Worktree，确认删除表示同时丢弃其未完成状态并回滚该 Session 留下的
Journal 草稿；仍有内存执行者或恢复中 Operation 的 Session 会拒绝删除，必须先停止
执行。若旧 Session 已被删除但工作区仍锁着孤儿草稿，任意剩余 Session 的
`Continue` 会接管该草稿，`Retry` 会先回滚再开新 Turn。Journal 准入失败的
Turn 即使没有 `turn.started`，这两类恢复仍然有效。

删除成功后，所属 Turn 的状态事实、终态记录和待发布记录会与 Session 一起原子清理，
不会留下失去所属 Turn 的 Kernel 状态。审计事件及其序号仍保留；被过滤的流式事件
对应的 `abandoned` 预留也可能是正常序号记录，不应作为孤儿状态直接删除。

Agent 明确声明任务尚未完成并提供后续动作时，Session 显示为黄色 `Blocked`，保留
Workspace 变更并允许 `Continue`。该状态不同于红色 `Failed`，也不同于用户主动暂停
产生的 `Paused`。Blocked Session 没有活动 Turn 时，Composer 的发送动作显示为
`Continue`，输入内容作为新 Turn 的真实 User Prompt 与 Work Item Goal，并通过
Source Turn 关系绑定到最新可恢复 Turn；模型上下文只注入短胶囊（源 Turn、
terminal、Known/Open、工具结论），不会递归拼接旧输入或把源请求整封当作本轮
Goal。源 Turn 已读路径在开局写入 KnownReads；覆盖范围内的重读回放原结果，
无法回放时放行，git 巡视不再被拒。
恢复请求提交后按钮保持 Pending，直到 Runtime 发布新 Turn 或明确拒绝请求。

### 撤回最近一个 Turn

最近一个 Turn 的用户消息气泡右下方显示垃圾桶按钮 `Withdraw turn`，用于撤回该轮
误发请求。点击后打开与应用其余弹窗一致的居中确认框，不撑开聊天内容，也不依赖浏览器
原生弹窗。支持 Escape、取消与焦点恢复；提交期间显示 `Withdrawing...` 并禁止重复提交，
失败原因显示在确认框中，不会隐藏原 Turn。确认后，运行中的
Turn 及当前会话的活动 Child Thread 先停止并完成结算，Runtime 再恢复该 Turn
开始前的完整模型上下文，包括历史、Plan、摘要和恢复 Checkpoint。撤回期间拒绝
新的执行操作，排队 Prompt 不会自动启动；队列仍可编辑或移除。

撤回成功后，整轮默认收起为 `Turn withdrawn / Excluded from context`，点击可展开审计
记录；通过搜索或 Trajectory 定位该轮时自动展开。不再允许该 Turn 的 Retry、Continue、
Checkpoint Restore/Fork 或 Plan 执行。Composer 恢复普通发送，不会继续误发的请求。
`turn_history` 不再回读被撤回 Turn。只支持最近一个 Turn，不支持删除历史中间一轮；
重复撤回同一轮是幂等操作。升级前未保存启动前基线的旧 Turn 会明确拒绝撤回，
不会通过删文本猜测原始上下文。

撤回不自动回滚文件或外部副作用。已保留的 Journal 草稿按用户确认结算，文件保持
原样，实际修改路径继续以未验证风险进入上下文；费用和 Token 用量也不回退。
撤回记录与当前上下文指针原子持久化，刷新和重启不会重新启用被撤回的请求。

## 配置与凭证

首次进入且尚未完成 Runtime Setup 时，Web 不提供默认 Provider 或 Model。用户必须
选择 OpenAI、DeepSeek、GLM 或自定义 OpenAI-Compatible 服务，并输入准确的
Model ID。自定义 Endpoint 或未进入内置目录的 Model 还必须填写 Base URL（自定义
Provider）、`openai_chat` / `openai_responses` 协议，以及 Canonical ID、Wire ID、
Context、Max Output 和完整 Capability 声明。字段为空或不一致时 Runtime 拒绝构造
Route，不会按 Model 名称或 `/models` 列表猜测能力。Credential Value 只发送到本机
Loopback Runtime，由 Credential Control 写入操作系统 Keyring 加密保存；浏览器不
持久化原始值。

Web Settings 将 Workspace Connection 与 Session 配置分开：Connection 展示固定的
Provider、Endpoint、Protocol 和 Keyring Credential；Models、Reasoning、Mode、
Approval、执行目标和 Tool allowlist 属于当前 Session。Session 配置先进入 Draft，
点击 Apply 后才通过 Runtime `profile/update` 原子生效，并显示具体变更摘要。

每个 Session 独立持久化准确的 Model ID，并可在 Composer 中切换当前连接已验证的
Catalog Model。Composer 的 `New model...` 打开独立模型配置弹窗；探测并确认元数据后，
新模型追加到当前 Connection 的模型注册表，不替换默认模型，也不迁移其他 Session。
同一 Provider、Endpoint、Protocol 和 Credential 下的注册模型可在 Turn 之间热切换；
更换连接仍由 Connection 设置负责。
模型变化会重置该 Session 的 Prompt Cache，Active Turn 期间拒绝修改。Settings 明确
显示 Limits 与 Capabilities 的来源；`Test connection` 检查 Endpoint、Credential 和
启动模型，`Test model` 只检查 Provider 模型目录是否包含 Model ID，不把该结果视为
容量或能力证明。

Composer 内的 Reasoning 菜单直接采用当前模型目录声明的档位。DeepSeek 显示
Off、Low、High、Max，默认 High；GLM-5.3、GLM-5.3-Flash 和 GLM-5.3-FlashX
显示 Low、High、Max，默认 Max；其他模型保留各自完整档位，不做跨档位折算。
三个 GLM 模型均在聊天框的模型菜单中默认列出，使用同一 GLM 连接：
`https://open.bigmodel.cn/api/coding/paas/v4` 的 OpenAI Chat Completions 兼容接口。

Credential 支持创建或轮换、在线校验和二次确认删除。Settings 还可查看 Tool 的
Policy、Constitution 和 Sandbox 信息，以及 Skill 的来源、健康、信任、权限
和 Runtime Control 操作结果。

Agent Preset 保存经过 Runtime 校验的 Session Profile，不包含 Credential。Preset
按 Workspace 隔离并持久化，可创建、更新、复制、删除、载入 Draft，或直接应用到当前
Session；浏览器刷新和 Runtime 重启后仍可恢复。

General Settings 中可选择启用桌面通知。通知默认关闭，并且必须获得浏览器权限；
只报告后台 Session 等待审批、等待输入、阻塞、失败、暂停或完成，不包含 Prompt、Tool 名称
或 Tool Output。点击通知会切换到对应 Session，并定位最新 Turn 或待处理控件。
页面标题和 Session Rail 始终直接投影 Runtime Session 状态，不依赖通知权限。

## 验证

开发时使用：

```bash
make web-check
make web-test
make web-e2e
make docs-check
```

完整门禁使用 `make verify`。
