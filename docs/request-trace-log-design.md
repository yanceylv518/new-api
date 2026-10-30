# 请求追踪日志设计

## 当前实现：rc35 流式请求时间线（2026-10-30 修订）

基线为 `custom/rc35` 的 `af903ed7b`，开发分支为 `feat/rc35-stream-timeline`。本节是本次实现契约；后文 2026-10-09 的全量追踪设想仅保留作历史参考，其中“不新增表”和完整请求追踪等条目不适用于本版。

### 启用、范围与入口

- 默认关闭；在系统设置的“日志维护”中开启“记录流式请求时间线”，配置键为 `RequestTimelineEnabled`，保存到既有 `options` 表。保存成功后本节点对新请求生效，其他节点沿用现有配置同步周期；无需为切换开关重启。主节点启动时准备独立 `request_timelines` 表，各节点复用一个后台写入器；应先启动主节点。关闭只停止新请求采集，已开始的请求正常收尾，历史记录仍可查询。迁移失败只禁用时间线，不中止已有转发服务。
- 仅对原始客户端请求要求流式的业务 Chat Completions、Responses，以及 Responses 转 Chat 链路采集；不改变现有首字口径、业务响应、计费或重试策略。
- 首版覆盖实际复用 OpenAI 流式处理器的 OpenAI、OpenRouter、Xinference、Codex、NewAPI、Sub2API API 类型。其他原生 SDK、Claude/Gemini 入口、WebSocket、异步任务不纳入本版；支持的渠道以 `BeginRequestTimeline` 的协议/适配器条件为准。
- 普通非流式、手动/自动模型测试不采集。业务重试或 transport 再次发送时放弃本次时间线，避免把多次请求合并为一条；原有重试照常执行。
- 管理员在用量/错误日志详情展开“请求时间线”时才查询。列表不关联、查询新表。`other.admin_info.timeline_candidate=true` 仅表示当时具备采集条件，不保证已入库；不向 `other` 写入时间线内容。
- 详情接口为 `GET /api/request-timelines/:request_id`，同时执行路由 `AdminAuth` 和控制器角色检查。无普通用户查询接口，不增加全局时间线搜索或无日志请求列表。

### 五个时间点

| 节点 | 界面名称 | 实际观察位置 | 含义及限制 |
| --- | --- | --- | --- |
| A | 开始处理 | 现有 `RelayInfo.StartTime` | 沿用 rc35 首字起点；通常已完成第一轮渠道选择，不代表请求刚到最外层入口 |
| B | 请求发送至上游 | `httptrace.WroteRequest` 成功回调 | 完整请求已交给本地网络写出，不证明上游已开始生成；包含连接、发送等前置时间 |
| C | 收到上游首段响应 | 原有 `SetFirstResponseTime` | 首个非空且非 `[DONE]` 的数据段；可能只是角色或元信息，不是响应头时间，也不保证已有正文 |
| D | 首段响应写出完成 | 现有写出/刷新返回后 | 首次业务响应写出成功；保活消息不计入，不证明客户端已实际收到 |
| E | 输出结束 | 最后写出/刷新或流终止观察 | 包含完整输出结束；不使用控制器退出时间替代，避免混入后续计费、日志入库耗时 |

界面显示 B−A（发送前准备）、C−B（等待上游）、D−C（本地首段转发）、C−A（现有首字耗时）、E−A（完整输出耗时），各节点另显示相邻间隔。C−A 与原日志 `frt` 的毫秒计算保持一致。其他间隔使用进程单调时钟，并对齐相同毫秒舍入；时间倒序或时钟异常时隐藏耗时而保留字段。

缺失的节点为 `null`，不补零。A 的绝对毫秒时间用于与其他系统核对，其余存储相对 A 的毫秒偏移。时钟同步和上游统计口径仍需单独核对；C−B 较长只能把范围缩小到上游等待及往返链路，不能单凭本服务日志断言是哪一方责任。

### 表结构与记录内容

表位于 `LOG_DB`，未配置独立日志库时自然使用业务库。SQLite、MySQL、PostgreSQL 使用相同八列结构；不依赖 JSON 数据库类型、外键、JOIN 或消费日志自增 ID。

| 字段 | 类型 | 约束/含义 |
| --- | --- | --- |
| `request_id` | VARCHAR(64) | 唯一主键、非空；MySQL 使用 `utf8mb4_bin`，大小写区分 |
| `user_id` | BIGINT | 非空，请求所属用户 |
| `created_at` | BIGINT | 非空，生成快照时的 Unix 秒；单列清理索引 |
| `upstream_status` | SMALLINT | 可空，真实上游 HTTP 状态，区别于平台最终状态 |
| `result` | VARCHAR(16) | 非空：`success` / `failed` / `interrupted` / `unknown` |
| `error_kind` | VARCHAR(32) | 可空，失败、中断或未确认结束的原因类别 |
| `upstream_request_body_bytes` | BIGINT | 可空，最终上游完整请求体的字节数；0 表示确实为空 |
| `data` | TEXT | 非空，版本化 JSON；单条序列化内容上限 16 KiB |

`data.version=1` 包括 A 的绝对时间、B/C/D/E 偏移、时间有效标记、请求/返回模型、渠道 ID、上游请求 ID、响应 ID、首段类型、返回结束原因、流终止原因、上游结束标记、实际返回的 token 用量、首次异常的偏移/阶段/类别/代码/脱敏消息。

