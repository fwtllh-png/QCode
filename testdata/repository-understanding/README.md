# 仓库引用关系回归语料

`cases.json` 的每个 case 包含：

- `name`：稳定的场景标识。
- `files`：仓库相对文件路径到完整源码的映射。
- `expected`：人工标注的跨文件关系集合，每条含 `source`、`destination`、`target`。

只标注跨文件引用候选；不标注纯 import 依赖、文件内使用和不确定的动态调用。
一个目标在同文件出现多次只计一条关系。负例使用空 expected，并安排同名
声明作为干扰；正例应保留应当命中的声明，防止分析器通过不产出任何关系过关。

执行 `make repository-understanding-eval`。新增样例前应独立核对语言作用域和
目标声明，避免仅按当前实现填写答案。详细指标和边界见
[专项评测说明](../../docs/zh-CN/repository-understanding-evaluation.md)。

`tasks.json` 是另一组真实 QCode 源码任务标注，使用源码路径允许列表而非复制
片段。它记录任务描述、预期文件、必要测试、证据锚点和两条固定工具路线。
执行 `make repository-task-eval` 生成观察性报告；评分变化不作为门禁。
修改标注必须说明源码依据，不应仅照抄当前工具结果。标注当前由代理编写，
尚待独立人工复核；文件级命中不表示已完成修改或已运行测试。
