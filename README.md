# MailBasin

本地运行的邮箱池管理工具：把 Apple、微软、Google 与自有 IMAP 邮箱集中接入，统一收信、取验证码、批量创建与清理。后端使用 Go，前端使用 Vue 3，账号、邮箱、邮件、任务和系统设置统一保存在 SQLite。

## 环境要求

- Go 1.25 或更高版本
- Node.js 20.19+ 或 22.12+
- npm

## 快速启动

首次运行先复制配置文件：

```bash
cp config.example.json config.json
```

直接启动：

```bash
go run . -config config.json
```

打开：

```text
http://127.0.0.1:8788/
```

首次打开会要求创建本地管理员，项目没有默认账号或密码。

## 忘记管理员密码

密码是单向散列（`pbkdf2_sha256`，12 万次迭代），找不回来，只能重置：

```bash
./bin/ipm-server -reset-password -config config.json
```

也可以在启动菜单里选 **5. 重置管理员密码**。

命令会先打印目标数据库和管理员账号，再要求输入两次新密码。运行期间服务可以照常开着，
重置后**无需重启**即可用新密码登录。

几点说明：

- **就地更新原记录**，保留管理员 ID。删除重建会让账号和邮箱的 `owner_id` 变成悬空引用。
- **同时作废全部登录态**，避免出现"密码改了但旧会话还能用"。
- 密码不会作为命令行参数传入（那会留在进程列表里），也不进 shell 历史：
  交互式输入时关闭回显；也可以从管道读入，便于脚本化。
- 数据库位置由 `-config` 指定的配置里的 `data_path` 决定，也可以用 `-data` 直接覆盖：

```bash
./bin/ipm-server -reset-password -data data/app.db
```

重置前建议先备份 `data/app.db` 和 `data/app.db.key`。

## 构建

一键构建前端和 Go 服务：

```bash
./scripts/build.sh
```

构建产物：

```text
bin/ipm-server
```

启动构建产物：

```bash
./bin/ipm-server -config config.json
```

只重新构建前端并同步到 Go 嵌入目录：

```bash
npm --prefix frontend ci
npm --prefix frontend run build
./scripts/sync-web.sh
```

## 开发模式

```bash
./scripts/dev.sh
```

- 前端：http://127.0.0.1:5174/
- 后端：http://127.0.0.1:8788/

## 测试

```bash
go test ./...
go vet ./...
npm --prefix frontend run build
```

## 常用配置

配置文件默认为 `config.json`，完整字段可参考 `config.example.json`。

| 配置 | 说明 |
| --- | --- |
| `host` / `port` | 服务监听地址和端口 |
| `data_path` | SQLite 数据库路径 |
| `secure_cookie` | HTTPS 部署时启用安全 Cookie |
| `api_key` | 公共取号 API Key |
| `database_backup_dir` | SQLite 备份目录 |
| `database_backup_retention_count` | 自动或手动备份后最多保留的份数，默认 3 |
| `server_chan_send_key` | Server 酱 SendKey |
| `microsoft_enabled` | 是否启用 Microsoft 账号模块，默认 `true` |
| `microsoft_client_id` | Azure 应用的客户端 ID；留空时使用内置公共客户端 |
| `microsoft_tenant` | Azure 租户，默认 `common` |
| `microsoft_mail_watcher_poll_ms` | Graph 收信轮询间隔，默认 15000（下限 5000） |
| `microsoft_mail_watcher_fetch_limit` | 单轮每个 Microsoft 账号最多拉取的邮件数，默认 30 |

SQLite 主数据库和密钥文件必须成对保留：

```text
data/app.db
data/app.db.key
```

服务启动 1 分钟后执行首轮自动备份，此后每 24 小时备份一次。系统设置页也可以点击“立即备份”。每次备份后默认只保留最新 3 份 `.db` 和 `.db.key` 文件。

## 版本与公告

系统设置的更新检查只读取 [`internal/updatecheck/announcements.json`](./internal/updatecheck/announcements.json)。发布新版本时更新 `latest`；项目消息放入 `announcements`。该方式使用 GitHub Raw 公开文件，不请求 GitHub REST API。

```json
{
  "schema_version": 1,
  "latest": {
    "version": "2.1.2",
    "name": "2.1.2 源码版",
    "notes": "新增 iCloud Web API 分场景开关，请重新下载最新源代码并按文档重新构建",
    "published_at": "2026-08-29T22:30:00+08:00",
    "url": "https://github.com/xiuxiu56/iCloud-Privacy-Mail-v2/archive/refs/heads/main.zip"
  },
  "announcements": []
}
```

