# VSphere 集群功能验证测试计划

> 按章节顺序执行。每个测试用例包含：前置条件、操作步骤、验证命令和期望结果。
> 占位符说明：`<cluster-name>`、`<namespace>`、`<kcp-name>` 等需替换为实际值。

---

## 1. 前置条件

### 管理集群
- [ ] global 集群就绪，kubectl 可访问
- [ ] CAPI + CAPV controller 已安装并运行
- [ ] ClusterResourceSet 已启用

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
- [ ] 负载均衡器已配置，VIP 对应的 realserver 指向所有 CP 节点 IP（5 个），端口 6443
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
- [ ] 容量充足（系统盘 300G × 9 节点 + 持久盘按 slot 定义）

### 静态资源池
- [ ] CP pool：5 个 slot，分布 3+2（DC1 3 个，DC2 2 个），每个 slot 配置：
  - hostname、双网卡（NIC1 + NIC2，各含 IP/网关/DNS）
  - 持久盘 3 块：var-cpaas（/var/cpaas）、var-lib-containerd（/var/lib/containerd）、var-lib-etcd（/var/lib/etcd）
- [ ] Worker pool：4 个 slot，分布 2+2（DC1 2 个，DC2 2 个），每个 slot 配置：
  - hostname、双网卡（NIC1 + NIC2，各含 IP/网关/DNS）
  - 持久盘 2 块：var-cpaas（/var/cpaas）、var-lib-containerd（/var/lib/containerd）
- [ ] 2 个 MachineDeployment 定义，引用同一个 worker pool，分别绑定不同的 failureDomain
- [ ] VSphereMachineTemplate 的 network.devices 配置 2 个网卡（与 pool slot 中的 2 个 network 条目对应）

### 计算资源
- [ ] CP 节点：至少 4C/8G/300G 系统盘
- [ ] Worker 节点：至少 2C/4G/300G 系统盘

---

## 2. 集群创建

> 创建集群并验证 CP、Worker、资源池、持久盘、双网卡全部就绪。

### 操作
```bash
kubectl apply -f <env-extension-dir>/
```

### 验证

**2.1 Cluster 与 VSphereCluster 状态**
```bash
kubectl get cluster <cluster-name> -n <namespace> -o jsonpath='{.status.phase}'
# 期望：Provisioned

kubectl get vspherecluster <cluster-name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}'
# 期望：Ready=True, VCenterAvailable=True, FailureDomainsAvailable=True
```

**2.2 Control Plane 节点**
```bash
# 3 副本全部 Running
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> -o wide
# 期望：3 个 Machine，Phase=Running

# hostname 与 slot 一致
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.status.nodeRef.name}{"\n"}{end}'
# 期望：输出为 CP pool 中已分配 slot 的 hostname（如 master-01、master-02、master-03）

# 双网卡 IP（每个 VM 应有 2 个 IP）
kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'
# 期望：每个 VM 2 个 IP，与 slot 中 NIC1/NIC2 的 ip 一致（不含前缀长度）
```

**2.3 Worker 节点**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/cluster-name=<cluster-name>,!cluster.x-k8s.io/control-plane-name
# 期望：
#   其中一个 MD 的 Machine Phase=Running（先抢到 pool 的 MD）
#   另一个 MD 的 Machine Phase=Provisioning（被 consumerRef 拦截）
```

**2.4 资源池状态**
```bash
# --- CP pool ---
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'
# 期望：consumerRef=KubeadmControlPlane/<kcp-name>

kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：3 个 InUse（machineRef 指向对应 VSphereMachine），2 个 Available

kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}'
# 期望：ClusterRefReady=True, VCenterAvailable=True

# --- Worker pool ---
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'
# 期望：consumerRef=MachineDeployment/<md-name>（先抢到的 MD）

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：1 个 InUse，其余 Available
```

**2.5 Slot 与 Machine 匹配**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json | \
  jq -r '.status.resourceStatuses[] | select(.state=="InUse") | "\(.hostname) -> \(.machineRef.name)"'
# 对输出的每一行验证 annotation：
kubectl get vspheremachine <machineRef-name> -n <namespace> \
  -o jsonpath='{.metadata.annotations.infrastructure\.cluster\.x-k8s\.io/resource-slot-hostname}'
# 期望：与 slot hostname 一致
```

