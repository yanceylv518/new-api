# 用户模型折扣移植设计与验收说明

文档日期：2026-09-09。折扣移植已提交为 `65d2bd225`，后续日志适配作为独立提交，见第 12 节；未推送或部署。实际通过、失败和未验证边界见第 11 节；不将基线检查失败或压力轮丢弃表述为通过。

本文是实施与审查依据，不是功能完成证明，也不授权提交、推送或部署。表名和新增接口内部结构属于建议设计；实施中若改变业务语义、缓存一致性或迁移范围，必须更新本文并明确差异。

## 1. 版本、来源与边界

| 项目 | 固定依据 |
| --- | --- |
| 仓库 | https://github.com/yanceylv518/new-api |
| 目标提交 | `bee45b58a3c0b77e8dc81e6b5aeb4474aa9058d1` |
| 目标目录 | `E:/WorkSpace/开发/GS/new-api-bee45b5` |
| 目标分支 | `custom/rc35-user-model-discount` |
| 源目录 | `E:/WorkSpace/开发/GS/new-api` |
| 源分支与 HEAD | `custom/rc23-user-model-discount`，`36995be82781f8e157500c3fc857317104d3aba0` |
| 完整功能来源 | 源 HEAD 中的折扣功能，加源工作树尚未提交的相关修改和未跟踪文件 |

`rc35` 是用户指定的分支命名约定；本文不将它当作已核实的官方发布标签。不得自动升级目标到 `origin/main` 最新提交。

历史折扣提交依次为 `882b98cbe3ec5d0d37baa9e1d7747a554096f2d9`、`a6589e02bfa7668a6da7743e307fef0938fec9f9`、`36995be82781f8e157500c3fc857317104d3aba0`。只提取这三个提交会遗漏批量编辑、金额记录、渠道统计和后续性能优化。

源工作树不是不可变提交。以下 Git blob ID 仅用于识别本轮读到的关键文件，不代表完整归档：

| 源文件 | `git hash-object` 结果 |
| --- | --- |
| `model/user_model_pricing.go` | `db9dca83238119a2a7546bd0867e26d0cd9fc6fc` |
| `model/user_model_pricing_cache.go` | `54494d452db56d8009cd163986545d1c32c70e79` |
| `types/discount_amounts.go` | `4bf84cb0b54b13f9edd7324e5a96df34f54ceaad` |
| `types/user_model_discount.go` | `985581e7009ea55894c6523f10354d657e9481ca` |
| `service/task_billing.go` | `a9ab2ec85bef34f81ed7cca2447074adeb28dc8f` |
| `web/src/features/users/components/dialogs/user-model-pricing-dialog.tsx` | `4d436c6313aa1ddf34d4955adc9bcfc8742f14df` |

实施前记录完整候选文件清单及内容指纹，包含未跟踪文件；若源文件变化，重新核对差异。保留源工作树全部用户修改，不通过 reset、覆盖或批量复制使两端相同。

## 2. 必须保留的行为

1. 折扣独立存储，不读取 `users.setting` 中任何历史折扣字段。
2. 未配置规则按 100% 计费；输入为空是校验错误，不自动转换成免费或原价。
3. 折扣以整数基点保存，合法范围 1–10000，对应 0.01%–100%；10000 不保存为有效折扣规则。
4. 管理弹窗展示跨全部分组的启用模型，仍执行目标用户管理权限校验；不因模型未展示而删除其已有规则。
5. 多选支持跨分页、跨筛选保留；表头全选作用于当前筛选结果的全部分页。批量应用只改草稿，保存时才写入。
6. 完整规则集上限 1000 条，包含界面不可见但保留的规则；批量应用超限应整批拒绝。
7. 已开始计费的请求使用冻结折扣；改价影响后续取得新快照的请求。异步任务持久化提交时的折扣，轮询不重新读取用户当前规则。
8. 用户和令牌承担折后费用；渠道 `used_quota` 记录排除用户模型折扣后的站点费用。该金额包含分组等站点倍率，不等于供应商实际成本。
9. 消费日志记录折前、折后、优惠三项；工具附加费和违规费用不参与模型折扣。
10. 保留只读快照、LRU、singleflight 和 Pipeline 的有效优化，不通过取消版本检查换取性能。

## 3. 目标版本已核实的架构差异

以下路径均相对于目标仓库；函数名用于定位，避免依赖移植后变化的行号。