### 一键更新

系统设置「版本与更新」里的**立即更新**按钮做的是：

1. `git fetch origin main`，把源码切到 `origin/main`；工作树里有**已跟踪文件**的改动时直接拒绝，不会覆盖你的改动（未跟踪的新文件不算，见 `update.ps1` 里的 `--untracked-files=no`）；
2. 在本机构建（前端产物已经提交在仓库里，所以这一步只要有 Go，不需要 Node）；
3. 停服务 → 备份当前可执行文件并逐字节校验 → 替换 → 启动 → 探活；
4. 新版本起不来就用备份自动回滚。

**这是冷更新，不是热更新。** Go 编译出来的是原生可执行文件，一个进程没法替换自己正在运行的镜像，所以第 3 步必须把服务停掉再起来，实测一次停机 6.4 秒——做法是在更新期间用独立进程每 100ms 探一次 /api/health，取「最后一次成功响应」到「换完之后第一次成功响应」之间的间隔；停机期间每次探测都要等到 2 秒超时，采样精度因此退化到约 2 秒，所以这个数字带 ±2 秒粒度。第 1、2 步以及任何一步失败都发生在停服务之前，代价是零停机。

**这个按钮要点两次。** 第一次点击不会更新，只会展开一条确认条，写明「构建提交 `X` → 远端 `main` 当前的提交 `Y`」以及期间会短暂连不上服务；再点确认条里的**确认更新**才真的开始。那个 `Y` 不是推断：检查更新时后端会直接问远端 `main` 分支此刻指向哪个提交（走 git 的 `info/refs`，和 `git ls-remote` 同一条路，不需要令牌、没有 API 速率限制），而更新器 `reset --hard origin/main` 得到的正是它。取不到时确认条退回「`origin/main`」的字样并说明暂时问不到，**不会拿分支名或上一次的提交号冒充目标**。确认之后按钮先显示「正在启动更新…」，等更新器报出第一步再换成真正的进度。更新器跑在计划任务 `MailBasin-Update` 里、在服务之外，所以服务被停掉的那几秒它照样在干活，页面刷新后也能看到结果。

会被拒绝的情况：批量同步或 Apple 邮件清理正在跑（它们正在读写 SQLite，强杀进程会破坏 WAL）、已经有一次更新在跑、工作树里有已跟踪文件的改动、或配置里 `update_enabled` 为 `false`。

更新卡片上的「A → B」是**工作树提交 → 目标提交**。如果源码已经在目标提交上、只是正在运行的不是它，两边会显示同一个提交号——这时更新器做的是重新构建并装配，没有搬运代码。

**信任模型（说清楚）**：更新器信任 `origin/main`，不做签名校验——本项目只发布源码，没有可分发的签名产物。也就是说，**能往 main 推送的人，就能在这台机器上执行代码**。如果这不是你要的边界，请换成你自己控制的私有仓库，或用只读密钥来跑更新。

## 主题模式

### 亮色模式

![亮色模式](./docs/screenshots/09-light-mode.jpg)

### 深色模式

![深色模式](./docs/screenshots/08-dark-mode.jpg)

## 页面

### 登录

![登录页面](./docs/screenshots/00-login.jpg)

### 控制台

![控制台](./docs/screenshots/01-dashboard.jpg)

### Apple 账号

![Apple 账号](./docs/screenshots/02-apple-accounts.jpg)

### 邮箱池

![邮箱池](./docs/screenshots/03-mailboxes.jpg)

工具栏的**体检邮箱能否读取**逐个登录池里的邮箱，把能读的和读不了的分开列出并写明原因，读不了的排在最前面。收信数为 0 这种迹象是不够的——它同时表示「从没检查过」「检查过但没有邮件」「本地根本没有这条记录」三件完全不同的事，所以这三件事被分开报出。

**每个邮箱页都有同一个按钮**：Apple 账号、Microsoft 账号、Google 账号，以及 QQ、163、Gmail 三页。放在账号页上，是为了让筛选发生在进池之前——读不了的地址在进池时就被挡住，不必先进池，再等它永远等不到验证码。账号页的体检取的是**该页列出的账号名下的全部地址，包含默认不进池的那些**（QQ、163、Gmail 的地址默认就不进池，只看池内的话恰恰是这批永远没人检查）。这几页是分页的，所以面板上会写明本次覆盖了几个账号、多少个地址；翻页后再点一次即可检查下一页。按页是刻意的，不是没做完：Gmail 这一类在这台机器上有四百多个账号，一次点错就让服务端朝几百个账号同时发起登录，既慢，也容易被对面的服务商按限流拦下，而拦下之后就分不清「这个地址读不了」和「这次被限流了」——那正是体检最不该出错的地方。

