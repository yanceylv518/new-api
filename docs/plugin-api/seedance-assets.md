# 私域素材库 API Key 接口

使用 `Authorization: Bearer sk-...` 调用 `/v1/seedance`。这是网关提供的用户级
素材管理 API，不是上游厂商 Action 接口的原样透传。
后台 `/api/user/seedance` 接口继续使用后台凭证；两者读取相同的用户数据。

客户兼容素材接口 `/doubao/api/assets/...`、素材 Action 入口
`/doubao/api/ark/assets` 和视频接口 `/doubao/api/v3/contents/generations/tasks`
及其兼容别名也接受本站签发的 AK/SK 请求签名。
原有 Bearer API Key 方式继续可用；签名凭据绑定一把本站 API Key，并使用该 Key 的
用户、分组、IP 白名单、模型权限、额度及计费规则。

使用该 API Key 的 Bearer 认证管理签名凭据：

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `GET` | `/api/token/aksk?token_id=<ID>` | 查询状态和 AK，不返回 SK |
| `POST` | `/api/token/aksk` | 签发或重签；JSON 可传 `token_id`，SK 只在此次响应中返回 |
| `DELETE` | `/api/token/aksk?token_id=<ID>` | 吊销；重复吊销仍成功 |

管理请求只能操作当前 Bearer API Key 自身，`token_id` 省略时使用当前 Key。签名
采用 `HMAC-SHA256 Credential=<AK>/<日期>/<region>/<service>/request, SignedHeaders=<头列表>, Signature=<签名>`。
请求必须有 UTC 格式 `X-Date: YYYYMMDDTHHMMSSZ`，允许与服务器时间相差 15 分钟；
`host` 和 `x-date` 必须包含在 `SignedHeaders` 中。可选的 `X-Content-Sha256` 必须
与原始请求体的 SHA-256 一致。签名覆盖原始 HTTP 方法、路径、查询串、声明的请求头和
原始正文；请在客户端最终确定 Base URL、文件内容和请求头后再签名。

签发示例（`API_KEY` 是当前用户的本站 API Key）：

```bash
curl --request POST "$BASE_URL/api/token/aksk" \
  --header "Authorization: Bearer $API_KEY" \
  --header 'Content-Type: application/json' \
  --data '{}'
```

响应 `data.access_key` 为 AK，`data.secret_key` 为 SK；请在签发时保存 SK。
客户端可使用火山 V4 签名实现：规范请求依次包含 HTTP 方法、按 RFC 3986 编码的
路径、按键排序并以 `%20` 表示空格的查询串、签名头、头名列表和正文 SHA-256；
按日期、region、service 和 `request` 依次派生 HMAC 密钥。相同查询键的多个值
保持原有顺序；签名头使用小写名称和去掉首尾空格的值。

SK 以加密形式保存在本站，只在签发响应中返回；重签会立即使旧 AK/SK 失效，
吊销或删除绑定的 API Key 后也不能再使用。多节点需配置一致且持久的
`CRYPTO_SECRET` 或 `SESSION_SECRET`。签名应通过 HTTPS 传输；15 分钟时效限制并不能
防止窗口内重放，客户端应保护签名并按业务需要处理重复提交。

旧平台或火山账号的 AK/SK 不能直接用于本站；本站签发的凭据也不是对象存储凭据。
`/v1/seedance` 和 `/doubao/v2/assets` 继续使用 Bearer 认证。

## 客户兼容素材接口

`POST /doubao/api/ark/assets?Action=<Action>` 使用下述素材管理和真人认证 Action。
等价入口为 `/doubao/v1/ark/asset`、`/doubao/api/v3/asset`、`/doubao` 和 `/doubao/`。
这些入口支持 Bearer 和本站签发的 AK/SK；`Version` 可省略，默认 `2024-01-01`。
素材组和素材操作的 `ProjectName` 仅支持缺省或 `default`；真人认证会话仍使用
自身的项目名校验。兼容入口的 `CreateAsset` 可省略 `AssetType`，网关会按 URL
扩展名或资源内容识别媒体类型；无法识别时返回错误。`/doubao/v2/assets` 仍需
显式提供 `AssetType`。