| 依据 | 已核实事实 | 移植要求 |
| --- | --- | --- |
| `relay/common/relay_info.go`、`relay/helper/price.go` | 存在 `BillingModelName`、`GetBillingModelName` 与计费身份解析 | 折扣匹配需适配计费别名和修饰符 |
| `relay/relay_task.go:RelayTaskSubmit` | 任务有独立的用量表达式预扣路径 | 不能只改旧的按次和 token 分支 |
| `service/task_polling.go:settleTaskBillingOnComplete` | 表达式优先于适配器调整、token 重算；返回 bool 控制失败退款 | 保留目标控制流和返回语义，接入金额快照 |
| `controller/relay.go:executeTaskSubmissionWith` | 预留调整额度、插入任务、结算，再由 `presentTaskSubmission` 返回响应 | 不引入旧 `taskResponseWriter` |
| `model/token_cache.go:cacheInitToken` | 存在更新屏障；已有 Hash 只续期，不被回源快照覆盖 | 不复制 rc23 的通用 Hash 覆盖/保留字段方案 |
| `model/quota_reserve.go` | 已有原子预留与额度增减机制 | 复用，按目标契约验证折后额度 |
| `model/log.go`、`model/log_other.go` | 日志接受 `*LogOther`，区分 public/admin/root/audit | 三项金额通过 `SetPublic` 写入；不退回普通 Map |
| `model/task.go:TaskPrivateData` | 已有 Execution、PluginState、PollFailures、TieredSnapshot 等状态 | 增量扩展，不覆盖或删除目标字段 |
| `common/quota_math.go` | 单请求上限仍是 int32，钱包另有 `MaxWalletQuota` | 不以钱包 64 位化为由放宽单请求金额边界 |
| `logger/logger.go` | 日志计数及轮转标记仍为普通并发变量 | 独立并发缺陷，不能当作已被目标解决 |

历史补丁的 `git apply --check` 已报告多处无法应用。这只证明上下文存在冲突，不能据此计算移植工作量，也不能断言所有新增文件可直接编译。此前将整棵分支树的差异当作折扣改动范围是不准确的。

目标 `go.mod` 声明 Go 1.25.1；前端为 ESM、React 19、Rsbuild、Bun、Vitest、tsgo、oxlint。精确安装版本以目标锁文件和实际工具输出为准，不因移植升级依赖。实施 UI 前读取目标 `web/AGENTS.md` 与相关 UI/i18n 技能；涉及鉴权 Pipeline 时遵循目标鉴权安全规范。

## 4. 数据模型与大表迁移

### 4.1 实施采用两张独立表

| 表 | 字段与约束 | 目的 |
| --- | --- | --- |
| `user_model_pricings` | `id` 主键；`user_id`；`model_name`；`model_key`；`discount_bps`；唯一键 `(user_id, model_key)` | 保存有效折扣规则 |
| `user_model_pricing_revisions` | `user_id` 主键；非空正整数 `revision`，初始 1 | 空规则也有稳定版本，改价不更新 users 行版本列 |

模型名按确定后的计费身份规范化，长度上限沿用源实现的 128 字节校验；`model_key` 为规范化名称的 SHA-256 十六进制字符串，隔离数据库排序规则。查询后仍以完整模型名匹配，不把哈希作为面向用户的模型身份。

不为 `users` 新增 `model_pricing_version`，不为 `tasks` 或 `logs` 新增三项费用列。独立版本表是避免 users 新增列的设计选择，不是缓存一致性的替代品。

### 4.2 首次使用、并发和删除

- 推荐在首次管理读取或计费回源时惰性创建版本行，不扫描历史 users 批量回填。禁止先创建新版本行却返回不存在用户的有效折扣状态。
- 初始化、规则读取/替换与用户硬删除必须采用一致锁顺序：先用户行，再版本行。MySQL/PostgreSQL 使用 `lockForUpdate`；SQLite 采用可验证的事务写入/CAS 方案，不依赖无效的 `FOR UPDATE`。
- 版本行初始化必须使用数据库唯一约束保证幂等；不要在 PostgreSQL 事务中吞掉唯一冲突后继续使用已失败事务。
- 替换规则在同一事务内校验版本、递增版本、删除旧规则并插入新规则。两个相同 revision 的并发保存只允许一个成功，另一个返回冲突。
- 硬删除在同一事务内清理规则和版本行，并处理本地快照及 Redis 状态。验证“改价与删除并发”不留下孤儿记录，不复活已删除用户；用户 ID 不得在缓存有效期内被复用为另一身份。
- 版本递增必须检查整数上界；不得溢出、回绕或以 1 重新开始。HTTP revision 使用 JSON 数字时还需限制在 JavaScript 安全整数范围内并明确拒绝超界写入。

独立表避免 users 新增列及启动回填，但上述锁仍可能与同用户余额写入竞争；只有缓存命中路径不需要该数据库锁。不能宣称“独立表完全消除锁竞争”。

### 4.3 迁移范围

本次不迁移旧折扣数据，不添加 `users.setting` 兼容读，不搬入旧 `model_key` 回填和旧索引切换代码。但必须验证含真实旧用户、任务、日志的目标数据库可以新增两张表，原有数据不变，重复启动不重复 DDL。

若目标部署实际已存在旧折扣表，应停止按“首次安装折扣功能”执行，重新确定数据处理范围；不得自动删除表或覆盖现有规则。既有无金额快照任务的安全处理是运行兼容，不是旧折扣数据迁移。

## 5. 缓存协议与请求快照

保留独立定价版本键、本地不可变规则快照及提交前 pending 屏障。建议沿用现有缓存参数：30 秒绝对 TTL、120 秒 pending TTL、5 秒查询预算、单次 Redis 操作 1 秒；LRU 默认 4096 用户及估算 64 MiB。它们来自源实现，必须在目标环境重新验证，不能作为一致性证明或容量承诺。

写入协议：锁定用户/版本 → 校验 revision → 递增版本并替换规则 → 提交前发布 pending 屏障 → 提交事务。Redis 已启用而屏障发布失败时回滚，不能成功返回后继续让其他节点命中旧价。

