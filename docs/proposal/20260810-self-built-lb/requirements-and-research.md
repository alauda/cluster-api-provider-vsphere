# vSphere 自建 LB 需求与调研

本文记录 CAPV govmomi 模式支持 provider-managed control-plane LB 的需求、现状调研、选型判断和待确认问题。设计见 [design.md](design.md)，验证用例见 [test-cases.md](test-cases.md)。

参考实现是 DCS provider 的同名需求（AIT-66125，`cluster-api-provider-dcs` 的 `docs/20260610-self-built-lb/` 与 MR 47）。DCS 已确认的通用结论本文直接引用，不重复论证；本文只展开 vSphere 侧不同的部分。

核心结论：

1. vSphere 侧没有可直接复用的 vCenter 原生四层 LB API，`type=internal` 只能走 provider-managed self-built LB，与 DCS 结论一致。
2. **vSphere 与 DCS 的现状差异在 API 表面**：DCS 已有 `controlPlaneLoadBalancer` 字段只是没有实现，CAPV 需要新增整个字段。govmomi 模式当前没有任何 provider-managed VIP，多 control-plane 集群依赖使用方自备外部 LB；self-built LB 的实质是把 VIP 生命周期建立在 `VSphereClusterReconciler` 上。
3. self-built LB 选择 alive，由 provider 创建、patch 和等待管理集群的 alive `ModuleInfo`，不直接创建 workload `AppRelease`；与 DCS 保持一致。
4. CAPV 的 bootstrap 通道是 cloud-init cloud-config，不是 Ignition；但仓库已有成熟的 userdata 改写与合并机制，bootstrap 临时 VIP 的注入成本低于 DCS。
5. 存量外部 LB 集群不迁移到 self-built LB：provider 升级不自动迁移，webhook 也拒绝在 UPDATE 时写入该字段。要启用只能新建集群，理由见 design.md 9.2。

相关上下文：

| 项 | 内容 |
| --- | --- |
| CAPV 仓库 | 本文所在仓库 `github.com/alauda/cluster-api-provider-vsphere`，基线分支 `dev/v1.13.1`。 |
| DCS 对照实现 | `gitlab-ce.alauda.cn/ait/cluster-api-provider-dcs`，`release-1.0` 的 `docs/20260610-self-built-lb/`，实现见 MR 47。 |
| baremetal 对照实现 | DCS 调研已记录，本文不重复；结论是「两阶段 bootstrap VIP + runtime alive」模型可复用。 |
| 已有规范差距记录 | `docs/proposal/20260725-acp-provider-standard-alignment/requirements-and-research.md` 的 P2-2（#19），当时标记「⏸ 暂缓，待平台级自建 VIP 统一方案」。本需求即该统一方案在 vSphere 的落地。 |
| 当前方案范围 | 主路径只覆盖 cluster creating 阶段的 `type=internal`；存量集群不在范围内，也不提供迁移路径。 |

## 1. 背景

CAPI / kubeadm 使用 `Cluster.spec.controlPlaneEndpoint` 作为 kube-apiserver 入口。多 control-plane 场景下该入口不能等同某一台 master IP，否则 master join、客户端访问和故障切换都依赖单点。

CAPV govmomi 模式当前没有解法：模板只把 `${CONTROL_PLANE_ENDPOINT_IP}` 写进 `controlPlaneEndpoint`，谁来持有这个地址完全交给使用方——要么自备外部 LB，要么单 control-plane 直接用节点 IP。

这不满足 provider 规范：

- `VSphereClusterSpec` 没有表达 LB 意图的字段，provider 不知道 VIP 是什么、由谁持有。
- VIP 的创建、校验、就绪判断、清理都不在 `VSphereClusterReconciler` 里，Cluster 层面没有任何 condition 能表达 endpoint / LB 是否可用。
- 入口形态由使用方自行决定，provider 无法保证同一产品形态下的一致性。

目标是让 `spec.controlPlaneLoadBalancer.type=internal` 真正代表 provider-managed self-built LB：

