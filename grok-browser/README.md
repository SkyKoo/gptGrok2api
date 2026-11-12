# CFM Browser Worker

这是 CFM 的共享浏览器 Worker，运行 Grok 原生注册、xAI 授权和 Adobe Firefly 注册/会话维护。Go 主进程负责邮箱收码、任务、日志和账号保存。Compose 服务与容器默认名为 `cfm-browser-worker`；源码目录保留 `grok-browser/`，旧 DNS 名作为兼容别名。

`POST /v1/session/start` 启动持久页面；`/send-email` 点击原生邮箱注册并发起验证码；`/verify-email` 填写验证码；`/signup` 在同一页面完成 Turnstile、Castle 和账号创建并返回 SSO；`/v1/session/close` 释放会话。

Turnstile 和 Castle 只使用目标注册页的真实状态；worker 不提供 synthetic route fixture 或独立 token。旧的 `/v1/turnstile/session` 仅用于兼容诊断，注册主链路使用持久原生页面。

worker 只应通过 Docker 内网访问，注册任务和邮箱验证码仍由 CFM 管理；密码只在最终页面操作时短暂传给 worker，不写入 worker 日志或持久状态。

本服务只应通过 Docker 内网访问，可用 `CFM_BROWSER_WORKER_TOKEN` 增加 Bearer 认证（兼容 `GROK_BROWSER_TOKEN`）。`TURNSTILE_HEADLESS=0` 时通过 Xvfb 运行 headed Chromium，用户不会看到窗口。

每个 Grok 注册任务都有自己的浏览器环境记录；它不会复用 GPT 的 Cookie、登录会话或页面状态，但会使用相同的 profile 数据结构保持浏览器身份字段一致。CFM 会把该任务的非敏感字段（UA、语言、时区、窗口尺寸和 DPR）传给 worker，worker 只应用经过筛选的字段，不接收账号密码、Cookie 或验证码。

正式 Compose 配置中，app 和 worker 都加入 `cfm-warp` 网络。这样 worker 与 app 使用同一个代理出口，避免 worker 因无法解析代理服务名而直连 xAI。语言统一在启动层设置为 en-US；时区优先使用任务 profile，缺省时可通过 `GROK_BROWSER_TIMEZONE` 设置。

页面未进入验证码或注册表单时，worker 只返回脱敏诊断：当前页面 URL（去掉查询参数）、标题、HTTP 状态、是否检测到 Cloudflare 挑战或代理连接失败，以及稳定的 `reason`。不会把页面 HTML、Cookie、挑战值或邮箱内容写入日志。

原生注册页面使用已安装 Chromium 自身的 User-Agent，避免把 Go 协议客户端的 Chrome 131 UA 覆盖到更新的浏览器上，造成 UA 与 Client Hints 版本冲突。旧协议调用仍可使用保存的 UA；原生注册通过独立的 `native_browser_context_kwargs()` 使用 `no_viewport=True`，不在上下文层覆盖 UA、locale、视口或 DPR；启动层仍设置 en-US 语言并应用任务时区和代理。协议调用的 profile 行为由原 helper 保持。

2026-10-31 本地官方诊断页对照：同一 CloakBrowser 155 进程中，默认上下文手工通过，只增加 `locale="en-US"` 后报 600010。该结果支持取消上下文层的重复语言覆盖，但不说明英语本身不可用，也不证明所有浏览器版本均受影响。原生窗口配置沿用已通过的对照组合。2026-11-01 同进程返回对照再次通过；修复镜像恢复 CFM 页面捕获脚本后诊断页通过，随后单次实际注册的发码、邮箱验证和创建账户接口均返回 HTTP 200，并取得 SSO。注册阶段的安全验证由 worker 流程完成。这是本地单次验证结果，不代表长期或批量注册成功率。

Turnstile 状态读取优先使用页面的 `getResponse(widgetId)`；未捕获 widget ID 时直接调用无参 `getResponse()`，指定 ID 失效时再尝试默认 widget。也检查开放 Shadow DOM 中的响应字段，并通过 Playwright frame tree 补全封闭 Shadow DOM 内 iframe 的数量。诊断只包含数量和布尔值，不包含 token。回调捕获持续到页面关闭，避免人工收码超过两分钟后漏掉新 widget。单次复选框探测受剩余等待时间限制；最终提交仍要求页面已经生成真实 token。

本地回归：`PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s grok-browser -p 'test_*.py'`。JavaScript 回归需要 Node.js；没有 Node.js 时会显示跳过，不能视作全部验证完成。离线用例只验证读取和诊断逻辑，实际注册是否成功仍须通过真实页面验证。

