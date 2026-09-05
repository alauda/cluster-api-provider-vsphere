# Proposal: MachineConfigPool 与 VSphereCluster 删除安全

## 背景介绍

`VSphereMachineConfigPool` 的持久盘在 `VSphereMachine` 删除时会先从 VM 卸载并保留在 vSphere datastore，后续由 pool controller 异步回收。删除 pool 时如果直接移除 finalizer，可能留下未回收的 VMDK；如果磁盘仍被任意 VM 使用，直接删除 VMDK 还会造成数据损坏或影响运行中的 VM。

同时，pool 通过 `spec.clusterRef` 使用 CAPI `Cluster` 对应的 `VSphereCluster` 连接信息执行磁盘回收。若 `VSphereCluster` 在关联 pool 仍存在时先删除，pool 将失去可靠的 vCenter 访问入口，删除流程可能永久卡住或留下磁盘垃圾。

当前实现的相关事实：

- pool controller 在删除路径中发现仍存在的 `VSphereMachine` 时保留 `MachineConfigPoolFinalizer`，并返回阻塞错误（`controllers/vspheremachineconfigpool_controller.go:898-953`）。
- 回收每块持久盘前通过 vCenter 查询附件；只要仍挂载到 VM，就不调用 datastore 删除，并延迟重试（`controllers/vspheremachineconfigpool_controller.go:735-753`）。
- 回收任务完成后才将磁盘标记为已回收，所有槽位处理完成且没有重试等待后才移除 pool finalizer（`controllers/vspheremachineconfigpool_controller.go:917-959`）。
- `VSphereCluster` 删除路径当前在没有 `VSphereMachine` 后继续清理 cluster modules、Secret，并移除 cluster finalizer；该路径尚未把关联 `VSphereMachineConfigPool` 纳入删除阻塞条件（`controllers/vspherecluster_reconciler.go:242-296`）。

## Reviewer Brief

请重点确认以下决策：

1. **pool 删除的安全顺序**：先确认没有活跃机器和磁盘附件，再回收持久盘，最后移除 pool finalizer。
2. **附件检查范围**：按持久盘的实际 `VolumePath` 查询 vCenter 中的所有 VM，不仅检查 pool status 中记录的 owner VM。
3. **cluster 删除依赖**：只要关联的 `VSphereMachineConfigPool` 对象仍存在（包括正在删除的对象），就保留 `VSphereCluster` finalizer。
4. **失败处理**：附件残留、vCenter 不可用或删除任务失败都只能 requeue，不能通过移除 finalizer 或清空磁盘身份字段绕过安全检查。

## 目标与非目标

### 目标

- 防止删除 `VSphereMachineConfigPool` 时遗留持久盘或误删仍在使用的 VMDK。
- 保证 pool 的持久盘回收完成后才移除 `MachineConfigPoolFinalizer`。
- 保证删除 `VSphereCluster` 时，所有引用它的 machine config pool 已经完成删除。
- 让删除中的对象通过 status、condition、event 和日志暴露明确的阻塞原因。
- 保持删除流程幂等，支持 controller 重启、vCenter 异步任务轮询和重复 reconcile。

### 非目标

- 不改变正常创建、扩缩容和滚动升级时的槽位分配策略。
- 不删除仍由 `VSphereMachine` 使用的持久盘，也不强制 detach 其他 VM 的磁盘。
- 不改变 ephemeral disk 随 VM 删除的生命周期。
- 不在本提案中引入新的用户可配置“跳过回收”或“强制删除”开关。

## 总体设计

删除依赖关系如下：

```text
VSphereCluster deletion
  -> wait until related VSphereMachineConfigPools no longer exist
  -> remove VSphereCluster finalizer

VSphereMachineConfigPool deletion
  -> wait until all referenced VSphereMachines are gone
  -> verify every persistent disk is detached from every vCenter VM
  -> reclaim persistent-disk backing (sync start + async task polling)
  -> remove VSphereMachineConfigPool finalizer
```

pool 与 cluster 的关联以 `VSphereMachineConfigPool.spec.clusterRef` 指向的 CAPI `Cluster` 为准；该 `Cluster.spec.infrastructureRef` 必须指向当前 `VSphereCluster`。pool 不通过 ownerReference 代替这一业务关联，避免 ownerReference 的级联删除绕过 controller 的回收顺序。

## 详细设计

### 1. VSphereMachineConfigPool 删除门禁

pool controller 处理带 `deletionTimestamp` 的对象时，按以下顺序处理每个 slot：

1. 读取 `status.configStatuses[].machineRef`。若对应 `VSphereMachine` 仍存在，记录为阻塞对象并保留 finalizer；不开始该 slot 的回收。
2. 若机器对象已不存在，将 `InUse` 槽位转为 `Released`，保留足够的磁盘身份信息继续处理。
3. 对 `Released` 槽位的每个持久盘计算观测到的 `VolumePath`。路径为空表示没有可回收的 vSphere backing，可清理陈旧 status 记录，但不能把未知路径当作“已确认删除”。
4. 对路径非空的持久盘调用 vCenter 附件查询。查询结果必须为空，才允许启动 `DeleteDatastoreFile`。附件列表应覆盖所有 VM，并在 event/log 中包含 `VolumePath`、VM 名称或引用和 disk key 等诊断信息。
5. 删除任务为异步任务时，将任务引用和盘状态写入 `status.persistentDiskStatuses`，下一轮只轮询任务，不重复启动删除。
6. 任务成功或 vCenter 返回文件不存在时，将该盘标记为 `Reclaimed` 墓碑；不得因为一次 API 调用成功就直接移除 pool finalizer，必须继续检查其他 slot 和其他持久盘。
7. 只有以下条件全部满足时才移除 `MachineConfigPoolFinalizer`：
   - 没有仍存在的 `VSphereMachine` 使用任一 slot；
   - 没有持久盘仍附着到任何 vCenter VM；
   - 没有运行中的回收任务；
   - 没有处于重试等待窗口或未处理的回收错误；
   - 所有持久盘 backing 已回收或从未创建；
   - controller 已将相关 slot/status 收敛到可删除状态。

