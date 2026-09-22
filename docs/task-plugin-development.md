# 任务插件开发设计

文档状态：提案

适用基线：`custom/newapi-video`，提交 `28e880e37`

配套文档：[任务插件架构设计](task-plugin-architecture.md)

## 1. 目的和开发约束

这份文档把架构提案拆成可实施的模块、接口、数据迁移、测试和发布步骤。它不是对现有 API v1 的立即破坏性改造。第一阶段应保持 Doubao Video、Hailuo Video、Suno、Kling、Vidu、Alibaba、Vertex 等已有插件继续工作，再通过 capability 逐步启用增强能力。

必须遵守以下约束：

- `docs/plugin-api/v1.md`、`v1.schema.json` 和 `v1.d.ts` 继续作为当前 v1 契约的基线。
- JavaScript 插件仍然只做同步转换，HTTP、认证、数据库、计费和产物存储由宿主执行。
- 插件不能返回 host quota、钱包余额、退款金额或数据库更新指令，只能返回受限的 usage facts 和 normalized result。
- 任务终态、结算、日志投递和产物保存分别保证幂等，任何一个失败都不能触发另一个阶段的重复执行。
- 根模块和 `relaykit` 的独立构建、SQLite/MySQL/PostgreSQL 兼容、现有安全限制和中文代码注释要求不变。

## 2. 当前代码到目标模块的映射

| 现有模块 | 第一阶段职责 | 目标演进 |
| --- | --- | --- |
| `pkg/jsplugin/engine.go` | Sobek 编译、hook 超时、并发池 | 增加资源预算、hook 级指标和隔离策略 |
| `pkg/jsplugin/registry.go` | factory/override、元数据校验 | 增加 key/version/hash resolver 和 head 发布 |
| `pkg/jsplugin/routing.go` | native route、host protocol、generation | 保持请求 generation pin，增加 capability 绑定 |
| `relay/channel/task/jsplugin/adaptor.go` | JS 到 Go descriptor、查询、usage、artifact | 提取 versioned Driver bridge 和统一结果校验 |
| `relay/relay_adaptor.go` | 按 platform 获取当前 adaptor | 新增按 `PluginRef`/execution 获取 adaptor 的入口 |
| `service/task_polling.go` | 15 秒系统任务、按渠道查询、CAS 终态 | 调度 due execution、lease、backoff 和统一 reducer |
| `model/task.go` | 任务公共字段和历史 JSON | 保持兼容，新增执行表作为高频查询来源 |
| `model/task_plugin.go` | 插件源码版本和 active 字段 | 增加 `task_plugin_heads` 并将 active 指针化 |
| `model/task_video_settlement.go` | H3/Doubao 原子结算 | 抽象为所有 task plugin 共用的 settlement receipt |
| `service/task_artifact_store.go` | disabled store 接口 | 增加对象存储、本地存储和异步复制实现 |
| `controller/task_plugin*.go` | 管理 API、native action | 增加统一 operation API、dry-run 和 rollout 状态 |
| `controller/video_proxy.go` | 上游媒体代理和 SSRF 防护 | 优先服务已存储 artifact，明确回退状态 |
| `plugins/tasks/*.js` | 厂商请求、解析、能力矩阵 | 按 model profile 和标准库拆分重复规则 |

## 3. 第一阶段交付边界

第一阶段只解决会影响正确性和后续扩展的基础问题：

1. 插件版本可解析、可保留、可按任务执行引用恢复。
2. 激活版本在多管理员和多节点环境下只有一个权威指针。
3. 任务执行拥有独立的可查询记录，至少包括下次轮询时间、租约、上游 ID、插件 ref 和结算状态。
4. 结算拥有独立 receipt，完成、退款和重试不会重复应用。
5. 提交响应区分 rejection、acceptance 和 uncertainty，并把不确定结果留在可恢复状态。
6. 现有插件不声明新 capability 时保持原行为。

轮询重构、artifact store、webhook 和通用管理操作依次建立在第一阶段之上，不在第一阶段用临时字段互相耦合。

## 4. 规范化接口设计

### 4.1 插件引用

在 `pkg/jsplugin` 增加宿主侧不可变身份：

```go
type PluginRef struct {
    Key        string
    APIVersion int
    Version    string
    SourceHash string
}
```

