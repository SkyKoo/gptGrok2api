# Adobe Firefly 接入

开发分支：`codex/adobe-integration`，基于 main `b201d11`。仅本地开发与 Docker 验证，未发布线上。
验证日期：2026-11-11（Asia/Shanghai）。免费模型权益、余额与上游协议可能变化。

## 模型与统一接口

调用方只指定逻辑模型和参数；无需也不能通过 provider/channel 指定渠道。现有 ChatGPT 和
Grok 模型 ID 保留。Adobe 模型沿用 CFM 的 `/v1/images/generations`、`/api/image-tasks/generations`
以及 `/v1/videos` 接口。`/v1/models` 和管理目录返回能力组合，Studio 仅展示模型和对应参数。

当前目录包含新免费账号在官方 `generation-preflight` 中确认授权的 8 个图片模型、6 个视频模型。
权限检查通过与生成完成分别记录；本轮实际生成仍遇到 HTTP 408，不能宣称全部模型已实测生成成功。

| 类型 | 逻辑模型 | 名称 |
| --- | --- | --- |
| 图片 | gemini-3.1-flash-image | Nano Banana 2 |
| 图片 | gemini-3.1-flash-image-2.1 | Nano Banana 2.1 |
| 图片 | firefly-image-4 / firefly-image-5 | Firefly Image 4 / 5 |
| 图片 | humain-image-1 | HUMAIN Image 1 |
| 图片 | gpt-image-2 | GPT Image 2 |
| 图片 | gpt-image-2.5-flare / gpt-image-2.6-flare | GPT Image 2.5 / 2.6 Flare |
| 视频 | veo-3.1-lite / veo-3.1-fast | Veo 3.1 Lite / Fast |
| 视频 | gemini-omni-flash-1.1 / gemini-omni-flash-1.2 | Gemini Omni Flash 1.1 / 1.2 |
| 视频 | firefly-video | Firefly Video |
| 视频 | ray-3.14 | Ray 3.14 |

Nano Banana 2 开放 10 个有精确尺寸映射的 1K 比例和至多 6 张参考图；例如 4:3 是 1200×896，
不按像素最大公约数误显示为 75:56。Nano Banana 2.1 可使用 14 张参考图。其他模型参考图数量、
用途、尺寸与档位来自各自目录，不能把一个模型的能力复制给所有模型。
Veo Lite/Fast 当前开放 1080p、8 秒、16:9，可选择音频并使用首尾帧；Omni 的音频必须开启。
前端依完整参数组合联动，调用方仍只选模型；不支持的组合在上传和提交前拒绝。

GPT Image 2、2.5 Flare、2.6 Flare 同时保留 ChatGPT 与 Adobe 渠道，管理员可配置优先级。
Sunburst、Nano Banana Pro、标准 Veo 等候选在本次账号/参数查询没有免费授权，不加入当前目录。
每次生成前再次检查实际参数的免费权益，只接受官方免费资源，不能用通用积分自动走付费资源。
详细权限证据、历史成功与本轮失败记录见 [协议验证记录](protocol-validation.md)。

同步生图示例：

```json
{"model":"gemini-3.1-flash-image","prompt":"A blue ceramic vase on a white table","n":1,"size":"1024x1024","response_format":"url"}
```

参考图生图沿用 `/v1/images/edits` 与 `/api/image-tasks/edits`。异步生图使用相同参数并提供 `client_task_id`；按调用者隔离幂等，同 ID 不同输入返回 409。
视频为 multipart：model=veo-3.1-lite、prompt、seconds=8、size=1920x1080、
resolution_name=1080p、generate_audio=true/false；参考图复用现有 multipart 图片字段。`Idempotency-Key` 或 `client_task_id` 提供幂等，
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
不能误释放 Adobe 占用。Adobe 注册地区固定为美国，每个新任务自动生成实际年龄 25–50 岁的
合法出生日期，保存在加密任务记录中；同一任务恢复沿用原日期。页面不再提供地区、生日
或验证码等待/轮询配置，旧地区/生日配置在读取和保存时清理。

