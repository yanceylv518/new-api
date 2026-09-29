# 私域素材库 API Key 接口

使用 `Authorization: Bearer sk-...` 调用 `/v1/seedance`。这是网关提供的用户级
素材管理 API，不是上游厂商 Action 接口的原样透传。
后台 `/api/user/seedance` 接口继续使用后台凭证；两者读取相同的用户数据。

## 归属、权限与计费

- 素材组和素材归属于 `user_id`，不绑定创建它的 API Key。同用户所有有效 Key
  可以列出、上传、刷新、修改和删除该用户的素材，包含后台已创建的历史数据。
- 不接受调用方指定用户归属；其他用户即使知道素材或组 ID，也不能访问对应资源。
- API Key 的禁用、删除、过期、IP 限制、用户禁用和分组有效性检查沿用网关认证。
  额度耗尽的 Key 同样按现有 TokenAuth 规则拒绝；素材管理本身不扣视频额度。
- 同用户所有 Key 共用素材限流（当前每分钟 60 次），上传另受上传限流约束。
- 创建素材组时 API Key 必须传 `model`，模型必须在 Key 的允许范围内，且在其
  可用分组中有启用的 Doubao 渠道。后台创建组不传模型时保留原有默认选择行为。
- `model` 只参与建组时的上游渠道选择，不是素材的独占模型标签。组和素材的本地
  记录及 OSS 原件是素材库的权威数据；创建时使用的渠道只代表当时首次导入的账号。
- 生成请求引用 `asset://<AssetId>` 时，网关先按常规规则完成令牌、分组、模型和渠道
  选择，再按最终选中的渠道密钥解析当前用户的本地素材，并将上游请求中的媒体引用
  替换为该账号对应的素材 ID。素材不会改变渠道选择，也不能绕过令牌的渠道权限。
- AIGC 素材可跨不同 BaseURL、不同密钥的上游渠道使用。若目标账号尚无素材组，
  网关会在该账号创建同名、同类型的组；若尚无素材
  副本，则从 OSS 原件生成新的短期签名地址并导入。目标账号的组、素材 ID 和审核状态
  独立保存；后续调用复用映射。一个请求可以引用来自不同原始账号或素材组的素材。
  用户始终引用上传时返回的 `asset_id`，不需要获取或更换目标上游的副本 ID。
  原生视频接口和 OpenAI 兼容接口的 JSON、multipart 请求均使用最终选中账号的映射，
  渠道重试切换账号时也会更新引用；网关同时更新插件的已解码请求，避免旧 ID 被再次发送。
  目标上游需提供兼容的素材创建、查询接口并开通素材服务；导入仍需通过目标账号的审核。
- 真人认证组 `LivenessFace` 只在完成认证的同一上游账号内复用。跨不同上游账号
  必须在目标账号完成真人认证；复制 OSS 图片或普通建组不能转移认证结果。
  网关会明确拒绝这种跨账号复制，不会将真人素材降级为 AIGC 素材。
- 目标账号的组或素材仍在创建/审核时，生成请求返回 HTTP 409 和错误码
  `private_asset_mapping_pending`，并返回 `Retry-After: 5`；请求不会转发生成。调用方应
  至少等待该间隔，并以带随机抖动的有上限指数退避重试；后台轮询继续同步状态。
  无效、不存在、已删除或不属于当前用户的引用返回 HTTP 400。上游导入失败返回映射错误，
  不会把未完成的映射当作成功。
- 渠道新增或轮换密钥后会按新的上游账号命名空间建立映射；停用旧渠道不影响新渠道
  从 OSS 原件重新导入。相同上游地址、素材 API 路径和密钥的渠道可以复用映射。外部
  URL 登记的素材没有 OSS 原件，跨账号重导入依赖原 URL 仍可访问。
