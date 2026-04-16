# vSphere 故障域 (Failure Domain) 技术指南

本文档面向不了解 VMware 故障域的开发和运维人员，介绍 Cluster API Provider vSphere (CAPV) 中故障域的架构设计、工作流程、vCenter Tag 管理，以及一个完整的端到端示例。

---

## 一、概述与背景

### 1.1 什么是故障域

故障域 (Failure Domain) 是基础设施中的一个隔离边界。在同一个故障域内的资源可能会因为同一个故障（如电源故障、网络中断）而同时不可用。将工作负载分布到不同的故障域，可以确保单点故障不影响整体服务的可用性。

在公有云中，故障域通常是预定义的"可用区"(Availability Zone)。而 vSphere 没有原生的 Region/Zone 概念，CAPV 通过 **vCenter Tag** 机制在物理基础设施之上构建逻辑拓扑，实现与公有云类似的故障域能力。

### 1.2 vSphere 基础设施层级

在理解故障域之前，需要先了解 vSphere 的对象层级：

```
vCenter Server
  └── Datacenter (数据中心)
        ├── host (主机文件夹)
        │     ├── Cluster (计算集群, ClusterComputeResource)
        │     │     ├── ResourcePool (根资源池 "Resources", 可嵌套子池)
        │     │     └── Host (ESXi 主机, HostSystem)
        │     │           └── VM (虚拟机)
        │     └── Host (独立主机, 非集群管理)
        │           ├── ResourcePool
        │           └── VM
        ├── vm (虚拟机文件夹)
        │     └── Folder / VM
        ├── datastore (数据存储文件夹)
        │     └── Datastore
        └── network (网络文件夹)
              └── Network
```

> 这四个顶级文件夹对应 vSphere inventory 路径，例如：
> - 资源池路径：`/dc-bj-1/host/cluster-a/Resources/my-pool`
> - VM 文件夹路径：`/dc-bj-1/vm/my-folder`
> - 数据存储路径：`/dc-bj-1/datastore/shared-ds-1`

CAPV 中的故障域可以映射到上述层级中的不同对象：
- **Datacenter** — 数据中心级别的故障隔离
- **ComputeCluster** — 计算集群级别的故障隔离
- **HostGroup** — 主机组级别的故障隔离（同一集群内的不同主机组）

### 1.3 Host、ComputeCluster 与 HostGroup 的区别

这三个概念容易混淆，以下从层级关系、功能定位和故障域角色三个维度进行对比：

| | **Host (ESXi 主机)** | **ComputeCluster (计算集群)** | **HostGroup (主机组)** |
|---|---|---|---|
| **是什么** | 运行 ESXi 虚拟化程序的物理服务器，是实际承载 VM 的计算资源 | 多台 ESXi 主机的逻辑集合，统一管理和调度资源 | ComputeCluster 内部的一个主机子集分组，是 DRS 的逻辑概念 |
| **层级关系** | vSphere 最底层的计算单元 | 包含一组 Host，位于 Datacenter 下 | **必须属于某个 ComputeCluster**，是其中部分 Host 的分组，不能独立于 Cluster 存在 |
| **管理能力** | 独立运行 VM，但没有 DRS、HA 等高级调度能力（这些是 Cluster 级功能） | 提供 HA（高可用）、DRS（分布式资源调度）、vMotion（在线迁移）等集群级功能 | 本身不提供独立管理能力，依赖所在 ComputeCluster 的 DRS 进行 VM 放置 |
| **资源隔离** | 物理隔离——不同主机是不同的物理机器 | 逻辑隔离——不同集群有独立的资源池和调度策略 | 软隔离——通过 DRS 亲和规则将 VM 约束到特定主机子集，非强制绑定 |
| **故障域角色** | 不直接作为故障域类型，但 HostGroup 场景下 Zone 标签打在 Host 上 | 可作为 Zone（常见）或 Region | 只能作为 Zone，且必须配合 VM-Host 亲和规则使用 |
| **vSphere 对象类型** | `HostSystem` | `ClusterComputeResource` | 非独立 vSphere 对象，是 Cluster DRS 配置的一部分 |

