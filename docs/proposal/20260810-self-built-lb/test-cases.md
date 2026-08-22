# 测试用例：vSphere self-built LB

本文给出 CAPV 支持 `VSphereCluster.spec.controlPlaneLoadBalancer.type=internal` 后的 E2E / 运维验证用例。设计见 [design.md](design.md)。

本文只保留真实环境的 E2E 和运维验证；单元测试、envtest 等代码级测试不在此展开。基础拓扑为 3 control-plane + 1 worker（`KubeadmControlPlane.spec.replicas=3`，一个 `MachineDeployment.spec.replicas=1`）。

## 0. 通用前置

- 管理集群已部署包含本需求实现的 CAPV controller，`VSphereCluster` CRD、webhook、RBAC 和交付 chart CRD 已更新。
- vCenter 凭据、datacenter、datastore、网络（PortGroup / dvPortGroup）、VM 模板等基础参数可用。
- 测试 VIP 与 control-plane 节点在同一二层网段，已通过 `arping -D` 或等价方式确认未被占用。
- VIP 不在 `VSphereMachineConfigPool` 任一槽位 IP 中，也不在 IPAM pool 范围内。
- 管理集群存在 cluster-transformer 通用渲染链路所需的 platform Cluster、clusterregistry Cluster、`ClusterModule`，且 workload client 可用。
- 管理集群或离线包已包含 alive `ModulePlugin`、对应 `ModuleConfig`、chart 内容、alive installer 与 keepalived 镜像。
- alive 所需内核模块和 sysctl（`ip_vs` 系列、`nf_conntrack`、`net.ipv4.vs.conntrack=1`）由 alive 前置任务或 VM 模板覆盖。若测试环境尚未覆盖，执行人可用临时 privileged DaemonSet 加载后删除，作为测试前置补偿；这不是 provider 职责。
- provider 不直接创建 workload `AppRelease`，验证时需区分 provider 创建的 `ModuleInfo` 和 cluster-transformer 创建的 `AppRelease`。
- alive backend 由 valuesTemplate 从 workload control-plane Node `InternalIP` 生成，provider 不写 `masterIPs`。
- 测试资源统一使用 `tc-vsphere-sblb-*` 前缀，清理时只删除本测试创建的对象。

基础 `VSphereCluster` 片段：

```yaml
spec:
  controlPlaneEndpoint:
    host: <vip>
    port: 6443
  controlPlaneLoadBalancer:
    type: internal
    host: <vip>
    port: 6443
    vrid: 42
    interface: ens192
```

## 1. 用例索引

| TC | 场景 | 类型 | 关键路径 |
| --- | --- | --- | --- |
| TC-CAPV-SBLB-00 | vSphere 网络与内核 preflight | 环境前置 | VIP/GARP、VRRP 组播、IPVS |
| TC-CAPV-SBLB-01 | 字段缺省保持现状 | 兼容 | 存量集群升级后行为不变 |
| TC-CAPV-SBLB-02 | `type=external` 保持既有语义 | 兼容 | 只校验回填 endpoint |
| TC-CAPV-SBLB-03 | `type=internal` 新建集群主路径 | E2E 主路径 | bootstrap VIP + alive minfo + VIP API |
| TC-CAPV-SBLB-04 | bootstrap 仅 init 节点注入临时 VIP | 节点检查 | cloud-config + systemd unit |
| TC-CAPV-SBLB-05 | bootstrap VIP 失败阻断 kubeadm | 异常 | `runcmd` 前插 + `exit 1` |
| TC-CAPV-SBLB-06 | kube-proxy IPVS 配置调整 | 配置 | `strictARP`、`excludeCIDRs`、DaemonSet 滚动 |
| TC-CAPV-SBLB-07 | alive `ModuleInfo` 创建和 patch | E2E 主路径 | version、label、config、所有权 |
| TC-CAPV-SBLB-08 | readiness 与 VIP probe | E2E 主路径 | minfo、AppRelease、pod、`/version`、condition |
| TC-CAPV-SBLB-09 | backend 来源为 Node `InternalIP` | 配置 | 不写 `masterIPs` |
| TC-CAPV-SBLB-10 | VIP 冲突与 endpoint 一致性校验 | 异常 | webhook + reconcile 防御 |
| TC-CAPV-SBLB-11 | 多网卡显式 `interface` | 网络 | bootstrap 脚本 + minfo config |
| TC-CAPV-SBLB-12 | control-plane 节点替换 | E2E 主路径 | VIP join + backend 更新 |
| TC-CAPV-SBLB-13 | 空字段可回填，写入后冻结 | 异常 | webhook immutable |
| TC-CAPV-SBLB-14 | 删除流程 | 删除 | 不新增 cleanup，不阻塞 |

