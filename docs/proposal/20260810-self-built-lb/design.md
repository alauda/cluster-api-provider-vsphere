# 设计：vSphere self-built LB

本文给出 CAPV govmomi 模式支持 cluster creating 阶段自建 control-plane LB 的设计。调研结论见 [requirements-and-research.md](requirements-and-research.md)，验证用例见 [test-cases.md](test-cases.md)。

参考实现是 DCS provider 的同名需求（AIT-66125）。与 DCS 一致的部分本文只给结论，差异部分展开说明。

## Reviewer Brief

vSphere self-built LB 采用 alive。用户在 `VSphereCluster.spec.controlPlaneLoadBalancer.type=internal` 声明 provider-managed LB。provider 负责 bootstrap 临时 VIP、alive `ModuleInfo` 的创建与 patch、运行态 readiness 和 `SelfBuiltLoadBalancerReady` condition。alive 所需内核模块和 sysctl 由 alive 前置任务负责，provider 不注入、不持久化、不校验。

与 DCS 的起点差异是 API 表面：DCS 已有 `controlPlaneLoadBalancer` 字段只是没有实现，CAPV 需要新增整个字段。govmomi 模式当前不提供任何 provider-managed VIP，多 control-plane 集群依赖使用方自备外部 LB，因此 `type=internal` 是 provider 内建的唯一 VIP 来源。

核心链路分两段：

1. Bootstrap：`VMService.reconcileBootstrapUserData()` 只在首个 `kubeadm init` 节点注入临时 VIP，让 `controlPlaneEndpoint` 在 apiserver 起来前可达。
2. Controller：workload apiserver 可访问且 control-plane Node 注册完成后，`clusterReconciler` 创建或 patch alive `ModuleInfo`，cluster-transformer 走通用链路渲染 workload `AppRelease`，alive installer 在 control-plane 节点安装 keepalived / IPVS static pod。

需要重点 review 的决策：

| 关注点 | 设计结论 | 章节 |
| --- | --- | --- |
| 入口 | 新增可选字段 `spec.controlPlaneLoadBalancer`；字段缺省与 `type=external` 都保持现状语义，`type=internal` 启用 self-built LB。 | `3.1` |
| 运行组件 | 固定使用 alive；provider 创建和 patch alive `ModuleInfo`，不直接创建 workload `AppRelease`。 | `2`、`5.1` |
| bootstrap | 仅 `kubeadm init` 节点注入临时 VIP，走 cloud-config write_files + `runcmd` 前插；失败即阻断 kubeadm。 | `4` |
| backend 来源 | provider 不写 `masterIPs`；alive valuesTemplate 从 workload control-plane Node `InternalIP` 生成。 | `5.2` |
| 状态表达 | 新增 `SelfBuiltLoadBalancerReady` condition，v1beta1 与 v1beta2 双写。与 DCS 不同。 | `3.2` |
| 可变性 | 只在 CREATE 时可写：UPDATE 时 `nil` 不允许写成任何值，非空后 `type`、`host`、`port`、`vrid`、`interface` 全部不可变，也不允许置回 `nil`。无放行入口。 | `3.1`、`6` |
| 存量集群 | provider 升级不改变已有集群行为；字段为空的集群不能改用 self-built LB，只能新建集群启用。 | `6`、`9` |
| 删除边界 | cluster 删除不新增 self-built LB cleanup；minfo 清理走 platform Cluster `OwnerReference` 与 GC 通用链路。 | `7` |
| 依赖边界 | provider 只检查通用 minfo 渲染前置对象和 alive 制品，不合成 platform Cluster、`ClusterModule` 或 clusterregistry Cluster；不引入 alauda 编译期依赖，统一用 `unstructured`。 | `5.2`、`10` |

## 1. 目标

为 vSphere workload cluster 的 kube-apiserver 提供 provider-managed self-built LB，使多 control-plane cluster 的 `Cluster.spec.controlPlaneEndpoint` 指向可漂移、可转发的 VIP，并把 VIP 生命周期收敛到 `VSphereClusterReconciler`。

目标行为：

- `type=internal` 时，provider 注入 bootstrap 临时 VIP，创建、patch 并等待 alive `ModuleInfo`。
- `kubeadm init` 前 VIP 已在首个 control-plane 节点存在；后续 control-plane 通过 VIP join。
- control-plane Node 注册完成后，alive 接管 VIP 和 IPVS backend。
- `VSphereCluster` 通过 `SelfBuiltLoadBalancerReady` condition 表达 LB 状态。
- 字段缺省或 `type=external` 时，行为与今天完全一致。
- 删除 cluster 时不新增 self-built LB cleanup，不因 alive 状态阻塞 VM 或 cluster 删除。

非目标：

- 不调用 NSX ALB / NSX-T LB 等 vSphere 生态的独立 LB 产品 API。
- 不把 alive 安装职责交给 provider-alauda 或 cluster-transformer 的自动插件安装链路，也不为此改造这两个组件。
- 不支持在集群创建后设置或修改 `controlPlaneLoadBalancer`：字段为空的集群不能改用 self-built LB，已写入的不能改 LB 模式或任一关键字段。
- 不支持 control-plane 节点原地重启升级；节点升级只走 CAPI 滚动替换。bootstrap VIP 相应只保证一次性建立，不保证跨重启存活。
- 不在 provider 升级过程中把已有集群自动迁移成 `type=internal`。
- 不提供 Service type=LoadBalancer 的 VIP 能力；alive 只承担 control-plane VIP。
- 不实现 IPv6 或 dual-stack self-built LB。
- 不改变 supervisor 模式（`apis/vmware/v1beta1`）已有的 `LoadBalancerReady` 语义。

## 2. 设计总览

