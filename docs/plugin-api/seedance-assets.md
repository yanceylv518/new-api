# 私域素材库 API Key 接口

使用 `Authorization: Bearer sk-...` 调用 `/v1/seedance`。这是网关提供的用户级
素材管理 API，不是书言 `/seedance?Action=...` 的原样透传接口。
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
- `model` 只参与建组时的上游渠道选择，不是素材的独占模型标签。已有组的上游
  渠道和凭证指纹继续用于上传、查询、修改、删除，不随调用方轮换 Key 而改变。
- 生成仍调用现有视频接口，并执行现有生成权限和计费校验。不会把 `asset://`
  自动改为普通 OSS URL，也不会因本地显示 `Active` 就承诺上游任务必然受理。

书言[素材库文档](https://doc.shuyanai.com/doc-9030011)明确说明：同一上游账号
不同 Seedance 2.0 模型分组的 API 令牌共享素材库；不同上游账号相互隔离。
网关渠道编号或 Key 指纹不同，不能据此证明属于不同书言账号。

## 接口

| 方法 | 路径（加 `/v1/seedance` 前缀） | 请求 |
| --- | --- | --- |
| GET | `/asset-groups` | 当前用户的组，支持筛选与可选分页 |
| GET | `/asset-groups/:id` | 当前用户的单个组，本地数字 ID |
| POST | `/asset-groups` | JSON：`name`、`model` |
| PUT | `/asset-groups/:id` | JSON：`name` |
| DELETE | `/asset-groups/:id` | 删除组及其素材 |
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

以下能力由网关查询本地授权映射提供，不是书言 Action 接口的原样透传。
当前书言素材组为 AIGC，不包含火山直连的 H5 真人认证流程。

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

同用户换用另一把 API Key 查询时，可直接得到相同的组和素材。通常每 5 秒读取
列表中的状态即可；后台负责上游审核轮询，不需要反复调用单个素材刷新接口。
`has_pending: false` 后可以停止轮询。上传及生成发生网络超时时不要盲目重发，
先查询已有资源或任务，避免重复创建。

安全边界参考 OWASP ASVS 5.0 的认证、会话及授权原则，以及
[Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
和 [Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)：
凭证放 Header、正式部署使用 HTTPS、逐资源检查用户归属、不赋予后台管理员权限、
复用现有认证及限流。上述说明不是对整个系统的 ASVS 合规认证。
