# 持久盘状态匹配加固：用确定 vmdk 标识替代容量猜盘

本设计针对持久盘观测态迁入 pool status 后的认盘逻辑。背景与验收标准见
[`requirements-and-research.md`](requirements-and-research.md)。
本设计只改「更新 status 时如何把实盘认回声明盘」这一段,不改盘的生命周期语义。

## 结论

**新建持久盘时,由「hostname + primaryIP + 盘名」推导出确定的 vmdk 标识塞给 clone,并把实际路径写进 status;观测时按
完整路径或确定 basename 精确认盘。** 有 effective datastore 时使用完整路径；只有磁盘级 storage policy 时使用
确定 basename，由 vCenter 决定实际 datastore，观测后回填完整 `VolumePath`。status 写失败时，下一轮观测仍可
重新生成 basename 并认回——自愈、幂等。

匹配不使用 SCSI unit 或容量作为身份 fallback。无匹配或 basename 多个匹配时返回 nil，不回填 status，并由
`ValidatePersistentDiskBackfill` 拒绝生成盘表或开机，避免同规格磁盘被误认。

## 问题:为什么必须使用 vmdk 标识匹配

`findPersistentDiskDevice`(`service.go:1334`)只接受两种确定身份：status 中的完整 `VolumePath`，或路径为空时
由 `hostname + primaryIP + diskName` 生成的 vmdk basename。按容量无法区分同规格磁盘，因此当前实现不使用
unit/capacity 身份 fallback，认不出即显式失败。

- clone 前 `resolveEffectiveDatastore`(`clone.go:270`) 只解析一次 effective datastore，`createDataDisks` 与
  `spec.Location.Datastore`、`getDiskLocators` 共用同一结果；
- 新建持久盘由 `createDataDisks` 写入确定的完整路径或 basename，clone 后观测从
  `VirtualDiskFlatVer2BackingInfo.FileName` 取得 vCenter 实际完整路径并回填 status；
- `reconcilePersistentDiskStatuses`(`service.go:1259`) 在 `VolumePath` 为空时生成同一确定 basename，按
  `path.Base(backing.FileName)` 精确匹配；无匹配或多命中都返回 nil，不再按 unit 或容量猜盘。

## 为什么使用确定 vmdk 标识

`unitNumber` 仍用于 guest 盘表和设备配置，但不再作为持久盘身份键。确定名称只依赖 slot 的 hostname、静态
primary IP 和磁盘 name；当 status 写入丢失时，观测端可重新生成同一 basename，直接与 vCenter 返回的
`backing.FileName` 比较，实现幂等自愈。

## 设计

### 建盘(reconcile 第 1 轮,create 路径)

`Clone` 先调用 `resolveEffectiveDatastore` 一次。解析优先级为显式 VM datastore、storage policy 约束下的兼容
datastore、vCenter 默认 datastore；返回值同时包含名称和 `types.ManagedObjectReference`，后续建盘与 clone
定位共用该值。

`createDataDisks` 对 `slotVolumePath == ""` 的新建持久盘：

- 磁盘声明有 datastore，或没有磁盘级 datastore/storage policy 而使用 effective datastore：用
  `DeterministicDiskPath(hostname, primaryIP, diskName)` 生成完整路径，赋给 `backing.FileName`，并写回内存
  `pd.VolumePath`；
- 只有磁盘级 storage policy、没有磁盘 datastore：使用
  `DeterministicDiskName(hostname, primaryIP, diskName)+".vmdk"` 作为 basename，不预设 datastore；clone 后
  观测到的完整 `backing.FileName` 再回填 `VolumePath`；
- 非持久盘仍使用 datastore file hint 并由 vCenter 命名，不进入本设计。

两种持久盘新建分支都保持 `FileOperationCreate`。`slotVolumePath != ""` 时直接使用该完整路径重挂已有 vmdk。

确定路径与「create/attach 判定」是两条独立的键:create/attach 仍只看 `slotVolumePath`(status 观测到的**真实**
路径,新盘为空 → 走 create),我们新赋的 `pd.VolumePath` 是**期望**路径,同一轮内在判定之后写入,不会把新盘
误判成复用而翻成 attach。下一轮 VM 若需重建,`HydrateSlotFromStatus` 会把已落库的路径填回 `slotVolumePath`,
届时才走 attach 复用同一块盘——语义自洽。

