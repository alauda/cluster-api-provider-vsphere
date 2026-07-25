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

CAPV 的 CAPI 合同与主模型四件套完整，规范点名的正确性问题（pause/deletion 顺序、地址 ready、kube-ovn 状态、槽位复用）已修复，交付产物已在交付仓库具备。剩余差距集中在三块，也是建议的推进顺序：

1. **ConfigPool 的 status/conditions/validation**（P1-1/2/3）：缺容量计数器，Pool 级 condition 不全，webhook 校验覆盖少。
2. **ACP 文档与默认固定 IP 模板**（P1-4/5）：capabilities 等四件套缺失，默认模板仍走 DHCP。
3. **交付仓库质量**（P1-6）：CRD 同步机制待确认，values.yaml 含明文凭据，缺 render 校验。

这三块直接对应规范的「质量门禁」与「最小验收清单」。此外，规范有 7 条条款因 CAPV 的 fork 属性不适用或需裁剪，见第五章。

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
| 7 | ConfigPool status 计数器 total/available/allocated | 仅 per-slot `configStatuses`，无计数器与 printcolumn | 🟡 | P1-1 |
| 8 | Pool 级 conditions + v1beta2 | Pool 仅 `ClusterRefReady`；槽位分配信号在 Machine 侧；无 v1beta2 | 🟡 | P1-2 |
| 9 | ConfigPool validation（唯一性、immutable、finalizer 阻删等） | webhook 仅校验 hostname 格式与 clusterRef（`internal/webhooks/vspheremachineconfigpool.go`） | 🟡 | P1-3 |
| 10 | 持久盘 observed state 落 status | `volumePath/diskUUID` 回填到 spec（`vspheremachineconfigpool_types.go:148-155`），无 `PersistentDiskStatus` | 🟡 | P2-1 |
| 11 | 固定 IP 为默认交付路径 | 默认模板 `dhcp4: true`，无 `machineConfigPoolRef` | 🟡 | P1-5 |
| 12 | 固定 IP 升级默认 `maxSurge: 0` | KCP/MD webhook 查 pool 共享，但不强制 `maxSurge: 0` | 🟡 | P2-4 |
| 13 | 控制面 LB/VIP 由 ClusterReconciler 管理 | `VSphereClusterSpec` 无 `controlPlaneLoadBalancer`，VIP 由模板 kube-vip 承担 | ⏸ | P2-2 |
| 14 | bootstrap 状态可诊断 | 仅通用 `VMProvisioned` + `WaitingForBootstrapData`，无独立 `BootstrapReady` | 🟡 | P2-3 |
| 15 | 交付质量（CRD 同步、凭据、render 校验） | CRD 为提交副本；`values.yaml` 含明文凭据；无 render/lint | 🟡 | P1-6 |
| 16 | ACP 文档 capabilities/usage/known-issues/testing | 源码与交付仓库均缺（交付仓库仅 `integration-prerequisites.md`） | ❌ | P1-4 |
| 17 | README 说明 fork 基线与兼容矩阵 | 仍为上游 README，无 baseline/patch 范围/版本矩阵 | 🟡 | P1-4 |
| 18 | spec 不承载 observed state | controller 回写 `spec.clusterModules.moduleUUID`（`clustermodule_reconciler.go:166`），为上游既有设计 | 🟡 | P2-5 |
| 19 | `XxxClusterSpec` 建模 `networkType` 等字段 | 未建模；kube-ovn 适用性靠 CAPI Cluster annotation `cpaas.io/network-type` 判断（`vspherecluster_reconciler.go:365`） | 🟡 | P1-4（文档化） |
| 20 | 数据盘区分 ephemeral/persistent 两类 | Pool 槽位仅有 `persistentDisks`；非持久盘只有上游模板级 `dataDisks`（仅 attach，无格式化挂载，模板内同构），槽位级无法声明 | 🟡 | P2-6 |

---

## 三、改进方案

P1 = 验收/上架必需；P2 = 随产品节奏推进。

### P1-1　ConfigPool status 计数器 + printcolumn（#7）

- **设计**：status 增加 `Total/Available/Allocated int32`，每次 reconcile 由 `configStatuses` 汇总；加 printcolumn。
- **参考**：HCS `HCSMachineConfigPoolStatus.{TotalCount,AvailableCount}` 及其 printcolumn。
- **验收**：`kubectl get vspheremachineconfigpool` 显示三个计数；单测覆盖。

### P1-2　Pool 级 conditions + v1beta2（#8）

- **设计**：新增规范建议的 Pool 级 condition（`Ready/MembersValid/MembersUnique/SlotAvailable/PersistentDisksReady`），并补 v1beta2 conditions 字段与 getter/setter（对齐本仓库 Cluster/Machine 已有做法）。
- **参考**：DCS/HCS 在 `api/v1beta1/conditions*.go` 集中定义 condition，reason 用 UpperCamelCase。
- **验收**：重复 hostname/IP、无可用槽位、盘未就绪等场景有对应 condition。