1. cluster creating 阶段 VIP 已可访问，首个 control-plane 能完成 `kubeadm init`，后续 control-plane 能通过 VIP join。
2. control-plane 节点 Ready 后，VIP 可在 control-plane 节点间漂移。
3. VIP 后端能转发到多个 kube-apiserver，且后端池随 control-plane 节点替换、扩缩容自动维护。
4. LB 就绪状态可以通过 `VSphereCluster` condition 表达。
5. 删除 cluster 不因为 self-built LB 状态额外阻塞。

## 2. CAPV 当前状态

### 2.1 API 表面

`VSphereClusterSpec`（`apis/v1beta1/vspherecluster_types.go`）只有 `ControlPlaneEndpoint APIEndpoint`，**没有** `controlPlaneLoadBalancer`。

这与 DCS 的起点不同：DCS 已有 `host` / `port` / `type` / `vrid` 字段，只是没有实现；CAPV 需要新增整个字段。带来两点约束：

- 新字段必须是可选的，缺省时行为与今天完全一致，否则存量 `VSphereCluster` 无法平滑升级。
- `type` 不能像 DCS 那样设成必填，nil 与 `external` 都要落到「保持现状」语义。

supervisor 模式（`apis/vmware/v1beta1`）另有 `LoadBalancerReady` condition 和 `kube-system/kube-apiserver-lb-svc` 服务发现路径，属于 vSphere with Tanzu 的独立形态，不在本需求范围。

### 2.2 当前实际行为

| 路径 | 当前行为 | LB 结论 |
| --- | --- | --- |
| `clusterReconciler.reconcileNormal()` | 协调 failure domain、vCenter 连通性、ClusterModule、kube-ovn AppRelease、系统组件镜像仓库。 | 全流程不涉及 VIP / LB。 |
| `controlPlaneEndpoint` | 由使用方模板或用户直接写入 `VSphereCluster.spec`。 | provider 不校验、不回填、不保证与真实 VIP 一致。 |
| `VMService.reconcileBootstrapUserData()` | VM 上电前改写 guestinfo userdata：kubeadm nodeRegistration、持久盘 cloud-config、kubelet serving 证书。 | 已具备注入能力，但没有任何 VIP 相关内容。 |
| `clusterReconciler.reconcileDelete()` | 等待 VSphereMachine 删除、回收 ClusterModule、摘除 identity secret finalizer。 | 不涉及 VIP / LB 清理。 |

### 2.3 能力边界

| 能力项 | 当前状态 |
| --- | --- |
| 表达 control-plane LB 意图 | 无，只有 `controlPlaneEndpoint`。 |
| endpoint 与真实 VIP 一致性校验 | 无。 |
| vCenter 原生 LB 创建 | 无，且平台不提供（见第 3 章）。 |
| 创建阶段 VIP 可达 | 无。多 control-plane 集群要求使用方在集群创建前自备可达入口。 |
| guest OS 层 LB 组件部署 | 无 provider 侧路径。 |
| workload AppRelease / ModuleInfo 方式部署 LB | 无；但 kube-ovn 已有 provider 直接创建 AppRelease 的先例（`reconcileKubeOvnAppRelease()`）。 |
| control-plane backend pool 更新 | 无。由使用方的外部 LB 自行维护，provider 不参与。 |
| LB 就绪 condition | 无（gap 分析 #19 记录项）。 |

## 3. vSphere 平台 LB 能力调研

### 3.1 vCenter 没有可复用的工作负载 LB API

govmomi / vCenter API 提供的是计算、存储、网络端口组和 DRS 相关对象，不提供 TCP listener、backend pool、health monitor 这类四层 LB 对象。与 DCS 侧 FusionCompute 的调研结论一致：文档中出现的「load balancing」多数属于 DRS 资源调度语义，不是网络 LB。

vSphere 生态里能提供真实四层 LB 的是 NSX Advanced Load Balancer（Avi）或 NSX-T LB，但它们是独立产品：需要单独部署控制器、单独的凭据体系和许可，且不在 CAPV govmomi 模式的依赖范围内（CAPV 只持有 vCenter 会话）。把它作为 `type=internal` 的实现会把产品依赖从「一套 vCenter」扩大到「vCenter + NSX ALB」，不符合当前交付形态。

结论：`type=internal` 只能走 guest / Kubernetes 层自建 VIP。

### 3.2 vSphere 侧的网络与内核前提

