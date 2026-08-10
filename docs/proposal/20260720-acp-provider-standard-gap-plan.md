# CAPV 对齐 ACP Cluster API Provider 开发规范改进计划

> 日期：2026-07-20  
> 参考规范：`cluster-management-docs/docs/cluster-api-provider-development-standard.md`  
> 目标：记录当前 CAPV 与 ACP provider 开发规范的主要差距、整改优先级和后续 agent 可直接跟进的任务拆分。

## 背景

当前仓库已经在 upstream CAPV 基础上实现了一批 ACP 相关能力，例如 `VSphereMachineConfigPool`、固定 hostname/IP slot、persistent disk reclaim、kube-ovn AppRelease 直管路径、providerID/failure domain/ClusterModule 等。但从 ACP provider 开发规范视角看，仍未形成完整产品级闭环。

主要缺口集中在：

1. 交付产物缺 Helm chart、ModulePlugin、`.build/build.yaml`。
2. 缺 ACP 风格能力边界、使用、已知问题和测试报告文档。
3. 固定 IP / ConfigPool 还不是默认交付主路径。
4. `VSphereMachineConfigPool` status、condition、validation 不完整。
5. kube-ovn AppRelease 缺 ready wait、condition 和 event。
6. govmomi 模式控制面 LB/VIP 生命周期未由 `VSphereClusterReconciler` 统一管理。
7. pause 语义可能阻断 deletion cleanup。
8. `VSphereMachine.Status.Ready` 可能在没有真实 IP 时被置为 true。
9. sample/test report 与当前 API 命名存在不一致。

## 非目标

本计划不要求一次性完成所有重构，也不要求立即改变 upstream CAPV 兼容 API。对历史字段或 upstream 行为，如果短期不迁移，应在 `docs/capabilities.md` 中明确标为兼容例外或 legacy path。

## 优先级总览

| 优先级 | 主题 | 目标 |
|---|---|---|
| P0 | 正确性和资源清理 | 修复可能导致资源残留或错误 ready 的问题 |
| P1 | 产品化闭环 | 补齐文档、交付、默认固定 IP 路径、ConfigPool status/validation |
| P2 | 架构收敛 | 收敛 LB/VIP、bootstrap、persistent disk、ClusterModule 语义 |

## P0：正确性和资源清理

### 1. 修复 pause/deletion reconcile 顺序

#### 问题

多个 reconciler 当前是先判断 pause，再进入 deletion path。规范要求 pause 只阻断 normal reconcile，deletion path 应继续执行必要清理。

#### 已知位置

- `controllers/vspherecluster_reconciler.go`
- `controllers/vspheremachine_controller.go`
- `controllers/vspherevm_controller.go`
- `controllers/vmware/vspherecluster_reconciler.go`

#### 目标行为

推荐顺序：

1. 获取对象。
2. 构造 patch helper / context。
3. 如果 `DeletionTimestamp` 非空，优先走 delete path。
4. 再检查 pause，pause 时仅跳过 normal path。

#### 验收标准

- pause 状态下删除 `VSphereCluster` / `VSphereMachine` / `VSphereVM` 仍能执行 provider-owned 资源清理。
- 增加或更新单测覆盖 paused deletion 场景。

---

### 2. 修复 `VSphereMachine` address readiness 判断

#### 问题

`reconcileNetwork` 会无条件追加 `MachineInternalDNS`，后续 “no addresses” 判断可能永远不触发，导致没有真实 IP 时仍可能把 machine 标记 ready。

#### 已知位置

- `pkg/services/vimmachine.go`

#### 目标行为

- 判断 network/address ready 时，至少要求存在一个 IP 类型地址：
  - `MachineInternalIP` 或
  - `MachineExternalIP`
- `MachineInternalDNS` 不应单独满足 IP ready 条件。

#### 验收标准

- 当 VM 没有任何 IP 时，`VSphereMachine.Status.Ready` 不应为 true。
- condition/message 能说明正在等待 IP 地址。
- 增加单测覆盖只有 DNS、没有 IP 的场景。

---

### 3. kube-ovn AppRelease ready 状态不能被 `VSphereCluster.ready=true` 掩盖

#### 问题

当前 `VSphereCluster.Status.Ready = true` 在 kube-ovn reconcile 前设置；kube-ovn AppRelease create/update 后没有等待 ready，也没有专门 condition/event。

#### 已知位置

- `controllers/vspherecluster_reconciler.go`
- `apis/v1beta1/condition_consts.go`

#### 目标行为

