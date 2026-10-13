# 界面 ↔ 后端 连通地图

写给要重构这套界面的 agent。目的不是介绍功能，而是回答一个问题：**改这一处，会牵动什么。**

本文的行号、接口名、字段名都来自当前仓库源码（`origin/main`，HEAD `4ac8b56`）。行号会随代码移动而失效——**别信行号，信文件里的符号名**。凡是源码里读不到的，我会明说没验过，不会用推断填空。

---

## 0. 一句话

一个 Vue 3 单页应用，**编译进 Go 二进制**，由同一个进程在 `8788` 端口既发页面又发 API。前端源码改了**不会自动生效**。

---

## 1. 构建与部署链（最容易踩的地方）

### 生产链路

```
frontend/src/**            ← 你改这里
   │  vite build（npm run build）
   ▼
frontend/dist/**           ← 中间产物，.gitignore 忽略
   │  手工复制
   ▼
internal/webui/dist/**     ← 被 git 跟踪，真正发布的东西
   │  //go:embed dist  (internal/webui/embed.go)
   ▼
bin/ipm-server.exe         ← 前端已经在这个二进制里了
   │  scripts\server.cmd restart
   ▼
http://127.0.0.1:8788
```

**推论：只改 `frontend/src` 而不重建 Go，浏览器上什么都不会变。** 这是这套架构最浪费时间的地方——你以为没生效是缓存，其实是产物根本没进去。

### 服务端怎么发文件

`internal/webui/embed.go` 的 `Handler()`：

1. 路径能对应到 `dist` 里的真实文件 → 直接发；
2. 否则**一律回 `index.html`**（SPA 的 history fallback，前端路由刷新不会 404）；
3. `index.html` 都不存在 → `503 前端资源尚未构建`。

### 开发模式

`frontend/vite.config.js`：dev server 在 `127.0.0.1:5174`，把 `/api` 代理到 `http://127.0.0.1:8788`。

设计时可以 `npm run dev` 开 5174，后端照常跑 8788，改样式有热更新，不必每次重建 Go。会话 cookie 按域名 `127.0.0.1` 存，端口不同不影响，理论上登录态能带过去。

> **这一条我本轮没有实际跑过**，第一次用请自己确认一次（开 dev server，看 `/api/auth/status` 能不能读到已登录状态）。

> **包管理器 — 已查实，按这个来，别自己判断。** 仓库跟踪的是 **npm 的 `package-lock.json`**，它是这份仓库钉依赖版本的唯一凭据。同目录下的 `pnpm-lock.yaml` 是构建时额外生成的副产品，已加进 `.gitignore`，不在仓库里，**忽略它**。
>
> **装依赖、加依赖一律用 npm。** 用别的工具加依赖，新依赖不会被记进仓库跟踪的那份锁文件——别人 clone 下来就装不出你这套版本，两边依赖树分叉，「在我这儿好的、在你那儿坏了」就是这么来的。这是接手界面时最容易出问题的一个动作。

---

## 2. HTTP 约定

### 请求封装

所有请求都过 `frontend/src/api/client.js` 的 `api(path, options)`：

- `credentials: 'same-origin'`；`/api/` 开头的路径强制 `cache: 'no-store'`；
- 有 `body` 且没设 Content-Type 时自动设 `application/json`；
- 网络层失败 → 抛 `ApiError('连接服务器失败，请检查网络或稍后重试')`；被 abort → `'请求已取消'`；
- 响应不是 JSON → `ApiError('服务器返回了无效响应（HTTP xxx）')`；
- `response.ok === false` **或** `payload.success === false` → 都算失败，抛 `ApiError(payload.message, status, payload.code)`；
- **401 特殊处理**：除非是 `/api/auth/login`、`/api/auth/setup` 或 `/api/v1/*`，否则**直接 `window.location.replace('/login?redirect=...')`**——会话过期会把整个页面顶掉，不是弹提示。任何一处 401 都会让你离开当前页面。

**注意返回值**：`api()` 最后是 `return payload?.data ?? payload`。调用方拿到的**已经是信封里的 `data`**，不是整个响应体。

### 响应信封

成功：

```json
{ "success": true, "data": { } }
```

失败（`internal/httpapi/server.go` 的 `writeError` / `writeErrorRetryable`）：