alive 有三项运行前提，都需要在目标环境 preflight。与 DCS / baremetal 不同的是，**这三项在 vSphere 现网都没有既有运行证据**：govmomi 模式此前没有任何自建 VIP 组件在跑，因此不能从「现有集群正常」推导出「alive 一定正常」。

| 项 | 说明 | 现状 |
| --- | --- | --- |
| 同 L2 的 VIP 与 GARP | VIP 是节点主网卡上的附加 `/32` 地址，靠 GARP 更新上游邻居表。若环境强制 IP/MAC 绑定或 ARP 抑制，GARP 会失效。 | 待实测。VIP 与节点 MAC 相同，通常不需要开启 dvPortGroup 的「MAC 地址更改」和「伪传输」，但 NSX-T 安全策略可能拦截。 |
| VRRP 组播（protocol 112，`224.0.0.18`） | keepalived 选主依赖。被阻断时会出现多主或无主。 | 待实测。标准 vSwitch / dvPortGroup 默认不阻断组播，但 NSX-T 分布式防火墙或安全策略可能拦截。 |
| IPVS 内核前提 | alive NAT / IPVS 模式需要 `ip_vs` 系列模块与 `net.ipv4.vs.conntrack=1`；未满足时 minfo 和 AppRelease 都可以 Ready，但 `VIP:6443` 会间歇性超时。DCS 侧已实测确认该现象。 | 由 alive 前置任务保障，provider 不注入、不持久化、不校验。长期前提是 VM 模板默认加载所需模块。 |

这三项构成 [test-cases.md](test-cases.md) 中 TC-00 的内容，是所有后续用例的前置。

### 3.3 IP 分配模型差异

DCS 的 VIP 冲突校验对象是 `DCSIpHostnamePool` 分配出来的 `DCSMachine.status.networkConfig.ip`。CAPV 没有等价的动态池，IP 有两个来源：

- `VSphereMachineConfigPool` 的静态槽位：`spec.configs[].network.primary.ip` 和 `network.additional[].ip`，由使用方预先声明。
- CAPI IPAM：`vspherevm_ipaddress_reconciler.go` 走 `IPAddressClaim`，地址范围由外部 IPAM provider 的 pool 决定。

因此 VIP 冲突校验只能覆盖第一类（provider 可读的静态槽位），IPAM 池范围 provider 读不到，只能作为环境前置条件要求 VIP 落在池外。这一点比 DCS 弱，需要在文档和用例里说清楚。

## 4. bootstrap 通道差异

DCS 走 Ignition，通过 `pkg/ignition/clc` 合并 CLC 片段，并用 `systemd` unit 顺序（`Before=kubeadm.service`）保证 VIP 先于 kubeadm 生效。

CAPV 走 cloud-init cloud-config，机制不同但能力齐备：

| 能力 | CAPV 对应实现 |
| --- | --- |
| 改写 bootstrap userdata 的时机 | `VMService.reconcileBootstrapUserData()`：VM clone 之后、上电之前，`powerState == PoweredOff` 且 `format == CloudConfig` 时改写 guestinfo。 |
| 合并额外 cloud-config | `util.MergeCloudConfigUserData()`，已被持久盘和 kubelet serving 证书两条链路使用。 |
| 保证注入内容先于 kubeadm 执行 | `mergeCloudConfigBodies()` 对 `runcmd` 使用 `mergeSequenceBefore()`，额外条目**前插**到 CABPK 生成的 kubeadm 命令之前。 |
| 幂等 / 覆盖 | `mergeWriteFiles()` 按 `path` 去重覆盖，重复 reconcile 不会追加重复条目。 |
| 区分 init 节点与 join 节点 | `UpdateKubeadmNodeRegistration()` 已按 write_files 路径分支：`/run/kubeadm/kubeadm.yaml` 是 init，`/run/kubeadm/kubeadm-join-config.yaml` 是 join。判定比 DCS 的命令行字符串匹配更稳。 |
| 节点主 IP / hostname | `resolveNodeIdentity()` + `util.GetPrimaryNodeIPAddress()` 已解析，可直接作为 VIP 绑定网卡的探测依据。 |

结论：vSphere 的 bootstrap 临时 VIP 不需要新增渲染框架，只需要新增一段 cloud-config 片段和一个上下文字段。