### 观测(reconcile 第 2 轮,update 路径)

`HydrateSlotFromStatus`(`machineconfigpool.go:565/571`)把 status 里的 VolumePath / UnitNumber 填回内存。
`reconcilePersistentDiskStatuses` 随后调用 `findPersistentDiskDevice`：

- status 有 VolumePath → **完整 `backing.FileName` 精确命中**；
- VolumePath 为空 → 生成确定 basename，按 `path.Base(backing.FileName)` 精确命中；
- basename 命中多个或没有命中 → 返回 nil，不使用 unit/capacity fallback；
- 命中后回填 vCenter 返回的完整 VolumePath、DiskUUID、UnitNumber，`ApplyDiskBackfill` 将记录标为 `Attached`。

### 闸门改法(`ApplyDiskBackfill` 持久盘分支)

`ApplyDiskBackfill` 只根据观测到的字段记录进度，且不降级已有更高阶段:

- `pd.VolumePath != ""` → 记 `Attached`(含首次观测到 vmdk、复用既有 vmdk 的重挂,以及本设计新增的「建盘即带
  确定路径」)。
- `pd.VolumePath == "" && pd.UnitNumber != nil` → 记 `Creating`，但该阶段不提供认盘 fallback；只有后续观测
  命中确定 basename 或完整路径并回填 VolumePath 后才转为 `Attached`。仅当既有记录为空 / `Creating` /
  `Reclaimed`(墓碑复用)时写入，避免活跃盘降级。
- 两者皆空且无既有记录 → 跳过(无可记)。

`Creating` 阶段早已在类型与 CRD enum 中预留(`vspheremachineconfigpool_types.go:415/469`)，用于表示已分配 unit
但尚未观测到 vmdk；它不参与持久盘身份匹配。`diskObservedEqual`(`1114`)比较 `Phase`，阶段变化时
`UpsertDiskStatus`(`44`)刷新 `LastTransitionTime`，因此 `Creating→Attached` 会自动落库。

### 删除 unit/容量匹配 fallback

`findPersistentDiskDevice` 只保留完整 `VolumePath` 或确定 basename 匹配。SCSI unit、容量、controller 类型都
不参与身份判断；即使只有一个同容量候选，也返回 nil。未能精确认盘时不会回填 status，
`ValidatePersistentDiskBackfill`(`service.go:1284`) 因缺少完整 VolumePath 拒绝生成盘表或开机，把问题显式暴露出来。

## 兼容与迁移

- **CRD 不变**:`Creating` 已在 enum 内；确定路径和实际 vmdk 路径都写入既有 `VolumePath` 字段。
- **存量 / 已建好的盘按实际记录路径匹配**:已观测过的盘 status 里有**真实 VolumePath**(P2-1 已迁入),
  始终以完整路径精确匹配；与「确定名称」推导无关，不改名、不迁移文件。
- **确定路径只是新建盘的创建期动作**:仅在**首次新建**盘时用来命名 vmdk 并落进 status;该盘首次观测后 status
  里就是真实路径,之后与存量盘一样按实际路径认。存量盘沿用 vCenter 已生成的旧路径,不改名、不迁移文件。
- **残留 `Creating`**:若 clone 后尚未观测到 vmdk，记录可暂处于 `Creating`（无 VolumePath）；
  `ValidatePersistentDiskBackfill` 会挡住开机，后续观测按确定 basename 或完整路径命中后转为 `Attached`。
- **condition 无联动**:`PersistentDisksReady` 语义不变;`Creating`、确定路径都只是观测态细化,不改
  v1beta1/v1beta2。

## 确定名字的生成(`DeterministicDiskName`)

vmdk 名由 `DeterministicDiskName(hostname, primaryIP, 盘名)` 幂等算出(同输入恒同输出,故 clone 与观测两处推导一致)。
primaryIP 为空时表示 DHCP，仍作为确定名字输入；IPv6-only 槽位使用 IPv6，双栈时优先使用 IPv4。规则:

- 可读形为 `<hostname>-<primaryIP>-<盘名>`；DHCP 时省略空的 primaryIP，使用 `<hostname>-<盘名>`；非 `[A-Za-z0-9._-]`
  的字符替换为 `-`;
- 若发生替换或长度超过单分量上限(VMFS/NFS 255 字节减去 `.vmdk`),改用 **`<截断前缀>-<全名 hash 前 5 位>`**
  (hash 取原始 `hostname\0primaryIP\0盘名` 的 SHA-256),保证不同输入不塌缩到同名;
- 盘名在 CRD/webhook 无字符校验,故**不拒绝、不静默回退**——总能算出一个合法且唯一的名字。

完整路径为 `[数据存储] <hostname>-<primaryIP>/<上述名字>.vmdk`；`[数据存储]` 前缀由 `pkg/util` 单一函数
生成。`DeterministicDiskPath` 与 `datastoreFileHint` 共用 datastore 前缀约定。

## 已知边界

- **确定完整路径要求有效 datastore**：effective datastore 由 clone 统一解析；只有磁盘级 storage policy 的盘
  只提交确定 basename，实际 datastore 由 vCenter 选择，观测后回填完整路径。
- **同路径重建撞文件**:若上一轮 clone 已建出 vmdk 但未注册成 VM(半失败),下一轮 `findVM` 找不到 VM → 重新
  clone,同确定路径的 `FileOperationCreate` 会撞「文件已存在」而报错。这是**响亮失败**(非静默认错盘),当前
  可接受;后续可在此处改为「探测到同路径已存在则转 attach 或先删孤儿盘」,不在本次范围。
- **非持久盘不走确定路径认盘**:ephemeral 盘恒新建、不记录 VolumePath,靠 clone 分配并落到
  `status.ephemeralDiskStatuses` 的 SCSI unit 跨轮匹配(与持久盘的路径匹配是两条独立机制),详见
  [`design-pool-ephemeral-disks.md`](design-pool-ephemeral-disks.md)。`DeterministicDiskName`
  只服务持久盘的确定路径命名。

## 相关但不在本次范围

- **运行中热加盘未实现**:`createDataDisks` 只在 clone 跑;`ReconcileVM`(`service.go:78`)在 `findVM`
  命中已存在 VM 后跳过整个 create 分支,后续 update 步骤无任何「把新 DataDisk 挂到现有 VM」的逻辑(挂/卸
  只在 `DestroyVM` 的 `detachPersistentDisks` 里做卸载)。故运行中往 pool 加盘只改 `vm.Spec.DataDisks`,
  vCenter 上不会真的多盘;该盘要等机器滚动重建、下一次 clone 才被创建(届时走本设计主路径认回)。这也说明:
  删除 unit/capacity fallback 不影响此场景——盘不存在时任何确定标识都匹配不到,由 `ValidatePersistentDiskBackfill` 拒绝开机;
  且认盘失败不会重建 VM/盘(`findVM` 靠 UID 认 VM,失败只重排队)。
- **unit 分配顺序**仍影响 guest 设备配置，但 unit 不参与持久盘身份匹配；本设计不改变
  `unitNumberAssigner` 的分配策略。

## 代码改动清单

- **`pkg/services/govmomi/vcenter/clone.go`**:新增 `resolveEffectiveDatastore`；`createDataDisks` 对
  `slotVolumePath == ""` 的新建持久盘按 hostname、primary IP、disk name 生成完整路径或 basename，并在
  clone 与建盘间复用同一 effective datastore。
- **`pkg/services/machineconfigpool.go: ApplyDiskBackfill`**:按上「闸门改法」重写持久盘分支的阶段选择与
  写入条件;临时盘分支不动。
- **`pkg/services/govmomi/service.go: findPersistentDiskDevice`**：完整路径精确匹配，或 VolumePath 为空时按
  确定 basename 精确匹配；无匹配或多命中返回 nil，绝不按 unit/capacity fallback。
- 其余链路(`persistMachineConfigSlotBackfill` create 路径调用、`HydrateSlotFromStatus` 回填、
  `reconcilePersistentDiskStatuses` 观测回填)**均已就位,无需改动**。

## 测试

环境验收见 [test-cases.md](test-cases.md) TC-CAPV-ACP-12；单元测试随代码提交。
