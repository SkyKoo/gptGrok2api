# CFM Grok Browser Worker

这是 CFM 的内部浏览器 worker，负责在隔离的 Chromium/Xvfb 进程中运行 xAI 原生邮箱注册页面。CFM 主进程继续负责邮箱收码、任务、日志、重试和账号保存。

`POST /v1/session/start` 启动持久页面；`/send-email` 点击原生邮箱注册并发起验证码；`/verify-email` 填写验证码；`/signup` 在同一页面完成 Turnstile、Castle 和账号创建并返回 SSO；`/v1/session/close` 释放会话。

Turnstile 和 Castle 只使用目标注册页的真实状态；worker 不提供 synthetic route fixture 或独立 token。旧的 `/v1/turnstile/session` 仅用于兼容诊断，注册主链路使用持久原生页面。

worker 只应通过 Docker 内网访问，注册任务和邮箱验证码仍由 CFM 管理；密码只在最终页面操作时短暂传给 worker，不写入 worker 日志或持久状态。

本服务只应通过 Docker 内网访问，可用 `GROK_BROWSER_TOKEN` 增加 Bearer 认证。`TURNSTILE_HEADLESS=0` 时通过 Xvfb 运行 headed Chromium，用户不会看到窗口。

每个 Grok 注册任务都有自己的浏览器环境记录；它不会复用 GPT 的 Cookie、登录会话或页面状态，但会使用相同的 profile 数据结构保持浏览器身份字段一致。CFM 会把该任务的非敏感字段（UA、语言、时区、窗口尺寸和 DPR）传给 worker，worker 只应用经过筛选的字段，不接收账号密码、Cookie 或验证码。

正式 Compose 配置中，app 和 worker 都加入 `cfm-warp` 网络。这样 worker 与 app 使用同一个代理出口，避免 worker 因无法解析代理服务名而直连 xAI。独立调试 worker 时，也可以通过 `GROK_BROWSER_LOCALE` 和 `GROK_BROWSER_TIMEZONE` 覆盖默认值；正式注册仍以任务 profile 为准。

页面未进入验证码或注册表单时，worker 只返回脱敏诊断：当前页面 URL（去掉查询参数）、标题、HTTP 状态、是否检测到 Cloudflare 挑战或代理连接失败，以及稳定的 `reason`。不会把页面 HTML、Cookie、挑战值或邮箱内容写入日志。
