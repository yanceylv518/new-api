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

## Review Fixes (2026-09-08)
- [x] 统一上游业务错误与幂等删除，代理请求保持独立超时
- [x] 素材组绑定上游密钥，删除状态与创建落库互斥
- [x] 持久化 OSS 对象所属位置，刷新返回完整最新记录
- [x] 素材列表接入服务端分页、搜索和筛选
- [x] 折扣管理接口同时返回跨分组启用模型目录，统一失败重试，保留隐藏折扣
- [x] 回归测试、类型检查、构建和最终差异复核

### Review Fix Verification
- Go 回归通过：`go.exe test ./model ./controller ./service ./relay ./relay/helper ./setting/system_setting -run 'Seedance|PrivateAssetOSS|UserModel|ModelPricing|TaskBilling|RecalculateTask|Tiered|TextQuota|AudioQuota' -count=1 -timeout=180s`。
- 最终后端改动验证通过：`go.exe test ./controller ./service ./model -run 'Seedance|PrivateAssetOSS|UserModelPricingIncludesModelsOutsideOperatorGroup' -count=1 -timeout=90s`。
- 前端通过：Jiti 执行的 40 项回归测试、`npm.cmd run typecheck`、变更文件的 oxlint 和 oxfmt 检查、`npm.cmd run build`。
- 浏览器验证通过：1440x1000、390x844、375x667；分页、搜索、单项刷新、预览节点稳定和局部滚动均正常。API 使用网络拦截测试数据，未进行真实 OSS/VPS 或外部 MySQL/PostgreSQL 集成验证。
- `git.exe diff --check` 通过；保留已有任务退款和翻译改动。本轮未提交或部署。

## Branch Extraction (2026-09-08)
- [x] 保存未提交改动并按折扣、素材库分别记录审查修复。
- [x] 重建 custom/develop，仅保留折扣及其修复，验证不再引入素材库。
- [x] 从 custom/rc23 独立提取视频功能与素材库修复，不引入折扣提交或实现。
- [x] 两分支分别通过回归、类型检查和生产构建；无关改动单独保存，不推送或部署。

### Branch Verification
- 共同基线为 `3d1eaf61f`；上方 Review Fix Verification 是拆分前完整代码的历史验证记录。
- 折扣分支：模型、服务和中继计费回归通过；折扣控制器测试解除对 OSS 测试初始化的依赖后通过；26 项前端测试及 `npm.cmd run build:check` 通过。
- 视频分支：`go.exe test ./model ./controller ./service ./setting/system_setting -run 'Seedance|PrivateAssetOSS' -count=1 -timeout=180s`、18 项前端测试及 `npm.cmd run build:check` 通过。
- 未提交的任务退款展示和翻译改动不属于此次拆分提交，保留后恢复至 custom/develop 工作区。
