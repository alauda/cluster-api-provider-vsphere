# CAPV 对齐 ACP Cluster API Provider 开发规范——差距分析与改进方案

> 日期：2026-07-25　分支：`feat/AIT-72806`（HEAD `0f1bb40e4`）
> 参考规范：`cluster-management-docs/docs/cluster-api-provider-development-standard.md`（草案 v0.1）、`provider-boundary.md`
> 参考实现：`ait/cluster-api-provider-dcs`、`ait/cluster-api-provider-hcs`
>
> **两个前提**：
> 1. 本仓库 `github.com/alauda/cluster-api-provider-vsphere` 是开源 CAPV 的源码 fork；ACP 交付产物（Helm chart、`module-plugin.yaml`、`.build/build.yaml`、Dockerfile）单独维护在交付仓库 `gitlab-ce.alauda.cn:ait/cluster-api-provider-vsphere`。交付类差距在交付仓库评估。
> 2. 控制面 LB/VIP：自建 VIP 后续会平台级统一改造，本文不讨论其规范适用性，相关条目统一标注「⏸ 暂缓」。

---

## 一、结论

CAPV 的 CAPI 合同与主模型四件套完整，规范点名的正确性问题（pause/deletion 顺序、地址 ready、kube-ovn 状态、槽位复用）已修复，交付产物已在交付仓库具备。剩余差距集中在五块（推进顺序见第四章）：

1. **MachineConfigPool 补齐**（P1-1/2/3/7）：缺容量计数器，Pool 级 condition 不全，webhook 校验覆盖少，槽位缺非持久化数据盘声明。
2. **ACP 文档与默认固定 IP 模板**（P1-4/5）：capabilities 等四件套缺失，默认模板仍走 DHCP。
3. **交付仓库质量**（P1-6）：values.yaml 含明文凭据。
4. **容灾支持**（P1-8，非规范项）：无 encryption-provider 配置注入，主备集群无法复用同一加密密钥。
5. **开机语义**（P1-9，非规范项）：VM 被 controller 每轮强制开机，管理员无法手动停机维护。

前三块直接对应规范的「质量门禁」与「最小验收清单」；第四、五块是与 DCS 对齐但规范未点名的能力（容灾密钥复用、开机语义）。

---

## 二、差距总览

图例：✅ 符合　🟡 需补强　❌ 缺失　⏸ 暂缓。#1–#6 为已符合项。