请求体大小取最终 `http.Request.ContentLength`/现有可重放请求体元数据，不读取、复制或重新序列化请求体。未知长度为 `null`，即便发送失败也不把“已发送部分字节数”冒充完整大小。页面将其放在 B 节点，按字节显示。没有实际返回 usage 对象时显示“上游未返回”，不写本地估算用量。

仅提取允许的标识与统计字段，不保存请求、响应正文、prompt、工具参数或整组头部。错误采集的输入预算为 8 KiB：Chat 错误对象在额外复制/解析之前检查大小，已解析的消息在脱敏之前检查大小；超限省略原文并记录长度和省略说明，不格式化任意嵌套错误对象。预算内消息复用现有凭证/媒体脱敏器，最终限 2048 字节；代码限 128 字节，标识/模型通常限 256 字节；截断或省略会标注。原始业务响应不受诊断采集上限影响。错误消息仍可能包含上游以自然语言回显的业务内容，因此仅管理员可查。

### 结束状态和降级行为

- HTTP 200 只表示 HTTP 层返回成功，不表示生成完成。流内业务错误一经观察即记录 `failed/upstream_business_error`，后续正常结束标记不覆盖首次错误。
- 有上游结束标记、成功业务写出且未观察到错误时才标为成功。缺少结束标记的 EOF 为 `unknown/unconfirmed_end`，不猜测正常完成。
- 客户端取消、下游写入失败、超时、上游读取失败分别保留类别。客户端取消优先于其引起的上游关闭错误，避免把断开误算成上游故障。
- 解析/处理异常复用 `StreamStatus` 首条错误的实际时间与消息，使用 `stream_processing_error` 错误代码，与 `done` 等结束原因分开保存。在处理下一段之前记录首次错误，避免相同时间戳下被后来的业务错误抢占；退出时补录更早的错误。异常时间在脱敏之前固定。缺少节点不伪造时间；`response.incomplete` / cancelled 不会被后续 `[DONE]` 标成成功。
- 快照只提交一次，重复请求 ID 的批次写入幂等。时间线和原日志不是同一事务：列表可能先出现，详情最多自动补查三次，然后显示缺失说明并保留手动刷新。
- ClickHouse 日志库不启用时间线；数据库表缺失、版本不支持、记录格式损坏等均显式降级，不影响列表和转发。

### 系统影响、清理与回退

- 热路径仅保存有界字段和时刻，复用原有 DTO 解析，不增加逐段数据库写入；最终快照入内存队列后由一个后台写入器处理。
- 队列 256 条、批次 32 条、最长等候 250 ms；每次数据库写入超时 3 秒，最多尝试两次。入队失败只增加原子计数并返回，不在请求线程打印日志；现有后台写入器约每 10 秒汇总新增丢弃，正常退出时补一次汇总。日志输出阻塞也不会让队列满时的请求等待告警。
- 正常退出在 HTTP 请求收尾后最多等待 15 秒排空；强制退出、写入超时或队列满可能丢失时间线，因此它不是保证不丢的审计账本。
- 新表初始化先于日志清理任务启动。沿用现有日志清理截止时间分批清理新表，即使原日志已清空也执行；保留原日志删除数量，额外返回 `timeline_deleted_count`。队列中迟到记录在下次清理处理，不引入墓碑或新定时任务。
- 关闭系统配置开关或回退旧二进制即可停采，新表保留，旧版本忽略它。迁移允许新增可空、有默认值或自增的额外字段，仍要求必要字段及请求 ID 单列主键，拒绝额外必填且无默认值的字段。详情显式查询既有八列，避免 PostgreSQL 在运行期间加列后发生缓存计划结果类型变化。发现不兼容的实验表结构时报告并禁用功能，不自动删表或改写数据。

### 本次验证

- 四项修复的用例已先验证失败、再验证通过：队列满且日志输出阻塞、早期解析错误后又发生业务错误、大错误帧省略且转发字节/用量不变、兼容新增字段并保持旧写入有效。三数据库额外验证默认值、拒绝新增必填无默认值字段、系统配置持久化；保存失败不会先改变运行时开关。
- 异常流性能命令：`go test ./controller -run '^$' -bench '^BenchmarkRequestTimelineErrorFrame$' -benchmem -benchtime=500ms -count=3`。同类约 1 MiB 错误帧，修复前开启约 677 ms、分配约 33.9 MB；修复后开启 5.227–5.356 ms、约 12.67 MB，关闭 5.265–5.722 ms、约 12.61 MB。修复消除了整段诊断脱敏带来的主要耗时和分配。
- 后端专项与相关模块命令：`go test ./controller -run '^TestRequestTimeline' -count=1 -timeout=90s`，`go test ./relay/channel/openai ./relay/helper ./relay/common ./model ./middleware ./router -count=1 -timeout=90s`，`go test ./service -run 'Test.*(Billing|Quota|Settle|SystemTask|LogCleanup|OtherInfo)' -count=1 -timeout=90s`。本轮均通过；正常与错误流的转发对照、缓存/推理/工具用量和 usage 开关对照均保持一致。