读取协议：鉴权 Hash 与鉴权/折扣版本可合并 Pipeline，保持先 Hash、后版本校验；只有本地快照版本满足读取结果且未过期才使用缓存。未命中在一致事务内读取版本与规则，持锁发布已提交版本；事务完成后构造只读快照。迟到旧回源不能覆盖较新缓存。

需要证明的异常行为：

| 情况 | 要求 |
| --- | --- |
| Redis 未启用、不可用、版本键丢失或格式非法 | 按数据库权威状态回源，不能解释为原价 |
| 数据库回源失败 | 返回明确错误，不能使用未知折扣或静默降级 |
| 保存回滚留下 pending | 暂时回源，不能把未提交 revision 当成已提交数据 |
| Redis 重启/清空、旧读晚到 | 不发布或复活失效版本；验证跨节点实际行为 |
| 单个请求取消 | 等待受 Context 约束；singleflight 其他等待者最多按既有规则重试一次 |
| LRU 淘汰、禁用缓存、内存预算不足 | 仅影响命中率，不改变账单 |
| 改价与读请求重叠 | 允许已取得旧快照的在途请求完成；保存成功后才开始获取快照的请求必须读新价 |

最后一项需按请求取快照的时间点判定，不能把“返回时间晚于保存”的所有请求都当作旧价错误。TTL 依赖事务和网络时间预算；Redis 长时间挂起、数据库提交延迟等边界需故障注入验证，不声称有限 TTL 在任意停顿下均提供强一致性。

多节点要求同一权威数据库和 Redis、相同缓存协议及启用配置。Pipeline 只减少网络往返，不是事务，也不能替代鉴权版本屏障。普通偏好或令牌信息刷新不得回写并发扣减后的额度。

## 6. 计费身份与金额计算

### 6.1 模型身份适配决策

源实现按 `FormatMatchingModelName(OriginModelName)` 查折扣。目标文本计费先解析 `BillingModelName`，路由还有 `RoutingMatchModelName`。不能假设三者等价。

折扣与公开计费身份一致：文本以已解析的 `GetBillingModelName()` 为依据；任务以用户请求的公开模型身份为依据。统一调用 `model.ResolveUserModelPricingName` 与 `FormatMatchingModelName`。渠道映射或上游表达式回退决定实际费用公式，不将用户规则切换为上游内部模型名。管理目录、API 保存规范化、价格展示和请求查找共用同一解析。

实施前固定用例：无修饰符、`@` 修饰符、显式计费别名、基础模型回退、Gizmo、Gemini thinking、大小写不同名称、渠道 model_mapping。存在显式独立价格的别名不能被无条件合并到基础模型。若目录中启用的是路由名而实际计价名不同，界面必须能明确它编辑哪条规则。

上述解析提取自目标现有价格解析函数，保留显式定价别名优先级；目标原有别名/修饰符测试继续保留，并新增折扣与渠道测试隔离用例。

### 6.2 三项费用的定义

令 M 为本次用量按模型、分组及非用户折扣倍率计算的未取整费用，T 为不打折的附加费，d 为冻结折扣。对本计费路径的既有取整/最低额度函数 Q：

```text
折前 = Q(M + T)
折后 = Q(M × d + T)
优惠 = 折前 − 折后
```

Q 不是全项目统一改成一种舍入：保持各路径既有 truncation/round、无有效用量、免费模型、免费组和最低额度规则。100% 折扣必须与目标原价行为一致，不能顺带改变目标原价的取整语义。

同次用量和表达式结果计算两种金额，不通过折后整数除以 d 反推。折扣只能生效一次；适配器倍率、请求字段、旧 remix 任务都不能覆盖保留折扣键。渠道测试仍按原价路径运行。

文本表达式与任务表达式有不同单位：目标 `pkg/billingexpr/settle.go` 对 TaskUsageBilling 使用表达式输出乘 QuotaPerUnit，普通对应路径使用每百万换算。复用目标计算结果，不复制旧公式到任务分支；实施前完整阅读 `pkg/billingexpr/expr.md`。

所有金额经目标额度转换器校验；捕获折前及折后饱和信息，保留管理员可见审计。非法值不得生成负费用或伪造的三项明细。钱包上限与单请求上限分开验证。

### 6.3 入口覆盖

| 链路 | 必须接入的位置/行为 |
| --- | --- |
| 普通文本、Responses、图片、嵌入、重排 | 鉴权快照、ModelPriceHelper、实际用量结算、日志、渠道统计 |
| 文本阶梯表达式 | 预扣、auto 跨组重新预留、终态表达式费用和附加费 |
| 音频及实时音频 | 增量预扣与最终结算一致；不能重复扣同一段用量 |
| MJ/换脸 | 目标现有预扣与失败补偿流程，折前渠道统计 |
| 原生/插件异步任务 | 估算、提交后倍率调整、落库快照、表达式/适配器/token 三条终态路径 |
| remix/续作 | 继承允许的时长等参数，使用新请求折扣，不能继承旧用户折扣 |
| Playground | 与真实消费同一折扣语义，保留其令牌处理契约 |
| 渠道测试、违规费用 | 不套用用户模型折扣 |

## 7. 异步任务、日志与渠道统计