**2.6 持久盘挂载**

CP 节点（3 块盘）：
```bash
ssh <cp-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT"
# 期望包含：
#   <disk>  <cp_var_cpaas_size_gib>G        ext4  /var/cpaas
#   <disk>  <cp_var_lib_containerd_size_gib>G  ext4  /var/lib/containerd
#   <disk>  <cp_var_lib_etcd_size_gib>G      ext4  /var/lib/etcd

ssh <cp-node-ip> "mount | grep -cE '/var/cpaas|/var/lib/containerd|/var/lib/etcd'"
# 期望：3

ssh <cp-node-ip> "df -T /var/cpaas /var/lib/containerd /var/lib/etcd"
# 期望：Type 列均为 ext4

ssh <cp-node-ip> "systemctl is-active containerd kubelet"
# 期望：两个均为 active
```

Worker 节点（2 块盘）：
```bash
ssh <worker-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT"
# 期望包含：
#   <disk>  <worker_var_cpaas_size_gib>G        ext4  /var/cpaas
#   <disk>  <worker_var_lib_containerd_size_gib>G  ext4  /var/lib/containerd

ssh <worker-node-ip> "mount | grep -cE '/var/cpaas|/var/lib/containerd'"
# 期望：2

ssh <worker-node-ip> "df -T /var/cpaas /var/lib/containerd"
# 期望：Type 列均为 ext4
```

**2.7 工作负载集群可用性**
```bash
kubectl get secret <cluster-name>-kubeconfig -n <namespace> \
  -o jsonpath='{.data.value}' | base64 -d > /tmp/<cluster-name>.kubeconfig

kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes -o wide
# 期望：3 个 CP + 1 个 Worker 全部 Ready，node name 与 slot hostname 一致
```

**2.8 双网卡**
```bash
ssh <node-ip> "ip -4 addr show | grep -E 'inet '"
# 期望：至少 2 个非 loopback IP，与 slot NIC1/NIC2 一致

ssh <node-ip> "ip route"
# 期望：default via <nic1_gateway>
```

---

## 3. 故障域

> CP pool 5 slot 按 3+2 分布（DC1 3 个，DC2 2 个）。
> Worker pool 4 slot 按 2+2 分布（DC1 2 个，DC2 2 个）。
> md-0 绑定 dz-dc1（DC1），md-1 绑定 dz-dc2（DC2）。

### 3.1 CP 跨故障域分布

**3.1.1 Slot datacenter 分布**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | "\(.hostname): datacenter=\(.datacenter // "<pool-default>")"'
# 期望：3 个 DC1，2 个 DC2
```

**3.1.2 VSphereVM 实际 datacenter**
```bash
kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: datacenter={.spec.datacenter}{"\n"}{end}'
# 期望：与对应 slot datacenter 一致
```

**3.1.3 Machine failureDomain**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: failureDomain={.spec.failureDomain}{"\n"}{end}'
# 期望：DC1 的 Machine → <dz-dc1-name>，DC2 的 Machine → <dz-dc2-name>
```

**3.1.4 Slot-Machine datacenter 交叉验证**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] as $slot | .status.resourceStatuses[] |
    select(.state=="InUse" and .hostname==$slot.hostname) |
    "\(.hostname): slot-dc=\($slot.datacenter // "<pool-default>"), machine=\(.machineRef.name)"'