```text
[新增] VSphereCluster.spec.controlPlaneLoadBalancer(type=internal)
  -> [新增] webhook 校验 + endpoint 一致性
  -> [调整] VMService.reconcileBootstrapUserData()
      -> [新增] kubeadm init 节点注入 bootstrap VIP（write_files + runcmd 前插）
  -> [现有] 首个 control-plane 完成 kubeadm init，后续 control-plane 通过 VIP join
  -> [现有] clusterReconciler.reconcileNormal 等待 workload client 与 control-plane Node 注册
  -> [现有] reconcileKubeOvnAppRelease 完成 CNI
  -> [新增] ensureSelfBuiltLB
      ├─ 校验 VIP 冲突
      ├─ 调整 kube-proxy IPVS 配置
      ├─ 校验 minfo 渲染前置对象与 alive 制品
      └─ createOrPatch alive ModuleInfo
  -> [现有] cluster-transformer 渲染 workload cpaas-system/alive AppRelease
  -> [现有] alive installer 写入 keepalived static pod，持有 VIP 并配置 IPVS
  -> [新增] 等待 minfo / AppRelease / pod / VIP probe Ready，置 condition
  -> [现有] reconcileWorkloadSystemComponentRepositories
```

`[现有]` 表示沿用当前路径，`[新增]` 是本需求新增，`[调整]` 是在现有路径上加分支。

运行态对象关系：

| 对象 | 所在集群 | 所有者 | 说明 |
| --- | --- | --- | --- |
| `VSphereCluster` | 管理集群 | 用户 / CAPI | 声明 LB 意图，承载 condition。 |
| `ModuleInfo` | 管理集群 | CAPV provider | provider-managed alive 实例，通过 label 防止接管未知对象。 |
| `ModulePlugin/alive`、`ModuleConfig/alive-<version>` | 管理集群 | 平台插件包 | 提供 chart、配置模板和版本来源。 |
| `ClusterModule` / platform Cluster / clusterregistry Cluster | 管理集群 | 平台通用链路 | cluster-transformer 渲染 minfo 所需上下文。 |
| `AppRelease/cpaas-system/alive` | workload cluster | cluster-transformer | 由 minfo 渲染，provider 不直接创建。 |
| `Pod/kube-system/alive` | workload control-plane 节点 | alive installer | keepalived / IPVS runtime。 |

## 3. CRD 变更设计

### 3.1 VSphereCluster 规格

在 `apis/v1beta1/vspherecluster_types.go` 的 `VSphereClusterSpec` 新增可选字段：

```go
// ControlPlaneLoadBalancer describes the control plane endpoint's load balancer.
// Leaving it nil is allowed and means the same as type=external: the endpoint is
// provided by the user and the provider manages no VIP.
// +optional
ControlPlaneLoadBalancer *ControlPlaneLoadBalancer `json:"controlPlaneLoadBalancer,omitempty"`
```

```go
type ControlPlaneLoadBalancerType string

const (
    ControlPlaneLoadBalancerTypeInternal ControlPlaneLoadBalancerType = "internal"
    ControlPlaneLoadBalancerTypeExternal ControlPlaneLoadBalancerType = "external"
)

type ControlPlaneLoadBalancer struct {
    // Type selects who owns the control plane VIP.
    // +kubebuilder:validation:Enum=internal;external
    // +kubebuilder:default=external
    // +optional
    Type ControlPlaneLoadBalancerType `json:"type,omitempty"`

    // Host is the control plane endpoint address. For type=internal it is the IPv4 VIP.
    Host string `json:"host"`

    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Port int32 `json:"port"`

    // VRID is the keepalived VRRP router ID. Only used when type=internal.
    // Range is validated by the webhook, not by the CRD schema.
    // +optional
    VRID int32 `json:"vrid,omitempty"`

    // Interface pins the guest NIC that holds the VIP. Empty means auto-detect
    // by matching the node's primary IP.
    // +kubebuilder:validation:MaxLength=15
    // +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.:-]*$`
    // +optional
    Interface string `json:"interface,omitempty"`
}
```

字段语义：

| 输入 | 行为 |
| --- | --- |
| 字段缺省（`nil`） | 允许，语义等同 `type=external`：endpoint 由使用方提供，provider 不做任何 LB 相关动作。这一形态的存在目的就是兼容 internal VIP 之前创建的集群——升级不触碰这块配置，存量集群升级后即此形态，且只能保持此形态。 |
| `type=external` | 显式声明使用方自备入口。provider 校验 `host/port` 与 `controlPlaneEndpoint` 一致；`controlPlaneEndpoint` 为空时回填。不创建 minfo，不注入 bootstrap VIP。 |
| `type=internal` | 启用 self-built LB。`host` 是 IPv4 VIP，`port` 是 apiserver 端口，`vrid` 是 keepalived VRID。 |
| `interface` 为空 | bootstrap VIP 脚本按节点主 IP 自动探测网卡；alive installer 同样自动探测。 |
| `interface` 非空 | provider 写入 alive `ModuleInfo.spec.config.interface`，bootstrap VIP 使用同一接口。 |

字段整体可选、`type` 带 `external` 默认值，是为了让存量 `VSphereCluster` 在 CRD 升级后语义不变——存量对象不带该字段，落到「字段缺省」行。这与 DCS 的 `type` 必填不同，原因是 DCS 的字段早已存在且必填。

webhook 校验（新增 `internal/webhooks/vspherecluster.go`）：

- `port` 在 `1-65535`；`type` 只能是 `internal` 或 `external`。
- `type=internal` 时：`host` 必须是 IPv4 地址；`vrid` 必须在 `1-255`；`host` 不能等于同集群 `VSphereMachineConfigPool` 任一槽位的 `network.primary.ip` 或 `network.additional[].ip`。
- `vrid` 不在 CRD schema 设 min/max，避免 `type=external` 或历史对象显式 `vrid: 0` 在 schema 层被拦截；internal 场景由 webhook 保证。
- `spec.controlPlaneEndpoint` 非空时必须与 `host/port` 一致，不一致直接拒绝（对应 gap 分析 #19 的「endpoint 与 VIP 不一致时 fail fast」）。
- `type=external` 时 `vrid` 和 `interface` 被忽略，用户填写时返回 warning。
- 可变性只看字段本身，不看 cluster 是否 initialized：字段只在 CREATE 时可写。UPDATE 时 `nil` 不允许写成任何值（含 `type=external`）；非 `nil` 后 `type`、`host`、`port`、`vrid`、`interface` 全部不可变，整个字段也不允许置回 `nil`。没有例外放行入口。

示例：

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereCluster
metadata:
  name: tc-vsphere-sblb
  namespace: cpaas-system
spec:
  server: vcenter.example.com
  controlPlaneEndpoint:
    host: 10.226.82.206
    port: 6443
  controlPlaneLoadBalancer:
    type: internal
    host: 10.226.82.206
    port: 6443
    vrid: 42
    interface: ens192
```

