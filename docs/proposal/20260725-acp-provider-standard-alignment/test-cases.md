# 测试用例：CAPV 对齐 ACP provider 规范

本文给出本目录各改进项的验收用例。差距分析与改进方案见 [requirements-and-research.md](requirements-and-research.md)，
两项展开设计见 [design-pool-ephemeral-disks.md](design-pool-ephemeral-disks.md) 与
[design-persistent-disk-status-matching.md](design-persistent-disk-status-matching.md)。

第 2 章是真实管理集群 / vCenter 上的验收用例，第 3 章是纯代码级的单测清单（不展开操作步骤）。

## 0. 通用前置

- 管理集群已部署包含对应改进项的 CAPV controller、CRD、webhook 与 RBAC；交付仓库 chart CRD 已同步重新生成。
- vCenter 凭据、datacenter、datastore、网络（PortGroup / dvPortGroup）、VM 模板等基础参数可用。
- 至少一个 `VSphereMachineConfigPool` 可用于固定 IP 路径，槽位数满足用例要求（TC-CAPV-ACP-01/02 需要能占满全部槽位）。
- 测试资源统一使用 `tc-capv-acp-*` 前缀，清理时只删除本测试创建的对象。
- 通用完成标准对所有用例适用，各用例不再重复：`make manifests`/`make generate` 无 diff；`go test -vet=off ./...` 通过；
  新增/变更有单测。

## 1. 用例索引

| TC | 改进项 | 场景 | 类型 |
| --- | :---: | --- | --- |
| TC-CAPV-ACP-01 | P1-1 | Pool status 计数器与 printcolumn | 观测 |
| TC-CAPV-ACP-02 | P1-2 | Pool 级 conditions（v1beta1/v1beta2 双写） | 观测 |
| TC-CAPV-ACP-03 | P1-3 | Pool validation（CRD marker + webhook） | 异常 |
| TC-CAPV-ACP-04 | P1-4 | ACP 文档四件套 + README fork 基线 | 文档评审 |
| TC-CAPV-ACP-05 | P1-5 | 默认固定 IP 模板 | E2E 主路径 |
| TC-CAPV-ACP-06 | P1-6 | 交付仓库去明文凭据 | 交付质量 |
| TC-CAPV-ACP-07 | P1-7 | 槽位非持久盘生命周期 | E2E |
| TC-CAPV-ACP-08 | P1-8 | 容灾 encryption-config 复用 | 部署约定 |
| TC-CAPV-ACP-09 | P1-9 | VM 开机仅在创建时执行一次 | 运维 |
| TC-CAPV-ACP-10 | P1-10 | `/var/log/pods` 软链到 containerd 持久盘 | 节点检查 |
| TC-CAPV-ACP-11 | P2-1 | 持久盘 observed state 迁 status 与存量迁移 | 升级/迁移 |
| TC-CAPV-ACP-12 | P2-1 | 确定 vmdk 路径认盘、删除容量猜盘 | 升级/迁移 |
| TC-CAPV-ACP-13 | P2-3 | `BootstrapReady` condition 与 reason 区分 | 异常 |
| TC-CAPV-ACP-14 | P2-4 | 固定 IP 升级约束（maxSurge/maxUnavailable/replicas） | 异常 + 升级 E2E |

P2-2（govmomi 控制面 LB/VIP）已暂缓，其用例见 [self-built LB 测试用例](../20260810-self-built-lb/test-cases.md)。

## 2. 用例正文

### TC-CAPV-ACP-01：Pool status 计数器与 printcolumn

**目标**：验证 `Total`/`Available`/`Allocated` 由 `configStatuses` 正确汇总，并在 printcolumn 可见。

**前置**：一个含 N 个槽位、全部未占用的 `VSphereMachineConfigPool`。

**步骤**：
1. `kubectl get vspheremachineconfigpool` 查看列。
2. 创建引用该池的 `MachineDeployment`，扩容占用 k 个槽位。
3. 缩容释放槽位，等待盘 reclaim 完成。
4. 直接编辑 pool，增加与删除未分配的槽位条目。

**预期**：
- printcolumn 显示三个计数；`Total` 等于 `spec.configs` 条目数。
- `Allocated` 随机器创建/删除变化，`Available` 与 `configStatuses` 中可分配槽位数一致，三者与 `configStatuses` 始终自洽。
- 增删槽位后计数在下一轮 reconcile 内跟上。