复用 `tasks.private_data.discount_amounts` 保存最新任务总金额，并在 BillingContext 中保存提交时折扣。JSON 的 Scanner/Valuer 空值判断需纳入新增字段，避免只有金额时被序列化为 NULL。

MJ 没有 `private_data`，实施复用原有 `properties` JSON 的站内保留键 `new_api_discount_amounts`，不增加第三张表或大表列。提交将实际 PriceData 传给持久化入口；轮询合并上游属性时保留该键并忽略上游同名值。退款先校验快照，再分别退用户折后金额和渠道折前金额。旧 MJ 无此键时保留原退款口径；快照损坏时明确报错，不反推金额。

任务结算必须区分两类差额：用户/令牌为新折后减旧折后，渠道为新折前减旧折前。即使折后差额为零，折前变化仍需更新渠道及任务快照。失败全退后快照归零；退款日志原有 quota 仍为本次退款额。

普通消费日志三项对应本次费用；异步调整日志三项对应任务总额，设置 `discount_cost_scope: "task_total"`，不得将多条任务总额当流水累计。历史缺失或求值失败时不生成推算明细，按目标既有可靠额度处理。

目标 `LogOther.SetPublic` 用于三项金额与折扣；饱和、渠道或其他敏感信息仍走目标权限分层。`types` 包不能为了 AddToLog 导入 `model` 形成循环依赖，金额到 LogOther 的组装可放服务层。

更新任务 JSON 时保留 Execution、PluginState、PollFailures 等目标状态；评估旧对象整段回写与并发轮询的覆盖风险，使用目标现有 CAS/事务所有权约束或最小必要保护。不得用旧任务结构覆盖新版本文件。

资金、令牌、渠道、任务快照、日志并非天然跨库原子提交。移植不得引入 outbox/收据系统，也不得因此宣称故障下 exactly-once。须注入各写入点失败，保证错误可见、已成功状态不被误当成未处理重试，并明确残留不一致和对账方式；如折扣新增逻辑无法在现有约束下安全完成，报告具体缺陷，不能以吞错验收。

部署前渠道已有使用金额保留历史口径；切换后增量才按新规则。记录切换时间，不能回填或用折扣反推旧累计。旧任务无金额快照时沿用原已知统计口径，避免将其假装为完整折前历史。

## 8. 文件级移植清单

| 源/目标范围 | 处理方式 |
| --- | --- |
| `model/user_model_pricing*.go` | 移植规则与缓存业务；替换版本存储，去掉本次不需要的历史回填；更新相关测试 |
| `types/user_model_discount*.go`、`types/discount_amounts*.go`、`types/price_data.go` | 保留只读所有权、金额数学及倍率隔离；调整日志适配边界 |
| `controller/user.go`、`router/api-router.go` | 增量接入管理 GET/PUT、revision 冲突、目标权限和审计 |
| `middleware/auth.go`、`model/user_cache.go`、`controller/playground.go`、`constant/context_key.go`、`relay/common/relay_info.go` | 接入只读快照与 Pipeline，保留目标鉴权和余额机制 |
| `controller/pricing.go`、`model/pricing.go` | 用户专属响应副本及缓存隔离；保留任务用量 schema 等新字段 |
| `relay/helper/price.go`、`relay/mjproxy_handler.go`、`relay/relay_task.go` | 手动重接目标价格、预扣及任务分支，不能覆盖整文件 |
| `service/quota.go`、`text_quota.go`、`tiered_settle.go`、`task_billing.go`、`task_polling.go`、`log_info_generate.go`、`violation_fee.go` | 同次折前/折后核算，目标日志类型，退款和渠道差额 |
| `model/task.go`、`controller/relay.go` | 在目标任务持久化流程内保存金额和折扣，不移植旧响应写入器 |
| `web/src/features/users/` | 复用最新批量编辑及草稿保护，适配目标权限/公共组件 |
| `web/src/features/pricing/` | 保留目标新模型卡、任务矩阵和动态定价展示，增量加折扣；用户/会话隔离查询缓存 |
| `web/src/features/usage-logs/` | 展示真实金额及任务总额语义，不重建旧日志页面 |
| `web/src/i18n/locales/`、`.env.example` | 合并必要翻译和两项缓存参数，不覆盖目标新增内容 |
| `tools/pricing-load/` | 适配作隔离验证工具；不启动生产服务，不纳入运行时依赖 |

`tools` 的源码是否纳入提交遵循用户此前屏蔽要求，在最终提交清单中明确；测试结果、数据库和可执行文件保持本地。排除工具源码不等于省略验收。

不移植 `controller/task_response_writer*.go`，不替换目标 `model/token*.go`/`common/redis.go` 的完整实现，不夹带邀请、认证、供应商、素材库或整套计费架构重构。日志计数 race 若阻断测试，应单列最小修复与证据，后续分支遵循 `custom/rc35-功能名`，不混称为折扣业务。

## 9. 测试矩阵与验收口径

下表定义验证关注点；目标版本的实际执行证据及未验证边界见第 11 节。源版本的历史测试通过记录不作为目标验证结果。