## TC-CAPV-SBLB-00：vSphere 网络与内核 preflight

**目标**：确认目标 vSphere 网络与节点具备 alive 运行前提。这是 vSphere 相对 DCS 必须单独验证的一步：govmomi 现网此前没有自建 VIP 组件在跑，VIP/GARP 与 VRRP 组播都没有既有运行证据。

**前置**：目标网络上有两台同网段可登录的 Linux 主机（可复用已有集群的 control-plane 节点），测试 VIP 未被占用。

**步骤**：
1. `arping -D -I <iface> -c 3 <vip>` 做冲突检测。
2. 在主机 A 的目标网卡上 `ip addr add <vip>/32`，执行 `arping -U -I <iface> -c 3 <vip>`。
3. 从主机 B 和一台业务跳板 ping VIP，检查邻居表中 VIP 对应的 MAC。
4. 删除主机 A 上的 VIP，在主机 B 上添加同一 VIP 并发 GARP，重复第 3 步。
5. 在主机 B 用 raw socket 监听 `224.0.0.18` 的 protocol 112，主机 A 向该组发送 protocol 112 报文，确认收到。
6. 在两台主机检查 `modprobe ip_vs`、`/proc/net/ip_vs` 存在，`sysctl net.ipv4.vs.conntrack`、`net.ipv4.ip_forward`。
7. 清理测试 VIP。

**预期**：
- VIP 无冲突，可在两台主机间漂移，GARP 能更新邻居表中的 MAC。
- VRRP 组播未被二层网络、dvPortGroup 安全策略或 NSX 分布式防火墙阻断。
- `ip_vs` 可加载；`net.ipv4.vs.conntrack` 若为 `0`，记录为需由 alive 前置任务持久化设置为 `1`。

**清理**：删除测试 VIP 与监听脚本。任一项不通过时，先解决网络策略再执行后续用例。

## TC-CAPV-SBLB-01：字段缺省保持现状

**目标**：验证不带 `controlPlaneLoadBalancer` 的集群在 provider 升级后行为完全不变。

**前置**：已有一个在旧 provider 上创建、正常运行的 vSphere 集群（外部 LB 或使用方自备入口形态）。

**步骤**：
1. 升级管理集群的 CAPV controller 与 CRD。
2. 观察该集群的 `VSphereCluster`、`Cluster`、control-plane Machine 至少一个完整 reconcile 周期。
3. 检查是否出现 alive `ModuleInfo`、`SelfBuiltLoadBalancerReady` condition 或 VM userdata 变更。

**预期**：
- `spec.controlPlaneLoadBalancer` 仍为空，CRD 升级不产生数据迁移。
- `controlPlaneEndpoint` 不变，集群 API 持续可用，control-plane Machine 不发生滚动更新。
- 不创建 alive `ModuleInfo`，不出现 `SelfBuiltLoadBalancerReady` condition。
- 集群原有的入口组件不受影响。

**清理**：无。

## TC-CAPV-SBLB-02：`type=external` 保持既有语义

**目标**：验证显式 `type=external` 只做 endpoint 校验与回填，不进入 self-built LB 链路。

**前置**：准备可用外部 LB 地址 `<external-lb-ip>:6443`。

**步骤**：
1. 创建 `type=external`、`host=<external-lb-ip>`、`port=6443` 的集群，`controlPlaneEndpoint` 留空。
2. 等待集群创建完成。
3. 额外提交一个填了 `vrid` 和 `interface` 的 `type=external` 对象。