兼容入口的素材和认证响应按对应接口示例排列字段。认证会话未指定 `model` 时，
`suggest_model` 返回实际选中渠道的模型。认证结果尚未就绪时，浏览器等待页会继续
查询，结果确认后再跳转到可选的 `callback_url`。

列表可使用 `MaxResults` 和 `NextToken`，默认每页 10 项，最多 100 项；返回的
`NextToken` 用于下一页。也可同时提供 `PageNumber` 和 `PageSize` 使用页码分页。
素材和组删除后立即不可使用，客户兼容入口返回 HTTP 200；上游资源和对象存储
由后台继续清理。`/doubao/v2/assets` 保持 HTTP 202 的删除响应。

使用 `GET /doubao/api/assets/<素材 ID>/public-url` 签发公开下载链接，返回的
`/doubao/api/a/<素材 ID>?e=...&s=...` 链接有效期为两小时，下载时无需 API Key。
此前签发的 `/doubao/api/assets/public/...` 链接仍在原有效期内可用。反向代理的
访问日志不得记录下载链接的查询串或 Referer。

## 素材管理 Action 兼容入口

也可通过 `POST /doubao/v2/assets?Action=<Action>&Version=2024-01-01` 管理
同一素材库。鉴权仍使用平台的 `Authorization: Bearer sk-...`，不接受火山 IAM
AK/SK 签名。这是官方风格的协议适配，不是整个上游账号的素材接口透传。

| Action | JSON 请求主要字段 |
| --- | --- |
| `CreateAssetGroup` | 必填 `Name`；可选 `Description`、`GroupType: "AIGC"`、扩展字段 `model` |
| `ListAssetGroups` | `PageNumber`、`PageSize`、`Filter.Name`、`Filter.GroupType`、`SortBy`、`SortOrder` |
| `GetAssetGroup` | `Id`，公开字符串组 ID |
| `UpdateAssetGroup` | `Id`；至少提供 `Name`、`Description` 之一 |
| `DeleteAssetGroup` | `Id` |
| `CreateAsset` | `GroupId`、`URL`、`AssetType`；可选 `Name` |
| `ListAssets` | `PageNumber`、`PageSize`、`Filter.GroupId`、`Filter.Name`、`Filter.AssetType`、`Filter.Statuses`、`SortBy`、`SortOrder` |
| `GetAsset` | `Id`，公开字符串素材 ID |
| `UpdateAsset` | `Id`、`Name` |
| `DeleteAsset` | `Id` |

`CreateVisualValidateSession`、`GetVisualValidateResult` 仍使用该入口，调用方式见下文。
普通建组只接受缺省或 `AIGC` 类型；`LivenessFace` 必须完成真人认证，不能绕过认证建组。
Action 建组未传 `model` 时，在当前 Key 的模型、分组及渠道权限内选择一个豆包渠道；
传入 `model` 可明确选择建组模型。原 REST 建组接口仍要求 API Key 调用方提供 `model`。

请求体为 JSON 对象，字段区分大小写。未知字段、错误类型和显式 `null` 会被拒绝，
不接受指定用户、渠道或替换已有素材的 URL、类型、所属组。`Name` 为 1–64 字符，
组 `Description` 最多 256 字符。`CreateAsset` 的 `URL` 必须为可访问的公网地址，
沿用网关的 SSRF 检查；本地文件继续通过 `/v1/seedance/assets/upload` 上传。
组描述和素材名称是平台素材库的元数据，不会批量修改既有跨账号副本的展示信息。
外部 URL 登记会先保存原件到 OSS，后续跨账号导入使用 OSS 原件。

分页默认第 1 页、每页 10 项，每页最多 100 项；页码上限 21474836。超出末页返回
空 `Items`，不会自动回到末页。`SortBy` 支持 `CreateTime`、`UpdateTime`、`Name`，
默认 `CreateTime`；`SortOrder` 为 `Asc` 或 `Desc`，默认 `Desc`。时间相同时按本地
ID 保持稳定顺序。`Filter.Name` 只匹配名称，`%`、`_`、`#` 作为普通字符。
组列表默认筛选 `AIGC`，可通过 `Filter.GroupType: "LivenessFace"` 查询已认证组。
`Filter.Statuses` 支持 `Processing`、`Active`、`Failed` 及平台扩展的 `Deleting`，
同一字段多值取并集，不同筛选条件取交集。

