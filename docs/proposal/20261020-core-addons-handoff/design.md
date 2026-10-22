# 设计：kube-ovn 和 CoreDNS 交给平台管理

平台 v4.5 起，kube-ovn 和 CoreDNS 由平台通过 ModuleInfo 安装、由 CVO 随平台升级，不再由 provider 写 AppRelease、改 CoreDNS。方案与参考实现见 `gitlab-ce:gangwang/cluster-management-docs` 的 `docs/cni-coredns-decouple/`（AIT-74888、AIT-74887），尤其是 `04-provider-implementation.md` 的移植清单。

本文只记录 vSphere 的落点。交接逻辑与 DCS（`ait/cluster-api-provider-dcs!64`，已在 FusionCompute 上验证）、HCS（`ait/cluster-api-provider-hcs!33`）一致，差异见最后一节。

## 当前行为

- kube-ovn 网络由 Cluster 注解 `cpaas.io/network-type: kube-ovn` 开启。provider 在工作负载集群里写 `cpaas-system/cni-kube-ovn` AppRelease：
  - 版本取 `cpaas.io/kube-ovn-version`，没有时取 kube-ovn ModulePlugin 的 `status.latestVersion`；
  - 网卡固定为 `eth0`；
  - values 带 `controlPlaneNodes`；
  - ovn-central 渲染短了时写 `renderRevision` 重新渲染（#18）。
- CoreDNS 由 `kubeadm init` 安装。provider 给 KCP 写 `dns.imageRepository` 和两个 skip 注解，控制面滚动完成后改工作负载集群里 CoreDNS Deployment 的镜像仓库并补拉取凭据。

## 本需求改动点

| 场景 | vSphere 的处理 |
| --- | --- |
| 新集群（ProductBase ≥ v4.5，kube-ovn 网络） | 在 `VSphereCluster.status.ready` 之前给 KCP 写 `initConfiguration.skipPhases: [addon/coredns]` 和 `skip-coredns` 注解；KCP 还不存在时不置 ready。KCP 的 create 事件会触发 reconcile |
| 交接时机 | `reconcileKubeOvnAppRelease` 里，`controlPlaneNodesRegistered` 报告 KCP 的控制面 Node 全部注册之后、写 AppRelease 之前。没注册齐时 `CoreAddonsHandedOff` 记 `CoreAddonsHandoffPending` |
| 交接前置条件 | ClusterModule ≥ v4.5，且与 ProductBase 的 spec、status 一致；已有的 `cni-kube-ovn` 用 `acp/chart-kube-ovn`、网络字段（含网卡）与平台渲染一致、版本不高于平台目标、sentry 已处理（`observedGeneration`、`installedRevision`）且 Sync/Health 为 True；kubeadm CoreDNS 的 Corefile 未定制、`kube-dns` 地址与 chart 一致；Global 接管已有 kube-ovn 时还要求节点全部 Ready、ovn-central 就绪；接管已有 kube-ovn 时 KCP 和 MachineDeployment 没有在滚动 |
| 交接动作 | 给 kubeadm 的 `kube-system/coredns` SA 补 sentry 拉取凭据；一次 patch 写 `kube-ovn.cpaas.io/join-cidr`、`kube-ovn.cpaas.io/network-device`、`cpaas.io/network-type`，最后写标签 `cni-type: kube-ovn` |
| 交接后 | 不再写 `cni-kube-ovn`（包括 `renderRevision` 重渲染，交接后的短渲染由 kubeadm provider 的 rollout gate 处理）；不再写 KCP 的 `dns.imageRepository`；不再改 CoreDNS Deployment。保留 KCP 的两个 skip 注解、kube-proxy 和 CPI 的仓库、自建 LB、删控制面机器时踢出 OVN raft |
| 交接后的 `KubeOvnAppReleaseReady` | 只读平台写的 `cni-kube-ovn`（同名），报告它是否 Sync/Health。自建 LB 排在 kube-ovn 之后，仍然要等 CNI 就绪 |
| 新集群的 kube-ovn | ClusterModule 已是 v4.5 及以上时，provider 不再新建 `cni-kube-ovn`，由平台在交接后安装 |
| 低于 v4.5 | `CoreAddonsHandedOff=False/CoreAddonsHandoffNotApplicable`，保持现有行为 |

