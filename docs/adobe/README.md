# Adobe Firefly 接入

开发分支：`codex/adobe-integration`，基于 main `b201d11`。仅本地开发与 Docker 验证，未发布线上。
验证日期：2026-11-11（Asia/Shanghai）。免费模型权益、余额与上游协议可能变化。

## 模型与统一接口

调用方只指定逻辑模型和参数；无需也不能通过 provider/channel 指定渠道。现有 ChatGPT 和
Grok 模型 ID 保留。Adobe 模型沿用 CFM 的 `/v1/images/generations`、`/api/image-tasks/generations`
以及 `/v1/videos` 接口。`/v1/models` 和管理目录返回能力组合，Studio 仅展示模型和对应参数。

| 逻辑模型 | 名称 | 已验证的 Web Free 参数 |
| --- | --- | --- |
| gemini-3.1-flash-image | Gemini 3.1 Flash Image / Nano Banana 2 | 1024×1024，单张 PNG，无参考图，默认画质 |
| veo-3.1-lite | Veo 3.1 Lite | 1920×1080、16:9、1080p、8 秒、无音频、无参考图 |

只允许已通过免费账号实测的组合；目录可见或请求字段存在不等于可用。其他档位、付费模型和
未验证的参考图/音频能力不开放。Adobe 的 Sunburst 在本轮测试返回 403 且未创建任务，因此
`gpt-image-2.5-sunburst` 当前仍仅走已有 ChatGPT 渠道。此前 Veo Lite 成功与本轮验证结果应
分别阅读[协议验证记录](protocol-validation.md)，不能把目录资格解释为每次请求必然成功。

同步生图示例：

```json
{"model":"gemini-3.1-flash-image","prompt":"A blue ceramic vase on a white table","n":1,"size":"1024x1024","response_format":"url"}
```

异步生图使用相同参数并提供 `client_task_id`；按调用者隔离幂等，同 ID 不同输入返回 409。
视频为 multipart：model=veo-3.1-lite、prompt、seconds=8、size=1920x1080、
resolution_name=1080p、generate_audio=false。`Idempotency-Key` 或 `client_task_id` 提供幂等，
重复调用返回原任务，不重新生成；不同输入使用同键返回 409。

## CFM 内部路由

系统设置中的渠道优先级仅在一个逻辑模型有多个合格渠道时显示。管理接口：
`GET /api/model-routing` 返回 models（model/name/channels/priority）；
`PUT /api/model-routing` 接受 `{"priorities":{"逻辑模型":["chatgpt","adobe"]}}`。
单渠道模型无需配置；新合格渠道追加到既有顺序之后。配置只影响以后首次选择的任务。

选择前检查完整参数组合、有效凭据、已确认额度与账号占用；无合格账号可检查下个候选渠道。
一旦选定渠道和账号，立即持久化绑定，后续失败不跨渠道、不换账号、不重发生成 POST。
这也适用于现有 GPT/Grok 媒体生成和内容核对；文本请求和注册恢复的独立策略不受影响。
GPT 多图批次固定同一账号，按顺序生成，预留整批额度；任何失败立即终止剩余输出。

Adobe 在收到上游任务句柄后加密保存，只在明确 COMPLETED 后交付资源，不能用 progress=100
或提前出现的下载地址判定完成。重启恢复已有句柄的轮询与下载；没有句柄的未知提交保留
未知结果和账号占用，不重新提交。轮询/下载中断可通过图片或视频任务 `/resume-poll` 恢复。
恢复使用原账号和原代理绑定，管理员更改模型优先级不能改变已有任务。

## 账号、额度与注册

注册和账号管理各有独立 Adobe 标签，邮箱平台占用/禁用/已使用状态独立；删除其他平台账号
不能误释放 Adobe 占用。注册设置、调度、结果恢复记录分别保存，已提交注册结果未知时不建新号。
取得账号但导入或校验失败时，只恢复已有结果。邮件验证码复用邮箱管理器；人机验证保留待人工
处理状态，不绕过验证。浏览器仅参与注册、登录态维护，图片与视频调用由 Go HTTP 完成。

