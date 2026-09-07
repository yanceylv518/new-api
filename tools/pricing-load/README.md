# 用户模型定价实测

使用独立 Docker Compose 的 MySQL、PostgreSQL 和 Redis，不连接部署站点。数据库端口仅绑定回环地址，数据目录为临时内存挂载。测试账号和令牌临时生成，结束后删除测试表和专用 Redis 逻辑库。

## 数据库回归

```powershell
docker.exe compose -f tools/pricing-load/compose.yml up -d --wait
$env:PRICING_EXTERNAL_TESTS = '1'
go.exe test ./model -run '^TestUserModelPricingExternalDatabases$' -v -count=1 -timeout=90s
```

覆盖独立表旧索引升级、重复迁移、大小写及重音不同模型、精确重复拒绝、缓存改价、并发 revision 冲突、真实行锁等待取消，以及清空规则后的原价状态。没有此环境变量时，普通单元测试跳过外部数据库测试。

Docker Hub 不可直连时可从 Google 公共镜像缓存拉取同一官方镜像，再设置本地标签：

```powershell
docker.exe pull mirror.gcr.io/library/mysql:5.7.44
docker.exe tag mirror.gcr.io/library/mysql:5.7.44 mysql:5.7.44
```

## 最终扣费回归

```powershell
$env:PRICING_EXTERNAL_TESTS = '1'
go.exe test ./tools/pricing-load -run '^TestFinalBillingHTTP$' -v -count=1 -timeout=120s
```

该测试调用真正的 `/v1/chat/completions` 路由。本地受控上游返回明确用量，生产代码负责鉴权、渠道选择、预扣、转发、结算、退款和日志写入。验证钱包与订阅、流式、按次及阶梯定价、最低折扣、余额不足、途中改价，以及多用户并发；按实际数据库余额、令牌余额、缓存剩余额度和逐条消费日志对账。只有上游服务被隔离替代，没有替换扣费函数。

这套测试与下方仅测鉴权及预扣估算的负载工具相互独立，不能将两者指标合并成最终扣费吞吐。

## HTTP 定价负载

```powershell
go.exe run ./tools/pricing-load -engine mysql -rate 1200 -seconds 90 -rules 1000 -output tools/pricing-load/results/mysql-1200.json
go.exe run ./tools/pricing-load -engine mysql -rate 3600 -seconds 60 -rules 1000 -output tools/pricing-load/results/mysql-3600.json
go.exe run ./tools/pricing-load -engine mysql -rate 2400 -seconds 60 -rules 1000 -output tools/pricing-load/results/mysql-2400.json
go.exe run ./tools/pricing-load -engine postgres -rate 1200 -seconds 90 -rules 1000 -output tools/pricing-load/results/postgres-1200.json
go.exe run ./tools/pricing-load -engine postgres -rate 3600 -seconds 60 -rules 1000 -output tools/pricing-load/results/postgres-3600.json
go.exe run ./tools/pricing-load -engine postgres -rate 2400 -seconds 60 -rules 1000 -output tools/pricing-load/results/postgres-2400.json
docker.exe compose -f tools/pricing-load/compose.yml down
```

同一数据库的测试必须串行；为避免相互争用影响指标，两个数据库的负载也应串行执行。工具拒绝覆盖非空用户表。若中断导致测试数据残留，用上述 Compose `down` 后重新 `up` 重建本工具专用环境。

本机运行数据与实测报告保存在 `results/`，由 Git 忽略，不随源码提交。

100 用户各配置 1,000 条独立规则，每个用户的折扣不同。每次请求通过真实 TCP/HTTP、生产 `TokenAuth`、独立定价读取和 `ModelPriceHelper` 预扣估算，并核对响应中的用户及计算结果。中途修改一个用户的完整规则集，提交后新请求必须使用新价；已开始的请求允许使用冻结的旧快照。

负载按固定到达率调度，最多 128 个执行协程、128 个排队请求。延迟从计划到达时间计算，包含排队时间；过载丢弃独立统计。预热不计入测量。持续 60/90 秒覆盖多轮 30 秒缓存过期。报告记录请求数、失败及错价数、延迟分位数、完整规则 SQL 查询数、数据库连接等待、测量结束时堆大小和 GC 次数。新增的最大调度滞后和最大延迟指标用于诊断发送队列丢弃，早期报告没有这两个字段。

这是鉴权和定价组件的 HTTP 实测，未调用模型上游、余额扣减、最终结算或消费日志。客户端与 HTTP 服务在同一进程，数据库运行在本机 Docker，数据使用内存挂载。结果不能直接当作 VPS 全网关容量，也不能替代持续 24 小时的亿次请求验证。全链路容量需另行纳入实际日志存储、余额热点更新、上游延迟与流式并发。