# 对每行验证 VSphereVM datacenter：
kubectl get vspherevm <machineRef-name> -n <namespace> -o jsonpath='{.spec.datacenter}'
# 期望：与 slot-dc 一致
```

### 3.2 Worker 故障域约束

> `<bound-md>` 绑定 failureDomain 对应 `<bound-dc>`。

**3.2.1 InUse slot datacenter**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] as $slot | .status.resourceStatuses[] |
    select(.state=="InUse" and .hostname==$slot.hostname) |
    "\(.hostname): datacenter=\($slot.datacenter // "<pool-default>")"'
# 期望：所有 InUse slot 的 datacenter 均为 <bound-dc>
```

**3.2.2 另一个 DC 的 slot 未被分配**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] as $slot | .status.resourceStatuses[] |
    select(.hostname==$slot.hostname) |
    "\(.hostname): datacenter=\($slot.datacenter // "<pool-default>"), state=\(.state)"'
# 期望：非 <bound-dc> 的 slot 状态为 Available
```

**3.2.3 VSphereVM datacenter**
```bash
kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: datacenter={.spec.datacenter}{"\n"}{end}'
# 期望：全部为 <bound-dc>
```

### 3.3 故障域不匹配场景

**前置**：`<bound-md>` replicas=2，占满该 DC 的 2 个 slot

**操作**：
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=3
```

**3.3.1 第 3 个 Machine 卡住**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}{"\n"}{end}'
# 期望：2 个 Running + 1 个 Provisioning

# 另一个 DC 的 slot 不会被分配
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}{"\n"}{end}'
# 期望：<bound-dc> 的 2 个 InUse，另一个 DC 的 2 个 Available
```

**3.3.2 错误信息**
```bash
kubectl get vspheremachines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o json | jq -r '.items[] | select(.status.ready != true) | .metadata.name' | head -1

kubectl get vspheremachine <stuck-machine-name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}: {.message}{"\n"}{end}'
# 期望：包含 "no available slots" 相关信息
```

**3.3.3 恢复**
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=1
```

---

## 4. 扩容

### 4.1 CP 扩容（3 → 5）

**前置**：KCP replicas=3，CP pool 3 InUse + 2 Available

**操作**：
```bash
kubectl patch kcp <kcp-name> -n <namespace> --type=merge -p '{"spec":{"replicas":5}}'
```

**4.1.1 Machine 状态**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}, node={.status.nodeRef.name}{"\n"}{end}'
# 期望：5 个 Machine，全部 Running
```

**4.1.2 CP Pool slot**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：5 个 InUse，0 个 Available
```

**4.1.3 hostname 和网络**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.status.nodeRef.name}{"\n"}{end}' | sort
# 期望：CP pool 中 5 个 slot 的 hostname

kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'
# 期望：每个 VM 2 个 IP，与 slot NIC1/NIC2 一致
```

**4.1.4 持久盘**
```bash
ssh <new-cp-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT | grep -E 'var/cpaas|var/lib/containerd|var/lib/etcd'"
# 期望：3 块盘，路径和大小与 slot 定义一致
```

**4.1.5 etcd 集群**
```bash
ssh <cp-node-ip> "ETCDCTL_API=3 etcdctl \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/peer.crt \
  --key=/etc/kubernetes/pki/etcd/peer.key \
  member list -w table"
# 期望：5 个成员，全部 started
```

**4.1.6 工作负载集群**
```bash
kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes
# 期望：5 个 CP Ready
```

### 4.2 Worker 扩容（1 → 2）

**前置**：`<bound-md>` replicas=1，绑定 failureDomain 对应 `<bound-dc>`

**操作**：
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=2
```

**4.2.1 Machine 状态**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}, node={.status.nodeRef.name}{"\n"}{end}'
# 期望：2 个 Running
```

**4.2.2 Slot 分配与 failureDomain**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：<bound-dc> 的 2 个 slot InUse，另一个 DC 的 slot Available

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] as $slot | .status.resourceStatuses[] |
    select(.state=="InUse" and .hostname==$slot.hostname) |
    "\(.hostname): datacenter=\($slot.datacenter // "<pool-default>")"'
# 期望：InUse slot 的 datacenter 全部为 <bound-dc>

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'
# 期望：不变
```

