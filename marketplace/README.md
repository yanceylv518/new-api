# PinduCloud 插件市场源

独立的 Vercel 静态项目，复用 New API 当前支持的 `indexVersion: 1` 市场格式。索引、插件源码和图标均来自同一个域名，插件安装仍通过网关原有的管理员确认、SHA-256 校验和编译流程。

## 已部署地址

- 市场索引：<https://pinducloud-plugin-market.vercel.app/index.json>
- 兼容索引：<https://pinducloud-plugin-market.vercel.app/api/v1/plugins/index.json>
- 项目管理：<https://vercel.com/z67x8c99b-2675/pinducloud-plugin-market>

在网关任务插件管理的“市场 → 管理源”中添加名称 **PinduCloud** 和上面的市场索引地址。Vercel 已确认首次生产部署为 `READY`，包含 12 个任务插件；当前未关联 Git 仓库，修改本地文件不会自动发布。

## 构建与发布

要求 Node.js 22 或更高版本，不需要安装第三方依赖。

从当前源码同步所有任务插件并生成部署文件：

```powershell
node.exe 'marketplace/build.mjs' --sync
```

`--sync` 读取本仓库 `plugins/tasks/` 的受信任源码，并生成 `catalog.json`、`releases/` 版本快照及 `public/` 静态文件。普通构建仅使用已有快照，独立上传 `marketplace` 目录到 Vercel 即可构建：

```powershell
node.exe 'marketplace/build.mjs'
```

Vercel 项目根目录为 `marketplace`，Framework Preset 为 Other，构建命令为 `node build.mjs`，输出目录为 `public`。项目不需要环境变量、数据库或后端函数。

固定市场源地址为部署域名下的 `/index.json`。`/api/v1/plugins/index.json` 也指向同一索引。在网关任务插件管理的“市场 → 管理源”中添加名称 **PinduCloud** 和完整索引地址。

后续可使用已安装的 Vercel CLI 发布到现有项目。访问令牌通过当前终端的 `VERCEL_TOKEN` 环境变量提供，不写入文件或命令参数；在仓库根目录执行：

```powershell
$env:VERCEL_ORG_ID = 'team_U6c7b35zsa1JL07oQDjPs6cf'
$env:VERCEL_PROJECT_ID = 'prj_sPSSsvtgxuNtHX5BuAvjVKM1gWIR'
vercel deploy --cwd 'marketplace' --prod --yes
```

首次部署使用 Vercel REST API 上传目录内的构建配置、索引及版本快照；构建依赖和发布令牌均不需要配置到项目环境中。

## 更新插件

1. 修改 `plugins/tasks/<key>/plugin.js`，递增插件 `meta.version`。
2. 按当前任务授权完成需要的检查，再执行 `node.exe 'marketplace/build.mjs' --sync`。
3. 发布新的 `marketplace` 目录到同一 Vercel 项目。若使用 Git 自动部署，应将插件源码、更新后的 `catalog.json` 和 `releases/` 一起提交；`public/` 不提交。
4. 站点管理员刷新 PinduCloud 市场源，选择新版本安装或更新。

同一插件版本禁止更换源码或图标。索引使用重新验证缓存，带版本的文件使用不可变缓存；更新版本会生成新路径和 SHA-256，不会覆盖旧文件。旧快照保留在索引版本列表中，可通过原有版本选择入口安装。

市场不会静默更新已经安装的插件。当前网关以插件版本判断“已是最新”，所以仅改文件内容而不递增版本既不能正常提示更新，也会触发构建的完整性保护。

## 公开范围

这是由 PinduCloud 维护、可公开读取的市场源，下载不要求登录 Vercel；发布权限由 Vercel 项目控制。只发布插件文件、描述、版本和许可证，不包含渠道配置、调用密钥、素材或数据库数据。

插件中的原始项目与作者信息保留，原始许可证及归属声明发布于 `/licenses/`。当前快照来自 `custom/newapi-video` 工作区，市场构建的完整性检查不代表各插件已完成业务测试。