| 类别 | 核心场景 | 必须断言 |
| --- | --- | --- |
| 三数据库迁移 | 真实 SQLite、MySQL、PostgreSQL；空库/有目标旧数据；两次启动 | 两表与唯一约束正确，无 users/tasks/logs 新增费用列，无旧数据变化 |
| 版本事务 | 首次并发初始化、同版本保存、清空规则、取消、删除并发 | 一个有效版本，无半套规则、孤儿行或失效用户状态 |
| 缓存 | 热/冷/空规则、超预算、禁用、Redis 丢键/故障、回滚屏障、迟到回源 | 新读使用新规则、在途快照不变、无跨用户污染、失败不按原价 |
| 鉴权 | 角色/封禁/撤销/过期、Pipeline 错误与回源 | 优化不绕过目标访问控制，不修改余额 |
| 模型身份 | 显式别名、修饰符、基础回退、大小写、渠道映射 | 管理、价格展示、预扣和结算匹配一致 |
| 金额 | 100%、80%、0.01%、免费组、无用量、最低额度、上界与非法值 | 精确预期整数；Before ≥ After ≥ 0；Savings = Before − After |
| 同步链路 | 钱包/订阅、流式/非流式、音频/WSS、图片/MJ、工具费用 | 数据库与缓存额度、三项日志、渠道增量一致 |
| 异步链路 | 表达式/适配器/token、remix、改价、进程重启、重复终态、零差额 | 沿用提交快照，用户折后/渠道折前，不重复套折扣或清除插件状态 |
| 故障 | 任务插入、资金、令牌、渠道、日志、JSON 回写分别失败 | 无虚假成功；补偿及重试不重复扣退；剩余风险有实际证据 |
| 日志隔离 | 多用户、多模型、主库与日志库分离 | 每条 request/task ID 对应正确快照；普通用户无敏感字段 |
| UI | 空输入、跨页全选、筛选、隐藏规则、409、关闭重开、切换用户 | 草稿不被后台刷新覆盖，保存范围准确，无跨账号缓存泄漏 |

外部数据库测试必须记录实际引擎版本；依赖最低版本特性的变更覆盖最低支持版本。ClickHouse 若作为目标支持的日志后端部署，需单独验证原日志写入及 JSON 金额，不将关系库通过等同于 ClickHouse 通过。

复用现有测试层，优先扩展合适用例，不机械按文件新增测试。源端可参考 `TestUserModelPricingImmutableRequestSnapshot`、`TestUserModelPricingPipelinePreservesAuthAndPricing`、`TestDiscountChannelStatisticsAcrossBillingPaths`、`TestFinalBillingHTTP`、`TestConcurrentDiscountLogSnapshotIsolation`。名称存在不代表目标已包含或已覆盖新表达式路径。

### 9.1 执行命令与前置条件

在目标根目录，用 PowerShell 7 执行；这些是移植完成后的命令，不是本轮执行结果：

```powershell
git.exe diff --check
go.exe version
go.exe test ./types ./model ./relay/helper ./service ./controller ./middleware -count=1
go.exe build ./...
go.exe vet ./...
```

已适配的折扣专项真实数据库/HTTP 测试在隔离环境中执行：

```powershell
$env:PRICING_EXTERNAL_TESTS = '1'
go.exe test ./model -run '^TestUserModelPricingExternalDatabases$' -count=1 -timeout=180s
go.exe test -race ./tools/pricing-load -run '^(TestFinalBillingHTTP|TestConcurrentDiscountLogSnapshotIsolation)$' -count=1 -timeout=15m
```

命令以前述测试及工具已移植、fixture 与新两表一致为前提；必须检查实际执行数量与 skip，不能把“无测试匹配”或环境变量未启用当作通过。源码工具现存参数和数据清理逻辑需先审查；只使用专用测试数据库/Redis，不读取生产凭证。执行后恢复测试环境变量。

Windows race 需可用的 Go CGO 与匹配架构 C 编译器。先检查 `go.exe env CGO_ENABLED CC GOOS GOARCH`；若受日志基础设施既有 race 阻断，保留失败报告，不能关掉 race 或删测试来判通过。改动 relaykit 公共接口时，另在该模块内设置 `GOWORK=off` 执行 `go.exe build ./...`，然后恢复环境。

在目标 `web` 目录执行：

```powershell
bun run test src/features/users src/features/pricing src/features/usage-logs
bun run typecheck
bun run lint
bun run format:check
bun run build
```

生产行为改动还需最终完整 `go.exe test ./...` 及必要前端交互验证。已有失败必须在未改目标基线上复现并分类，不能仅根据 rc23 的历史失败推断 rc35 也有同样基线。

### 9.2 折扣专项负载

性能测试只评价折扣相关路径；受控上游仅用于确定用量及结算对账，不扩展为全项目容量项目。

建立 A/B/C：A 为固定目标的无折扣功能基线；B 为移植版空规则；C 为移植版启用折扣。A/B 衡量接入开销，B/C 衡量规则与金额计算开销。原有 `-full-price` 仍运行折扣版本检查，不能冒充 A。

分开报告微基准、鉴权/定价 HTTP、完整结算正确性负载。至少覆盖 100 用户×1000 规则、1000 用户×100 规则、超过缓存预算和同用户热点；每档至少 90 秒覆盖多轮 TTL，执行中改价。高档位从可稳定档位递增，6000/8000 RPS 仅为候选施压值，不是预先保证的容量。