任一检查失败都应返回可重试结果。vCenter 查询失败、datacenter 或凭据无法解析属于未知安全状态，必须阻塞删除，不得乐观地移除 finalizer。

### 2. VSphereCluster 删除门禁

`VSphereCluster` controller 在执行现有 `VSphereMachine`、cluster module 和身份 Secret 清理前，增加关联 pool 检查：

1. 列出与当前 CAPI `Cluster` 同 namespace、且 `spec.clusterRef` 精确匹配该 `Cluster` 的 `VSphereMachineConfigPool`。
2. 只要列表非空，就记录阻塞原因并返回 `RequeueAfter`。对象处于 `deletionTimestamp` 状态也仍算存在，直到 API server 观察到对象已删除。
3. 只有列表为空后，才继续现有 cluster module 和 Secret 清理，并最终移除 `ClusterFinalizer`。
4. pool 列表读取失败时返回错误并保留 cluster finalizer；不能把读取失败解释为“没有 pool”。

当 CAPI `Cluster` 不可用但 VSphereCluster 仍有合法的 `Cluster` owner reference 时，controller 使用 owner reference 的名称和 namespace 继续检查 pool。若对象既没有可读取的 CAPI `Cluster`，也没有可用 owner reference，则无法可靠确定关联范围；此时沿用现有孤立 VSphereCluster 清理行为，不执行 pool 门禁。

### 3. 状态与可观测性

建议沿用现有 pool 条件和 per-disk status，不新增与现有模型重复的字段：

| 场景 | 状态/事件要求 | 删除动作 |
|---|---|---|
| 机器仍存在 | `blocking ... VSphereMachines` 日志和错误 | 保留 pool finalizer，重试 |
| 磁盘仍挂载 | `PersistentDiskStillAttached` warning event，记录附件 | 不启动删除任务，重试 |
| 回收任务进行中 | `Phase=Reclaiming`、`TaskRef` | 轮询任务，保留 finalizer |
| 回收失败 | `Phase=Error`、`LastError`、`RetryAfter` | 按退避时间重试 |
| cluster 仍有 pool | cluster 删除阻塞日志/事件，包含 pool 名称 | 保留 cluster finalizer，重试 |
| 所有依赖完成 | 记录完成日志 | 移除对应 finalizer |

### 4. 幂等性与并发

- 删除检查和 status 更新必须基于最新 resourceVersion；冲突时重新读取对象，不覆盖其他 reconcile 写入的磁盘状态。
- 同一 slot 同时最多存在一个 vCenter 删除任务。任务引用丢失但 backing 仍存在时，必须重新执行附件检查后再启动新任务。
- pool controller 与 VSphereVM controller 可能同时观察到 VM 删除；只有确认 VM 已不存在且磁盘未挂载，pool 才能进入回收阶段。
- cluster controller 的 pool 列表检查与 pool finalizer 删除之间存在时间窗口，因此 cluster controller 每次 reconcile 都必须重新列举，而不能缓存“已为空”的结果。

## 验收标准

1. pool 中存在仍在运行的 `VSphereMachine` 时，删除 pool 会保持 `MachineConfigPoolFinalizer`，并报告阻塞机器。
2. 所有机器对象已删除但某持久盘仍挂载到任意 VM 时，pool 不启动 VMDK 删除任务，至少 30 秒后重试。
3. 所有持久盘均已 detach 时，pool 能逐盘启动并轮询回收任务；任务未完成或失败时 finalizer 保持不变。
4. 所有持久盘回收完成后，pool finalizer 被移除；重复 reconcile 不会重新回收已标记为 `Reclaimed` 的磁盘。
5. 删除 VSphereCluster 时存在关联 pool，cluster finalizer 保持不变，并在 pool 删除完成后才继续 cluster 清理。
6. 关联 pool 列表读取失败时，VSphereCluster 删除被阻塞并自动重试。
7. cluster 删除期间新建或恢复一个关联 pool 时，下一轮 reconcile 重新阻塞 cluster 删除。
8. ephemeral disk 不参与本提案的持久盘回收门禁，但仍随其所属 VM 的既有删除流程处理。

## 参考资料

- `controllers/vspheremachineconfigpool_controller.go:898-959`：pool 删除、机器阻塞和 finalizer 移除。
- `controllers/vspheremachineconfigpool_controller.go:670-783`：持久盘附件检查、异步回收和墓碑状态。
- `controllers/vspherecluster_reconciler.go:223-299`：VSphereCluster 当前删除流程及 finalizer。
- `apis/v1beta1/vspheremachineconfigpool_types.go:75-123`：pool、slot 和持久盘 API 定义。
- `docs/proposal/20260330-machine-config-pool.md`：MachineConfigPool 槽位、滚动升级与持久盘生命周期背景。
- `docs/proposal/20260725-acp-provider-standard-alignment/requirements-and-research.md:42-47,93-103`：现有槽位释放安全和 pool finalizer 约束。