**清理**：删除测试 `MachineDeployment` 与 pool。

### TC-CAPV-ACP-02：Pool 级 conditions

**目标**：验证 Pool 级 condition 齐全、v1beta1 与 v1beta2 双写，且 `SlotAvailable` 不参与 `Ready` 聚合。

**前置**：同 TC-CAPV-ACP-01 的池；能临时摘除 `VSphereMachineConfigPool` 的 validating webhook 配置（用于制造 webhook 之后才能观测到的非法状态）。

**步骤**：
1. 正常池上检查 `status.conditions` 与 `status.v1beta2.conditions`。
2. 把某个槽位的网络名改成不存在的网络，等待 reconcile。
3. 占满全部槽位，观察 `SlotAvailable` 与 `Ready`。
4. 临时摘除 validating webhook，写入与已有槽位重复的 hostname，恢复 webhook 后观察 condition。
5. 制造一块 reclaim 失败的持久盘（如回收期间使数据存储不可写），观察 `PersistentDisksReady`；环境不具备时本步以单测覆盖，不作为阻塞项。

**预期**：
- 步骤 2：`MembersValid=False`、reason `NetworkNotFound`，只有该非法槽位跳过分配，其余槽位仍可分配。
- 步骤 3：`SlotAvailable=False`、reason `AllSlotsInUse`，而 `Ready` 仍为 `True`。
- 步骤 4：`MembersUnique=False`、reason `DuplicateHostname`。
- 步骤 5：`PersistentDisksReady=False`、reason `ReclaimFailed`。
- 各 condition 在 v1beta1 与 v1beta2 两处一致；`Ready` 的 reason 继承首个 False 子条件。

**清理**：恢复 webhook 配置与被改坏的槽位，删除池。

### TC-CAPV-ACP-03：Pool validation

**目标**：验证 CRD marker 与 webhook 的校验分工与拦截面。

**前置**：webhook 已启用。

**步骤**（逐条提交非法对象，记录拒绝方与错误信息）：
1. CRD 层：`configs` 为空数组；`sizeGiB=0`；`unitNumber=-1`、`16`、`7`。
2. webhook 池内唯一：重复 hostname；重复 primary IP/IPv6；重复的数据盘 name / unitNumber / mountPath（持久与非持久盘跨两类去重）。
3. webhook immutable：修改 `status.configStatuses` 中已分配（InUse/Released）槽位的 hostname、IP、盘 sizeGiB、unitNumber；删除该槽位条目；作为对照，修改未分配槽位的同名字段。
4. webhook 跨池唯一：同 namespace 同 `clusterRef` 的另一个池写入重复 hostname/IP。
5. 网络存在性：填一个格式合法但不存在的网络名。

**预期**：
- 步骤 1 由 apiserver（schema 与 CEL）拒绝，错误来自 CRD 而非 webhook。
- 步骤 2、4 由 webhook 拒绝，错误信息带首个冲突方的位置。
- 步骤 3 中已分配槽位的修改与删除被拒绝，未分配槽位的修改通过。
- 步骤 5 写入通过，reconcile 期落 `MembersValid=False`/`NetworkNotFound`（见 TC-CAPV-ACP-02）。
- `make manifests` 后 CRD 含新 marker，且已同步交付仓库 chart。

**清理**：删除测试池。

### TC-CAPV-ACP-04：ACP 文档四件套 + README fork 基线

**目标**：验证文档齐全且与代码一致。

**步骤**：
1. 交付仓库 `docs/` 下检查 `capabilities.md`、`usage.md`、`known-issues.md`、`testing/` 是否齐全。
2. 检查 `capabilities.md` 是否按七段结构（基线、评估说明、提供能力、关键边界、主要缺口、由其他组件承接、代码存在但未验收路径）组织，能力是否标注 supported/experimental/legacy/non-goal。
3. 抽查 3 条能力描述，对照源码与测试报告核实；重点核对固定 IP 为默认主路径、DHCP/IPAM 为 legacy、固定 IP 升级约束、kube-ovn 依赖 annotation `cpaas.io/network-type`、supervisor 模式为未验收路径。
4. 源码仓库 README 或 `docs/development.md` 检查 fork 基线、patch 范围、兼容矩阵。
5. 检查两仓交叉引用可达。

**预期**：四件套齐全；抽查的能力描述与代码、测试报告一致；fork 基线与兼容矩阵在源码仓库可查。