要求：

- `Key`、`APIVersion`、`Version`、`SourceHash` 必须全部匹配 catalog 中的同一版本。
- source hash 只允许由宿主从保存源计算并比对，不能由请求体覆盖。
- `PluginRef` 进入任务 execution 和 settlement snapshot 后不可修改。
- `GetTaskAdaptor(platform)` 保留作为 legacy 入口；新代码使用 `GetTaskAdaptorForPlugin(ref)` 或 `GetTaskAdaptorForExecution(execution)`。

Registry 应从“按 key 的当前插件”扩展为：

```text
factory[key][version][hash] -> LoadedPlugin
override[key][version][hash] -> LoadedPlugin
head[key] -> PluginRef
generation -> routing entries for new requests
```

历史版本可以不参与新请求路由，但必须在有 in-flight execution 引用时保持可执行。删除历史插件前，宿主要检查 execution 引用和 artifact projection 引用。

### 4.2 SubmitDescriptor

在保持现有 descriptor 字段的基础上增加宿主理解的提交语义：

```json
{
  "url": "https://provider.example/jobs",
  "method": "POST",
  "headers": {},
  "body": {},
  "responseType": "json",
  "retry": {
    "class": "reject_only | idempotent | unknown_on_transport"
  },
  "idempotency": {
    "supported": true,
    "header": "Idempotency-Key"
  }
}
```

规则：

- `idempotency.header` 只能选择受宿主允许的 header 名；值由宿主生成，不允许插件用任意用户输入覆盖。
- `retry.class` 不是插件自称安全就自动放行；宿主根据 HTTP 状态、是否已经发送请求和响应是否开始返回做最终判断。
- `unknown_on_transport` 表示连接失败、读超时、响应截断或无效 task id 时不能直接换渠道重试。
- descriptor 仍由宿主验证 URL、方法、header、body 类型、响应大小和模型身份。

### 4.3 NormalizedTaskResult

把现有 `TaskInfo`、plugin state 和 usage facts 收敛为一个结果：

```json
{
  "status": "QUEUED",
  "progress": "20%",
  "data": {},
  "state": {},
  "usage_facts": {
    "seconds": 5,
    "resolution": "2K"
  },
  "artifacts": [
    {"key": "video", "type": "video", "mimeType": "video/mp4", "source": {}}
  ],
  "error": {
    "code": "",
    "message": "",
    "retryable": false,
    "uncertain": false
  },
  "poll": {
    "next_after_seconds": 10,
    "retry_after_seconds": 0
  }
}
```

第一阶段可以继续使用现有 flat hooks，由 Go bridge 完成兼容转换。新 hook 不能直接返回未声明字段；未知字段、非法状态和超出大小的 state 在宿主边界失败。

### 4.4 ManagementOperation

将当前只允许 list/delete 的 native action 扩展为声明式 operation：

```json
{
  "name": "cancel",
  "methods": ["POST"],
  "input": {"type": "object"},
  "allowed_states": ["QUEUED", "RUNNING"],
  "request": {
    "url": "...",
    "method": "POST",
    "body": {}
  }
}
```

插件只提供 request descriptor 和响应解析结果。宿主负责：

- 当前用户和任务归属。
- plugin ref、channel 和 model 匹配。
- 状态前置条件。
- 上游请求的认证、重定向和大小限制。
- operation receipt、重试和最终本地状态。

`delete` 与 `cancel` 必须是两个不同操作。`delete` 表示上游记录删除，未必代表生成被取消；`cancel` 表示上游确认停止，通常才能进入退款状态。

## 5. 数据模型和迁移设计

### 5.1 task_plugin_heads

逻辑字段：

```text
plugin_key       primary key
version          not null
source_hash      not null
updated_at       not null
updated_by       nullable
revision         not null
```

激活流程：

1. 使用 GORM 事务锁定 `plugin_key` 对应 head。
2. 验证目标版本存在、hash 匹配、编译和 routing preflight 已通过。
3. 更新 head 的 version/hash/revision。
4. 更新兼容的 `task_plugins.active` 镜像字段，失败则整个事务回滚。
5. 节点同步 head，编译目标版本后以一个 generation 发布。