- 功能测试集中在 `controller/request_timeline_test.go`：真实 HTTP 发送回调和完整请求体大小，既有首字值一致性，Chat/Responses/Responses 转 Chat 启用前后输出字节一致，200 后业务错误，未知 EOF、超时、断开、写失败、保活排除、重试排除、队列满、后台写失败、排空、管理员权限和缺失记录。
- 三数据库实测：SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 18.6。覆盖首次建表、原有日志数据保留、重复迁移、大小写主键与重复入库、中文、NULL 与 0、过期清理；另测 MySQL/PostgreSQL 共库与独立日志库路由。生产 MySQL 的具体版本尚未提供，本次未连接生产。
- 后端命令：`go test ./controller -run '^TestRequestTimeline' -count=1 -timeout=90s`；数据库矩阵通过 `TIMELINE_TEST_MYSQL_DSN` 或 `TIMELINE_TEST_POSTGRES_DSN` 指向专用空库执行 `TestRequestTimelineDatabaseRoundtrip`，`TIMELINE_TEST_SHARED_DB=true` 验证共库。
- 正常流性能命令：`go test ./controller -run '^$' -bench '^BenchmarkRequestTimelineStream$' -benchmem -benchtime=2s -count=3`，使用专用 MySQL DSN；真实本地 HTTP 流式转发，每请求 100 段内容并异步入库。本次开关对照：关闭 1.981–2.069 ms/请求，开启 2.410–2.463 ms/请求，开启约多 0.4 ms、11–12 KiB 分配，无丢记录。另用修复前源码覆盖映射作同环境对照：修复前开启 2.012–2.739 ms，修复后开启 2.167–2.484 ms，范围重叠；没有证据表明这四项修复引入额外时间退化，采集本身仍有开销。这是本机样本，不代表生产尾延迟。
- 前端命令：`bun run typecheck`、受影响文件 `oxlint`、`bun run test src/features/system-settings src/features/usage-logs`、`bun run build`。共 19 个测试文件、138 项测试通过，新增设置开关的默认关闭、键盘启用、关闭及保存失败重试验证。时间线页面既有按需加载、权限、空值、错误和有限重试仍通过；日志维护开关另做真实组件页面检查。
- 后端受影响模块全量回归发现基线已有失败：Windows 的 SQLite 测试文件未释放导致清理失败，以及渠道亲和性缓存断言；用 `af903ed7b` 源码覆盖映射复现了代表性失败。时间线专项及相关转发、模型、middleware、router 测试通过；另通过临时源码覆盖映射运行实际日志清理任务，验证原日志为空时仍删除旧时间线且保留较新记录；全量 controller 套件因已有文件清理失败累计超时，不声称全量测试通过。当前 Windows Go 环境 `CGO_ENABLED=0` 且无 gcc，未执行 race detector。
- 详情授权复用既有认证体系并执行服务端管理员限制；参考 OWASP [Authorization](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html)、[Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)、[Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)。未改动认证机制，不声称完成全站 ASVS 审计。

## 历史方案（2026-10-09，以下不是本版交付范围）


文档日期：2026-10-09。状态：设计稿，尚未实现。基线为 `custom/rc35-user-model-discount` 的 `3cf493fbe`。

配套的可交互原型是同目录下的 `request-trace-log-prototype.html`，直接用浏览器打开即可，不依赖外网。原型里的报文和数值都是虚构示例。

本文中标注"待确认"的条目是按建议值写的默认方案，实施前需要确认。

## 1. 背景与目标

现有用量日志能回答"谁、用哪个模型、走哪个渠道、花了多少、报了什么错"，但几乎不记录请求和响应本身的形状，并且相当一部分失败请求不进 `logs` 表。排查问题时经常只有一句错误消息。

目标：

1. 每条请求日志附带足够定位问题的元数据：大小、状态、毫秒级耗时、结束原因、请求形状统计、上游返回的关键标识。
2. 未到达渠道的失败也能在日志页按请求 ID 查到。
3. 不保存请求体和响应体正文。
4. 不改 `logs` 表结构，不新增表。
5. 对热路径的开销可忽略，存储增量可通过开关控制。

## 2. 非目标

- 不保存请求体、响应体正文或其片段。唯一的原文是失败时上游返回的错误响应体（见 4.4）。
- 不保存上游响应头（限流头、`retry-after`、`cf-ray` 等）。
- 不给用量日志列表加列，不加筛选项，不做基于这些字段的统计报表。
- 不覆盖异步任务（视频生成、Midjourney 等）的提交与轮询日志。这条路径本次未分析。
- 不覆盖 Realtime WebSocket 的响应字节统计。
- 不引入独立追踪表、对象存储或采样系统。

## 3. 现状

### 3.1 已经记录的内容

| 位置 | 内容 |
| --- | --- |
| `logs` 表的列 | `request_id`、`upstream_request_id`、`use_time`（整数秒）、`is_stream`、`ip`（仅用户开启记录 IP 时）、`content` |
| 成功日志的 `other` | 首字耗时 `frt`、`request_path`、格式转换链、`stream_status`、上游模型名；`admin_info` 下有渠道重试链 `use_channel`、转换诊断、多 Key 序号 |
| 错误日志的 `other` | `error_type`、`error_code`、`status_code`、`request_path`，以及 `admin_info`（`controller/relay.go:417`） |