- 对 `cpaas.io/network-type=kube-ovn` 的集群设置 kube-ovn 相关 condition。
- create/update AppRelease 后等待 AppRelease ready 或至少记录 pending 状态。
- registry/version/podCIDR/serviceCIDR/workload client/control-plane node 不满足时写 condition。

#### 建议 condition

- `KubeOvnReady`
- `KubeOvnAppReleaseReady`
- `KubeOvnControlPlaneNodesReady`

#### 验收标准

- kube-ovn 未 ready 时，用户可通过 `VSphereCluster.status.conditions` 看到卡点。
- AppRelease 创建、更新、等待、失败均有 condition 或 event。
- 增加单测覆盖缺 annotation、ModulePlugin latestVersion 为空、workload client 不可用、control-plane node 不齐、AppRelease not ready 等场景。

## P1：文档和产品化闭环

### 4. 新增 ACP capabilities 文档

#### 目标文件

- `docs/capabilities.md`

#### 建议结构

1. 当前基线
   - upstream CAPV baseline version/commit
   - Alauda fork branch/commit
   - 验证日期
2. 当前已验收能力
3. 关键边界
4. 主要缺口
5. 明确不支持 / 非目标能力
6. 由其他组件承接的能力
7. 代码存在但未正式验收的路径
8. govmomi mode 与 Supervisor mode 差异
9. 固定 IP / ConfigPool / `maxSurge: 0` 升级策略
10. kube-ovn AppRelease 直管路径与未来 `minfo` 路径边界
11. persistent disk reclaim 策略
12. FailureDomain / ClusterModule 产品级说明

#### 验收标准

- 明确区分 supported、experimental、legacy、non-goal。
- 明确 fixed IP 是 ACP 默认主路径。
- 明确 DHCP/IPAM/template legacy path 边界。

---

### 5. 新增 usage、known issues、testing 文档

#### 目标文件/目录

- `docs/usage.md`
- `docs/known-issues.md`
- `docs/testing/`

#### `docs/usage.md` 至少包含

- 单控制面固定 IP 示例。
- HA 控制面固定 IP 示例。
- worker `MachineDeployment` 固定 IP 示例。
- `VSphereMachineConfigPool` 示例。
- persistent disk 示例。
- 多网卡示例。
- 删除和 reclaim 行为说明。

#### `docs/testing/` 至少包含

- 测试计划。
- 测试报告模板。
- 正式版本测试报告。
- 证据索引。

#### 验收标准

- 测试报告包含版本、commit、环境、用例、失败项、已知问题。
- 旧 sample/report 中不可复现或旧 API 命名应迁移或标注 legacy。

---

### 6. README 增加 fork baseline 和当前兼容矩阵

#### 问题

当前 README 仍偏 upstream CAPV，兼容矩阵过旧，缺 Alauda fork patch scope。

#### 目标内容

在 README 或 `docs/development.md` 增加：

- upstream CAPV baseline version/commit。
- 本地 patch 范围。
- Go / Kubernetes / CAPI / controller-runtime / controller-tools / envtest 版本。
- govmomi / Supervisor mode 差异。

#### 验收标准

- 用户能判断当前 fork 与 upstream CAPV 的差异。
- 用户能判断当前产品版本兼容范围。

## P1：交付产物

### 7. 补齐 Helm chart / ModulePlugin / BuildRun 交付

#### 目标文件

```text
chart/
  Chart.yaml
  values.yaml
  module-plugin.yaml
  templates/
.build/
  build.yaml
```

#### chart 要求

- `Chart.yaml` name、description、version、annotations 正确。
- `values.yaml` 中所有镜像 tag 可被流水线自动更新。
- `module-plugin.yaml` 包含 module name、display name、provider name、description、appRelease、mainChart、upgradeRisk。
- chart templates 不引用不存在的 values key。
- CRD/RBAC/Deployment/webhook 来源明确，避免手改漂移。

#### 流水线要求

- build manager image。
- 更新 chart image tag。
- 更新 module-plugin chart version。
- chart build。
- OCI label 包含 repo、commit、branch。

#### 验收标准

- 增加 `make chart-render` 或等价 target。
- 增加 chart render/build CI 或本地验证脚本。

## P1：固定 IP / ConfigPool 默认主路径

### 8. 新增 ACP 默认固定 IP cluster template

#### 问题

当前默认 `templates/cluster-template.yaml` 仍是 DHCP/IPAM 风格，无 `VSphereMachineConfigPool` 和 `machineConfigPoolRef`。

#### 目标

新增或调整 ACP 默认模板，包含：

