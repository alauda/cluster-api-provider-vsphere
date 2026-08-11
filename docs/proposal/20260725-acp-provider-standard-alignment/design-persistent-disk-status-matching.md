# 持久盘状态匹配加固：用确定 vmdk 路径替代容量猜盘

对 G / P2-1(持久盘观测态迁入 pool status,commit `554f8d152`)的一处修正。背景与验收标准见
[`requirements-and-research.md`](requirements-and-research.md)。
本设计只改「更新 status 时如何把实盘认回声明盘」这一段,不改盘的生命周期语义。

## 结论

**新建持久盘时,由「hostname + 盘名」推导出确定的 vmdk 路径塞给 clone,并把该路径写进 status;观测时按
路径精确认盘(Tier 1)。** 这样每块盘在被观测前,status 里必有 VolumePath,认盘不再落到按容量猜的
Tier 3。路径可由声明数据当场推导,即使 status 写失败,下一轮观测仍能重新推出并认回——自愈、幂等。

未声明数据存储、拼不出路径的盘,退回到「把 clone 分配的 SCSI unit 写进 status(阶段 `Creating`)、观测
按 unit 认(Tier 2)」的兜底路径。**按容量猜盘的 Tier 3 整个删除**:确定路径与 unit 已覆盖所有正常与自愈
情形,走到两级都认不出即为异常,应拒绝开机把问题顶出来,而非再用容量去猜(哪怕只剩一个候选)。

## 问题:容量匹配为何是常态,而非兜底

`findPersistentDiskDevice`(`service.go:1312`)三级匹配:①VolumePath ②SCSI unit ③容量。第三级在
同规格多盘时会认错盘(「不严谨」)。它本应是极端兜底,实际却是**每块自动分配盘首次观测的必经路径**,原因是:

- clone 时 `createDataDisks`(`clone.go:562-571`)给新盘 `assign()` 一个 unit 并写回内存 `pd.UnitNumber`,
  但**不写 VolumePath**(新盘的 `backing.FileName` 只到 `[datastore]` 为止,文件名由 vCenter 事后自动生成,
  clone 提交那刻还不知道最终路径,`clone.go:509→521`);
- `createVM` 返回后 `persistMachineConfigSlotBackfill`(`service.go:177`)调 `ApplyDiskBackfill`,但其
  **闸门 `machineconfigpool.go:1056`「无 VolumePath 且无既有记录就 `continue`」把这个 unit 也一并丢弃**;
- 闸门注释称「未就绪盘由 pool reconciler seed/observe」,但 `SeedPersistentDiskStatuses`(`609`)只迁移
  **已带 VolumePath** 的存量盘,从不预写新盘。于是新盘进入下一轮观测时 status 里既无 vmdk 也无 unit;
- `reconcilePersistentDiskStatuses`(`1237`)此时 `pd.VolumePath==""`、`pd.UnitNumber==nil`,Tier 1、
  Tier 2 全落空,只能落到 Tier 3 按容量猜。

即:路径要等 vCenter 事后生成才知道,unit 虽在 clone 那刻已算好却被闸门扔掉,逼得后一轮靠容量重新猜。

## 为什么选「确定路径」而非只补 unit

两条信息都能让观测避开容量猜,但**可推导性**不同:

- **unit 由 `unitNumberAssigner` 按当时空闲位动态分配,无法从声明数据反推**,因此必须持久化;一旦
  status 写失败,下一轮既拿不到 unit、也没有别的键去映射「声明盘 ↔ 实盘」,又掉回 Tier 3。
- **确定路径可由「hostname + 盘名 + 数据存储」当场推出,不依赖任何已持久化的状态**。status 写没写上都行:
  观测时重新推出期望路径,直接和 VM 实挂盘的 backing 路径比对即可命中。这使认盘对「丢失的 status 写」自愈。

因此以确定路径为主、unit 为兜底(仅用于拼不出路径的盘)。

## 设计

### 建盘(reconcile 第 1 轮,create 路径)

`createDataDisks` 对 `slotVolumePath == ""` 的新建持久盘:

- **能拼出路径时**(该盘有明确数据存储名):用 `deterministicDiskPath(vmName, datastore, diskName)` 推出
  完整路径 `[datastore] <vm目录>/<确定盘名>.vmdk`,赋给 `backing.FileName`(替换现在的 `[datastore]`
  占位),并写回内存 `pd.VolumePath = 该路径`。`FileOperationCreate` 保持不变——仍是新建,只是文件名由我们
  指定而非 vCenter 自动生成。
- **拼不出路径时**(无数据存储名):维持现状 `backing.FileName = [datastore]` / 留空,不写 `pd.VolumePath`,
  只保留 `pd.UnitNumber`(clone 已分配)供兜底。

`persistMachineConfigSlotBackfill` 随后把 `pd.VolumePath`(或兜底的 `pd.UnitNumber`)经 `ApplyDiskBackfill`
落到 status。

确定路径与「create/attach 判定」是两条独立的键:create/attach 仍只看 `slotVolumePath`(status 观测到的**真实**
路径,新盘为空 → 走 create),我们新赋的 `pd.VolumePath` 是**期望**路径,同一轮内在判定之后写入,不会把新盘
误判成复用而翻成 attach。下一轮 VM 若需重建,`HydrateSlotFromStatus` 会把已落库的路径填回 `slotVolumePath`,
届时才走 attach 复用同一块盘——语义自洽。

### 观测(reconcile 第 2 轮,update 路径)

`HydrateSlotFromStatus`(`machineconfigpool.go:565/571`)把 status 里的 VolumePath / UnitNumber 填回内存
(该逻辑已存在)→ `findPersistentDiskDevice`:

- status 有 VolumePath → **Tier 1 按路径命中**;
- 仅有 unit(兜底盘)→ **Tier 2 按 unit 命中**;
- 回填真实 VolumePath / DiskUUID / UnitNumber,`ApplyDiskBackfill` 见 VolumePath 非空 → 记 `Attached`。

status 写失败导致两者皆空时,对**能拼路径的盘**额外补一条自愈:观测端对每块声明盘当场推出期望路径,与实挂盘
backing 路径比对命中——无需任何已持久化状态。两级(含自愈)都认不出,即视为异常,不再有容量兜底兜底(见下)。

### 闸门改法(`ApplyDiskBackfill` 持久盘分支)

将「无 VolumePath 即跳过」改为按进度选择阶段,且不降级已有更高阶段:

- `pd.VolumePath != ""` → 记 `Attached`(含首次观测到 vmdk、复用既有 vmdk 的重挂,以及本设计新增的「建盘即带
  确定路径」)。
- `pd.VolumePath == "" && pd.UnitNumber != nil`(兜底盘)→ 记 `Creating`,**仅当既有记录为空 / `Creating` /
  `Reclaimed`(墓碑复用)时写入**;既有为 `Attached`/`Available`/`Reclaiming` 时跳过,避免把带 backing 的
  活跃盘降级(正常流程中这些阶段的 `pd.VolumePath` 已由 hydrate 填好,不会进本分支,此为防御)。
- 两者皆空且无既有记录 → 跳过(无可记)。

`Creating` 阶段早已在类型与 CRD enum 中预留(`vspheremachineconfigpool_types.go:415/469`),此前只在测试出现、
生产从不写入;本设计把兜底路径接上它。`diskObservedEqual`(`1114`)已比较 `Phase`,`UpsertDiskStatus`(`44`)
已在阶段变化时刷新 `LastTransitionTime`,故 `Creating→Attached` 自动被识别为变更并落库。

### 删除容量匹配(`findPersistentDiskDevice` Tier 3)

**整个删除按容量猜盘的 Tier 3**(现为「收集同容量候选 → 排序取第一个」)。确定路径(Tier 1)与 unit
(Tier 2)已覆盖所有正常与自愈情形;走到两级都认不出,意味着实盘与声明对不上号,这本身就是异常信号——哪怕
只剩一个同规格候选,也不该用容量去猜。改后 `findPersistentDiskDevice` 只保留 Tier 1、Tier 2,认不出即返回
nil;该盘不被回填,`ValidatePersistentDiskBackfill`(`service.go:1284`)因缺 VolumePath 拒绝开机——把问题
顶出来,而非静默挂错盘。

## 兼容与迁移