体检的结论有两种分辨率，界面上分得清：

- **按地址**：IMAP 与外部取码邮箱。每个地址有自己的一份凭据，逐个登录、或向取码接口要一次信。
- **按账号**：Microsoft、Google、Apple。名下地址共用母号的一份授权，母号读不了则名下每个地址一起读不了，所以结论来自母号那一次检查（Microsoft 与 Google 取访问令牌并读一次账号资料，Apple 检查母号的 IMAP 取码登录）。同一个母号在一次体检里只查一次：五个别名查五遍既让对面多收四次同样的请求，也可能给出互相矛盾的结论。报告里每一条都标着结论来自哪个母号。

体检会改状态，这一点必须说清楚：

- 判定为**凭据失效或连接信息缺失**的邮箱会被标成失败、移出邮箱池。这样「能不能用」看状态就够，不必去读报告。
- **判定要求两件事同时成立：错误码属于「凭据或配置本身不对」那一类，并且这次失败不可重试。** 同名的错误码会出现在意思相反的场合——IMAP 的 `imap_login_failed` 既是「登录请求本身失败」（网络断了，可以重试），也是「登录被拒」（凭据真错，重试也没用），只靠错误码分不开。可重试标记由服务商自己给出：它说这次可以重试，就永远不足以支撑一次永久的状态改动。
- **网络类的失败一律不改状态**（连不上、超时、问候失败、取码接口 503）。一次网络抖动不该摘掉一个本来能用的邮箱：摘错了，使用者连等一个验证码的机会都没有。这些仍会列在报告里，只是状态不动。
- **够不到的通道报成「本次未覆盖」，不报成「读不了」**。苹果母号只配了 iCloud 网页通道时属于这一类：那条通道的校验不在本次检查范围内，而名下地址照样能读到验证码，按「读不了」处理会白白摘掉好地址。没有关联母号的旧记录同理。
- 只动仍然标着「可用」的邮箱。手工设过的状态、正在出借的邮箱都不碰——改状态会顺带释放租约，碰错了等于把邮箱从正在用它的任务手里拽走。
- 不在**不发信、不取邮件正文**之外承诺更多只读：除此之外它只改上面说的那一种状态。
- 单次请求最多二十个邮箱，界面分批循环并逐批显示进度。
- 恢复办法：报告里每一条都记着摘之前的状态与原因，把状态改回「可用」即可。

### 创建隐私邮箱

![创建隐私邮箱](./docs/screenshots/04-tasks.jpg)

### 系统设置

![系统设置](./docs/screenshots/06-settings.jpg)

### 公共邮箱取码

![公共邮箱取码](./docs/screenshots/07-email-code.jpg)

## Microsoft 账号（Microsoft Graph）

同一个项目同时支持 Apple 与 Microsoft 两种邮箱来源，共用一个界面和一套邮箱池，不需要启动两个服务。
Microsoft 侧走**官方 Graph API**，不需要逆向接口，也不会因为客户端协议变更而失效。

### 和 iCloud 的区别

| | iCloud | Microsoft |
| --- | --- | --- |
| 授权方式 | Apple ID + 密码 + 2FA | OAuth2 设备码（浏览器里登录） |
| 读信通道 | IMAP IDLE / iCloud Web API | Microsoft Graph |
| 创建别名 | 支持（Hide My Email） | **不支持**，见下 |
| 发现已有别名 | 支持 | 支持（读取 `proxyAddresses`） |