| # | 规范要求 | CAPV 现状 | 评级 | 落点 |
|:---:|---|---|:---:|:---:|
| 1 | pause 下仍可清理（deletion 优先） | 四个 reconciler 均 deletion-first（`vspherecluster_reconciler.go:137-143` 等） | ✅ | — |
| 2 | Machine 地址 ready 需真实 IP | `hasMachineIPAddress` 要求至少 1 个非空 IP，DNS 地址不计入（`pkg/services/vimmachine.go:317-343`） | ✅ | — |
| 3 | kube-ovn 生命周期有 condition/event | 独立 `KubeOvnAppReleaseReady` condition + event，等待控制面节点，不掩盖 Ready（`vspherecluster_reconciler.go:363-476`） | ✅ | — |
| 4 | 槽位释放安全（盘未卸载不释放） | 释放前检查已挂载持久盘并 requeue（`vspherevm_controller.go:490-549`）；异步 reclaim 有 `MachineConfigSlotReclaimStatus` 落盘 | ✅ | — |
| 5 | providerID 格式稳定 | `vsphere://<BIOS-UUID>`，写入 spec 保证幂等 | ✅ | — |
| 6 | 交付产物 chart/module-plugin/.build | 交付仓库已具备（CRD ×10、manager+CPI 双镜像），与 DCS/HCS 同构 | ✅ | — |
| 7 | MachineConfigPool status 计数器 total/available/allocated | 仅 per-slot `configStatuses`，无计数器与 printcolumn | 🟡 | P1-1 |
| 8 | Pool 级 conditions + v1beta2 | Pool 仅 `ClusterRefReady`/`VCenterAvailable`；槽位分配信号在 Machine 侧；无 v1beta2 | 🟡 | P1-2 |
| 9 | MachineConfigPool validation（唯一性、immutable、finalizer 阻删等） | webhook 仅校验 hostname 格式、clusterRef 与 network 必填（`internal/webhooks/vspheremachineconfigpool.go`）；finalizer 阻删已在 controller 实现 | 🟡 | P1-3 |
| 10 | ACP 文档 capabilities/usage/known-issues/testing | 源码与交付仓库均缺（交付仓库仅 `integration-prerequisites.md`） | ❌ | P1-4 |
| 11 | README 说明 fork 基线与兼容矩阵 | 仍为上游 README，无 baseline/patch 范围/版本矩阵 | 🟡 | P1-4 |
| 12 | `XxxClusterSpec` 建模 `networkType` 等字段 | 未建模；kube-ovn 适用性靠 CAPI Cluster annotation `cpaas.io/network-type` 判断（`vspherecluster_reconciler.go:365`） | 🟡 | P1-4（文档化） |
| 13 | 固定 IP 为默认交付路径 | 默认模板 `dhcp4: true`，无 `machineConfigPoolRef` | 🟡 | P1-5 |
| 14 | 交付质量（凭据只引用 Secret，不落明文） | `values.yaml` 提交了真实形态凭据（server/username/password） | 🟡 | P1-6 |
| 15 | 数据盘区分 ephemeral/persistent 两类 | Pool 槽位仅有 `persistentDisks`；非持久盘只有上游模板级 `dataDisks`（仅 attach，无格式化挂载，模板内同构），槽位级无法声明 | 🟡 | P1-7 |
| 16 | 容灾主备复用同一 encryption-provider 配置（非规范项，DCS 已支持） | `VSphereClusterSpec` 无 `encryptionProviderConfigRef`；CAPV 全程不涉及 encryption-provider 配置注入，主备集群各自随机生成加密密钥 | ❌ | P1-8 |
| 17 | VM 开机仅在创建时执行一次，允许管理员手动停机维护（非规范项，DCS 已支持） | `reconcilePowerState` 每轮 reconcile 无条件对 `PoweredOff` 的 VM 强制开机（`service.go:438`，`ReconcileVM` 于 `service.go:225` 调用）；管理员手动停机维护会被 controller 重新开机 | 🟡 | P1-9 |
| 18 | 持久盘 observed state 落 status | `volumePath/diskUUID` 回填到 spec（`vspheremachineconfigpool_types.go:148-155`），无 `PersistentDiskStatus` | 🟡 | P2-1 |
| 19 | 控制面 LB/VIP 由 ClusterReconciler 管理 | `VSphereClusterSpec` 无 `controlPlaneLoadBalancer`，VIP 由模板 kube-vip 承担 | ⏸ | P2-2 |
| 20 | bootstrap 状态可诊断 | 仅 secret 未产出场景有准确 reason（`WaitingForBootstrapData`）；secret 读取失败/内容缺失/写入 VM guestinfo 失败均落 `CloningFailed`，与真实 clone 失败不可区分 | 🟡 | P2-3 |
| 21 | 固定 IP 升级默认 `maxSurge: 0` | KCP/MD webhook 只校验 pool 独占引用，不校验升级策略；池满时 `maxSurge≥1` 的升级会卡住 | 🟡 | P2-4 |

---

## 三、改进方案

P1 = 验收/上架必需；P2 = 随产品节奏推进。

### P1-1　MachineConfigPool status 计数器 + printcolumn（#7）

- **设计**：status 增加 `Total/Available/Allocated int32`，每次 reconcile 由 `configStatuses` 汇总；加 printcolumn。
- **参考**：HCS `HCSMachineConfigPoolStatus.{TotalCount,AvailableCount}` 及其 printcolumn。
- **验收**：`kubectl get vspheremachineconfigpool` 显示三个计数；单测覆盖。

