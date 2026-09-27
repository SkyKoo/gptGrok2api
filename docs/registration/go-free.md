# Go ChatGPT Free 注册

状态：2026-09-28，Go 注册主流程、HME 补偿注册和 protocol 浏览器画像已实现并通过本地模拟测试。
生产部署状态以 O 机器运维记录为准；本文不代表真实上游注册成功率承诺。

## 范围

- 分支 `codex/go-free-registration` 从本地 `main` 的 `6290521` 创建。
- 参考删除前 `01d49d300c1c7553e879bbcfbb085ec3952a11d2` 中的
  `services/register/openai_register.py`（`ChatGPTWebRegistrar`）及相关模拟测试。
- Go 主服务直接执行注册，无 Python 或真实浏览器运行时；Sentinel 仅使用镜像内置 Node VM
  运行 SDK，无新增容器或 Go 模块依赖。
- 每次只运行一个账号：`target=openai`、`mode=total`、`total=1`、`threads=1`。
- 邮箱接入的是独立部署的 `icloud-hme`，类型 `icloud_hme`。
  旧 `icloud_api` / `icloud_local` 面向另一个 Privacy Mail 项目，协议不同，不适用于此实现。
- 不包含自动支付、CPA/Sub2API 投递、Agent Identity 归档、额度补池或 Grok 注册。

## 管理页面配置

1. 进入“注册账号”，使用原注册页面的 OpenAI / Grok 页签，选择 OpenAI。
2. 保持单账号、单线程，总超时默认 600 秒，邮件等待默认 180 秒。无需填写姓名或出生日期。
3. 启用一个“iCloud HME（独立服务）”邮箱来源，填写：
   - 服务地址：HME 的根地址或其部署路径前缀，不加 `/api`。
   - 管理员密码：HME 管理台密码，不是 Apple 账户密码或 App 专用密码。
   - HME 账号 ID：HME `GET /api/accounts` 返回的 `acc_…` ID。
4. 注册代理留空沿用全局，`direct` 表示直连，也接受 HTTP/HTTPS/SOCKS5 地址。
   本版不接受代理组选择。HME 请求独立直连，不继承注册代理。
5. 启动任务后查看进度及任务记录。取消会终止等待和网络请求；已创建的邮箱别名保留。

HME 密码保存于 CFM 的私有注册配置文件（0600），管理接口及 SSE 仅返回
`admin_password_set`。同一来源 ID、服务地址、账号 ID 下留空密码会保留已保存值；
更换服务地址或账号需要重新填写。不要将真实配置、任务恢复文件或账号文件提交到 Git。

同机部署可将两容器加入专用 Docker internal 网络，使用 `http://hme:8081`。
CFM 环境变量 `GO_HME_INTERNAL_BASE_URL` 必须固定为同一地址，才能在该内部 HTTP
来源保留 HME 的 Secure 会话；此例外不作用于其他来源或重定向，公网 Cookie 设置不变。

HME 使用管理员 Cookie 会话及 CSRF，调用登录、创建别名、邮件列表和邮件详情接口。
验证码按别名、收件人、发件域及发码时间筛选，同时读取收件箱和垃圾邮件；读取详情时保留
列表返回的 `method` 与 `folder`，不混用 Web UID 和 IMAP UID。不会删除邮件或邮箱别名。

## 任务状态与恢复

流程：创建别名 → 初始化网页会话 → 提交邮箱 → 发码 → 收码 → 校验验证码 →
按上游实际跳转获取网页凭据 → 持久化结果 → 入库 → 校验账号。

- 注册结果先写入注册配置同目录的 `openai_registration_tasks.json`（0600）。
- 账号先以禁用、待校验状态入库；校验成功后启用。
- `completed`：已入库并通过账号校验。它不代表已验证实际生图权限。
- HME 别名创建成功后，邮箱会立即写入任务记录并在管理页面完整显示；如果后续注册流程
  失败或进程中断，任务进入 `registration_pending`，可点击“复用此邮箱重试注册”。
  补偿流程只重新登录 HME、复用已保存别名并重新执行 ChatGPT 流程，不调用 `/api/create`
  或再消耗一个邮箱。任务拿到凭据后仍按入库/校验状态恢复。
- `import_pending` / `verification_pending`：凭据保留，可点击“重试入库 / 校验”。
  重试只处理保存的结果，不创建别名、不再次注册、不重复发送验证码。