`CoreAddonsHandedOff` 与其他 Alauda condition 一样 v1beta1、v1beta2 双写，加进 v1beta2 owned conditions，不参与 Ready 汇总。reason：
- `CoreAddonsHandedOff`
- `CoreAddonsHandoffPending`
- `CoreAddonsHandoffBlocked`
- `CoreAddonsHandoffNotApplicable`
- `CoreDNSSkipPhasePending`

## 网卡

- provider 渲染的 `IFACE` 和 `vlan.VLAN_INTERFACE_NAME` 固定为 `eth0`。平台模板从注解 `kube-ovn.cpaas.io/network-device` 渲染这两项，没有注解时为空，kube-ovn 会按节点 IP 自选网卡。
- 交接时集群上没有这个注解就写 `eth0`，平台渲染出与原来相同的网卡。
- 集群上已有注解时保留它，并检查现有 `cni-kube-ovn` 的网卡与它一致；不一致时阻断交接，否则平台会把 kube-ovn 换到注解指定的网卡。
- provider 写的 `func.ENABLE_OVN_LB_PREFER_LOCAL`、`func.LS_CT_SKIP_DST_LPORT_IPS`、`NODE_LOCAL_DNS_IP` 与平台模板的默认值一致，不需要注解。

## 与 DCS、HCS 的差异

- **kube-ovn 开关**：vSphere 用 Cluster 注解 `cpaas.io/network-type`，不是 infra cluster 上的字段。交接写的也是这个注解，值不变。
- **依赖**：仓库不引入 alauda 的 Go 模块。ProductBase、ClusterModule、ModulePlugin 和 AppRelease 都按 `unstructured` 读；管理集群的 client 不缓存 unstructured 对象，所以 `productbases` 只需要 `get`。
- **就绪的暂缓位置**：`reconcileNormal` 先处理 vCenter、failure domain、cluster module，最后才置 `status.ready`。skip 写在最前面（紧跟 KCP 的 DNS 设置），暂缓放在置 ready 之前。
- **交接后的 kube-ovn 状态**：DCS、HCS 交接后直接不再看 kube-ovn；vSphere 的自建 LB 排在 CNI 之后，所以继续只读地报告平台的 `cni-kube-ovn`。
- **短渲染修复**：vSphere 自带的 `renderRevision` 重渲染只在交接前生效；DCS 的同类修复（!58）还没合入，HCS 的在 `fix/AIT-74290-kube-ovn-short-render`。
- **网卡**：与 HCS 相同，DCS 不渲染网卡。

## 代码

- `controllers/vspherecluster_core_addons.go`：交接逻辑。
- `controllers/vspherecluster_reconciler.go`：`reconcileNormal`（skip 与暂缓 ready）、`reconcileKubeOvnAppRelease`（交接、交接后只读、新集群不建 AppRelease）、`patch`（owned condition）。
- `controllers/vspherecluster_system_components.go`：交接后不写 `dns.imageRepository`，不改 CoreDNS Deployment。
- `apis/v1beta1/condition_consts.go`、`vspherecluster_types.go`：condition 和 reason；CRD 只多了 condition 列表描述里的一个名字。
- RBAC：`controllers/vspherecluster_controller.go` 加 `productbases get`，`config/rbac/role.yaml` 用仓库钉的 controller-gen v0.17.0 生成。打包仓库 `gitlab-ce:ait/cluster-api-provider-vsphere` 的 `chart/templates/rbac.yaml` 也要加这条，另开 MR。

## 测试

单测在 `controllers/vspherecluster_core_addons_test.go`，用 fake client 直接调交接相关的函数：
- skip 何时写入、readiness 何时暂缓；
- 交接的每项前置条件和阻断，包括新建的 Global 在节点 Ready 之前交接；
- 网卡：写入、保留、不一致时阻断，VLAN 网卡同样检查；
- 交接后的 `KubeOvnAppReleaseReady` 只读平台的 AppRelease；
- CoreDNS SA 的拉取凭据；
- 交接后不写 CoreDNS 和 KCP DNS；
- 新集群交给平台安装；
- 辅助函数。

`reconcileNormal` 和 `reconcileKubeOvnAppRelease` 里的接线需要 vCenter 和工作负载集群的 kubeconfig，单测覆盖不到，只覆盖到它们调用的函数。
