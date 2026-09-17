# 代码结构提取

QCode 在现有仓库索引中使用进程内 Go 语法解析器。无需安装 Rust、C 编译器、
语言服务器或额外解析进程，也不会在运行时下载 grammar。

## 语言与结果

| 源码语言 | 语法级提取 |
| --- | --- |
| Go | 函数、泛型方法及接收者、类型、常量、变量 |
| JavaScript、TypeScript | 函数、箭头函数、类、方法、接口、类型别名、模块级变量；包含 JSX/TSX |
| Python | 类、函数、方法、模块级大写常量、文档字符串 |
| Rust | 函数、方法、struct、enum、trait、类型别名、常量与 static |
| Java、C# | 类、接口、方法、构造函数、字段；C# 另含 struct、enum、record |
| C、C++ | 函数定义及声明、类、struct、enum、方法与命名空间容器 |
| Ruby、PHP | 类、方法与函数 |

符号保留名称、类型、容器、起始行、可见性、签名和文档。签名按语法节点的
函数体边界截取，因此跨行参数和参数中的匿名类型不会被当成函数体。
签名、文档和引用数量沿用已有公开配置上限；UTF-8 文本截断不切断字符。
可见性依据声明修饰符和语言命名约定，不代表跨包访问检查结果。
覆盖基础具名声明，不穷举每种语法：解构绑定、匿名类型成员和宏生成声明
暂不展开。

Go、JS/TS、Python、Rust、Java 和 C/C++ 的 import/include 说明符与符号在
同一次语法树遍历中提取，包括跨行导入、别名和 Rust 分组 use。其他语言
暂不建立导入边。文件级标识符计数排除注释和字符串；字符串插值内部的引用
也暂不计入。

## 作用域感知的引用候选

Go、JS/TS（含 JSX/TSX）成功解析后，索引额外保存跨文件引用候选的出现位置。
Go 使用标准库 AST 的局部对象绑定，排除参数、局部变量、字段及方法名，
将未绑定名称限定在同目录同包声明，显式导入别名限定到对应模块。
JS/TS 先收集词法绑定，再识别导入使用，处理函数、块、闭包、参数、解构、
catch、循环与 var 提升；局部遮蔽和未知对象属性不会按名称连到全仓库。

每条出现记录包含原名、目标名、模块、作用域起始字节、UTF-8 半开字节区间
及从 1 开始的行号。`ReferenceRelations` 将记录与源文件 digest 一起返回，
便于核对证据。作用域位置仅在该文件版本内有效，不是跨版本符号 ID。
图按出现次数聚合为 `package_candidate` 或 `import_candidate` 边，已有
Repo Map 排名和影响分析沿用此图。`search_references` 已接入这些出现记录，
无需新增工具或 Web 操作。

成功启用作用域分析的文件，即便没有候选，也不会回退到全仓库同名关联。
其他语言和语法失败文件仍保留原有近似规则。`ReferenceMaxCount` 独立约束
原有不同标识符数量及新增出现记录数量，超出时文件记录
`reference_sites_truncated=true`，结果不应当作完整引用集合。

这些关系仍是候选：模块路径沿用现有索引解析规则，不执行类型检查或构建。
Go 隐式导入名暂按路径末段识别，点导入、构建标签与跨文件类型推断未支持；
JS/TS 不解析配置路径别名、重导出链或动态接收者。默认导入仅关联候选文件，
不保证其导出符号身份。字符串插值、宏与动态调用也可能漏报。
专项回归范围和运行方式见[仓库理解专项评测](./repository-understanding-evaluation.md)。

## 现有工具如何使用

`search_symbol`、`search_definition` 与仓库地图继续使用同一个索引，无需新工具
或新的 Web 操作。每个符号的 `resolution` 说明提取来源：

- `syntax`：启用的 grammar 成功解析完整文件后，从语法树提取。
- `lexical`：语法解析失败、出现错误节点或提前停止时，使用现有语言词法规则。
- `heuristic`：没有启用语法或词法规则的语言，使用通用声明形态。

结果集合报告其中最弱的提取级别。语法错误文件的降级结果仍可能存在词法
误报，不能把它作为语法级证据。`syntax` 不意味着类型检查、宏展开、重载
解析或跨文件语义引用；这些仍由已有 LSP 能力提供。

## 索引、取消与构建

文件读取、忽略规则、安全约束、增量刷新和 SQLite 写入仍由现有索引负责。
索引提取规则版本为 5；派生索引新增引用出现表和文件分析状态列，
旧派生索引按现有 schema/版本检查机制重建，不迁移权威会话数据。单文件重建会重新解析，不保留跨刷新语法树缓存。
索引取消传入解析器取消标志；解析器独占复用，语法树在提取完成后释放。

解析器固定依赖 `github.com/odvcencio/gotreesitter v0.52.0`，使用其内嵌 grammar。
构建保持 `CGO_ENABLED=0`。该依赖是 tree-sitter 风格运行时的 Go 实现，
不是官方 C 运行时；支持范围以仓库多语言测试为准，不把依赖自带的全部
语言视为已经支持。依赖及 grammar 会增加构建产物体积。该依赖的维护集中度和公开 API 范围
曾是选型疑虑；当前通过固定版本、内部适配和本地回归约束接入范围，
升级时需要重跑这些检查，不能仅依据上游版本号替换。

主要验证位于 `internal/platform/symbols/syntax_test.go`、
`internal/persist/repoindex/index_test.go` 和
`internal/adapter/tool/search/symbol_test.go`。

## 查询引用证据

`search_references` 默认使用 `mode=auto`：

