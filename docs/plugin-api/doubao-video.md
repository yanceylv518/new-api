# Doubao Video 自定义接口文档

本文档对应内置 Doubao Video 插件 **1.1.4** 的自定义路由。文档只描述
插件声明的 `/doubao/api/v3/...` 接口，不包含网关的 OpenAI 兼容接口、
通用任务接口或宿主素材接口。

接口调用方连接的是 New API 网关。`$BASE_URL` 是网关地址，例如
`https://video.example.com`；Token 是网关用户 API Token，不是火山引擎
上游渠道密钥。

```bash
export BASE_URL="https://video.example.com"
export API_KEY="sk-your-new-api-token"
```

管理员需要先启用 Doubao Video 插件、配置渠道和模型，并为用户分组开放
对应模型。请求中的媒体引用必须是上游可访问的 URL；这组自定义接口不接收
本地文件 multipart 上传。

## 接口总览

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `POST` | `/doubao/api/v3/contents/generations/tasks` | 创建视频生成任务 |
| `GET` | `/doubao/api/v3/contents/generations/tasks/:task_id` | 查询单个任务 |
| `GET` | `/doubao/api/v3/contents/generations/tasks` | 查询当前用户任务列表 |
| `DELETE` | `/doubao/api/v3/contents/generations/tasks/:task_id` | 取消排队任务或删除上游任务 |

创建接口是异步接口，成功后返回的 `id` 是网关公开任务 ID。后续查询和删除
都使用这个 ID。上游任务 ID只由网关内部保存和使用。

## 通用请求约定

每个请求都应携带：

```http
Authorization: Bearer <网关 API Token>
Content-Type: application/json
```

示例：

```bash
curl --request GET "$BASE_URL/doubao/api/v3/contents/generations/tasks/task_gateway_doubao_01" \
  --header "Authorization: Bearer $API_KEY"
```

任务状态由网关本地轮询上游后保存。刚创建的任务可能暂时处于 `queued`，
需要重复查询，或通过列表接口读取最新快照。

## 创建任务

### 请求

```http
POST /doubao/api/v3/contents/generations/tasks
```

纯文本生成示例：

```bash
curl --request POST "$BASE_URL/doubao/api/v3/contents/generations/tasks" \
  --header "Authorization: Bearer $API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "doubao-seedance-2-0-260128",
    "content": [
      {"type": "text", "text": "一只纸飞机穿过城市上空，电影感镜头"}
    ],
    "resolution": "1080p",
    "duration": 5,
    "service_tier": "default"
  }'
```

参考图和参考视频示例：

```bash
curl --request POST "$BASE_URL/doubao/api/v3/contents/generations/tasks" \
  --header "Authorization: Bearer $API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "doubao-seedance-2-0-fast-260128",
    "content": [
      {"type": "text", "text": "让画面中的人物向镜头走来"},
      {"type": "image_url", "role": "reference_image", "image_url": {"url": "https://cdn.example/reference.png"}},
      {"type": "video_url", "role": "reference_video", "video_url": {"url": "https://cdn.example/motion.mp4"}}
    ],
    "resolution": "720p",
    "duration": 5
  }'
```

请求字段：

| 字段 | 类型 | 必填 | 范围或取值 | 说明 |
| --- | --- | --- | --- | --- |
| `model` | string | 是 | 当前插件声明的模型之一 | 模型名称会参与渠道选择 |
| `content` | array | 是 | 至少一个文本或媒体项 | 插件保留并透传内容项 |
| `resolution` | string | 否 | 由模型能力决定 | `480p`、`720p`、`1080p`、`4k` |
| `duration` | integer | 否 | 2.0 标准版/Fast/Mini：`-1` 或 4 到 15 | `-1` 表示上游智能时长；其他模型以官方文档为准 |
| `frames` | integer | 否 | 2.0 标准版/Fast/Mini 不支持 | 其他模型沿用通用帧数边界和上游校验 |
| `service_tier` | string | 否 | 2.0 标准版/Fast/Mini 不支持 | 其他模型以官方文档为准 |
| `ratio` | string | 否 | `adaptive`、`21:9`、`16:9`、`4:3`、`1:1`、`3:4`、`9:16` | 2.0 系列支持以上取值 |
| `generate_audio` | boolean | 否 | 2.0 系列支持 | `false` 用于请求无声视频；显式布尔值会保留并透传 |
| `priority` | integer | 否 | 0 到 9 | 任务优先级，需使用支持该字段的模型 |
| `camera_fixed` | boolean | 否 | 2.0 标准版/Fast/Mini 不支持 | 在网关校验时拒绝，避免无效预扣 |
| `output_format` | string | 否 | 2.0 标准版/Fast/Mini 只接受 `mp4` | `mov` 会在网关校验时拒绝 |
| `callback_url` | string | 否 | 上游可访问地址 | 作为官方字段透传 |
| `safety_identifier` | string | 否 | 不超过64字符的英文字符串 | 建议使用终端用户标识的哈希，不传入个人信息 |
| 其他官方字段 | 对应官方类型 | 否 | 以渠道和上游接口为准 | 未被插件改写的字段会随请求体透传 |