> **Microsoft Graph 没有创建别名的接口。** Outlook 别名只能在
> [account.microsoft.com](https://account.microsoft.com) 上手工创建，且单个账号最多 10 个。
> 因此 Microsoft 侧的流程是：先在微软官网建好别名，再用本项目的“发现别名 / 导入别名”把它拉进邮箱池。

### 使用步骤

1. 打开侧边栏的 **Microsoft 账号** 页面，点击“添加 Microsoft 账号”。
2. 点击“获取设备码”，页面会显示一个形如 `ABCD-EFGH` 的设备码。
3. 在浏览器打开页面给出的地址，输入设备码，登录你要接入的 Microsoft 账号并同意授权。
4. 页面会自动轮询并在授权完成后把账号加入列表。
5. 选中账号，点击“发现别名”从 Graph 读取该账号上的全部邮件地址；点击“导入别名”写入邮箱池。
   也可以在微软官网建好别名后用“手工添加”单独录入。
6. 之后邮箱池的取码、邮件列表、后台监听都会自动覆盖这些 Microsoft 别名。

### 用自己的 Azure 应用（推荐）

不配置时使用内置的微软 Office 公共客户端 ID（`d3590ed6-52b3-4102-aeff-aad2292ab01c`），开箱可用；
但那个 ID 被大量第三方工具共用，微软可能随时收紧策略，长期使用建议注册自己的应用。

**注册应用本身是免费的**，不需要 Azure 订阅，也不需要绑卡。需要的是一个能登录
[Azure 门户](https://portal.azure.com) 的 Microsoft 账号（个人账号也行，会自动获得一个默认目录）。

完整步骤：

1. 打开 [Azure 门户](https://portal.azure.com)，用任意 Microsoft 账号登录。
   个人账号首次进入可能提示“注册 Azure”，**不需要**创建订阅，直接跳过即可。
2. 搜索并进入 **Microsoft Entra ID**（旧名 Azure Active Directory）→ 左侧 **应用注册** → **新注册**。
3. 名称随便填（例如 `MailBasin`）。
4. **受支持的帐户类型**选：
   **“任何组织目录中的帐户和个人 Microsoft 帐户”**。
   这一项必须选对——只选单租户或仅组织目录的话，个人 outlook.com / hotmail.com 账号会授权失败。
5. **重定向 URI 留空**。设备码流不需要它。
6. 点 **注册**，在概览页复制 **应用程序(客户端) ID**。
7. 左侧 **身份验证** → 拉到最下面 **高级设置** → **允许公共客户端流** 选 **是** → 保存。
   设备码流属于公共客户端，不开这一项会报 `AADSTS7000218`。
8. 左侧 **API 权限** → **添加权限** → **Microsoft Graph** → **委托的权限**，
   勾选 `Mail.Read`、`User.Read`、`offline_access` → 添加。
   `offline_access` 是拿到 refresh token 的前提，漏了会导致过一小时就要重新授权。
9. **不要**创建客户端密码（client secret）。公共客户端用不上，创建了反而是多一个要保管的凭据。
10. 回到本项目的 **系统设置 → Microsoft 账号**，把客户端 ID 填进去，点 **保存 Microsoft 设置**。
    租户保持 `common`（个人账号必须用 `common`）。

也可以在 `config.json` 里设 `microsoft_client_id` / `microsoft_tenant`；界面里保存的值优先，
方便不重启就切换。

常见报错对照：

| 报错 | 原因 |
| --- | --- |
| `AADSTS7000218` | 没启用“允许公共客户端流” |
| `AADSTS50059` / 找不到租户 | 租户填成了具体域名，个人账号请用 `common` |
| `unauthorized_client` | 受支持的帐户类型没选“个人 Microsoft 帐户” |
| 授权一小时后就失效 | API 权限漏了 `offline_access` |

### 收信怎么分发到各个别名

Graph 返回的是**整个收件箱**，而 iCloud 是按别名分别取信。因此本项目按收件人地址
（`toRecipients` + `ccRecipients`）把每封邮件路由到对应的别名邮箱，一封邮件只归属第一个命中的别名。
没有命中任何本地别名的邮件会被计数并记录，不会静默丢弃——如果这个数字一直增长，
说明还有别名没有导入。

### 安全说明

- `access_token` / `refresh_token` 在 SQLite 里加密存储，且**不会**出现在实时事件推送里。
- 设备码登录过程中，密码只在微软官方页面输入，本地从不接触。
- 授权失效（`invalid_grant`）时账号会标记为“需要登录”，重新走一次设备码流程即可。

## 页面路由

| 路由 | 页面 |
| --- | --- |
| `/login` | 管理员登录 |
| `/` | 控制台 |
| `/apple-accounts` | Apple 账号与登录态 |
| `/microsoft-accounts` | Microsoft 账号、OAuth2 授权与别名导入 |
| `/mailboxes` | 邮箱池与邮件取码 |
| `/tasks` | 隐私邮箱创建任务 |
| `/settings` | 系统设置、数据库维护与消息推送 |
| `/email-code` | 公共邮箱取码页面 |

## 项目结构

```text
├── main.go                 Go 服务入口
├── config.example.json     配置示例
├── internal/               后端业务、协议、SQLite 和 HTTP API
├── frontend/               Vue 3 前端
├── scripts/                开发、构建和前端同步脚本
├── docs/screenshots/       页面截图
└── bin/                    本地构建产物
```

## 更新代码后重新运行

如果只修改 Go 代码：

```bash
go run . -config config.json
```

如果修改了 `frontend/src`：

```bash
npm --prefix frontend run build
./scripts/sync-web.sh
go run . -config config.json
```
