# Grok 纯协议注册

Grok 注册在 Go 注册中心中与 ChatGPT Free 保持两条平行链路。选择 `Grok（协议）` 后，任务、日志、定时启动、邮箱来源和恢复记录仍由注册中心统一管理；注册协议客户端和 Grok 账号归档使用独立实现。

## 注册顺序

每个任务按以下顺序执行：

1. 请求 `accounts.x.ai` 注册页，提取当前 site key、Next.js Server Action 和 router state tree。
2. CFM 调用内网 `grok-browser` worker。worker 在隔离的 Chromium/Xvfb 中运行官方 Turnstile widget，返回短期 token、Cookie 快照和会话编号；CFM 不自行生成 token。
3. CFM 使用同一代理、同一浏览器 profile 和已恢复的 Cookie/UA，调用 `auth_mgmt.AuthManagement/CreateEmailValidationCode` gRPC 发码。注册页的 `/api/auth/send-verification-code` 依赖页面绑定的 Castle step token，不作为协议注册的发码通道。
4. 复用现有 iCloud HME 邮箱来源等待验证码，再调用原生 `VerifyEmailValidationCode`。
5. 提交 Next.js Server Action。创建响应中的 SSO wrapper 会继续经过受信任的 cookie hop，并提取最终 `sso` 会话。
6. 先写入 Grok 任务恢复记录，再写入独立 Grok 账号归档并调用现有 Grok Fast quota 校验。

注册协议客户端和账号创建步骤仍由 Go 发送 HTTP 请求；浏览器 worker 只负责 Turnstile 和会话上下文，不会接收账号密码或提交创建账号请求。OpenAI 的 Sentinel 解码器不进入 Grok 流程。

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