### 3.2 VSphereCluster 状态

新增 condition，v1beta1 与 v1beta2 双写（本仓 condition 约定要求两套 API 版本同时处理）：

| API 版本 | 常量 |
| --- | --- |
| v1beta1 | `infrav1.SelfBuiltLoadBalancerReadyCondition`，reason 取 `SelfBuiltLoadBalancerInvalidConfigurationReason` / `SelfBuiltLoadBalancerReconcilingReason` / `SelfBuiltLoadBalancerNotReadyReason` |
| v1beta2 | `infrav1.VSphereClusterSelfBuiltLoadBalancerReadyV1Beta2Condition`，对应 reason 常量 |

命名与取值方式对齐已有的 `KubeOvnAppReleaseReadyCondition` / `VSphereClusterKubeOvnAppReleaseReadyV1Beta2Condition`，复用 `setKubeOvnAppReleaseCondition()` 同款写法，`condition_consts.go` 集中声明。

字段缺省或 `type=external` 时删除该 condition（`conditions.Delete` + `v1beta2conditions.Delete`），与 kube-ovn 在 `network-type != kube-ovn` 时的处理一致。

这一点与 DCS 不同：DCS 选择不新增 condition，只用 requeue 做 gate。CAPV 新增 condition 的理由是本仓已有成熟的 condition 约定，且 ACP provider 规范明确要求「Cluster condition 能表达 endpoint/LB/VIP ready 状态」（差距记录见 `docs/proposal/20260725-acp-provider-standard-alignment/requirements-and-research.md` 的 #19 / P2-2）。

不新增 self-built LB 专用 status 字段：VIP、backend、持有节点等运行态信息都能从 alive 侧观测，provider 复制一份只会引入不一致。

## 4. Bootstrap 流程

### 4.1 注入点

`VMService.reconcileBootstrapUserData()`（`pkg/services/govmomi/service.go`）是现有 hook：VM clone 之后、上电之前，在 `powerState == PoweredOff` 且 `format == CloudConfig` 时改写 guestinfo userdata，已经承担 kubeadm nodeRegistration 改写、持久盘 cloud-config 合并和 kubelet serving 证书注入。

bootstrap 调整解决的是创建阶段的启动顺序问题：

- `Cluster.spec.controlPlaneEndpoint` 已经指向 VIP。
- alive 要等 workload apiserver 可访问、control-plane Node 注册后才能通过 `ModuleInfo -> AppRelease -> alive pod` 部署。
- provider 自己也要通过该 endpoint 拿到 workload client 才能创建 minfo，所以 VIP 必须在 alive 之前先可用。
- 因此只在 init 节点添加临时 VIP，并保持到 alive 接管为止。

新增逻辑：

```text
[调整] VMService.reconcileBootstrapUserData
  ├─ [现有] getBootstrapData / 读取已写入的 userdata
  ├─ [现有] UpdateKubeadmNodeRegistration（hostname、nodeIP）
  ├─ [新增] 若 VMContext.SelfBuiltLB != nil 且当前是 kubeadm init 节点:
  │     merge 一段 bootstrap VIP cloud-config
  ├─ [现有] 持久盘 / 临时盘 cloud-config 合并
  ├─ [现有] kubelet serving 证书 cloud-config 合并
  └─ [现有] setUserData 回写 guestinfo
```

`VMContext` 新增 `SelfBuiltLB *SelfBuiltLBBootstrap` 字段，由 `vmReconciler` 在构造 VMContext 时解析并填充，方式与已有的 `MachineConfigSlot` 一致：按 `VSphereVM` 的 `cluster.x-k8s.io/cluster-name` label 取 `VSphereCluster`，`type != internal` 时置 nil。

```go
type SelfBuiltLBBootstrap struct {
    VIP       string
    Port      int32
    Interface string // 空表示按 NodeIP 自动探测
}
```

`NodeIP` 不放进该结构：`resolveNodeIdentity()` 已经解析出节点主 IP，注入时直接取用。

### 4.2 节点范围判定

- control-plane 判定沿用 CAPI `cluster.x-k8s.io/control-plane` label。
- `kubeadm init` 节点判定：userdata 的 `write_files` 含 `/run/kubeadm/kubeadm.yaml` 即 init 节点，含 `/run/kubeadm/kubeadm-join-config.yaml` 即 join 节点。`UpdateKubeadmNodeRegistration()` 已按同样的路径分支，判定逻辑抽成共享 helper。

join control-plane 不注入 VIP：join 时 VIP 已由 init 节点或 alive 持有。

### 4.3 注入内容

注入一段 cloud-config，交给 `util.MergeCloudConfigUserData()` 合并：

| 内容 | 说明 |
| --- | --- |
| `write_files: /etc/capv/bootstrap-vip.sh` | 探测网卡、添加 `<vip>/32`、发送 GARP；地址已存在时视为成功直接返回。 |
| `write_files: /etc/systemd/system/capv-bootstrap-vip.service` | `Type=oneshot`、`RemainAfterExit=yes`、`Wants/After=network-online.target`，`ExecStart=/etc/capv/bootstrap-vip.sh`。**不写 `[Install]` 段**。 |
| `runcmd`（前插）: `systemctl start capv-bootstrap-vip.service \|\| exit 1` | 保证 VIP 早于 kubeadm 生效，且失败时阻断后续 kubeadm。 |

