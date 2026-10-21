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

### 导入已有账号的 Session JSON

在账号管理中选择“导入 Session JSON”，粘贴同一浏览器登录 ChatGPT 后
`https://chatgpt.com/api/auth/session` 返回的完整 JSON。导入提取 Access Token、
`user.email`、`user.id` 和到期时间，保存后校验上游身份、套餐和额度。
同一 Token 重复导入只补全缺失身份资料，保留原分组、代理、启用状态和 2FA 密钥；
身份冲突会拒绝导入。不同 Token 按独立记录导入，不自动合并已有账号。
校验失败会明确提示，已保存账号仍保留，可在列表手动刷新信息；不会自动删除或重新登录。
导入结束会清除状态/分组筛选并按账号引用定位，避免因分页或邮箱缺失找不到账号。
Session JSON 不提供自动刷新保证；已有 2FA 密钥可在编辑账号中补填，不会重新开启 2FA。

### 账号重新登录进度

在账号管理中通过账号操作菜单启动重新登录。工具栏下方的“重新登录进度”同时展示多个账号的阶段、耗时、成功结果和失败原因；同一账号运行期间重复启动会复用原任务。页面刷新或重新进入后会恢复查看，不会再次发起登录。失败可手动重试，结束记录可手动收起。

管理员接口 `POST /api/accounts/relogin` 启动任务，`GET /api/accounts/relogin` 读取所有运行中及最近 100 个结束任务的摘要；原单任务进度接口仍可用。摘要不返回凭据或浏览器环境。进度保存在服务内存，服务重启后不能恢复；账号凭据仍按原方式持久化，不受面板记录清理影响。

额度或用户信息请求明确返回凭据无效（401）时，没有 `refresh_token` 的已启用 ChatGPT 账号会尝试自动重新登录。自动恢复复用账号、注册历史或旧指纹中保存的浏览器环境；缺少可用浏览器环境、设备标识、邮箱或 HME 配置时不生成新环境、不创建新账号。普通超时、403、429 和服务端错误不会触发登录；已有 `refresh_token` 的账号仍走原令牌刷新逻辑。

自动任务显示在同一进度区并标注“自动恢复”；同账号手动/自动请求共用进行中任务，后台最多同时恢复 2 个账号，每账号自动尝试间隔至少 30 分钟（时间持久化，重启仍有效）。并发名额占满时本次不发起登录，后续额度/巡检失败可再次触发；手动重试不受自动冷却限制。具备上述恢复条件的手动登录也会在成功后刷新信息，避免手动与自动操作交错时遗漏；登录成功后刷新上游用户信息和全部能力额度，校验账号身份后更新原账号。如果新凭据已取得、额度接口暂时失败，会保留新凭据和旧额度，页面提示“额度待刷新”；不会将旧额度标为新结果。登录失败不删除账号，也不覆盖原凭据；手动和自动登录失败均标记“异常”并持久化停止调度标记，旧额度或旧存活记录不会解除。新凭据已保存但信息/额度刷新失败时也暂停调度，成功重新登录或完整账号校验通过后解除。旧任务不会标记后来已更换凭据的账号。调度与页面共用异常判定：任何异常账号都不参与聊天、生图或其他请求调度，不因旧额度、旧存活记录或先前请求的延迟成功而放行；只有重新登录或上游账号校验成功并更新状态后才可恢复。仅图片额度耗尽的限流账号仍按各项能力额度判断。

登录成功后的完整校验失败会保留脱敏诊断（阶段、接口、HTTP 状态及原因）。对于网络错误、
超时、429、5xx 或缺少额度快照，使用已保存的新 Token 最多额外校验 3 次，间隔为
30 秒、2 分钟、5 分钟；若上游 Retry-After 更长则等待更久。校验重试不会请求邮箱验证码、
创建账号或调用生图。401、403、身份不匹配等不进行这类重试，仍保持异常并禁止调度。

待校验状态与尝试次数保存在原账号数据中，后台额度调度器串行执行，维护期间或账号禁用时
暂停；重启沿用剩余次数，不重置预算。页面显示待校验时间、重试进度及脱敏详情。
完整用户身份与额度校验通过后解除登录阻止标记；仅更新额度不解除。管理员后来更换凭据、
删除账号或其他完整校验已完成时，旧校验结果不会覆盖新状态。回退旧版会保留账号数据，
但不执行新增的校验重试；可使用原手动刷新账号信息进行完整校验。