原生注册仅在挑战 iframe 内出现可见复选框时点击该控件；处于 `Verifying` 状态时继续等待，不按 iframe 固定偏移盲点。Cloudflare 的 PAT 401 和部分挑战子域名探测失败不能单独作为根因，参见[官方排障说明](https://developers.cloudflare.com/cloudflare-challenges/troubleshooting/challenge-solve-issues/)。

当原生挑战把复选框放在封闭 Shadow DOM 中，普通选择器不可见时，通过 Chromium CDP 只读定位真实复选框、检查可见性并换算 frame 坐标后点击；找不到控件时继续等待，不退回固定偏移点击。该逻辑不会创建 widget 或生成响应值。

部署使用已通过本地端到端验证的 `cloakbrowser==0.6.4`。新版内核需要通过只读许可文件提供授权，免费许可仅允许单会话；供应方可能更新当前内核，因此发布后仍须检查实际版本。

### 注册完成判定

原生注册提交后，worker 同时检查可信 HTTPS 目的域名和适用于当前页面的非空
`sso` / `sso-rw` Cookie。账户页、账户欢迎页以及已登录的 `grok.com` /
`www.grok.com` 首页（包括资料引导页）均可进入凭据提取；仍显示完成注册表单时继续等待。
仅跳转到首页、出现 Sign Out 或 Welcome 文案不代表注册成功。后续 CFM 仍执行既有的凭据验证。
等待超时的诊断只记录目的页类型和 SSO 是否存在等布尔值，不记录 Cookie 值。
历史超时任务若已关闭浏览器且未保存凭据，不能据此补写成功状态，应先确认账号是否已创建。


### 邮箱注册入口与错误诊断

邮箱注册入口在30秒总窗口内等待页面绑定点击事件，使用正常点击并以邮箱输入框实际出现作为成功条件；延迟出现的隐私弹窗会重新检查，不强制点击穿透。不会为了诊断重新加载页面、提交邮箱或发送验证码。若页面脚本未就绪或入口仍未打开，保留 `email_input_missing`，诊断附带有限的交互状态、点击次数/错误类别，以及第一方脚本的待加载数量和耗时；不保存脚本URL、查询参数、事件函数或输入值。

整页导航和登录提示不再用于判断“邮箱已注册”；仅在提交后依据明确上游业务码或邮箱表单的错误反馈判断。`account:email-already-exists` 仍映射为409，注册不可用仍为422；入口等待超时仍为504，不能据此认定邮箱已使用。

## 后台 xAI OAuth

`POST /v1/oauth/authorize-device` 接收 CFM 后台发来的设备码、所选账号 SSO、预期邮箱和该账号日常出口，在新的隔离浏览器会话内访问 xAI 设备确认页并完成一次授权确认。接口沿用 Worker Bearer 认证和内部网络边界，正常流程不会打开用户浏览器。设备码的申请、Token 查询、邮箱核对和加密持久化由 Go app 负责。

只接受 `https://accounts.x.ai/oauth2/device` 与本次 `user_code`，会话 Cookie 仅绑定 accounts.x.ai 和 auth.x.ai。跳转到登录、MFA 或人机验证页时返回 `manual_action_required`；不自动创建账号、输入密码或求解挑战。确认点击超时返回 `uncertain`，由 Go 查询同一设备授权结果，不重放确认。浏览器异常只返回固定错误码，不打印 Cookie、设备链接或页面内容。每个请求有时限，并关闭临时上下文。

离线测试：`python3 grok-browser/test_oauth_device.py -v`（从仓库根执行）；使用模拟页面，无需真实 SSO。Go 的 `internal/oauth` 与 `internal/httpapi` 测试覆盖 Token 身份校验、队列去重、恢复、取消和秘密字段不泄漏。实际 xAI 登录态与授权页的端到端验收需单独授权真实测试账号。

HTTP(S) 代理网关允许空路径或根路径 `/`。Go 的 Resin 出口和 Worker 浏览器启动入口会在使用时去掉根路径，避免 Chromium 的认证代理参数触发 `ERR_NO_SUPPORTED_PROXIES`；保存的配置和既有任务 endpoint_hash 不迁移。Worker 拒绝带子路径、查询参数或片段的 HTTP(S) 代理，返回脱敏的 `proxy_configuration_invalid`（HTTP422）；CFM 将该错误归为需要人工修正，不自动补偿。此修复只保证代理参数可被浏览器接受，不代表 Resin 上游节点或完整注册流程已经通过验证。


## Adobe 注册和会话维护

Python/FastAPI 与 Node/CloakBrowser 驱动处于同一个镜像，统一由 `BrowserQueue`
创建和关闭浏览器；当前许可并发保持 1。Adobe 不另起独立 Worker 容器，Grok 与
Adobe 不共享登录 Cookie 或账号目录。Node 子进程失去应答后禁止继续发送业务动作，
浏览器及子进程关闭确认前不释放队列容量。

`POST /v1/browser/reservations` 通过 `provider=adobe` 创建会话，`profile_id` 是受校验的
任务标识；`POST /v1/adobe/run` 接受 `run`、`verify`、`resume` 和 `refresh`。注册的创建和
邮箱验证分别有持久提交标记，结果未知不再次提交。遇到持续人机验证、手机号或未识别
表单时结束本次流程。CFM 使用 Adobe 发件人域过滤邮箱验证码。

Adobe 初始化通过独立进度帧报告浏览器启动、账户页加载、创建入口等待、注册页打开和
注册表单准备。预约状态的 `startup_events` 保留至多 32 条固定阶段事件及毫秒耗时；
CFM 按事件序号去重，记录步骤开始、完成和失败，仍保留每秒轮询及取消检查。
“启动浏览器”不再覆盖整个 Adobe 表单准备过程；已有账号的刷新初始化只报告浏览器
启动阶段。整个初始化 RPC 仍使用原有的 150 秒总截止时间，不因收到进度帧续期。
初始化失败仅报告白名单动作、异常类别和时长，不包含原始错误、页面内容、URL 或凭据；
`login_ui_not_ready` 的失败步骤会随注册诊断返回。进度采集不新增上游提交或验证操作。

注册响应返回固定的 `creation_outcome`：`not_submitted`、`rejected`、`accepted` 或 `unknown`。
创建请求账本与可截断的页面诊断分开保存；明确拒绝必须在浏览器关闭且请求结果收齐后返回。
CFM 对明确未提交或已拒绝的失败，用原邮箱、新任务和新 profile 重新执行注册，并设置
`adobe_require_new=true`；只有已受理的创建才沿用原 profile 继续验证。未知结果不默认改走登录。

账号刷新先用 IMS Cookie 协议；浏览器回退读取同账号的持久 profile。SDK 就绪采用
有界只读轮询，核对邮箱、上游账号 ID 及邮件验证状态后才返回 Token。若没有登录态且
保存了密码，回退只提交已有账号登录表单，各表单至多一次；出现 OTP/MFA/人机验证
则停止，不跳到创建账号。完整身份核对仍是返回凭据的必要条件。

私密资料位于挂载的 `./data/browser-worker/adobe/<profile_id>/`：`state.json` 保存
注册恢复状态和必要凭据（0600），`browser/` 保存浏览器登录资料（0700）。Worker 模式
不写独立导入文件或账号截图。目录必须纳入加密备份且不可公开；浏览器缓存、临时
Singleton 锁不属于恢复资料。复制离线 profile 时只能清除复制副本中的陈旧 Singleton
锁，不能操作仍被浏览器使用的原目录。许可/二进制缓存使用 `/data/cloakbrowser`，
不得把许可文件提交到 Git。Go 注册恢复日志为 `adobe_registration_tasks.enc.json`，
其 JSON envelope 中的 `data` 使用 AES-GCM 加密。

2026-11-11 本机验证：共享 Worker 镜像由已有浏览器基础镜像构建；对已注册账号的
离线 profile 副本，通过显式本机代理进行一次成功会话刷新，取得身份匹配的新 Token
和 IMS Cookie，未进行新注册或生成。该次首页 Sherlock 仅包含 sid/ftr，不能视为
完整媒体会话已刷新；后续只读编辑页初始化未完成。实现会返回
`media_session_refreshed=false` 和空媒体 Cookie，保留账户原有完整媒体 Cookie。
已有账号密码登录回退仅离线验证，最后一次实际准备在浏览器启动阶段即失败，未进入
登录表单。刷新后队列容量恢复为 1。这证明当前会话可恢复，
不代表可以无限期免登录，也不能据此推断免费模型额度。

离线验证应在 Docker 内执行，例如挂载源码后运行 `python -m unittest discover -s
/tests -p 'test_*.py'`，以及在 Adobe 目录执行 `node --test *.test.mjs`。测试不需要真实
账号或公网访问。镜像构建可使用 `GROK_BROWSER_BASE_IMAGE` 指定已有兼容 Python
Playwright 镜像以复用缓存；Node 层使用 Playwright 1.65 的 Node 26 运行时。