一个 cloud-init 特有的行为需要利用：cloud-init 把全部 `runcmd` 条目拼进同一个 shell 脚本顺序执行，因此前插条目里的 `exit 1` 会终止整个 `runcmd`，kubeadm 不会执行，CAPI 的 bootstrap 成功哨兵文件也不会生成。这提供了与 DCS `Requires=/After=` 等价的「VIP 失败即阻断 kubeadm」能力。

## 5. 其他 provider 对照

DCS provider MR 47 已完整落地本模型，可直接复用的工程结论：

- 两阶段拆分：bootstrap 只提供临时 VIP，runtime 由 alive 接管，中间靠 alive installer 首次写 keepalived manifest 前的 `clear_vip()` 交接。
- alive minfo 的最小输入是 `vip` / `vrid` / `apiserverPort` / `httpPort` / `httpsPort` / `extraPorts` / `interface`；**不写 `masterIPs`**，backend 由 alive valuesTemplate 从 workload control-plane Node `InternalIP` 生成。
- 版本解析优先级：provider 启动参数 → `ModulePlugin.status.targetClusterVersions[ClusterModule.spec.version]` → `ModulePlugin.status.latestVersion`，再校验 `ModuleConfig/alive-<version>.status.readyForDeploy`。
- readiness 分层：minfo `status.version` 追平 → minfo phase Running → workload AppRelease `Sync` + `Health` → 每个 control-plane 节点上 `kube-system` `app=alive` pod Ready → 连续 5 次 `https://<vip>:<port>/version` probe。
- 落地过程中补的两个坑：alive AppRelease 需要 `global.controlPlaneNodeIdentity`，control-plane 身份变化（节点替换）时要 patch 触发重装；minfo status 有滞后，需要先比对 `status.version` 再判 phase。
- kube-proxy IPVS 模式下必须 `ipvs.strictARP=true` 且 `excludeCIDRs` 含 `<vip>/32`，改的是 `kube-system/kube-proxy` ConfigMap 的 `config.conf`，改完等待 DaemonSet 滚动完成。

CAPV 侧不能照搬的部分：

| DCS 做法 | CAPV 处理 |
| --- | --- |
| 直接依赖 `gomod.alauda.cn/operator-runtime` 的 `ModuleInfo` / `ModuleConfig` / `ModulePlugin` 类型 | CAPV 模块路径是 `sigs.k8s.io/cluster-api-provider-vsphere`，当前无任何 alauda 依赖，且已有 `modulePluginGVK` 用 `unstructured` 读取的先例。继续用 `unstructured`，不引入编译期依赖。 |
| `minfov1alpha1.GenerateModuleInfoName()` | 该函数是 `sha256(clusterName:moduleName:displayName)` 取前 32 位 hex，拼成 `<clusterName>-<hash>`。CAPV 侧自行实现同算法，保证与平台生成的名字一致。 |
| Ignition CLC 模板 + systemd unit 顺序 | cloud-config write_files + `runcmd` 前插，见第 4 章。 |
| 从 `DCSMachine.status.networkConfig` 校验 VIP 冲突 | 从 `VSphereMachineConfigPool` 静态槽位校验，见 3.3。 |
| 不新增 condition | CAPV 新增 `SelfBuiltLoadBalancerReady`（v1beta1 + v1beta2 双写），理由见 design.md 第 3.2 节。 |

## 6. 选型结论

vSphere self-built LB 选择 alive，运行阶段通过 `ModuleInfo` 安装和升级，不由 provider 直接创建 workload `AppRelease`。

理由：

1. 与 DCS、baremetal 的 self-built LB 模型一致，可复用两阶段工程经验和同一套 alive 制品。
2. 复用平台 `ModulePlugin` / `ModuleInfo` / AppRelease 生命周期，版本选择和升级不需要 provider 自研。
3. alive 已覆盖 keepalived + IPVS + kube-lock + installer/chart，provider 不需要自研四层转发配置管理。
4. VIP 后端是 IPVS 转发到全部 apiserver 而非单点承载，且全链路状态可被 provider 观测并落到 condition，满足 gap 分析 #19 的验收要求。

关于安装路径，本仓已有的 kube-ovn 是 provider 用 dynamic client 直接在 workload cluster 创建 `AppRelease`。alive 不沿用该做法，选 `ModuleInfo`：