两个机制保证顺序和阻断：

- `mergeCloudConfigBodies()` 对 `runcmd` 使用 `mergeSequenceBefore()`，注入条目会**前插**到 CABPK 生成的 kubeadm 命令之前。
- cloud-init 把全部 `runcmd` 条目拼进同一个 shell 脚本顺序执行，前插条目里的 `exit 1` 会终止整个 `runcmd`，kubeadm 不执行，CAPI 的 bootstrap 成功哨兵文件不会生成，失败在 `Machine` 层可见。这等价于 DCS 用 `Requires=`/`After=` 让 `kubeadm.service` 依赖 VIP unit 的效果。

用 systemd unit 而不是直接在 `runcmd` 里 `ip addr add`，理由是失败语义和可观测性：`Type=oneshot` 的 `systemctl start` 同步阻塞到脚本退出，退出码直接驱动上面的 `|| exit 1`；执行记录进 journal，`systemctl status` 可直接定位。

unit 不写 `[Install]` 段是刻意的：没有 `WantedBy` 就无法被 `enable`，systemd 结构上不可能在开机时拉起它。VIP 只在 `runcmd` 显式 `start` 的这一次被添加，此后不再有任何自动执行路径。

`bootstrap-vip.sh` 的网卡选择：

- `spec.controlPlaneLoadBalancer.interface` 非空时直接使用该接口，并校验节点主 IP 确实在该接口上。
- 为空时按节点主 IP 在本机 IPv4 地址中查找匹配接口，最多重试 120 秒等待网络就绪。
- 找不到接口时脚本退出非零，`systemctl start` 失败，kubeadm 被阻断；故障通过 VM console、cloud-init 日志和 `systemctl status` 暴露。

`mergeWriteFiles()` 按 `path` 覆盖去重，所以重复 reconcile 不会产生重复条目。

### 4.4 退场

bootstrap VIP 不是最终 HA 组件，交接由 alive 侧完成：alive installer 首次写入 keepalived manifest 前调用 `clear_vip()`，删除仍在目标接口上的 `/32` 地址，再交给 keepalived 持有。此后 VIP 只由 keepalived 在选主节点上持有。

VIP 添加只发生一次，之后不存在任何复活路径：

| 场景 | 行为 |
| --- | --- |
| 首台 control-plane 初始化 | 唯一添加 VIP 的时机，由 `runcmd` 显式 `systemctl start` 触发。 |
| join 的 control-plane 节点 | 按 4.2 不注入。 |
| worker 节点 | 不注入。 |
| control-plane 滚动升级 | CAPI 以新建 VM 替换旧机器，新 VM 走 join 路径，同样不注入；全部 control-plane 轮换后节点上不再存在该脚本和 unit。 |
| 节点重启 | unit 无 `[Install]` 段、从未被 `enable`，systemd 不会在开机时拉起，不重建 VIP。 |

节点原地重启不在支持范围内（见非目标）。这带来一个已知空窗：从 `kubeadm init` 成功到 alive pod Ready 期间，`controlPlaneEndpoint` 只由这个临时 VIP 承载，若首台节点在此期间重启，VIP 不会重建，集群失去 apiserver 入口且无法自愈，需人工在该节点重新执行 `systemctl start capv-bootstrap-vip.service`。窗口内的重启属于运维异常，不做自动化处理。

## 5. Controller 流程

### 5.1 职责表

| Controller / 模块 | 创建和更新职责 | 删除职责 | 不负责 |
| --- | --- | --- | --- |
| `clusterReconciler` | endpoint 一致性；`ensureSelfBuiltLB`；minfo；kube-proxy IPVS 配置；readiness gate 与 condition。 | 沿用现有删除流程，不新增 self-built LB cleanup。 | 不渲染 `AppRelease`；不合成平台对象；不在 cluster 删除时专门删 minfo；不维护 LB 专用 status 字段。 |
| `vmReconciler` / `VMService` | 解析 self-built LB bootstrap 配置；init 节点注入临时 VIP。 | 走现有 VM 删除逻辑。 | 不创建 minfo；不等待 AppRelease；不操作 alive pod；不注入内核模块或 sysctl。 |
| `VSphereCluster` webhook（新增） | 校验 LB 输入与 endpoint 一致性；UPDATE 时拒绝一切 LB 字段变化，含把空字段写成有值。 | 不参与 runtime 清理。 | 不访问 workload cluster。 |
| `VSphereMachineConfigPool` 校验 | 槽位 IP 不得与同集群 LB VIP 冲突。 | 保持现有槽位释放逻辑。 | 不管理 VIP 漂移和 alive lifecycle。 |
| cluster-transformer | 消费 `ModuleInfo`，按通用逻辑渲染 workload `AppRelease`。 | 带 platform Cluster `OwnerReference` 的 minfo 由 Kubernetes GC 删除；workload API 可达时 plugin minfo finalizer 按通用逻辑卸载 AppRelease。 | 不新增 vSphere 专属 affinity 或自动安装规则。 |
| alive runtime | 安装 keepalived static pod，持有 VIP 并转发 backend。 | static pod 停止时执行 `/live/stop.sh` 清理 runtime 状态。 | 不创建 VM；不选择 minfo version。 |

### 5.2 主流程

接入 `clusterReconciler.reconcileNormal()`，放在 `reconcileKubeOvnAppRelease()` 之后、`reconcileWorkloadSystemComponentRepositories()` 之前——alive pod 需要 CNI 就绪才能调度：

