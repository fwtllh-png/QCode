## Summary / 变更摘要

Describe the user-visible or engineering outcome.
说明用户可见结果或工程结果。

## Verification / 验证

List the exact commands run and their results.
列出实际执行的命令和结果。

```text
make build
go test ./path/to/package
make docs-check
git diff --check
```

## Risk / 风险

Describe affected runtime contracts, persistence, security boundaries,
configuration, release artifacts, or documentation.
说明受影响的 Runtime 契约、持久化、安全边界、配置、发布产物或文档。

## Documentation Impact / 文档影响

Use `affected` when source, tests, commands, protocol, configuration, or
observable behavior changes. List every updated document path. Use `none`
only when documentation facts do not change, and provide a concrete rationale.

源码、测试、命令、协议、配置或可观察行为变化时使用 `affected`，并列出所有已更新的
文档路径。只有文档事实没有变化时才使用 `none`，且必须给出具体理由。

```text
Documentation-impact: affected
Documentation-paths: docs/zh-CN/architecture.md, docs/zh-CN/usage.md
Documentation-rationale: N/A
```

## Checklist / 检查清单

- [ ] The change is scoped to the owning package or document.
- [ ] Tests cover success, failure, and relevant security behavior.
- [ ] Documentation impact metadata above is complete and accurate.
- [ ] Chinese documentation matches the current implementation.
- [ ] Generated files were produced with repository commands.
- [ ] No credential, private source, machine path, or unrelated change is included.
- [ ] 变更限定在对应的 Package 或文档范围内。
- [ ] 测试覆盖成功、失败和相关安全行为。
- [ ] 上述文档影响元数据完整且准确。
- [ ] 中文文档已与当前实现同步。
- [ ] 生成文件通过仓库标准命令产生。
- [ ] 未包含凭证、私有源码、本机路径或无关改动。