- `interrupted`：进程中断且未保存凭据。人工核查邮箱标签及账号状态，不能断言注册失败。
- 上游直接完成或给出合法回调时获取会话；若明确返回资料补全页面，立即报
  `session: profile_completion_required`，保留任务，不提交或生成个人资料。
- 网络错误、验证码拒绝、未知页面或上游验证要求会停止任务；不无限重试或自动换邮箱。
- 重启不会自动启动新注册；恢复文件损坏时阻止启动，避免覆盖原始恢复信息。
- 最多保留 100 条任务；“清理已完成记录”仅移除成功记录，不删除账号、配置及失败记录。
  大量失败记录需先离线备份、核查后再维护恢复文件，不应盲目清空。

管理 API 继续使用管理员鉴权：

| 接口 | 行为 |
| --- | --- |
| `GET /api/register` | 配置及脱敏任务快照 |
| `POST /api/register` | 保存配置；运行期间拒绝修改 |
| `POST /api/register/start` | 校验配置并启动一次任务 |
| `POST /api/register/stop` | 请求取消当前任务 |
| `GET /api/register/runtime` | 配置就绪状态、任务和存储错误 |
| `GET /api/register/events` | 与前端协议一致的配置 SSE 快照 |
| `POST /api/register/openai/retry-result` | `{"id":"任务 ID"}`；仅重试保存结果 |
| `POST /api/register/openai/retry-registration` | `{"id":"任务 ID"}`；复用已创建 HME 邮箱补偿注册 |
| `POST /api/register/reset` | 清理已完成记录，保留配置和待恢复结果 |

## 协议边界

HTTP 注册参照历史网页流程，包括两次 CSRF/sign-in 初始化、Cookie 会话、发码上下文、
一次性验证码提交及回调域名/state 校验。Go 网络客户端使用已有 TLS 客户端依赖；TLS
证书验证保持开启，没有迁移旧代码的 `verify=False`。

注册校验使用旧 Python 的 HTTP Sentinel 回退协议及已有 Go 解码器。
protocol 模式参考 Turb 的 `config/browser.py`，为每个注册任务从候选池选择一组稳定的
`BrowserProfile`，同时供 tls-client、HTTP Client Hints 和 Sentinel VM 使用；同一任务内不变，
补偿注册从任务恢复文件复用原画像。当前画像池使用 tls-client 已有的 Chrome 131 Profile，
并随机选择屏幕尺寸、CPU 核数、JS 堆和设备内存组合。它只是在协议层模拟参数，不是完整浏览器
指纹，也没有引入 Roxy、Cloak、Browser Use 或 Skyvern 等外部浏览器服务。
没有移植 Turb 的真实浏览器驱动、Session Observer 或外部浏览器辅助服务。
上游协议、页面或验证要求变化时会返回明确失败；模拟测试通过不保证真实注册成功。
日志仅包含固定阶段/错误类别/HTTP 状态，不记录验证码、Token、Cookie 或上游响应体。

## 测试与合并条件

2026-09-28 本地结果：全量 Go 测试、注册/HTTP/provider 包竞态检查、`go vet`、前端构建、
Linux ARM64 无 CGO 编译均通过。所有新增网络测试均使用 loopback 模拟服务；没有连接真实
HME 或创建 ChatGPT 账号。最终复核补充了验证码后的授权跳转、嵌套回调和已有 Cookie 复用场景。

本地验证命令：

```sh
go test ./...
go test -race ./internal/register ./internal/httpapi ./internal/provider
go vet ./...
npm --prefix web-vue run build
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/cfm-register-arm64 ./cmd/gptgrok2api
```

新增覆盖：发码与收码顺序、会话保持、验证码不重放、回调域名/state/邮箱校验、垃圾邮件详情、
旧邮件和非目标邮件过滤、取消、并发启动拒绝、恢复文件损坏、重启不重复注册、导入失败重试、
校验失败保持禁用、HME 密码脱敏与变更地址时不转发密码、注册失败后复用原邮箱补偿注册、
画像池字段一致性和补偿重试复用原画像。

真实验收应使用隔离测试实例及明确选择的 HME 账号，完成一次注册、账号校验和一次实际生图。
验收需另行登记 O 机器的部署/数据变更；本功能实现没有改动线上实例。

原 main 工作区有未提交的生图补丁，本分支不包含这些修改。合并前需先妥善提交或整理它们，
审查 `server.go`、README 等共同文件，保留原有图片任务、超时和下载校验行为。
