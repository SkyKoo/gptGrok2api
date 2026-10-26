# Grok 注册协议流程

Grok 注册在 Go 注册中心中与 ChatGPT Free 保持两条平行链路。选择 `Grok（协议）` 后，任务、日志、定时启动、邮箱来源和恢复记录仍由注册中心统一管理；注册协议客户端和 Grok 账号归档使用独立实现。

## 注册顺序

每个任务按以下顺序执行：

1. CFM 为每次注册创建新的浏览器 profile、HTTP 客户端和 Grok browser session，并通过同一代理打开 `accounts.x.ai/sign-up?redirect=grok-com`。
2. `grok-browser` 在持久 Chromium 页面中点击原生“Sign up with email”，填写邮箱并提交。Castle 请求上下文、页面 Cookie 和原生注册状态因此属于同一会话。
3. 复用现有 iCloud HME 邮箱来源等待验证码，再由同一浏览器页面填写并提交验证码。
4. 浏览器页面填写姓名和密码，运行页面自己的 Turnstile widget，提交“Create account”，并在同一会话中等待最终 `sso`/`sso-rw` Cookie。
5. Go 注册引擎继续负责任务状态、阶段日志、邮箱收码、重试和账号归档；worker 只负责隔离的浏览器页面生命周期。
6. 先写入 Grok 任务恢复记录，再写入独立 Grok 账号归档并调用现有 Grok Fast quota 校验。

Grok 的账号创建请求由 worker 在原生页面中提交，因为 Castle/Turnstile 与页面 React 状态绑定；密码只在一次请求期间传给内网 worker，不写入 worker 日志或持久状态。OpenAI 的 Sentinel 解码器不进入 Grok 流程。

## Docker 配置

`docker-compose.go.yml` 和 `docker-compose.yml` 会启动一个 `grok-browser` 服务，默认只暴露 Docker 内网 `8899` 端口。CFM app 通过 `CFM_GROK_BROWSER_URL=http://grok-browser:8899` 调用它；本地调试也可以把该地址改成可访问的 worker 地址。

worker 的可选环境变量：

- `GROK_BROWSER_HEADLESS=0`：通过 Xvfb 运行 headed Chromium，用户不会看到窗口。
- `GROK_BROWSER_CONCURRENCY=1`：浏览器并发槽位。
- `GROK_BROWSER_TOKEN`：启用后，CFM 的 `grok.api_key` 作为 Bearer token 发送。
- 注册任务会把账号 profile 中的语言和时区传给 worker；独立调试时可用 `GROK_BROWSER_LOCALE` 和 `GROK_BROWSER_TIMEZONE` 临时覆盖。

注册请求和 worker 必须使用同一代理出口。Token 是短期且单次使用的，worker 返回的 Cookie 仅用于当前注册任务，不写入日志。

## 配置

注册目标选择 `Grok（协议）`，邮箱来源保持与 GPT 相同：启用且只启用一个 `icloud_hme` provider。`grok` 配置字段如下：

| 字段 | 含义 |
| --- | --- |
| `provider` | 固定为 `browser`。 |
| `api_base` | `grok-browser` worker 地址，Docker 默认 `http://grok-browser:8899`。 |
| `api_key` | worker 的可选 Bearer token。 |
| `request_timeout` | 注册协议 HTTP 请求超时，10–180 秒。 |
| `captcha_timeout` | worker 获取 Turnstile 的超时，30–900 秒。 |

## 任务与账号管理

- Grok 任务记录保存在注册配置目录的 `grok_registration_tasks.json`，凭据恢复字段只写入该文件，不通过任务快照返回。
- Grok 账号保存在独立账号文件；账号管理页通过 Grok 标签页读取，公共列表会脱敏密码和 SSO。
- `/api/register/grok/jobs` 查看历史任务；`POST /api/register/grok/retry-registration` 复用已经创建的 HME 邮箱重试协议注册；`POST /api/register/grok/retry-result` 只重试已经拿到凭据但入库或校验未完成的结果。
- `target=openai` 时仍只启动原有 `FreeEngine`；`target=grok` 时只启动 `GrokEngine`，两条链路不会共用任务日志或账号文件。

## SSO 处理

Server Action 可能先返回包含 `config.success_url` 的 wrapper JWT，而不是最终会话。CFM 会先检查 Set-Cookie 和响应 JSON，再沿受信任的 `auth.grokusercontent.com`、`auth.x.ai`、`accounts.x.ai` 和 `grok.com` hop 跟随 wrapper；最终只接受不含 `success_url`/配置 token 的会话 JWT。若上游返回既没有 cookie、又没有可跟随 wrapper 的响应，任务会停在 `sso: missing_sso`，不会把 wrapper 当作可用账号凭据保存。