```json
{ "success": false, "code": "snake_case_code", "message": "给用户看的中文", "retryable": true }
```

`retryable` 只在个别路径出现。**`code` 是稳定的，`message` 是给人看的**——要判断就看 `code`，要展示就用 `message`。

### 鉴权

- Cookie 名：`ipm_v2_session`（`internal/auth/service.go`）。
- 绝大多数 `/api/*` 包在 `s.protected(...)` 里（`internal/httpapi/server.go`），没会话就 401。
- **公开的例外**：`GET /api/health`（不鉴权，返回运行中的构建信息，适合探活）；`POST /api/auth/setup`；`POST /api/auth/login`；`GET /api/auth/status`（判断「要不要初始化 / 有没有登录」）。
- 前端 `useAuth.js` 把结果缓存在模块级 `authState`，`loadAuthStatus()` 默认不重复请求。

### 两族 API，别搞混

| | 路径 | 鉴权 | 谁用 |
|---|---|---|---|
| **后台界面族** | `/api/...`（除 `/api/v1/`） | 会话 cookie | 这个 SPA |
| **对外取码族** | `/api/v1/...` | 邮箱 API token（`key=` 查询参数） | 外部脚本、第三方 |

`/api/v1/*` 是给机器调的（领取邮箱、取验证码、租约 commit/release），**除 `PublicCodeView.vue` 外界面不用它**。改界面基本可以无视，但别把两者路径混了。

---

## 3. 实时通道（SSE）

### 服务端

`GET /api/realtime`（`internal/httpapi/realtime.go`，需要会话）：

- `Content-Type: text/event-stream`，先发 `retry: 2000`；
- 每条事件：`id: <sequence>` / `event: change` / `data: {...}`；
- 每 15 秒一次注释心跳（`: 心跳 <unix>`）保活；
- 支持 `Last-Event-ID` 头（或 `?after=`）**补齐断线期间错过的变更**——浏览器原生 `EventSource` 重连会自动带这个头，所以**重连不丢事件**。实时不是「尽力而为」。

事件体是 `store.Change`（`internal/store/sqlite.go`）：

```json
{
  "sequence": 1234,
  "type": "...",
  "resource": "mailbox",
  "resource_id": "...",
  "operation": "...",
  "payload": { },
  "created_at": "..."
}
```

**`resource` 是界面唯一该关心的字段**，它决定哪个页面要刷新。

### 前端

`frontend/src/composables/useRealtime.js`：

- 全局**单例** `EventSource`，由 `layouts/AppLayout.vue` 在 `onMounted` 建连、`onBeforeUnmount` 断连；
- `subscribeRealtime(resources, cb)` 注册监听，**按 `change.resource` 过滤**；`resources` 传 `null` 表示不过滤；
- 按 `sequence` 去重，重复或更旧的丢弃；
- **单个回调抛异常不影响连接**（源码注释：「单个页面刷新失败时保留实时连接，页面仍会由低频轮询兜底」）；
- `realtimeState.status` 驱动顶栏那个「实时已连接／连接中／重连中／已断开」徽标。

### 各页面订阅了哪些 resource

| 文件 | 订阅的 resource | 兜底轮询 |
|---|---|---|
| `views/DashboardView.vue` | `scheduler, event, mailbox, message, apple-account` | 30s × 2 |
| `views/AppleAccountsView.vue` | `apple-account, apple-session` | — |
| `views/GoogleAccountsView.vue` | `google-session, apple-account, mailbox` | — |
| `views/MicrosoftAccountsView.vue` | `microsoft-session, apple-account, mailbox` | 登录轮询 |
| `views/ImapAccountsView.vue` | 全部（`null`） | — |
| `views/MailboxesView.vue` | `mailbox, mailbox-lease, message, apple-account, apple-mail-cleanup, mailbox-message-sync` | 30s |
| `views/SettingsView.vue` | `settings, mailwatcher, apple-session, apple-account, mailbox` | 30s |
| `views/TasksView.vue` | `scheduler, apple-account, create-settings` | 30s |
| `views/ExportsView.vue` | `settings` | — |
| `components/AccountInboxPanel.vue` | 全部 | — |

**改页面生命周期时必须保留 `realtimeUnsubscribe()`**（都在 `onBeforeUnmount` 里调用）。删掉就是监听泄漏——切十次页面攒十个回调。

