# CFM Grok Browser Worker

这是 CFM 的内部浏览器 worker，负责在隔离的 Chromium/Xvfb 进程中运行 xAI 原生邮箱注册页面。CFM 主进程继续负责邮箱收码、任务、日志、重试和账号保存。

`POST /v1/session/start` 启动持久页面；`/send-email` 点击原生邮箱注册并发起验证码；`/verify-email` 填写验证码；`/signup` 在同一页面完成 Turnstile、Castle 和账号创建并返回 SSO；`/v1/session/close` 释放会话。

Turnstile 和 Castle 只使用目标注册页的真实状态；worker 不提供 synthetic route fixture 或独立 token。旧的 `/v1/turnstile/session` 仅用于兼容诊断，注册主链路使用持久原生页面。

worker 只应通过 Docker 内网访问，注册任务和邮箱验证码仍由 CFM 管理；密码只在最终页面操作时短暂传给 worker，不写入 worker 日志或持久状态。

本服务只应通过 Docker 内网访问，可用 `GROK_BROWSER_TOKEN` 增加 Bearer 认证。`TURNSTILE_HEADLESS=0` 时通过 Xvfb 运行 headed Chromium，用户不会看到窗口。

每个注册任务会把保存的浏览器 profile 中的语言与时区传给 worker；这样 `Accept-Language` 和 JS 时区跟随账号环境，而不是使用全局固定值。独立调试 worker 时，也可以通过 `GROK_BROWSER_LOCALE` 和 `GROK_BROWSER_TIMEZONE` 覆盖。
