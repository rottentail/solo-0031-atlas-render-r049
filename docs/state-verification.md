# 状态化模型验证：修订、归档、恢复与冻结

`internal/statematch` 与 `cmd/stateverify` 为场景生命周期提供**状态化模型验证（stateful model-based verification）**：随机生成把旧版本编辑、归档、恢复、冻结、撤回与素材退役穿插在一起的命令序列，同时喂给真实服务（system under test, SUT）和一个独立的参考模型（oracle），每条命令后比对结果分类、实体状态、快照集合、事件流和持久化恢复；发现违规后自动缩减为最短可复现反例。

## 运行

```sh
# 真实实现：必须全部通过
go run ./cmd/stateverify --fault none --iterations 200 --steps 60

# 注入一种规格违规，观察检测与缩减
go run ./cmd/stateverify --fault stale-put
go run ./cmd/stateverify --fault archive-freeze
go run ./cmd/stateverify --fault half-revision
go run ./cmd/stateverify --fault phantom-event
go run ./cmd/stateverify --fault snapshot-drift

# 每条命令后关闭并重开持久存储，验证恢复与启动校验
go run ./cmd/stateverify --fault none --reopen-every-step

# 机器可读输出；退出码 0 表示通过，1 表示发现违规
go run ./cmd/stateverify --fault snapshot-drift --json
```

标志：`--iterations`（序列数）、`--steps`（每条序列命令数）、`--seed`（首个随机种子，同种子可复现）、`--no-shrink`、`--reopen-every-step`、`--workdir`。包级测试用 `go test ./internal/statematch/`。

## 模型预期来自哪里

参考模型是**对照 `PROJECT_SPEC.md` 的文字规格独立重写的**，它不导入 `internal/service`，因此不会复制生产代码的错误。每条规则在 `internal/statematch/model.go` 中标注了出处：

| 规格原文（PROJECT_SPEC.md） | 参考模型行为 | 对应不变量 |
|---|---|---|
| 场景 `active ↔ archived`，每次有效状态转换增加修订；归档后拒绝修订和冻结 | `applyState` 仅在状态真正变化时修订号 +1；`applyPut`/`applyFreeze` 对 archived 场景返回 conflict | `scene-state-agreement` |
| 修订检查与状态写入在同一事务内完成；冲突不能覆盖已提交内容 | 过期 `expectedRevision` 直接返回 conflict，模型不发生任何变化 | `stale-request-rejected` |
| 冻结把场景修订与可用素材字节捕获成快照；冻结后的渲染不读取当前素材记录 | `applyFreeze` 深拷贝文档与素材闭包，之后的编辑/退役无法触达已捕获内容 | `snapshot-immutability` |
| 快照 `active → revoked`，不可恢复 | 撤回只改可变状态，快照记录与摘要保留；重复撤回幂等 | `snapshot-set-agreement` |
| 素材 `available → retired`，相同 ID 不能替换为不同内容 | `applyImport` 对同 ID 异内容返回 conflict，同内容幂等 | `asset-state-agreement` |
| 400 输入不合法、404 实体不存在、409 状态或版本冲突 | 模型三态分类 `invalid/missing/conflict`，与 `model.Error` 的 HTTP 状态一一对应 | `outcome-classification` |
| 事件只记录有效领域动作；事件只保留最近 500 条 | 失败路径不追加事件；比对时两侧都取末尾 500 条 | `event-stream-agreement` / `failure-atomicity` |
| 先写临时文件 fsync 再原子替换，成功后才切换内存状态 | 每条命令（或序列末尾）重开真实 `persist.Store`，重放 `Validate` 并逐字节比对状态摘要 | `persistence-recovery` |

服务生成的快照 ID 与时间戳无法由模型预知，因此模型用**创建序号（ordinal）**寻址快照，执行器通过共享映射把真实 ID（`snap-00000001`…）翻译回序号；事件比对也把快照事件的主体规范化为序号。

## 检查的不变量与违规归因

每条命令执行后，runner（`runner.go`）依次检查：

1. **结果分类** —— 模型预测的 ok/invalid/missing/conflict 与 SUT 实际返回一致，成功路径还比对修订号、场景状态和快照摘要。
2. **失败原子性** —— 任何非 ok 的命令，整份持久状态摘要必须与执行前完全相同：不能推进事件、不能留下一半修订、不能产生多余快照。
3. **场景/素材状态** —— 修订号、生命周期状态、名称、文档摘要逐实体比对。
4. **快照集合与不可变性** —— 快照数量必须一致（归档/冻结被拒绝时不得出现新快照），每条快照的场景、修订、文档、素材闭包、摘要逐字段比对，并用 `SnapshotDigest` 重新认证字节。
5. **事件流** —— 规范化后的事件种类与主体序列必须一致。
6. **持久化恢复** —— 重开存储后状态摘要不变，且启动期 `persist.Validate` 必须接受。

## 故障注入演示

`faults.go` 在不修改生产代码的前提下，包装真实执行器并注入一种缺陷，用来端到端演示"生成失败序列 → 缩减 → 确认"。典型缩减结果：

| 故障 | 模拟的缺陷 | 被违反的规则 | 缩减后反例长度 |
|---|---|---|---|
| `stale-put` | 过期写入仍提交并覆盖新内容 | `stale-request-rejected` | 2 条命令 |
| `archive-freeze` | 归档场景上冻结仍生成快照 | `snapshot-set-agreement` | 5 条命令 |
| `half-revision` | 失败的状态转换留下修订号自增 | `stale-request-rejected`/原子性 | 2 条命令 |
| `phantom-event` | 被拒绝的请求仍发出事件 | `failure-atomicity` | 1 条命令 |
| `snapshot-drift` | 后续编辑改写旧快照内容 | `snapshot-immutability` | 4 条命令 |

## 缩减器

`shrink.go` 在保持**同一条违规规则**可复现的前提下缩减序列，每次重放都使用全新的临时状态目录：

1. 截到失效步骤的有效前缀；
2. 贪心逐条删除（只接受严格变短的候选）；
3. ddmin 分块删除，粒度 2→n 倍增；
4. 参数简化（修订号归 0、文档索引归 0、撤回引用归 1、关闭坏名称标志），每个被接受的修改都严格降低复杂度度量，因此不会循环。

`MaxShrinkReplays`（默认 4000）是硬预算，达到后返回当前最短序列并在结果中标记 `budgetReached`。缩减完成后驱动器会再独立重放一次确认，文本输出同时给出可直接粘贴的复现命令。

## 范围

本验证聚焦场景/快照生命周期与事务边界，不覆盖渲染像素语义、任务队列和 HTTP 协议细节——这些仍由 `scripts/walkthrough.py` 的真实进程工作流覆盖。生成器只使用合法标识符与受约束的小文档池，使拒绝路径由过期修订、生命周期状态和校验失败驱动，而不是被格式噪音稀释。