### P1-3　ConfigPool validation 补齐（#9）

- **设计**：基础约束用 CRD marker（`configs` MinItems=1、`sizeGiB` minimum、`unitNumber` 范围）；跨字段/跨资源约束用 webhook（hostname/primary IP/IPv6 唯一，持久盘 name/unitNumber 唯一且排除 7，已分配槽位关键字段 immutable，跨 pool 重复检查，删除时有 allocated slot 或未清理盘则 finalizer 阻止）。网络名称只做格式校验，存在性在 reconcile 期校验（见第五章 N6）。
- **验收**：create/update/delete validation 单测齐全。

### P1-4　ACP 文档四件套 + README fork 基线（#16、#17、#19）

- **放置**（需 owner 确认）：capabilities/usage/known-issues/testing 放交付仓库（跟随交付版本）；fork 基线、patch 范围、兼容矩阵放源码仓库 README 或 `docs/development.md`；两仓交叉引用。
- **设计**：
  - `capabilities.md` 按 HCS 七段结构：基线、评估说明、提供能力、关键边界、主要缺口、由其他组件承接、代码存在但未验收路径。需写明：固定 IP 是默认主路径而 DHCP/IPAM 是 legacy、`maxSurge: 0` 升级策略、kube-ovn 依赖 annotation `cpaas.io/network-type` 的平台约定、supervisor 模式与历史 API 版本为未验收路径。
  - `usage.md`：固定 IP 建集群（单控制面/HA/worker）、ConfigPool、持久盘、多网卡、删除与 reclaim。
  - `known-issues.md`：按 HCS `KI-xxx-NNN` 格式。
  - `testing/`：测试计划、报告模板、正式版本报告。
- **验收**：四件套齐全；capabilities 与代码、测试报告一致；能力标注 supported/experimental/legacy/non-goal。

### P1-5　默认固定 IP 模板（#11）

- **设计**：ACP 默认模板改为 `VSphereMachineConfigPool` + `machineConfigPoolRef` + KCP/MD `maxSurge: 0`；DHCP/IPAM 模板保留并在 capabilities 标注 legacy。
- **验收**：默认创建路径走固定 IP/hostname 槽位；样例与 CRD 一致。

### P1-6　交付仓库质量补强（#6、#15）

chart 已具备，本项为质量补强：

1. **CRD 同步**：确认 `chart/templates/crds/` 由源码 `make manifests` 同步；否则加同步脚本或 CI diff 校验。
2. **凭据**：`values.yaml` 中的明文密码/server/username 改为占位符或安装时注入。
3. **命名/版本**：确认 `appReleases[0].name` 与模块名不一致、`Chart.yaml.version: v0.0.0` 是流水线占位而非漏配。
4. **校验**：补 `helm lint`/render 的 CI 或本地脚本。

- **验收**：CRD 与源码无 diff；values 无明文凭据；lint/render 通过。

### P2-1　持久盘 observed state 迁到 status（#10）

- **设计**：新增 `status.persistentDiskStatus`（绑定 hostname/slot，含 volumePath/diskUUID、phase、owner machine、attached VM、lastError 等），controller 不再回写 spec；现有 `MachineConfigSlotReclaimStatus` 并入其 phase 子状态。
- **参考**：DCS/HCS 的 `PersistentDiskStatus` 均已在 status，字段结构可参照。
- **验收**：spec 只含用户声明字段；升级不误删盘；attach/detach/reclaim 单测。

### P2-2　govmomi 控制面 LB/VIP（#13）——⏸ 暂缓

自建 VIP 后续平台级统一改造，本项不单独推进，待统一方案评审。现状留档：`VSphereClusterSpec` 只有 `controlPlaneEndpoint`；govmomi 模式 VIP 由模板中的 kube-vip static pod 承担；supervisor 模式已有 `LoadBalancerReady`。

### P2-3　BootstrapReady condition（#14）

- **设计**：新增 `BootstrapReady` condition，用不同 reason 区分 secret 缺失、内容缺失、读取失败、投递失败。规范的投递 hash 要求对 CAPV 大部分不适用（见第五章 N5），checksum 仅作可选诊断信息。
- **验收**：用户能区分 bootstrap 未就绪、VM 未创建完、投递失败。

### P2-4　升级 `maxSurge: 0` 约束（#12）

- **设计**：infra template 引用 `machineConfigPoolRef` 时，KCP/MD webhook 默认要求 `maxSurge: 0`；允许 surge 时 precheck 需确认有足够空闲槽位。位置：`internal/webhooks/{kubeadmcontrolplane,machinedeployment}.go`。
- **验收**：未扩容 ConfigPool 时无法绕过固定 IP 约束；webhook 单测。