### P1-2　Pool 级 conditions + v1beta2（#8）

- **设计**：新增下表 Pool 级 condition，并补 v1beta2 conditions 字段与 getter/setter（对齐本仓库 Cluster/Machine 已有做法）。要点：
  - 校验函数与 P1-3 webhook 共用；`SlotAvailable` 与 P1-1 计数器同源汇总。
  - `MembersValid`/`MembersUnique` 为 False 时仅非法槽位跳过分配，不整池阻塞。
  - 规范名单中的 `ReclaimReady` 并入 `PersistentDisksReady`：reclaim 进度已按槽位落在 `configStatuses[].reclaimStatus`，Pool 级一个盘条件足够。
- **参考**：DCS/HCS 的 Pool 均无 condition（成员校验靠 webhook 拒绝写入），本项无先例可抄；仅 reason 的 UpperCamelCase 命名风格参照两者 Cluster 级 condition（`VpcReady`/`LoadBalancerReady` 等）。
- **验收**：Pool status 可观测到以下 condition（v1beta1 与 v1beta2 双写；已有 `ClusterRefReady`/`VCenterAvailable` 保留）：

  | Condition | True 含义 | 典型 False reason |
  |---|---|---|
  | `Ready` | 健康类条件（`MembersValid`/`MembersUnique`/`PersistentDisksReady`/`ClusterRefReady`/`VCenterAvailable`）的 summary；不含 `SlotAvailable` | 继承首个 False 子条件的 reason |
  | `MembersValid` | 所有槽位字段合法、引用网络存在（存在性在 reconcile 期校验） | `InvalidMemberConfig`、`NetworkNotFound` |
  | `MembersUnique` | hostname/primary IP 池内及同集群跨池唯一 | `DuplicateHostname`、`DuplicateIPAddress` |
  | `SlotAvailable` | 至少 1 个 Available 槽位。容量信号，不参与 `Ready` 聚合——满分配是固定 IP 池的预期状态 | `AllSlotsInUse`、`WaitingForReclaim` |
  | `PersistentDisksReady` | 无持久盘处于 reclaim 失败或挂载残留状态 | `ReclaimFailed`、`DiskStillAttached` |

### P1-3　MachineConfigPool validation 补齐（#9）

- **现状**：webhook 已有 clusterRef 校验（含 consumerRef 已设时 immutable）、hostname 格式、network 必填；删除阻断已由 controller finalizer 实现（有 InUse 槽位时报错阻塞，盘 reclaim 完成后才移除 finalizer），不是缺口。缺：CRD 数值约束、唯一性、已分配槽位 immutable、跨池检查。
- **设计**：
  1. CRD marker：`configs` MinItems=1；`sizeGiB` Minimum=1；`unitNumber` Minimum=0、Maximum=15，排除 7 用 CEL（`+kubebuilder:validation:XValidation:rule="self != 7"`），不进 webhook。
  2. 池内唯一与字段合法（webhook）：hostname、primary IP/IPv6、数据盘（持久与非持久，见 P1-7）name/unitNumber/mountPath 唯一；写法参照 DCS `validateStatic`（`dcsiphostnamepool_webhook.go`，`field.Duplicate` 错误信息带首个冲突方位置）。
  3. immutable（webhook）：由 old 对象 `status.configStatuses` 判定已分配（InUse/Released）槽位，禁止修改其 hostname/IP/盘 size/unitNumber、禁止删除该槽位条目；参照 DCS `indexPool` 的 old/new 索引对比。
  4. 跨池唯一（webhook 注入 client）：同 namespace 同 clusterRef 的池间 hostname/IP 重复检查；并发写入存在竞态，由 P1-2 `MembersUnique` condition 兜底。
  5. 校验函数放公共包，webhook 与 reconciler（P1-2）共用，避免逻辑漂移。网络名称只做格式校验，存在性在 reconcile 期校验。
  6. 可选增强：`ValidateDelete` fail-fast（当场拒绝删除有已分配槽位的池，而非删除后卡在 deleting）；DCS/HCS 均未做，非规范差距。
