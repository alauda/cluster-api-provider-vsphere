# 测试用例：vSphere VM inventory metadata

本文覆盖 VM inventory metadata 的功能语义与本次 Custom Fields API 优化。代码级用例使用 govmomi simulator；真实 vCenter 用例用于验证权限、版本和并发行为。

## 0. 通用前置

- 管理集群已部署包含 inventory metadata 实现的 CAPV controller、CRD、RBAC 与 chart。
- vCenter 凭据具备 Custom Fields manager 的读取、创建和 VM 值写入权限。
- 测试 VM、Cluster、VSphereCluster 与字段统一使用 `tc-capv-im-*` 前缀。
- 代码级测试可使用 `simulator.VPX()`、`vcsim.NewBuilder()` 和现有 govmomi 测试 helper。

## 1. 用例索引

| TC | 场景 | 类型 |
| --- | --- | --- |
| TC-CAPV-IM-00 | Custom Fields 权限与字段前置 | 环境前置 |
| TC-CAPV-IM-01 | annotation allowlist 与来源优先级 | 单测 |
| TC-CAPV-IM-02 | 已有字段复用与缺失字段创建 | simulator |
| TC-CAPV-IM-03 | 相同值重复 reconcile 无写入 | simulator / API 计数 |
| TC-CAPV-IM-04 | 单字段变化只写一次 | simulator / API 计数 |
| TC-CAPV-IM-05 | annotation 删除清理 stale value | simulator |
| TC-CAPV-IM-06 | nil 与 non-nil empty metadata | 单测 / simulator |
| TC-CAPV-IM-07 | 并发 Add 与错误重试 | simulator / vCenter |
| TC-CAPV-IM-08 | 无 allowlisted annotation 的兼容性 | 回归 |

## 2. 用例正文

### TC-CAPV-IM-00：Custom Fields 权限与字段前置

**目标**：确认部署账号具备功能所需权限，并确认权限错误按预期暴露。

**步骤**：
1. 使用具备 Custom Fields list/add/set 权限的账号创建或更新 VM metadata。
2. 使用缺少其中一项权限的账号重复 reconcile。
3. 恢复权限并等待下一轮 reconcile。

**预期**：
- 完整权限下字段可创建、值可写入。
- 权限不足时 reconcile 返回错误并重试；这属于功能前置条件，不应静默忽略。
- 恢复权限后 metadata 最终收敛。

**清理**：删除测试 VM 与字段值，保留或按环境策略清理全局定义。

### TC-CAPV-IM-01：annotation allowlist 与来源优先级

**目标**：确认只有 provider prefix annotation 被复制，Cluster 同名值覆盖 VSphereCluster。

**步骤**：
1. 在 VSphereCluster 写入 provider key 与无关 key。
2. 在 Cluster 写入同名 provider key、另一个 provider key 和无关 key。
3. 触发 VSphereVM reconcile。

**预期**：
- vSphere 只出现 provider prefix 字段。
- 同名字段值来自 Cluster；不同名字段均被保留。
- 无关 annotation 不创建 Custom Field。

**清理**：删除测试 annotation。

### TC-CAPV-IM-02：已有字段复用与缺失字段创建

**目标**：确认一次字段定义快照可复用既有字段，并为缺失 key 创建定义。

**步骤**：
1. 预先创建一个 global 或 VirtualMachine 类型的 provider field。
2. 为 VM 设置该 key 与另一个尚不存在的 key。
3. reconcile 两次，并读取全局字段定义。

**预期**：
- 已有字段 key 被复用，不创建重复定义。
- 新 key 只创建一个定义。
- 同一轮 desired keys 不触发逐 key 的 Field 查询；请求计数时正常路径每轮仅一次字段定义读取。

**清理**：删除 VM 值。

### TC-CAPV-IM-03：相同值重复 reconcile 无写入

**目标**：确认已收敛值不会重复调用 `SetField`。

**步骤**：
1. 首次 reconcile 两个 metadata key。
2. 在 SOAP round tripper 或等价测试 transport 上清零计数。
3. 不改变任何对象，再次 reconcile。

