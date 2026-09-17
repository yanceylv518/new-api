# Hailuo Video 自定义接口文档

本文档对应内置 Hailuo Video 插件 **1.3.7** 的自定义路由。文档只描述
插件声明的 `/hailuo/v2/...` 接口，不包含网关的 OpenAI 兼容接口、通用任务
接口或宿主素材接口。

接口调用方连接的是 New API 网关。`$BASE_URL` 是网关地址，例如
`https://video.example.com`；Token 是网关用户 API Token，不是 MiniMax
上游渠道密钥。

```bash
export BASE_URL="https://video.example.com"
export API_KEY="sk-your-new-api-token"
```

管理员需要先启用 Hailuo 插件、配置 Hailuo 渠道和模型，并为用户分组开放
对应模型。当前自定义路由只服务 `MiniMax-H3`。

## 接口总览

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `POST` | `/hailuo/v2/video_generation` | 创建 H3 视频生成任务 |
| `POST` | `/hailuo/v2/h3_context_ir` | 创建 H3-Context-IR 文本任务 |
| `POST` | `/hailuo/v2/video_regeneration` | 创建 H3 视频再生成任务 |
| `GET` | `/hailuo/v2/query/video_generation/:task_id` | 查询单个 H3 任务 |
| `GET` | `/hailuo/v2/query/video_generation` | 查询当前用户的 H3 任务列表 |
| `DELETE` | `/hailuo/v2/video_generation/:task_id` | 取消排队任务或删除上游任务 |

所有创建请求都是异步任务。成功创建后返回的 `task_id` 是网关公开任务
ID，查询、删除、再生成和管理操作都使用这个 ID。插件不会把上游任务 ID暴露给
客户端，也不会把客户端传入的公开 ID直接发送给上游。

## 通用请求约定

每个请求都应携带：

```http
Authorization: Bearer <网关 API Token>
Content-Type: application/json
```

完整示例：

```bash
curl --request GET "$BASE_URL/hailuo/v2/query/video_generation/task_gateway_h3_01" \
  --header "Authorization: Bearer $API_KEY"
```

任务状态由网关本地轮询上游后保存。刚创建的任务可能暂时处于 `queued`，
需要重复查询，或使用任务列表读取最新保存状态。

## H3 视频生成

### 请求

```http
POST /hailuo/v2/video_generation
```

请求体：

```json
{
  "model": "MiniMax-H3",
  "content": [
    {
      "type": "text",
      "text": "一座灯塔矗立在雾海中，镜头缓慢向前推进"
    }
  ],
  "resolution": "768P",
  "duration": 5,
  "ratio": "16:9",
  "callback_url": "https://client.example.com/callback",
  "aigc_watermark": false
}
```

cURL：

```bash
curl --request POST "$BASE_URL/hailuo/v2/video_generation" \
  --header "Authorization: Bearer $API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "MiniMax-H3",
    "content": [
      {"type": "text", "text": "一座灯塔矗立在雾海中，镜头缓慢向前推进"}
    ],
    "resolution": "768P",
    "duration": 5,
    "ratio": "16:9",
    "aigc_watermark": false
  }'
```

参数：

| 字段 | 类型 | 必填 | 可用值或范围 | 说明 |
| --- | --- | --- | --- | --- |
| `model` | string | 是 | `MiniMax-H3` | 当前自定义 Hailuo 路由固定使用此模型 |
| `content` | array | 是 | 至少包含一个非空 `text` 项 | 多模态输入，见下表 |
| `resolution` | string | 是 | `768P`、`2K` | 输出分辨率 |
| `duration` | integer | 是 | 4 到 15 | 输出视频时长，单位为秒 |
| `ratio` | string | 否 | `adaptive`、`21:9`、`16:9`、`4:3`、`1:1`、`3:4`、`9:16` | 画面比例 |
| `callback_url` | string | 否 | HTTP/HTTPS 回调地址 | 网关校验类型和基本格式后传递给上游；受理成功不保证回调可达 |
| `aigc_watermark` | boolean | 否 | `true`、`false` | 是否添加水印，显式 `false` 会保留 |

不传 `ratio` 时，纯文本输入默认为 `16:9`，含图片或视频时默认为
`adaptive`。显式 `adaptive` 也支持音频参考输入，纯文本输入不能使用它。

### `content` 项