**4.2.3 hostname 与网络**
```bash
kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes \
  -l nodepool=<bound-md-nodepool-label> -o wide
# 期望：2 个 Worker Ready，NAME 为 <bound-dc> 中 slot hostname

ssh <new-worker-ip> "ip -4 addr show | grep 'inet ' | grep -v '127.0.0.1'"
# 期望：2 个 IP，与 slot NIC1/NIC2 一致
```

**4.2.4 持久盘**
```bash
ssh <new-worker-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT | grep -E 'var/cpaas|var/lib/containerd'"
# 期望：2 块盘，路径和大小与 slot 定义一致
```

**4.2.5 工作负载集群**
```bash
kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes
# 期望：原有节点 + 新 Worker 全部 Ready
```

---

## 5. 滚动更新

> **触发方式**：创建新 VSphereMachineTemplate（修改 memoryMiB 或 numCPUs），更新 KCP/MD 的 infrastructureRef。
>
> **重点**：验证滚动替换后 hostname、IP、持久盘被复用。
>
> **说明**：如果有配套新版本 Kubernetes 的 OS 模板，也可以在新 template 中引用新 OS 模板（修改 `spec.template.spec.template` 路径），同时更新 KCP/MD 的 `spec.version`，实现集群版本升级。验证方式相同。

### 5.1 CP 滚动更新

**前置**：KCP 5 副本 Running，CP pool 5 slot 全部 InUse

**5.1.1 记录更新前状态**
```bash
# slot → machine 映射
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json | \
  jq -r '.status.resourceStatuses[] | "\(.hostname): machine=\(.machineRef.name)"'

# 持久盘 VolumePath（更新后应完全一致）
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | "\(.hostname): \([.persistentDisks[] | "\(.name)=\(.volumePath)"] | join(", "))"'

# 节点 IP
kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'

# 写入 etcd 测试数据
ssh <cp-node-ip> "ETCDCTL_API=3 etcdctl \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/peer.crt \
  --key=/etc/kubernetes/pki/etcd/peer.key \
  put /capv-test/rolling-update-marker 'before-update'"
```

**5.1.2 触发滚动更新**
```bash
# 创建新 template（以修改 memoryMiB 为例）
cat <<EOF | kubectl apply -f -
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
EOF

kubectl patch kcp <kcp-name> -n <namespace> --type=merge \
  -p '{"spec":{"machineTemplate":{"infrastructureRef":{"name":"<cp-template-name>-v2"}}}}'
```

**5.1.3 观察滚动替换**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> -w
# 期望：逐个替换（先删旧再建新），直到全部完成
```

**5.1.4 验证 hostname 不变**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.status.nodeRef.name}{"\n"}{end}' | sort
# 期望：与更新前相同的 5 个 hostname
```

**5.1.5 验证 IP 复用**
```bash
kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/control-plane-name=<kcp-name> \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'
# 期望：与更新前一致
```

**5.1.6 验证持久盘复用**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | "\(.hostname): \([.persistentDisks[] | "\(.name)=\(.volumePath)"] | join(", "))"'
# 期望：与 5.1.1 记录完全一致

ssh <cp-node-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT | grep -E 'var/cpaas|var/lib/containerd|var/lib/etcd'"
# 期望：3 块盘，与更新前一致
```

**5.1.7 验证 etcd 数据保留**
```bash
ssh <cp-node-ip> "ETCDCTL_API=3 etcdctl \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/peer.crt \
  --key=/etc/kubernetes/pki/etcd/peer.key \
  get /capv-test/rolling-update-marker"
# 期望：before-update
```

**5.1.8 Pool slot**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：5 个 InUse，machineRef 指向新 Machine（名称与更新前不同）
```

**5.1.9 工作负载集群**
```bash
kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes -o wide
# 期望：5 个 CP Ready，IP 和 hostname 不变
```

### 5.2 Worker 滚动更新

**前置**：`<bound-md>` replicas≥1，worker slot InUse

