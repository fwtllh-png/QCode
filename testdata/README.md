# Test Data

该目录保存 Go 测试直接使用的最小输入数据（例如 `providers/` 下的 HTTP/SSE fixture）。

`benchmarks/` 是 hermetic coding benchmark 任务集，由 `internal/host/intergration_test` 中的
`_test.go` 驱动执行（`make bench`），格式见 [benchmarks/README.md](./benchmarks/README.md)。

`contracts/` 保存本地验证、Web Host 和架构门禁直接消费的机器契约。它不保存一次性阶段
Evidence 或已退休的重构 Baseline。

新增 Fixture 时就近放在对应包的 `testdata/` 或本目录，并在测试中直接引用。