每档串行重复至少三次；固定硬件、引擎、连接池、缓存预算、二进制及数据集。记录完成/失败/丢弃/错价数、p50/p95/p99、实际吞吐、Redis 往返、规则 SQL、数据库等待、分配量、GC。race 用于正确性检测，不用于性能对比。

账务错配、跨用户污染、重复扣退必须为零；某档存在请求失败或调度丢弃时，该档不算稳定容量。延迟与吞吐的性能接受预算应在测量前记录，当前尚无用户批准的数值阈值；未定阈值时只报告对比和回归，不宣称“性能影响可忽略”。本地工具吞吐不能外推 VPS 容量。

## 10. 部署、回退与完成清单

本次已授权实施和本地验收，未授权提交、推送或部署。未来发布先记录构建版本、目标数据库 schema、备份与切换时间；全部负责消费的节点须运行兼容折扣协议的版本，避免无折扣旧节点按原价收费。

回退到完全不支持折扣的目标基线会忽略用户折扣，也可能错误处理仍在途的折扣任务，不能视为无损回滚。需要停止新请求并妥善处理在途任务，或回退到仍理解折扣快照的兼容构建；保留两表、金额 JSON 和对账证据，不删除数据求恢复。

| 阶段 | 交付物与依赖 | 当前状态 |
| --- | --- | --- |
| 来源核对 | 固定目标、识别源未提交功能、架构证据 | 已核对；交付前重新核对第 1 节六个关键来源指纹均一致 |
| 设计 | 两表、模型身份、缓存协议、账务范围与验收矩阵 | 已实施，实际差异及验证边界见第 11 节 |
| 后端移植 | 数据模型→快照→全部计费入口→任务及日志 | 已接入；最终专项回归通过 |
| 前端移植 | 管理 API 就绪后接入批量编辑、模型广场、金额详情 | 已接入；255 项前端测试及生产构建通过 |
| 验证 | 三库、HTTP 对账、故障注入、race、前端、专项负载 | 已执行；同库/分库日志 race 通过，18 轮压力结果与基线限制见第 11 节 |
| 最终审查 | 最小 diff、无遗漏功能、无旧代码覆盖、风险与测试证据 | 已检查最终 diff；源工作树关键指纹一致；专用测试容器已移除 |
| 提交/推送/部署 | 按用户明确授权分别执行 | 本次未授权执行 |

完成移植必须同时交付代码、更新后的本文、实际命令/版本/测试数量与结果、性能报告及未解决风险。不得把编译通过、某一阶段结束或历史 rc23 测试通过作为整个移植完成。

## 11. 本次实施及实际验证记录

### 11.1 已核实的实施差异

- 两表存储替代源版本的 users 版本列；不搬入旧规则迁移和兼容读取。
- 任务金额原地扩展 JSON；JSON 回写在事务中重新读取当前记录，仅替换金额，保留插件状态。
- 原价保持目标路径的转换规则：`QuotaFromDecimalChecked` 四舍五入，`QuotaFromFloatChecked` 截断；新增最低额度只在原价本来收费且实际应用折扣时生效。
- 原价行为有一个明确修复：目标 `PostWssConsumeQuota` 创建 `QuotaInfo` 时漏传 `ModelPrice`，导致配置固定价格的实时请求结算为 0。本次传入原始固定价格，保证实时链路的用户收费和折前渠道统计准确；`TestDiscountChannelStatisticsAcrossBillingPaths/realtime` 覆盖 100% 与 50% 两种情况。不能将此项表述为所有无折扣场景逐位不变。
- 旧任务缺少显式 `user_model_discount` 标记时保留旧定价回退；新任务保存包括 100% 在内的冻结倍率。
- 管理接口覆盖权限、跨分组启用模型目录、非法/重复规则、revision 冲突；未改目标认证、令牌预留和任务提交状态机。
- 原版 `logger.logHelper` 的 `logCount++` 被真实 HTTP race 检测命中。只将计数和轮转调度标记改为原子变量，保留原日志设施；未引入 outbox、收据或日志库结构变化。
- `tools/` 按用户要求忽略；其中保留隔离测试工具、原版归档和聚合压测报告，不进入产品提交。

### 11.2 已执行检查

环境：Windows amd64；Go 1.26.0（模块声明 1.25.1）；Bun 1.3.14；Rsbuild 2.1.6；MySQL 5.7.44、PostgreSQL 17.11、SQLite 3.50.4。测试端口仅绑定回环地址：13326 / 15427 / 16389。