**5.2.1 记录更新前状态**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.status.resourceStatuses[] | select(.state=="InUse") | "\(.hostname): machine=\(.machineRef.name)"'

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | "\(.hostname): \([.persistentDisks[] | "\(.name)=\(.volumePath)"] | join(", "))"'

kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'
```

**5.2.2 触发滚动更新**
```bash
cat <<EOF | kubectl apply -f -
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
EOF

kubectl patch md <bound-md> -n <namespace> --type=merge \
  -p '{"spec":{"template":{"spec":{"infrastructureRef":{"name":"<worker-template-name>-v2"}}}}}'
```

**5.2.3 观察滚动替换**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> -w
# 期望：旧 Machine Deleting → 新 Machine Running
```

**5.2.4 验证 hostname、IP、持久盘复用**
```bash
kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes \
  -l nodepool=<bound-md-nodepool-label> -o wide
# 期望：NAME 与更新前相同

kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.status.addresses}{"\n"}{end}'
# 期望：IP 与更新前一致

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | "\(.hostname): \([.persistentDisks[] | "\(.name)=\(.volumePath)"] | join(", "))"'
# 期望：VolumePath 与更新前一致

ssh <worker-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT | grep -E 'var/cpaas|var/lib/containerd'"
# 期望：2 块盘不变
```

**5.2.5 Pool slot**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：machineRef 指向新 Machine，hostname 不变

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.name}'
# 期望：不变
```

---

## 6. 缩容

### 6.1 Worker 缩容（2 → 1）

**操作**：
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=1
```

**6.1.1 Machine**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}{"\n"}{end}'
# 期望：1 个 Running
```

**6.1.2 Slot**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'
# 期望：1 个 InUse + 1 个 Released（lastReleasedTime 已设置，machineRef 保留） + 其余 Available
```

**6.1.3 ConsumerRef**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.name}'
# 期望：仍为 <bound-md>
```

**6.1.4 工作负载集群**
```bash
kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes \
  -l nodepool=<bound-md-nodepool-label>
# 期望：1 个 Worker Ready
```

### 6.2 Worker 超配

**前置**：`<bound-md>` replicas=1，该 DC 仍有 1 个 Available slot

**操作**：
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=3
```

**6.2.1 分配结果**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}{"\n"}{end}'
# 期望：2 个 Running + 1 个 Provisioning
```

**6.2.2 卡住原因**
```bash
kubectl get vspheremachines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md> \
  -o json | jq -r '.items[] | select(.status.ready != true) | .metadata.name'

kubectl get vspheremachine <stuck-machine-name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}: {.reason} - {.message}{"\n"}{end}'
# 期望：包含 "no available slots"
```

**6.2.3 恢复**
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=1
```

### 6.3 Worker 缩容至 0

**操作**：
```bash
kubectl scale md <bound-md> -n <namespace> --replicas=0
```

**6.3.1 Machine**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<bound-md>
# 期望：No resources found
```

**6.3.2 Slot**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'
# 期望：之前 InUse 的 slot → Released
```

**6.3.3 ConsumerRef**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.name}'
# 期望：仍存在（Released slot 未回收，pool 不是 fully reusable）
```

**6.3.4 vCenter**
```bash
govc find / -type m -name '<cluster-name>-*' | grep -i worker
# 期望：无输出
```

---

## 7. 静态资源池

### 7.1 Slot 生命周期：Released → Available

> 通过手动修改 lastReleasedTime 加速 releaseDelay 过期，验证磁盘回收和 slot 状态变化。

**前置**：worker pool 中至少 1 个 Released slot（第 6 章缩容后）

**7.1.1 确认 Released slot**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.status.resourceStatuses[] | select(.state=="Released") |
    "\(.hostname): lastReleasedTime=\(.lastReleasedTime)"'
# 期望：至少 1 个 Released
```

**7.1.2 确认可回收持久盘**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | select(.persistentDisks != null) |
    "\(.hostname): \([.persistentDisks[] | select(.volumePath != null and .volumePath != "") | "\(.name)=\(.volumePath)"] | join(", "))"'