简单来说：
- **Host** 是一台物理机器
- **ComputeCluster** 是一组物理机器组成的**独立管理单元**，有自己的资源池、DRS、HA 配置，不同 Cluster 之间资源完全隔离，VM 不会跨 Cluster 调度
- **HostGroup** 是同一个 ComputeCluster 内部的**主机子集标签**，所有 Host 仍然共享 Cluster 的调度策略和资源池，VM 放置靠 DRS 亲和规则约束，且默认是软约束（should 规则，资源不足时 DRS 可以违反）

> 类比：ComputeCluster 像不同的办公楼，各自有独立的物业管理和门禁；HostGroup 像同一栋楼内的不同楼层，共享物业管理，只是逻辑上划了区。

```
ComputeCluster (cluster-1)
  ├── HostGroup-A                ← DRS 分组
  │     ├── Host-1 (物理机)
  │     └── Host-2 (物理机)
  └── HostGroup-B                ← DRS 分组
        ├── Host-3 (物理机)
        └── Host-4 (物理机)
```

> **为什么 HostGroup 不是独立的 vSphere 对象？** HostGroup 是 ComputeCluster 的 DRS Group 配置项，不像 Datacenter 或 Cluster 那样出现在 vSphere inventory 树中。因此在 CAPV 中，HostGroup 类型的故障域需要将 Zone 标签打在组内的各个 Host 上（而非 HostGroup 本身），并且必须配置 VMGroup 和 VM-Host 亲和规则才能生效。

### 1.4 核心术语

| 术语 | 说明 |
|------|------|
| **Region** | 较大粒度的故障隔离单元（如：数据中心、地理区域） |
| **Zone** | 较小粒度的故障隔离单元（如：计算集群、主机组） |
| **Topology** | 故障域对应的具体 vSphere 资源（Datacenter、Cluster、Network、Datastore 等） |
| **Tag Category** | vCenter 标签类别，是标签的分组容器 |
| **Tag** | vCenter 标签，附加到 vSphere 对象上，用于标识该对象所属的 Region 或 Zone |
| **PlacementConstraint** | 放置约束，指定 VM 创建时使用的 ResourcePool 和 Folder |

---

## 二、架构设计

### 2.1 CRD 资源模型

CAPV 通过三种资源协同工作来实现故障域：

#### VSphereFailureDomain（集群级别资源）

定义"什么是一个故障域"。包含三个核心部分：

```yaml
spec:
  region:           # Region 定义
    name: "..."       # vCenter Tag 名称
    type: "..."       # Datacenter | ComputeCluster（不允许 HostGroup）
    tagCategory: "..."# vCenter Tag Category 名称
  zone:             # Zone 定义（结构同上）
    name: "..."
    type: "..."       # Datacenter | ComputeCluster | HostGroup
    tagCategory: "..."
  topology:         # 实际的 vSphere 基础设施
    datacenter: "..."          # (必填) Datacenter 名称
    computeCluster: "..."      # (可选) ComputeCluster 名称
    hosts:                     # (可选) 主机组信息
      vmGroupName: "..."         # VM 组名称
      hostGroupName: "..."       # Host 组名称
    networks: [...]            # (可选) 网络列表
    networkConfigurations: [...] # (可选) 详细网络配置
    datastore: "..."           # (可选) 数据存储
```

#### VSphereDeploymentZone（集群级别资源）

定义"如何使用一个故障域"。将故障域与具体的放置约束绑定：

```yaml
spec:
  server: "vcenter.example.com"                              # vCenter 地址
  failureDomain: "fd-zone-a"                                 # 引用的 VSphereFailureDomain 名称
  controlPlane: true                                          # 是否用于控制平面节点
  placementConstraint:
    resourcePool: "/dc-bj-1/host/cluster-a/Resources/my-pool" # 资源池
    folder: "/dc-bj-1/vm/my-folder"                           # VM 文件夹
```

#### VSphereCluster.spec.failureDomainSelector

通过标签选择器关联部署区域：

```yaml
spec:
  failureDomainSelector:
    matchLabels:
      env: production
```