| `type` | `role` | 数据形状 | 用途 |
| --- | --- | --- | --- |
| `text` | 无 | `{ "text": "..." }` | 视频提示词 |
| `image_url` | `first_frame` | `{ "image_url": { "url": "https://..." } }` | 首帧 |
| `image_url` | `last_frame` | 同上 | 尾帧 |
| `image_url` | `reference_image` | 同上 | 参考图 |
| `video_url` | `reference_video` | `{ "video_url": { "url": "https://..." } }` | 参考视频 |
| `audio_url` | `reference_audio` | `{ "audio_url": { "url": "https://..." } }` | 参考音频 |

输入限制：

- 至少一个非空文本项。
- 首帧最多 1 张，尾帧最多 1 张。
- 图片总数最多 9 张，参考图最多 9 张。
- 参考视频最多 3 个，参考音频最多 3 个。
- 帧图片不能和参考图片、参考视频、参考音频混用。
- 图片、视频、音频应使用上游可以访问的 URL。

创建成功响应：

```json
{
  "task_id": "task_gateway_h3_01"
}
```

## H3-Context-IR

Context-IR 是文本任务，用于根据文本和多模态输入生成增强提示词，不生成
视频。成功结果中的增强提示词位于 `task.content.prompt`。

### 请求

```http
POST /hailuo/v2/h3_context_ir
```

```bash
curl --request POST "$BASE_URL/hailuo/v2/h3_context_ir" \
  --header "Authorization: Bearer $API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "MiniMax-H3",
    "content": [
      {"type": "text", "text": "把这段描述扩展成电影级视频提示词"},
      {"type": "image_url", "role": "reference_image", "image_url": {"url": "https://cdn.example/reference.png"}}
    ],
    "duration": 5,
    "ratio": "16:9",
    "callback_url": "https://client.example.com/callback"
  }'
```

参数规则：

- `model` 必须为 `MiniMax-H3`。
- `content` 必须为数组，最终必须包含非空文本项。
- `duration` 必填，范围为 4 到 15 的整数。
- `ratio` 可选，取值和视频生成接口相同。
- 支持图片、视频和音频等多模态输入，数量和混用限制与视频生成相同。
- 支持可选 `callback_url`；此接口不需要 `resolution`。

创建响应：

```json
{
  "task_id": "task_gateway_ir_01"
}
```

## H3 视频再生成

再生成输出分辨率固定为 `2K`，支持从网关源任务再生成和直接传入源视频两种
模式。

### 源任务模式

`source_task_id` 使用当前用户已有的网关公开任务 ID。网关会验证任务归属、
插件平台、渠道、模型、成功状态和源分辨率，然后才调用 MiniMax 上游。

```bash
curl --request POST "$BASE_URL/hailuo/v2/video_regeneration" \
  --header "Authorization: Bearer $API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "MiniMax-H3",
    "source_task_id": "task_gateway_h3_01",
    "resolution": "2K",
    "aigc_watermark": false
  }'
```

要求：

- `model` 必须为 `MiniMax-H3`。
- `source_task_id` 必须是当前用户自己的成功 H3 视频生成任务。
- 源任务分辨率必须为 `768P`。
- `resolution` 必须为 `2K`。
- 不能同时提交 `content`、`prompt`、`images`、`image` 或 `input_reference`。

成功响应：

```json
{
  "task_id": "task_gateway_regen_01"
}
```

### 源视频内容模式

直接内容模式需要在 `content` 中提供一个 `base_video`，并提供生成源视频时的最终
非空提示词。

```bash
curl --request POST "$BASE_URL/hailuo/v2/video_regeneration" \
  --header "Authorization: Bearer $API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "MiniMax-H3",
    "resolution": "2K",
    "content": [
      {"type": "text", "text": "保留原有动作，改成黄昏光线和更宽的镜头"},
      {"type": "video_url", "role": "base_video", "video_url": {"url": "https://cdn.example/source-768p.mp4"}},
      {"type": "image_url", "role": "reference_image", "image_url": {"url": "https://cdn.example/style.png"}}
    ]
  }'
```

要求：

- `resolution` 必须为 `2K`。
- `content` 必须包含恰好一个 `base_video`。
- `base_video` 必须是 `video_url`，且包含 URL。
- 必须包含非空 `text` 项；如果只传 `base_video` 会返回 400。
- 普通参考媒体可以继续提供，但帧图片不能和参考媒体混用。
- 支持可选 `callback_url` 和 `aigc_watermark`。
- 直接内容模式不能通过 JSON 以外的文件上传方式替代 `base_video`。

### 外部视频规格与读取