`other` 是一个 JSON 文本列，已按受众分成四层（`model/log_other.go`）：顶层用户可见，`admin_info` 仅管理员，`root_info` 仅 Root，`audit_info` 用于审计。查询时按身份裁剪（`model/log.go:116` 起的三个格式化函数）。新字段直接复用这套分层。

### 3.2 缺口

| 缺口 | 说明 |
| --- | --- |
| 请求侧 | 请求体大小、Content-Type、压缩方式、User-Agent、消息数、工具数、多媒体个数、`max_tokens` 等都没有记录 |
| 响应侧 | 成功时的上游状态码与协议、写回客户端的字节数、流分片数、`finish_reason` 都没有记录 |
| 上游错误原文 | `service/error.go:87` 解析出错误消息后丢弃原始响应体，只在本地文件日志留一段预览 |
| 时间精度 | 总耗时只有整数秒；不足 1 秒的错误记录在详情里不显示耗时 |
| 上游请求 ID | 只识别 `X-Oneapi-Request-Id` 和 `X-Request-Id`（`relay/channel/api_request.go:552`）。Anthropic 用的是 `request-id`，走通用路径时记不到 |
| 错误覆盖 | 见 3.3 |

### 3.3 未到达渠道的失败不落库

`model.RecordErrorLog` 只有一个调用点（`controller/relay.go:431`），位于 `processChannelError` 内，只在已选到渠道且上游返回错误后才会走到。并且还需要环境变量 `ERROR_LOG_ENABLED=true`，代码默认关闭（`common/init.go:199`）。

| 失败类型 | 发生位置 | 进 `logs` 表 | 进本地文件日志 |
| --- | --- | --- | --- |
| 401 令牌无效、403 令牌被禁或无权限 | `middleware/auth.go` | 否 | 是 |
| 429 用户请求数限流 | `middleware/model-rate-limit.go:95` | 否 | 是 |
| 429 全局或 IP 限流 | `middleware/rate-limit.go:143` | 否 | 只有访问日志一行 |
| 400 模型名缺失、请求无法解析 | `middleware/distributor.go:45` | 否 | 是 |
| 503 无可用渠道、模型不存在 | `middleware/distributor.go:178` | 否 | 是 |
| 400 请求校验失败、413 请求体过大 | `controller/relay.go:115` | 否 | 是 |
| 敏感词、计价错误 | `controller/relay.go:142` | 否 | 是 |
| 403 余额或订阅额度不足 | `service/billing_session.go:202` | 否，且被显式标记为不记录 | 是 |
| 重试时选不到下一个渠道 | `controller/relay.go:200` | 否 | 是 |

中间件的拦截统一走 `abortWithOpenAiMessage`（`middleware/utils.go:14`），它只返回错误并写一行本地日志。结果是：用户拿着错误消息里的请求 ID 来报障，管理员在日志页搜不到任何记录。

## 4. 数据设计

所有新增字段都写在 `logs.other` 里，分三处：`other.trace`（用户可见）、`other.admin_info.trace`（管理员）、`other.root_info`（Root）。字段名为提案。

通用规则：

- 取不到或为零的可选项不写入，前端也不显示对应的行。例外是消息条数，为 0 时也保存。
- 整个子对象为空时省略该键。
- 旧日志没有这些字段，前端按"有才显示"渲染，不需要回填。

### 4.1 用户可见：`other.trace`

| 字段 | 类型 | 含义 | 何时写入 |
| --- | --- | --- | --- |
| `ms` | int | 从请求开始到本条日志写入的毫秒数 | 始终 |
| `req_bytes` | int | 解压后的请求体字节数 | 已读取请求体时 |
| `resp_bytes` | int | 写回客户端的字节数 | 成功日志 |
| `finish_reason` | string | 上游给出的结束原因原值；流被中断未收到时为空串 | 成功日志 |
| `stage` | string | 失败阶段：`auth`、`rate_limit`、`validate`、`price`、`pre_consume`、`select_channel`、`upstream` | 错误日志 |

`finish_reason` 统一保存各协议的原值：OpenAI 的 `finish_reason`、Claude 的 `stop_reason`、Gemini 的 `finishReason`，不做跨协议归一。