**预期**：
- `controlPlaneEndpoint` 被回填为 `<external-lb-ip>:6443`，集群正常创建。
- 不创建 alive `ModuleInfo`，不注入 bootstrap VIP（VM 内无 `/etc/capv/bootstrap-vip.sh`）。
- 无 `SelfBuiltLoadBalancerReady` condition。
- 填写 `vrid` / `interface` 时 webhook 返回 warning 但不拒绝，且这两个字段被忽略。

**清理**：删除测试集群。

## TC-CAPV-SBLB-03：`type=internal` 新建集群主路径

**目标**：验证创建阶段的完整链路：bootstrap 临时 VIP → kubeadm init → control-plane join → alive minfo → VIP API 可用。

**前置**：TC-00 通过。

**步骤**：
1. 按第 0 章基础片段创建 3 control-plane + 1 worker 集群。
2. 观察首个 control-plane VM 上电后的 cloud-init 执行、`capv-bootstrap-vip.service` 状态和 VIP 是否出现。
3. 观察后续 control-plane 是否通过 VIP 完成 join。
4. 观察 `SelfBuiltLoadBalancerReady` condition 从 False 到 True 的过程。
5. 集群 Ready 后，用标准 kubeconfig（不跳过 TLS 校验）访问 `https://<vip>:6443/version` 与 `kubectl get nodes`。

**预期**：
- 首个 control-plane 在 kubeadm 之前持有 `<vip>/32`。
- 3 个 control-plane 与 1 个 worker 全部 Ready。
- 管理集群出现 provider-managed alive `ModuleInfo`，workload 出现 `cpaas-system/alive` AppRelease 和 3 个 Ready 的 `kube-system` alive pod。
- `SelfBuiltLoadBalancerReady` 最终为 True，v1beta1 与 v1beta2 两套 condition 语义一致。
- VIP API 访问稳定，TLS 校验通过（说明 serving 证书 SAN 含 VIP）。

**清理**：保留集群供后续用例使用。

## TC-CAPV-SBLB-04：bootstrap 仅 init 节点注入临时 VIP

**目标**：验证注入范围、载体形式和交接行为。

**前置**：TC-03 集群。

**步骤**：
1. 在首个 control-plane VM 内检查 `/etc/capv/bootstrap-vip.sh`、`/etc/systemd/system/capv-bootstrap-vip.service` 和 `systemctl status capv-bootstrap-vip.service`。
2. 在其余 control-plane VM 和 worker VM 内检查同名文件。
3. 检查各 VM 的 guestinfo userdata：注入的 `runcmd` 条目是否位于 kubeadm 命令之前。
4. 确认 unit 文件不含 `[Install]` 段，且 `systemctl is-enabled capv-bootstrap-vip.service` 返回 `static`（非 `enabled`）。
5. alive Ready 后，检查首个 control-plane 上 VIP 的持有方式和 keepalived manifest。
6. 触发一次 control-plane 滚动升级，检查替换出的新 VM 上是否存在 bootstrap 脚本与 unit。
7. 重启当前持有 VIP 的节点，观察 VIP 是否漂移到其它节点。

**预期**：
- 只有 `kubeadm init` 节点存在 bootstrap VIP 脚本与 unit；join control-plane 和 worker 都没有。
- 注入的 `runcmd` 条目排在 CABPK 生成的 kubeadm 命令之前。
- unit 为 `static`，systemd 无法在开机时拉起，不存在与 keepalived 争抢同址的路径。
- alive 接管后 VIP 由 keepalived 持有，`clear_vip()` 已清掉 bootstrap 的 `/32` 地址。
- 滚动升级替换出的新 control-plane VM 上不存在该脚本与 unit。
- 节点重启后 VIP 正常漂移，API 恢复访问。

**清理**：无。

## TC-CAPV-SBLB-05：bootstrap VIP 失败阻断 kubeadm