`content` 中的文本项用于构建上游 `prompt`，媒体项用于识别文生视频、图生
视频或视频生视频。媒体通常使用如下形状：

```json
{
  "type": "image_url",
  "image_url": {"url": "https://cdn.example/image.png"}
}
```

或：

```json
{
  "type": "video_url",
  "video_url": {"url": "https://cdn.example/video.mp4"}
}
```

2.0 标准版/Fast/Mini 的 `content` 中图片如只有一张可以省略 `role`，多张图片必须明确写出
`first_frame`、`last_frame` 或 `reference_image`。首尾帧和参考媒体不能混用；
音频至少需要同时提供一张图片或一个参考视频。`content` 中如果包含
`draft_task`，其 `draft_task.id` 应是网关公开任务 ID；
网关会在发送给上游前将其替换为已验证的上游任务 ID。这样可以安全地使用
已有任务作为视频生视频或参考任务输入。

2.0 标准版/Fast/Mini 不支持 `draft_task` 和 `draft: true`。

`duration: -1` 是智能时长控制值；OpenAI 兼容接口的 `seconds: -1` 具有相同含义。
宿主仅对 Doubao 请求中的这两个控制字段放行 `-1`，计费预估仍使用正数时长，
完成用量仍必须非负。渠道模型别名不会导致该控制值被误判为负计费用量。

### 临时媒体地址与回调

`asset://<AssetId>` 与普通公网媒体 URL 是两种输入方式。有效且上游可访问的
临时预览 URL 可作为普通媒体地址输入，仍须满足模型的素材要求。预览地址可能在
排队或读取前过期，稳定复用素材优先使用 `asset://`；不能把裸 `asset_id` 当 URL。

`callback_url` 用于接收异步状态通知，创建接口不以地址可达性作为受理前提。
返回任务 ID 不表示回调验证成功或通知已送达。客户端必须处理乱序、重复事件，
防止较早状态覆盖运行中或终态，并通过任务查询确认最终结果。

### 分辨率能力

| 模型 | 当前插件声明的可用分辨率 |
| --- | --- |
| `doubao-seedance-2-0-fast-260128` | `480p`、`720p` |
| `doubao-seedance-2-0-mini-260615` | `480p`、`720p` |
| 其他已声明模型 | `480p`、`720p`、`1080p`、`4k`，最终以渠道和上游限制为准 |

Fast 和 Mini 模型不接受 `1080p`、`4k` 或等价的高分辨率尺寸。未传分辨率
时，Fast/Mini 默认按 `720p` 估算；其他模型默认按 `1080p` 估算。

当前插件声明的模型：

```text
doubao-seedance-1-0-pro-250528
doubao-seedance-1-0-lite-t2v
doubao-seedance-1-0-lite-i2v
doubao-seedance-1-5-pro-251215
doubao-seedance-2-0-260128
doubao-seedance-2-0-fast-260128
doubao-seedance-2-0-mini-260615
doubao-seedance-2-5-260628
```

### 成功响应

创建成功只返回任务 ID：

```json
{
  "id": "task_gateway_doubao_01"
}
```

## 查询单个任务

```http
GET /doubao/api/v3/contents/generations/tasks/:task_id
```

```bash
curl --request GET "$BASE_URL/doubao/api/v3/contents/generations/tasks/task_gateway_doubao_01" \
  --header "Authorization: Bearer $API_KEY"
```

成功任务响应是官方任务对象的网关快照，网关公开 ID位于 `id`：

```json
{
  "id": "task_gateway_doubao_01",
  "model": "doubao-seedance-2-0-260128",
  "status": "succeeded",
  "resolution": "1080p",
  "duration": 5,
  "content": {
    "video_url": "https://provider.example/video.mp4",
    "last_frame_url": "https://provider.example/last-frame.png"
  }
}
```

状态值：

| 返回状态 | 含义 |
| --- | --- |
| `queued` | 已提交，等待处理 |
| `running` | 上游正在处理 |
| `succeeded` | 已成功 |
| `failed` | 上游失败 |
| `cancelled` | 已取消 |
| `expired` | 上游任务已过期 |

成功响应中的 `content.video_url` 和 `content.last_frame_url` 是上游快照中的
地址，可能带有有效期。客户端不能假定提供方签名 URL永久有效。

## 查询任务列表

```http
GET /doubao/api/v3/contents/generations/tasks
```

```bash
curl --get "$BASE_URL/doubao/api/v3/contents/generations/tasks" \
  --header "Authorization: Bearer $API_KEY" \
  --data-urlencode "page_num=1" \
  --data-urlencode "page_size=20" \
  --data-urlencode "filter.model=doubao-seedance-2-0-fast-260128" \
  --data-urlencode "filter.status=succeeded" \
  --data-urlencode "filter.service_tier=default"
```

参数：