**预期**：
- 第二轮值保持不变。
- 第二轮 `SetField` 请求数为 0。
- 字段定义仍稳定，不产生重复定义。

**清理**：删除测试 VM。

### TC-CAPV-IM-04：单字段变化只写一次

**目标**：确认变化检测不会影响其他已收敛字段。

**步骤**：
1. 设置两个 metadata key 并完成首次 reconcile。
2. 只修改其中一个值。
3. 记录并检查 `SetField` 请求与 VM customValue。

**预期**：
- 只发生一个变化字段的写入。
- 未变化字段不产生写请求且值保持不变。

**清理**：删除测试 VM。

### TC-CAPV-IM-05：annotation 删除清理 stale value

**目标**：确认删除 annotation 会清除 VM 值，但不会删除全局定义。

**步骤**：
1. 设置两个 provider metadata 并 reconcile。
2. 删除其中一个 annotation，保留另一个。
3. 等待 VM reconcile，读取字段定义与 customValue。

**预期**：
- 删除字段的 VM 值变为空字符串，且只有非空 stale 值触发一次 Set。
- 字段定义仍存在。
- 保留字段的值不变。

**清理**：删除测试 VM。

### TC-CAPV-IM-06：nil 与 non-nil empty metadata

**目标**：保护 context 控制语义。

**步骤**：
1. 使用 `InventoryMetadata == nil` 的 deletion fallback context reconcile。
2. 使用非 nil 空 map 的 owning Cluster context reconcile。
3. 读取 VM customValue。

**预期**：
- nil context 不获取 manager、不读写 VM、不改变已有值。
- 空 map 清理所有 provider-owned 非空值；已经为空的字段不产生冗余写。

**清理**：删除测试 VM。

### TC-CAPV-IM-07：并发 Add 与错误重试

**目标**：确认两个 reconcile 同时创建同名字段时最终可收敛。

**步骤**：
1. 让两个 VM reconcile 使用同一个尚不存在的 provider key。
2. 并发触发 reconcile，或在真实 vCenter 上制造 Add 竞争。
3. 观察 Add 失败后的字段重新读取与后续 VM 值写入。

**预期**：
- 一个创建成功，另一个通过异常恢复读取到兼容定义并继续。
- 非重复/权限错误仍返回并重试，不被错误吞掉。

**清理**：删除 VM 值。

### TC-CAPV-IM-08：无 allowlisted annotation 的兼容性

**目标**：确认没有 provider annotation 时不引入新的用户可见行为。

**步骤**：
1. 创建或升级没有 provider prefix annotation 的集群。
2. 运行多个 VM reconcile 周期。
3. 检查 vCenter 和 VM 状态。

**预期**：
- 不创建新的 provider Custom Field。
- 已有 provider-owned stale values 按既有 non-nil empty 语义清理；删除 fallback 的 nil context 不操作。
- VM 核心调谐与 Ready 行为不受影响。

**清理**：无。

## 3. 代码级覆盖清单

- `TestReconcileInventoryMetadata`：字段创建、值写入、重复 reconcile、stale 清理。
- 字段索引：global/VirtualMachine 复用、缺失字段 Add、无关字段过滤。
- 变化检测：current == desired 跳过、单值变化只 Set 一次、stale 非空清空、stale 空值跳过。
- nil/non-nil empty map 控制流。
- Add 竞争恢复与字段读取/VM 属性读取错误传播（具备 fault injection 时覆盖）。
- controller 层继续覆盖 annotation 过滤与 Cluster 优先级。

推荐命令：

```bash
go test ./pkg/services/govmomi -run TestReconcileInventoryMetadata -count=1
go test ./controllers -run 'TestInventoryMetadata' -count=1
go test ./pkg/services/govmomi ./controllers
go test ./...
```

## 4. 执行顺序

1. 先执行代码级 simulator 与 controller 单测。
2. 在具备 Custom Fields 权限的真实 vCenter 执行 IM-00、IM-02 至 IM-05。
3. 再执行权限错误、并发 Add 和恢复场景 IM-00、IM-07。
4. 最后执行兼容性 IM-08，并确认最终 diff、文档和测试结果已记录。