```text
[调整] reconcileNormal
  ├─ [现有] reconcileKubeadmControlPlaneSystemComponents
  ├─ [现有] reconcileDeploymentZones / identity / vCenter / ClusterModules
  ├─ [现有] Status.Ready = true
  ├─ [现有] reconcileKubeOvnAppRelease
  ├─ [新增] ensureSelfBuiltLB
  └─ [现有] reconcileWorkloadSystemComponentRepositories

[新增] ensureSelfBuiltLB
  ├─ LB 缺省或 type!=internal -> 删除 condition，skip
  ├─ 校验 endpoint 与 VIP 一致
  ├─ 校验 VIP 未与 VSphereMachineConfigPool 槽位 IP 冲突
  ├─ newRemoteClients 获取 workload client（不可用则 requeue）
  ├─ controlPlaneNodesRegistered 等待全部 control-plane Node
  ├─ ensureKubeProxyIPVSForVIP（strictARP、excludeCIDRs，等待 DaemonSet 滚动）
  ├─ 校验通用 minfo 渲染前置对象
  ├─ resolveAliveModuleVersion
  ├─ createOrPatch alive ModuleInfo
  ├─ 等待 minfo status.version 追平 spec.version（phase 只记日志，不阻塞）
  ├─ 等待 workload AppRelease cpaas-system/alive 的 Sync 与 Health 为 True
  ├─ 等待每个 control-plane Node 上 kube-system app=alive pod Ready
  ├─ 连续 probe https://<vip>:<port>/version
  └─ Ready 后置 condition=True
```

control-plane Node 全部注册后才创建 minfo，是为了让 alive valuesTemplate 拿到完整 backend 列表。复用现有的 `controlPlaneNodesRegistered()`（该函数按 `KubeadmControlPlane.spec.replicas` 比对已注册 control-plane Node 数）。

未 Ready 时统一 `RequeueAfter: 10 * time.Second` 并置 condition=False，与 kube-ovn 路径的节奏一致；制品缺失类问题返回 error。

前置对象处理：

| 前置对象 | 行为 |
| --- | --- |
| `ModulePlugin/alive` 缺失 | 返回错误，属于交付制品缺失。 |
| 按 `--plugin-alive-version`、`targetClusterVersions`、`latestVersion` 仍解析不到版本 | 返回错误，属于交付制品缺失。 |
| `ModuleConfig/alive-<version>` 缺失或 `status.readyForDeploy=false` | 返回错误，属于交付制品缺失。 |
| `ClusterModule/<clusterName>` 缺失 | requeue，等待平台通用投影对象出现。 |
| platform Cluster / clusterregistry Cluster 缺失 | requeue。 |
| workload remote client 不可用 | requeue。 |

所有平台对象统一用 `unstructured` 读取，不引入 `gomod.alauda.cn` 编译期依赖，延续本仓 `modulePluginGVK` 的既有做法：

| 对象 | GVK |
| --- | --- |
| `ModulePlugin` | `cluster.alauda.io/v1alpha1`（已有常量） |
| `ModuleInfo` | `cluster.alauda.io/v1alpha1`，资源 `moduleinfoes` |
| `ModuleConfig` | `cluster.alauda.io/v1alpha1` |
| `ClusterModule` | `cluster.alauda.io/v1alpha1` |
| platform Cluster | `platform.tkestack.io/v1` |
| clusterregistry Cluster | `clusterregistry.k8s.io/v1alpha1`，namespaced，与 Cluster 同 namespace |
| `AppRelease`（workload） | `operator.alauda.io/v1alpha1`（已有 `appReleaseGVR`） |

alive version 选择（与 DCS 一致）：

1. provider 启动参数 `--plugin-alive-version=<version>` 非空时直接使用。
2. 否则取 `ModulePlugin/alive.status.latestVersion` 作默认。
3. 若存在 `ModulePlugin/alive.status.targetClusterVersions[ClusterModule.spec.version]`，使用该映射版本。
4. 读取 `ModuleConfig/alive-<version>`，缺失或未 ready 即失败。

`--plugin-alive-version` 是 provider 级覆盖，影响该 provider 管理的所有 self-built LB 集群，只覆盖版本选择，不绕过 `ModuleConfig` 的存在性与 ready 检查。

### 5.3 ModuleInfo

名称与平台生成规则保持一致：`<clusterName>-<sha256(clusterName:alive:alive) 前 32 位 hex>`。该算法与 `GenerateModuleInfoName(clusterName, "alive", "alive")` 等价，CAPV 侧自行实现，不引入依赖。

```yaml
metadata:
  name: <clusterName>-<hash>
  annotations:
    cpaas.io/display-name: alive
  labels:
    cpaas.io/cluster-name: <clusterName>
    cpaas.io/module-name: alive
    cpaas.io/module-type: plugin
    infrastructure.cluster.x-k8s.io/self-built-lb-managed: "true"
spec:
  version: <resolved alive version>
  config:
    vip: <controlPlaneLoadBalancer.host>
    vrid: <controlPlaneLoadBalancer.vrid>
    apiserverPort: <controlPlaneLoadBalancer.port>
    httpPort: 11780
    httpsPort: 11781
    extraPorts: ""
    interface: <controlPlaneLoadBalancer.interface or "">
```

`vrid` 与三个端口写成数字，`extraPorts` 写成字符串，与 cluster-transformer `pkg/moduleinfo/alive.go` 为平台自建 alive 写入的类型一致。

`cpaas.io/module-catalog`、`cpaas.io/product` 等 label 从 `ModulePlugin/alive` 透传，与 DCS 一致，但**只在创建时写**：cluster-transformer 的 minfo mutating webhook 会用 `ModuleConfig` 覆写 `cpaas.io/product` 和 `cpaas.io/display-name`、`cpaas.io/module-name` 两个 annotation，provider 每轮重新断言会与它来回翻转。

provider **不写 `masterIPs`**：alive `plugin-config.yaml` 已从 workload control-plane Node `InternalIP` 生成 backend 并传给 installer 的 `MASTER_IPS`。

patch 规则：

- minfo 不存在时创建 provider-managed minfo。
- 存在且属于 provider-managed（`self-built-lb-managed=true`）时，只 patch 身份 label（`cluster-name`、`module-name`、`module-type`、`self-built-lb-managed`）、`spec.version` 和 `spec.config` 中 provider 负责的字段；annotation 与透传 label 交给平台 webhook，不参与 reconcile。
- 同名 minfo 存在但所有权 label 不匹配时返回错误，不接管未知对象。
- minfo status 存在滞后：readiness 只以 `status.version == spec.version` 为准，phase 不为 `Running` 只记日志，实际健康由 AppRelease、alive pod 和 VIP probe 判定。

