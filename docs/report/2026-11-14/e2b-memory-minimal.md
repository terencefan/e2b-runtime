# 2026-11-14 E2B 最小内存保护补丁

> [!NOTE]
> 当前文档类型：调研报告

报告日期：2026-11-14

## 侦察结论

复用社区 hugePageAdmission，不接入此前自写的 memorybudget 模块。社区已有按 RamMb 的混合规格准入、节点锁、在途预留及内核 hugepage used/reserved 计数；仅启用默认开关不足以保证本次不超售要求。

### 可复现方法

阅读 server/hugepage_admission.go、checkpoint_steps.go、prefetch_harvest.go 和 metrics/host.go，比较补丁与当前基线；执行 strict admission 和原有 admission 的 Go race 测试。

## 范围

生产节点目标预算 456 GiB，首期无内存超售。修改现有 orchestrator gate；不新增账本、公共 API 或数据库。生产重启窗口为北京时间 2026-11-15 00:00–01:00，当前没有安排自动执行任务。

## 方法

检查本地 fork 和社区公开源码。源码静态检查与实际部署状态分开记录。

## 事实

- 社区 freeHugePages 使用基础池加 kernel overcommit 额度，读取失败放行；HostMetrics 会返回历史样本。
- checkpoint 与 prefetch 的内部恢复原来调用 hold-only 路径。严格模式改走相同 checked admission。
- 新增 MEMORY_BUDGET_MIB 配置，非零强制启用严格检查，不依赖 LaunchDarkly；生产配置值为 466944。
- 有效容量取预算和基础池的较小值，读取失败拒绝；请求按 hugepage 向上对齐，容量不足使用现有 ResourceExhausted 调度重试路径。
- 旧 memorybudget 两个未接入文件已移除。

## 排除项

不修改 CPU 超分策略、租户配额、SDK、数据库或 API 镜像。节点端作为最终裁决，保留现有调度重试。

## 未确认项

server、metrics、cfg 全量包测试已经通过 `go test -race`，orchestrator 二进制构建通过。构建记录保存在 workspace/runtime/e2b-memory-admission-20261114/build-record.json；该二进制仅用于验证，不是已发布制品。真实 Firecracker 对全量 guest RAM 的内核预留及 balloon/discard 后的预留语义须完成隔离验证后才可上线。生产宿主机尚未配置新变量；制品尚未发布。

## 建议

完成编译及 race 测试，在隔离节点使用小预算验证混合规格、耗尽、销毁回收、暂停恢复、并发和页回收行为。只有配置内存上限在该版本内核预留中始终得到完整覆盖才能放行生产，否则继续补充已有 gate 的计费来源，不引入独立账本。

## 参考资料

- [社区节点准入](https://github.com/e2b-dev/runtime/blob/main/packages/orchestrator/pkg/server/hugepage_admission.go)
- [社区内存采样](https://github.com/e2b-dev/runtime/blob/main/packages/orchestrator/pkg/metrics/host.go)
- [社区调度筛选](https://github.com/e2b-dev/runtime/blob/main/packages/api/internal/orchestrator/placement/memory_compatibility.go)