不使用依赖数据库方言的 partial unique index。每个 key 只有一行 head，从模型层保证单一生效版本。

### 5.2 task_plugin_executions

建议字段：

```text
id                    primary key
task_id               unique
plugin_key            not null
plugin_api_version    not null
plugin_version        not null
plugin_source_hash    not null
generation            not null
protocol              not null
operation             not null
channel_id            not null
upstream_task_id      nullable
idempotency_key       nullable
state                 not null
status                not null
state_version         not null
next_poll_at          nullable
lease_owner           nullable
lease_until           nullable
poll_attempts         not null
poll_failures         not null
last_http_status      nullable
last_error_code       nullable
last_error_message    nullable
settlement_status     not null
artifact_status       not null
created_at            not null
updated_at            not null
finished_at           nullable
```

建议索引：

```text
unique(task_id)
(state, next_poll_at, id)
(channel_id, state, next_poll_at, id)
(plugin_key, plugin_version, state)
(idempotency_key)
```

`Task.PrivateData` 继续保留旧数据和必要的私有兼容字段。新流程以 execution 表作为调度和对账索引，避免在任务大表上按 JSON 字段反复筛选。

### 5.3 task_plugin_settlements

建议以 `(execution_id, phase)` 建立唯一业务键：

```text
id
execution_id
phase                 submit_reserve | complete_settle | cancel_refund | reconcile
receipt_key           unique
reserved_quota
actual_quota
delta
status                pending | applied | failed
last_error_code
attempts
created_at
applied_at
```

结算操作必须在主库事务中完成：锁定 execution、验证 state version、锁定资金来源、应用用户/Token/渠道用量、写 settlement row 和本地任务状态。外部日志库投递仍使用独立 outbox，不可作为资金事务的成功条件。

### 5.4 task_plugin_artifacts

建议字段：

```text
id
execution_id
artifact_key
artifact_type
mime_type
source_kind       upstream_url | upstream_file | data_uri | media_ref
source_ref        encrypted or opaque provider reference
storage_backend   upstream | local | s3 | oss
storage_key       nullable
content_hash      nullable
size_bytes        nullable
status            discovered | copying | stored | expired | failed
attempts
last_error_code
created_at
updated_at
```

唯一键使用 `(execution_id, artifact_key)`。公开 URL 只引用 artifact key，不直接把 provider URL 当作长期主键。

### 5.5 task_plugin_operations 和 events

管理操作使用 `(execution_id, operation, client_request_key)` 幂等；webhook 使用 `(plugin_key, provider_event_id)` 或插件声明的组合键去重。

事件 payload 过大时只在对象存储保留 blob ref，数据库保存摘要、hash、序号和处理状态。原始 payload 必须按敏感数据策略加密或脱敏。

### 5.6 迁移顺序

1. 创建新表和索引，不改变现有 Task 列含义。
2. 新提交写入 task row 和 execution row；execution 写入失败时不得发送上游请求。
3. 读取旧任务时按 `TaskExecutionSnapshot` 懒补 execution；没有 snapshot 的标记为 legacy。
4. 轮询先处理有 execution 的任务，legacy 继续使用旧入口。
5. 结算和 artifact 迁移完成并通过观测后，再关闭新任务对旧 fallback 的依赖。
6. 历史 `task_plugins.active` 仅作为兼容镜像，稳定运行后停止直接作为路由事实。

每个数据库迁移必须覆盖：新库、最新生产库升级、启动两次、唯一键、索引和回滚验证。需要同时验证独立日志库存在时的 settlement outbox 行为。

## 6. 工作流实现

### 6.1 安装、编译和激活

```text
upload source
  -> size/hash/schema/security validation
  -> compile with bounded runtime
  -> dry-run required hooks
  -> routing preflight against candidate generation
  -> save immutable task_plugins row
  -> optionally update task_plugin_heads in a locked transaction
  -> publish generation once
```

编译成功不等于可以激活。必须执行以下 fixture：

- 每个 required hook 的成功结果和错误结果。
- native、openai_video、openai_responses 的 request context。
- model mapping、origin task、multipart file reference。
- usage profile、显式零值、缺失值和非法值。
- artifact projection 和管理操作 descriptor。

### 6.2 提交流程