2026-09-17 当前渠道实测：宽高不能被 32 整除、无音轨、100 帧的 `base_video`
分别被上游拒绝；帧数错误要求为 107–362、步长 17。768×768、24 FPS、124 帧、
带 AAC 音轨的 MP4 成功再生成。额外参考视频在 20/61 FPS 时被拒绝，上游提示
范围为 23.976–60 FPS。上述是实际样本及错误证据，不代表已穷举所有素材边界。

媒体地址必须在上游读取时仍可访问。已有 H3 产物签名 URL 也可能被上游拒绝；
本轮一例返回 `media URL cannot access a private address`，不能据此推断所有
签名 URL 都不支持。优先使用源任务 ID；外部内容模式必要时将视频转存至自己的
对象存储。受理成功不代表后续素材校验通过，应继续查询终态及退款记录。

### 回调行为

Hailuo 实测发送 `challenge`，接收端应以 JSON 原样返回其值，随后接收状态通知。
但可达性不是提交前置门槛：不可达回调地址下的 H3 生成和 Context-IR 仍受理并
完成。回调消费者需处理重复、乱序，避免旧状态覆盖终态，并以任务查询作为补充。

## 查询单个任务

```http
GET /hailuo/v2/query/video_generation/:task_id
```

```bash
curl --request GET "$BASE_URL/hailuo/v2/query/video_generation/task_gateway_h3_01" \
  --header "Authorization: Bearer $API_KEY"
```

典型成功任务：

```json
{
  "task": {
    "id": "task_gateway_h3_01",
    "model": "MiniMax-H3",
    "task_type": "generation",
    "status": "succeeded",
    "modality": "video",
    "resolution": "768P",
    "duration": 5,
    "content": {
      "url": "https://provider.example/video.mp4"
    }
  }
}
```

网关会把返回对象中的任务 ID替换为公开任务 ID，并补齐 `model`、
`task_type`、`modality`、时间和失败信息等字段。其他已保存的上游任务字段
会随任务快照返回。

状态映射：

| 返回值 | 含义 |
| --- | --- |
| `queued` | 已提交，等待处理 |
| `running` | 上游正在处理 |
| `succeeded` | 已成功 |
| `failed` | 上游失败 |
| `cancelled` | 已取消 |

Context-IR 任务的 `task_type` 为 `h3_context_ir`，`modality` 为 `text`，
成功文本位于：

```json
{
  "task": {
    "task_type": "h3_context_ir",
    "modality": "text",
    "status": "succeeded",
    "content": {"prompt": "增强后的完整视频提示词"}
  }
}
```

## 查询任务列表

```http
GET /hailuo/v2/query/video_generation
```

```bash
curl --get "$BASE_URL/hailuo/v2/query/video_generation" \
  --header "Authorization: Bearer $API_KEY" \
  --data-urlencode "page_num=1" \
  --data-urlencode "page_size=20" \
  --data-urlencode "filter.status=succeeded" \
  --data-urlencode "filter.task_type=generation"
```

参数：

| 参数 | 类型和范围 | 说明 |
| --- | --- | --- |
| `page_num` | 整数，1 到 100000，默认 1 | 页码 |
| `page_size` | 整数，1 到 100，默认 20 | 每页数量 |
| `filter.model` | 单个值，只能为 `MiniMax-H3` | 模型过滤 |
| `filter.status` | `queued`、`running`、`succeeded`、`failed`、`cancelled` | 状态过滤 |
| `filter.task_type` | `generation`、`regeneration`、`h3_context_ir` | 任务类型过滤 |
| `filter.task_ids` | 可重复，最多 100 个 | 按网关公开任务 ID过滤 |

任务列表只返回当前用户在网关创建的任务。成功响应：

```json
{
  "items": [
    {
      "id": "task_gateway_h3_01",
      "model": "MiniMax-H3",
      "task_type": "generation",
      "modality": "video",
      "status": "succeeded"
    }
  ],
  "total": 1
}
```

`filter.task_ids` 的重复参数示例：

```bash
curl --get "$BASE_URL/hailuo/v2/query/video_generation" \
  --header "Authorization: Bearer $API_KEY" \
  --data-urlencode "filter.task_ids=task_gateway_h3_01" \
  --data-urlencode "filter.task_ids=task_gateway_h3_02"
```

## 取消或删除任务

```http
DELETE /hailuo/v2/video_generation/:task_id
```