# 期望：Released slot 有 VolumePath
```

**7.1.3 加速过期**
```bash
kubectl patch vsphereresourcepool <worker-pool-name> -n <namespace> \
  --type=merge --subresource=status \
  -p '{"status":{"resourceStatuses":[
    {"hostname":"<released-slot-hostname>","state":"Released","lastReleasedTime":"<25-hours-ago-RFC3339>"},
    ...
  ]}}'
# 注意：需包含所有 slot 的 status
```

**7.1.4 观察磁盘回收**
```bash
# 等待 ~30s 后检查
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.status.resourceStatuses[] | select(.hostname=="<released-slot-hostname>") |
    "state=\(.state), reclaimStatus=\(.reclaimStatus)"'
# 期望：reclaimStatus.state=Running → Completed
```

**7.1.5 VolumePath/DiskUUID 清空**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.spec.resources[] | select(.hostname=="<released-slot-hostname>") |
    "\(.hostname): \([.persistentDisks[] | "\(.name): volumePath=\(.volumePath), diskUUID=\(.diskUUID)"] | join(", "))"'
# 期望：volumePath 和 diskUUID 均为空或 null
```

**7.1.6 Slot 变为 Available**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> -o json | \
  jq -r '.status.resourceStatuses[] | select(.hostname=="<released-slot-hostname>") |
    "state=\(.state), lastReleasedTime=\(.lastReleasedTime), machineRef=\(.machineRef)"'
# 期望：state=Available, lastReleasedTime=null, machineRef=null
```

**7.1.7 vCenter 验证**
```bash
govc datastore.ls -dc=<datacenter> -ds=<datastore> <released-vm-folder>/
# 期望：vmdk 文件已不存在
```

### 7.2 ConsumerRef 自动清空

**前置**：所有 slot Available（7.1 完成后）

**7.2.1 清空前**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.name}'
# 期望：仍指向旧 MD

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}{"\n"}{end}'
# 期望：全部 Available
```

**7.2.2 等待清空**
```bash
sleep 30
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef}'
# 期望：空
```

### 7.3 ConsumerRef 切换

**前置**：consumerRef 为空，`<other-md>` 的 Machine 仍卡在 Provisioning

**7.3.1 触发重新 reconcile**
```bash
kubectl annotate vspheremachine <other-md-machine-name> -n <namespace> \
  kick=$(date +%s) --overwrite
```

**7.3.2 ConsumerRef 切换**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='consumerRef={.status.consumerRef.kind}/{.status.consumerRef.name}'
# 期望：MachineDeployment/<other-md>
```

**7.3.3 Slot 分配**
```bash
kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, machine={.machineRef.name}{"\n"}{end}'
# 期望：<other-md> failureDomain 对应 DC 的 slot InUse
```

**7.3.4 VM 正常创建**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<other-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase}, node={.status.nodeRef.name}{"\n"}{end}'
# 期望：1 个 Running

kubectl get vspherevms -n <namespace> \
  -l cluster.x-k8s.io/deployment-name=<other-md> \
  -o jsonpath='{range .items[*]}{.metadata.name}: addresses={.status.addresses}{"\n"}{end}'
# 期望：IP 与 slot NIC1/NIC2 一致

ssh <new-worker-ip> "lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT | grep -E 'var/cpaas|var/lib/containerd'"
# 期望：2 块盘正确挂载

kubectl --kubeconfig=/tmp/<cluster-name>.kubeconfig get nodes -o wide
# 期望：新 Worker Ready，NAME 为 slot hostname
```

### 7.4 ClusterRef 凭据链

**7.4.1 正常 conditions**
```bash
kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
# 期望：ClusterRefReady=True, VCenterAvailable=True
```

**7.4.2 ClusterRef 指向不存在的 Cluster**
```bash
cat <<EOF | kubectl apply -f -
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
EOF

sleep 30
kubectl get vsphereresourcepool test-invalid-clusterref -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
# 期望：ClusterRefReady=False (ClusterNotFound)

kubectl delete vsphereresourcepool test-invalid-clusterref -n <namespace>
```

