# VSphere 集群功能验证测试计划

> 按章节顺序执行。每个测试用例分为 **操作** 和 **验证** 两部分，操作是需要执行的变更，验证是需要检查的结果。
> 占位符说明：`<cluster-name>`、`<namespace>`、`<kcp-name>` 等需替换为实际值。

<!-- 环境概要：
  CP pool: 5 slots, DC1×3 + DC2×2
  Worker pool: 4 slots, DC1×2 + DC2×2
  CP: 8C/16G, 5 持久盘 (3 格式化+挂载, 2 raw 50G)
  Worker: 16C/32G, 4 持久盘 (2 格式化+挂载, 2 raw 50G)
  KCP replicas=3 (初始), 两个 MD replicas=3
  双网卡: br300 + br314
-->

---

## 1. 前置条件

### 管理集群
- [ ] global 集群就绪，kubectl 可访问
- [ ] CAPI + CAPV controller 已安装并运行
- [ ] ClusterResourceSet FeatureGate 已开启（ACP 4.2 默认关闭，需手动开启；ACP 4.3 已默认开启）
- [ ] clusterrole `capi-manager-role` 具有 configmap 的 delete 权限（ACP 4.2 默认缺少，需手动添加，否则 ClusterResourceSet 无法正常工作；ACP 4.3 已修复）

### vCenter
- [ ] vCenter 可达（从管理集群网络）
- [ ] 至少 2 个 Datacenter（下称 DC1、DC2），每个 Datacenter 至少 1 个 ComputeCluster
- [ ] 每个 Datacenter 有 VM 模板（同名，含 cloud-init + open-vm-tools + containerd）
- [ ] vCenter 凭据 Secret 已创建（IdentityRef 方式）

### 故障域
- [ ] 2 个 VSphereFailureDomain + 2 个 VSphereDeploymentZone（分别对应 DC1、DC2）
- [ ] 两个 DeploymentZone 均标记 controlPlane: true（允许 CP 和 Worker 在两个 Datacenter 调度）

### VIP 与负载均衡
- [ ] Control plane VIP 已分配
- [ ] 负载均衡器已配置，VIP 对应的 realserver 指向所有 CP 节点 IP，端口 6443
- [ ] VIP 从管理集群可达

### 网络
- [ ] 每个 Datacenter 有 2 个可用的 PortGroup/Network（用于双网卡测试）
- [ ] Pod/Service CIDR 无冲突
- [ ] 为每个节点分配 2 组静态 IP（NIC1 + NIC2）：
  - CP 节点：5 × 2 = 10 个 IP（跨 DC1、DC2）
  - Worker 节点：4 × 2 = 8 个 IP（跨 DC1、DC2）

### 存储
- [ ] 系统盘 Datastore：在 DC1 和 DC2 中都存在（VSphereMachineTemplate 中指定的 datastore 名称相同）
- [ ] 持久盘 Datastore：每个 slot 配置的 datastore 在该 slot 对应的 Datacenter 中存在
- [ ] 容量充足（系统盘 300G × 9 节点 + 持久盘按 slot 定义：CP 每节点 240G、Worker 每节点 220G）

### 静态资源池
- [ ] CP pool：5 个 slot，分布 3+2（DC1 3 个，DC2 2 个），每个 slot 配置：
  - hostname、双网卡（NIC1 + NIC2，各含 IP/网关/DNS）
  - 持久盘 5 块：var-cpaas（/var/cpaas）、var-lib-containerd（/var/lib/containerd）、var-lib-etcd（/var/lib/etcd）、data-disk-1（50G 空盘）、data-disk-2（50G 空盘）
- [ ] Worker pool：4 个 slot，分布 2+2（DC1 2 个，DC2 2 个），每个 slot 配置：
  - hostname、双网卡（NIC1 + NIC2，各含 IP/网关/DNS）
  - 持久盘 4 块：var-cpaas（/var/cpaas）、var-lib-containerd（/var/lib/containerd）、data-disk-1（50G 空盘）、data-disk-2（50G 空盘）
- [ ] 2 个 MachineDeployment 定义（各 replicas=3），引用同一个 worker pool，分别绑定不同的 failureDomain（md-0→dz-dc1/DC1，md-1→dz-dc2/DC2）
- [ ] VSphereMachineTemplate 的 network.devices 配置 2 个网卡（与 pool slot 中的 2 个 network 条目对应）

### 计算资源
- [ ] CP 节点：至少 8C/16G/300G 系统盘
- [ ] Worker 节点：至少 16C/32G/300G 系统盘

---

## 2. 集群创建

**测试目的：** 验证集群从零创建的完整流程，包括 Cluster/VSphereCluster 状态收敛、CP 和 Worker 节点创建、静态资源池 slot 分配、持久盘格式化挂载、双网卡配置以及工作负载集群可达性。确保所有组件在初始部署后处于预期状态。

### 2.1 创建集群

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 2.1.1 | 创建集群资源 | `kubectl apply -f <env-extension-dir>/` |

等待集群就绪（所有 CP Machine 进入 Running）。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.1.2 | Cluster phase | `kubectl get cluster <cluster-name> -n <namespace>`<br>`-o jsonpath='{.status.phase}'` | `Provisioned` |
| 2.1.3 | VSphereCluster conditions | `kubectl get vspherecluster <cluster-name> -n <namespace>`<br>`-o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}'` | `Ready=True`，`VCenterAvailable=True`，`FailureDomainsAvailable=True` |

### 2.2 Control Plane 节点

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.2.1 | 3 副本全部 Running | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name> -o wide` | 3 个 Machine，Phase=Running |
| 2.2.2 | hostname 与 slot 一致 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.status.nodeRef.name}{"\n"}{end}'` | 输出为 CP pool 中已分配 slot 的 hostname（如 master-01、master-02、master-03） |
| 2.2.3 | 双网卡 IP（每个 VM 应有 2 个 IP） | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'` | 每个 VM 2 个 IP，与 slot 中 NIC1/NIC2 的 ip 一致（不含前缀长度） |