- **CRD 不变**:`Creating` 已在 enum 内;新增的确定路径只写既有 `VolumePath` 字段。
- **存量 / 已建好的盘按实际记录路径匹配**:已观测过的盘 status 里有**真实 VolumePath**(P2-1 已迁入),
  始终以它走 Tier 1;与「确定路径」推导无关,从不进 `Creating` 或路径推导分支。
- **确定路径只是新建盘的创建期动作**:仅在**首次新建**盘时用来命名 vmdk 并落进 status;该盘首次观测后 status
  里就是真实路径,之后与存量盘一样按实际路径认。存量盘沿用 vCenter 已生成的旧路径,不改名、不迁移文件。
- **残留 `Creating`**:兜底盘若 clone 失败始终没出现,记录停在 `Creating`(无 VolumePath),
  `ValidatePersistentDiskBackfill` 天然挡住开机;重试/重建时被新一轮回填覆盖,无需额外状态机或清理码。
- **condition 无联动**:`PersistentDisksReady` 语义不变;`Creating`、确定路径都只是观测态细化,不改
  v1beta1/v1beta2。

## 确定名字的生成(`DeterministicDiskName`)

vmdk 名由 `DeterministicDiskName(hostname, 盘名)` 幂等算出(同输入恒同输出,故 clone 与观测两处推导一致)。规则:

- 可读形为 `<hostname>-<盘名>`,非 `[A-Za-z0-9._-]` 的字符替换为 `-`;
- 若发生替换或长度超过单分量上限(VMFS/NFS 255 字节减去 `.vmdk`),改用 **`<截断前缀>-<全名 hash 前 5 位>`**
  (hash 取原始 `hostname\0盘名` 的 SHA-256),保证不同输入不塌缩到同名;
- 盘名在 CRD/webhook 无字符校验,故**不拒绝、不静默回退**——总能算出一个合法且唯一的名字(取代早期「不安全就返回
  空、退回 unit」的做法)。

路径为 `[数据存储] <VM 名>/<上述名字>.vmdk`;`[数据存储]` 前缀由 `pkg/util` 单一函数生成,`DeterministicDiskPath`
与 `datastoreFileHint` 共用(避免括号约定两处分叉)。

## 已知边界

- **确定路径要求已知数据存储名**:未声明数据存储的盘拼不出路径,退回 unit 兜底(见「设计」)。
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
  删除 Tier 3 不影响此场景——盘不存在时任何 tier 都匹配不到,由 `ValidatePersistentDiskBackfill` 拒绝开机;
  且认盘失败不会重建 VM/盘(`findVM` 靠 UID 认 VM,失败只重排队)。
- **unit 跨 VM 重建的稳定性**(review 头号问题:`unitNumberAssigner` 先给无号盘 `assign()` 可能抢占后续
  pinned 盘的号,致 `markUsed` 冲突、clone 失败)是另一处缺陷,应「先预留所有 pinned/observed 号、再给无号盘
  分配」。它不影响本设计要修的**首次观测认盘**(同一次创建内无重分配),故单独处理。

## 代码改动清单

- **`pkg/services/govmomi/vcenter/clone.go`**:新增 `deterministicDiskPath(vmName, datastore, diskName)`;在
  `createDataDisks` 对 `slotVolumePath == ""` 且数据存储已知的新建持久盘,设 `backing.FileName` 为确定路径并
  写回 `pd.VolumePath`;数据存储未知时维持现状(只留 unit)。
- **`pkg/services/machineconfigpool.go: ApplyDiskBackfill`**:按上「闸门改法」重写持久盘分支的阶段选择与
  写入条件;临时盘分支不动。
- **`pkg/services/govmomi/service.go: findPersistentDiskDevice`**:**删除 Tier 3(容量匹配)**,只保留
  Tier 1(实际记录路径)与 Tier 2(unit),认不出返回 nil;并对**首次新建、路径写丢**的盘补一条「按推导路径
  认盘」的自愈比对(仅限能拼路径的新盘,存量盘不涉及)。
- 其余链路(`persistMachineConfigSlotBackfill` create 路径调用、`HydrateSlotFromStatus` 回填、
  `reconcilePersistentDiskStatuses` 观测回填)**均已就位,无需改动**。

## 测试

环境验收见 [test-cases.md](test-cases.md) TC-CAPV-ACP-12,单测清单见同文第 3 章。