### 注册后自动开启 2FA

在“注册账号 → 账号安全”中打开“注册成功后自动开启 2FA”；默认关闭，
配置自动保存，对之后启动的 OpenAI 注册任务（含定时批次）生效，运行期间不能切换。
通过 `openai_free.auto_twofa` 配置。注册取得账号凭据并安全落盘后，复用同一会话、浏览器
环境、代理和 HME 邮箱进行重认证、申请与激活；不另建邮箱，不自动重发 2FA 邮箱验证码。
任务记录独立显示 2FA 阶段和结果，密钥入库后可在账号管理中查看验证码。

激活前同步保存待激活密钥；激活失败仍继续导入、校验新账号，2FA 失败会明确显示，
可从账号管理补充开启或续跑。停止、超时、服务重启及“重试入库 / 校验”不会自动重放
2FA 激活或重新注册；待恢复密钥保留在账号或私有注册恢复记录中。注册总超时包含 2FA 步骤。

### 为已有账号开启 2FA

在“账号管理 → 单个账号 → 更多 → 开启 2FA”启动独立任务；列表与卡片视图均支持。
账号必须有邮箱、已保存的浏览器环境和设备标识，缺少环境时先完成一次重新登录。
任务复用账号环境、与账号请求相同的代理解析规则及绑定当前 Token 的 ChatGPT Cookie，
通过邮箱重认证取得新鲜认证状态，再申请并激活 TOTP。重认证 Token 仅用于本次设置，
不会替换原业务 Token，也不会修改账号启用状态或额度。

工具栏下方的“2FA 开启进度”展示阶段、结果、失败原因和手动邮箱验证码入口。
明确绑定 HME 注册邮箱且当前 HME 配置可用时自动取码；其他邮箱、配置不匹配或自动取码
失败时可手动输入。不会创建邮箱、购买短信或自动重发验证码。同账号不能同时执行本任务
与重新登录、Codex OAuth；全局最多同时执行 2 个 2FA 任务，每次最多等待 10 分钟。

激活前同步保存待激活密钥和 enrollment 标识；只有上游明确返回激活成功，才将密钥转入
原 `two_factor_secret` 字段。激活超时或失败保留待激活密钥，手动“续跑激活”使用原密钥；
不会自动重放激活请求或重新申请密钥。若上游已激活但响应丢失，续跑可能因已经激活而失败，
此时应人工核对；管理员可在“编辑账号”中复制待激活密钥，确认后填入 `2FA Secret` 保存，
会清理待激活状态。保存失败时不会发送激活请求；激活后的最终保存失败仍保留此前的密钥备份。

刷新页面恢复查看当前任务。服务重启后原 HTTP 会话不能恢复，未结束任务显示中断；需要
手动重试，已持久化的待激活密钥保留。账号列表仅返回是否有密钥及是否待激活；密钥通过
原管理员凭据接口按需读取，进度、错误和普通列表不返回密钥、Token、验证码或 enrollment 标识。
“未配置”仅表示本地没有密钥，不代表已确认上游关闭了 2FA。

已配置密钥的账号可在列表“2FA”列或账号卡片中点击“看验证码”，查看当前六位 TOTP 与
剩余有效时间；展开期间每 30 秒自动换码，也可手动刷新、隐藏和复制验证码。
“复制密钥”按需读取管理员凭据接口。验证码由服务端本地生成，不请求上游，
不使用待激活密钥，不写入列表、任务日志或浏览器持久化存储。服务端时间应保持准确。
`POST /api/accounts/twofa/code` 传入 `{"account_ref":"账号引用"}` 返回验证码、
服务端时间及到期时间（Unix 毫秒），仅管理员可用，响应禁止缓存。

管理员接口：`GET /api/accounts/twofa` 读取各账号最近一次任务；`POST /api/accounts/twofa`
传入 `{"account_ref":"账号引用"}` 启动或复用任务。提交验证码使用
`{"progress_id":"任务ID","action":"code","code":"6位邮箱验证码"}`；取消使用
`{"progress_id":"任务ID","action":"cancel"}`。同一任务只接受一次邮箱验证码提交。
本功能独立于注册后的自动开启策略及 Codex OAuth，不会因为获取 OAuth 自动启用 2FA。

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

## 已有邮箱自动补偿注册