**清理**：无。

### TC-CAPV-ACP-05：默认固定 IP 模板

**目标**：验证 ACP 默认创建路径走固定 IP/hostname 槽位，且升级策略预置正确。

**前置**：可用的 vCenter 环境与规划好的固定 IP 段。

**步骤**：
1. 用默认模板渲染集群 YAML，检查是否含 `VSphereMachineConfigPool` 与 `machineConfigPoolRef`。
2. 检查 KCP 为 `maxSurge=0` 且 `replicas≥3`，MD 为 `maxSurge=0` 且 `maxUnavailable≥1`。
3. 应用 YAML 创建集群，等待控制面与 worker Ready。
4. 核对节点 IP 与 hostname 是否来自槽位声明。
5. 用保留的 DHCP/IPAM 模板另建一个集群。

**预期**：
- 默认路径走固定 IP/hostname 槽位；YAML 与 CRD 一致（apply 无字段被裁剪或校验拒绝）。
- 节点 IP/hostname 与槽位声明逐一对应。
- DHCP/IPAM 模板仍可用，且在 capabilities 中标注为 legacy。

**清理**：删除两个测试集群与池。

### TC-CAPV-ACP-06：交付仓库去明文凭据

**目标**：验证 chart 不再提交明文凭据。

**步骤**：
1. 在交付仓库检查 `chart/values.yaml` 的 `cloudProviderVSphere.config`。
2. 用默认 values 安装 chart，凭据经 Secret 注入。
3. 检查渲染结果与运行中的 CPI 是否读到正确凭据。

**预期**：values 中 server/username/password 为占位符或 Secret 引用，无真实形态凭据；按 Secret 注入后 CPI 正常工作。

**清理**：卸载测试 release。

### TC-CAPV-ACP-07：槽位非持久盘生命周期

**目标**：验证 `ephemeralDisks` 随 VM 创建/删除，按 unit 认盘，不进入 reclaim 与槽位释放门禁。

**前置**：一个槽位同时声明持久盘与非持久盘（不同 name / unitNumber / mountPath），非持久盘声明 `sizeGiB`、`mountPath`、`fsFormat`。

**步骤**：
1. 创建引用该槽位的机器，等待 Ready。
2. 节点上 `lsblk`、`findmnt <mountPath>`、检查 `/etc/fstab` 是否以 `LABEL=` 挂载。
3. 在非持久盘上写一个哨兵文件，重启节点（VM 级 reboot）。
4. 检查 pool `status.ephemeralDiskStatuses` 中该盘的 SCSI unit 是否与 vCenter 实际一致。
5. 删除机器，检查 vCenter 上非持久盘是否随 VM 一并删除、槽位是否直接释放。
6. 触发滚动升级重建同一槽位的机器，检查新盘。

**预期**：
- 非持久盘随 clone 创建、格式化并按 `LABEL=` 挂载到声明的 `mountPath`。
- 步骤 3 重启后哨兵文件仍在（不重刷格式化）。
- 步骤 4 中 unit 与 vCenter 一致，且与持久盘 unit 不冲突。
- 步骤 5 中盘随 VM 删除，不产生 reclaim 记录，槽位释放不被该盘阻塞。
- 步骤 6 重建出的是空盘（哨兵文件消失），持久盘则被认回。
- 存量 pool 无 `ephemeralDisks` 字段时行为不变，`PersistentDisksReady` 语义不变。

**清理**：删除测试机器与池。

### TC-CAPV-ACP-08：容灾 encryption-config 复用

**目标**：验证主备集群通过 KCP YAML 声明复用同一份 encryption-provider 配置，且 CAPV 代码不参与。

**前置**：两套可创建集群的环境，一份共用的 `encryption-provider.conf`。

**步骤**：
1. 主备两集群的 `KubeadmControlPlane` 中以 `files` 写入同一份 `encryption-provider.conf`，并配好 `clusterConfiguration.apiServer` 的 `--encryption-provider-config` extraArgs 与 extraVolumes。
2. 两集群创建完成后，在控制面节点核对文件内容一致。
3. 主集群创建若干 Secret，备份 etcd 并恢复到备集群。
4. 在备集群读取这些 Secret。

**预期**：备集群可解密从主集群恢复的加密资源；`VSphereClusterSpec` 无 `encryptionProviderConfigRef`，CAPV 代码无 encryption 相关处理。

**清理**：删除测试集群。