**实时只是「更快」，不是「唯一」。** 好几个页面另有 30 秒兜底轮询，断线时界面仍会自己更新。设计时不要把「实时连接正常」当作界面可用的前提。

---

## 4. 页面 → 接口对照表

路由在 `frontend/src/router/index.js`。`meta.title` / `meta.subtitle` 决定顶栏标题，**改标题改这里**。

下表是 `grep` 各视图里真实的 `api('/api/...')` 调用点得到的，不是按页面名字猜的。

| 路由 | 视图 | 调用的接口 |
|---|---|---|
| `''` 控制台 | `DashboardView.vue` | `GET /api/tasks`、`GET /api/events`、`GET /api/dashboard`、`POST /api/events/clear` |
| `apple-accounts` | `AppleAccountsView.vue` | `GET /api/apple-accounts`、`DELETE`/`GET /api/apple-accounts/{id}`、`POST /api/apple-accounts/login/start`、`login/2fa`、`{id}/check`、`{id}/imap`、`{id}/mailboxes`、`{id}/mailboxes/sync` |
| `microsoft-accounts` | `MicrosoftAccountsView.vue` | `GET /api/microsoft-accounts`、`POST login/start`、`login/poll`、`PUT /api/microsoft/settings`、`GET/DELETE {id}`、`{id}/check`、`{id}/aliases`、`{id}/aliases/import`、`{id}/aliases/import-primary`、`{id}/aliases/generate-batch`、`{id}/mailboxes`，另用 `GET /api/mailboxes/{id}/messages`、`DELETE /api/mailboxes/{id}?local_only=true` |
| `google-accounts` | `GoogleAccountsView.vue` | `GET /api/google-accounts`、`GET /api/google/status`、`PUT /api/google/settings`、`POST login/start`、`GET/DELETE {id}`、`{id}/check`、`{id}/aliases`、`{id}/aliases/import`、`{id}/aliases/generate`、`{id}/aliases/generate-batch`、`{id}/mailboxes` |
| `qq-accounts` / `mail-163-accounts` / `gmail-accounts` | `ImapAccountsView.vue`（**同一个组件**，靠 `meta.preset` 区分） | `GET /api/imap-accounts`、`POST /api/imap-accounts`、`batch`、`sync-batch`、`pool-batch`、`GET /ids`、`GET/DELETE {id}`、`{id}/addresses`、`{id}/sync`，另 `POST /api/mailboxes/{id}/pool`、`{id}/backfill` |
| `mailboxes` 邮箱池 | `MailboxesView.vue`（最大，约 109 KB） | `GET /api/mailboxes`、`{id}`、`{id}/messages`、`{id}/messages/{mid}`、`{id}/code`、`POST {id}/status`、`{id}/sync`、`{id}/backfill`、`{id}/remote-clean`、`DELETE {id}`、`POST /api/mailboxes`、`import-external`、`resolve`、`sync-messages`、`GET /api/mailboxes/sync-messages/status`、`GET /api/apple-mail/cleanup/status`、`POST /api/apple-mail/cleanup`、`GET /api/dashboard` |
| `tasks` 创建隐私邮箱 | `TasksView.vue` | `GET/PUT /api/create-settings`、`GET /api/scheduler/status`、`POST /api/scheduler/start`、`stop`、`logs/clear`、`GET /api/apple-accounts`、`POST /api/apple-accounts/{id}/mailboxes` |
| `exports` 本地导出 | `ExportsView.vue` | `GET /api/settings`（导出本身是 `GET /api/runtime/export*` 文件下载） |
| `settings` 系统设置 | `SettingsView.vue` | `GET/PUT /api/settings`、`POST /api/server-chan/test`、`GET /api/database/status`、`POST /api/database/{backup\|check\|optimize}`、`GET /api/update/status`、`POST /api/update/apply` |
| `login`（公开） | `LoginView.vue` | `GET /api/auth/status`、`POST /api/auth/setup`、`POST /api/auth/login` |
| `email-code`（公开） | `PublicCodeView.vue` | **`/api/v1/public-code*`**（对外那族） |
| 其它 | `NotFoundView.vue` | — |

完整路由表在 `internal/httpapi/server.go` 的 `routes()`（180+ 个端点，含本表未列的后台接口）。

---

## 5. 样式系统

### 技术栈