`/api/adobe/accounts` 及其 import/refresh/disabled 子接口只允许管理员。列表采用安全字段白名单，
不返回 Cookie、令牌、密码、Sherlock、代理凭据或浏览器资料。凭据按账号身份和版本校验后更新；
刷新返回其他身份时拒绝覆盖。IMS 刷新失败可使用原 browser profile，仍须核对相同身份。
后台维护与生成占用互斥，不能在提交失败后刷新并重发生成。

令牌刷新与媒体会话更新分别显示：仅取得新 Token/IMS Cookie 时，保留已有完整媒体 Cookie，
标记 `preserved`；取得完整且不同的媒体 Cookie 才标记 `refreshed`。不能拼装缺失的人机验证
字段，也不能把令牌刷新成功解释为媒体会话已续期。现有账号密码回退只有离线测试通过，
真实启动测试止于 `browser_start_failed`，未验证实际登录。

Adobe 图片和视频共用账号余额。余额保留 known/unknown/stale/error，零与未知不同；未确认余额
不得当作可用。生成过程中独占账号，完成后先等待结算再查询。当前余额接口多次仍为 10/0/10，
因此不按“每次扣 1”估算，也不标记无限额度；真实成本规则尚未由余额观察确定。

## Docker 与持久化

共享 Worker 服务/容器默认 `cfm-browser-worker`，镜像 `cfm-browser-worker:local`；旧 URL 环境变量
与 DNS 别名兼容。单会话队列负责 Python/Grok 与 Node/Adobe 生命周期；确认浏览器进程关闭后
才释放容量。CloakBrowser 当前许可只支持一个并发会话，保持 concurrency=1。

Compose 挂载 `data/browser-worker:/data`；Adobe profile 位于 `/data/adobe/<profile_id>/`，与 Grok
分开。账号 `adobe_accounts.json.enc` 和 `adobe_jobs/*.json.enc` 使用应用密钥派生的 AES-GCM
加密；注册日志使用加密 JSON 信封。不要改动或遗失解密密钥。普通回滚只换应用，不覆盖新数据。
备份范围和恢复步骤见 [核心备份](../../deploy/backup/README.md)。

开发验收不需要运行生产 Compose：使用独立容器、临时数据卷和回环端口。不要挂载线上业务数据。
实现分支允许继续审阅和本地验证；合并或发布需要另行安排。

## 本地验收记录

所有依赖安装、编译及测试都在 Docker 内执行，未在宿主机安装开发依赖。最后应用启动检查
使用 `--network none`、只读根文件系统、空 tmpfs 数据目录，无宿主端口映射。

| 检查 | 结果 |
| --- | --- |
| Go 全量 `go test ./...`、`go vet ./...` | 通过 |
| Adobe、路由、恢复及不重发关键路径 race 检查 | 通过 |
| 图片四类入口拒绝调用方指定渠道 | 40 个组合通过；包括空白字段和重复 multipart 值 |
| Node Worker 测试 | 93 项通过 |
| Python Worker 生命周期与队列测试 | 77 项通过 |
| Vue TypeScript/Vite 构建 | 通过 |
| 离线 UI 浏览器检查 | 9 项通过，无未知 API 写入或页面异常 |
| PostgreSQL 第三渠道邮箱迁移与隔离 | 临时数据库验证通过 |
| 核心备份测试 | 9 项通过 |
| 三份 Compose 配置 | 仅解析校验通过，未启动业务 Compose |
| 最终应用镜像断网检查 | 健康、静态资源、鉴权、模型目录、优先级约束、注册设置/队列/调度、渠道字段拒绝全部通过 |

本地 ARM64 镜像（不上传）：

- `cfm:adobe-dev`：`sha256:8cd5a592dc2f25055bc00fbf02eda0b1becc224f789c94c6ccaf4a988a62d55c`。
- `cfm-browser-worker:adobe-test`：`sha256:048d2c77559c158faae5c130e11773e24c2d50a4a35b9b215b892af3f0b4177d`。

运行时镜像验证与第三方实测边界分开记录：新 Go 图片任务成功；新 Go Veo 提交尚未通过。
同一当前凭据、代理和参数的单次 Node 对照也返回 403，不能将其归因为 Go 独有问题。
完整媒体 Cookie 长期自动续期与额度扣费规则也未得到实测确认。详细证据和失败记录见
[协议验证记录](protocol-validation.md)。这些结果不构成线上发布验收。