### 4.2 管理员可见：`other.admin_info.trace`

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `attempt` | int | 第几次尝试，从 0 开始，取自 `RelayInfo.RetryIndex` |
| `client.ua` | string | User-Agent，截断到 256 字符 |
| `client.content_type` | string | 请求的 Content-Type |
| `client.content_encoding` | string | 请求的压缩方式，未压缩时省略 |
| `client.wire_bytes` | int | 请求头 Content-Length，即线上传输的字节数 |
| `client.sdk_retry` | int | `X-Stainless-Retry-Count`，官方 SDK 的客户端重试计数 |
| `client.headers` | object | 请求头白名单：`anthropic-version`、`anthropic-beta`、`openai-beta` |
| `shape.messages` | int | 消息条数 |
| `shape.text_chars` | int | 全部文本内容的字符数之和 |
| `shape.images` / `videos` / `audios` / `files` | int | 各类多媒体输入的个数 |
| `shape.tools` | int | 工具定义个数 |
| `shape.tool_choice` | string | 工具选择模式，只存类型 |
| `shape.max_tokens` | int | 各协议的最大输出 token 参数 |
| `shape.temperature` | number | 采样温度 |
| `shape.response_format` | string | 响应格式，只存类型 |
| `shape.include_usage` | bool | 流式请求是否要求返回用量分片 |
| `shape.system` | bool | 是否带系统提示 |
| `shape.keys` | string[] | 请求体顶层字段名列表，仅在请求校验失败时写入 |
| `upstream.status` | int | 上游 HTTP 状态码 |
| `upstream.proto` | string | 上游协议版本，如 `HTTP/2.0` |
| `upstream.req_bytes` | int | 转换后发给上游的请求体字节数 |
| `upstream.header_ms` | int | 从发出请求到收到上游响应头的毫秒数 |
| `upstream.resp_id` | string | 上游响应体里的 ID |
| `upstream.resp_model` | string | 上游实际使用的模型名 |
| `upstream.fingerprint` | string | `system_fingerprint` |
| `upstream.error_type` | string | 上游错误体里的 `type` 原值 |
| `resp.choices` | int | 返回的选项数 |
| `resp.content_chars` | int | 回复文本字符数 |
| `resp.tool_calls` | int | 工具调用个数 |
| `resp.reasoning_tokens` | int | 推理 token 数 |
| `resp.chunks` | int | 流分片数，取自 `RelayInfo.ReceivedResponseCount` |
| `resp.last_chunk_ms` | int | 最后一个分片到达的毫秒数 |
| `resp.done` | bool | 流是否收到协议规定的结束标记 |
| `resp.usage_chunk` | bool | 流是否收到用量分片 |

`shape.keys` 用来替代"保存请求体片段"。它不含任何值，能直接看出必填字段拼错、把一种协议的请求体发到另一种协议的接口这两类问题。JSON 本身损坏时解析不出字段名，这种情况只能依靠错误消息。

### 4.3 多媒体计数的来源

请求解析时已经把多媒体输入分成图片、音频、视频、文件四类（`relaykit/types/request_meta.go:6`）。各协议的识别位置：

| 请求格式 | 视频的识别方式 | 位置 |
| --- | --- | --- |
| OpenAI 兼容 | 内容分片类型为 `video_url` | `relaykit/dto/openai_request.go:227` |
| Gemini 原生 | 内联或引用数据的 MIME 类型为视频 | `relaykit/dto/gemini.go:90` |
| Claude 原生 | 协议不支持视频输入 | — |

视频通常以地址传入，请求体很小，日志里只能知道"带了几个视频"，拿不到视频的大小、时长和分辨率。

### 4.4 Root 可见：`other.root_info`

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `upstream_error_body` | string | 上游错误响应体原文，截断到 4096 字节，写入前经 `common.MaskCredentials` 脱敏 |
| `upstream_error_body_truncated` | bool | 是否被截断 |

只在失败记录上保存，受单独的开关控制。它的内容是供应商的报错文本，不含用户的提示词。前端沿用现有的"Root 诊断"分区展示。

### 4.5 明确不保存的内容

- 请求体与响应体正文，包括截断片段。
- 令牌和渠道密钥。
- 终端用户标识（OpenAI 的 `user`、Claude 的 `metadata.user_id`）。
- 工具定义、工具调用参数。
- 多媒体的地址和数据。
- 上游响应头。

### 4.6 修正：识别 Anthropic 的上游请求 ID

`relay/channel/api_request.go:552` 和 `service/http.go:28` 在 `X-Oneapi-Request-Id`、`X-Request-Id` 之外补充识别 `request-id`，写入现有的 `upstream_request_id` 列。这是独立的缺陷修复，不受追踪级别控制。

## 5. 未到达渠道的失败落库

这类失败写成一条普通的错误日志，全部使用 `logs` 表现有的列，不改表结构。

| 现有列 | 填什么 |
| --- | --- |
| `type` | 5，现有的"错误"类型 |
| `user_id`、`username`、`token_id`、`token_name` | 已鉴权的填实际值；未鉴权的填 0 和空串 |
| `model_name` | 能解析出来就填，否则为空 |
| `channel` | 0 |
| `quota`、`prompt_tokens`、`completion_tokens` | 0 |
| `request_id` | 返回给客户端的请求 ID |
| `content` | 错误消息，经 `common.MaskCredentials` 脱敏 |
| `ip` | 已鉴权的沿用"用户是否开启记录 IP"；未鉴权的直接记录来源 IP |
| `other` | `status_code`、`error_code`、`error_type`、`request_path`，以及 4.1、4.2 中能取到的字段 |

按能否归属到用户分两档，各配开关：

| 档位 | 包含 | 默认 | 理由 |
| --- | --- | --- | --- |
| 已鉴权的失败 | 请求校验失败、无可用渠道、用户限流、敏感词、计价错误 | 开 | 能归属到用户和令牌，数量受用户自身请求量约束 |
| 未鉴权的失败 | 令牌无效、IP 限流 | 关 | 不属于任何用户，被扫描时量可能很大 |

额度不足（403）默认仍不记录：余额耗尽的用户持续重试时每次都会写一行。待确认是否需要单独开关放开。

对用户侧的影响：

- 用户会在自己的日志页看到已鉴权的失败记录。
- 未鉴权的记录 `user_id` 为 0，不属于任何用户，只有管理员和 Root 能看到。
- 用量、RPM、TPM 统计只查 `type = 2` 的消费记录（`model/log.go:680`、`model/log.go:722`），不受新增错误行影响。