### 5.4 kube-proxy IPVS 配置

kube-proxy 处于 IPVS 模式时会代理并宣告 VIP，与 alive 冲突。处理方式：

- 读取 workload cluster 的 `kube-system/kube-proxy` ConfigMap 的 `config.conf`；`mode != ipvs` 时不改，直接继续。
- `mode == ipvs` 时设置 `ipvs.strictARP=true`，并把 `<vip>/32` 追加进 `ipvs.excludeCIDRs`。
- kube-proxy 只在启动时读配置，改完 ConfigMap 不会自己重启，因此在 DaemonSet pod template 上打 `infrastructure.cluster.x-k8s.io/kube-proxy-config-hash`（配置哈希）触发滚动；provider 实际写过 ConfigMap 时另打 `kube-proxy-config-patched-at`，让「配置被改回去、哈希没变」的情况也能滚动一次。
- 之后等待 `kube-system/kube-proxy` DaemonSet 滚动完成（`observedGeneration`、`updatedNumberScheduled`、`numberAvailable` 与 `desiredNumberScheduled` 一致），期间 requeue。

与现有 `reconcileKubeProxyRepository()` 不冲突：后者改的是 DaemonSet 的镜像与 imagePullSecret，本流程改的是 ConfigMap 的 `config.conf`，对象不同、字段不重叠。为避免滚动竞态，self-built LB 的 ConfigMap 变更在 `ensureSelfBuiltLB` 内完成并等待收敛，之后才进入 `reconcileWorkloadSystemComponentRepositories()`。

### 5.5 VIP probe

minfo Running 和 AppRelease Ready 只能证明部署完成，不能证明四层流量稳定（DCS 实测过 `net.ipv4.vs.conntrack=0` 时 minfo/AppRelease 全 Ready 但 `VIP:6443` 间歇超时）。因此加一层端到端 probe：

- 复用 `newRemoteRestConfig()` 拿到 workload cluster 的 CA 与认证配置，`rest.CopyConfig` 后把 `Host` 覆盖为 `https://<vip>:<port>`，超时 5 秒。
- 连续执行 5 次 `GET /version`，间隔 2 秒；任意一次失败即视为本轮未 Ready，置 condition=False 并 requeue。
- 使用标准 TLS 校验，不跳过证书校验——这同时验证了 apiserver serving 证书 SAN 包含 VIP。

### 5.6 control-plane 身份变化

alive AppRelease 的 `global.controlPlaneNodeIdentity` 用于在 control-plane 集合变化（节点替换、扩缩容）时触发 alive 重装。alive pod 未在全部 control-plane 节点 Ready 时，provider 按 `<name>/<uid>/<providerID>` 排序拼接生成身份串并 patch 到 workload AppRelease 的 values，然后继续等待。这是 DCS 落地时补的行为，vSphere 沿用。

## 6. 存量与升级行为

- provider 升级后，不带 `controlPlaneLoadBalancer` 的存量集群行为完全不变（外部 LB 或使用方自备入口继续工作）。
- 新建 cluster 可直接使用 `type=internal`。
- 字段为空的 cluster（含全部存量集群）保持空值，不能改用 self-built LB，见第 9 章。
- 字段非空的 cluster 拒绝任何修改，`type=external` 也不能再改成 `internal`。显式写过 `external` 的集群不再有切换入口，这是选择该规则的直接代价。
- `type=internal` 的 cluster 按 provider-managed minfo 继续 reconcile；minfo 所有权不匹配时不接管未知对象。

CRD 变更是纯新增可选字段，存量对象不需要数据迁移；交付仓库的 chart CRD 需要随之重新生成。

## 7. 删除流程

cluster 删除沿用 `reconcileDelete()` 现有流程，不新增 self-built LB cleanup，也不因 alive runtime、VIP、IPVS 或 `ModuleInfo` 状态阻塞 VM / cluster 删除。

provider 不专门删除 alive `ModuleInfo`：非 global minfo 由 cluster-transformer 补 platform Cluster `OwnerReference`，cluster 删除时交给 Kubernetes GC；workload `AppRelease` 走 cluster-transformer 通用 finalizer 链路，workload API 不可访问时不由 provider 单独处理。

VM 删除后 guest OS 内的 keepalived manifest、VIP、IPVS 规则随 VM 消失，provider 不进入 VM 清理。

## 8. 关键设计决策

| 决策 | 结论 | 原因 |
| --- | --- | --- |
| 运行时 | 使用 alive。 | 与 DCS / baremetal 模型一致，复用同一套制品与工程经验；VIP 后端走 IPVS 转发到全部 apiserver，全链路状态可观测。 |
| 安装路径 | provider 创建和 patch alive `ModuleInfo`，不直接建 `AppRelease`。 | 复用插件版本选择与生命周期；删除走 ownerRef + GC。代价是与本仓 kube-ovn 的安装风格不一致，短期接受。 |
| API 形状 | 新增可选 `controlPlaneLoadBalancer`，`type` 默认 `external`。 | 存量 `VSphereCluster` 不带该字段，必须落到「保持现状」语义才能平滑升级；`nil` 就是为兼容 internal VIP 之前的集群而保留的形态。 |
| 可变性 | 只在 CREATE 时可写，创建后一律冻结（含 `nil` 不能写成有值）。 | VIP 配置一旦确定就不允许更改：endpoint、apiserver serving 证书 SAN、guest runtime 全部由它派生，运行中无法就地重新派生；`nil` 允许写入会让存量集群被切到从未准备过的 VIP 上。 |
| 状态表达 | 新增 `SelfBuiltLoadBalancerReady`（v1beta1 + v1beta2）。 | 本仓 condition 约定 + gap 分析 #19 验收要求；与 DCS 的「不加 condition」不同。 |
| bootstrap / runtime 分离 | bootstrap 只提供临时 VIP，runtime 由 alive 接管。 | `kubeadm init` 和 provider 取 workload client 都需要 endpoint 先可达；完整 backend 列表只能在 Node 注册后生成。 |
| bootstrap 载体 | cloud-config write_files + oneshot systemd unit（无 `[Install]`）+ `runcmd` 前插。 | 复用现有 merge 机制；unit 提供同步执行、退出码传递和 journal 可观测性；无 `[Install]` 结构上杜绝开机复活；`runcmd` 前插 + `exit 1` 提供失败阻断。 |
| 内核模块和参数 | 由 alive 前置任务负责，provider 不写入、不持久化、不校验。 | 属于 alive runtime 运行前提，不是 provider 的 VM bootstrap 职责；失败通过 alive readiness 与 VIP probe 暴露。 |
| 依赖 | 平台对象一律 `unstructured`。 | 保持 CAPV 无 alauda 编译期依赖，延续 `modulePluginGVK` 既有做法。 |
| 删除边界 | 不新增 cleanup。 | VM 消失后 guest OS 残留随之消失；minfo / AppRelease 走通用链路。 |