成功响应使用 `ResponseMetadata` 与 `Result`。创建和更新返回 `Result.Id`；
列表返回 `Result.TotalCount`、`PageNumber`、`PageSize`、`Items`。组详情返回
`Id`、`Name`、`Description`、`GroupType`、`Status`、`CreateTime`、`UpdateTime`；
素材详情返回 `Id`、`Name`、`GroupId`、`AssetType`、`Status`、`URL`、`CreateTime`、
`UpdateTime`。时间使用 UTC RFC3339，素材 `URL` 仅在 `Active` 时返回。
没有可靠保存的上游项目和审核详情不伪造为 `ProjectName` 或 `Moderation`。

创建素材返回 HTTP 200 只表示已登记，后台继续审核，需查询至 `Active` 后才能引用。
`GetAsset` 和列表读取本地同步状态，不为每次查询额外发起上游请求；预览链接按需签名。
删除返回 HTTP 202 和 `Result.Status: "Deleting"`，表示删除已持久化受理；
`DeleteAssetGroup` 还返回 `Result.Id`。后台清理本站副本映射及 OSS 原件，最终移除记录；
上游删除不阻塞本站清理，也不保证成功。配置素材共享组的渠道无法仅凭共享组确认创建账号，
删除时不会向这些渠道发起上游删除。不能把 HTTP 202 当作所有上游资源已经删除。错误使用 `ResponseMetadata.Error`，
其他用户和不存在的资源均返回 HTTP 404、`ResourceNotFound`。
认证和限流在业务入口前执行，失败仍沿用平台中间件的响应格式。

```bash
curl "$BASE_URL/doubao/v2/assets?Action=CreateAssetGroup&Version=2024-01-01" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"Name":"参考素材","GroupType":"AIGC","model":"doubao-seedance-2-0-fast-260128"}'

curl "$BASE_URL/doubao/v2/assets?Action=CreateAsset&Version=2024-01-01" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"GroupId":"<组 ID>","URL":"https://example.com/reference.jpg","AssetType":"Image"}'

curl "$BASE_URL/doubao/v2/assets?Action=ListAssets&Version=2024-01-01" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"Filter":{"Statuses":["Active"],"AssetType":"Image"},"PageNumber":1,"PageSize":10}'
```

## 归属、权限与计费

- 同一用户可创建同名素材组，使用各自的 `group_id` 区分。
- 素材组和素材归属于 `user_id`，不绑定创建它的 API Key。同用户所有有效 Key
  可以列出、上传、刷新、修改和删除该用户的素材，包含后台已创建的历史数据。
- 不接受调用方指定用户归属；其他用户即使知道素材或组 ID，也不能访问对应资源。
- API Key 的禁用、删除、过期、IP 限制、用户禁用和分组有效性检查沿用网关认证。
  额度耗尽的 Key 同样按现有 TokenAuth 规则拒绝；素材管理本身不扣视频额度。
- 同用户所有 Key 共用素材限流，默认每分钟 60 次；上传和公开下载还分别按 IP 限流，默认各为每分钟 10 次。管理员可在系统设置中修改。
- 素材单文件大小、每账号素材数量和 OSS 原件容量由管理员在系统设置中配置；上传还受渠道及反向代理限制。
  客户兼容下载使用对应类型的当前文件上限，并保留原有 200 MiB 下限；已上传的 OSS 原件按保存的文件大小允许下载，不受后续下调上传上限影响。
- API Key 调用 `/v1/seedance/asset-groups` 创建素材组时必须传 `model`；Action
  兼容入口和后台可省略。指定模型时，它必须在 Key 的允许范围内，且其可用分组中有启用的渠道。
- `model` 只参与建组时的上游渠道选择，不是素材的独占模型标签。组和素材的本地
  记录及 OSS 原件是素材库的权威数据；创建时使用的渠道只代表当时首次导入的账号。