### 2.3 Worker 节点

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.3.1 | Worker Machine 状态 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/cluster-name=<cluster-name>,!cluster.x-k8s.io/control-plane-name` | 先抢到 pool 的 MD（`<bound-md>`）：其 failureDomain 对应 DC 的 2 个 slot 被分配，2 个 Machine Running；第 3 个 Machine 因该 DC 无可用 slot 而卡在 Provisioning。另一个 MD（`<other-md>`）：3 个 Machine 均 Provisioning（被 consumerRef 拦截） |

### 2.4 资源池状态

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.4.1 | CP pool consumerRef | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'` | `consumerRef=KubeadmControlPlane/<kcp-name>` |
| 2.4.2 | CP pool slot 状态 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | 3 个 InUse（machineRef 指向对应 VSphereMachine），2 个 Available |
| 2.4.3 | CP pool conditions | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}'` | `ClusterRefReady=True`，`VCenterAvailable=True` |
| 2.4.4 | Worker pool consumerRef | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'` | `consumerRef=MachineDeployment/<md-name>`（先抢到的 MD） |
| 2.4.5 | Worker pool slot 状态 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | `<bound-md>` failureDomain 对应 DC 的 2 个 slot InUse，其余 Available |

### 2.5 VSphereMachine ResourcePoolReady Condition

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.5.1 | ResourcePoolReady condition | `kubectl get vspheremachines -n <namespace>`<br>`-l cluster.x-k8s.io/cluster-name=<cluster-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {range .status.conditions[?(@.type=="ResourcePoolReady")]}{.status} ({.reason}){end}{"\n"}{end}'` | 已分配 slot 的 VSphereMachine 显示 `True (SlotAllocated)`；`<bound-md>` 中无可用 slot 的 VSphereMachine 显示 `False (NoAvailableSlots)`；`<other-md>` 的 VSphereMachine 显示 `False (PoolBoundToOtherConsumer)` |

### 2.6 Slot-Machine 匹配

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.6.1 | 获取 slot→machine 映射 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json \|`<br>`jq -r '.status.resourceStatuses[] \| select(.state=="InUse") \| "\(.hostname) -> \(.machineRef.name)"'` | 输出 InUse slot 对应的 machine 名称 |
| 2.6.2 | annotation 与 slot hostname 一致 | `kubectl get vspheremachine <machineRef-name> -n <namespace>`<br>`-o jsonpath='{.metadata.annotations.infrastructure\.cluster\.x-k8s\.io/resource-slot-hostname}'` | 与 slot hostname 一致 |

### 2.7 持久盘挂载

<!-- CP 节点：5 块盘（3 块格式化挂载 + 2 块空盘） -->

**验证（CP 节点）：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.7.1 | 磁盘布局 | `ssh <cp-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT"` | 包含：`<disk> 20G ext4 /var/cpaas`，`<disk> 100G ext4 /var/lib/containerd`，`<disk> 20G ext4 /var/lib/etcd`，`<disk> 50G`（data-disk-1，无文件系统，未挂载），`<disk> 50G`（data-disk-2，无文件系统，未挂载） |
| 2.7.2 | 已挂载盘数量 | `ssh <cp-node-ip> "mount \| grep -cE '/var/cpaas\|/var/lib/containerd\|/var/lib/etcd'"` | `3` |
| 2.7.3 | 文件系统类型 | `ssh <cp-node-ip> "df -T /var/cpaas /var/lib/containerd /var/lib/etcd"` | Type 列均为 `ext4` |
| 2.7.4 | 空盘 symlink 存在 | `ssh <cp-node-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 两个 symlink 指向实际块设备 |
| 2.7.5 | 空盘无文件系统 | `ssh <cp-node-ip> "blkid $(readlink -f /dev/disk/by-capv/data-disk-1); echo $?"` | 无输出，退出码 2（无文件系统） |
| 2.7.6 | 关键服务运行 | `ssh <cp-node-ip> "systemctl is-active containerd kubelet"` | 两个均为 `active` |

**验证（Worker 节点）：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.7.7 | 磁盘布局 | `ssh <worker-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT"` | 包含：`<disk> 20G ext4 /var/cpaas`，`<disk> 100G ext4 /var/lib/containerd`，`<disk> 50G`（data-disk-1，无文件系统，未挂载），`<disk> 50G`（data-disk-2，无文件系统，未挂载） |
| 2.7.8 | 已挂载盘数量 | `ssh <worker-node-ip> "mount \| grep -cE '/var/cpaas\|/var/lib/containerd'"` | `2` |
| 2.7.9 | 文件系统类型 | `ssh <worker-node-ip> "df -T /var/cpaas /var/lib/containerd"` | Type 列均为 `ext4` |
| 2.7.10 | 空盘 symlink 存在 | `ssh <worker-node-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 两个 symlink 指向实际块设备 |
| 2.7.11 | 空盘无文件系统 | `ssh <worker-node-ip> "blkid $(readlink -f /dev/disk/by-capv/data-disk-1); echo $?"` | 无输出，退出码 2（无文件系统） |

### 2.8 工作负载集群可用性

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 2.8.1 | 获取 kubeconfig | `kubectl get secret <cluster-name>-kubeconfig -n <namespace>`<br>`-o jsonpath='{.data.value}' \| base64 -d > /tmp/<cluster-name>.kubeconfig` |

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.8.2 | 节点就绪 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes -o wide` | 3 个 CP + 2 个 Worker 全部 Ready，node name 与 slot hostname 一致 |

### 2.9 双网卡

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 2.9.1 | 双 IP 验证 | `ssh <node-ip> "ip -4 addr show \| grep -E 'inet '"` | 至少 2 个非 loopback IP，与 slot NIC1/NIC2 一致 |
| 2.9.2 | 默认路由 | `ssh <node-ip> "ip route"` | `default via <nic1_gateway>` |

---

## 3. 故障域

**测试目的：** 验证故障域（FailureDomain / DeploymentZone）对 CP 和 Worker 节点调度的约束作用。CP pool 5 slot 分布 3+2（DC1 3 个，DC2 2 个）；Worker pool 4 slot 分布 2+2（DC1 2 个，DC2 2 个）。md-0 绑定 dz-dc1（DC1），md-1 绑定 dz-dc2（DC2）。需确保 VM 实际创建的 Datacenter 与 slot 声明一致，且当某个 DC 的 slot 不足时 Machine 正确阻塞而非跨 DC 分配。