**7.4.3 ClusterRef 可修改（consumerRef 为空）**
```bash
cat <<EOF | kubectl apply -f -
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
EOF

kubectl patch vsphereresourcepool test-clusterref-mutable -n <namespace> --type=merge \
  -p '{"spec":{"clusterRef":{"name":"another-cluster"}}}'
# 期望：成功

kubectl delete vsphereresourcepool test-clusterref-mutable -n <namespace>
```

**7.4.4 ClusterRef 不可修改（consumerRef 非空）**
```bash
kubectl patch vsphereresourcepool <cp-pool-name> -n <namespace> --type=merge \
  -p '{"spec":{"clusterRef":{"name":"another-cluster"}}}'
# 期望：被拒绝，"cannot change clusterRef while consumerRef is set"
```

### 7.5 Pool 删除

**7.5.1 有 InUse slot 时阻止删除**
```bash
kubectl delete vsphereresourcepool <cp-pool-name> -n <namespace> --wait=false

kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='deletionTimestamp={.metadata.deletionTimestamp}, finalizers={.metadata.finalizers}'
# 期望：deletionTimestamp 已设置，finalizer 仍存在
```

**7.5.2 无 Machine 引用时正常删除**
```bash
cat <<EOF | kubectl apply -f -
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
EOF

sleep 10
kubectl delete vsphereresourcepool test-pool-delete -n <namespace>
# 期望：成功删除

kubectl get vsphereresourcepool test-pool-delete -n <namespace>
# 期望：NotFound
```

**7.5.3 有回收任务运行时阻止删除**

> 可在 7.1.4 观察到 reclaimStatus.state=Running 时尝试删除 pool 来验证。

```bash
kubectl delete vsphereresourcepool <worker-pool-name> -n <namespace> --wait=false

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='deletionTimestamp={.metadata.deletionTimestamp}, finalizers={.metadata.finalizers}'
# 期望：finalizer 阻止删除，等回收完成后自动删除
```

---

## 8. Webhook 验证

> 手动 kubectl apply 非法 yaml，验证 webhook 拦截。用完即删。

### 8.1 VSphereResourcePool Webhook

**8.1.1 clusterRef.name 为空**
```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-webhook-no-clusterref
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    name: ""
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host"
EOF
# 期望：拒绝，"clusterRef.name" "must be set"
```

**8.1.2 clusterRef.apiVersion 不匹配**
```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-webhook-bad-apiversion
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: v1
    kind: Cluster
    name: <cluster-name>
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host"
EOF
# 期望：拒绝，"must be cluster.x-k8s.io/v1beta1"
```

**8.1.3 clusterRef.kind 不匹配**
```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-webhook-bad-kind
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: cluster.x-k8s.io/v1beta1
    kind: MachineDeployment
    name: <cluster-name>
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host"
EOF
# 期望：拒绝，"must be Cluster"
```

**8.1.4 clusterRef.namespace 不匹配**
```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereResourcePool
metadata:
  name: test-webhook-bad-ns
  namespace: <namespace>
spec:
  clusterRef:
    apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    name: <cluster-name>
    namespace: other-namespace
  datacenter: "<datacenter>"
  resources:
  - hostname: "test-host"
EOF
# 期望：拒绝，"must match pool namespace"
```

**8.1.5 consumerRef 非空时修改 clusterRef**
```bash
kubectl patch vsphereresourcepool <cp-pool-name> -n <namespace> --type=merge \
  -p '{"spec":{"clusterRef":{"name":"another-cluster"}}}' 2>&1
# 期望：拒绝，"cannot change clusterRef while consumerRef is set"
```

### 8.2 KCP Consumer Webhook