| 命令/场景 | 实际结果 |
| --- | --- |
| `go test ./model -run '^TestUserModelPricingExternalDatabases$' -v -count=1 -timeout=180s`，`PRICING_EXTERNAL_TESTS=1` | MySQL/PostgreSQL 通过：两表重复安装、版本冲突、真实行锁取消、大小写/重音区分、清空规则、普通任务/MJ JSON 金额重载 |
| `go test -race ./model -run '^TestUserModelPricing' -count=1 -timeout=5m` | 通过，包含本地缓存、Pipeline、回滚 pending、Redis 清空、故障回源、取消、内存预算 |
| `go test -race ./tools/pricing-load -run '^(TestFinalBillingHTTP\|TestConcurrentDiscountLogSnapshotIsolation)$' -count=1 -timeout=10m`，`PRICING_EXTERNAL_TESTS=1` | 修复原版日志计数 race 后通过（86.149 秒）；两种真实数据库分别对账；日志隔离每库 100 用户、10,000 请求，混合三种计价路径及在途改价 |
| `go test ./controller -run '^TestUserModelPricingManagementContract$' -count=1` | 通过 |
| `go test ./service -run '^TestDiscountTaskZeroDeltaPreservesPluginState$' -count=1` | 通过；零差额、插件 JSON 保留、快照落库失败时不变更渠道 |
| `bun run test src/features/users src/features/pricing src/features/usage-logs --maxWorkers=2` | 26 文件、255 测试通过；新增单位价格场景后，该测试文件 3 项全部重跑通过，总用例数 256 |
| `bun run typecheck`、`bun run build` | 通过 |
| `go build ./...`、`go vet ./...` | 最终微调后再次通过 |
| `go test ./types ./model ./relay ./relay/helper ./controller ./service ./middleware ./router -run 'Test.*(Discount\|Pricing\|Midjourney\|Tiered\|Task\|Quota\|HardDeleteUser)' -count=1 -timeout=5m` | 最终 8 个包均通过；包含模型身份、表达式回退、折前饱和审计和旧任务原价兼容 |
| `go test -race ./model ./service -run 'TestUserModelPricing\|TestHardDeleteUser\|TestDiscount\|TestAudioDiscount\|TestMidjourneyRefund' -count=1 -timeout=5m` | 最终通过（20.192 / 25.556 秒） |
| 同一组 HTTP/日志隔离 race 命令，另设 `PRICING_SEPARATE_LOG_DB=1` | MySQL/PostgreSQL 均通过（91.598 秒）；日志实际位于独立 `pricing_logs_review` 数据库，测试结束关闭连接并删除该隔离库 |
| `bun run format:check` | 全量因未修改文件的格式问题失败；报告未列出本次改动的前端文件。检查脚本已恢复临时格式变更 |
| `go test ./... -count=1 -timeout=10m` | 未全绿：路由夹具缺新表已修复；其余已识别基线失败见下文，最终结果不得表述为全量通过 |

未修改的 `bee45b58...` 归档实际复现了 `TestObserveChannelAffinityUsageCacheByRelayFormat_UnsupportedModeKeepsEmpty` 的共享计数失败，以及 `TestSecurityLoginPasskeyConcurrentCompletionCreatesOneSession` 的 Windows `audit.db` 文件仍被占用导致清理失败。全量前端 lint 也存在未涉及文件的既有错误；最终交付分别记录全量状态和变更文件状态，不通过关闭检查掩盖它们。

### 11.3 负载口径与限制

`tools/pricing-load/results/` 保存每轮 JSON。A 使用未修改目标核心加测试入口；B 使用移植版初始空规则；C 使用移植版有效规则。B/C 的测量中途将一个用户改为 30%，校验旧在途快照及新读价格；A 没有折扣接口，只保留相同调度时点。它们测试真实鉴权和预扣估算 HTTP，不调用供应商，不代表全网关或 VPS 最大容量。

独立最终结算测试会检查用户/令牌数据库与缓存、渠道累计、每请求日志三项金额；负载工具本身只检查用户和预扣价格。两种测试的结果不能混用。

当前不宣称任意进程中断下的跨库 exactly-once。目标原有资金、令牌、任务标记和日志分步写入仍存在部分失败后的人工对账需求；本次未引入 outbox。尚未验证 ClickHouse 实例、任意长时间进程停顿或真实供应商端故障，因此不能将现有测试描述成这些条件下的保证。

### 11.4 实测负载结果

共 18 轮，每轮 90 秒，串行运行。主性能比较使用 MySQL 5.7.44；PostgreSQL 17.11 另执行真实最终结算及日志隔离，不冒充两种引擎都有同样的容量数据。每轮均在计时中途执行改价（A 为同时间点空操作）。延迟从计划调度时点算到 HTTP 完成，包含发生器调度等待；p95/p99 下表取三轮中位数。

| 场景 | 用户 × 规则 | 目标 RPS | 计划请求合计 | 调度丢弃 | HTTP 失败 / 错价 | p95 / p99（ms） | 每轮规则查询 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| A 原版核心 | 100 × 1000 个候选模型 | 1200 | 324000 | 149 | 0 / 0 | 16.63 / 69.82 | 0 |
| B 初始空规则 | 100 × 1000 个候选模型 | 1200 | 324000 | 0 | 0 / 0 | 12.93 / 15.36 | 293–296 |
| C 全量折扣 | 100 × 1000 | 1200 | 324000 | 109 | 0 / 0 | 13.36 / 17.69 | 294–298 |
| 多用户 | 1000 × 100 | 1200 | 324000 | 0 | 0 / 0 | 15.21 / 28.66 | 2954–2966 |
| 同用户热点 | 1 × 1000 | 6000 | 1620000 | 587 | 0 / 0 | 17.19 / 21.20 | 3 |
| 强制缓存淘汰（1 MiB） | 16 × 1000 | 20 | 5400 | 0 | 0 / 0 | 88.76 / 97.02 | 1800 |