### 3.1 CP 跨故障域分布

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 3.1.1 | Slot datacenter 分布 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| "\(.hostname): datacenter=\(.datacenter // "<pool-default>")"'` | 3 个 DC1，2 个 DC2 |
| 3.1.2 | VSphereVM 实际 datacenter | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: datacenter={.spec.datacenter}{"\n"}{end}'` | 与对应 slot datacenter 一致 |
| 3.1.3 | Machine failureDomain | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: failureDomain={.spec.failureDomain}{"\n"}{end}'` | DC1 的 Machine → `<dz-dc1-name>`，DC2 的 Machine → `<dz-dc2-name>` |
| 3.1.4 | Slot-Machine datacenter 交叉验证 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] as $slot \| .status.resourceStatuses[] \| select(.state=="InUse" and .hostname==$slot.hostname) \| "\(.hostname): slot-dc=\($slot.datacenter // "<pool-default>"), machine=\(.machineRef.name)"'`<br><br>对每行验证：<br>`kubectl get vspherevm <machineRef-name> -n <namespace> -o jsonpath='{.spec.datacenter}'` | VSphereVM datacenter 与 slot-dc 一致 |

### 3.2 Worker 故障域约束

<!-- <bound-md> 绑定 failureDomain 对应 <bound-dc> -->

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 3.2.1 | InUse slot datacenter | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] as $slot \| .status.resourceStatuses[] \| select(.state=="InUse" and .hostname==$slot.hostname) \| "\(.hostname): datacenter=\($slot.datacenter // "<pool-default>")"'` | 所有 InUse slot 的 datacenter 均为 `<bound-dc>` |
| 3.2.2 | 另一个 DC 的 slot 未被分配 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] as $slot \| .status.resourceStatuses[] \| select(.hostname==$slot.hostname) \| "\(.hostname): datacenter=\($slot.datacenter // "<pool-default>"), state=\(.state)"'` | 非 `<bound-dc>` 的 slot 状态为 Available |
| 3.2.3 | VSphereVM datacenter | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: datacenter={.spec.datacenter}{"\n"}{end}'` | 全部为 `<bound-dc>` |

### 3.3 故障域不匹配 — slot 不足阻塞

<!-- 前置：<bound-md> replicas=3，该 DC 仅 2 个 slot，初始创建时即触发 -->

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 3.3.1 | 第 3 个 Machine 卡住 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}{"\n"}{end}'` | 2 个 Running + 1 个 Provisioning |
| 3.3.2 | 另一个 DC 的 slot 不被分配 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}{"\n"}{end}'` | `<bound-dc>` 的 2 个 InUse，另一个 DC 的 2 个 Available |
| 3.3.3 | 找到 stuck machine | `kubectl get vspheremachines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o json \| jq -r '.items[] \| select(.status.ready != true) \| .metadata.name' \| head -1` | 输出 stuck machine 名称 |
| 3.3.4 | condition 详情 | `kubectl get vspheremachine <stuck-machine-name> -n <namespace>`<br>`-o jsonpath='{range .status.conditions[*]}{.type}: {.status} ({.reason}) - {.message}{"\n"}{end}'` | `ResourcePoolReady: False (NoAvailableSlots) - "no available slots..."` |

**操作（恢复）：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 3.3.5 | 缩容恢复 | `kubectl scale md <bound-md> -n <namespace> --replicas=2` |

---

## 4. 扩容

**测试目的：** 验证集群在运行过程中动态扩容的能力。CP 从 3 副本扩到 5 副本（消耗完全部 CP pool slot），Worker 从已有副本数扩到 4（消耗完全部 Worker pool slot）。需确认新节点正确获得 slot、hostname、双网卡 IP、持久盘，etcd 集群正常扩展，工作负载集群全部 Ready。

### 4.1 CP 扩容（3 → 5）

<!-- 前置：KCP replicas=3，CP pool 3 InUse + 2 Available -->

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 4.1.1 | 触发扩容 | `kubectl patch kcp <kcp-name> -n <namespace> --type=merge -p '{"spec":{"replicas":5}}'` |

等待所有 Machine 进入 Running。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 4.1.2 | Machine 状态 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}, node={.status.nodeRef.name}{"\n"}{end}'` | 5 个 Machine，全部 Running |
| 4.1.3 | CP Pool slot | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | 5 个 InUse，0 个 Available |
| 4.1.4 | hostname 验证 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.status.nodeRef.name}{"\n"}{end}' \| sort` | CP pool 中 5 个 slot 的 hostname |
| 4.1.5 | 网络验证 | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'` | 每个 VM 2 个 IP，与 slot NIC1/NIC2 一致 |
| 4.1.6 | 新 CP 持久盘 — 格式化盘 | `ssh <new-cp-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT \| grep -E 'var/cpaas\|var/lib/containerd\|var/lib/etcd'"` | 3 块格式化挂载盘，路径和大小与 slot 定义一致 |
| 4.1.7 | 新 CP 持久盘 — 空盘 | `ssh <new-cp-node-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 2 块空盘 symlink 存在 |
| 4.1.8 | etcd 集群 | `ssh <cp-node-ip> "ETCDCTL_API=3 etcdctl`<br>`--cacert=/etc/kubernetes/pki/etcd/ca.crt`<br>`--cert=/etc/kubernetes/pki/etcd/peer.crt`<br>`--key=/etc/kubernetes/pki/etcd/peer.key`<br>`member list -w table"` | 5 个成员，全部 started |
| 4.1.9 | 工作负载集群 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes` | 5 个 CP Ready |

### 4.2 Worker 扩容（→ 4）

<!-- 前置：<bound-md> replicas=2（3.3.5 缩容后），2 个 Running -->

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 4.2.1 | 触发扩容 | `kubectl scale md <bound-md> -n <namespace> --replicas=4` |