| | 直接建 AppRelease（kube-ovn 现状） | ModuleInfo（alive 选择） |
| --- | --- | --- |
| 版本来源 | provider 自行读 `ModulePlugin.status.latestVersion` 并拼 chart 名 | 复用插件版本解析与 `ModuleConfig` ready 校验 |
| 升级 | provider 负责 | 插件生命周期负责 |
| 删除 | provider 或随集群消失 | platform Cluster `OwnerReference` + GC，cluster-transformer 通用 finalizer |
| 与平台一致性 | 绕过插件体系 | 与 DCS 及平台其它插件一致 |

代价是 provider 内部出现两种插件安装风格。这是已知的不一致，短期接受，kube-ovn 是否收敛到 minfo 不在本需求范围。

`type=external` 和字段缺省仍代表使用方自备入口，是需要兼容保留的既有语义。

## 7. alive minfo 落地判断

可行。判断依据与 DCS 一致：alive chart 与 minfo 输入已具备能力，vSphere 不需要先接入 provider-alauda / cluster-transformer 的自动插件安装链路，provider 显式创建 minfo 即可绕开 `ModulePlugin/alive` 当前只匹配 Baremetal 的 affinity 限制。

vSphere 侧需要新增的能力：

- 新增 `spec.controlPlaneLoadBalancer` 字段与 `VSphereCluster` 校验 webhook（本仓目前没有 `VSphereCluster` webhook，只有 `VSphereClusterTemplate`）。
- 在 `reconcileBootstrapUserData()` 注入 bootstrap 临时 VIP。
- 在 `reconcileNormal()` 接入 `ensureSelfBuiltLB`：前置对象校验、版本解析、minfo create/patch、readiness gate、VIP probe、kube-proxy IPVS 配置。
- 新增 `SelfBuiltLoadBalancerReady` condition（v1beta1 与 v1beta2 两套）。
- 管理集群 RBAC：`moduleinfoes`、`moduleplugins`、`moduleconfigs`、`clustermodules`、platform Cluster、clusterregistry Cluster。

## 8. 风险与待确认问题

平台前置对象：

- 通用渲染前置：目标环境需要具备 platform Cluster、clusterregistry Cluster、`ClusterModule` 和可用的 workload client，否则 provider 创建的 minfo 渲染不出 AppRelease。缺失时按环境前置条件不满足处理，provider 不合成这些对象。
- alive 制品：需要确认 alive `ModulePlugin`、对应 `ModuleConfig`、chart 与 installer / keepalived 镜像进入 vSphere 交付的离线包，并明确版本来源。

vSphere 网络与节点：

- **VIP/GARP 与 VRRP 组播是否放行**：govmomi 现网此前没有自建 VIP 组件在跑，两者都没有既有运行证据。这是 vSphere 侧最高优先级的实测项。
- IPVS 内核前提（`ip_vs` 系列模块、`net.ipv4.vs.conntrack=1`）是否由 VM 模板或 alive 前置任务覆盖，且节点重启后仍生效。
- VIP 预留：VIP 必须不在 `VSphereMachineConfigPool` 槽位 IP 中，也不在 IPAM pool 范围内。后者 provider 读不到，只能作为环境要求。
- 网卡选择：多网卡节点上 VIP 应绑定哪张网卡，是固定为主网卡还是由用户显式指定，决定 `interface` 字段的默认策略。

交付和运维：

- 交付使用的 cluster 模板由谁维护，`type=internal` 时如何把 `CONTROL_PLANE_ENDPOINT_IP` 同时写入 `controlPlaneEndpoint` 和 `controlPlaneLoadBalancer.host`，需要与模板维护方确认。
- Service type=LoadBalancer：alive 只承担 control-plane VIP，不提供 svc VIP 选举。若交付形态需要该能力，要单独找方案，不在本需求范围。
- IPv6 / dual-stack：本方案按 IPv4 `/32` 展开，IPv6 需单独设计。
- CRD 变更：新增字段是可选字段，存量对象无需迁移；交付仓库的 chart CRD 需同步重新生成。

这些问题不影响 alive 作为 vSphere self-built LB 的选型结论，但需要在落地前确认清楚。