注册已取得邮箱但因验证码等待超时、网络/读取失败、HTTP 408/429/5xx 等暂时性错误未取得
账号凭据时，后台复用原邮箱及保存的 BrowserProfile 自动补偿。最多额外 3 次，失败后分别
等待 5 分钟、30 分钟、2 小时；上游 Retry-After 更长时按其等待。不会创建新邮箱或新任务记录。

自动补偿使用当前保存的注册配置，每次仅一个任务，已有手动/定时注册批次或补偿任务时等待；
定时批次到期时优先尝试批次。补偿独立于“定时启动新批次”的开关，无需新增数量或线程设置，
网页关闭仍执行。维护标记存在、服务关闭、邮箱来源配置不匹配时不启动。新任务绑定 HME 地址和
账号；旧记录至少要求原 HME 账号匹配，首次补偿时绑定当前地址。缺少原浏览器环境则交由人工处理。

任务记录显示已尝试次数、下次时间和等待状态。次数在请求发出前持久保存，重启不重置，执行中
异常重启先等待预留的超时与退避时间；旧版已有的暂时失败记录首次接入时至少等待 5 分钟。
明确拒绝、验证挑战、身份/配置错误及手动停止的任务不自动重跑，达到上限后仍可手动重试。
手动重试不重置自动次数；已取得凭据的任务仅保留原“重试入库/校验”入口，避免重复注册。
清理历史保留待补偿邮箱。回退旧版后不执行本功能，原账号/邮箱不删除；不要用旧备份覆盖新数据。

## 定时启动注册

“注册账号 → 执行控制”新增“定时启动”，只设置开关及间隔分钟数（1～10080），点击
“保存定时设置”生效。仅支持现有 ChatGPT Free 注册；每次触发读取当前已保存的批次
账号总数、线程数、邮箱与网络配置，不单独设置另一套注册参数。启用前先保存批次配置。

首次启用或修改间隔后，等待一个完整间隔再触发；手动启动不移动定时计划。已有注册任务
（包括补偿任务）或服务维护时跳过本次，下一间隔再尝试。关闭定时只阻止后续自动触发，
不会停止当前批次；手动“停止”只停止当前批次，定时开关仍保留。网页关闭不影响调度。

状态保存在 `data/openai_registration_schedule.json`（若另设注册配置路径，则在其同目录），
默认关闭；重启保留设置和下次时间，错过多个周期最多触发一批，不集中补跑。
先持久记录本次触发和下次时间，再调用原注册入口；若进程恰好在两者间退出，本次可能
未启动，等待下一周期，避免重复创建账号。存储错误时不启动新批次。

管理员 `GET/POST /api/register/schedule` 读取/保存定时配置，POST 仅接受设置
`{"enabled":true,"interval_minutes":60}`；账号参数仍使用原 `/api/register`。
`GET /api/register` 和注册事件流附带 `registration_schedule`，显示下次触发及上次启动结果。
当前批次执行结果仍查看注册任务记录。历史条数不限制手动或定时启动，页面每次固定加载
50 条，可翻页查看；不会自动删除历史、邮箱、待恢复任务或账号。仍可手动清理历史记录。
旧版本忽略新增状态文件；回退后旧版的 100 条历史限制可能再次阻止启动，但不会删除已有记录。

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

对于 ChatGPT Web 图片工具明确返回的通用错误 `We experienced an error when generating images.`
（含已知完整说明），同步和异步生图都会自动重试：每张输出最多额外 3 次，即最多 4 次尝试，
按 1、2、4 秒退避。沿用原提示词、参考图、尺寸和模型，通过账号池重新申请额度并上传参考图；
仅有一个可用账号时也可重试。已成功的其他输出不会重复生成。每次实际请求均记录额度消耗估计，
没有可用账号/额度、任务取消或到达原任务总超时则提前结束。此重试上限独立于 `GO_CHAT_MAX_RETRIES`；
生图的常规账号切换尝试也计入这 4 次上限，不叠加新的重试预算。
日志 `model_routing.attempts` 记录每次尝试及 `upstream_image_generation_retry` 原因。
审核拒绝、额度业务错误、未知终态、结果格式错误及下载失败不会因此重新生图；已发送生成请求后
结果不明的网络超时也不触发此重试，以免重复生成。重试失败仍保留最终完整原因。

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
只有明确的模型拒绝（包括会话提交/准备接口的 `{"detail":"Model not found"}`）才暂时排除
该账号的对应模型，并触发目录刷新；重试仍使用相同
模型、其他支持该模型且额度允许的账号。文件接口的 404、网络超时等已可能被接受的
请求不会因此自动重发。重试耗尽返回 `upstream_model_unavailable`，管理日志保留上游原始原因。