| 参数 | 类型和范围 | 说明 |
| --- | --- | --- |
| `page_num` | 整数，1 到 500，默认 1 | 页码 |
| `page_size` | 整数，1 到 500，默认 20 | 每页数量 |
| `filter.model` | 单个值，长度最多 191 | 模型过滤，空值表示全部模型 |
| `filter.status` | `queued`、`running`、`succeeded`、`failed`、`cancelled` | 状态过滤 |
| `filter.service_tier` | `default`、`flex` | 服务等级过滤 |
| `filter.task_ids` | 可重复，最多 100 个，每个非空且长度最多 191 | 按网关任务 ID过滤 |

列表只返回当前用户在网关创建的任务，官方最近七天窗口由插件自动设置。
成功响应：

```json
{
  "items": [
    {
      "id": "task_gateway_doubao_01",
      "model": "doubao-seedance-2-0-fast-260128",
      "status": "succeeded",
      "content": {"video_url": "https://provider.example/video.mp4"}
    }
  ],
  "total": 1
}
```

重复任务 ID示例：

```bash
curl --get "$BASE_URL/doubao/api/v3/contents/generations/tasks" \
  --header "Authorization: Bearer $API_KEY" \
  --data-urlencode "filter.task_ids=task_gateway_doubao_01" \
  --data-urlencode "filter.task_ids=task_gateway_doubao_02"
```

## 取消或删除任务

```http
DELETE /doubao/api/v3/contents/generations/tasks/:task_id
```

```bash
curl --request DELETE "$BASE_URL/doubao/api/v3/contents/generations/tasks/task_gateway_doubao_01" \
  --header "Authorization: Bearer $API_KEY"
```

网关会先同步任务最终状态，再执行上游 DELETE：

- 排队任务需要上游确认取消，成功后本地状态为 `cancelled`。
- 已成功或已失败任务执行上游删除，但网关保留本地任务和计费历史。
- 上游空成功响应无法区分取消和删除时，网关会再次查询确认。
- 无法确认最终状态时返回错误，不会把未确认操作当成成功退款。

上游成功删除可返回空响应体或空 JSON 对象；网关保持返回空 JSON 对象：

```json
{}
```

如果上游取消已提交但本地尚未完成确认，接口会返回网关错误并保留任务的
可恢复状态。

## 上游请求映射

以下是插件使用渠道 Base URL调用的上游路径。客户端不应直接调用这些路径，
也不需要接触上游密钥。

| 网关自定义接口 | 上游方法和路径 |
| --- | --- |
| `POST /doubao/api/v3/contents/generations/tasks` | `POST {channel_base_url}/api/v3/contents/generations/tasks` |
| `GET /doubao/api/v3/contents/generations/tasks/:task_id` | `GET {channel_base_url}/api/v3/contents/generations/tasks/{upstream_task_id}` |
| `DELETE /doubao/api/v3/contents/generations/tasks/:task_id` | `DELETE {channel_base_url}/api/v3/contents/generations/tasks/{upstream_task_id}` |

查询列表是网关按当前用户的本地任务记录生成的，不会把共享上游账号的其他
用户任务直接返回给调用方。

## 计费事实

Doubao 任务的默认用量表达式可以读取：

| 字段 | 单位 | 说明 |
| --- | --- | --- |
| `tokens` | token | 提交阶段按时长和分辨率估算，成功后使用上游完成 Token |
| `resolution` | 枚举 | `480p`、`720p`、`1080p`、`4k`，Fast/Mini 只允许前两项 |
| `video_input` | 枚举 | `none` 或 `video`，表示是否包含参考视频 |

示例表达式：

```text
u("resolution") == "720p" && u("video_input") == "video"
  ? tier("reference", u("tokens") * 20 / 1000000)
  : tier("plain", u("tokens") * 40 / 1000000)
```

提交阶段 Token 估算使用分辨率的最大像素尺寸和视频秒数。完成阶段优先使用
`completion_tokens`；当该字段缺失或无效时才回退 `total_tokens`。如果上游
明确返回 `completion_tokens: 0`，零值会被保留，不会被非零总量覆盖。

## 错误响应

Doubao 自定义路由的插件错误通常使用：

```json
{
  "error": {
    "code": "invalid_request",
    "message": "doubao-seedance-2-0-fast-260128 only supports 480p and 720p resolution"
  }
}
```

常见错误：

| HTTP或错误信息 | 原因 |
| --- | --- |
| 400 `model is required` | 缺少模型 |
| 400 `only supports 480p and 720p resolution` | Fast/Mini 使用了 1080p 或 4k |
| 400 `duration must be -1 or an integer between 2 and 30` | 时长不是合法整数或超出范围 |
| 400 `frames must be an integer between 29 and 289...` | 帧数超界或不满足 25+4n |
| 400 `filter.status is invalid` | 列表状态过滤值不受支持 |
| 404 `task_not_found` | 任务不存在、不属于当前用户或不属于当前插件 |
| 502 `task_sync_failed` | 取消/删除前无法同步上游最终状态 |
| 503 `task_cancel_pending` | 上游取消已提交但尚未确认 |
| 503 `task_settlement_unavailable` | 上游删除时最终用量尚未可用，预扣费等待对账 |