- **验收**：create/update/delete validation 单测齐全；`make manifests` 后 CRD 含新 marker 并同步交付仓库 chart。

### P1-4　ACP 文档四件套 + README fork 基线（#10、#11、#12）

- **放置**（需 owner 确认）：capabilities/usage/known-issues/testing 放交付仓库（跟随交付版本）；fork 基线、patch 范围、兼容矩阵放源码仓库 README 或 `docs/development.md`；两仓交叉引用。
- **设计**：
  - `capabilities.md` 按 HCS 七段结构：基线、评估说明、提供能力、关键边界、主要缺口、由其他组件承接、代码存在但未验收路径。需写明：固定 IP 是默认主路径而 DHCP/IPAM 是 legacy、固定 IP 升级约束（`maxSurge=0` / MD `maxUnavailable≥1` / KCP `replicas≥3`，见 P2-4）、kube-ovn 依赖 annotation `cpaas.io/network-type` 的平台约定、supervisor 模式与历史 API 版本为未验收路径。
  - `usage.md`：固定 IP 建集群（单控制面/HA/worker）、MachineConfigPool、持久盘、多网卡、删除与 reclaim。
  - `known-issues.md`：按 HCS `KI-xxx-NNN` 格式。
  - `testing/`：测试计划、报告模板、正式版本报告。
- **验收**：四件套齐全；capabilities 与代码、测试报告一致；能力标注 supported/experimental/legacy/non-goal。

### P1-5　默认固定 IP 模板（#13）

- **设计**：ACP 默认模板改为 `VSphereMachineConfigPool` + `machineConfigPoolRef`,并按 P2-4 约束配好升级策略（KCP `maxSurge=0` 且 `replicas≥3`；MD `maxSurge=0` 且 `maxUnavailable≥1`）；DHCP/IPAM 模板保留并在 capabilities 标注 legacy。
- **验收**：默认创建路径走固定 IP/hostname 槽位；样例与 CRD 一致。

### P1-6　交付仓库去明文凭据（#6、#14）

chart 已具备，唯一质量差距：`values.yaml` 提交了真实形态凭据（`cloudProviderVSphere.config` 的 server/username/password）。改为占位符或安装时注入，凭据只引用 Secret。

- **验收**：values 无明文凭据。

### P1-7　Pool 槽位非持久化数据盘（#15）

- **现状**：Pool 槽位无法声明非持久盘（DCS/HCS 同样如此）。上游仅有模板级 `dataDisks`（`apis/v1beta1/types.go:205-210`），但只 attach 不格式化挂载、模板内所有节点同构，表达不了每节点不同大小/挂载点、随 VM 删除重建的盘。
- **设计**：Pool 槽位 `configs[]` 增加非持久盘声明（如 `ephemeralDisks`，字段对齐 `PersistentDisk` 的 sizeGiB/datastore/unitNumber/mountPath/fsFormat，但无 reclaim/保留语义）。上游模板级 `dataDisks` 保持不动，分工写入 capabilities：模板级用于同构 attach-only 盘，槽位级用于差异化且需格式化挂载的盘。
  - **生命周期**：随 clone 创建（同 `createDataDisks` 路径），随 VM 销毁一并删除；不进入槽位释放门禁与 reclaim。
  - **定位**：按 unitNumber 寻址（`/dev/disk/by-path/…-scsi-0:0:<unitNumber>:0`）。不能沿用持久盘的 diskUUID 键——guestinfo bootstrap 数据在 createVM 时一次性写入，彼时盘尚未创建、无 UUID 可用。
  - **格式化**：幂等模式（`blkid` 探测，空盘才 `mkfs`，参照 DCS `pkg/ignition/clc/clc.go` 的 ignition 模板）。新 VM 的 ephemeral 盘必为空盘，自然全新格式化；同一 VM 重启不重刷。
  - **挂载**：`mkfs` 时打 label，fstab 以 `LABEL=` 挂载（参照 HCS `hcsmachine_userdata.go` 对临时盘的处理），不受设备名漂移影响。
  - **管线**：复用持久盘现有脚本框架、systemd unit 与 cloud-init 注入（`pkg/util/machines.go`），盘表区分持久/非持久两类。