等待所有 Machine 进入 Running。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 4.2.2 | Machine 状态 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}, node={.status.nodeRef.name}{"\n"}{end}'` | 4 个 Running |
| 4.2.3 | Slot 分配 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | 4 个 slot 全部 InUse，0 个 Available |
| 4.2.4 | consumerRef 不变 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'` | 不变 |
| 4.2.5 | hostname 与网络 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes`<br>`-l nodepool=<bound-md-nodepool-label> -o wide` | 4 个 Worker Ready，NAME 为 slot hostname |
| 4.2.6 | 新 Worker 双网卡 | `ssh <new-worker-ip> "ip -4 addr show \| grep 'inet ' \| grep -v '127.0.0.1'"` | 2 个 IP，与 slot NIC1/NIC2 一致 |
| 4.2.7 | 新 Worker 持久盘 — 格式化盘 | `ssh <new-worker-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT \| grep -E 'var/cpaas\|var/lib/containerd'"` | 2 块格式化挂载盘，路径和大小与 slot 定义一致 |
| 4.2.8 | 新 Worker 持久盘 — 空盘 | `ssh <new-worker-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 2 块空盘 symlink 存在 |
| 4.2.9 | 工作负载集群 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes` | 原有节点 + 新 Worker 全部 Ready |

---

## 5. 滚动更新

**测试目的：** 验证通过创建新 VSphereMachineTemplate（修改 memoryMiB 或 numCPUs）并更新 KCP/MD 的 infrastructureRef 触发滚动替换后，hostname、IP、持久盘被完整复用，etcd 数据不丢失。这是静态资源池场景下最关键的运维操作之一：确保滚动更新不会导致网络/存储身份变化。

<!-- 说明：如果有配套新版本 Kubernetes 的 OS 模板，也可以在新 template 中引用新 OS 模板（修改 spec.template.spec.template 路径），
     同时更新 KCP/MD 的 spec.version，实现集群版本升级。验证方式相同。 -->

### 5.1 CP 滚动更新

<!-- 前置：KCP 5 副本 Running，CP pool 5 slot 全部 InUse -->

#### 5.1.1 记录更新前状态

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 5.1.1a | 记录 slot → machine 映射 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json \|`<br>`jq -r '.status.resourceStatuses[] \| "\(.hostname): machine=\(.machineRef.name)"'` |
| 5.1.1b | 记录持久盘 VolumePath | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| "\(.hostname): \([.persistentDisks[] \| "\(.name)=\(.volumePath)"] \| join(", "))"'` |
| 5.1.1c | 记录节点 IP | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'` |
| 5.1.1d | 写入 etcd 测试数据 | `ssh <cp-node-ip> "ETCDCTL_API=3 etcdctl`<br>`--cacert=/etc/kubernetes/pki/etcd/ca.crt`<br>`--cert=/etc/kubernetes/pki/etcd/peer.crt`<br>`--key=/etc/kubernetes/pki/etcd/peer.key`<br>`put /capv-test/rolling-update-marker 'before-update'"` |

> 保存上述输出，用于更新后对比。

#### 5.1.2 触发滚动更新

**操作：**

创建新 template（以修改 memoryMiB 为例）：

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereMachineTemplate
metadata:
  name: <cp-template-name>-v2
  namespace: <namespace>
spec:
  template:
    spec:
      server: "<vsphere-server>"
      template: "<vm-template-path>"
      cloneMode: <clone-mode>
      datastore: "<datastore>"
      diskGiB: <disk-gib>
      memoryMiB: <new-memory-mib>
      numCPUs: <num-cpus>
      os: Linux
      powerOffMode: trySoft
      network:
        devices:
        - networkName: "<nic1-network>"
        - networkName: "<nic2-network>"
      resourcePoolRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: VSphereResourcePool
        name: <cp-pool-name>
        namespace: <namespace>
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 5.1.2a | 创建新 template | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |
| 5.1.2b | 更新 KCP infrastructureRef | `kubectl patch kcp <kcp-name> -n <namespace> --type=merge`<br>`-p '{"spec":{"machineTemplate":{"infrastructureRef":{"name":"<cp-template-name>-v2"}}}}'` |

等待滚动替换完成（逐个替换，先删旧再建新）：

```
kubectl get machines -n <namespace> -l cluster.x-k8s.io/control-plane-name=<kcp-name> -w
```

#### 5.1.3 验证滚动替换结果

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 5.1.3a | hostname 不变 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.status.nodeRef.name}{"\n"}{end}' \| sort` | 与更新前相同的 5 个 hostname |
| 5.1.3b | IP 复用 | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/control-plane-name=<kcp-name>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'` | 与更新前一致 |
| 5.1.3c | 持久盘复用 — VolumePath | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| "\(.hostname): \([.persistentDisks[] \| "\(.name)=\(.volumePath)"] \| join(", "))"'` | 与 5.1.1b 记录完全一致 |
| 5.1.3d | 持久盘复用 — 格式化盘 | `ssh <cp-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT \| grep -E 'var/cpaas\|var/lib/containerd\|var/lib/etcd'"` | 3 块格式化挂载盘，与更新前一致 |
| 5.1.3e | 持久盘复用 — 空盘 | `ssh <cp-node-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 2 块空盘 symlink 存在，仍未格式化 |
| 5.1.3f | etcd 数据保留 | `ssh <cp-node-ip> "ETCDCTL_API=3 etcdctl`<br>`--cacert=/etc/kubernetes/pki/etcd/ca.crt`<br>`--cert=/etc/kubernetes/pki/etcd/peer.crt`<br>`--key=/etc/kubernetes/pki/etcd/peer.key`<br>`get /capv-test/rolling-update-marker"` | `before-update` |
| 5.1.3g | Pool slot 更新 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | 5 个 InUse，machineRef 指向新 Machine（名称与更新前不同） |
| 5.1.3h | 工作负载集群 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes -o wide` | 5 个 CP Ready，IP 和 hostname 不变 |

### 5.2 Worker 滚动更新

<!-- 前置：<bound-md> replicas>=1，worker slot InUse -->