日志 `request_meta.model_routing` 保存客户端请求模型、实际发送模型以及各输出/尝试
的额度种类、结果和重试原因。上游提供结构化 `model_slug` 时额外记录；未提供时
显示“上游未提供”，不从提示词猜测。此值是 Web 会话模型，不代表底层图片引擎版本。

两个管理刷新接口返回 `refreshed`、`failed` 和 `deferred`；正在使用的额度账号、
凭据发生变化或已经刷新中的账号列为延期，不误报成功。部署标记
`data/cfm-maintenance` 还会使新的写入/生成提交返回 503 和 `Retry-After`，已有请求
继续执行，状态查询和存活检测的暂停/恢复仍可用。先排空再停机备份和切换镜像；完成
检查后删除标记。回退 main 忽略新增配置与缓存，不恢复旧账号数据覆盖新增业务记录。

## ChatGPT Web 登录与业务环境

注册、重新登录、账号信息/额度查询和聊天/生图统一读取账号保存的 `BrowserProfile`：
TLS Chrome 版本、User-Agent、Client Hints、语言、屏幕尺寸和 Sentinel 浏览器参数使用同一套
配置；设备 ID 复用账号记录，会话 ID 在同一凭据期间保持稳定。新注册仍为每个账号选择
独立配置，旧的已知 `fp` 可恢复原模板；没有历史配置的导入账号使用一致的默认配置。
已保存但无法识别的配置会明确报错，避免静默换成另一种浏览器环境。

成功注册或重新登录后，ChatGPT 站点的 Cookie 快照随账号私有数据保存，并绑定本次
access token。业务客户端按账号、凭据、配置和出口隔离 Cookie Jar，遵守域名、路径、
Secure 和有效期限制；手动更换 Token 不会加载旧登录 Cookie。新一轮登录仍重新获取
CSRF、授权状态和验证码，不保存认证域的临时 Cookie。账号列表和登录进度不返回 Cookie，
失败诊断对已保存的 Cookie 值脱敏。Cookie 是凭据，账号数据及备份应按私密凭据保管。

旧账号不会凭空补出历史 Cookie，需要下次成功注册/重新登录后才会获得快照。已有账号
字段和浏览器配置格式兼容，无需迁移；旧版本会忽略新增的私有字段。此统一并不等于确认
上游 `Model not found` 已恢复，仍需发布后以实际账号验证。

## ChatGPT Web 出口时区

聊天、生图、图片编辑、可编辑文件和账号额度请求使用实际选定出口的时区；会话
`timezone`、`timezone_offset_min`、Sentinel 日期使用同一份出口时区快照，偏移和时间戳
按当前时刻计算，参考图上传耗时不会冻结时钟。
使用内置 IANA 时区库计算夏令时，不改变系统时钟、Token 到期时间或日志 UTC 时间。

通过独立无凭据客户端，按顺序查询 ipwho.is、ipapi.co、freeipapi.com；查询沿用业务选定
的直连或 HTTP(S)/SOCKS5 代理，不向定位服务发送账号 Token、Cookie 或用户内容，也不在
代理查询失败时改走直连。首次查询总预算 4.5 秒；按出口缓存 10 分钟，同出口并发合并查询。
缓存过期后按需刷新；失败保留上次有效时区，无记录时使用脚本的默认 America/Los_Angeles，
一分钟后允许重试。缓存只在内存中，重启后重新获取，不修改已绑定的注册/登录 BrowserProfile。
同一代理地址若按连接轮换到不同地区，无法保证定位请求与业务请求的实际 IP 相同，应使用
稳定出口。自定义非 ChatGPT Web Base URL 不自动访问定位服务。

2026-10-15 在 O 机器进行同账号对照：原参数和 America/Phoenix 参数的短文本、生图均返回
404 `Model not found`；时区校准功能并不代表该上游故障已恢复。生图现在会保留真实 HTTP
错误，不再把非成功响应按 SSE 解析后误报为“没有会话 ID”。