- `VSphereMachineConfigPool`
- control-plane `VSphereMachineTemplate.spec.template.spec.machineConfigPoolRef`
- worker `VSphereMachineTemplate.spec.template.spec.machineConfigPoolRef`
- KCP / MD 默认 `maxSurge: 0` 或等价先删后建策略

#### 验收标准

- 默认 ACP 创建路径使用固定 IP/hostname slot。
- DHCP/IPAM path 明确标为 legacy/compatibility/experimental。
- sample 与当前 CRD 保持一致，不再使用旧 `VSphereResourcePool` / `resourcePoolRef`。

---

### 9. pool 场景 enforce `maxSurge: 0` 或要求额外 slot

#### 问题

已有 KCP/MD webhooks 检查 pool 引用和 consumer 共享，但没有强制 fixed IP pool 场景下的升级策略。

#### 已知位置

- `internal/webhooks/kubeadmcontrolplane.go`
- `internal/webhooks/machinedeployment.go`

#### 目标行为

- 当 infrastructure template 引用 `machineConfigPoolRef` 时：
  - 默认要求 KCP/MD 使用 `maxSurge: 0`；或
  - 如果允许 surge，必须 precheck 有足够额外 fixed IP/hostname slot，并在文档中说明。

#### 验收标准

- 未扩容 ConfigPool 时，不允许通过 DHCP/IPAM 绕过固定 IP 约束。
- 增加 webhook/precheck 单测。

## P1：ConfigPool API / status / condition / validation

### 10. 增加 ConfigPool status 汇总字段

#### 当前状态

`VSphereMachineConfigPoolStatus` 只有：

- `configStatuses`
- `consumerRef`
- `conditions`

#### 目标字段

建议增加：

```go
Total     int32 `json:"total,omitempty"`
Available int32 `json:"available,omitempty"`
Allocated int32 `json:"allocated,omitempty"`
```

#### 验收标准

- controller 每次 reconcile 更新汇总字段。
- `kubectl get/describe` 能快速看出容量状态。
- 单测覆盖 total/available/allocated 计算。

---

### 11. 增加 Pool 级 conditions

#### 建议 condition

- `Ready`
- `MembersValid`
- `MembersUnique`
- `SlotAvailable`
- `PersistentDisksReady`
- `ReclaimReady` 或 `PersistentDiskReclaimReady`

#### 验收标准

- duplicate hostname/IP、无可用 slot、persistent disk attached/reclaim failed 等问题有明确 condition。
- UI/运维脚本可按 condition type 判断池状态。

---

### 12. 完善 ConfigPool validation

#### 当前缺口

需要补充：

- `configs` minItems >= 1。
- hostname 唯一。
- primary IP / IPv6 唯一。
- persistent disk name/unit number 唯一。
- `unitNumber` 范围，排除 7。
- `sizeGiB` minimum。
- allocated slot 的 hostname/IP/disk size/type/unit immutable。
- subnet/reference 合法性。
- 同 namespace / 同 cluster 下跨 pool 重复 IP/hostname 检查。

#### 验收标准

- 优先用 CRD marker 表达基础约束。
- CRD marker 无法表达的跨字段/跨资源约束放到 webhook。
- 单测覆盖 create/update/delete validation。

---

### 13. 增加 ConfigPool v1beta2 conditions 兼容层

#### 目标

为 `VSphereMachineConfigPool` 增加类似 `VSphereCluster` / `VSphereMachine` 的 v1beta2 conditions 字段和 getter/setter。

#### 验收标准

- Patch helper 使用 owned v1beta2 conditions。
- v1beta1/v1beta2 condition 保持语义一致。

## P2：控制面入口 / LB / VIP

### 14. govmomi 模式收敛 provider-owned LB/VIP 生命周期

#### 当前问题

- `VSphereClusterSpec` 只有 `controlPlaneEndpoint`，没有 `controlPlaneLoadBalancer`。
- 默认模板通过 kube-vip static pod 隐式处理 VIP。
- govmomi path 缺 `EndpointReady` / `LoadBalancerReady` condition。

#### 目标

- 设计 `spec.controlPlaneLoadBalancer` 或 provider-specific VIP 字段。
- `VSphereClusterReconciler` 负责创建、引用、校验、回填和清理控制面入口。
- endpoint 与 VIP 不一致时 fail fast。
- 删除 Cluster 时清理 provider-owned LB/VIP；用户预创建资源只解除引用。

#### 验收标准

