# 设计：vSphere VM inventory metadata reconcile 优化

## 1. 目标

在不改变 inventory metadata 对外语义的前提下，减少 vCenter Custom Fields API 调用：字段定义只做一次快照查询，VM 值只做一次读取，并且只写入发生变化的字段。

## 2. 字段定义索引

`reconcileInventoryMetadata` 首先保留 nil map 闸门，然后调用一次 `CustomFieldsManager.Field`。对返回定义执行统一的 `isVMInventoryField` 判断：名称必须带 provider prefix，managed object type 必须为空或 `VirtualMachine`。符合条件的定义写入 `name -> key` 索引。

对每个 desired name：

- 索引已存在：复用 key，不再查询字段列表。
- 索引不存在：调用 `Add(name, VirtualMachine)`；成功后立即把返回 key 写入索引。
- `Add` 失败：再次 `Field` 仅用于处理并发 reconcile 已创建同名定义的异常路径。若发现兼容定义则复用，否则返回原始 Add 错误。

这样正常路径的字段定义读取从 `1 + N` 次降为 1 次。字段定义是 vCenter 全局对象，本次快照不是事务锁；保留 duplicate-add 恢复以覆盖实际竞态。

## 3. 变化感知写入

字段定义解析完成后，通过 `vmCtx.Obj.Properties(..., []string{"customValue"}, &mo.VirtualMachine{})` 读取 VM 当前值。防御性地，如果 context 没有 Obj，则用 session client 和 VM reference 构造 `object.VirtualMachine` 再读取。

把 `CustomFieldStringValue` 转为 `key -> current value`。对索引中的每个 provider-owned field：

| 当前值 | desired map | 操作 |
| --- | --- | --- |
| 等于 desired | 任意 | 不写 |
| 不等于 desired | 有 key | `Set` desired value |
| 非空 | 无 key | `Set` 空字符串，清理 stale value |
| 空/缺失 | 无 key | 不写 |

desired map 的 map lookup 沿用 Go 的零值语义，因此缺失 key 等价于空字符串。定义不会删除，避免 vCenter 全局 schema 抖动；只清理对应 VM 的值。

## 4. 控制流与错误

- `InventoryMetadata == nil` 直接返回，不获取 manager，也不读写 VM。
- Custom Fields manager、字段定义读取、VM 属性读取、Add 或 Set 失败均返回带上下文的错误；权限要求和 fail-fast 语义不变。
- 日志继续报告本轮考虑的 provider-owned 字段数量。
- provider-owned field 的筛选逻辑集中在 `isVMInventoryField`，避免初始索引与并发恢复路径出现不一致。

## 5. API 调用对比

| 场景 | 优化前 | 优化后（正常路径） |
| --- | --- | --- |
| 字段定义 | 1 次初始 Field + 每个 desired key 1 次 Field | 1 次 Field |
| 缺失定义 | 每个 key 额外 Add，且可能重复查询 | 每个缺失 key 1 次 Add |
| VM 值 | 不读取，所有字段直接 Set | 1 次 `customValue` 属性读取 |
| 写入 | 所有 provider-owned field 每轮 Set | 仅 current != desired 的字段 Set |

优化后的单次属性读取换来了大量重复写入的消除；在值已收敛的常规 reconcile 中，写请求为零。

## 6. 边界与非目标

- 不改变 annotation allowlist、Cluster 覆盖 VSphereCluster 的优先级或空 map 清理语义。
- 不删除 vCenter 全局 Custom Field 定义。
- 不把 Custom Fields 权限错误转换为 warning 或忽略；该权限是功能前置条件。
- 不在本设计中增加 Kubernetes annotation 到 vSphere 字段名/值限制的转换策略。
- 并发定义变更只能通过 Add 失败后的单次重读恢复，不能提供跨 vCenter 的事务一致性。