所有错误日志仍受现有的 `ERROR_LOG_ENABLED` 总开关约束：它关闭时，本节的记录同样不写。

## 6. 配置项

新增一组运行时设置，按现有模式注册到全局配置管理器（参考 `setting/operation_setting/general_setting.go`），后台修改后立即生效，不需要重启。

| 设置 | 取值 | 默认 | 作用 |
| --- | --- | --- | --- |
| 请求追踪级别 | `off` / `errors` / `all` | `errors`（待确认） | 见下表 |
| 记录未到达渠道的失败 | 开 / 关 | 开 | 第 5 节的已鉴权档 |
| 记录未鉴权的失败 | 开 / 关 | 关 | 第 5 节的未鉴权档 |
| 保存上游错误原文 | 开 / 关 | 开 | 4.4 的 `root_info.upstream_error_body` |

级别的含义：

| 级别 | 成功日志 | 错误日志 | 未到达渠道的失败 |
| --- | --- | --- | --- |
| `off` | 与现在完全一致 | 与现在完全一致 | 不记录，三个开关全部失效 |
| `errors` | 只多存 `other.trace` 的四项，约 80 字节 | 完整的 4.1、4.2、4.4 | 按开关 |
| `all` | 完整的 4.1、4.2 | 完整的 4.1、4.2、4.4 | 按开关 |

4.6 的上游请求 ID 修正不受级别影响。

## 7. 后端实现要点

### 7.1 采集点

| 数据 | 采集位置 | 说明 |
| --- | --- | --- |
| `trace.ms` | 上下文里已有的请求开始时间 | `processChannelError` 已在用同一个时间点 |
| `trace.req_bytes` | `common.GetBodyStorage(c).Size()` | 请求体已缓存，取大小是常数时间 |
| `client.*` | 请求进入时的请求头 | 解压中间件会删除 `Content-Encoding` 头（`middleware/gzip.go:91`），必须在它之前或之内记下 |
| `trace.resp_bytes` | gin 的 `c.Writer.Size()` | 流式时包含保活的 ping 帧；未写出任何内容时返回 -1，需按 0 处理 |
| `attempt` | `RelayInfo.RetryIndex` | 已存在 |
| `upstream.status` / `proto` / `header_ms` / `req_bytes` | `relay/channel/api_request.go:532` 的 `doRequest` | 所有走通用请求路径的渠道一处覆盖 |
| `shape.*` | 各请求 DTO | 见 7.2 |
| `shape.keys` | 请求校验失败的返回处 | 见 7.3 |
| `finish_reason`、`resp.*` | 各协议的流式与非流式响应处理 | 见 7.4 |
| `upstream_error_body`、`upstream.error_type` | `service/error.go:87` 的 `RelayErrorHandler` | 见 7.5 |

采集结果挂在 `RelayInfo` 上的一个追踪结构里，在生成日志 `other` 时写入。成功日志的入口是 `service/log_info_generate.go` 的 `GenerateTextOtherInfo`，错误日志的入口是 `controller/relay.go` 的 `processChannelError`，两处共用同一个写入函数，由它按级别决定写哪些层。

### 7.2 请求形状的轻量计数

现有的 `TokenCountMeta` 已包含消息数、工具数和文件列表，但只有在开启 token 统计或敏感词检查时才完整生成；其余情况走只取 `max_tokens` 的快速路径（`controller/relay.go:136`）。完整生成会拼接全部文本，开销不小。

需要为每种请求 DTO 提供一个只计数、不拼接文本的方法：遍历消息累加文本长度并按类型数多媒体个数。它的成本与消息条数成正比，不复制内容。级别为 `off` 时不调用；级别为 `errors` 时只在写错误日志时调用。

### 7.3 顶层字段名

只在请求校验失败时执行：把请求体解析成"字段名到原始值"的映射后只取键。约束：

- 请求体超过 1 MiB 时跳过。
- 最多保存 32 个字段名，每个最长 64 字符。
- 解析失败时不写入该字段。
- JSON 解析必须走 `common/json.go` 的封装。

### 7.4 结束原因与响应统计

这是改动面最大的一项：OpenAI 兼容、Claude、Gemini、Responses 四种协议各有流式和非流式两条处理路径，需要在各自解析响应的位置把结束原因、响应 ID、实际模型、选项数、工具调用数写到追踪结构上。部分位置已经在读取这些值（例如 `relay/channel/claude/relay-claude.go:97`、`relay/channel/gemini/relay-gemini.go:257`），只需顺手记录。

回复字符数优先复用流式处理中已有的累计文本；没有累计的路径按分片累加长度。实现时需逐条路径确认。

未覆盖到的渠道适配器保持字段缺省，前端不显示对应的行，不影响其他功能。

### 7.5 上游错误原文

`RelayErrorHandler` 已经读入完整的错误响应体。在这里截取前 4096 字节，随错误对象带回 `processChannelError`，经 `common.MaskCredentials`（传入当前渠道密钥）脱敏后写入 `root_info`。

`NewAPIError` 定义在 `relaykit/types/error.go:90`。给它加载体属于 `relaykit` 的公开接口变更，必须按项目规则单独验证：