#### 5.2.1 记录更新前状态

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 5.2.1a | 记录 slot → machine 映射 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.status.resourceStatuses[] \| select(.state=="InUse") \| "\(.hostname): machine=\(.machineRef.name)"'` |
| 5.2.1b | 记录持久盘 VolumePath | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| "\(.hostname): \([.persistentDisks[] \| "\(.name)=\(.volumePath)"] \| join(", "))"'` |
| 5.2.1c | 记录节点 IP | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'` |

> 保存上述输出，用于更新后对比。

#### 5.2.2 触发滚动更新

**操作：**

创建新 template（以修改 memoryMiB 为例）：

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereMachineTemplate
metadata:
  name: <worker-template-name>-v2
  namespace: <namespace>
spec:
  template:
    spec:
      server: "<vsphere-server>"
      template: "<vm-template-path>"
      cloneMode: <clone-mode>
      datastore: "<datastore>"
      diskGiB: <disk-gib>
      memoryMiB: <new-memory-mib>
      numCPUs: <num-cpus>
      os: Linux
      powerOffMode: trySoft
      network:
        devices:
        - networkName: "<nic1-network>"
        - networkName: "<nic2-network>"
      resourcePoolRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: VSphereResourcePool
        name: <worker-pool-name>
        namespace: <namespace>
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 5.2.2a | 创建新 template | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |
| 5.2.2b | 更新 MD infrastructureRef | `kubectl patch md <bound-md> -n <namespace> --type=merge`<br>`-p '{"spec":{"template":{"spec":{"infrastructureRef":{"name":"<worker-template-name>-v2"}}}}}'` |

等待滚动替换完成：

```
kubectl get machines -n <namespace> -l cluster.x-k8s.io/deployment-name=<bound-md> -w
```

#### 5.2.3 验证滚动替换结果

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 5.2.3a | hostname 复用 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes`<br>`-l nodepool=<bound-md-nodepool-label> -o wide` | NAME 与更新前相同 |
| 5.2.3b | IP 复用 | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'` | IP 与更新前一致 |
| 5.2.3c | 持久盘 — VolumePath | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| "\(.hostname): \([.persistentDisks[] \| "\(.name)=\(.volumePath)"] \| join(", "))"'` | VolumePath 与更新前一致 |
| 5.2.3d | 持久盘 — 格式化盘 | `ssh <worker-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT \| grep -E 'var/cpaas\|var/lib/containerd'"` | 2 块格式化挂载盘不变 |
| 5.2.3e | 持久盘 — 空盘 | `ssh <worker-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 2 块空盘 symlink 存在，仍未格式化 |
| 5.2.3f | Pool slot | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | machineRef 指向新 Machine，hostname 不变 |
| 5.2.3g | consumerRef | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.name}'` | 不变 |

---

## 6. 缩容

**测试目的：** 验证集群缩容操作（减少 replicas）后，slot 正确从 InUse 转为 Released、Machine 被删除、工作负载集群节点移除。同时测试超配场景（replicas 超过可用 slot 数）的正确阻塞行为，以及缩容至 0 后的完整清理。

### 6.1 Worker 缩容（4 → 2）

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 6.1.1 | 触发缩容 | `kubectl scale md <bound-md> -n <namespace> --replicas=2` |

等待缩容完成。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 6.1.2 | Machine 状态 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}{"\n"}{end}'` | 2 个 Running |
| 6.1.3 | Slot 状态 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'` | 2 个 InUse + 2 个 Released（lastReleasedTime 已设置，machineRef 保留） |
| 6.1.4 | ConsumerRef | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.name}'` | 仍为 `<bound-md>` |
| 6.1.5 | 工作负载集群 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes`<br>`-l nodepool=<bound-md-nodepool-label>` | 2 个 Worker Ready |

### 6.2 Worker 超配

<!-- 前置：<bound-md> replicas=2，pool 中 2 个 Released + 2 个 InUse -->

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 6.2.1 | 触发超配 | `kubectl scale md <bound-md> -n <namespace> --replicas=5` |

等待可分配的 Machine 进入 Running。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 6.2.2 | 分配结果 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}{"\n"}{end}'` | 4 个 Running + 1 个 Provisioning |
| 6.2.3 | 找到 stuck machine | `kubectl get vspheremachines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>`<br>`-o json \| jq -r '.items[] \| select(.status.ready != true) \| .metadata.name'` | 输出 stuck machine 名称 |
| 6.2.4 | 卡住原因 | `kubectl get vspheremachine <stuck-machine-name> -n <namespace>`<br>`-o jsonpath='{range .status.conditions[*]}{.type}: {.status} ({.reason}) - {.message}{"\n"}{end}'` | `ResourcePoolReady: False (NoAvailableSlots) - "no available slots..."` |

**操作（恢复）：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 6.2.5 | 恢复 | `kubectl scale md <bound-md> -n <namespace> --replicas=2` |

### 6.3 Worker 缩容至 0

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 6.3.1 | 触发缩容至 0 | `kubectl scale md <bound-md> -n <namespace> --replicas=0` |

等待所有 Machine 删除完成。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 6.3.2 | Machine 状态 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<bound-md>` | No resources found |
| 6.3.3 | Slot 状态 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'` | 之前 InUse 的 slot → Released |
| 6.3.4 | ConsumerRef | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.name}'` | 仍存在（Released slot 未回收，pool 不是 fully reusable） |
| 6.3.5 | vCenter VM 清理 | `govc find / -type m -name '<cluster-name>-*' \| grep -i worker` | 无输出 |

---

## 7. 静态资源池

**测试目的：** 深入验证静态资源池的生命周期管理，包括 Released → Available 状态转换（磁盘回收）、consumerRef 自动清空与切换、ClusterRef 凭据链校验、以及 pool 删除保护机制。这些是资源池正确性和安全性的核心保障。

### 7.1 Slot 生命周期：Released → Available

<!-- 通过手动修改 lastReleasedTime 加速 releaseDelay 过期，验证磁盘回收和 slot 状态变化 -->
<!-- 前置：worker pool 中至少 1 个 Released slot（第 6 章缩容后） -->

