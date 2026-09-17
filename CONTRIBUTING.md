# 参与 QCode 开发

## 原则

- 保持一套 Runtime 和一条受 Guard 保护的执行路径。
- 当前代码事实优先于历史设计叙事。
- 变更限定在所属 Package。
- 只有已发布契约需要时才增加兼容逻辑。
- 文档、测试、生成文件和运维行为都属于实现的一部分。

## 修改代码前

1. 阅读[架构设计](./docs/zh-CN/architecture.md)。
2. 检查 `git status`，保留无关改动。
3. 找到 Package Test 和所有权边界。
4. 判断是否影响 Protocol、Persistence、Security、Configuration、Release Artifact
   或中文文档。

## 开发流程

```bash
make build
go test ./path/to/package
make docs-check
git diff --check
```

根据影响面扩大测试，详见[本地开发](./docs/zh-CN/development.md)。

## 变更要求

### Runtime 与 Protocol

- 定义 Operation、Event、Cancel、Error 与 Replay 语义。
- 更新 Schema/Golden 和 Web Transport Contract。
- 通过仓库命令重新生成并提交 Artifact。

### Persistence

- 初始化始终等于最新结构。
- 公开 Schema 变更必须使用事务 Migration 与 Migration Test。
- 测试 Reopen、Rollback、Constraint 与 Corruption。

### Security

- 说明攻击者可控输入。
- 保持 Fail-closed 与 Guard 路径。
- 测试 Deny、Malformed Input、Cleanup 与 Redaction。
- 没有证据时不扩大平台支持声明。

### Documentation

- 只维护 `docs/zh-CN` 下的中文产品文档，不创建英文镜像。
- 示例命令需通过 `--help` 核对。
- 删除被替代文档，不保留冲突副本。
- 运行 `make docs-check`。
- 完整填写 PR Documentation Impact 区块并列出文档路径。事实来源变化必须同步更新中文文档，
  或给出具体的 `Documentation-impact: none` 理由。
- 准备 Release 前按本地开发指南执行发布验证。

## Commit 质量

使用描述行为的 Subject，例如：

```text
fix: preserve task lease across worker restart
docs: add provider configuration guide
```

避免只写阶段号，也不要夹带无关重构。生成文件应与生成它的源变更一起提交。

## Review 检查表

- [ ] 行为和失败语义清晰。
- [ ] 所有权边界未破坏。
- [ ] Security Check 无法绕过。
- [ ] Persistence/Protocol Compatibility 是有意设计。
- [ ] 测试与风险匹配，并在支持环境中通过。
- [ ] 已声明文档影响并列出受影响文档路径。
- [ ] 中文文档与代码事实同步。
- [ ] 未提交 Credential、个人路径或无关生成文件。

## 报告环境受限测试

不能隐藏 Full Suite 失败。应说明具体 Package/Test 和环境原因，再单独重跑聚焦测试。
缺少强 Sandbox 等平台限制不代表应用成功，也不必然是应用回归。