```bash
curl --request DELETE "$BASE_URL/hailuo/v2/video_generation/task_gateway_h3_01" \
  --header "Authorization: Bearer $API_KEY"
```

网关会先同步任务最终状态，再执行上游管理操作：

- 排队任务请求上游取消，确认后返回 `cancelled`。
- 已成功或已失败任务请求上游删除，网关本地任务和计费记录仍保留。
- 上游无法区分取消和删除的空响应会触发再次查询确认。
- 删除前无法取得最终状态时返回错误，不会把保留中的预扣费直接当作退款完成。

取消成功：

```json
{
  "task_id": "task_gateway_h3_01",
  "action": "cancelled",
  "status": "cancelled"
}
```

删除成功：

```json
{
  "task_id": "task_gateway_h3_01",
  "action": "deleted",
  "status": "deleted"
}
```

## 上游请求映射

以下是插件使用渠道 Base URL调用的上游路径。客户端不应直接调用这些路径，
也不需要接触上游密钥；表中的上游任务 ID由网关内部维护。

| 网关自定义接口 | 上游方法和路径 |
| --- | --- |
| `POST /hailuo/v2/video_generation` | `POST {channel_base_url}/v2/video_generation` |
| `POST /hailuo/v2/h3_context_ir` | `POST {channel_base_url}/v2/h3_context_ir` |
| `POST /hailuo/v2/video_regeneration` | `POST {channel_base_url}/v2/video_regeneration` |
| `GET /hailuo/v2/query/video_generation/:task_id` | `GET {channel_base_url}/v2/query/video_generation/{upstream_task_id}` |
| `DELETE /hailuo/v2/video_generation/:task_id` | `DELETE {channel_base_url}/v2/video_generation/{upstream_task_id}` |

## 计费事实

H3 任务会把以下事实交给网关表达式引擎：

| 字段 | 生成/再生成 | Context-IR |
| --- | --- | --- |
| `operation` | `generation` 或 `regeneration` | `context_ir` |
| `resolution` | 生成是 `768P`/`2K`，再生成是 `2K` | 不适用 |
| `seconds` | 上游输出计费秒数，可能与媒体文件小数时长不同 | `0` |
| `input_images` | 输入图片总数 | `0` |
| `input_video_seconds` | 输入视频秒数，提交时未知则按上限预留 | `0` |
| `prompt_tokens` | `0` | 输入 Token 数 |
| `completion_tokens` | `0` | 输出 Token 数 |

实测再生成输入与产物经 `ffprobe` 检测均为 5.166667 秒、24 FPS、124 帧，上游
`output_seconds=6`，网关按 6 秒结算。若配置 $0.30/秒且无其他费用，净扣为
$1.80；本例按上限预扣 $4.50，完成后退回 $2.70。其他时长以上游实际用量为准，
价格由管理员配置，不应把预扣或文件时长当作最终费用。

例如：

```text
u("operation") == "generation" && u("resolution") == "768P"
  ? tier("generation:768P", u("seconds") * 0.5 + max(u("input_images") - 5, 0) * 0.2)
  : u("operation") == "regeneration" && u("resolution") == "2K"
  ? tier("regeneration:2K", u("seconds") * 0.3 + max(u("input_images") - 5, 0) * 0.15)
  : tier("context_ir", u("prompt_tokens") * 23 / 1000000 + u("completion_tokens") * 5.8 / 1000000)
```

免费图片数量由管理员在表达式中的 `N` 配置，插件不写死免费额度。上游缺失
应计费的实际用量时，网关保留提交阶段的有界预估；上游明确返回的合法零值
会按零结算。

## 错误响应

插件自定义错误通常使用以下结构：

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request",
    "message": "MiniMax-H3 resolution must be 768P or 2K",
    "http_code": "400"
  }
}
```

常见错误：

| HTTP或错误信息 | 原因 |
| --- | --- |
| 400 `model must be MiniMax-H3` | 原生 H3 路由使用了其他模型 |
| 400 `duration must be an integer between 4 and 15 seconds` | H3 时长越界或不是整数 |
| 400 `regeneration requires a 768P source task` | 源任务不是成功的 H3 768P任务 |
| 404 `task_not_found` | 任务不存在、不属于当前用户或不属于当前插件 |
| 502 `task_sync_failed` | 取消/删除前无法同步上游最终状态 |
| 503 `task_cancel_pending` | 上游取消已提交，但尚未确认 |
| 503 `task_settlement_unavailable` | 上游删除时最终用量尚未可用，预扣费等待对账 |