#### 7.1.1 确认前置状态

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.1.1a | 确认 Released slot | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.status.resourceStatuses[] \| select(.state=="Released") \| "\(.hostname): lastReleasedTime=\(.lastReleasedTime)"'` | 至少 1 个 Released |
| 7.1.1b | 确认可回收持久盘 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| select(.persistentDisks != null) \| "\(.hostname): \([.persistentDisks[] \| select(.volumePath != null and .volumePath != "") \| "\(.name)=\(.volumePath)"] \| join(", "))"'` | Released slot 有 VolumePath |

#### 7.1.2 加速过期并观察回收

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.1.2a | 加速过期 | `kubectl patch vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`--type=merge --subresource=status`<br>`-p '{"status":{"resourceStatuses":[{"hostname":"<released-slot-hostname>","state":"Released","lastReleasedTime":"<25-hours-ago-RFC3339>"}, ...]}}'` |

> 注意：需包含所有 slot 的 status，否则会覆盖其他 slot 状态。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.1.2b | 磁盘回收状态 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.status.resourceStatuses[] \| select(.hostname=="<released-slot-hostname>") \| "state=\(.state), reclaimStatus=\(.reclaimStatus)"'` | `reclaimStatus.state=Running` → `Completed`（等待 ~30s 后检查） |
| 7.1.2c | VolumePath/DiskUUID 清空 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.spec.resources[] \| select(.hostname=="<released-slot-hostname>") \| "\(.hostname): \([.persistentDisks[] \| "\(.name): volumePath=\(.volumePath), diskUUID=\(.diskUUID)"] \| join(", "))"'` | volumePath 和 diskUUID 均为空或 null |
| 7.1.2d | Slot 变为 Available | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json \|`<br>`jq -r '.status.resourceStatuses[] \| select(.hostname=="<released-slot-hostname>") \| "state=\(.state), lastReleasedTime=\(.lastReleasedTime), machineRef=\(.machineRef)"'` | `state=Available`，`lastReleasedTime=null`，`machineRef=null` |
| 7.1.2e | vCenter 验证 | `govc datastore.ls -dc=<datacenter> -ds=<datastore> <released-vm-folder>/` | vmdk 文件已不存在 |

### 7.2 ConsumerRef 自动清空

<!-- 前置：所有 slot Available（7.1 完成后） -->

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.2.1 | 清空前 — consumerRef | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.name}'` | 仍指向旧 MD |
| 7.2.2 | 清空前 — slot 状态 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}{"\n"}{end}'` | 全部 Available |
| 7.2.3 | 等待自动清空 | `sleep 30`<br>`kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef}'` | 空 |

### 7.3 ConsumerRef 切换

<!-- 前置：consumerRef 为空，<other-md> 的 Machine 仍卡在 Provisioning -->

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.3.1 | 触发重新 reconcile | `kubectl annotate vspheremachine <other-md-machine-name> -n <namespace>`<br>`kick=$(date +%s) --overwrite` |

等待 Machine 进入 Running。

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.3.2 | ConsumerRef 切换 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'` | `MachineDeployment/<other-md>` |
| 7.3.3 | Slot 分配 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'` | `<other-md>` failureDomain 对应 DC 的 slot InUse |
| 7.3.4 | Machine 状态 | `kubectl get machines -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<other-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}, node={.status.nodeRef.name}{"\n"}{end}'` | 1 个 Running |
| 7.3.5 | IP 与 slot 一致 | `kubectl get vspherevms -n <namespace>`<br>`-l cluster.x-k8s.io/deployment-name=<other-md>`<br>`-o jsonpath='{range .items[*]}{.metadata.name}: addresses={.status.addresses}{"\n"}{end}'` | IP 与 slot NIC1/NIC2 一致 |
| 7.3.6 | 格式化盘 | `ssh <new-worker-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT \| grep -E 'var/cpaas\|var/lib/containerd'"` | 2 块格式化挂载盘正确 |
| 7.3.7 | 空盘 | `ssh <new-worker-ip> "ls -l /dev/disk/by-capv/data-disk-1 /dev/disk/by-capv/data-disk-2"` | 2 块空盘 symlink 存在 |
| 7.3.8 | 工作负载集群 | `kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes -o wide` | 新 Worker Ready，NAME 为 slot hostname |

### 7.4 ClusterRef 凭据链

#### 7.4.1 正常 conditions

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.4.1 | ClusterRef conditions | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'` | `ClusterRefReady=True`，`VCenterAvailable=True` |

#### 7.4.2 ClusterRef 指向不存在的 Cluster

**操作：**

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-invalid-clusterref
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    name: non-existent-cluster
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host"
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.4.2a | 创建无效 ClusterRef 的 pool | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.4.2b | condition | `sleep 30`<br>`kubectl get vsphereresourcepool test-invalid-clusterref -n <namespace>`<br>`-o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'` | `ClusterRefReady=False (ClusterNotFound)` |

**操作（清理）：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.4.2c | 清理 | `kubectl delete vsphereresourcepool test-invalid-clusterref -n <namespace>` |

#### 7.4.3 ClusterRef 可修改（consumerRef 为空）

**操作：**

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-clusterref-mutable
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    name: <cluster-name>
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host"
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.4.3a | 创建 pool | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |
| 7.4.3b | 修改 clusterRef | `kubectl patch vsphereresourcepool test-clusterref-mutable -n <namespace> --type=merge`<br>`-p '{"spec":{"clusterRef":{"name":"another-cluster"}}}'` |

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.4.3c | 修改成功 | 上一步 patch 命令 | 成功（无错误） |

**操作（清理）：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.4.3d | 清理 | `kubectl delete vsphereresourcepool test-clusterref-mutable -n <namespace>` |

#### 7.4.4 ClusterRef 不可修改（consumerRef 非空）

**操作 & 验证：**

| 步骤 | 操作 | 命令 | 预期结果 |
|------|------|------|----------|
| 7.4.4 | 修改已绑定 pool 的 clusterRef | `kubectl patch vsphereresourcepool <cp-pool-name> -n <namespace> --type=merge`<br>`-p '{"spec":{"clusterRef":{"name":"another-cluster"}}}'` | 被拒绝，`"cannot change clusterRef while consumerRef is set"` |

### 7.5 Pool 删除