Tailwind CSS **v4**，通过 `@tailwindcss/vite` 插件接入（不是老的 PostCSS 配置）。入口 `frontend/src/style.css`（约 5100 行），第一行 `@import "tailwindcss";`。

**没有 `tailwind.config.js`**——v4 不用它，配置写在 CSS 里。

图标库 `@lucide/vue`。

### 暗色模式

```css
@custom-variant dark (&:where(.dark, .dark *));
```

暗色由 `<html>` 上的 **`.dark` 类**切换，不是 `prefers-color-scheme`。切换在 `layouts/AppLayout.vue` 的 `applyTheme()`：改 class + 存 `localStorage['ipm_v2_theme']`。首次进站读 localStorage，没有就跟随系统。

### 设计令牌（CSS 变量）

亮色在 `:root`，暗色在 `.dark`。**这是改视觉最该动的地方**：

| 变量 | 亮色 | 暗色 |
|---|---|---|
| `--app-bg` | `#f8fafc` | `#020617` |
| `--app-surface` | `#ffffff` | `#0f172a` |
| `--app-surface-soft` | `#f8fafc` | `#111c30` |
| `--app-text` | `#0f172a` | `#f1f5f9` |
| `--app-muted` | `#64748b` | `#94a3b8` |
| `--app-line` | `#e2e8f0` | `#1e293b` |
| `--app-primary` | `#10b981` | `#34d399` |
| `--app-primary-dark` | `#047857` | `#6ee7b7` |
| `--app-sidebar` | `#ffffff` | `#0b1220` |
| `--app-shadow` | `0 10px 28px rgb(15 23 42 / 5.5%)` | `0 18px 42px rgb(0 0 0 / 25%)` |
| `--control-height` | `36px` | — |
| `--control-radius` | `10px` | — |

字体栈也在 `:root`：`Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif`。

### 两种写法并存（重要）

模板里**同时**存在两套：

1. **语义类名**，定义在 `style.css` 里：`primary-button`、`secondary-button`、`app-shell`、`main-shell`、`topbar`、`topbar-actions`、`page-scroll`、`realtime-mode`、`profile-menu` 等。
2. **Tailwind 工具类内联在模板上**，例如 `SettingsView.vue` 里到处是 `text-sm`、`rounded-xl`、`dark:bg-slate-800`、`tracking-[0.14em]`。

**不要只在 `style.css` 里改了语义类就以为覆盖了全部**——同一处视觉可能有内联工具类在覆盖它。反过来，`--app-*` 变量被大量复用，改令牌的收益远大于逐个改类名。重构时要么统一，要么明确边界。

---

## 6. 全局骨架

`layouts/AppLayout.vue`：

```
app-shell
├── Sidebar（+ ThemeToggle、移动端抽屉，Esc 关闭）
└── main-shell
    ├── topbar
    │   ├── 面包屑：写死的「控制台 /」+ 路由 meta.title
    │   ├── 副标题：meta.subtitle
    │   ├── 实时状态徽标（realtimeState.status 驱动）
    │   ├── 「本地模式」标签
    │   ├── ThemeToggle
    │   ├── AnnouncementCenter
    │   └── 管理员菜单（用户名 + 退出登录）
    └── page-scroll
        └── <RouterView> + <Transition name="page" mode="out-in">
```

- 路由守卫**每次导航**都调一次 `loadAuthStatus()`；未登录跳 `/login?redirect=`，已登录访问 `/login` 跳回控制台。
- `document.title` = `` `${meta.title} · MailBasin` ``。
- `scrollBehavior`：有 hash 时平滑滚到锚点（`top: 80`，给固定顶栏留位）。
- **新增页面**要同时改：路由文件、`Sidebar.vue` 导航项、`meta.title/subtitle`。

---

## 7. 设计前必须知道的 8 条约束