- **验收**：槽位声明的非持久盘随 VM 创建/删除；升级重建后按声明重建空盘；unitNumber 与持久盘不冲突（validation 并入 P1-3）。

### P1-8　容灾 encryptionProviderConfigRef（#16）

- **背景**：容灾主备切换时，备集群从主集群恢复 etcd 数据；若两侧 kube-apiserver 的 encryption-provider 加密密钥不同，备集群无法解密已加密的 Secret 等资源。需让主备复用同一份 `encryption-provider.conf`。
- **现状**：CAPV 源码无任何 encryption 相关处理，加密密钥由各集群 bootstrap 各自生成，主备互不相同。
- **DCS 做法（参考）**：`DCSClusterSpec.EncryptionProviderConfigRef *corev1.LocalObjectReference` 引用一个含 `encryption-provider.conf` 键的 Secret；设值时 controller 用它替代随机生成，并在控制面节点 bootstrap 时注入 `/etc/kubernetes/encryption-provider.conf`。`resolveEncryptionProviderConf` 仅对控制面 Machine 生效（`internal/controller/dcsmachine_vm.go:81`）：未设值时首个控制面节点新生成、后续节点从已有 apiserver 读取。
- **CAPV 落点差异（需 owner 确认）**：CAPV 走 kubeadm bootstrap（CABPK），投递路径与 DCS 的自渲染 ignition 不同——可不新增 `VSphereClusterSpec` 字段，直接由 `KubeadmControlPlane` 的 `files` + `clusterConfiguration.apiServer`（extraArgs `--encryption-provider-config`、extraVolumes）把引用 Secret 的内容写入控制面节点；是否仍在 `VSphereClusterSpec` 建模 `encryptionProviderConfigRef` 由 provider 统一注入（与 DCS 对齐），需 owner 在「复用 kubeadm 既有能力」与「provider 统一建模」间取舍。
- **验收**：主备两集群 kube-apiserver 使用同一 encryption-provider 配置；备集群可解密从主集群恢复的加密资源。

### P1-9　VM 开机仅在创建时执行一次（#17，非规范项）

- **背景**：管理员需要能手动停机某台节点做维护（如硬件排障、离线操作），停机期间 controller 不应把它重新拉起。共识是：provider 只负责把 VM「创建后开一次机」，之后的开关机由管理员掌控。
- **现状**：`reconcilePowerState`（`pkg/services/govmomi/service.go:438`）在 `ReconcileVM` 每轮 reconcile 被无条件调用（`service.go:225`），凡发现 VM 处于 `PoweredOff` 即强制开机。VSphereVM 会周期性 reconcile，因此管理员手动停机的 VM 会被 controller 重新开机，无法维护。
- **DCS 做法（参考）**：仅在 `createVm` 时随创建开机（autoBoot / 平台 start-on-create，`internal/controller/dcsmachine_vm.go:49-78`）；之后 reconcile 遇到已存在且 `Stopped` 的 VM，只把 Machine 标记 NotReady 并 requeue（`dcsmachine_controller.go:227-230`），不强制重开，如实反映停机状态。
- **设计**：门闩放在 `VSphereVM`（每台 infra machine 自己的对象），用一个新 condition 固化「已完成首次开机」——放 Cluster 级会导致第一台开机后新增节点被判定「已开机」而永不开机，扩容坏掉。
  - 新增 `InitialPowerOnCompleted` condition（v1beta1 与 v1beta2 两组常量，加 `condition_consts.go`）。
  - `reconcilePowerState`（`pkg/services/govmomi/service.go:438`）改为先看该 condition：未置且 `PoweredOff` → 照常开机（现有逻辑）；首次观测到 `PoweredOn` 时置为 True。
  - condition 已 True 后再遇 `PoweredOff`，不再强制开机，如实反映 NotReady 并 requeue，由管理员决定何时开机。
  - 经既有 `SetMirror` 链（`pkg/services/vimmachine.go:118-121`）镜像到 `VSphereMachine`，用户在 Machine 侧可见。
  - 区分两条路径：删除走 `DestroyVM` 照常关机销毁；升级重建的是新 `VSphereVM`，condition 未置，首次开机不受影响。