- 生成请求引用 `asset://<AssetId>` 时，网关在令牌、分组和模型允许的范围内筛选
  素材已就绪的渠道，再将媒体引用映射到该渠道的素材 ID。真人素材还限制在其认证
  共享范围内；两者都不能绕过令牌的渠道权限。
- 上传文件或登记公网地址后，AIGC 素材会主动同步到用户当前可用分组中启用的素材渠道。
  本地素材与同步目标在同一事务内保存，独立后台任务执行导入，不等待首次视频调用；
  有积压时完成一批后立即继续处理下一批，同时保持上游并发限制和失败退避；
  重启后会继续处理未完成的同步。未配置素材共享组的渠道按模型、渠道和密钥分别同步；
  配置了素材共享组的渠道继续按共享范围复用。暂时性失败先指数退避重试；快速重试 6 次后，
  每 15 分钟低频重试并附加抖动，不需要生成请求唤醒。用户或目标渠道暂不可用时同样
  低频复核权限，恢复后继续同步；明确的永久业务失败停止自动导入并保留原因。
  每次执行前重新检查用户分组权限；仅本地链接模式不向上游导入，真人素材不跨认证范围复制。
  主动同步尚未完成时，生成请求返回 `private_asset_mapping_pending`，不会接管后台慢导入。
- AIGC 素材只有在当前凭证可用的每个 Seedance 模型都至少有一个渠道的素材审核通过后，
  才显示 `Active`。模型数量按当前权限动态确定，每个模型不需要等待其全部渠道。
  网页按用户可用分组判断；API 按令牌分组和模型权限判断，因此不同凭证看到的状态可能不同。
  未配置共享组时，即使素材地址和密钥相同，不同模型或渠道也分别导入；明确配置了相同共享范围时，
  一个审核通过的副本可覆盖该范围内的多个模型。
  素材接口不接收模型参数，该状态依据对应渠道返回的素材审核结果，不代表已验证视频生成。
- 目标素材自同步记录创建起超过 15 分钟且至少经过 6 次状态检查仍未就绪时，本站将该目标标记为审核超时。
  `failure_code: review_timeout` 表示本站等待超时，不代表上游审核拒绝；后台每 15 分钟继续检查，
  上游审核通过后自动恢复，用户也可以手动刷新。其他已就绪的目标仍可使用。
- AIGC 素材可跨不同 BaseURL、不同密钥的上游渠道使用。若目标账号尚无素材组，
  网关会在该账号创建同名、同类型的组；若尚无素材
  副本，则从 OSS 原件生成新的短期签名地址并导入。目标账号的组、素材 ID 和审核状态
  独立保存；后续调用复用映射。一个请求可以引用来自不同原始账号或素材组的素材。
  用户始终引用上传时返回的 `asset_id`，不需要获取或更换目标上游的副本 ID。
- 删除素材时，网关在同一事务内锁定组和素材、读取全部当前副本并建立清理任务。
  已完成导入的副本使用锁内读取的上游 ID；删除开始后不能新建副本映射，尚在导入的
  远端素材通过既有补偿流程清理。上游删除失败仍不阻塞本站删除。
  原生视频接口和 OpenAI 兼容接口的 JSON、multipart 请求均使用最终选中账号的映射，
  渠道重试切换账号时也会更新引用；网关同时更新插件的已解码请求，避免旧 ID 被再次发送。
  目标上游需提供兼容的素材创建、查询接口并开通素材服务；导入仍需通过目标账号的审核。
- 管理员可在支持私域素材的渠道中配置 `seedance_asset_share_group`。仅素材服务地址、
  素材接口协议和共享组名称均相同的渠道被视为同一共享范围；组名区分大小写，最多
  64 个字符。未配置共享组时，AIGC 素材按模型、渠道和上游密钥分别同步。
  应只为已确认可以共用同一素材库的渠道填写相同组名；共享组不会扩大用户的模型、
  分组或渠道权限。配置或更改共享组不会迁移既有素材，既有绑定不自动转换。
  同一共享范围的不同密钥直接复用组和素材 ID，不需要重新导入。