## 9. 存量集群

self-built LB 只在创建阶段选择。存量集群不迁移，也没有接管入口。

### 9.1 默认行为

provider 升级后，不带 `controlPlaneLoadBalancer` 的集群继续使用原入口：provider 不创建 minfo、不修改 `controlPlaneEndpoint`、不改 workload kubeconfig secret 或平台入口、不进入已有 VM 补写 bootstrap VIP。字段保持 `nil`，provider 不回填。

### 9.2 不支持迁移到 alive

字段为空的集群不能改用 self-built LB：webhook 拒绝在 UPDATE 时把 `controlPlaneLoadBalancer` 从 `nil` 写成任何值（见 3.1 可变性）。要用 self-built LB 只能新建集群。

这不是校验上的保守取舍，而是流程上没有安全落点：bootstrap VIP 在第一个 control-plane 节点 `kubeadm init` 期间注入并随后交给 alive 持有，运行中的控制面没有等价的注入时机；且迁移必然改变 endpoint 地址，牵动 apiserver serving 证书 SAN 轮换、workload kubeconfig、管理集群 remote client、平台入口和全部外部客户端。

## 10. 文件变更清单

| 文件 | 变更 |
| --- | --- |
| `apis/v1beta1/vspherecluster_types.go` | 新增 `ControlPlaneLoadBalancer` 类型与 `VSphereClusterSpec.ControlPlaneLoadBalancer` 字段。 |
| `apis/v1beta1/condition_consts.go` | 新增 `SelfBuiltLoadBalancerReady` 的 v1beta1 与 v1beta2 condition / reason 常量。 |
| `apis/v1beta1/zz_generated.deepcopy.go` | `make generate` 生成。 |
| `apis/v1alpha3`、`v1alpha4` 的 conversion | 新增字段在旧版本无对应项，按 restored-fields 约定补 conversion 与 fuzz 测试。 |
| `internal/webhooks/vspherecluster.go`（新增） | `VSphereCluster` 校验 webhook：LB 输入、endpoint 一致性、创建后不可变。 |
| `main.go` | 注册 `VSphereCluster` webhook。 |
| `config/webhook/manifests.yaml`、`config/crd/...` | `make manifests` 更新。 |
| `controllers/vspherecluster_reconciler.go` | `reconcileNormal` 接入 `ensureSelfBuiltLB`；新增 self-built LB GVK 常量。 |
| `controllers/vspherecluster_selfbuiltlb.go`（新增） | 主流程、前置对象校验、版本解析、minfo create/patch、readiness gate、VIP probe、kube-proxy IPVS 配置、condition 设置。 |
| `pkg/context/vm_context.go` | `VMContext` 新增 `SelfBuiltLB *SelfBuiltLBBootstrap`。 |
| `controllers/vspherevm_controller.go` | 构造 VMContext 时解析 `VSphereCluster` 的 LB 配置并填充。 |
| `pkg/services/govmomi/service.go` | `reconcileBootstrapUserData()` 增加 bootstrap VIP 合并分支；抽出 init/join 节点判定 helper。 |
| `pkg/util/machines.go` | 新增 `GetBootstrapVIPCloudConfig()`，与 `GetPersistentDiskCloudConfig()` 同风格。 |
| `config/rbac/role.yaml` + kubebuilder marker | 管理集群新增 `moduleinfoes`（含 `moduleinfoes/status`）、`moduleplugins`、`moduleconfigs`、`clustermodules`、platform Cluster、clusterregistry Cluster 权限。 |
| `main.go` / provider chart values | 新增启动参数 `--plugin-alive-version`。 |
| `controllers/*_test.go`、`internal/webhooks/*_test.go`、`pkg/util/machines_test.go` | 覆盖 bootstrap VIP、minfo、readiness、kube-proxy、webhook、RBAC/CRD schema。 |

依赖与交付制品：

- Go 依赖不变：平台对象一律 `unstructured`，不引入 `gomod.alauda.cn`。
- 管理集群 RBAC：provider 需要读写 alive `ModuleInfo`，读取 `ModulePlugin`、`ModuleConfig`、`ClusterModule`、platform Cluster、clusterregistry Cluster。
- vSphere 离线包：包含 alive 插件制品（`ModulePlugin/alive`、`ModuleConfig/alive-<version>`、chart 内容、installer 与 keepalived 镜像）。
- 交付仓库的 chart CRD 需要随 `config/crd` 一起重新生成同步。
- 使用方 cluster 模板：`type=internal` 时把 `CONTROL_PLANE_ENDPOINT_IP` 同时写入 `controlPlaneEndpoint` 和 `controlPlaneLoadBalancer.host`，模板本身不引入任何 VIP 组件。

## 11. 边界情况与风险