- 生成仍调用现有视频接口，并执行现有生成权限和计费校验。同步完成前网关不会把
  `asset://` 发送到上游，也不会因本地或目标账号显示 `Active` 就保证生成任务受理。

同一上游账号下，不同 Seedance 2.0 模型分组的 API 令牌共享素材库；不同上游账号
相互隔离。网关渠道编号或 Key 指纹不同，不能据此证明属于不同上游账号。

## 接口

| 方法 | 路径（加 `/v1/seedance` 前缀） | 请求 |
| --- | --- | --- |
| GET | `/asset-groups` | 当前用户的组，支持筛选与可选分页 |
| GET | `/asset-groups/:id` | 当前用户的单个组，本地数字 ID |
| POST | `/asset-groups` | JSON：`name`、`model` |
| PUT | `/asset-groups/:id` | JSON：`name`；可选本地备注 `description`、`tags` |
| DELETE | `/asset-groups/:id` | 删除组及其素材 |
| POST | `/validation-sessions` | JSON：`name`、`description`、`tags`；创建真人认证会话并返回 H5 地址和本地会话编号 |
| GET | `/validation-sessions/:id` | 查询真人认证会话并同步认证结果 |
| GET | `/assets` | `group_id`、`p`、`page_size`、`search`、`asset_type`、`status` |
| GET | `/assets/:id` | 当前用户的单个素材，本地数字 ID |
| POST | `/assets` | JSON：`group_id`、`source_url`、`asset_type`、`name` |
| POST | `/assets/upload` | multipart：`group_id`、`file`；可选 `name`、`asset_type` |
| POST | `/assets/:id/refresh` | 查询上游状态并更新本地 |
| DELETE | `/assets/:id` | 删除单个素材 |
| POST | `/assets/batch-delete` | JSON：`ids`，最多 100 个本地素材 ID |

路径 `:id` 和批量 `ids` 使用响应中的本地数字 `id`；上传字段 `group_id` 使用
上游字符串组 ID。生成引用使用 `asset://` 加上响应的 `asset_id`。
`asset_type` 支持 `Image`、`Video`、`Audio`；`status` 筛选支持 `all`、
`Processing`、`Active`、`Failed`、`Deleting`。

响应沿用素材管理契约：必须检查 `success`，不能只检查 HTTP 200。
上传/URL 登记成功返回 HTTP 202、`success: true` 和 `data` 素材对象，初始通常为
`Processing`。列表包含 `data` 数组、`total`、`page`、`page_size`、`has_pending`。
批量删除返回 `deleted_ids`、`failed_ids`、`pending_ids`，逐项处理部分失败。
预览地址 `preview_url` 是临时签名地址，过期后重新查询；不是永久公开地址。

## 调用示例

### 查询参数

以下能力由网关查询本地授权映射提供，不是上游 Action 接口的原样透传。
普通建组接口固定创建 `AIGC` 组，不接受 `GroupType` 覆盖参数；显式传入该字段会返回
参数错误。`LivenessFace` 真人组必须完成真人认证会话，认证结果返回的 `GroupId` 会在
网关内自动落成本地素材组，不能通过普通建组请求创建。
真人认证会话有效期为 30 分钟，会按当前请求的模型、分组、令牌限制和渠道约束生成候选渠道，令牌在主库内
以密文保存，网页和自定义 `/v1/seedance` 接口只返回本地会话状态，不返回 `BytedToken`。
如果候选渠道明确返回认证 Action 不支持、未实现或 Action 不存在，网关会在同一个本地会话
内回退到下一个候选渠道；鉴权失败、参数错误、网络超时和其他上游业务错误不会盲目换渠道。
上游拒绝时，响应中的上游错误信息会作为接口失败原因返回。