### TC-CAPV-ACP-09：VM 开机仅在创建时执行一次

**目标**：验证 `InitialPowerOnCompleted` 门闩生效，管理员手动停机不被 controller 重开。

**前置**：一个正常运行的集群。

**步骤**：
1. 新建一台 Machine，等待 VM 开机、Machine Ready，检查 `VSphereVM` 与 `VSphereMachine` 上的 `InitialPowerOnCompleted`。
2. 在 vCenter 手动关闭该 VM，观察至少两个完整 reconcile 周期。
3. 手动开机，观察 Machine 状态。
4. 在 guest 内 `reboot`，观察 condition。
5. 删除该 Machine。
6. 触发一次滚动升级，观察新建 VM 的开机行为。

**预期**：
- 步骤 1：VM 自动开机一次，condition 为 `True`（v1beta1 与 v1beta2 两组），并镜像到 `VSphereMachine`。
- 步骤 2：controller 不重新开机，Machine 如实 NotReady 并 requeue。
- 步骤 3：恢复 Ready。
- 步骤 4：condition 单向不复位，仍为 `True`。
- 步骤 5：`DestroyVM` 照常关机销毁。
- 步骤 6：新 `VSphereVM` 的 condition 未置，首次开机不受影响，扩容/重建路径正常。

**清理**：删除测试 Machine。

### TC-CAPV-ACP-10：`/var/log/pods` 软链到 containerd 持久盘

**目标**：验证声明 `/var/lib/containerd` 数据盘的节点上 `/var/log/pods` 为软链，未声明的节点不受影响。

**前置**：一个槽位声明数据盘 `mountPath: /var/lib/containerd`，另一个槽位不声明。

**步骤**：
1. 两个槽位各创建一台机器，等待 Ready。
2. 声明节点上执行 `findmnt /var/lib/containerd`、`ls -ld /var/log/pods`，检查 kubelet/containerd 状态。
3. 起一个会产生日志的 pod，用 `stat` 比对 pod 日志文件与数据盘的设备号。
4. 未声明节点上检查 `/var/log/pods`。
5. 升级 provider 后观察已运行的旧节点；再触发滚动替换，检查替换出的新节点。

**预期**：
- 声明节点：`/var/lib/containerd` 为数据盘挂载点，`/var/log/pods` 为指向 `/var/lib/containerd` 的软链，kubelet/containerd 正常启动，pod 日志落在数据盘。
- 未声明节点：`/var/log/pods` 是普通目录，不创建软链。
- 存量节点不自动补软链（CAPV 只在首次开机前写 guestinfo）；滚动替换出的新节点补齐。

**清理**：删除测试机器与 pod。

### TC-CAPV-ACP-11：持久盘 observed state 迁 status 与存量迁移

**目标**：验证观测态只写 status、存量对象幂等播种、reclaim 相变与 `Reclaimed` 墓碑收敛。本用例针对 release 1（spec 旧字段冻结但仍在）。

**前置**：一个基线（`dev/v1.13.1`）上创建、spec 已回填 `volumePath`/`diskUUID` 的存量 pool，其中一个槽位处于 mid-reclaim（带 `configStatuses[].reclaimStatus`）；另备一个全新 pool。

**步骤**：
1. 全新 pool 上创建机器，检查观测值写到 `status.persistentDiskStatuses`，`spec.configs[].persistentDisks[]` 的 `volumePath`/`diskUUID` 保持为空。
2. 升级 provider，触发存量 pool 的 reconcile；重复触发若干轮。
3. 删除占用存量槽位的机器，跟踪盘记录的 `Phase` 相变直到回收完成。
4. 回收完成后继续 reconcile 若干轮，并尝试删除该 pool。
5. 让新机器复用刚释放的槽位。
6. 对一台在用机器触发滚动升级重建。

**预期**：
- 步骤 1：controller 不再回写 spec。
- 步骤 2：存量盘幂等播种一条 status 记录（`Phase` 按槽位态取 `Attached`/`Available`，owner 取槽位的 `machineRef`），旧 `reclaimStatus` 折叠进盘记录后清空；重复 reconcile 不重复播种、不覆盖既有记录。
- 步骤 3：`Attached` →（机器删）`Available` → `Reclaiming` → `Reclaimed` 墓碑（记录保留、观测值清空）；失败落 `Error` 并带 `LastError` 与 `RetryAfter`。
- 步骤 4：收敛到 `reclaimed=true`，槽位回到 `Available`，删除 pool 时 finalizer 能摘掉，不出现「重新播种 → 重删盘」的循环。
- 步骤 5：新机器新建盘而非挂载已删 vmdk，backfill 覆盖墓碑。
- 步骤 6：盘被认回重建后的新机器（`OwnerMachineName`/`OwnerMachineUID` 更新），盘未被删除。