**目标**：验证 VIP 未成功建立时不会继续执行 kubeadm，避免集群带着错误 endpoint 起来。

**前置**：可创建新集群。

**步骤**：
1. 创建 `type=internal` 集群，把 `interface` 显式设成一个节点上不存在的网卡名（如 `ens999`）。
2. 观察首个 control-plane VM 的 cloud-init 日志、`capv-bootstrap-vip.service` 状态。
3. 检查 kubeadm 是否执行、CAPI bootstrap 成功哨兵文件是否生成、`Machine` 状态。

**预期**：
- 脚本在等待超时后退出非零，unit 失败。
- kubeadm 未执行，bootstrap 成功哨兵文件不存在，`Machine` 停留在未 provisioned 状态。
- 失败原因可从 VM console、cloud-init 日志和 `systemctl status` 定位。

**清理**：删除该测试集群。

## TC-CAPV-SBLB-06：kube-proxy IPVS 配置调整

**目标**：验证 IPVS 模式下的 `strictARP` 与 `excludeCIDRs` 处理，以及非 IPVS 模式下不误改。

**前置**：TC-03 集群；另准备一个 kube-proxy 为 iptables 模式的集群或场景。

**步骤**：
1. 检查 workload 的 `kube-system/kube-proxy` ConfigMap 中 `config.conf` 的 `mode`、`ipvs.strictARP`、`ipvs.excludeCIDRs`。
2. 观察 ConfigMap 变更后 kube-proxy DaemonSet 的滚动过程与 controller 的 requeue 行为。
3. 手工把 `strictARP` 改回 `false`，观察下一轮 reconcile。
4. 在 iptables 模式集群上重复第 1 步。
5. 确认 kube-proxy 镜像仓库仍由既有路径正常处理，与本流程无冲突。

**预期**：
- IPVS 模式下 `strictARP=true` 且 `excludeCIDRs` 含 `<vip>/32`，重复 reconcile 幂等不重复追加。
- ConfigMap 变更后等待 DaemonSet 滚动完成才继续后续步骤。
- 手工改回后能被 reconcile 纠正。
- iptables 模式下不写入 `strictARP` / `excludeCIDRs`，流程继续。
- DaemonSet 的镜像与 imagePullSecret 由既有路径维护，两条路径互不覆盖。

**清理**：无。

## TC-CAPV-SBLB-07：alive `ModuleInfo` 创建和 patch

**目标**：验证 minfo 名称、label、annotation、version、config 与所有权保护。

**前置**：TC-03 集群。

**步骤**：
1. 检查 minfo 名称是否为 `<clusterName>-<sha256(clusterName:alive:alive) 前 32 位>`。
2. 检查 label（`cpaas.io/cluster-name`、`cpaas.io/module-name`、`cpaas.io/module-type`、`self-built-lb-managed`）、annotation `cpaas.io/display-name` 与 `spec.config` 各字段。
3. 手工把 `spec.config.vrid` 改成其它值，观察下一轮 reconcile。
4. 手工删除 `self-built-lb-managed` label，观察 controller 行为。
5. 分别测试三种版本来源：设置 `--plugin-alive-version`、`ModulePlugin.status.targetClusterVersions` 命中、仅 `latestVersion`。
6. 临时把 `ModuleConfig/alive-<version>` 置为未 ready 或删除，观察 controller 行为。

**预期**：
- 名称、label、annotation、config 与设计一致，`spec.config` 不含 `masterIPs`。
- 手工改动被 patch 回期望值。
- 所有权 label 不匹配时 controller 返回错误、不接管该对象、condition 置 False。
- 版本按「启动参数 → targetClusterVersions → latestVersion」优先级解析。
- `ModuleConfig` 缺失或未 ready 时返回错误并在 condition message 中体现，而不是静默等待。

**清理**：恢复被改动的对象。

## TC-CAPV-SBLB-08：readiness 与 VIP probe

**目标**：验证分层 readiness 判断，尤其是「部署 Ready 但四层不通」的场景能被 probe 拦住。

**前置**：TC-03 集群。

