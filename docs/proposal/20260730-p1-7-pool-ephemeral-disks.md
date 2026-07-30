# P1-7　Pool 槽位非持久化数据盘（ephemeral disks）实现设计

面向 K / 差距 #15 的落地设计，背景、动机与验收标准见
[`20260725-acp-provider-standard-gap-analysis.md`](20260725-acp-provider-standard-gap-analysis.md)
的 P1-7 节与差距表，此处不重复。

**依赖前提**：持久盘认盘加固（确定 vmdk 路径、删容量猜盘、`DeterministicDiskName` 命名）见
[`20260730-persistent-disk-status-matching.md`](20260730-persistent-disk-status-matching.md)。非持久盘与持久盘
共用建盘（`createDataDisks`）、并盘（`mergeSlot*Disks`）、盘表生成（`GetPersistentDiskCloudConfig`）与 guest 侧
reconcile 脚本，仅在「如何认盘」与「生命周期」两处不同。

## 结论

非持久盘与持久盘共用同一套建盘 / 并盘 / 盘表 / guest 挂载代码，两点不同：

1. **认盘机制不同**：持久盘按记录的 vmdk 真实路径匹配（Tier 1）；非持久盘恒新建、不记录 VolumePath，靠 clone
   分配并落到 `status.ephemeralDiskStatuses` 的 **SCSI unit** 跨 reconcile 匹配。两条机制彼此独立。
2. **生命周期不同**：持久盘销毁 VM 时先 detach 保留、可跨 VM 重挂、参与 reclaim 与释放门禁；非持久盘随 VM 一起
   删除、每次新建空盘、不 reclaim。

## 为什么非持久盘用 unit 而非确定路径

guest 侧定位块设备靠 `/dev/disk/by-uuid/<DiskUUID>` 或 `/dev/disk/by-path/*-scsi-0:0:<unit>:0`（脚本
`find_device_by_uuid` → 回退 `find_device_by_unit`）；vmdk 路径在 guest 里不可见。当前代码未设 `disk.EnableUUID`，
故 guest 实际主要按 **unit** 定位——unit 必须出现在 guest 盘表里。

盘表在 clone 的**下一轮** reconcile 才写入 guest，彼时 clone 在内存里分配的 unit 已丢失。持久盘靠观测记录的真实
vmdk 路径重新认盘、回读 unit；非持久盘无 VolumePath 可认，因此把 clone 分配的 unit 落到
`status.ephemeralDiskStatuses`，下一轮 `HydrateSlotFromStatus` 再读回内存 slot，供盘表生成消费。这是非持久盘**保留
status 的唯一理由**。

（曾评估过「非持久盘也走确定路径、每轮现读 unit、删 `ephemeralDiskStatuses`」的统一方案，因非持久盘无 status 兜底、
遇 datastore cluster/SDRS 动态放置时路径推不出会认不出盘，未采用；当前实现保留 status。）

## 与持久盘的异同

| 维度 | 持久盘 PersistentDisk | 非持久盘 EphemeralDisk |
| --- | --- | --- |
| spec 字段 | name/sizeGiB/datastore/storagePolicy/mountPath/mountOptions/fsFormat/unitNumber/wipeFilesystem | name/sizeGiB/datastore/storagePolicy/mountPath/mountOptions/fsFormat |
| clone backing | 复用记录的 VolumePath 则重挂；否则 `DeterministicDiskPath` 确定路径新建 | **恒新建**：`datastoreFileHint` 数据存储占位、vCenter 命名文件（不记录 VolumePath） |
| 认盘 | 记录的真实 VolumePath（Tier 1）→ unit（Tier 2） | clone 分配、落 status 的 **unit** |
| 观测持久化 | VolumePath/DiskUUID/UnitNumber/Phase 落 `persistentDiskStatuses` | 仅 UnitNumber 落 `ephemeralDiskStatuses`（无 VolumePath/DiskUUID/Phase） |
| VM 销毁 | 先 detach 保留 | 不 detach，随 VM 删 |
| 跨 VM 重建 | 复用同一 vmdk 重挂 | 永远新建空盘 |
| reclaim / 释放门禁 / tombstone | 参与 | 不参与 |
| guest 定位 | DiskUUID 优先、回退 unit | unit（DiskUUID 列留空） |

「新建 vs 重挂」只由 `slotVolumePath`（观测到的真实路径，经 hydrate 填回）决定：持久盘复用时非空→重挂；非持久盘从不
记录→恒空→恒新建。两类天然分流，无需额外开关。

## 数据流

新建：`spec.configs[].ephemeralDisks[]` →（`vimmachine.go: mergeSlotEphemeralDisks` 经 `upsertDataDisk` 并入
`VSphereVM.Spec.DataDisks`）→ `clone.go: createDataDisks` 用 `datastoreFileHint(datastore)` 作 backing、恒
`FileOperationCreate` 新建空盘，`unitNumberAssigner.assign()` 分配 unit 并回填 `ed.UnitNumber`。

观测（clone 后同轮 `ApplyDiskBackfill`）：`ed.UnitNumber` 已知则 upsert 到 `status.ephemeralDiskStatuses`（无
VolumePath 门禁）。

hydrate（后续每轮 `HydrateSlotFromStatus`）：从 `ephemeralDiskStatuses` 读回 unit 到内存 slot 的
`ed.UnitNumber`（作 clone 的 pinned unit 与盘表的 unit 来源）。

