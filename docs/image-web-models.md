# GPT Web 图片模型

此实现使用 ChatGPT Web，不等同于公开 OpenAI Images API。模型可用性取决于上游和账号当时的权限、额度。

## 路由和能力

| 请求型号 | 发往 Web 的型号 | 静态输出 | GIF |
| --- | --- | --- | --- |
| gpt-image-2 | 配置的 image_conversation_model，缺省 gpt-5-3 | 保留已有行为 | 不支持 |
| gpt-image-2.5-sunburst | 原名 | PNG / JPEG / WebP | 不开放 |
| gpt-image-2.5-flare | 原名 | PNG / JPEG / WebP | 不开放 |
| gpt-image-2.6-sunburst | 原名 | PNG / JPEG / WebP | 生成接口支持 |
| gpt-image-2.6-flare | 原名 | PNG / JPEG / WebP | 生成接口支持 |

显式型号不读取旧会话模型配置，也不要求聊天目录包含这些型号；它们只登记为图片模型。
GIF 能力单独判断，不能因支持原名路由而自动开放。2.5 请求 GIF 会在预占额度、入队和请求上游之前返回 400。

仅对这四个明确登记的型号，prepare 返回成功状态 `status=ok` 且无 `error` 时允许空 conduit token，后续省略空的 `X-Conduit-Token`。认证失败、挑战、错误/未知状态不放行，未登记型号不套用该兼容。

生成失败不自动换账号、降级型号或再次生成。账号仍受原有启用状态、认证和额度预占保护。
图片路由结果不写入聊天模型发现记录，避免把能出图错误地当成可聊天。

## 调用

同步 `POST /v1/images/generations`：

```json
{
  "model": "gpt-image-2.5-sunburst",
  "prompt": "白色背景上从左到右摆放蓝色立方体、红色球体和黄色圆锥",
  "n": 1,
  "size": "auto",
  "output_format": "png"
}
```

同步返回 `data[].b64_json` 和 `output_format`。沿用现有 GPT Web 约束：不发送 `response_format`。

异步使用 `POST /api/image-tasks/generations`，增加 `client_task_id`；轮询 `GET /api/image-tasks/{id}` 到成功或失败终态。
成功后通过 `data[].url` 下载。相同任务编号及参数复用已有结果，变更型号返回 409，重启后仍生效。

参考图/编辑沿用 `/v1/images/edits` 和 `/api/image-tasks/edits` 的现有协议。新增型号的参数传递有本地回归覆盖；这不等于所有账号的编辑、mask 和透明背景都已实测。
PNG 是 Web 请求的来源格式，JPEG/WebP 按原有逻辑在本地编码。尺寸与质量沿用 Web 提示语义，不能保证精确像素、2K/4K 或公开 API 的全部能力。

## 验证

本地回归覆盖四个显式型号的同步/异步生成和参考图传递、重启复用、变更型号冲突、聊天目录隔离、上游失败只提交一次，以及2.5拒绝GIF和2.6动画保真。

真实验证需逐项核对：实际prepare与conversation的发送型号、上游model_slug、终态、下载MIME、完整图片解码、真实尺寸及画面内容。上游型号回显只证明路由元数据，不提供独立底层引擎版本保证。

### 2026-11-05 受控实测

使用本分支候选代码的完整 HTTP 处理链，在隔离服务中各提交一次，使用已有记录为 Free 的账号及其日常出口；凭据仅在服务器内存、匿名管道和临时 tmpfs 使用。没有发布候选代码或修改生产业务库。

| 型号 | 接口 | 结果 | 实际原图 | 耗时 |
| --- | --- | --- | --- | --- |
| gpt-image-2.5-sunburst | 同步 generations | HTTP 200 | PNG，1536×1024 | 约17.6秒 |
| gpt-image-2.5-flare | 异步 generations | HTTP 202 → success，下载200 | PNG，1536×1024 | 约18.0秒 |

两次均只发送一次 prepare 和 conversation，发送的 model 与上游 model_slug 都对应请求的完整2.5型号；无旧模型替换或失败重生。输出通过完整PNG解码、独立PNG块CRC与压缩数据校验；逐张查看确认蓝色立方体、红色球体、黄色圆锥的数量、颜色和左右顺序符合提示词。2.5的GIF请求在真实上游请求前被拒绝。

这次实际验证的是无参考图静态生成。没有实测2.5的编辑、mask、透明背景、JPEG/WebP、精确尺寸、2K/4K或更多账号，不把本次结果推广为这些能力已验证。两次耗时只描述本次样本，不作为性能承诺。