- `nil`（不设置）: 禁用故障域功能
- `{}`（空选择器）: 选择所有 VSphereDeploymentZone
- 设置标签选择器: 仅选择匹配的 VSphereDeploymentZone

### 2.2 资源关系

```
VSphereCluster
  │
  │  spec.failureDomainSelector (标签选择器)
  │         │
  │         ▼
  │   ┌─────────────────────┐      ┌─────────────────────┐
  │   │ VSphereDeploymentZone│─────▶│ VSphereFailureDomain │
  │   │  - server            │      │  - region (tag)      │
  │   │  - placementConstraint│      │  - zone (tag)        │
  │   │  - controlPlane      │      │  - topology          │
  │   └─────────────────────┘      └─────────────────────┘
  │
  │   ┌─────────────────────┐      ┌─────────────────────┐
  │   │ VSphereDeploymentZone│─────▶│ VSphereFailureDomain │
  │   └─────────────────────┘      └─────────────────────┘
  │
  ▼
CAPI Machine
  │  spec.failureDomain = "VSphereDeploymentZone 名称"
  ▼
VSphereVM（使用故障域信息创建 VM）
```

### 2.3 两种拓扑场景

#### 场景一：Region = Datacenter, Zone = ComputeCluster

适用于：需要将节点分布到不同计算集群，支持单数据中心和跨数据中心部署。

```
Datacenter (dc-1)          ← Region 标签打在这里
  ├── Cluster (cluster-a)  ← Zone 标签打在这里
  └── Cluster (cluster-b)  ← Zone 标签打在这里
```

这是最常见的场景。CPI/CSI 能感知这种拓扑，会为 Node 自动添加 `topology.kubernetes.io/region` 和 `topology.kubernetes.io/zone` 标签。

> 注意：跨数据中心部署时，网络通常不同，需要使用外部负载均衡器或配置 BGP。

#### 场景二：Region = ComputeCluster, Zone = HostGroup

适用于：单个计算集群内按主机组（防火隔间）隔离，所有主机共享同一存储。

```
Cluster (cluster-1)             ← Region 标签打在这里
  ├── HostGroup (hg-a)
  │     ├── Host-1              ← Zone 标签打在 Host 上
  │     └── Host-2
  └── HostGroup (hg-b)
        ├── Host-3              ← Zone 标签打在 Host 上
        └── Host-4
```

---

## 三、vCenter Tag 管理

### 3.1 Tag 系统概念

vCenter 的 Tag 系统由两层组成：

- **Tag Category (标签类别)**: 标签的分组容器。每个类别定义：
  - 名称（如 `k8s-region`、`k8s-zone`）
  - 可关联的对象类型（Datacenter、ClusterComputeResource、HostSystem）
  - 基数（SINGLE 或 MULTIPLE）
- **Tag (标签)**: 属于某个类别的具体标签值（如 `region-east`、`zone-cluster-a`）

标签可以附加 (attach) 到 vSphere 对象上。CAPV 通过检查 vSphere 对象上是否有预期的标签来验证故障域配置。

### 3.2 Tag 与故障域类型的对应关系

| 故障域类型 | 标签附加的对象类型 | vSphere 对象 |
|-----------|-------------------|-------------|
| `Datacenter` | Datacenter | 数据中心 |
| `ComputeCluster` | ClusterComputeResource | 计算集群 |
| `HostGroup` | HostSystem | ESXi 主机 |

### 3.3 使用 govc 管理标签