#### 7.5.1 有 InUse slot 时删除被阻止

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.5.1a | 尝试删除 | `kubectl delete vsphereresourcepool <cp-pool-name> -n <namespace> --wait=false` |

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.5.1b | finalizer 阻止删除 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='deletionTimestamp={.metadata.deletionTimestamp}, finalizers={.metadata.finalizers}'` | `deletionTimestamp` 已设置，finalizer 仍存在 |

#### 7.5.2 无 Machine 引用时正常删除

**操作：**

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-pool-delete
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    name: <cluster-name>
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host-1"
  - hostname: "test-host-2"
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.5.2a | 创建测试 pool | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |
| 7.5.2b | 删除 pool | `sleep 10`<br>`kubectl delete vsphereresourcepool test-pool-delete -n <namespace>` |

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.5.2c | 确认已删除 | `kubectl get vsphereresourcepool test-pool-delete -n <namespace>` | NotFound |

#### 7.5.3 有回收任务运行时阻止删除

<!-- 可在 7.1.2b 观察到 reclaimStatus.state=Running 时尝试删除 pool 来验证 -->

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 7.5.3a | 回收进行中尝试删除 | `kubectl delete vsphereresourcepool <worker-pool-name> -n <namespace> --wait=false` |

**验证：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 7.5.3b | finalizer 阻止删除 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='deletionTimestamp={.metadata.deletionTimestamp}, finalizers={.metadata.finalizers}'` | finalizer 阻止删除，等回收完成后自动删除 |

---

## 8. Webhook 验证

**测试目的：** 验证准入 Webhook 对非法资源配置的拦截能力。手动提交各种非法 YAML，确保 VSphereResourcePool、KubeadmControlPlane、MachineDeployment 的创建/修改在违反约束时被正确拒绝。测试完成后清理临时资源。

### 8.1 VSphereResourcePool Webhook

**操作 & 验证（逐条提交非法配置，验证被拒绝）：**

| 步骤 | 测试场景 | 命令 | 预期结果 |
|------|----------|------|----------|
| 8.1.1 | clusterRef.name 为空 | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`kind: VSphereResourcePool`<br>`metadata:`<br>`  name: test-webhook-no-clusterref`<br>`  namespace: <namespace>`<br>`spec:`<br>`  clusterRef:`<br>`    apiVersion: cluster.x-k8s.io/v1beta1`<br>`    kind: Cluster`<br>`    name: ""`<br>`  datacenter: "<datacenter>"`<br>`  resources:`<br>`  - hostname: "test-host"`<br>`EOF` | 拒绝，`"clusterRef.name"` `"must be set"` |
| 8.1.2 | clusterRef.apiVersion 不匹配 | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`kind: VSphereResourcePool`<br>`metadata:`<br>`  name: test-webhook-bad-apiversion`<br>`  namespace: <namespace>`<br>`spec:`<br>`  clusterRef:`<br>`    apiVersion: v1`<br>`    kind: Cluster`<br>`    name: <cluster-name>`<br>`  datacenter: "<datacenter>"`<br>`  resources:`<br>`  - hostname: "test-host"`<br>`EOF` | 拒绝，`"must be cluster.x-k8s.io/v1beta1"` |
| 8.1.3 | clusterRef.kind 不匹配 | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`kind: VSphereResourcePool`<br>`metadata:`<br>`  name: test-webhook-bad-kind`<br>`  namespace: <namespace>`<br>`spec:`<br>`  clusterRef:`<br>`    apiVersion: cluster.x-k8s.io/v1beta1`<br>`    kind: MachineDeployment`<br>`    name: <cluster-name>`<br>`  datacenter: "<datacenter>"`<br>`  resources:`<br>`  - hostname: "test-host"`<br>`EOF` | 拒绝，`"must be Cluster"` |
| 8.1.4 | clusterRef.namespace 不匹配 | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`kind: VSphereResourcePool`<br>`metadata:`<br>`  name: test-webhook-bad-ns`<br>`  namespace: <namespace>`<br>`spec:`<br>`  clusterRef:`<br>`    apiVersion: cluster.x-k8s.io/v1beta1`<br>`    kind: Cluster`<br>`    name: <cluster-name>`<br>`    namespace: other-namespace`<br>`  datacenter: "<datacenter>"`<br>`  resources:`<br>`  - hostname: "test-host"`<br>`EOF` | 拒绝，`"must match pool namespace"` |
| 8.1.5 | consumerRef 非空时修改 clusterRef | `kubectl patch vsphereresourcepool <cp-pool-name> -n <namespace> --type=merge`<br>`-p '{"spec":{"clusterRef":{"name":"another-cluster"}}}' 2>&1` | 拒绝，`"cannot change clusterRef while consumerRef is set"` |

### 8.2 KCP Consumer Webhook — 引用已被绑定的 pool

**操作：**

先创建引用已绑定 CP pool 的 template：

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereMachineTemplate
metadata:
  name: test-webhook-kcp-template
  namespace: <namespace>
spec:
  template:
    spec:
      server: "<vsphere-server>"
      template: "<vm-template-path>"
      cloneMode: linkedClone
      datastore: "<datastore>"
      diskGiB: 300
      memoryMiB: 8192
      numCPUs: 4
      os: Linux
      powerOffMode: trySoft
      network:
        devices:
        - networkName: "<nic1-network>"
      resourcePoolRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: VSphereResourcePool
        name: <cp-pool-name>
        namespace: <namespace>
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 8.2.1 | 创建 template | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |

**验证：**

| 步骤 | 测试场景 | 命令 | 预期结果 |
|------|----------|------|----------|
| 8.2.2 | 创建引用该 template 的 KCP | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: controlplane.cluster.x-k8s.io/v1beta1`<br>`kind: KubeadmControlPlane`<br>`metadata:`<br>`  name: test-webhook-kcp`<br>`  namespace: <namespace>`<br>`spec:`<br>`  replicas: 1`<br>`  version: "<k8s-version>"`<br>`  machineTemplate:`<br>`    infrastructureRef:`<br>`      apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`      kind: VSphereMachineTemplate`<br>`      name: test-webhook-kcp-template`<br>`  kubeadmConfigSpec:`<br>`    clusterConfiguration: {}`<br>`    initConfiguration: {}`<br>`    joinConfiguration: {}`<br>`EOF` | 拒绝，`"resource pool <cp-pool-name> is bound to"` |