1. **改前端 ≠ 刷新页面。** 构建 → 同步到 `internal/webui/dist` → 重编译 Go → 重启服务，四步缺一不可。
2. **`internal/webui/dist` 被 git 跟踪。** 你重建一次，它就进 diff。**不要手改**——它是产物。
3. **改过的已跟踪文件会挡住「一键更新」。** 更新器的脏检查是 `git status --porcelain --untracked-files=no`：**未跟踪的新文件不算，改过的已跟踪文件算**。做设计做到一半，一键更新会被拒绝（提示里会列出是哪些文件）——**这是故意的，防止覆盖你的工作**。做完提交即可。
4. **更新页面必须在后端消失约 6.4 秒的情况下活下来。** 一键更新会停掉服务换二进制。那几秒页面联系不上后端（`api()` 会抛「连接服务器失败」）。而且更新进度卡片的数据**来自磁盘上的状态文件**（`<data_path 所在目录>/run/update-status.json`），不是当前请求的状态——所以刷新页面也能看到结果。设计这个页面时要当成「后端可能随时短暂消失」来做。
5. **只通过 `scripts\server.cmd start|stop|restart|status` 管服务。** 不要自己起第二个 `ipm-server.exe`，端口和数据库会打架。
6. **没有国际化层。** 所有界面文案是模板里硬编码的中文。不存在翻译文件或 i18n key。
7. **暗色不是媒体查询**，是 `<html>.dark`。任何新加的暗色样式都要用 `dark:` 变体或 `.dark` 作用域。
8. **没有默认账号。** 首次打开要走 `/api/auth/setup` 建本地管理员。要用脚本驱动界面，得先有会话（见下节）。

---

## 8. 怎么验收界面改动

这套环境里验证 UI 的做法是**用 CDP 驱动真实 Chrome**，不是看代码猜：

1. Chrome 开调试端口（本机用 `9333`）；
2. `PUT http://127.0.0.1:9333/json/new?about:blank` 开新页；
3. 连 `webSocketDebuggerUrl`，然后 `Network.setCookie` 塞会话、`Page.navigate`、`Runtime.evaluate` 断言、`Page.captureScreenshot` 存证。（Node 24 自带 `WebSocket` 和 `fetch`，不用装包。）

**会话怎么来**：往 `data/app.db` 的 `web_sessions` 表插一行临时会话，cookie 值就是 token；会话 ID 是 `base64url(sha256(token))` 去掉填充。这台机器上现成的脚本在 `D:\DSHXM\iCloud\ui-verify\`（`create-session.py` / `drop-session.py`）——**注意这个目录在仓库之外**，clone 下来是没有的。

两条这一轮吃过的亏，值得照做：

- **断言必须会红。** 只跑一次看到绿等于没验。「这行提示出现了」这种断言，要做一次对照臂：把产品代码改坏、重建、确认断言确实变红。本轮就有一条断言在整页文本里找关键词，而那个词在别处本来就有——产品坏掉它也绿，白验一轮。
- **截图和结论必须是真实存在的文件。** 先看文件大小和时间戳，再谈内容。

---

## 9. 已知遗留（不是 bug，是取舍）

- **一键更新的确认条只写到 `origin/main`，写不出真实的「从 X 到 Y」。** 因为 `/api/update/status` 只给 `current.commit`（正在运行的），`run.from_commit`/`to_commit` 是**上一次**运行的结果，不是预报。要显示真实目标，得让更新器 fetch 后把目标写进状态、或加一个预览接口——**属于后端改动**。
- **从老版本升上来、且 `data_path` 非默认值的人，第一次更新的进度卡片会是空的。** 那次跑的是**旧** `update.ps1`（PowerShell 启动时就把脚本读进内存了），旧版把状态写死在 `<repo>\data\run`，而新版服务从 `filepath.Dir(data_path)\run` 读。更新本身正常完成，只是那一次看不到进度；下一次恢复正常。默认 `data/app.db` 不受影响。
- **锁文件**：仓库用 npm 的 `package-lock.json` 钉版本（已跟踪，是唯一凭据）。`pnpm-lock.yaml` 是构建副产品，已忽略。**统一用 npm**——混用会让新依赖进不了被跟踪的那份锁文件。重做界面本身不受影响，受影响的只有「装依赖」这一个动作。

---

## 附：想快速上手，按这个顺序读

1. `frontend/src/router/index.js` —— 有哪些页面
2. `frontend/src/layouts/AppLayout.vue` —— 全局骨架和主题切换
3. `frontend/src/api/client.js` —— 所有请求的约定（41 行，先读完）
4. `frontend/src/composables/useRealtime.js` —— 实时怎么走（67 行）
5. `frontend/src/style.css` 前 70 行 —— 令牌和暗色机制
6. `internal/httpapi/server.go` 的 `routes()` —— 后端到底有哪些接口