**8.2.1 引用已被绑定的 pool**
```bash
cat <<EOF | kubectl apply -f -
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
EOF

cat <<EOF | kubectl apply -f - 2>&1
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: test-webhook-kcp
  namespace: <namespace>
spec:
  replicas: 1
  version: "<k8s-version>"
  machineTemplate:
    infrastructureRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
      kind: VSphereMachineTemplate
      name: test-webhook-kcp-template
  kubeadmConfigSpec:
    clusterConfiguration: {}
    initConfiguration: {}
    joinConfiguration: {}
EOF
# 期望：拒绝，"resource pool <cp-pool-name> is bound to"

kubectl delete vspheremachinetemplate test-webhook-kcp-template -n <namespace> 2>/dev/null
```

### 8.3 MachineDeployment Consumer Webhook

**8.3.1 两个 MD 引用同一个 pool 模板**
```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: cluster.x-k8s.io/v1beta1
kind: MachineDeployment
metadata:
  name: test-webhook-md-dup
  namespace: <namespace>
spec:
  clusterName: <cluster-name>
  replicas: 1
  selector:
    matchLabels:
      nodepool: test-dup
  template:
    metadata:
      labels:
        cluster.x-k8s.io/cluster-name: <cluster-name>
        nodepool: test-dup
    spec:
      clusterName: <cluster-name>
      version: "<k8s-version>"
      bootstrap:
        configRef:
          apiVersion: bootstrap.cluster.x-k8s.io/v1beta1
          kind: KubeadmConfigTemplate
          name: <worker-bootstrap-name>
      infrastructureRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: VSphereMachineTemplate
        name: <worker-template-name>
EOF
# 期望：拒绝，duplicate reference
```

**8.3.2 MD 引用已被 KCP 绑定的 pool**
```bash
cat <<EOF | kubectl apply -f -
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
EOF

cat <<EOF | kubectl apply -f - 2>&1
apiVersion: cluster.x-k8s.io/v1beta1
kind: MachineDeployment
metadata:
  name: test-webhook-md-bound
  namespace: <namespace>
spec:
  clusterName: <cluster-name>
  replicas: 1
  selector:
    matchLabels:
      nodepool: test-bound
  template:
    metadata:
      labels:
        cluster.x-k8s.io/cluster-name: <cluster-name>
        nodepool: test-bound
    spec:
      clusterName: <cluster-name>
      version: "<k8s-version>"
      bootstrap:
        configRef:
          apiVersion: bootstrap.cluster.x-k8s.io/v1beta1
          kind: KubeadmConfigTemplate
          name: <worker-bootstrap-name>
      infrastructureRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: VSphereMachineTemplate
        name: test-webhook-md-template
EOF
# 期望：拒绝，"resource pool <cp-pool-name> is bound to KubeadmControlPlane"

kubectl delete vspheremachinetemplate test-webhook-md-template -n <namespace> 2>/dev/null
```

---

## 9. 集群删除

> 最后执行，验证级联删除和资源池独立性。

### 操作
```bash
kubectl delete cluster <cluster-name> -n <namespace>
```

### 验证

**9.1 级联删除**
```bash
kubectl get machines -n <namespace> \
  -l cluster.x-k8s.io/cluster-name=<cluster-name> -w
# 期望：逐个 Deleting，最终全部消失

for kind in vspherevms vspheremachines vspherecluster cluster; do
  kubectl get $kind -n <namespace> -l cluster.x-k8s.io/cluster-name=<cluster-name> 2>&1
done
# 期望：全部 No resources found 或 NotFound
```

**9.2 资源池独立性**
```bash
kubectl get vsphereresourcepools -n <namespace>
# 期望：CP pool 和 Worker pool 仍存在

kubectl get vsphereresourcepool <cp-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'
# 期望：之前 InUse → Released

kubectl get vsphereresourcepool <worker-pool-name> -n <namespace> \
  -o jsonpath='{range .status.resourceStatuses[*]}{.hostname}: state={.state}, lastReleasedTime={.lastReleasedTime}{"\n"}{end}'
# 期望：同上
```

**9.3 vCenter**
```bash
govc find / -type m -name '<cluster-name>-*'
# 期望：无输出
```