```bash
cd relaykit && GOWORK=off go build ./...
```

### 7.6 未到达渠道的失败

- 中间件的统一拦截点 `abortWithOpenAiMessage`（`middleware/utils.go:14`）里补一次错误日志写入。鉴权、限流、渠道分发三个中间件在入口处各自标记当前阶段，拦截点读取后写入 `trace.stage`。
- 全局与 IP 限流没有走这个拦截点（`middleware/rate-limit.go:143`），需要单独补调用。
- `controller/relay.go` 里进入重试循环之前的几处提前返回（`:115`、`:142`、`:159`）和循环内选不到渠道的分支（`:200`）补调用。
- 是否已鉴权按上下文里是否有用户 ID 判断。
- 每个请求最多写一条这类记录。
- 未鉴权档按来源 IP 做每分钟限量，超出后丢弃并在本地日志计数。限量值待确认，建议每 IP 每分钟 10 条。

记录令牌无效这类事件属于认证审计。按 `AGENTS.md` 的认证安全要求，实现前需阅读 OWASP 的认证与会话管理速查表，并确保记录里不含令牌本身。本设计不保存 `Authorization` 头的任何部分。

## 8. 前端设计

可交互原型见 `request-trace-log-prototype.html`。

### 8.1 列表

不加列、不加筛选。现有的列是时间、渠道、用户、令牌、模型、流、Tokens、花费、耗时、详情，保持不变。

### 8.2 日志详情弹窗

改动集中在 `web/src/features/usage-logs/components/dialogs/details-dialog.tsx`。全部复用现有的 `DetailSection` 和 `DetailRow`（`log-detail-layout.tsx`），不新增通用组件。该文件已超过一千行，新增的分区放进同目录的一个新组件文件，由弹窗引用。

弹窗从上到下的结构，标"新"的是本次新增：

| 分区 | 可见范围 | 内容 |
| --- | --- | --- |
| 概览行（现有） | 按现有规则 | 请求 ID、上游请求 ID、渠道、重试链路、令牌、分组、响应时间。响应时间改用 `trace.ms`，精度到毫秒；没有该字段的旧日志沿用整数秒 |
| 请求转换（现有） | 管理员 | 不变 |
| 错误信息（新，红色分区） | 用户 | 状态码、错误码、失败阶段；管理员多一行上游错误类型。状态码和错误码现在已保存但未显示 |
| 请求信息（新） | 用户 | 请求体大小；管理员多出传输大小与压缩方式、发往上游的大小、4.2 的请求形状统计、User-Agent、客户端重试次数、请求头白名单 |
| 上游响应（新） | 管理员 | 有重试时逐次列出渠道、状态和耗时；状态与协议、响应头耗时、响应 ID、实际模型、指纹 |
| 响应信息（新） | 用户 | 响应体大小、结束原因；管理员多出回复字符数、工具调用数、推理 token，流式时的分片数、末片到达时间、是否收到结束标记和用量分片 |
| Root 诊断（现有分区，新增一行） | Root | 上游错误原文 |
| Token 明细、计费详情、流状态、内容（现有） | 按现有规则 | 不变 |

显示规则：

- 所有新增行都是"有值才显示"，分区内没有任何行时整个分区不渲染。
- 结束原因为空串时显示"缺失"标记，用来提示流被中断。
- 有重试时的逐次尝试来自同一请求 ID 的多条日志。详情弹窗目前只拿到当前这一条，要列出逐次尝试就得按请求 ID 再查一次；否则只能显示当前这一次。取哪种做法待确认（见第 14 节）。

### 8.3 后台设置

加在"系统设置 → 维护 → 日志维护"（`web/src/features/system-settings/maintenance/log-settings-section.tsx`），沿用该页现有的开关条目样式：

- 请求追踪级别：三选一。
- 记录未到达渠道的失败、记录未鉴权的失败、保存上游错误原文：三个开关。级别为"关闭"时三个开关置灰。

### 8.4 国际化与测试

- 新增文案需要补齐 en、zh、zh-TW、fr、ru、ja、vi 七个语言文件。
- 测试放在 `web/src/features/usage-logs/components/__tests__/`，覆盖：三种身份下各分区的显示与隐藏、字段缺省时不渲染空行、结束原因缺失的标记、旧日志（无追踪字段）渲染不变。

## 9. 性能与容量

以下为根据代码和原型示例数据的推算，未做压测。

每条日志 `other` 的 JSON 字节数：

| 记录 | 现在 | 级别 `all` | 加上游错误原文 |
| --- | --- | --- | --- |
| 非流式成功 | 148 | 755 | — |
| 流式成功 | 236 | 764 | — |
| 限流错误 | 156 | 528 | 859 |
| 校验失败 | 117 | 366 | — |

"现在"一列省略了部分计费字段，真实值更大，但增量是准的：级别 `all` 每条约多 400 到 600 字节，级别 `errors` 的成功日志约多 80 字节。上游错误原文最坏 4 KB，只出现在失败记录上。

| 环节 | 影响 |
| --- | --- |
| 写入 | 每个请求仍只写一条日志，写入发生在响应返回之后（`model/log.go:103`），不影响用户感知的延迟 |
| 列表查询 | 筛选和排序都走已有索引列，不读 `other`；每页多返回几十 KB |
| 统计查询 | MySQL 和 PostgreSQL 里行变宽后，按时间范围扫描的统计查询会按行宽增长的比例变慢；ClickHouse 是列存，不受影响 |
| 行数 | 未到达渠道的失败落库会增加行数，对表和每个索引都有成本，影响大于行宽 |

