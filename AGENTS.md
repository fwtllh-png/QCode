# AGENTS.md

This file is the repository entry point for coding agents.

完整说明见 [docs/zh-CN/agent-guide.md](./docs/zh-CN/agent-guide.md)。

## Objective

Maintain QCode as one local, guarded coding-agent runtime exposed through
Web and shared by the main agent and subagents.

## Read Before Editing

1. `README.md`
2. `docs/zh-CN/architecture.md`
3. the nearest package tests
4. `Makefile`
5. `git status --short`

## Hard Rules

- Hosts submit operations; they do not execute provider/tool/sandbox logic.
- Business loops stay in `internal/runtime/agent`.
- Construction stays in `internal/runtime/app/wire`.
- Consequential tools pass through `internal/adapter/tool/guard`.
- Do not bypass policy, approval, constitution, journal, or sandbox.
- Do not store raw secrets in tracked code, config, fixtures, logs, or docs.
- Never read, print, patch, or force-add `docs/DEEPSEEK-LIVE.zh-CN.md`.
- Do not overwrite unrelated worktree changes.
- Do not add pre-release compatibility migrations without an explicit need.
- Do not introduce undocumented fixed thresholds, model tiers, or heuristic
  constants for context, capacity, latency, or resource decisions. Derive
  defaults from authoritative model/provider capabilities, explicit
  configuration, negotiated protocol limits, and observed runtime state.
  Necessary absolute safety limits must be public contract or configuration
  fields with provenance, validation, documentation, and boundary tests.
- Product documentation is maintained in Chinese only under `docs/zh-CN`.
- Use repository commands for generated protocol and compatibility files.
- Do not reintroduce architecture line-count, fanout, or function-length
  ratchets. Do not compress or split code just to satisfy a size budget.

## Ownership

```text
Web host                   internal/host
protocol/app/agent/wiring  internal/runtime
providers/tools/ecosystem internal/adapter
policy/sandbox            internal/security
tasks/workflows/subagents internal/orchestration
durable state             internal/persist
usage/traces/verification internal/observability
Web client                 web
```

## Standard Validation

```bash
go test ./path/to/package
make docs-check
git diff --check
```

For Web:

```bash
npm --prefix web run check
npm --prefix web test
```

Broaden validation based on risk. Report environment-limited failures rather
than hiding them.
