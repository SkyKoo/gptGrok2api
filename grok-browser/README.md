# CFM Grok Browser Worker

这是 CFM 的内部浏览器 worker，负责在隔离的 Chromium/Xvfb 进程中运行官方 Turnstile widget。CFM 主进程继续负责邮箱收码、任务、日志、协议请求和账号保存。

`POST /v1/turnstile/session` 返回短期 token、浏览器 Cookie 快照和会话编号；协议注册使用 Go 的 `CreateEmailValidationCode` gRPC 发码；`POST /v1/session/close` 用于释放仍保留的浏览器会话。`POST /v1/sso/materialize` 只用于把协议响应中的 wrapper SSO 转成会话 SSO。

Turnstile 只使用目标注册页上的真实 widget；worker 不提供 synthetic route fixture、独立 Castle token 或浏览器内账号创建接口。协议发码、验证码验证和最终 Server Action 保持在 Go 的同一协议客户端中，浏览器会话只用于 Turnstile、Cookie/UA 绑定和必要的 SSO materialize。

worker 只应通过 Docker 内网访问，注册任务、邮箱验证码和密码仍由 CFM 管理；密码不会发送给 worker，也不会写入 worker 状态。

本服务只应通过 Docker 内网访问，可用 `GROK_BROWSER_TOKEN` 增加 Bearer 认证。`TURNSTILE_HEADLESS=0` 时通过 Xvfb 运行 headed Chromium，用户不会看到窗口。

每个注册任务会把保存的浏览器 profile 中的语言与时区传给 worker；这样 `Accept-Language` 和 JS 时区跟随账号环境，而不是使用全局固定值。独立调试 worker 时，也可以通过 `GROK_BROWSER_LOCALE` 和 `GROK_BROWSER_TIMEZONE` 覆盖。