**清理**：删除测试 pool 与机器。

### TC-CAPV-ACP-12：确定 vmdk 路径认盘

**目标**：验证新建持久盘按确定路径命名并写进 status，观测按路径精确认盘；容量猜盘（Tier 3）已删除后不出现认错盘或认不出。

**前置**：一个槽位声明两块同规格（同 `sizeGiB`）持久盘，其中一块声明数据存储、另一块不声明；另有 TC-CAPV-ACP-11 用到的存量 pool。

**步骤**：
1. 创建机器，检查 vCenter 上盘文件名与 `status.persistentDiskStatuses[].volumePath`。
2. 检查不声明数据存储的那块盘的记录。
3. 手工把某块新盘的 status `volumePath` 清空，触发下一轮观测。
4. 存量 pool 上观察已有真实路径的盘的认盘路径。
5. 用一个含非法字符或超长的盘名重复步骤 1。

**预期**：
- 步骤 1：盘文件名为 `<hostname>-<盘名>`（或截断加 hash 后缀）形式，路径落在 `[数据存储] <VM 名>/…vmdk`，`Phase` 直接为 `Attached`。
- 步骤 2：该盘走 unit 兜底，记 `Creating`；观测到 `VolumePath` 后翻 `Attached`；clone 始终未出现时停在 `Creating`，开机被 `ValidatePersistentDiskBackfill` 挡住。
- 步骤 3：下一轮按推导路径自愈认回，不需要人工介入。
- 步骤 4：存量盘按记录的真实路径走 Tier 1，不改名、不迁移文件、不进 `Creating`。
- 步骤 5：盘名被规整为合法且唯一的名字，不拒绝、不静默退回 unit。
- 全程无「同规格多盘认错盘」现象。

**清理**：删除测试机器与池。

### TC-CAPV-ACP-13：`BootstrapReady` condition

**目标**：验证 bootstrap 失败的 reason 可区分，且 condition 正确镜像到 `VSphereMachine`。

**前置**：一个可创建机器的集群。

**步骤**：
1. 正常创建一台机器，检查 `VSphereVM` 与 `VSphereMachine` 上的 `BootstrapReady`。
2. 制造 CAPI 未产出 bootstrap secret 的场景。
3. 删除或改名 bootstrap secret，使读取失败。
4. 保留 secret 但删掉 `value` 键。
5. 观察 provisioning 期间 `VSphereMachine` 的 v1beta2 `Ready`。

**预期**：
- 步骤 1：`createVM` 成功后 `BootstrapReady=True`，v1beta1 经 `SetMirror` 链、v1beta2 经专门镜像带到 `VSphereMachine`。
- 步骤 2：reason 仍为 `WaitingForBootstrapData`（落在 `VSphereMachine` 上）。
- 步骤 3：reason 为 `BootstrapSecretGetFailed`，不再落 `CloningFailed`。
- 步骤 4：reason 为 `BootstrapSecretContentInvalid`。
- 步骤 5：condition 缺失时不把 `Ready` 拖成 `Unknown`。
- 边界：写入 guestinfo 失败仍落 `CloningFailed`，按设计暂缓，不作为失败项。

**清理**：恢复 secret，删除测试机器。

### TC-CAPV-ACP-14：固定 IP 升级约束

**目标**：验证 KCP/MD webhook 对升级策略的强制校验，以及满池滚动升级可完成。

**前置**：一个槽位数刚好等于副本数（无空闲槽位）的池，KCP 引用其控制面槽位、MD 引用其 worker 槽位。

**步骤**（逐条提交，记录是否被拒绝与 reason）：
1. KCP：`maxSurge` 不填（默认 1）；`maxSurge=1`；`maxSurge=0` 且 `replicas=1`；`maxSurge=0` 且 `replicas=3`。
2. MD：`maxSurge=1`；`maxSurge=0` 且 `maxUnavailable=0`（CAPI 默认）；`maxSurge=0` 且 `maxUnavailable=1`；策略为 `OnDelete`。
3. 不引用 `machineConfigPoolRef` 的 KCP/MD 提交任意策略作为对照。
4. 用通过校验的配置对满池的 KCP 与 MD 各做一次滚动升级（如改镜像版本）。