- 真人认证组 `LivenessFace` 只在其共享范围内复用；未配置共享组时按上游密钥区分。
  复制 OSS 图片或普通建组不能转移认证结果，网关不会将真人素材降级为 AIGC 素材。
- 目标账号的组或素材仍在创建/审核时，生成请求返回 HTTP 409 和错误码
  `private_asset_mapping_pending`，并返回 `Retry-After: 5`；请求不会转发生成。调用方应
  至少等待该间隔，并以带随机抖动的有上限指数退避重试；后台轮询继续同步状态。
  无效、不存在、已删除或不属于当前用户的引用返回 HTTP 400。上游导入失败返回映射错误，
  不会把未完成的映射当作成功。
- 未配置共享组时，渠道新增或轮换密钥会建立新的模型映射；停用旧渠道不影响新渠道从
  OSS 原件重新导入。配置共享组后，同一共享范围内轮换密钥可继续使用该范围内的组和素材。
  公网 URL 登记的素材也会先保存至 OSS，供后续同步使用。
- 生成仍调用现有视频接口，并执行现有生成权限和计费校验。同步完成前网关不会把
  `asset://` 发送到上游，也不会因本地或目标账号显示 `Active` 就保证生成任务受理。

上游账号与项目的素材权限由各上游服务决定；管理员声明共享范围前应先确认相关密钥
可以共享素材库。网关渠道编号或 Key 不同，不能据此证明上游素材一定隔离。

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
认证完成前查询结果固定使用创建会话时的上游密钥；认证成功后才按素材共享范围复用。
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
渠道约束。网关使用系统设置中的 `TaskPublicAddress`，否则回退到 `ServerAddress`，
生成一次性能力地址作为上游回跳地址。传入 `callback_url` 时，网关先同步认证状态，
再携带认证参数跳转到该地址；公共地址必须能从认证页面访问。
回跳必须包含唯一且有效的 `state`；渠道未回传 `bytedToken` 时仍可处理，
提供该字段时必须与创建会话时的令牌匹配。认证结果始终通过绑定渠道查询确认，
不采用回跳中未经确认的 `GroupId` 或成功状态。自定义回调收到的 `session_id`、
`status` 和成功时的 `GroupId` 使用本站确认后的值；状态仍待确认时返回 503，
客户端可继续查询会话，不能将浏览器回跳参数视为素材组已就绪。
上游认证查询成功返回组 ID 后，网关会核对组 ID 和 `LivenessFace` 类型。原生
`group-...` 真人组的详情允许省略 `Status`；包装协议提供的状态仍须为 `Active`，
空值和非法类型不会视为认证成功。若组仍在处理，后续查询直接检查该组，避免重复
使用一次性认证令牌；明确的组响应结构错误会将会话标记为 `Failed`。
认证结果的组 ID 会先保存，再发布本地素材组和会话成功状态；发布操作不随客户端断开
取消，独立超时为 10 秒。若收尾超时，后续查询复用已保存的组 ID。

```bash
curl "$BASE_URL/v1/seedance/validation-sessions" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"真人素材组","description":"用于已认证人物肖像","tags":"演员, 已认证","model":"doubao-seedance-2-0-fast-260128"}'

curl "$BASE_URL/v1/seedance/validation-sessions/$SESSION_ID" \
  -H "Authorization: Bearer $API_KEY"
```

第一次响应中的 `data.launch_url` 是认证页面地址。认证完成后可继续查询本地会话；后台会对
等待认证或暂时无法查询的会话退避重试，使用自定义 `callback_url` 时无需依赖客户主动查询才能创建本站素材组。只有
`status` 为 `Succeeded` 且返回 `group_id` 后，才上传对应真人的肖像并等待上游审核。组名、说明和标签会保存在本地组信息中。认证未完成、
上游暂时不可查询或渠道正在切换时，会话保持 `Pending` 并保存有限错误摘要，不会把未认证
素材组显示为可用。会话有效期为 30 分钟，过期后需要重新创建。
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

素材 Action 兼容入口位于 `/doubao/v2/assets`，由网关后端提供，与 Doubao 插件路由保持同一命名空间。
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