| 参数 | 素材列表 | 素材组列表 |
| --- | --- | --- |
| `group_id` / `group_ids` | 单组 / 多组筛选 | 按字符串组 ID 精确筛选 |
| `ids` | 批量本地数字素材 ID | 批量本地数字组 ID |
| `asset_id` / `asset_ids` | 单个 / 多个上游素材 ID | 不支持 |
| `search` | 名称或素材 ID 模糊搜索 | 名称或组 ID 模糊搜索 |
| `status` / `statuses` | `active`、`processing`、`failed`、`deleting`、`all` | `active`、`deleting`、`all` |
| `asset_type` / `asset_types` | `image`、`video`、`audio`、`all` | 不支持 |
| `created_after` | 创建时间大于等于此时间 | 同左 |
| `created_before` | 创建时间严格小于此时间 | 同左 |
| `sort_by` | `id`（默认）、`created_at`、`updated_at`、`name` | 同左 |
| `sort_order` | `asc` / `desc`（默认） | 同左 |
| `p` / `page_size` | 页码默认 1，每页默认 10、最多 100 | 显式传入任一分页参数启用分页；未传时保留全量数组 |

批量参数支持逗号分隔或重复键，例如 `statuses=active,processing` 或
`statuses=active&statuses=processing`。每个批量参数最多 100 个元素（去重前），
空元素不允许；对应的单复数参数不能同时传入。相同字段多值取并集，不同字段取交集。
`all` 不能与其他状态或类型组合。状态、类型和排序方向不区分大小写。
ID 最多 128 字符，搜索最多 128 字符；`%`、`_`、`#` 按普通文本搜索。

时间使用带时区的 RFC3339 格式，如 `2026-09-20T00:00:00Z`，统一转换为 UTC，
范围采用 `[created_after, created_before)`。这里是本地记录创建/更新时间，不是上游时间。
相同排序值按本地 ID 同方向排序，防止并列记录分页不稳定；并发插入/删除下页码分页不保证快照一致。
页码超出末页时沿用旧行为返回末页；空结果返回第一页。`ps`、`size` 分页别名继续支持。
格式错误、非法枚举、超长参数、单复数冲突及非正整数分页参数返回 HTTP 400。
每页数量超过 100 时截断为 100。

组列表分页时响应保留 `data` 数组并增加 `total`、`page`、`page_size`；未启用分页时
保持原响应结构。素材列表 `has_pending` 只考虑当前用户及选定组，忽略其他筛选条件，
避免筛选终态后客户端停止跟踪仍在处理的素材。

单条详情使用 GET，返回 `success/message/data`；不属于当前用户和不存在的 ID
统一返回 HTTP 404，不泄露其他用户的资源。详情不会调用上游刷新；预览签名失败时
仍返回素材记录，但 `preview_url` 为空或省略。需同步上游状态时调用已有刷新接口。

```bash
curl --get "$BASE_URL/v1/seedance/assets" \
  -H "Authorization: Bearer $API_KEY" \
  --data-urlencode 'group_ids=group-a,group-b' \
  --data-urlencode 'statuses=active,processing' \
  --data-urlencode 'asset_types=image,video' \
  --data-urlencode 'sort_by=created_at' \
  --data-urlencode 'sort_order=desc' \
  --data-urlencode 'p=1' --data-urlencode 'page_size=24'
```

### 上传和生成前查询

以下 curl 示例使用调用方自己的环境变量，勿将真实 Key 写入脚本仓库。

```bash
curl "$BASE_URL/v1/seedance/asset-groups" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"视频素材","model":"doubao-seedance-2-0-fast-260128"}'

curl "$BASE_URL/v1/seedance/assets/upload" \
  -H "Authorization: Bearer $API_KEY" \
  -F "group_id=$GROUP_ID" \
  -F 'file=@image.jpg'

curl "$BASE_URL/v1/seedance/assets?group_id=$GROUP_ID&p=1&page_size=24" \
  -H "Authorization: Bearer $API_KEY"

curl "$BASE_URL/v1/seedance/assets/$LOCAL_ASSET_ID/refresh" \
  -X POST -H "Authorization: Bearer $API_KEY"
```