**预期**：
- 步骤 1：仅 `maxSurge=0` 且 `replicas≥3` 通过，其余拒绝并给出对应 reason（`maxSurge` 非 0、`replicas<3` 分别可辨）。
- 步骤 2：仅 `maxSurge=0` 且 `maxUnavailable≥1` 通过；`OnDelete` 跳过 surge 校验。
- 步骤 3：不受新约束影响。
- 步骤 4：升级采用先删后建，复用刚释放的槽位，不卡在等待 IP。

**清理**：恢复原策略，删除测试对象。

## 3. 单测清单

以下为纯代码级验收，随实现 PR 一并提交。

**P1-7 槽位非持久盘**

- `pkg/services/govmomi/vcenter/clone_test.go`：非持久盘恒 `FileOperationCreate`、不记录 `VolumePath`、unit 回填；与持久盘 unit 不冲突。
- `pkg/services/machineconfigpool_test.go`：`ApplyDiskBackfill` 落 `ephemeralDiskStatuses`、`HydrateSlotFromStatus` 读回 unit；跨两类的 name/mountPath 唯一性校验。
- `pkg/util/machines_test.go`：盘表含非持久盘行（unit、空 UUID、`wipe=false`）；持久盘与非持久盘混合并盘（`upsertDataDisk`）。
- `DestroyVM` 断言不 detach 非持久盘。

**P2-1 持久盘 status 与确定路径认盘**

- `pkg/services/govmomi/vcenter/clone_test.go`：新建持久盘（有数据存储）的 `backing.FileName` 为确定路径、仍带 `FileOperationCreate`，且 `pd.VolumePath` 被写回；无数据存储时只回填 unit；复用盘（`slotVolumePath != ""`）仍走 attach。
- `pkg/services/machineconfigpool_test.go`：hydrate（overlay）三种情形——status 优先 / spec 显式 `UnitNumber` 保留而其余观测值仍覆盖 / 无记录时 slot 不变；带确定路径的新盘经 `ApplyDiskBackfill` 直接记 `Attached`，仅带 unit 的兜底盘记 `Creating` 并在观测到 `VolumePath` 后翻 `Attached`；`Attached`/`Available`/`Reclaiming` 不被降级；`Reclaimed` 墓碑可被 `Creating` 覆盖；回填只写 status 不写 spec，`Status().Update` 冲突重试。
- `pkg/services/govmomi/service_test.go`：有实际路径记录走 Tier 1；`Creating`+unit 走 Tier 2；首次新建盘两者皆空但能推导路径时按推导路径自愈命中；两级都认不出时返回 nil（含同规格多盘、以及只剩单一同规格候选——Tier 3 已删除，不再按容量猜）。

**P1-10 `/var/log/pods` 软链**

- 覆盖 persistent 与 ephemeral 两类触发、无触发时不生成也不执行软链逻辑、已有目录或已有软链时的幂等处理。

**P1-1/P1-2/P1-3/P2-3/P2-4**

- 计数器汇总函数；Pool 级 condition 的置位与聚合（含 `SlotAvailable` 不参与 `Ready`）；create/update/delete validation；`BootstrapReady` 各 reason 出口；KCP/MD webhook 的各边界。

**命令**：`go test -vet=off ./...`；controllers 包带
`KUBEBUILDER_ASSETS=/home/vscode/.local/share/kubebuilder-envtest/k8s/1.32.0-linux-amd64`。
`apis/v1alpha3` fuzz 转换失败、`pkg/services/govmomi/metadata` 与
`TestUpdateKubeadmNodeRegistrationJoinWithoutKubernetesVersion` 为既有问题，忽略。

## 4. 跑测顺序建议

1. TC-CAPV-ACP-03、TC-CAPV-ACP-14 只需 webhook，先跑，确认拦截面后再建集群。
2. TC-CAPV-ACP-05 建出固定 IP 主路径集群，TC-CAPV-ACP-01、02、07、09、10、13 在该集群上复用。
3. TC-CAPV-ACP-11、TC-CAPV-ACP-12 需要基线 CRD 上创建的存量对象，单独准备环境。
4. TC-CAPV-ACP-04、06、08 与集群生命周期无关，可并行。