[govc](https://github.com/vmware/govmomi/tree/main/govc) 是 VMware 官方的命令行工具，可以方便地管理 vCenter 标签。

#### 环境变量配置

```bash
export GOVC_URL="https://vcenter.example.com/sdk"
export GOVC_USERNAME="administrator@vsphere.local"
export GOVC_PASSWORD="your-password"
export GOVC_INSECURE=true  # 如果使用自签名证书
```

#### 创建 Tag Category

```bash
# 创建 Region 类别，可关联到 Datacenter 对象
govc tags.category.create -t Datacenter k8s-region

# 创建 Zone 类别，可关联到 ClusterComputeResource 对象
govc tags.category.create -t ClusterComputeResource k8s-zone
```

> `-t` 参数指定类别可以关联的 vSphere 对象类型。注意 govc 中使用的是 vSphere API 对象类型名称，与 VSphereFailureDomain 中 `type` 字段的对应关系为：
> | FailureDomain type | govc `-t` 参数 |
> |---------------------|-------------------------|
> | `Datacenter` | `Datacenter` |
> | `ComputeCluster` | `ClusterComputeResource` |
> | `HostGroup` | `HostSystem` |

#### 创建 Tag

```bash
# 在 k8s-region 类别下创建标签
govc tags.create -c k8s-region region-bj

# 在 k8s-zone 类别下创建标签
govc tags.create -c k8s-zone zone-cluster-a
govc tags.create -c k8s-zone zone-cluster-b
```

#### 将 Tag 附加到 vSphere 对象

```bash
# 将 region 标签附加到 Datacenter
govc tags.attach k8s-region/region-bj /dc-bj-1

# 将 zone 标签附加到 ComputeCluster
govc tags.attach k8s-zone/zone-cluster-a /dc-bj-1/host/cluster-a
govc tags.attach k8s-zone/zone-cluster-b /dc-bj-1/host/cluster-b
```

> 标签附加的格式为 `<category>/<tag> <对象路径>`。

#### 验证标签

```bash
# 列出所有标签
govc tags.ls

# 列出某个标签附加到的对象
govc tags.attached.ls k8s-region/region-bj

# 列出某个对象上附加的所有标签
govc tags.attached.ls -r /dc-bj-1
```

### 3.4 Tag 与 CPI/CSI 的关系

vSphere Cloud Provider Interface (CPI) 和 Container Storage Interface (CSI) 使用与 CAPV **相同的** Tag 系统来感知拓扑：

- **CPI** 根据 Tag 为 Kubernetes Node 自动设置 `topology.kubernetes.io/region` 和 `topology.kubernetes.io/zone` 标签
- **CSI** 利用 Node 上的拓扑标签进行 topology-aware 存储卷调度

因此，在 vCenter 中配置的 Tag Category 名称需要与 CPI 配置文件中的 `region` 和 `zone` 参数一致。这样 CAPV 故障域和 CPI/CSI 拓扑感知就能使用同一套标签体系。

---

## 四、工作流详解

### 4.1 整体流程

```
┌──────────────┐    ┌─────────────────────┐    ┌───────────────────────┐
│  1. vCenter  │    │ 2. 创建 K8s CR 资源   │    │ 3. 控制器自动验证      │
│  Tag 配置    │───▶│ FailureDomain +      │───▶│ 标签/拓扑/放置约束     │
│              │    │ DeploymentZone       │    │ 标记 Ready            │
└──────────────┘    └─────────────────────┘    └───────────┬───────────┘
                                                           │
                    ┌─────────────────────┐                │
                    │ 6. VM 使用故障域信息  │                ▼
                    │ 创建到正确位置       │◀──  ┌───────────────────────┐
                    └─────────────────────┘    │ 4. VSphereCluster     │
                                               │ 发现并关联故障域       │
                              ▲                └───────────┬───────────┘
                              │                            │
                    ┌─────────────────────┐                ▼
                    │ 5. CAPI 调度器       │    ┌───────────────────────┐
                    │ 为 Machine 分配域    │◀── │ Status.FailureDomains │
                    └─────────────────────┘    │ 填充可用域             │
                                               └───────────────────────┘
```

### 4.2 准备阶段

1. **创建 VSphereFailureDomain CR** — 声明故障域的 Region/Zone 定义和拓扑信息
2. **创建 VSphereDeploymentZone CR** — 引用 FailureDomain 并指定放置约束

> Tag Category 和 Tag 不需要手动创建。VSphereDeploymentZone 控制器在调谐时会自动创建所需的 Tag Category 和 Tag，并将 Tag 附加到对应的 vSphere 对象上。

### 4.3 控制器调谐阶段

VSphereDeploymentZone 控制器会自动执行以下验证并更新状态条件：

```
┌────────────────────────────────────┐
│  验证 vCenter 连通性               │ → VCenterAvailable
│  (使用 spec.server 地址连接)       │
└──────────────┬─────────────────────┘
               ▼
┌────────────────────────────────────┐
│  验证放置约束                      │ → PlacementConstraintReady
│  - ResourcePool 是否存在           │
│  - Folder 是否存在                 │
└──────────────┬─────────────────────┘
               ▼
┌────────────────────────────────────┐
│  创建并验证故障域                  │ → FailureDomainValidated
│  - 自动创建 Tag Category 和 Tag   │
│  - 自动将 Tag 附加到 vSphere 对象  │
│  - 验证 Tag 是否正确附加           │
│  - ComputeCluster 是否存在         │
│  - Datastore 是否存在              │
│  - Network 是否存在                │
│  - (HostGroup) 亲和规则是否正确    │
└──────────────┬─────────────────────┘
               ▼
┌────────────────────────────────────┐
│  所有验证通过                      │ → Ready = true
└────────────────────────────────────┘
```

### 4.4 VM 放置阶段

1. **VSphereCluster 控制器** 根据 `failureDomainSelector` 列出匹配的 VSphereDeploymentZone，过滤出 Ready 状态的，填充到 `VSphereCluster.Status.FailureDomains`
2. **CAPI 调度器**（KCP 控制器）根据可用域列表为每个 Machine 分配一个域，设置 `Machine.Spec.FailureDomain = <VSphereDeploymentZone 名称>`
3. **VSphereVM 控制器** 读取 Machine 的故障域，获取对应的 VSphereDeploymentZone 和 VSphereFailureDomain
4. 用故障域信息**覆盖** VM 创建参数：
   - `Server` ← VSphereDeploymentZone.spec.server
   - `Datacenter` ← VSphereFailureDomain.spec.topology.datacenter
   - `ResourcePool` ← VSphereDeploymentZone.spec.placementConstraint.resourcePool
   - `Folder` ← VSphereDeploymentZone.spec.placementConstraint.folder
   - `Datastore` ← VSphereFailureDomain.spec.topology.datastore
   - `Networks` ← VSphereFailureDomain.spec.topology.networks 或 networkConfigurations
5. （HostGroup 场景）VM 创建后，将其加入 VMGroup，DRS 根据 VM-Host 亲和规则将 VM 调度到对应的 HostGroup

---

## 五、完整示例

### 5.1 场景说明

```
vCenter: vcenter.example.com
  └── Datacenter: dc-bj-1
        ├── Cluster: cluster-a
        │     └── ResourcePool: /dc-bj-1/host/cluster-a/Resources/capv-pool
        ├── Cluster: cluster-b
        │     └── ResourcePool: /dc-bj-1/host/cluster-b/Resources/capv-pool
        ├── Datastore: shared-ds-1
        └── Network: VM Network
```

目标：将 Kubernetes 控制平面节点分布到 `cluster-a` 和 `cluster-b` 两个计算集群中。

### 5.2 第一步：vCenter Tag 配置

```bash
# 设置 govc 环境变量
export GOVC_URL="https://vcenter.example.com/sdk"
export GOVC_USERNAME="administrator@vsphere.local"
export GOVC_PASSWORD="password"
export GOVC_INSECURE=true

# 1. 创建 Tag Category
govc tags.category.create -t Datacenter k8s-region
govc tags.category.create -t ClusterComputeResource k8s-zone

# 2. 创建 Tag
govc tags.create -c k8s-region region-bj
govc tags.create -c k8s-zone zone-cluster-a
govc tags.create -c k8s-zone zone-cluster-b

# 3. 将 Tag 附加到 vSphere 对象
govc tags.attach k8s-region/region-bj /dc-bj-1
govc tags.attach k8s-zone/zone-cluster-a /dc-bj-1/host/cluster-a
govc tags.attach k8s-zone/zone-cluster-b /dc-bj-1/host/cluster-b

# 4. 验证
govc tags.attached.ls k8s-region/region-bj
# 输出: Datacenter:datacenter-1 /dc-bj-1

govc tags.attached.ls k8s-zone/zone-cluster-a
# 输出: ClusterComputeResource:domain-c1001 /dc-bj-1/host/cluster-a
```

### 5.3 第二步：创建 VSphereFailureDomain

```yaml
# fd-zone-a.yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereFailureDomain
metadata:
  name: fd-zone-a
spec:
  region:
    name: region-bj            # Tag 名称，必须与 vCenter 中创建的一致
    type: Datacenter            # Region 对应 Datacenter 级别
    tagCategory: k8s-region     # Tag Category 名称
  zone:
    name: zone-cluster-a        # Tag 名称
    type: ComputeCluster        # Zone 对应 ComputeCluster 级别
    tagCategory: k8s-zone       # Tag Category 名称
  topology:
    datacenter: dc-bj-1                       # vSphere Datacenter 名称
    computeCluster: cluster-a                  # vSphere Cluster 名称
    datastore: shared-ds-1                     # 数据存储名称
    networks:
      - VM Network                             # 网络名称（简单模式）
    # 或者使用 networkConfigurations 配置更详细的网络参数（与 networks 二选一）：
    # networkConfigurations:
    #   - networkName: VM Network
    #     dhcp4: true
    #     nameservers:
    #       - 8.8.8.8
    #     searchDomains:
    #       - example.com
---
# fd-zone-b.yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereFailureDomain
metadata:
  name: fd-zone-b
spec:
  region:
    name: region-bj
    type: Datacenter
    tagCategory: k8s-region
  zone:
    name: zone-cluster-b
    type: ComputeCluster
    tagCategory: k8s-zone
  topology:
    datacenter: dc-bj-1
    computeCluster: cluster-b
    datastore: shared-ds-1
    networks:
      - VM Network
```

### 5.4 第三步：创建 VSphereDeploymentZone

```yaml
# dz-zone-a.yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereDeploymentZone
metadata:
  name: dz-zone-a
  labels:
    env: production           # 用于 VSphereCluster 的标签选择器
spec:
  server: vcenter.example.com
  failureDomain: fd-zone-a    # 引用 VSphereFailureDomain 名称
  controlPlane: true           # 允许部署控制平面节点（设为 false 则仅用于 Worker 节点）
  placementConstraint:
    resourcePool: /dc-bj-1/host/cluster-a/Resources/capv-pool
    folder: /dc-bj-1/vm/capv-vms
---
# dz-zone-b.yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereDeploymentZone
metadata:
  name: dz-zone-b
  labels:
    env: production
spec:
  server: vcenter.example.com
  failureDomain: fd-zone-b
  controlPlane: true
  placementConstraint:
    resourcePool: /dc-bj-1/host/cluster-b/Resources/capv-pool
    folder: /dc-bj-1/vm/capv-vms
```

### 5.5 第四步：配置 VSphereCluster

在 VSphereCluster 中启用故障域选择：

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: VSphereCluster
metadata:
  name: my-cluster
  namespace: default
spec:
  server: vcenter.example.com
  thumbprint: "..."
  # 选择所有 label 为 env=production 的 VSphereDeploymentZone
  failureDomainSelector:
    matchLabels:
      env: production
  # ... 其他配置
```

### 5.6 第五步：验证

```bash
# 查看 VSphereDeploymentZone 状态
kubectl get vspheredeploymentzones
# NAME        READY
# dz-zone-a   true
# dz-zone-b   true

# 查看详细条件
kubectl describe vspheredeploymentzone dz-zone-a
# Conditions:
#   Type                       Status
#   ----                       ------
#   VCenterAvailable           True
#   PlacementConstraintReady   True
#   FailureDomainValidated     True
#   Ready                      True

# 查看 VSphereCluster 的故障域状态（需要安装 jq，或去掉 | jq . 直接查看原始 JSON）
kubectl get vspherecluster my-cluster -o jsonpath='{.status.failureDomains}' | jq .
# {
#   "dz-zone-a": { "controlPlane": true },
#   "dz-zone-b": { "controlPlane": true }
# }

# 创建集群后，查看 Machine 的故障域分配
kubectl get machines -o custom-columns=NAME:.metadata.name,FAILURE-DOMAIN:.spec.failureDomain
# NAME                           FAILURE-DOMAIN
# my-cluster-control-plane-xxx   dz-zone-a
# my-cluster-control-plane-yyy   dz-zone-b
# my-cluster-control-plane-zzz   dz-zone-a
```

---

## 六、VSphereMachineConfigPool 与故障域的联动

### 6.1 背景

VSphereMachineConfigPool 为每台 VM 预分配固定的主机名、IP 地址、持久化磁盘等资源（称为 **Slot**）。每个 Slot 绑定到特定的 Datacenter。当某个 Datacenter 下的 Slot 全部耗尽时，即使故障域本身是 Ready 的，也不应该再将该故障域上报给 CAPI 调度器——否则 CAPI 会将 Machine 分配到该域，但 VM 创建时无法获得可用的 IP 或磁盘，导致创建失败。

因此，VSphereCluster 控制器需要**动态过滤**上报的 FailureDomain 列表，将 Slot 已耗尽的 Datacenter 对应的故障域排除在外。

### 6.2 工作流程

```
VSphereMachineConfigPool (per cluster)
  │
  │  每个 Slot 有 Datacenter + State (Available/InUse/Released)
  │
  ▼
VSphereCluster 控制器 reconcileDeploymentZones()
  │
  │  1. 列出所有匹配 failureDomainSelector 的 VSphereDeploymentZone
  │  2. 列出本集群的所有 VSphereMachineConfigPool
  │  3. 统计每个 Datacenter 是否还有可分配的 Slot
  │  4. 对每个 Ready 的 DeploymentZone：
  │     - 查找其关联的 VSphereFailureDomain.spec.topology.datacenter
  │     - 如果该 Datacenter 没有可分配的 Slot → 排除
  │     - 如果有 → 上报到 VSphereCluster.Status.FailureDomains
  │
  ▼
CAPI 调度器只能看到有可用 Slot 的故障域
```

### 6.3 关键逻辑

**Slot 可用性判定**：一个 Slot 在以下状态时被视为可分配：

- `Available` — 空闲
- `Released` — 已释放，等待回收延迟后可复用
- 未初始化（状态为空）

状态为 `InUse` 的 Slot 不计入可用。

**Datacenter 维度聚合**：每个 Slot 通过 `slot.datacenter`（优先）或 `pool.spec.datacenter`（兜底）确定所属 Datacenter。控制器遍历所有 Pool 的所有 Slot，聚合出"哪些 Datacenter 还有余量"。

**安全兜底**：如果所有 Ready 的故障域都因 Slot 耗尽被排除，控制器会保留全部 Ready 的故障域而不是上报空列表。空列表会导致 CAPI 创建 Machine 时不分配 FailureDomain，反而可能抢占其他 Datacenter 的 Slot。

### 6.4 示例

假设集群有两个故障域，分别对应 `dc-bj-1` 的 `cluster-a` 和 `cluster-b`：

```yaml
# VSphereMachineConfigPool
spec:
  clusterRef:
    name: my-cluster
    namespace: default
  datacenter: dc-bj-1
  configs:
    - hostname: node-1    # Slot 1
    - hostname: node-2    # Slot 2
    - hostname: node-3    # Slot 3
```

当 3 个 Slot 全部变为 `InUse` 后，`dc-bj-1` 没有可分配 Slot。此时即使 `dz-zone-a` 和 `dz-zone-b` 都是 Ready 状态，也会被排除出 `VSphereCluster.Status.FailureDomains`（触发安全兜底，保留全部域并设置告警条件）。

如果某个 Slot 被释放（对应 Machine 被删除），该 Datacenter 重新出现可分配 Slot，对应的故障域会被重新上报。

### 6.5 没有 VSphereMachineConfigPool 时的行为

如果集群没有关联任何 VSphereMachineConfigPool，Slot 过滤逻辑完全跳过，行为与标准 CAPV 一致——所有 Ready 的 DeploymentZone 都会上报。