```text
1. route/protocol selects candidate and pins request generation
2. resolve plugin head and build immutable PluginRef
3. decode and validate request
4. calculate bounded estimate and reserve billing
5. create task row + execution row in one primary-db transaction
6. send upstream request with host-generated submission/idempotency key
7. classify response:
   - rejected: mark execution rejected and refund reserve
   - accepted: persist upstream task id and schedule next poll
   - unknown: mark SUBMIT_UNKNOWN and schedule recovery only
8. return public task id
```

不能在持有数据库行锁时等待上游 HTTP。预扣和 execution intent 先提交，外部请求完成后使用 state version 更新结果。

### 6.3 不确定提交恢复

恢复顺序：

1. 使用同一 channel、同一 upstream account 和幂等键执行 provider 的 idempotent lookup。
2. 如果查到 task id，写入 execution 并进入正常轮询。
3. 如果 provider 明确返回“没有创建”，按拒绝处理并退款。
4. 如果 provider 不支持查找或再次响应不确定，保留 `SUBMIT_UNKNOWN` 和预扣，创建 reconciliation item，禁止自动换渠道创建第二个任务。

客户端可以拿到稳定的 `submission_pending` 错误和 request id；该响应不能伪装成成功 task id。

### 6.4 轮询流程

```text
1. scheduler selects due executions by indexed next_poll_at
2. claim each execution with lease_owner/lease_until/state_version
3. resolve exact PluginRef from registry/catalog
4. call buildQueryRequest and execute bounded HTTP
5. call parseTaskResult and validate normalized result
6. update snapshot/state and poll decision outside stale ownership
7. if non-terminal: increment attempt, store next_poll_at
8. if terminal: one transaction applies state + settlement receipt
9. enqueue artifact copy and log outbox independently
10. release lease
```

`next_poll_at` 的最小策略：

- 成功解析的 queued/running：使用插件返回 hint，但限制在宿主上下限内。
- 429：优先使用合法的 `Retry-After`，否则指数退避加抖动。
- transport/5xx/parser failure：增加 failure counter，达到上限才转失败，不进行无间隔重试。
- 任务完成或进入 reconciliation：不再进入普通 poll queue。

### 6.5 Batch 轮询

Batch Driver 必须声明：

```text
max_batch_size
correlation_key
partial_result_allowed
unknown_item_policy
```

宿主把一个批次拆成每个 execution 的结果，逐项进行 state version 校验。批次中一个任务解析失败不能覆盖其他任务；未知 upstream id 只记录诊断，不改变任何本地任务。

### 6.6 取消、删除和重试

取消流程：

```text
QUEUED/RUNNING
  -> refresh once
  -> issue idempotent cancel operation
  -> verify provider result or query
  -> CAS to CANCELLED/FAILED
  -> apply cancel_refund receipt once
```

删除流程必须明确：

- 如果只是删除远端记录而没有终态 usage，不得自动全额退款。
- 进入 `RECONCILIATION_PENDING` 时，任务不应归入普通失败列表。
- 上游确认失败、超时、取消和删除分别保存 reason/code，不用同一个字符串猜测业务含义。

重试分两类：

- query retry：同一 execution、同一上游 task id，可按 poll policy 重试。
- submit retry：只有 provider 幂等确认或明确未发送/未接受时才允许；不能把 submit retry 当作 query retry。

### 6.7 产物读取和保存

```text
terminal success
  -> plugin lists artifact identities
  -> create artifact rows
  -> async copier obtains source with provider auth or safe public URL
  -> validate content type, size, hash and redirect policy
  -> write object store atomically
  -> mark stored
```

读取优先级：

1. `stored` artifact。
2. 仍有效的 upstream source ref。
3. 明确的 `artifact_expired`/`artifact_unavailable` 错误。

复制任务应支持断点、范围请求、超时、并发上限和失败重试。复制过程中不修改任务成功状态，也不重新扣费。

## 7. 计费实现约束

### 7.1 事实与价格分离

插件只返回：

- `seconds`、`count`、`token`、`credit` 等 usage quantity。
- `resolution`、`operation` 等枚举事实。
- 受宿主限制的布尔事实。

宿主负责：