- Cluster condition 能表达 endpoint/LB/VIP ready 状态。
- 文档说明 internal/external、host、port、VIP mode、leader election、health check、故障切换、清理策略。

## P2：Bootstrap 投递

### 15. 增加 `BootstrapReady` condition 和 bootstrap data hash

#### 当前问题

bootstrap secret 缺失和投递失败会通过 VM provisioning 相关 condition 暴露，但缺独立 `BootstrapReady` 和 checksum/hash status。

#### 目标

- 增加 `BootstrapReady` condition。
- 增加 bootstrap data checksum/hash 状态字段或 annotation。
- secret 缺失、内容缺失、读取失败、投递失败使用不同 reason。

#### 验收标准

- 用户能区分 bootstrap 未准备、VM provisioning 未完成、guestinfo/cloud-init 投递失败。
- 内容变化时能判断是否已重新投递。

## P2：Persistent disk 语义收敛

### 16. 将 persistent disk observed state 收敛到 status

#### 当前问题

`volumePath`、`diskUUID` 等 observed 字段在 `spec.configs[].persistentDisks[]` 中，并由 controller backfill。

#### 目标

新增 per-disk status，例如：

```go
type PersistentDiskStatus struct {
    Name string `json:"name"`
    VolumePath string `json:"volumePath,omitempty"`
    DiskUUID string `json:"diskUUID,omitempty"`
    Phase string `json:"phase,omitempty"`
    OwnerMachineRef *corev1.ObjectReference `json:"ownerMachineRef,omitempty"`
    AttachedInstanceID string `json:"attachedInstanceID,omitempty"`
    LastError string `json:"lastError,omitempty"`
    LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}
```

#### 验收标准

- spec 表示用户期望，status 表示实际 observed state。
- 文档明确 reclaim 策略：释放后是否删除 persistent disk、何时删除、如何保留。
- 升级重建场景下不得误删 persistent disk。

## P2：ClusterModule observed state

### 17. 文档化或迁移 controller-written `spec.clusterModules`

#### 当前问题

controller 会重写 `VSphereCluster.Spec.ClusterModules`，该字段更像 observed state。

#### 目标

短期：

- 在 `docs/capabilities.md` 中说明这是 upstream CAPV 兼容例外。
- 明确该字段由 controller 管理，用户不应手动设置。

中长期：

- 评估迁移到 `status.clusterModules`。
- 如迁移，提供 deprecation 和 conversion 策略。

#### 验收标准

- GitOps 用户不会误以为 `spec.clusterModules` 是用户声明字段。
- 产品文档说明 ClusterModule 生命周期和验收边界。

## 建议拆分 issue / agent 任务

### Issue 1：Fix pause/deletion reconcile ordering

- 修改 cluster/machine/vm/supervisor cluster reconciler。
- 添加 paused deletion 单测。

### Issue 2：Fix machine address readiness

- 修改 `reconcileNetwork` 地址判断。
- 添加无 IP / 仅 DNS / 有 IP 单测。

### Issue 3：Add ACP capabilities and testing docs

- 新增 `docs/capabilities.md`、`docs/usage.md`、`docs/known-issues.md`、`docs/testing/`。
- README 增加 fork baseline 和兼容矩阵。

### Issue 4：Make ConfigPool fixed-IP template the ACP default path

- 新增固定 IP 默认模板。
- 迁移 sample 中旧 `VSphereResourcePool` / `resourcePoolRef`。
- 标注 DHCP/IPAM legacy path。

### Issue 5：Complete ConfigPool status/validation/conditions

- 增加 counters。
- 增加 Pool 级 conditions。
- 完善 webhook/CRD validation。
- 增加 v1beta2 conditions。

### Issue 6：Add kube-ovn AppRelease readiness condition/events

- 增加 kube-ovn condition constants。
- wait AppRelease ready。
- 增加 event recorder。
- 增加单测。

### Issue 7：Add ACP delivery chart/module-plugin/build config

- 新增 Helm chart。
- 新增 ModulePlugin。
- 新增 `.build/build.yaml`。
- 增加 chart render/build target。

### Issue 8：Define govmomi LB/VIP lifecycle

- 设计 API。
- controller 管理 endpoint/LB/VIP。
- condition/event/status。
- 文档和测试。

## 后续跟进建议

建议优先让后续 agent 按 issue 顺序推进。Issue 1、2 是代码正确性问题，应优先修。Issue 3、4、5 是 ACP 规范验收主干。Issue 6 与 ACP kube-ovn 正式场景相关，也建议尽早推进。Issue 7、8 可以在产品交付节奏中并行设计。
