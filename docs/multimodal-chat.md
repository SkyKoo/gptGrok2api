# ChatGPT Web 多模态聊天

CFM 使用现有 ChatGPT JWT 账号，接收文字和参考图片并返回文字回答。对外采用 OpenAI Chat Completions 和 Responses 的消息、响应及 SSE 事件格式；对内调用 ChatGPT Web。这里只实现文字与图片对话子集，不表示支持官方 API 的全部能力。

## 接入

| 接口 | 输入 | 普通响应 | 流式响应 |
| --- | --- | --- | --- |
| `POST /v1/chat/completions` | `messages`，`text` / `image_url` | `chat.completion` | `chat.completion.chunk`，以 `[DONE]` 结束 |
| `POST /v1/responses` | `input`、可选 `instructions`，`input_text` / `input_image` | `response`，`output[].content[].text` | 官方具名 Responses 事件，以 `response.completed` 或 `response.failed` 结束 |

认证沿用 CFM API Key：`Authorization: Bearer <CFM_API_KEY>`。密钥不是 ChatGPT 账号令牌。

`model: "auto"` 是 CFM 已有的 ChatGPT Web 模型标识。Free 账号的实际模型由上游调度，不保证等同于官方 API 中某个固定模型。`gpt-image-2` 等生图模型继续使用图片接口，不用于这两个接口的普通看图聊天分支。

Chat Completions 示例：

```json
{
  "model": "auto",
  "messages": [
    {"role": "developer", "content": "请根据参考图给出创作建议，只返回文字。"},
    {"role": "user", "content": [
      {"type": "text", "text": "描述这张图，并给出两种构图方案。"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,<BASE64_IMAGE>"}}
    ]}
  ],
  "stream": false
}
```

Responses 示例：

```json
{
  "model": "auto",
  "instructions": "请根据参考图给出创作建议，只返回文字。",
  "input": [{"role": "user", "content": [
    {"type": "input_text", "text": "描述这张图，并给出两种构图方案。"},
    {"type": "input_image", "image_url": "data:image/png;base64,<BASE64_IMAGE>"}
  ]}],
  "store": false,
  "stream": true
}
```

`<BASE64_IMAGE>` 需要替换成实际图片数据。两种接口均可将 URL 换成可公开下载的 HTTP(S) 图片地址；保留既有私网地址和重定向检查。图片获取不附带 CFM 或 ChatGPT 认证信息。

支持 `system`、`developer`、`user`、`assistant` 的文字历史，图片仅放在 `user` 消息中。每次传入完整历史；Responses 也接受上次输出中 `type: "message"` 的 assistant `output_text` 作为历史。图片与文字的排列、不同消息之间的顺序均保留；developer 在 Web 请求内映射为 system。只有图片、没有文字的 user 消息也可以提交。

## 能力边界

- 本适配器每次请求最多 7 张图片（包括历史消息），总图片字节不超过 50 MiB，单图不超过 4000 万像素；整个 JSON 请求沿用 64 MiB 限制。支持 PNG、JPEG、WebP、GIF；实际可用性仍受上游账号额度和图片策略限制。这些数值是 CFM 的输入保护限制，不是对 ChatGPT 官方上限的声明。
- `detail` 省略或使用 `auto`。显式 `low` / `high` 目前不能可靠映射，返回 400。
- 两个接口都支持 `stream: false` / `true`。流式模式会实时转发可见文字，不会等待完整回答后再一次性发送。内部推理内容不作为回答输出。
- Web 上游不提供官方 API token usage，返回 `usage: null`，不伪造计量数据。
- 不保存可查询的 Responses 对象；`store`、`background` 仅允许 false 或省略。不支持 `previous_response_id`、conversation ID、后台任务、Files API 的 `file_id`、工具调用、音视频输入。
- 不支持官方严格 JSON / JSON Schema 输出，也不支持显式 `temperature`、`top_p`、token 数限制等 Web 无法保证的参数。发送这些参数会得到带 `error.message/type/param/code` 的 400 错误。可以在提示词中要求 JSON，但调用方仍需要校验回答。
- `reasoning_effort`（Chat）或 `reasoning.effort`（Responses）沿用已有 Web 努力级别透传，具体是否生效取决于所选 Web 模型；不支持 reasoning summary 等扩展。
- 当前功能不包含 Lumora provider 抽象、规划提示词、计划校验、任务拆分或生图调度。

## 调用链与失败处理

1. 校验官方消息格式，获取并校验图片字节；无效输入在选择账号前返回错误。
2. 从现有 OpenAI 账号池获取一个账号，固定该次尝试的代理出口。
3. 用该账号申请上传、传入图片、确认上传，得到账号内的文件引用。
4. 将文字与引用组装为 Web 的 `multimodal_text` 消息，调用 `/backend-api/conversation`，解析可见回答。
5. 按请求协议返回普通 JSON 或实时 SSE。上传和对话共享账号租约。

仅在尚未输出可见文字，且错误属于现有可重试状态码时换号；每次换号重新上传全部图片，不复用其他账号的文件 ID。已经输出文字后出错，不重新发起对话，避免重复或拼接不同回答。客户端断开会取消上游请求，不把断开归因于账号异常。

普通请求出错返回 HTTP 错误；SSE 建立后，Chat 返回 error 数据并结束，Responses 返回 `response.failed`（保留已收到的部分文字）。未收到正常终止信息的上游流会报错，不发送虚假的完成事件。

## 验证记录与复测

2026-10-08，功能开发前先通过 O 机器现有 Free 账号完成真实上游验证：上传程序生成的红圆、蓝方块两张测试图，要求识别顺序、颜色、形状并分别生成海滩和森林的创作提示词。前两个旧令牌被上游以 401 / `token_revoked` 拒绝；第三个较新的有效令牌在约 5.2 秒内正确返回两张图的特征和两份方案。没有调用生图接口。

该实测验证了上传、同账号普通对话、两图理解及文字响应。它不代表每个账号可用，也不代表严格结构化输出、全部模型或所有多轮消息组合都通过了真实上游测试。角色/历史保留、两个公开接口格式、实时 SSE、错误及重试由本地模拟上游回归测试覆盖。

默认测试不调用真实 ChatGPT：

```bash
go test ./...
go vet ./...
go test -race ./internal/protocol ./internal/provider ./internal/httpapi
```

可选上游探针位于 `internal/provider/openai_chat_live_test.go`，通过编译标签和环境开关双重启用；运行会上传两张生成的几何图并消耗一次对话额度。需在已配置账号数据路径和出口的环境中执行：

```bash
CFM_LIVE_CHAT=1 go test -tags cfm_live ./internal/provider \
  -run '^TestLiveOpenAIMultimodalChat$' -count=1 -v
```

每次运行仅使用一个 Free 账号，不写账号反馈或刷新凭据。可选 `CFM_LIVE_SKIP_ACCOUNTS=0..2` 在选择前离线跳过前几个候选；`CFM_LIVE_NEWEST_TOKEN=1` 从剩余 Free JWT 中选择到期时间最新且可租用的账号。探针不输出令牌或账号标识。发布前应按部署环境单独验收；本次上游探针没有替换正式容器或部署功能分支。

协议参照（核对日期 2026-10-08）：[图片输入](https://developers.openai.com/api/docs/guides/images-vision)、[Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)、[Responses 迁移指南](https://developers.openai.com/api/docs/guides/migrate-to-responses)、[Responses 流式事件](https://developers.openai.com/api/reference/resources/responses/streaming-events)。