### 真人认证素材组

创建会话时 `model` 可选；API Key 调用建议明确传入模型，以便沿用令牌的模型、分组和
渠道约束。未传 `callback_url` 时，网关使用系统设置中的 `TaskPublicAddress`，否则回退
到 `ServerAddress`，生成一次性能力地址作为上游回跳地址。

```bash
curl "$BASE_URL/v1/seedance/validation-sessions" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"真人素材组","description":"用于已认证人物肖像","tags":"演员, 已认证","model":"doubao-seedance-2-0-fast-260128"}'

curl "$BASE_URL/v1/seedance/validation-sessions/$SESSION_ID" \
  -H "Authorization: Bearer $API_KEY"
```

第一次响应中的 `data.launch_url` 是认证页面地址。认证完成后继续查询本地会话；只有
`status` 为 `Succeeded` 且返回 `group_id` 后，才上传对应真人的肖像并等待上游审核。组名、说明和标签会保存在本地组信息中。认证未完成、
上游暂时不可查询或渠道正在切换时，会话保持 `Pending` 并保存有限错误摘要，不会把未认证
素材组显示为可用。会话有效期为 24 小时，过期后需要重新创建。
认证会话依赖固定的 `CRYPTO_SECRET`；未设置时使用固定的 `SESSION_SECRET`。至少配置其中一个并在实例重启及多节点间保持不变，否则会话令牌无法解密，认证回调也无法关联原会话。

网关同时提供官方 Action 兼容入口：

```bash
curl "$BASE_URL/doubao/v2/assets?Action=CreateVisualValidateSession&Version=2024-01-01" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"CallbackURL":"https://example.com/validation/callback","ProjectName":"default"}'

curl "$BASE_URL/doubao/v2/assets?Action=GetVisualValidateResult&Version=2024-01-01" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"BytedToken":"<创建会话返回的令牌>","ProjectName":"default"}'
```

官方创建接口要求 `CallbackURL`，`ProjectName` 可选；查询接口的 `ProjectName` 也可选，
网关会优先使用创建会话时保存的项目。官方入口返回与上游一致的 `ResponseMetadata` 和
`Result` 外形；`BytedToken` 只会在官方创建接口响应中返回，查询时必须属于当前 API Key
用户创建的会话，不能跨用户复用。

插件 Action 兼容入口位于 `/doubao/v2/assets`，与 Doubao 插件路由保持同一命名空间。
若渠道 BaseURL 的路径以 `/seedance` 结尾，上游素材 Action 直接向 BaseURL 本身发送，Action 与
Version 放在查询参数中；其他渠道 BaseURL 使用官方 `/v2/assets` 路径。网关不会自动添加
`/seedance`，由管理员配置渠道 BaseURL 决定上游命名空间。
真人认证会话
必须由实际支持 `CreateVisualValidateSession` 和 `GetVisualValidateResult` 的上游账号处理；
候选渠道中只有部分支持时，会按上述规则自动回退到支持的渠道。
普通建组接口固定创建 `AIGC` 组，真人组只能由认证会话成功后创建；如果目标上游账号
不允许重建真人组，素材映射会返回上游错误，不会把真人组降级为 `AIGC` 组。

同用户换用另一把 API Key 查询时，可直接得到相同的组和素材。通常每 5 秒读取
列表中的状态即可；后台负责上游审核轮询，不需要反复调用单个素材刷新接口。
`has_pending: false` 后可以停止轮询。上传及生成发生网络超时时不要盲目重发，
先查询已有资源或任务，避免重复创建。

安全边界参考 OWASP ASVS 5.0 的认证、会话及授权原则，以及
[Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
和 [Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)：
凭证放 Header、正式部署使用 HTTPS、逐资源检查用户归属、不赋予后台管理员权限、
复用现有认证及限流。上述说明不是对整个系统的 ASVS 合规认证。
