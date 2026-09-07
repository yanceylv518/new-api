# Notes: Seedance 私域素材库

## Sources

### ShuYan compatibility documentation
- URL: https://doc.shuyanai.com/doc-9030011
- Key points:
  - 素材组和素材使用 `Action` 查询参数，字段使用 PascalCase。
  - 支持 Create/List/Get/Update/Delete AssetGroup 和 Asset。
  - 支持 Image、Video、Audio，上传后需要审核，Active 才可引用。
  - 视频请求通过 `asset://AssetId` 引用素材。
  - 视频生成接口使用 `/api/v3/contents/generations/tasks`。

### Current repository
- `relay/channel/task/doubao/adaptor.go` 已支持 Seedance 任务提交、轮询和 `image_url`/`video_url`/`audio_url` 透传。
- `router/video-router.go` 已提供视频任务和结果代理路由。
- 当前没有持久化的用户素材库模型或素材管理前端。

## Synthesized Findings
- 应新增独立的 Seedance asset provider 能力层，并由渠道配置提供 BaseURL/API Key。
- 必须在本地以 user_id 做所有素材读写授权，不能依赖上游 asset_id 隔离。
- 首期可复用现有任务链路，仅增加素材 CRUD、上传/状态轮询和前端选择器。