- 按提交时 usage schema 和表达式预估。
- 把表达式和 schema hash 冻结到 execution/billing snapshot。
- 按完成事实覆盖可验证字段。
- 通过 `common.QuotaFrom*Checked` 等集中 helper 做安全转换。
- 记录折前、折后、优惠、预扣、实际和差额的审计字段。

### 7.2 终态不变量

对每个 execution：

```text
reserve applied <= 1
complete_settle applied <= 1
cancel_refund applied <= 1
reconcile applied <= 1 per receipt
```

任何 settlement transaction 失败时：

- 任务保持可重试的内部状态，或者进入 settlement pending。
- 不清除预扣标记。
- 不执行 fallback 全额退款。
- 不创建新的 receipt key。

### 7.3 旧任务和旧表达式

旧任务没有 usage schema hash 时，使用其已有 `BillingContext` 和 legacy 标记；不能用当前插件的新价格字段重新解释历史扣费。升级只改变新任务的默认表达式，in-flight execution 使用冻结快照。

## 8. 插件代码重构规则

### 8.1 Provider Driver 目录

以 Hailuo/Doubao 为例，目标结构可以是：

```text
plugins/tasks/hailuo/
  manifest.js
  models.js
  request.js
  response.js
  billing.js
  artifacts.js
  operations.js
  plugin.js

plugins/tasks/doubao/
  manifest.js
  models.js
  request.js
  response.js
  billing.js
  artifacts.js
  operations.js
  plugin.js
```

如果 v1 必须是单文件，则在构建阶段合并模块，源码层仍按职责拆分。每个 provider 文件不得再次实现一套协议生命周期；协议适配器只负责把统一 request/context 映射成 provider input。

### 8.2 Model profile

每个模型 profile 至少声明：

```text
model
aliases
operations
input media kinds
allowed resolutions
duration range
provider request fields
usage schema/profile
completion usage sources
artifact projection
```

profile 必须被以下路径共享：

- native decode。
- `openai_video` decode。
- `openai_responses` decode。
- submit-time usage。
- completion-time usage。
- native list/filter。
- 价格编辑器 examples。

### 8.3 错误分类

Driver 应返回稳定的内部分类：

```text
invalid_request
unsupported_model
unsupported_field
upstream_auth
upstream_rate_limit
upstream_rejected
upstream_transient
upstream_not_found
response_invalid
usage_invalid
artifact_unavailable
operation_uncertain
```

宿主根据分类决定 HTTP 状态、是否重试、是否改变任务状态和是否保留预扣。错误 message 可以保留供应商可诊断文本，但必须在输出前截断和脱敏。

## 9. 测试和验证设计

### 9.1 Contract fixture

每个插件必须有确定性 fixture，至少覆盖：

| 领域 | 必测项 |
| --- | --- |
| Manifest | 版本、模型、profile、capability、route conflict |
| Submit | JSON、multipart、空值、显式 0/false、模型映射、文件引用 |
| Provider response | 2xx 成功、排队、运行、失败、过期、取消、未知状态、错误 envelope |
| Retry | 429、5xx、超时、断链、截断 JSON、无 task id |
| Billing | 缺失 usage、显式零、超界、估算覆盖、表达式错误、结算重放 |
| Management | cancel/delete、空响应、非 JSON、POST action、重复 operation |
| Artifact | 多产物、Range/HEAD、过期 URL、重定向、SSRF、存储失败 |
| Version | 激活新版本后旧任务继续使用旧 Driver，删除被引用版本被拒绝 |

Fixture 不能通过日志“看起来运行了”来断言成功，必须检查 observable result、任务状态和账务收据。

### 9.2 数据库矩阵

新表、索引、查询和事务必须在真实实例上验证：

- SQLite：新库和升级库，包含并发 CAS。
- MySQL：至少 5.7.8 兼容语法，检查索引和事务行为。
- PostgreSQL：至少 9.6 兼容语法，检查 JSON、锁和 simple protocol。
- 独立 log DB：验证主库 receipt 已提交而日志投递失败时，outbox 可重试且不重复。

每种数据库至少执行两次启动/迁移，验证幂等。不要只用 SQLite 单元测试替代方言矩阵。

### 9.3 故障演练

需要确定性注入以下故障：