- 提供有效的 `path`、`line`、`character` 且有语义 Provider 时，优先查询 LSP。
  Provider 失败后回退索引，并在结果正文和元数据报告 `semantic_fallback`。
- 按名称查询时，Go、JS/TS 的作用域文件返回跨文件候选，可按目标名称或导入
  别名查询。即使没有候选，也不会用该文件的注释、字符串或局部同名变量补齐。
- 未启用作用域分析的文件补充整词文本匹配，结果来源标为 `text`。
- `mode=text` 显式跳过 LSP 和作用域分析，保留原有整词搜索行为；可用于查找
  局部使用、注释和字符串。它不保证名称属于同一个符号。

例如查询 `run`（也能找到 `import {run as launch}` 后的 `launch()`）：

```json
{"name":"run","max_results":20}
```

限定返回路径并包含声明：

```json
{"name":"run","path_prefix":"src/","include_definitions":true,"max_results":20}
```

需要更广的文本匹配时：

```json
{"name":"run","mode":"text","max_results":20}
```

每条结果包含 `file`、`line`、`text`、`source`、`resolution` 与 `relation_type`。
结构化候选另含 `target_file`、`site` 和 `source_digest`；`site` 包含出现名称、
目标名称、模块与 UTF-8 字节范围。读取源码后会校验 digest 和出现范围，
过期证据不返回。LSP 结果只在文件已索引且可读取时附当前源码片段，否则
提供 `snippet_unavailable`；该片段不是 Provider 文档版本一致性的证明。

`source=repoindex_scoped` 表示结构化候选，`text` 表示文本补充，LSP 结果携带
实际 Provider 来源。`relation_type` 区分 `package_candidate`、`import_candidate`、
`declaration`、`text_match` 和 `semantic_reference`。LSP 本身不区分返回位置
是否为声明，因此含声明的 LSP 结果仍标为 `semantic_reference`。

`completeness` 明确报告索引截断、结果截断、引用出现上限和读取跳过原因。
候选/文本查询状态为 `partial`；LSP 为 `provider_reported`，不推定 Provider
已找到全部引用。`total` 是本次实际找到的结果数，不是仓库真实引用总量。
`truncated` 仅表示 `max_results` 截断，不能代替完整性判断。结构化候选优先
占用结果额度；同一出现有多个候选目标时会分别返回。

`path_prefix` 对 LSP 和索引结果均按返回文件路径前缀过滤。名称匹配区分大小写；
不使用 `path` 猜测目标身份，该参数与行列一起用于语义定位。同文件内部的
绑定使用和未知动态关系可能缺失，可用位置查询 LSP 或显式文本模式补查。
文本模式排除声明仍沿用整行规则，因此声明同行的使用可能一并被排除。

## 解释推荐测试的依据

`search_related_tests` 接受改动文件列表，按每个来源独立计算反向依赖闭包：

```json
{"paths":["internal/platform/repowalk/repowalk.go"]}
```

返回的 `coverage` 每项包含 `source`、`tests` 与 `completeness`。每个测试除
原有 `path`、`hops`、`via`、`resolution` 外，还报告 `reason` 与 `chain`。
`chain` 从改动文件向外排列，每步的 `dependent` 依赖 `dependency`；链中的
中间文件让模型可以解释“改动文件 → 使用它的模块 → 推荐测试”。

| reason | 含义 |
| --- | --- |
| `scoped_reference_candidate` | 整条路径由作用域引用候选连接 |
| `dependency_candidate` | 路径包含普通依赖边，不证明具体符号被调用 |
| `name_based_candidate` | 路径包含旧的同名近似关系，可能存在同名误连 |
| `naming_convention` | 仅按语言文件命名约定推荐，不虚构依赖链 |
| `input_is_test` | 输入文件自身就是测试，直接推荐它 |

每步包含 `kind`。有出现记录时，附一个代表性的 `evidence`，其中包括名称、
行号、字节位置、源文件 digest 和候选目标；工具读取当前源码核对后附 `text`，
并将 `evidence_status` 标为 `verified_digest`。文件过期、过大或证据不合法时，
移除该步的出现记录和片段，明确返回对应状态；普通 import/同名边没有出现
证据时标为 `not_recorded`。缺少片段不等于该依赖不存在。

同一路径只返回一条最短链，不枚举全部调用路径。同一对文件的平行边优先采用
作用域引用证据，其余选择保持确定性。多个改动文件分别计算，不能将来源 A
的测试归因于来源 B。闭包中的环不会让改动文件成为自身的间接受影响项。

这里的作用域是文件依赖，正文 `scope=file_dependencies`、
`claim=recommended_tests_not_proven_coverage` 明确限制结论：每步出现可以涉及
不同符号，不能将文件链解读为确定的函数调用链或测试覆盖证明。目录名推断
也可能推荐测试辅助文件。当前尚不接受“只改了文件内某个函数”的精确范围。

每个来源的 `completeness` 报告现有配置的 `max_depth`、`max_results`，以及
深度截断、数量截断、索引截断、仓库引用出现截断和图/引用证据不可用状态。
顶层 `truncated` 表示某个来源的闭包被深度或数量限制截断；`status=partial`
表示图的候选性质及未覆盖的语言语义。即使列表为空，也不能断言不需要测试。
图查询失败时继续提供命名约定推荐，并标明 `graph_unavailable`。

回归包含多来源隔离、两跳证据链、环、深度/数量边界、过期片段拒绝，以及
仅选取 QCode 的 `repowalk.go`、`repowalk_test.go` 和一个无关符号测试文件的
真实源码样例。该小样例验证真实语法与位置，不代表全仓库影响分析准确率。
