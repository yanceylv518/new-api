# Task Plan: Seedance 私域素材库

## Goal
为当前项目增加面向用户的火山方舟 Seedance 私域素材库管理和视频引用能力。

## Phases
- [x] Phase 1: 梳理火山方舟素材 API 与现有任务链路
- [x] Phase 2: 实现后端素材模型、客户端、权限和路由
- [ ] Phase 3: 接入 Seedance 素材引用与任务状态
- [ ] Phase 4: 实现前端素材库和生成页选择器
- [ ] Phase 5: 测试、构建和低成本真实冒烟验证

## Decisions Made
- 素材能力按 Seedance/火山方舟抽象，不绑定 ShuYan 渠道名称。
- 原始文件由上游托管，本地只保存元数据和 asset_id。
- 真实测试只使用 doubao-seedance-2-0-fast-260128 的最小请求。

## Errors Encountered
- 暂无

## Status
**Currently in Phase 3** - 后端基础已完成；正在处理官方火山方舟与兼容代理的素材 API 路径差异，再接入状态轮询和前端。

## Hardening Checklist (2026-09-07)
- [x] 列表读取与上游状态同步解耦，改为有界服务端后台轮询
- [x] 处理手动刷新、后台轮询和旧响应之间的并发覆盖
- [x] 补齐本地落库失败、预览签名失败和分组更新失败的远端补偿
- [x] 为素材接口增加禁缓存、用户维度限流和请求上下文传播
- [x] 补充后台轮询、退避、业务失败和取消请求回归测试