1. 上游已接受后关闭连接。
2. 上游返回 2xx 但 body 截断或 task id 缺失。
3. registry 在任务提交后切换版本。
4. 轮询进程在结算提交前崩溃。
5. 两个节点同时结算同一任务。
6. Redis 不可用、主库不可用、日志库慢或拒绝写入。
7. artifact URL 在复制前过期。
8. cancel 与 upstream success 同时发生。
9. 两个管理员同时激活不同版本。

每个演练都检查：上游任务数量、用户余额、Token 额度、渠道使用量、Task 状态、settlement receipt 数量、日志数量和 artifact 状态。

### 9.4 性能基线

每次架构改动前后记录相同测试集：

```text
submit p50/p95/p99
hook p50/p95/p99 by plugin/version/hook
poll throughput per channel
poll lag and due queue depth
primary-db writes per task
Redis commands per task
settlement transaction latency
artifact copy throughput and failure rate
goroutine count and heap after plugin timeout burst
```

验收优先使用相对基线：提交核心路径无明显回归、每个 phase 只应用一次、轮询队列在固定输入下持续下降、Redis/DB 失败时队列有界。不要用不受控的随机大循环或固定 sleep 伪造性能结论。

## 10. 发布步骤和回滚

### 10.1 发布顺序

1. 发布只新增表、resolver 和观测，不改变现有插件路由。
2. 对新任务双写 execution，但旧轮询仍作为 shadow 验证。
3. 开启按版本 resolver，先覆盖 Hailuo/Doubao，再覆盖其他插件。
4. 开启 receipt 结算，比较旧路径和新路径的账务结果。
5. 开启 due queue 和 backoff，保留 legacy poll fallback 开关。
6. 开启 artifact store，先按插件或用户组灰度。
7. 最后开放 webhook、通用 management operation 和 marketplace capability。

### 10.2 回滚条件

出现以下任一情况立即关闭新能力开关并保留数据：

- 同一个 execution 出现两个 applied receipt。
- 账务对账出现非零差异且无法由舍入解释。
- 旧任务被新版本错误解析或无法查询。
- due queue 长时间增长，或上游 429 明显增加。
- artifact 复制造成请求线程阻塞或内存持续增长。

回滚只切换 feature flag 或 plugin head；不能删除 execution、receipt、artifact 和事件数据，也不能强制改写已经结算的 Task。

## 11. 开发任务拆分

### P1 阶段

- `plugin-ref-and-head`：PluginRef、head 表、并发激活、按版本 resolver。
- `submission-uncertainty`：submit intent、幂等键、unknown 状态和恢复查询。
- `execution-and-receipt`：execution/settlement 表、CAS reducer、账务幂等。
- `due-poll-scheduler`：next_poll_at、lease、backoff、批量上限和公平调度。
- `artifact-persistence`：artifact 表、对象存储实现、复制 worker 和过期状态。

### P2 阶段

- `typed-plugin-contract`：normalized result、schema/codegen、fixture CLI。
- `provider-model-profile`：Doubao/Hailuo 能力矩阵和协议入口去重。
- `management-operations`：cancel/delete/retry/refresh 统一契约。
- `webhook-inbox`：签名、去重、乱序、回放和轮询共用 reducer。
- `runtime-hardening`：有界 auth cache、资源指标、熔断和低信任隔离。
- `rollout-observability`：多节点加载状态、版本 drain 和审计报表。

每个任务必须同时提交实现、适用迁移、确定性测试、错误分类和回滚说明。不能只添加接口或 UI 而把真实生命周期留给后续工作。

## 12. 完成标准

一项插件框架改动只有在以下条件同时满足时才算完成：

- 新旧插件都能按声明的 API/capability 加载。
- 新任务和升级中的旧任务分别使用正确的 plugin ref。
- 提交、查询、取消、失败、超时、结算和产物路径都有明确状态。
- 不确定提交不会盲目产生第二个上游任务。
- 结算和退款可在进程崩溃、重复轮询、重复 webhook 后重放而不重复扣费。
- 用户、管理员、root 和匿名 artifact 访问仍按原有边界隔离。
- SQLite、MySQL、PostgreSQL 及独立日志库验证通过。
- 关键指标和告警能定位 plugin key、version、model、channel、task 和 request id。
- 文档、schema、d.ts、fixture 和插件示例保持一致。