按级别 `all` 估算：每天 10 万条日志约多 50 MB，每天 100 万条约多 500 MB。

## 10. 兼容性与约束

- **数据库**：不改表结构，SQLite、MySQL、PostgreSQL、ClickHouse 都不需要迁移。`other` 是未限长的文本列。按项目规则这类改动不触发三库迁移验证，但仍需在三种关系库上各跑一次写入与读取，确认 JSON 文本往返无损。
- **新旧节点混跑**：旧节点读到新日志时，未知字段随所在层一起被保留或裁剪，行为正确。旧前端忽略未知字段。
- **旧日志**：没有追踪字段，前端各分区不渲染，无需回填。
- **`relaykit` 独立性**：7.5 涉及 `relaykit` 公开类型，需单独构建验证。
- **JSON**：所有序列化与反序列化走 `common/json.go` 的封装。
- **回退**：把级别设为 `off` 即恢复现有行为；已写入的追踪字段留在历史日志里，不影响任何功能。

## 11. 测试计划

后端，优先扩展现有测试文件，新增测试文件不超过一个：

- 追踪结构到 `other` 的写入：三种级别 × 成功与失败，断言各层出现的字段；零值与空对象省略。
- 身份裁剪：用户看不到 `admin_info.trace` 和 `root_info`，管理员看不到 `root_info`。
- 上游错误原文：超过 4096 字节被截断并置标记；渠道密钥被脱敏；开关关闭时不写。
- 未到达渠道的失败：两档开关各自生效；`ERROR_LOG_ENABLED` 关闭时不写；每个请求最多一条；未鉴权档的 IP 限量。
- 请求形状计数：四种协议各一个带多媒体与工具的请求，断言各项个数；不含的项不出现。
- 顶层字段名：拼错的必填字段出现在列表里；超过大小上限时跳过；损坏的 JSON 不写入。
- 上游请求 ID：响应头为 `request-id` 时被识别，且优先级低于 `X-Oneapi-Request-Id`。

前端见 8.4。完成前执行 `bun run typecheck`、涉及文件的 lint，以及受影响的测试文件。

## 12. 实施拆分

按可独立提交、独立验证的顺序：

1. 设置项与追踪结构；写入无需解析响应即可得到的字段（耗时、请求体大小、响应体大小、客户端信息、尝试序号、上游状态与协议、响应头耗时）；三种级别。
2. 请求形状的轻量计数；校验失败时的顶层字段名。
3. 各协议的结束原因与响应统计。
4. 上游错误原文与上游错误类型；Anthropic 上游请求 ID 的识别。
5. 未到达渠道的失败落库与两档开关。
6. 前端：类型、详情弹窗分区、后台设置、七种语言、测试。

第 3 步改动面最大，可以按协议拆成多次提交。第 4 步里的上游请求 ID 识别是独立修正，可以提前单独合入。

## 13. 讨论中已确定的决定

- 请求体和响应体正文不保存，也不保存截断片段。
- 失败时只保留上游错误原文；请求侧用顶层字段名列表代替请求体片段。
- 上游响应头整组不保存。
- 列表不加列；耗时沿用现有的列，请求与响应大小、结束原因只在详情里显示。
- 详情弹窗沿用现有布局与组件，只增加分区和行。
- 图片、工具、视频等统计项没有时不保存、不显示。
- 全部新增字段放在 `logs.other`，不改表。
- 未到达渠道的失败用现有的列写成错误日志。

## 14. 待确认

1. 请求追踪级别的默认值。取决于日请求量和日志库类型：量小或使用 ClickHouse 可以默认 `all`；每天百万级且使用 MySQL 或 PostgreSQL 建议 `errors`。
2. 额度不足（403）是否需要一个单独的开关放开记录。
3. 未鉴权档的每 IP 每分钟限量值。
4. 详情弹窗里重试的逐次尝试，是按请求 ID 再查一次，还是只显示当前这一条。
5. 请求头白名单是否保留 `anthropic-version` 等三项。
6. 在哪个分支上实现：沿用当前分支，或按既有约定新开 `custom/rc35-request-trace`。

## 15. 风险

- **各渠道适配器覆盖不全**：结束原因和响应统计依赖各协议的处理代码，冷门渠道可能缺字段。缓解：字段缺省时前端不显示，不阻塞其他功能。
- **`resp_bytes` 不等于上游响应大小**：它是写回客户端的字节数，经过格式转换后与上游原始大小不同，流式时还包含保活帧。文案上写"响应体大小"时需在提示里说明口径。
- **未鉴权日志被刷量**：依赖默认关闭和按 IP 限量。开启前应确认日志清理策略。
- **统计查询变慢**：行变宽对 MySQL 和 PostgreSQL 的范围统计有影响，上线后需观察；必要时把级别降到 `errors`。
- **上游错误原文里的敏感信息**：部分上游会在错误体里回显请求片段或账号标识。缓解：仅 Root 可见、凭证脱敏、可整体关闭。脱敏只覆盖凭证，不覆盖回显的用户内容。