验证码复用 GPT/Grok 的共享 HME 邮箱资源管理器，使用所选邮箱来源自动领取、收码和
记录归属。Adobe 使用统一的内置收码等待/轮询策略，不读取其他渠道遗留的时间设置；
邮件同时检查 Adobe 发件人、目标邮箱和时间窗口，并识别 HME 的 iCloud 转发地址。
注册设置、调度、结果恢复记录分别保存，已提交注册结果未知时不建新号。
取得账号但导入或校验失败时，只恢复已有结果。邮件验证码复用邮箱管理器；人机验证保留待人工
处理状态，不绕过验证。浏览器仅参与注册、登录态维护，图片与视频调用由 Go HTTP 完成。

`/api/adobe/accounts` 及其 import/refresh/disabled 子接口只允许管理员。列表采用安全字段白名单，
不返回 Cookie、令牌、密码、Sherlock、代理凭据或浏览器资料。凭据按账号身份和版本校验后更新；
刷新返回其他身份时拒绝覆盖。IMS 刷新失败可使用原 browser profile，仍须核对相同身份。
后台维护与生成占用互斥，不能在提交失败后刷新并重发生成。

令牌刷新与媒体会话更新分别显示。ARP 会话按官方客户端的不透明值原样转发，不要求或拼装
`sid/ark/bfp/ftr` 等内部字段；新账号合法导出可能只有 sid/ftr。仅取得新 Token/IMS Cookie 而
没有新媒体会话时保留已有值，不能把令牌刷新成功解释为媒体会话已续期。

账号页分别显示 `quota.generation.pools` 中的共享免费额度，不按模型复制或合并为一个总次数。
本次新账号返回：图片池 3 次/日、视频池 1 次/日、GPT Image 2 独立 3 次体验池；实际数值及
刷新周期以各账号接口为准。`quota.available` 的 10/0/10 是通用积分，不能代替这些生成额度。
未知、查询失败、过期与明确零值分开。已成功查询的额度快照超过 5 分钟后保留上次数值和
查询时间，页面明确标为“待刷新”，不将历史数值当作当前可用额度；已检查通过的账号不会仅因
快照过期变为“待检查”。未曾查询或查询失败仍单独提示，凭据异常、过期和禁用状态保持。
闲置账号后台每小时检查，近期使用账号每 5 分钟检查；展示旧快照不会放宽实际生成的权限校验。
选择账号前按目标模型过滤新鲜、明确不可用的额度；未知额度由精确 preflight 判定。
已绑定后失败不换账号或渠道，408 按提交结果未知保留，不重发。
生成仍独占账号，结束后等待结算并重新查询，既不假定每次固定扣 1，也不据积分不变推断无限。

账号列表每行提供“刷新额度”和“更多”菜单，可单独刷新登录态、启用/禁用或删除。
勾选后才显示批量操作栏；单行操作仅作用于当前账号，删除确认固定目标账号，取消不会执行删除。

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
| Node Worker 测试 | 95 项通过（含可选出生日期与不透明媒体会话） |
| Python Worker 生命周期与队列测试 | 77 项通过 |
| Vue TypeScript/Vite 构建 | 通过 |
| 离线 UI 浏览器检查 | 9 项通过，无未知 API 写入或页面异常 |
| PostgreSQL 第三渠道邮箱迁移与隔离 | 临时数据库验证通过 |
| Adobe 注册资料及 HME 收码 | 25–50 岁边界、任务资料持久化、直发/转发验证码及跨平台邮件隔离通过 |
| 核心备份测试 | 9 项通过 |
| 三份 Compose 配置 | 仅解析校验通过，未启动业务 Compose |
| 最终应用镜像断网检查 | 健康、静态资源、鉴权、模型目录、优先级约束、注册设置/队列/调度、渠道字段拒绝全部通过 |

本地预览镜像使用 `cfm:adobe-dev` 与 `cfm-browser-worker:adobe-test`；仅在独立本地
Compose 项目更新，不上传、不发布线上。当前构建的实际 image ID 以本地 Docker inspect 为准。

本轮 14 个模型的免费参数预检全部通过，参考图上传成功；Nano Banana 2 参考图、Veo Lite
首帧加音频、CFM Go 客户端的 GPT Image 2.5 Flare 文生图均返回 HTTP 408 `timeout_error`，
没有任务句柄且没有重试。先前普通 Nano Banana 2 与 Veo Lite 的成功记录仍保留，不能用它们
替代本轮新增能力的生成完成验收。协议与界面已接入，当前真实提交链路仍有未解决的上游错误。