**操作（清理）：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 8.2.3 | 清理 | `kubectl delete vspheremachinetemplate test-webhook-kcp-template -n <namespace> 2>/dev/null` |

### 8.3 MachineDeployment Consumer Webhook

#### 8.3.1 两个 MD 引用同一个 pool 模板

**操作 & 验证：**

| 步骤 | 测试场景 | 命令 | 预期结果 |
|------|----------|------|----------|
| 8.3.1 | 创建重复引用 pool 的 MD | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: cluster.x-k8s.io/v1beta1`<br>`kind: MachineDeployment`<br>`metadata:`<br>`  name: test-webhook-md-dup`<br>`  namespace: <namespace>`<br>`spec:`<br>`  clusterName: <cluster-name>`<br>`  replicas: 1`<br>`  selector:`<br>`    matchLabels:`<br>`      nodepool: test-dup`<br>`  template:`<br>`    metadata:`<br>`      labels:`<br>`        cluster.x-k8s.io/cluster-name: <cluster-name>`<br>`        nodepool: test-dup`<br>`    spec:`<br>`      clusterName: <cluster-name>`<br>`      version: "<k8s-version>"`<br>`      bootstrap:`<br>`        configRef:`<br>`          apiVersion: bootstrap.cluster.x-k8s.io/v1beta1`<br>`          kind: KubeadmConfigTemplate`<br>`          name: <worker-bootstrap-name>`<br>`      infrastructureRef:`<br>`        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`        kind: VSphereMachineTemplate`<br>`        name: <worker-template-name>`<br>`EOF` | 拒绝，duplicate reference |

#### 8.3.2 MD 引用已被 KCP 绑定的 pool

**操作：**

先创建引用 CP pool 的 template：

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereMachineTemplate
metadata:
  name: test-webhook-md-template
  namespace: <namespace>
spec:
  template:
    spec:
      server: "<vsphere-server>"
      template: "<vm-template-path>"
      cloneMode: linkedClone
      datastore: "<datastore>"
      diskGiB: 300
      memoryMiB: 4096
      numCPUs: 2
      os: Linux
      powerOffMode: trySoft
      network:
        devices:
        - networkName: "<nic1-network>"
      resourcePoolRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: VSphereResourcePool
        name: <cp-pool-name>
        namespace: <namespace>
```

| 步骤 | 操作 | 命令 |
|------|------|------|
| 8.3.2a | 创建 template | `cat <<EOF \| kubectl apply -f -`<br>(上述 YAML)<br>`EOF` |

**验证：**

| 步骤 | 测试场景 | 命令 | 预期结果 |
|------|----------|------|----------|
| 8.3.2b | 创建引用该 template 的 MD | `cat <<EOF \| kubectl apply -f - 2>&1`<br>`apiVersion: cluster.x-k8s.io/v1beta1`<br>`kind: MachineDeployment`<br>`metadata:`<br>`  name: test-webhook-md-bound`<br>`  namespace: <namespace>`<br>`spec:`<br>`  clusterName: <cluster-name>`<br>`  replicas: 1`<br>`  selector:`<br>`    matchLabels:`<br>`      nodepool: test-bound`<br>`  template:`<br>`    metadata:`<br>`      labels:`<br>`        cluster.x-k8s.io/cluster-name: <cluster-name>`<br>`        nodepool: test-bound`<br>`    spec:`<br>`      clusterName: <cluster-name>`<br>`      version: "<k8s-version>"`<br>`      bootstrap:`<br>`        configRef:`<br>`          apiVersion: bootstrap.cluster.x-k8s.io/v1beta1`<br>`          kind: KubeadmConfigTemplate`<br>`          name: <worker-bootstrap-name>`<br>`      infrastructureRef:`<br>`        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1`<br>`        kind: VSphereMachineTemplate`<br>`        name: test-webhook-md-template`<br>`EOF` | 拒绝，`"resource pool <cp-pool-name> is bound to KubeadmControlPlane"` |

**操作（清理）：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 8.3.2c | 清理 | `kubectl delete vspheremachinetemplate test-webhook-md-template -n <namespace> 2>/dev/null` |

---

## 9. 集群删除

**测试目的：** 验证集群删除时的级联清理行为：所有 Machine、VSphereVM、VSphereMachine、VSphereCluster、Cluster 资源被正确删除，vCenter 中 VM 被清理，同时静态资源池（VSphereResourcePool）作为独立生命周期的资源不被级联删除，其 slot 从 InUse 转为 Released。

**操作：**

| 步骤 | 操作 | 命令 |
|------|------|------|
| 9.1 | 删除集群 | `kubectl delete cluster <cluster-name> -n <namespace>` |

等待所有 Machine 删除完成（`kubectl get machines -n <namespace> -l cluster.x-k8s.io/cluster-name=<cluster-name> -w`）。

**验证（级联删除）：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 9.2 | 所有资源清理 | `for kind in vspherevms vspheremachines vspherecluster cluster; do`<br>`kubectl get $kind -n <namespace> -l cluster.x-k8s.io/cluster-name=<cluster-name> 2>&1`<br>`done` | 全部 No resources found 或 NotFound |

**验证（资源池独立性）：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 9.3 | pool 仍存在 | `kubectl get vsphereresourcepools -n <namespace>` | CP pool 和 Worker pool 仍存在 |
| 9.4 | CP pool slot 状态 | `kubectl get vsphereresourcepool <cp-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'` | 之前 InUse → Released |
| 9.5 | Worker pool slot 状态 | `kubectl get vsphereresourcepool <worker-pool-name> -n <namespace>`<br>`-o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'` | 同上 |

**验证（vCenter 清理）：**

| 步骤 | 检查项 | 命令 | 预期结果 |
|------|--------|------|----------|
| 9.6 | VM 清理 | `govc find / -type m -name '<cluster-name>-*'` | 无输出 |