- **边界**：控制面/worker 一致适用；condition 为单向门闩，只置真不复位（VM 重启不影响判定）；非规范项，规范未点名，但已达成共识。
- **验收**：新建 Machine 时 VM 正常开机并 Ready；之后管理员手动停机，controller 不再自动重开，Machine 状态如实反映停机；删除与升级重建路径不受影响。

### P2-1　持久盘 observed state 迁到 status（#18）

- **设计**：新增 `status.persistentDiskStatus`（绑定 hostname/slot，含 volumePath/diskUUID、phase、owner machine、attached VM、lastError 等），controller 不再回写 spec；现有 `MachineConfigSlotReclaimStatus` 并入其 phase 子状态。
- **参考**：DCS/HCS 的 `PersistentDiskStatus` 均已在 status，字段结构可参照。
- **存量迁移**：分两个 release——先加 status 字段并保留 spec 旧字段，controller 每轮幂等 backfill（status 缺记录时用 spec 值播种），下一版本再从 CRD 删除 spec 字段。不可同版本删除，否则 pruning 会在 backfill 前裁掉 `volumePath/diskUUID`。兜底：数据在 `.vmdk` 上，指针可按 unitNumber 从 vCenter 重发现。
- **验收**：spec 只含用户声明字段；存量对象 backfill 后 status 完整；attach/detach/reclaim 单测。

### P2-2　govmomi 控制面 LB/VIP（#19）——⏸ 暂缓

自建 VIP 后续平台级统一改造，本项不单独推进，待统一方案评审。现状留档：`VSphereClusterSpec` 只有 `controlPlaneEndpoint`；govmomi 模式 VIP 由模板中的 kube-vip static pod 承担；supervisor 模式已有 `LoadBalancerReady`。

### P2-3　BootstrapReady condition（#20）