总计计划 **2,921,400**，完成 **2,920,555**，调度丢弃 **845**。完成请求的 HTTP 失败和错价均为 0。发生调度丢弃的轮次均按验收失败保留，不能称为稳定容量；尤其不保证 6000 RPS。多用户 1200 RPS 三轮均通过，只代表本机本配置，不外推生产部署。

默认预算下，100 用户 × 1000 规则约每轮 296 次规则查询，说明每请求不再全量拉规则；热点只发生 3 次规则查询。三轮每请求分配字节中位数，A 约 20.0 KiB，C 约 21.9 KiB，C 较 A 约增加 9.8%。尾延迟受本机调度波动影响，不能以 C 的 p95 低于 A 宣称移植加速。没有单独采集 Redis 服务端往返延迟或 CPU profile；热读的两条合并命令由专项 Pipeline 测试断言。

将缓存限制为 1 MiB 后，本数据集几乎每请求都查询 1000 条规则，每请求分配约 415 KiB。该场景证明淘汰不会造成错价，也说明过小预算会显著降低性能；默认保留 4096 用户 / 64 MiB，按实际规则数量调整。

复现入口为 `tools/pricing-load/pricing-load.exe -engine mysql -rate 1200 -seconds 90 -users 100 -rules 1000 -output ...`；B 另加 `-full-price`，A 使用 `pricing-baseline.exe`。所有原始 JSON 在 `tools/pricing-load/results/`，退出码非零的报告同样保留。测试二进制 SHA-256：

- 移植版：`4AC97EE6813F3C9B9F6BB2128DAB661899AC37D2CB8269024962E3F5AEC5A35F`。
- 原版核心：`0515E53E2CCD2B506503DFE7CBB4E3C25D40892E40614E1750D0AA9F206E4DD0`。

这两个二进制固定了负载使用的代码；负载后的 MJ 退款、SQLite 快照写锁、删除缓存和审计收尾改动不在该性能 HTTP 路径内，另由最终回归/race 验证。

### 11.5 鉴权核对与交付状态

鉴权相关复核参照 [OWASP Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)、[Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)，以及稳定发布 ASVS 5.0.0 的 [V7](https://github.com/OWASP/ASVS/blob/v5.0.0_release/5.0/en/0x16-V7-Session-Management.md) / [V8](https://github.com/OWASP/ASVS/blob/v5.0.0_release/5.0/en/0x17-V8-Authorization.md)。相关控制范围为服务端授权（8.2.1、8.2.2、8.3.1）、删除账户后的会话失效（7.4.2）及敏感响应缓存隔离。对应证据为管理接口权限测试、Pipeline 鉴权屏障测试、硬删除墓碑/缓存清理测试、用户与会话隔离的价格缓存键和 `private, no-store` 响应。未对整个系统进行 ASVS 合规认证。

折扣验收时工作树位于 `custom/rc35-user-model-discount`，基线为 `bee45b58a3c0b77e8dc81e6b5aeb4474aa9058d1`；后续提交见第 12 节。`git diff --check` 通过。原版归档、压力工具、二进制和 JSON 报告保留在已忽略的 `tools/`，用于复查。本次 `newapi-rc35-pricing-review` 的三个临时容器及网络已移除，其他环境未操作。

## 12. 独立日志适配提交

折扣功能先单独提交 `65d2bd225`。按用户后续要求，将远端 `custom/rc23` 的以下三个提交适配为当前分支的另一个提交；已通过 fetch 和祖先关系检查确认来源。

- `eaea656769ab1b8fb0c50ccd7415d563e0737c3a`：日志默认时间窗口改为当前时刻前后一小时；用户自助视图不显示模型映射，管理员全量视图继续显示。这是 UI 显示范围调整，保留 rc35 现有 API 权限分层。
- `c40e9df6e2863e4ae19e34e0c0631a05ce30d46b`：关系型数据库的用户日志按用户/request_id 只显示 ID 最大的最终记录，管理员保留完整链。筛选和计数共用相同条件；空串和 NULL 请求 ID 的历史行独立保留。ClickHouse 展示 ID 不具备此顺序，因此沿用原行为。
- `3d1eaf61fa6524a5d5d3ecf263067774c232ec11`：rc35 没有源提交引用的 `idx_logs_user_created_type`，改用现有 `idx_user_id_id` 的 MySQL 优化器注释提示，匹配用户条件和 ID 排序，不增加日志大表索引。MySQL 5.7 等不支持该提示的版本会忽略注释；兼容测试不等于这些版本已获得强制索引效果。

验证：`PRICING_EXTERNAL_TESTS=1 go test ./model -run '^TestUserLogFinalOutcomeDatabaseMatrix$' -v -count=1 -timeout=3m` 在 SQLite 3.50.4、MySQL 5.7.44、PostgreSQL 17.11 通过；覆盖分页、类型/模型筛选不复活中间错误、跨用户同 request_id、空/NULL 请求 ID、管理员完整链。`go test ./model -count=1 -timeout=3m` 和 `go build ./...` 通过。前端 `bun run typecheck` 通过；`bun run test src/features/usage-logs --maxWorkers=2` 为 13 个文件、114 项全部通过。