| 场景 | 行为 | 处理 |
| --- | --- | --- |
| 字段缺省 | 完全保持现状。 | 存量集群升级后的默认形态。 |
| `type=external` | 只校验并回填 endpoint，不创建 minfo，不注入 bootstrap VIP。 | 使用方自备 LB 语义。 |
| endpoint 与 VIP 不一致 | kubeadm 与实际 VIP 分叉。 | webhook 拒绝；reconcile 再做防御性校验并返回错误。 |
| VIP 与槽位 IP 冲突 | internal LB 无法工作。 | webhook 拒绝；`ensureSelfBuiltLB` 防御性校验。 |
| VIP 落在 IPAM pool 范围内 | 后续节点可能拿到 VIP。 | provider 读不到外部 IPAM pool 范围，只能作为环境前置条件要求，并在文档与用例中说明。 |
| control-plane Node 未全部注册 | alive backend 列表不完整。 | 复用 `controlPlaneNodesRegistered()`，未就绪时 requeue。 |
| 平台前置对象缺失 | minfo 渲染不出 AppRelease。 | 缺 platform Cluster / clusterregistry Cluster / `ClusterModule` 时 requeue；缺 `ModulePlugin` / `ModuleConfig` 时返回错误。 |
| `ModulePlugin/alive` affinity 只匹配 Baremetal | 自动安装路径不覆盖 vSphere。 | provider 显式创建 minfo，不依赖 affinity。 |
| 同名 minfo 不是 provider-managed | 存在接管风险。 | 校验所有权 label，不匹配时返回错误。 |
| minfo status 滞后 | 误判 Ready，或 phase 长期不 `Running` 导致永远不 Ready。 | 只比对 `status.version == spec.version`，phase 不阻塞。 |
| minfo Running 但 VIP API 不稳定 | 部署状态不能证明四层流量稳定。 | 连续 5 次 `/version` probe。 |
| alive 内核模块或 sysctl 缺失 | NAT 模式间歇超时。 | 由 alive 前置任务保障；provider 只通过 readiness 和 VIP probe 暴露。 |
| VIP/GARP 或 VRRP 组播被网络阻断 | VIP 不可达，或 keepalived 无法选主导致多主 / 无主。 | 目标环境 preflight（TC-00）；provider 侧表现为 alive pod 或 VIP probe 长期不 Ready。 |
| kube-proxy IPVS 干扰 VIP | kube-proxy 可能代理或宣告 VIP。 | IPVS 模式下设置 `strictARP=true` 与 `excludeCIDRs=<vip>/32`。 |
| 多网卡节点 | 自动探测可能选错接口。 | 用户填写 `interface`，provider 同时用于 bootstrap VIP 和 minfo config。 |
| bootstrap VIP 脚本失败 | VIP 不可达。 | `runcmd` 前插条目 `exit 1` 阻断 kubeadm，失败在 Machine / cloud-init 日志可见。 |
| 首台节点在 alive Ready 前重启 | VIP 不重建，集群失去 apiserver 入口且无法自愈。 | 不在支持范围（非目标）；窗口内重启属运维异常，人工重新 `systemctl start` 恢复。 |
| 节点原地重启 | 若 unit 能开机自启会与 keepalived 争抢同一地址。 | unit 无 `[Install]` 段，systemd 结构上无法开机拉起；TC-04 验证。 |
| control-plane 节点替换 | backend 需要更新。 | alive valuesTemplate 重新生成 `masterIPs`；provider patch `global.controlPlaneNodeIdentity` 触发重装。 |
| workload API 在删除时不可访问 | 无法通过通用链路卸载 AppRelease。 | 不因此阻塞删除；workload cluster 随 VM 消失。 |
| 字段已写入后再 patch LB 字段 | endpoint、证书、bootstrap 和 alive runtime 会不一致。 | webhook 一律拒绝，无放行入口；写错只能重建 cluster。 |
| 创建时 `interface` 留空、自动探测选错网卡 | VIP 落在非预期接口。 | 不能靠 patch 纠正（字段已冻结）；多网卡环境要求创建时显式填写 `interface`。 |
| IPv6 / dual-stack | 本设计未覆盖。 | webhook 对 internal `host` 只接受 IPv4。 |

## 12. 实施步骤

1. 新增 `ControlPlaneLoadBalancer` 类型与 `VSphereClusterSpec` 字段，补 conversion 与 deepcopy。
2. 新增 `SelfBuiltLoadBalancerReady` 的 v1beta1 / v1beta2 condition 与 reason 常量。
3. 新增 `internal/webhooks/vspherecluster.go`，按现有 `CustomValidator` 模式接入 manager 并注册 webhook manifest。
4. 扩展 RBAC：`moduleinfoes`、`moduleplugins`、`moduleconfigs`、`clustermodules`、platform Cluster、clusterregistry Cluster。
5. `pkg/util` 新增 `GetBootstrapVIPCloudConfig()`，并抽出 init/join 节点判定 helper。
6. `VMContext` 新增 `SelfBuiltLB` 字段，`vmReconciler` 解析填充。
7. `reconcileBootstrapUserData()` 增加 bootstrap VIP 合并分支，只对 `kubeadm init` control-plane 节点生效。
8. 新增 `controllers/vspherecluster_selfbuiltlb.go`：VIP 冲突校验、kube-proxy IPVS 配置、前置对象校验、版本解析、minfo create/patch。
9. 补 readiness：minfo version、AppRelease Sync+Health、control-plane alive pod、连续 VIP probe、`controlPlaneNodeIdentity` patch。
10. `reconcileNormal` 接入 `ensureSelfBuiltLB` 并设置 condition。
11. 新增启动参数 `--plugin-alive-version` 与 chart value。
12. 补单元测试与 envtest：bootstrap VIP cloud-config、minfo create/patch、readiness gate、kube-proxy、webhook、创建后冻结规则（含拒绝写入空字段）。
13. 执行 `make generate`、`make manifests`，同步交付仓库 chart CRD。
14. 在 vSphere 测试环境先跑网络 preflight（VRRP 组播、VIP/GARP、IPVS 内核），再创建 3 control-plane 的 `type=internal` 集群，验证 bootstrap init、control-plane join、alive minfo、VIP API、节点替换和删除流程。