### P2-5　`spec.clusterModules` observed state（#18）

- **设计**：属上游既有设计（见第五章 N3），短期在 capabilities 标注为上游兼容例外、字段由 controller 管理；中长期跟随上游演进评估迁移。
- **验收**：文档明确该字段由 controller 管理，用户不应手改。

### P2-6　Pool 槽位非持久化数据盘（#20）

- **现状**：非持久盘只能通过上游模板级 `dataDisks` 表达（`apis/v1beta1/types.go:205-210`）：仅 name/sizeGiB/provisioningMode，只 attach 不格式化挂载，且模板内所有节点同构。槽位级差异化的非持久盘（每节点不同大小/挂载点、随 VM 删除重建）无法声明。DCS/HCS 的 pool 槽位同样只有持久盘。
- **设计**：Pool 槽位 `configs[]` 增加非持久盘声明（如 `ephemeralDisks`，字段对齐 `PersistentDisk` 的 sizeGiB/datastore/unitNumber/mountPath/fsFormat，但无 reclaim/保留语义）；创建 VM 时随 clone attach，复用持久盘现有的格式化/挂载管线；删除 VM 时随 VM 清理，不进入槽位释放门禁。与模板级 `dataDisks` 的分工写入 capabilities：模板级用于同构 attach-only 盘，槽位级用于差异化且需挂载的盘。
- **验收**：槽位声明的非持久盘随 VM 创建/删除；升级重建后按声明重建空盘；unitNumber 与持久盘不冲突（validation 并入 P1-3）。

---

## 四、落地路线图

仓库：`[源]` = `github.com/alauda/cluster-api-provider-vsphere`；`[交]` = `ait/cluster-api-provider-vsphere`。

| Issue | 主题 | 仓库 | 优先级 | 依赖 | 规模 |
|:---:|---|:---:|:---:|---|:---:|
| A | ConfigPool status 计数器 + printcolumn | [源] | P1 | — | 小 |
| B | Pool 级 conditions + v1beta2 | [源] | P1 | — | 中 |
| C | ConfigPool validation | [源] | P1 | — | 中 |
| D | ACP 文档四件套 + README fork 基线 | [交]+[源] | P1 | — | 中 |
| E | 默认固定 IP 模板 | [源] | P1 | 可与 J 合并 | 小 |
| F | 交付质量补强 | [交] | P1 | — | 小 |
| G | 持久盘 observed state 迁 status | [源] | P2 | — | 中 |
| H | govmomi LB/VIP | [源] | ⏸ 暂缓 | 自建 VIP 统一改造 | 大 |
| I | BootstrapReady condition | [源] | P2 | — | 小 |
| J | maxSurge: 0 约束 + precheck | [源] | P2 | E | 小 |
| K | clusterModules 文档化/迁移评估 | [源] | P2 | D | 小 |
| L | Pool 槽位非持久化数据盘 | [源] | P2 | C 可并 | 中 |

**推进顺序**：A/B/C → D/F → E/J → G/I/L → K。

**通用完成标准**：`make manifests`/`make generate` 无 diff；`go test ./...` 通过；新增/变更有单测；capabilities 与代码一致；CRD 变更同步交付仓库 chart。

---

## 五、规范条款对 CAPV 的不适用项

本章回答：规范（草案 v0.1）中哪些条款对 CAPV 不适用或需要裁剪，及其原因。根因是同一个：**规范以从零新建 provider 为默认视角，而 CAPV 是上游 fork**——上游的 CRD 字段、API group、目录结构不是本仓库可自由重设的，强行对齐会带来 rebase 冲突和 conversion 负担。规范在「仓库结构-允许例外」中已认可这一例外，本章将其落到具体条款。

| # | 规范条款 | 适用性 | 替代做法 |
|:---:|---|:---:|---|
| N1 | 交付产物与源码同仓 | 不适用 | 交付仓库承接，两仓交叉引用 |
| N2 | `XxxClusterSpec` 建模 `networkType`、`encryptionProviderConfigRef` | 不适用 | 平台约定 annotation + capabilities 文档化 |
| N3 | spec/status 分离追溯到上游既有字段 | 不适用（追溯） | capabilities 标注上游兼容例外 |
| N4 | 单一 API group、仅 v1beta1 | 不适用 | supervisor group 与历史版本标注未验收 |
| N5 | bootstrap 投递 hash 防重复写入 | 大部分不适用 | 只补 `BootstrapReady` condition |
| N6 | ConfigPool 校验「subnet 引用合法」 | 部分不适用 | webhook 格式校验 + reconcile 期 condition |
| N7 | 「不透传 IaaS 全量参数」追溯执行 | 不适用（追溯） | capabilities 分层标注能力状态 |

