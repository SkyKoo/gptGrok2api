# CFM Grok Browser Worker

这是 CFM 的内部浏览器 worker，负责在隔离的 Chromium/Xvfb 进程中运行 xAI 原生邮箱注册页面。CFM 主进程继续负责邮箱收码、任务、日志、重试和账号保存。

`POST /v1/session/start` 启动持久页面；`/send-email` 点击原生邮箱注册并发起验证码；`/verify-email` 填写验证码；`/signup` 在同一页面完成 Turnstile、Castle 和账号创建并返回 SSO；`/v1/session/close` 释放会话。

Turnstile 和 Castle 只使用目标注册页的真实状态；worker 不提供 synthetic route fixture 或独立 token。旧的 `/v1/turnstile/session` 仅用于兼容诊断，注册主链路使用持久原生页面。

worker 只应通过 Docker 内网访问，注册任务和邮箱验证码仍由 CFM 管理；密码只在最终页面操作时短暂传给 worker，不写入 worker 日志或持久状态。

本服务只应通过 Docker 内网访问，可用 `GROK_BROWSER_TOKEN` 增加 Bearer 认证。`TURNSTILE_HEADLESS=0` 时通过 Xvfb 运行 headed Chromium，用户不会看到窗口。

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