盘表（`GetPersistentDiskCloudConfig`）：持久盘、非持久盘归一为同一写循环产出 `/etc/capv/persistent-disks.tsv`；
非持久盘行 DiskUUID 列留空、wipe 恒 false。guest 侧 `blkid` 探测空盘则 `mkfs` 挂载。

删除：`DestroyVM` 只 detach 持久盘，非持久盘不在该列表，随 VM 销毁（零额外删除代码）。

## 代码落点

**API（`apis/v1beta1/vspheremachineconfigpool_types.go`）**
- `MachineConfigSlot.EphemeralDisks []EphemeralDisk`（`+optional`）。
- `EphemeralDisk`：`Name/SizeGiB/Datastore/StoragePolicy/MountPath/MountOptions/FSFormat` 为序列化 spec 字段；
  `UnitNumber *int32`（tag `json:"-"`）作仅内存 overlay，由观测端回填、hydrate 读回。
- `EphemeralDiskStatus{Hostname,Name,UnitNumber}` + `Status.EphemeralDiskStatuses`（不含
  VolumePath/DiskUUID/Phase）。`FindEphemeralDiskStatus` / `UpsertEphemeralDiskStatus` 助手。

**并盘（`pkg/services/vimmachine.go`）**
- `upsertDataDisk(vm, name, sizeGiB)` 被 `mergeSlotPersistentDisks` / `mergeSlotEphemeralDisks` 共用（评审 #5）。

**clone（`pkg/services/govmomi/vcenter/clone.go: createDataDisks`）**
- 按 name 同时匹配 `PersistentDisks` / `EphemeralDisks`（name 全局唯一）。命中非持久盘：`backing.FileName` =
  `datastoreFileHint(slotDatastore)`（经 `util.DatastorePrefix`，评审 #7），恒 `FileOperationCreate`，unit 用
  hydrate 回来的 `ed.UnitNumber`（有则 `markUsed`）或 `assign()`（无则分配后回填 `ed.UnitNumber`）。持久盘
  pinned/observed unit 先 `markUsed`、非持久盘后 `assign`，不冲突。

**观测 / hydrate（`pkg/services/machineconfigpool.go`）**
- `ApplyDiskBackfill`：非持久盘 `ed.UnitNumber != nil` 时 upsert 到 `ephemeralDiskStatuses`（无 VolumePath 门禁）。
- `HydrateSlotFromStatus`：从 `ephemeralDiskStatuses` 读回 unit 到 `ed.UnitNumber`。

**校验（并入 P1-3，`ValidateSlotFields`）**
- name / mountPath 唯一性、sizeGiB≥1 跨持久盘与非持久盘同一命名空间校验。非持久盘无 spec unit，跨两类不冲突由
  `unitNumberAssigner` 保证。

**cloud-init（`pkg/util/machines.go: GetPersistentDiskCloudConfig`）**
- 同时接收两类切片，两类归一为同一写循环（评审 #6）；非持久盘行 DiskUUID 留空、wipe 恒 false。
- `ValidateEphemeralDiskBackfill` 校验非持久盘盘表字段完整（unit 等）。

**CRD 与生成**
- `zz_generated.deepcopy.go` 由 controller-gen 生成。CRD manifest 增 `spec.configs[].ephemeralDisks` 子树（照抄
  `persistentDisks` 删去 volumePath/diskUUID/wipeFilesystem/unitNumber）与 `status.ephemeralDiskStatuses` 子树
  （仅 hostname/name/unitNumber）。交付仓库 chart 的 CRD 同步。

## 已知边界

- **datastore cluster / SDRS**：非持久盘靠 vCenter 命名文件 + unit 认盘，不依赖预测路径，故不受动态放置影响
  （与持久盘的确定路径不同）。
- **命名**：非持久盘不用 `DeterministicDiskName`（该函数只服务持久盘确定路径），vmdk 名由 vCenter 生成。

## 兼容性

`ephemeralDisks` / `ephemeralDiskStatuses` 为新增可选字段，存量 pool 无此字段行为不变，无需迁移。非持久盘不改
condition，`PersistentDisksReady` 语义不变，v1beta1/v1beta2 无联动。

## 测试

- `pkg/services/govmomi/vcenter/clone_test.go`：非持久盘恒 `FileOperationCreate`、不记录 VolumePath、unit 回填；
  与持久盘 unit 不冲突。
- `pkg/services/machineconfigpool_test.go`：`ApplyDiskBackfill` 落 `ephemeralDiskStatuses`、`HydrateSlotFromStatus`
  读回 unit；跨两类 name/mountPath 唯一性校验。
- `pkg/util/machines_test.go`：盘表含非持久盘行（unit、空 UUID、wipe=false）；持久盘 + 非持久盘混合并盘
  （`upsertDataDisk`）。
- `DestroyVM` 断言不 detach 非持久盘。

命令：`go test -vet=off ./...`；controllers 包带
`KUBEBUILDER_ASSETS=/home/vscode/.local/share/kubebuilder-envtest/k8s/1.32.0-linux-amd64`。
`apis/v1alpha3` fuzz 转换失败、`pkg/services/govmomi/metadata` 与
`TestUpdateKubeadmNodeRegistrationJoinWithoutKubernetesVersion` 为既有问题，忽略。