**步骤**：
1. 依次确认 minfo `status.version` 追平 `spec.version`、phase Running、AppRelease 的 `Sync` 与 `Health` 为 True、每个 control-plane 上 alive pod Ready。
2. 在某个 control-plane 上临时设置 `net.ipv4.vs.conntrack=0`，从 VIP 连续发起 API 请求。
3. 观察 `SelfBuiltLoadBalancerReady` condition 与 controller 日志。
4. 恢复该参数，观察 condition 恢复。
5. 临时停止一个非 VIP 持有节点的 alive pod，观察 condition 变化。

**预期**：
- 任一层未就绪时 condition 为 False 且 message 指明具体层级，controller 按 10 秒 requeue。
- `conntrack=0` 时 minfo 与 AppRelease 仍可 Ready，但 VIP probe 失败，condition 不为 True。
- 恢复后 condition 回到 True。
- probe 使用标准 TLS 校验，不跳过证书校验。

**清理**：恢复 sysctl 与 alive pod。

## TC-CAPV-SBLB-09：backend 来源为 Node `InternalIP`

**目标**：验证 backend 由 alive 从 workload Node 生成，provider 不参与。

**前置**：TC-03 集群。

**步骤**：
1. 确认 minfo `spec.config` 不含 `masterIPs`。
2. 在 control-plane 节点上检查 `ipvsadm -Ln` 或 `/proc/net/ip_vs` 中 `<vip>:6443` 的 real server 列表。
3. 与 3 个 control-plane Node 的 `InternalIP` 比对。
4. 从 VIP 多次发起 API 请求，确认能轮询到不同 backend。

**预期**：backend 恰好是 3 个 control-plane Node 的 `InternalIP` + apiserver 端口，请求可正常轮询且不超时。

**清理**：无。

## TC-CAPV-SBLB-10：VIP 冲突与 endpoint 一致性校验

**目标**：验证 webhook 与 reconcile 两层防御。

**前置**：可提交新对象。

**步骤**：
1. 提交 `type=internal` 且 `host` 等于某槽位 `network.primary.ip` 的对象。
2. 提交 `host` 等于某槽位 `network.additional[].ip` 的对象。
3. 提交 `controlPlaneEndpoint` 与 `controlPlaneLoadBalancer.host/port` 不一致的对象。
4. 提交 `type=internal` 但 `host` 为 IPv6 地址、或 `vrid` 为 0 / 256、或 `port` 为 0 的对象。
5. 绕过 webhook 直接写入冲突对象（临时关闭 webhook），观察 reconcile 行为。
6. 在 `VSphereMachineConfigPool` 中新增一个 IP 等于 VIP 的槽位。

**预期**：
- 第 1–4 步均被 webhook 拒绝，错误信息指明具体字段。
- 第 5 步 reconcile 做防御性校验并返回错误、condition 置 False，不创建 minfo。
- 第 6 步被拒绝。
- 文档中已说明：IPAM pool 范围 provider 读不到，VIP 落在 IPAM 池内属于环境前置条件问题，不由校验覆盖。

**清理**：删除测试对象，恢复 webhook。

## TC-CAPV-SBLB-11：多网卡显式 `interface`

**目标**：验证多网卡节点上 VIP 绑定到指定网卡。

**前置**：槽位配置主网卡 + 至少一张附加网卡。

**步骤**：
1. 创建 `type=internal` 且显式指定 `interface` 为主网卡名的集群。
2. 检查 bootstrap 脚本中的接口取值与实际 VIP 所在网卡。
3. 检查 minfo `spec.config.interface` 与 alive 实际绑定的网卡。
4. 另建一个不指定 `interface` 的集群，确认自动探测按节点主 IP 选中正确网卡。

**预期**：
- 指定时 bootstrap VIP 与 alive 都使用同一网卡，不落到附加网卡上。
- 指定的网卡上不存在节点主 IP 时，bootstrap 脚本失败并阻断 kubeadm（与 TC-05 一致）。
- 不指定时自动探测结果与节点主 IP 所在网卡一致。

**清理**：删除测试集群。

