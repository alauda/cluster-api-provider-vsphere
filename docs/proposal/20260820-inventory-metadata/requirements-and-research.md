# vSphere VM inventory metadata：需求与调研

## 1. 背景

CAPV 将 `VSphereCluster` 与 CAPI `Cluster` 上的 provider-owned annotation 复制到 vSphere VM 的 Custom Fields，使 vCenter 运维工具可以按 key/value 查询 VM。当前实现的功能语义正确，但每轮 VM reconcile 会产生不必要的 Custom Fields API 请求：字段定义对每个 desired key 重复查询，并且每个 provider-owned 字段都会无条件写入，即使 VM 当前值已经一致。

本文记录优化需求。Custom Fields 权限是该功能的前置条件；缺少权限或 vCenter 写入失败时返回错误属于预期的 fail-fast 行为，不在本次改动中降级。

## 2. 现有契约

- 只处理名称以 `vsphere.cluster.x-k8s.io/` 开头的 annotation。
- metadata 来源为 VSphereCluster 与 CAPI Cluster；Cluster 的同名值覆盖 VSphereCluster 值。
- metadata map 为 `nil` 时表示没有 owning Cluster（例如删除 fallback），跳过 vSphere metadata 操作。
- 非 nil 的空 map 有明确含义：清理该 VM 上已不存在的 provider-owned Custom Field 值。
- 删除 annotation 时保留全局 Custom Field 定义，只将该 VM 的值清为空字符串。
- vCenter Custom Fields 是全局定义，字段按名称和 managed object type 查找；支持 global 或 VirtualMachine 定义。

## 3. 调研结论

`CustomFieldsManager.Field` 读取全局字段定义；`Add` 创建定义；`Set` 更新某个 VM 的字段值。VM 的 `customValue` 属性可以一次读取全部当前值，并按字段 key 匹配。

当前代码路径：

1. reconcile 先执行一次 `Field`。
2. 每个 desired annotation 进入 `ensureCustomField`，再次执行 `Field`。
3. 对全部 provider-owned 定义执行 `Set`，包括值没有变化的字段和已经为空的 stale 字段。

这会造成：

- desired key 数量为 N 时，正常路径至少 N+1 次字段定义读取。
- 每次 VM reconcile 都产生 O(provider-owned fields) 次远程写入。
- vCenter 负载、reconcile 延迟与瞬时写失败暴露面随 VM 和字段数量放大。

## 4. 需求

1. 正常 reconcile 每轮只读取一次字段定义；只有并发 `Add` 竞争失败后的恢复路径允许额外读取一次。
2. 新增字段后立即加入本轮索引，不能再次发现或创建同名定义。
3. 每轮只读取一次 VM `customValue`，只对 current value 与 desired value 不同的字段调用 `Set`。
4. stale 字段仍必须清理：desired map 中不存在且当前值非空时写入空字符串。
5. 已经为空的 stale 字段不应重复写入。
6. 保持 nil 与 non-nil empty map 的原有语义，以及定义保留策略。
7. 保持 concurrent custom-field creation 的幂等恢复行为。
8. 只有测试覆盖与行为文档变化；不改变 Custom Fields 权限要求或 annotation 来源契约。

## 5. 范围与兼容性

没有 allowlisted annotation 时，metadata map 仍可为空，现有 VM 的 provider-owned values 会按原契约清理；nil fallback 仍跳过操作。无关 annotation 不会创建 vSphere 字段。现有 vCenter 权限、字段名和值限制由部署与使用方保证；本需求不新增跨系统输入校验。