- **背景**：CAPV 交付 bootstrap 数据的方式是——clone 虚拟机时把 CABPK 生成的 cloud-init 用户数据写进 VM 的 guestinfo 属性（`guestinfo.userdata` 等），VM 首次开机由 cloud-init 从 guestinfo 读取执行。下文「写入 guestinfo 失败」即这一步失败。
- **现状**：bootstrap 异常不会丢，但 reason 错位。四类失败场景中仅「CAPI 未产出 secret」有准确 reason，其余三类均落 `CloningFailed`，与真实 clone 失败不可区分，真实原因只在 message 里（v1beta2 侧更粗，后三类统一为 `NotProvisioned`）。
- **设计**：新增 `BootstrapReady` condition（v1beta1 与 v1beta2 两组常量，加 `condition_consts.go`），在各场景的错误出口设值，reason 与场景一一对应：

  | 场景 | 现状 reason | 改后 reason | 检测位置 |
  |---|---|---|---|
  | CAPI 未产出 secret | `WaitingForBootstrapData`（准确） | 沿用现名 | `vspheremachine_controller.go:557` |
  | secret 读取失败 | `CloningFailed`（实际发生在 clone 前） | `BootstrapSecretGetFailed` | `getBootstrapData`（`service.go:135`） |
  | secret `value` 键缺失 | 同上 | `BootstrapSecretContentInvalid` | 同上 |
  | 写入 guestinfo 失败 | `CloningFailed`（与真 clone 失败同出口） | 暂缓，仍 `CloningFailed`（见边界） | clone 出口（`service.go:159`） |

  - **落点**：condition 定义在 `VSphereVM`（`BootstrapRef` 在其 spec 上），且在 `createVM` 成功后置 `BootstrapReady=True`。镜像到 `VSphereMachine`：v1beta1 经既有 `SetMirror` 链（`vimmachine.go:120`，镜像 `VSphereVM` 的 `Ready`，而 `Ready` 已聚合 `BootstrapReady`）自动带到；v1beta2 需专门的按类型镜像 `reconcileBootstrapReadyCondition`（`vimmachine.go`，仿 `reconcilePoweredOnCondition`）。「未产出」场景发生在 VSphereVM 创建前，直接落 `VSphereMachine`。用户统一在 `VSphereMachine` 上查看。
  - **Ready 汇总**：两侧 `Ready` summary 名单加入 `BootstrapReady`；v1beta2 侧列入 `IgnoreTypesIfMissing`（该条件仅在 VSphereVM 上报后才出现，缺失时忽略，避免 provisioning 期间把 `Ready` 拖成 `Unknown`）。v1beta1 `SetSummary` 本就只聚合已存在的条件，无需额外处理。
  - **边界**：guestinfo 写入不是独立步骤，bootstrap 数据作为 clone 请求的参数随 `createVM` 一次性提交，与真实 clone 失败共用一个出口，无法干净区分，故本次暂缓——`createVM` 失败仍落 `CloningFailed`，只拆分 clone 前的 secret 读取失败与内容缺失两类。规范的投递 hash 要求对 CAPV 大部分不适用，checksum 仅作可选诊断信息。
- **验收**：secret 读取失败与内容缺失两类在 `VSphereMachine` 上有可区分的 reason（不再落 `CloningFailed`）；guestinfo 写入失败按边界暂缓。

### P2-4　升级 `maxSurge: 0` 约束（#21）

- **现状**：KCP/MD webhook 已校验 pool 的独占引用（引用的池存在、未被其他 KCP/MD 占用），但不校验升级策略。固定 IP 场景下这是缺口：`maxSurge≥1` 的滚动升级先建新机再删旧机，新机需要额外空闲槽位拿固定 IP，池满时升级会卡在等 IP。
- **设计**：固定 IP 场景强制先删后建（复用刚释放的槽位），不支持 surge，故 webhook 采用强制校验而非 precheck。infra template 引用 `machineConfigPoolRef` 时，KCP/MD webhook（`internal/webhooks/{kubeadmcontrolplane,machinedeployment}.go`）追加三条约束：
  - **`maxSurge == 0`**（KCP 与 MD 均适用）：只接受整型 0，`nil`（默认 1）与非零值一律拒绝。MD 的 `OnDelete` 策略无 surge，跳过。
  - **MD `maxUnavailable ≥ 1`**：`maxSurge` 被钉为 0 后，滚动升级只能靠先删旧机腾槽位再建新机；而 CAPI 默认 `maxUnavailable=0`，与 `maxSurge=0` 组合会 0/0 卡死，故要求 `maxUnavailable` 显式 ≥ 1。
  - **KCP `replicas ≥ 3`**：CAPI 仅允许控制面在副本数 ≥ 3 时用 `maxSurge=0`（scale-in）。由本 webhook 提前拦截并给出清晰 reason，避免落到 CAPI 那条含糊的 scale-in 报错；不支持单副本固定 IP 控制面。
- **验收**：固定 IP 的 KCP/MD 只能以 `maxSurge=0`（且 MD `maxUnavailable≥1`、KCP `replicas≥3`）创建/更新，否则被 webhook 拒绝并给出对应 reason；webhook 单测覆盖各边界。

---

## 四、落地路线图