LB/VIP 相关条款（「LoadBalancer/VIP 生命周期」章节）不在本章讨论范围，理由见文首前提 2。

### N1　交付产物与源码同仓

规范要求 `chart/`、`.build/`、`docs/` 与源码同仓。本仓库为维持 rebase 能力保留上游目录结构，交付产物在交付仓库维护。规范「允许例外」明确支持这种形态：「如果基于上游 CAPV 这类大型 provider，可保留上游目录，但必须补齐 ACP 交付和能力说明文档」。例外豁免的是目录形态，不豁免产物本身——文档（P1-4）与交付质量（P1-6）义务保留。

### N2　`XxxClusterSpec` 建模 `networkType`、`encryptionProviderConfigRef`

`VSphereClusterSpec` 是上游 CRD，fork 新增 spec 字段需长期 carry patch 并维护 conversion，且无法回流上游。kube-ovn 适用性判断已有实现路径——CAPI Cluster 的 annotation `cpaas.io/network-type`，属于规范 kube-ovn 章节允许的「平台约定资源」输入；`encryptionProviderConfigRef` 面向灾备，vSphere 当前无该产品场景，按规范「无场景不建模」的原则不应新增。annotation 约定需写入 capabilities（P1-4）；未来灾备覆盖 vSphere 时再按规范流程建模。

### N3　spec/status 分离追溯到上游既有字段

`spec.clusterModules.moduleUUID` 由 controller 回写，不符合「spec=期望、status=observed」，但这是上游既有设计：单方面迁移到 status 需要改上游 CRD、做 conversion、处理存量对象，并在之后每次 rebase 时与上游冲突，成本大于收益。豁免仅限上游既有字段——Alauda 自有的持久盘回填字段仍适用规范（P2-1 成立）。处理方式见 P2-5。

### N4　单一 API group、仅 v1beta1

CAPV 上游自带第二 group `vmware.infrastructure.cluster.x-k8s.io`（supervisor 模式）和历史版本 `v1alpha3`/`v1alpha4` 及 conversion webhook。ACP 不使用 supervisor 模式，但删除这些 API 会使 fork 大幅偏离上游，而保留它们对 ACP 无影响（不部署对应 controller 即可）。规范要求的「第二组 API 必须说明原因」由 capabilities 承接：标注为「代码存在但未正式验收路径」（P1-4）。

### N5　bootstrap 投递 hash 防重复写入

该条款针对可重复推送的投递机制（如 Elemental plan）：controller 每次 reconcile 都可能重新投递，需要 hash 避免重复写入。CAPV 的 bootstrap userdata 走 guestinfo，仅在 createVM 时一次性写入（`pkg/services/govmomi/service.go:135-150`），且 CAPI 合同规定 bootstrap data 对单个 Machine 不可变，变更靠重建 Machine——不存在重复写入路径。条款中「secret 缺失等待并写 condition、投递失败写 condition」的部分仍适用，落点 P2-3。

### N6　ConfigPool 校验「subnet 引用合法」

DCS/HCS 的 subnet 是平台侧可查询的资源对象，webhook 可校验引用存在性；vSphere 的网络是 port group 名称字符串，没有 subnet 资源，校验存在性需在 admission webhook 中同步调 vCenter——既拖慢 admission，又在 vCenter 不可达时阻塞所有 ConfigPool 写操作。裁剪为：webhook 只做格式校验（P1-3），存在性在 reconcile 期校验并以 condition 暴露（P1-2）。

### N7　「不透传 IaaS 全量参数」追溯执行

上游 CAPV 的 API 已暴露超出 ACP 场景的参数（`CloneMode`、`CustomVMXKeys`、`PciDevices`、`hardwareVersion` 等，见 `apis/v1beta1/types.go`）。这些是上游既有 API surface，删除会破坏 CRD 兼容性。该条款对 fork 只能前瞻性适用（Alauda 新增字段遵守）。替代机制即规范自身给出的 capabilities 分层：验收过的标 supported，上游存在但未验收的如实标注，明确不做的标 non-goal（P1-4）。评审时「上游字段多」本身不应记为缺陷。

---

## 六、附录：代码位置与参考

**源码仓库关键位置**

| 主题 | 位置 |
|---|---|
| ConfigPool 类型/status/webhook | `apis/v1beta1/vspheremachineconfigpool_types.go`、`internal/webhooks/vspheremachineconfigpool.go` |
| Cluster/Machine 类型与 conditions | `apis/v1beta1/{vspherecluster,vspheremachine}_types.go`、`condition_consts.go` |
| 地址 ready | `pkg/services/vimmachine.go:279-343` |
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
