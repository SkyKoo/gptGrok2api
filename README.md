# GPTGrok2API Go

GPTGrok2API Go 是一个自托管的 OpenAI 兼容网关。它使用 Go 运行时接入 ChatGPT/OpenAI JWT 账号池、Grok SSO/OAuth 账号池和代理出口，并提供账号调度、图片任务、文件存储、实时监控和 Web 管理控制台。

当前发布版本：<code>v1.2.4-go</code>  ·  [GitHub Releases](https://github.com/lichao199208/gptGrok2api/releases)

> 本项目通过逆向研究接入 ChatGPT 和 Grok 网页能力，不是 OpenAI 或 xAI 官方服务。上游协议、账号策略和可用模型随时可能变化。请使用你有权使用的账号，遵守相关服务条款和当地法律，并自行承担账号、网络和内容风险。

## 能力概览

- OpenAI 兼容接口：Chat Completions、Responses、Anthropic Messages、搜索、图片、视频和可编辑文件任务。
- ChatGPT Web 多模态聊天：通过 Chat Completions / Responses 接收文字和参考图，返回文字或实时 SSE；支持范围与示例见 [多模态聊天](docs/multimodal-chat.md)。
- OpenAI 图片：<code>gpt-image-2</code> 文生图、图生图和多参考图编辑。
- Grok：文本、Grok Imagine 图片、图片编辑、视频，以及 Console/Thinking 模型。
- 多账号池：JWT、OAuth refresh token、Grok SSO/OAuth、账号分组、失败换号、限流冷却和并发调度。
- 代理出口：默认代理、代理池、代理组、订阅导入、节点健康检测和图片任务专用并发限制。
- 管理控制台：账号、代理、图片图库、日志、实时请求、提示词、注册任务、备份和系统设置。
- 本地持久化：账号和配置使用 JSON 文件，队列可使用 JSON 或 Redis，图片和视频保存在 <code>data/files/</code>。

## 架构

~~~mermaid
flowchart LR
  Client[兼容 API 客户端] --> API[/v1 API]
  Admin[管理员 / Web 控制台] --> Console[/api 管理接口]
  API --> Core[Go 网关与账号调度]
  Console --> Core
  Core --> ChatGPT[ChatGPT Web API]
  Core --> Grok[Grok Web / Console API]
  Core --> Proxy[代理出口与健康检测]
  Core --> Files[data/files 图片和视频]
  Core --> Queue[JSON 或 Redis 队列]
  Gateway[可选图片队列网关] --> Queue
  Gateway --> API
~~~

## Docker 快速开始

要求 Docker Engine 24+、Docker Compose v2，以及能够访问 ChatGPT/Grok 的网络出口。

~~~bash
git clone https://github.com/lichao199208/gptGrok2api.git
cd gptGrok2api
cp .env.example .env
mkdir -p data logs
test -f data/auth_keys.json || printf '{"items":[]}\n' > data/auth_keys.json
docker network inspect gptgrok2api_default >/dev/null 2>&1 || docker network create gptgrok2api_default
~~~

编辑 <code>.env</code>，至少设置两个不同的随机密钥：

~~~dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-api-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
CHATGPT2API_GO_PORT=3000
GO_PUBLIC_BASE_URL=http://your-server:3000
~~~

启动 Go 服务、Redis 和图片队列网关：

~~~bash
docker compose -f docker-compose.go.yml up -d --build
docker compose -f docker-compose.go.yml ps
curl -fsS http://127.0.0.1:3000/health
~~~

管理控制台地址为 <code>http://服务器地址:3000/</code>。图片队列网关默认只监听 <code>127.0.0.1:3001</code>，不应直接暴露到公网；普通客户端直接访问主服务的 <code>/v1</code> 接口即可。

### 服务器 8000 端口

服务器部署使用 Compose 覆盖文件。覆盖文件会把主服务绑定到本机 <code>127.0.0.1:8000</code>，适合由 Nginx 或其他 HTTPS 反向代理对外提供服务：

~~~bash
cp .env.example .env
mkdir -p data logs
test -f data/auth_keys.json || printf '{"items":[]}\n' > data/auth_keys.json
docker network inspect gptgrok2api_default >/dev/null 2>&1 || docker network create gptgrok2api_default
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
~~~

服务器 <code>.env</code> 示例：

~~~dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-api-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
CHATGPT2API_PORT=8000
GO_PUBLIC_BASE_URL=https://gpt.example.com
GO_VERSION=1.2.4-go
~~~

<code>CHATGPT2API_PORT</code> 是服务器覆盖文件使用的宿主机端口；本地单 Compose 部署使用 <code>CHATGPT2API_GO_PORT</code>。首次接入域名时，请确认反向代理把 <code>/</code>、<code>/v1</code>、<code>/api</code> 和 <code>/images</code> 一并转发到 <code>127.0.0.1:8000</code>。

## 认证和控制台

AI 接口支持以下认证方式：

~~~http
Authorization: Bearer <api-key>
~~~

也兼容 <code>X-API-Key</code>。<code>CHATGPT2API_AUTH_KEY</code> 用于普通 API 调用，<code>CHATGPT2API_ADMIN_KEY</code> 用于管理接口；控制台中创建的用户密钥会保存为哈希，不会以明文写入 <code>data/auth_keys.json</code>。除非明确需要，不要开启 <code>GO_ALLOW_ANONYMOUS</code>。

## API

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| <code>GET</code> | <code>/health</code> | 健康检查 |
| <code>GET</code> | <code>/v1/models</code> | 当前模型目录 |
| <code>POST</code> | <code>/v1/chat/completions</code> | 文本、图片聊天和工具调用 |
| <code>POST</code> | <code>/v1/responses</code> | Responses 兼容接口 |
| <code>POST</code> | <code>/v1/messages</code> | Anthropic Messages 兼容接口 |
| <code>POST</code> | <code>/v1/search</code> | 搜索请求 |
| <code>POST</code> | <code>/v1/images/generations</code> | 文生图 |
| <code>POST</code> | <code>/v1/images/edits</code> | 图生图/图片编辑 |
| <code>GET</code> | <code>/v1/files/image?id=...</code> | 下载生成图片 |
| <code>POST</code> | <code>/v1/videos</code> | 创建视频任务 |
| <code>GET</code> | <code>/v1/videos/{id}</code> | 查询视频任务 |
| <code>POST</code> | <code>/v1/editable-file-tasks</code> | 创建 PPT/PSD 等可编辑文件任务 |
| <code>GET</code> | <code>/files/{path}</code> | 下载可编辑文件产物 |

可用模型以 <code>/v1/models</code> 返回值和账号实际权限为准。当前目录按能力分为 OpenAI GPT 文本、Grok 文本、<code>gpt-image-2</code>/Grok Imagine 图片、Grok Imagine 视频和 Grok Console/Thinking 模型，不建议在客户端硬编码完整模型清单。

### 文本聊天

~~~bash
curl http://127.0.0.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer your-api-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"介绍一下这个项目"}]}'
~~~

## 日志图片预览

日志详情将「输入参考图」和「生成结果」分区展示，参考图按实际提交顺序编号，
并显示原始宽高；重复提交同一图片仍保留各自的位置。同步/异步图片编辑与 OpenAI
多模态聊天保存输入预览，失败调用同样可对照已解析的输入。

参考图保存在 `data/log_input_images/`，只有管理员通过 `/api/logs/input-images/{filename}`
读取，不暴露到公开图片目录。日志只记录关联信息，不保存图片 Base64 或上游签名 URL；
输入预览沿用图片保留期限自动清理，也计入管理页的保留期清理。无法保存或已过期时
显示占位提示，不影响生成请求；旧日志未保存的输入图不能恢复，仍可展示已有尺寸信息。

## 图片生成、编辑与参考图逻辑

### 尺寸语义

GPT 图片的同步与异步生成、编辑统一将省略 `size` 或 `size: "auto"` 视为自动尺寸。
CFM 不再将自动尺寸改成 `1024x1024`，也不追加固定尺寸提示词或向 Web 发送固定尺寸字段。
`auto` 由上游根据提示词与参考图决定构图，不表示严格保留第一张参考图的像素尺寸。
显式 `宽x高` 继续使用现有尺寸提示与兼容映射，但 ChatGPT Web 不保证按指定像素输出；
CFM 保留返回原图，不因尺寸偏差重新生成、缩放或裁剪。Grok 的默认尺寸保持不变。
调用日志的 `request_meta.image_size` 记录请求尺寸、按顺序排列的输入尺寸、上游尺寸字段是否发送、
发送值及实际输出尺寸，便于区分参数和结果。已完成的旧异步任务重复提交仍返回原结果。

### 调用示例

文生图：

~~~bash
curl http://127.0.0.1:3000/v1/images/generations \
  -H 'Authorization: Bearer your-api-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"一只漂浮在太空里的猫","size":"1024x1024","n":1}'
~~~

图片编辑使用 <code>multipart/form-data</code>，可以提交一张或多张 <code>image</code>：

~~~bash
curl http://127.0.0.1:3000/v1/images/edits \
  -H 'Authorization: Bearer your-api-key' \
  -F 'model=gpt-image-2' \
  -F 'prompt=保留主体，把背景改成蓝色' \
  -F 'image=@reference.png;type=image/png'
~~~

同步 `gpt-image-2` 生成与编辑返回 `{"created": <Unix 秒>, "data": [{"b64_json": "..."}]}`，
与官方 GPT Image 的 Base64 返回方式一致；不再返回 `data[].url`。请求请省略
`response_format`，显式传入该参数返回 400（`error.param=response_format`）。
标准 multipart 输入继续支持；CFM 的 JSON `images: ["URL 或 data URL"]` 简写仍是扩展，
不同于官方 JSON 图片引用对象。原有数量和参考图限制保持。
上游没有提供的 token 用量、实际质量等元数据不会伪造返回。Grok 的返回方式不变。

同步 `/v1/images/generations`、`/v1/images/edits` 及 CFM 异步扩展
`/api/image-tasks/generations`、`/api/image-tasks/edits` 支持以下官方命名的输出参数：

| 参数 | 当前 CFM 支持范围 |
| --- | --- |
| `background` | `auto`、`transparent`、`opaque`；省略或 `null` 沿用原有自动行为 |
| `output_format` | `png`、`jpeg`、`webp`；省略或 `null` 默认 `png` |

例如编辑请求可增加 `-F 'background=transparent' -F 'output_format=png'`。
这些参数仅支持 GPT Image 模型。`background` 描述输出背景属性，不代表“去背景”操作；
编辑内容仍由原始 `prompt` 指定。CFM 将透明/不透明要求转换为 Web 上游提示，
不直接转发尚未验证有效的顶层 Web 字段。输出格式由 CFM 在下载后编码实现，
图片实际字节、返回的 `output_format`、下载 Content-Type 和文件扩展名保持一致。
默认输出 PNG，已经是 PNG 的结果保留原始字节；PNG 和无损 WebP 保留 Alpha。
JPEG 不支持透明度，`background=transparent` 与 `output_format=jpeg` 的组合会在生图前返回 400。
JPEG 编码使用质量 100；自动背景模式下若上游仍返回透明像素，合成到白底后编码，避免黑底或黑边。
这属于格式转换，不执行主体识别或去背景；JPEG 是有损格式，不能保证与源图像素完全相同。
WebP 使用纯 Go 的 [nativewebp v1.3.0](https://github.com/HugoSmits86/nativewebp/tree/v1.3.0)
编码，无需 C 库或新增容器，适用于 `CGO_ENABLED=0` 的 ARM64 构建；许可证位于
`third_party/nativewebp/LICENSE` 并随镜像分发。本次没有实现 `output_compression` 控制。

CFM 在保存前检查实际像素：透明输出必须同时包含完全透明和可见像素；
不透明输出不能包含半透明或全透明像素。若上游生成结果不符合要求，同步返回 502、
异步任务标记失败，不自动换号重新生图，也不把该结果存入图片库。
此检查只验证透明属性，不能保证抠图边缘、主体保真度或背景分离质量。
异步任务保存这两个参数并将其纳入重复提交校验；已有未传参任务的校验摘要保持兼容。
GPT Image 编辑现已支持可选 `mask`，详见 [原生蒙版接入](docs/image-mask.md)。
逐步出图及其他未列出的官方能力尚未实现。
协议来源：[OpenAI 图片编辑接口](https://developers.openai.com/api/reference/go/resources/images/methods/edit)。

调用日志直接关联服务器已保存的生成图片，不依赖对外返回 URL。对于历史日志缺少图片地址的记录，
日志管理会按图片元数据中的 `call_id` 补齐预览；只关联仍存在的生成结果，不重写原日志、
不重新生成图片，也不改变图片保留期限。

ChatGPT Web 的完整会话若确认当前回复已结束、只有文字且本轮未调用工具，CFM 会立即
按业务失败结束任务，例如返回“上游未生成图片：请上传需要优化的照片”，不再等到 600 秒超时。
同步接口返回 422；异步任务保存 `status: "error"` 和相同原因，重启查询仍保留。
此判断不作用于流式片段、进行中回复或存在后台工具任务的会话；已得到图片时优先保留生成结果。
明确的审核拒绝、额度等错误优先提取具体说明，不仅显示 `failed` / `blocked` 状态词，
且不会因可重试状态配置而自动重新生图。业务说明仍走下述摘要与脱敏流程。
无需数据迁移或配置；回退二进制后旧错误记录仍兼容，但无图回复会再次等待超时。

ChatGPT Web 明确终止生图时，对外错误消息和页面默认展示保留简短摘要；调用日志的
`raw_error` / `upstream_error` 保存脱敏后的完整原因，日志详情可展开并复制。
异步任务文件也保留 `error_detail`，重启后仍可排查；任务公开查询只返回摘要。
记录前会过滤账号凭据、认证头、Cookie、邮箱、URL 和图片 data URL。单条诊断最多保存
65,536 个字符，超出时明确标记 `error_detail_truncated`，不会静默截断。
此调整不改变失败状态或重试策略；旧日志已丢失的后半段无法恢复。

<code>gpt-image-2</code> 也可以通过 <code>/v1/chat/completions</code> 调用。纯文本 content 是提示词；只有 <code>image_url</code>、<code>input_image</code> 或 <code>image</code> 内容块会被当作参考图输入。

### 异步图片任务

长时间生成可使用 `POST /api/image-tasks/generations`：提交 JSON 中的
`client_task_id`、`model`、`prompt`、`n`、`size`、`quality`，服务先将任务写入
`data/image_tasks/`，返回 HTTP 202 和任务 `id`，后台继续生成。随后用同一 API Key
调用 `GET /api/image-tasks/{id}`，每隔几秒查询一次；`queued`、`running` 为进行中，
`success` 时读取 `data` 图片列表，`error` 时读取 `error` 原因。
`POST /api/image-tasks/edits` 支持 multipart 的同名字段与 `image` / `image[]`。

同一调用者重复提交相同 `client_task_id` 和内容会返回原任务；内容不同返回 409。
调用者之间任务隔离。提交连接断开不会取消后台生成；每次查询都是独立的短请求，
适用于 Cloudflare 等反向代理的读取超时限制。标准 `/v1/images/generations` 和
`/v1/images/edits` 仍为同步接口，客户端需要明确使用上述提交与查询协议。

任务记录不保存 API Key、提示词和输入图片原文。完成状态与结果链接在重启后仍可查询；
由于上游执行没有可恢复的任务句柄，重启时未完成任务会标记为 `error`，不自动重新生成。
任务记录与图片按 `GO_IMAGE_RETENTION_DAYS` 清理，保留期后不再保证去重或结果可读。
当前实现面向单个主服务进程，多个实例不可共享同一个任务目录。

### 图片任务总超时

在管理控制台「设置 → 基础配置」中设置「图片任务总超时」，单位秒，默认 600
（10 分钟），范围 60–900。配置键为 `image_task_timeout_secs`，保存后立即对新任务
生效；正在执行的任务使用提交时的期限。该期限包含排队、上游生成、重试、结果轮询
和下载，适用于图片生成、编辑和图片聊天；异步任务返回 202 后继续使用同一期限。
上游结果轮询使用任务剩余时间，不再受原 6 分钟总上限限制。单次网络请求仍受连接
和传输超时约束。同步接口仍受客户端或反向代理期限限制，长任务应使用异步接口。

原 `image_timeout_retry_secs` 未接入 Go 生图逻辑，已从控制台和新安装配置移除。
已有配置中的旧值可以保留，不会影响新期限。默认 10 分钟适配现有 Cherry ModelScope
轮询窗口；使用其他客户端时还需确保客户端等待时间足够。

### 结果筛选规则

参考图和生成图在 ChatGPT 上游响应中都可能表现为 <code>file-service://</code> 或 <code>sediment://</code> 资产指针，且 SSE 过程中可能先回显上传的参考图。为避免“把参考图当成生成结果”，Go 版按以下规则处理：

1. 请求中的客户端 <code>user</code> 图片只用于上传和生成，不加入结果列表。
2. 轮询 conversation 的 <code>mapping</code> 时，只读取 <code>tool</code> 和 <code>assistant</code> 记录中的图片资产；<code>user</code> 记录全部忽略。
3. 按消息 <code>create_time</code> 排序，并过滤 SSE 回显、输入资产指针和与参考图字节相同的内容。
4. 识别 <code>file-service://</code>、<code>sediment://</code> 以及当前文件 ID 结构，下载真正的生成资产。
5. 生成资产写入本地图片存储；GPT Image 同步接口返回 Base64，异步任务和图库使用本站 URL。

因此，编辑请求的参考图只会参与生成，不会出现在返回的 <code>data</code> 图片列表中。异步任务的结果仍为 URL，例如：

~~~text
http://your-server:3000/v1/files/image?id=<image-id>
~~~

公网部署时请设置 <code>GO_PUBLIC_BASE_URL</code> 为外部 HTTPS 地址。图片文件默认保留 1 天，后台定期清理；可用 <code>GO_IMAGE_RETENTION_DAYS</code> 和 <code>GO_IMAGE_CLEANUP_INTERVAL_SECONDS</code> 调整。

## 账号、重试和代理

### Go ChatGPT Free 注册（实验功能）

注册页支持通过独立部署的 iCloud HME 服务创建邮箱别名、接收验证码，并由 Go 原生执行器
完成单账号注册、自动入库和校验。每次运行 1 个任务，无新增容器；真实上游可用性需单独验收。
配置要求、任务恢复和功能边界见 [Go 注册说明](docs/registration/go-free.md)。

### 账号池行为

- 图片和聊天请求遇到 <code>401</code>、<code>403</code>、<code>429</code>、<code>500</code>、<code>502</code>、<code>503</code>、<code>504</code> 或网络错误时，会排除当前账号并按限制尝试其他账号。
- OAuth 账号优先使用 <code>refresh_token</code> 刷新 access token，并持久化新 token；Go 版不会使用账户密码或 2FA Secret 自动登录生成 token。
- 图片请求默认单账号并发为 <code>1</code>，单进程总并发为 <code>128</code>。代理组会按真实请求的成功率、延迟和并发容量选择节点。
- 代理订阅支持每行一个 HTTP/HTTPS/SOCKS 节点或 Base64 编码列表，自动去重并保留手工节点；节点检测结果会保存到配置。
- 代理、账号 Token、Cookie 和生成文件都属于敏感数据，请只在可信网络中使用，并通过 HTTPS 暴露服务。

## 主要环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| <code>CHATGPT2API_AUTH_KEY</code> | 空 | 普通 API 密钥 |
| <code>CHATGPT2API_ADMIN_KEY</code> | 空 | 管理密钥 |
| <code>CHATGPT2API_GO_PORT</code> | <code>3000</code> | <code>docker-compose.go.yml</code> 宿主机端口 |
| <code>CHATGPT2API_PORT</code> | <code>8000</code> | 服务器覆盖 Compose 的宿主机端口 |
| <code>GO_PUBLIC_BASE_URL</code> | 空 | 图片和文件公开 URL 前缀 |
| <code>GO_CONFIG_PATH</code> | <code>config.json</code> | JSON 配置文件路径 |
| <code>GO_ACCOUNTS_PATH</code> | <code>data/accounts.json</code> | OpenAI 账号文件 |
| <code>GO_AUTH_KEYS_PATH</code> | <code>data/auth_keys.json</code> | 用户密钥文件 |
| <code>GO_QUEUE_BACKEND</code> | <code>json</code> | <code>json</code> 或 <code>redis</code> |
| <code>GO_REDIS_ADDR</code> | <code>127.0.0.1:6379</code> | Redis 地址 |
| <code>GO_REQUEST_TIMEOUT_SECONDS</code> | <code>180</code> | 上游请求超时，范围 10-300 秒 |
| <code>GO_CHAT_MAX_RETRIES</code> | <code>2</code> | 聊天/图片最大重试次数 |
| <code>GO_IMAGE_ACCOUNT_CONCURRENCY</code> | <code>1</code> | 单账号图片并发，范围 1-4 |
| <code>GO_IMAGE_MAX_CONCURRENCY</code> | <code>128</code> | 单进程图片总并发，范围 1-1024 |
| <code>GO_IMAGE_RETENTION_DAYS</code> | <code>1</code> | 本地图片保留天数 |
| <code>GO_IMAGE_CLEANUP_INTERVAL_SECONDS</code> | <code>3600</code> | 图片清理间隔，最少 60 秒 |
| <code>GO_PROXY_URL</code> | 空 | 默认代理 |
| <code>GO_PROXY_POOL</code> | 空 | 逗号分隔的代理池 |
| <code>GO_VERSION</code> | <code>1.2.4-go</code> | 版本标识 |

完整配置项和上游地址见 [<code>.env.example</code>](./.env.example) 与 [<code>config.example.yaml</code>](./config.example.yaml)。运行时配置通过控制台保存到 <code>config.json</code>，不要把真实配置提交到 Git。

## 升级与备份

升级前先备份 <code>data/</code>、<code>.env</code> 和自定义反向代理配置。然后在仓库目录执行：

~~~bash
git fetch --tags user-origin
git checkout v1.2.4-go
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml pull
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
~~~

如果使用本地构建而非服务器覆盖文件，去掉第二个 Compose 文件并把端口检查改为 <code>CHATGPT2API_GO_PORT</code> 对应的端口。不要删除 <code>data/</code>，其中包含账号、密钥、队列和图片。

## 本地开发与测试

Go 版运行不需要 Python 或 Uvicorn：

~~~bash
go run ./cmd/gptgrok2api
go test ./internal/... ./cmd/...
CGO_ENABLED=0 go build -trimpath -o gptgrok2api ./cmd/gptgrok2api
~~~

前端源码在 <code>web-vue/</code>，需要 Node.js 22+：

~~~bash
cd web-vue
npm ci
npm run build
~~~

## 数据安全

以下内容只属于本地运行时数据，不要提交到 Git：

~~~text
.env
config.json
data/
logs/
web_dist/
~~~

<code>data/</code> 可能包含 OpenAI/Grok Token、Cookie、OAuth 凭据、管理密钥、调用日志和生成图片。生产环境请使用随机密钥、限制管理接口来源、启用 HTTPS，并定期备份数据目录。

## 上游与许可证

本项目参考并继承了 [yukkcat/chatgpt2api](https://github.com/yukkcat/chatgpt2api) 的接口和业务思路，当前 Go 版代码与修改以本仓库为准。请保留仓库中的 [<code>LICENSE</code>](./LICENSE) 和 [<code>GROK2API_LICENSE</code>](./GROK2API_LICENSE) 文件，并按其中条款使用和分发。

## ChatGPT Web 能力额度

账号保存上游 `limits_progress`，对话 (`reason`)、图片 (`image_gen`) 和上传
(`file_upload`) 分别检查。剩余量未知时显示“未知”，不会作为 0 或推算总额度。
旧 `quota` 字段仍表示图片额度，旧版读取方式不变。仅因图片额度耗尽而限流的账号
仍可处理额度允许的文字请求；认证失败、禁用和其他未知限流不会因此解除。

账号列表和卡片直接展示图片、推理、文件上传、长文本转文件与深度研究的最近剩余量，
额外的上游额度项也会按原标识展示。悬停查看各自的恢复时间、核对时间及待核对用量；
刷新失败时保留旧值并明确提示。推理额度不等于所有普通文本请求的可用性。

每个上游尝试在发送前持久化预占；多张图片按子任务分别预占。未发送的预占释放，
已发送或响应不确定的消耗保留为待核对。上游已可能接收的请求不自动重复生图。
活跃账号每 5 分钟核对一次，完成请求后优先核对；重置时间到期仅触发重新读取，
不自动补满。无在途请求时才应用快照，查询失败保留旧值。

管理员可通过 `POST /api/accounts/quotas/refresh`，以
`{"account_refs":["账号管理 API 返回的 account_ref"]}` 查询 1–10 个账号。
此接口只读取额度，不刷新令牌或重新登录。新增字段均可被旧版忽略，回退仅切换
程序镜像，不覆盖账号、任务或图片数据。部署期间 `data/cfm-maintenance` 暂停
新的周期额度查询；检查完成后删除该标记即可恢复。

## 动态 ChatGPT Web 模型目录

CFM 使用各账号凭据读取 `/backend-api/models?history_and_training_disabled=false`，
账号仅关联目录摘要，相同的模型/能力目录只存一份；缓存保存在
`data/chatgpt_models.json`，6 小时过期，重启保留。后台每轮最多刷新两个账号，
失败退避并保留上一次目录，不因目录查询失败禁用账号。缓存损坏时保留原文件，
停止覆盖并在管理目录状态中报告，便于恢复。

`/v1/models`、单模型查询和 `/api/model-catalog` 共用动态目录；显式聊天请求只从
该账号已发现支持的模型中选账号。`auto` 和已验证但可能未被上游列出的 `gpt-5-3`
保留为明确的兼容入口。`research`、工作模式等尚未适配的条目只记录，不开放普通聊天。
目录包含的名称是 ChatGPT Web 显示名称，不据此猜测对应哪个官方 API 型号。

管理员可 `POST /api/accounts/models/refresh`，传入
`{"account_refs":["账号管理返回的 account_ref"]}` 主动刷新 1–10 个账号。
`GET /api/accounts/models/refresh` 和 `/api/model-catalog` 返回缓存就绪、过期、
错误、不同目录数量及模型发现/验证状态。公共模型目录不会返回账号身份或凭据。
前端读取后端目录，失败显示错误并可重试，不再提供写死的 GPT 聊天备用列表。

## ChatGPT Web 模型路由与诊断

管理员在“系统设置 → ChatGPT 模型路由”单独保存生图会话模型，或使用
`GET/POST /api/openai/routing`。POST 请求为
`{"image_conversation_model":"gpt-5-6"}`，仅接受目录中已开放的 ChatGPT 聊天模型
和明确保留的兼容入口。保存的模型成为生图默认会话模型，重启后保留；未配置时的
兼容回退值为 `gpt-5-3`。外部图片接口的 `model` 保持
`gpt-image-2`。目录可见只证明上游列出该会话模型，不保证所有生图、蒙版或透明输出
能力一致，应验证后再切换。配置在请求开始执行时读取，同一请求的各子任务使用同一个值。

文本请求保留调用方指定的 `model`，`auto` 原样交给上游，不做静默跨模型降级。
只有明确的模型拒绝才暂时排除该账号的对应模型，并触发目录刷新；重试仍使用相同
模型、其他支持该模型且额度允许的账号。网络超时等已可能被接受的请求不会自动重发。

日志 `request_meta.model_routing` 保存客户端请求模型、实际发送模型以及各输出/尝试
的额度种类、结果和重试原因。上游提供结构化 `model_slug` 时额外记录；未提供时
显示“上游未提供”，不从提示词猜测。此值是 Web 会话模型，不代表底层图片引擎版本。

两个管理刷新接口返回 `refreshed`、`failed` 和 `deferred`；正在使用的额度账号、
凭据发生变化或已经刷新中的账号列为延期，不误报成功。部署标记
`data/cfm-maintenance` 还会使新的写入/生成提交返回 503 和 `Retry-After`，已有请求
继续执行，状态查询和存活检测的暂停/恢复仍可用。先排空再停机备份和切换镜像；完成
检查后删除标记。回退 main 忽略新增配置与缓存，不恢复旧账号数据覆盖新增业务记录。