仓库：`[源]` = `github.com/alauda/cluster-api-provider-vsphere`；`[交]` = `ait/cluster-api-provider-vsphere`。

| Issue | 主题 | 仓库 | 优先级 | 依赖 | 规模 |
|:---:|---|:---:|:---:|---|:---:|
| A | MachineConfigPool status 计数器 + printcolumn | [源] | P1 | — | 小 |
| B | Pool 级 conditions + v1beta2 | [源] | P1 | — | 中 |
| C | MachineConfigPool validation | [源] | P1 | — | 中 |
| D | ACP 文档四件套 + README fork 基线 | [交]+[源] | P1 | — | 中 |
| E | 默认固定 IP 模板 | [源] | P1 | 可与 J 合并 | 小 |
| F | 交付仓库去明文凭据 | [交] | P1 | — | 小 |
| G | 持久盘 observed state 迁 status | [源] | P2 | — | 中 |
| H | govmomi LB/VIP | [源] | ⏸ 暂缓 | 自建 VIP 统一改造 | 大 |
| I | BootstrapReady condition | [源] | P2 | — | 小 |
| J | 固定 IP 升级约束（maxSurge=0 / MD maxUnavailable≥1 / KCP replicas≥3） | [源] | P2 | E | 小 |
| K | Pool 槽位非持久化数据盘 | [源] | P1 | C 可并 | 中 |
| L | 容灾 encryption-config（改为 KCP YAML 声明，不涉及代码） | — | ⏸ 无代码 | — | 小 |
| M | VM 开机仅在创建时执行一次 | [源] | P1 | — | 小 |

**推进顺序**：A/B/C/K/L/M → D/F → E/J → G/I。

**通用完成标准**：`make manifests`/`make generate` 无 diff；`go test ./...` 通过；新增/变更有单测；capabilities 与代码一致；CRD 变更同步交付仓库 chart。

---

## 五、附录：代码位置与参考

**源码仓库关键位置**

| 主题 | 位置 |
|---|---|
| MachineConfigPool 类型/status/webhook | `apis/v1beta1/vspheremachineconfigpool_types.go`、`internal/webhooks/vspheremachineconfigpool.go` |
| Cluster/Machine 类型与 conditions | `apis/v1beta1/{vspherecluster,vspheremachine}_types.go`、`condition_consts.go` |
| 地址 ready | `pkg/services/vimmachine.go:279-343` |
| VM 开机 reconcile | `pkg/services/govmomi/service.go:225`（调用）、`:438`（`reconcilePowerState`） |
| pause/deletion 顺序 | `controllers/vspherecluster_reconciler.go:137-143`、`vspheremachine_controller.go:327-333`、`vspherevm_controller.go:264-426`、`vmware/vspherecluster_reconciler.go:124-131` |
| kube-ovn AppRelease | `controllers/vspherecluster_reconciler.go:363-476` |
| 槽位复用/持久盘检查 | `controllers/vspherevm_controller.go:490-549` |
| clusterModules 回写 spec | `controllers/clustermodule_reconciler.go:166` |
| 默认模板 / 升级 webhook | `templates/cluster-template.yaml`、`internal/webhooks/{kubeadmcontrolplane,machinedeployment}.go` |

**交付仓库**：`ait/cluster-api-provider-vsphere` — `chart/`（Chart.yaml、module-plugin.yaml、values.yaml、templates/、templates/crds/×10）、`.build/build.yaml`、`Dockerfile.capv`、`Dockerfile.cpi`、`docs/integration-prerequisites.md`。

**参考实现**

- HCS chart/docs：`ait/cluster-api-provider-hcs/{chart,.build/build.yaml,docs/capabilities.md}`
- HCS Pool 计数器：`ait/cluster-api-provider-hcs/api/v1beta1/hcsmachineconfigpool_types.go`
- DCS/HCS 持久盘 status：`ait/cluster-api-provider-dcs/api/v1beta1/dcsiphostnamepool_types.go`