## TC-CAPV-SBLB-12：control-plane 节点替换

**目标**：验证节点替换后新节点通过 VIP join，且 backend 与 alive 部署自动收敛。

**前置**：TC-03 集群。

**步骤**：
1. 删除一个非 VIP 持有的 control-plane Machine，触发替换。
2. 观察新 VM 是否注入 bootstrap VIP（不应注入，它不是 init 节点）。
3. 观察新节点是否通过 VIP 完成 join。
4. 观察 alive AppRelease 的 `global.controlPlaneNodeIdentity` 是否被 patch、alive pod 是否在新节点上 Ready。
5. 检查 IPVS backend 列表是否更新为新的 Node `InternalIP`。
6. 重复一次，这次删除当前持有 VIP 的 control-plane。

**预期**：
- 新 control-plane 不注入 bootstrap VIP，通过 VIP 正常 join。
- `controlPlaneNodeIdentity` 变化触发 alive 重装，新节点上 alive pod Ready。
- backend 列表收敛到当前 3 个 control-plane。
- 删除 VIP 持有者时 VIP 漂移到其它节点，API 中断时间在 keepalived 切换预期内，`SelfBuiltLoadBalancerReady` 短暂 False 后恢复 True。

**清理**：等待集群恢复 3 control-plane。

## TC-CAPV-SBLB-13：空字段可回填，写入后冻结

**目标**：验证可变性只取决于 `spec.controlPlaneLoadBalancer` 是否为空，与集群是否 initialized 无关。

**前置**：TC-03 集群（字段为 `type=internal`），以及一个字段为空的集群（TC-01 形态）。

**步骤**：
1. 对 TC-03 集群分别 patch `type`、`host`、`port`、`vrid`、`interface`，以及把整个字段置回 `null`。
2. 对字段为空的集群回填一份与其 `controlPlaneEndpoint` 一致的 `type=external`。
3. 对第 2 步已回填的集群再 patch 任一 LB 字段。
4. 对一个 `ControlPlaneInitialized=False` 且字段非空的新建集群 patch `host`。

**预期**：
- 第 1 步全部被拒绝，错误信息说明字段写入后不可变、且无放行入口。
- 第 2 步通过，`controlPlaneEndpoint` 不变。
- 第 3 步被拒绝。
- 第 4 步被拒绝，证明规则不看 initialized 状态。

**清理**：删除第 2 步使用的集群（回填不可撤销）。

## TC-CAPV-SBLB-14：删除流程

**目标**：验证删除不因 self-built LB 状态阻塞，且 minfo / AppRelease 走通用链路。

**前置**：TC-03 集群。

**步骤**：
1. 正常删除 `Cluster`，观察 VM、`VSphereMachine`、`VSphereCluster` 的删除过程与耗时。
2. 检查 alive `ModuleInfo` 是否随 platform Cluster `OwnerReference` 被 GC。
3. 另建一个集群，先让 workload API 不可访问（例如先删掉全部 control-plane VM），再删除 `Cluster`。

**预期**：
- 删除流程不新增 self-built LB 相关等待，不因 alive pod、VIP、IPVS 或 minfo 状态卡住。
- provider 不专门删除 minfo；minfo 由 GC 处理，workload AppRelease 由 cluster-transformer 通用 finalizer 链路处理。
- workload API 不可访问时删除仍能完成。

**清理**：确认测试命名空间内无 `tc-vsphere-sblb-*` 残留对象。

## 2. 跑测顺序建议

1. TC-00 网络与内核 preflight。不通过则先解决网络策略，后续用例无意义。
2. TC-01、TC-02 兼容性，确认升级不影响存量。
3. TC-03 主路径，产出后续用例复用的集群。
4. TC-04、TC-06、TC-07、TC-08、TC-09 在该集群上依次验证注入、配置、minfo 与 readiness。
5. TC-05、TC-10、TC-11 各自新建集群验证异常与网络分支。
6. TC-12、TC-13 回到主路径集群验证运行态变更。
7. TC-14 最后执行，顺带清理主路径集群。
