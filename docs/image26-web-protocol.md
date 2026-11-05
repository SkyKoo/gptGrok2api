# Image 2.6 与 GIF 的 Web 接入

验证日期：2026-11-05。此实现使用 ChatGPT Web，不调用公开 OpenAI Images API。
上游模型和账号能力可能变化；结构化模型名称不等于对底层图像引擎身份的独立保证。

## 请求

新增图片模型 `gpt-image-2.6-sunburst` 和 `gpt-image-2.6-flare`。它们按指定名称发送到 Web，
不会套用旧 `gpt-image-2` 的“图片会话模型”设置，也不依赖聊天选择器列出这些名称。
旧模型的路由设置保持原有行为；2.6 型号只加入图片目录，不冒充聊天模型。

同步：`POST /v1/images/generations`。异步：`POST /api/image-tasks/generations`，
异步提交还需要 `client_task_id`，通过 `GET /api/image-tasks/{id}` 获取终态。

```json
{
  "model": "gpt-image-2.6-sunburst",
  "prompt": "蓝色小球轻轻上下弹跳，白色背景，无缝循环",
  "n": 1,
  "size": "auto",
  "output_format": "gif",
  "frame_count": 9,
  "frame_duration_ms": 200
}
```

- 默认仍输出 PNG；需要动画时必须显式设置 `output_format=gif`，并选择 2.6 模型。
- `frame_count` 默认9，接受2–16。`frame_duration_ms` 默认200，接受20–2000，按10毫秒四舍五入。
- 两个帧参数只适用于GIF。GIF只支持生成，不支持图片编辑、参考图、mask、stream、partial_images、output_compression或显式透明/不透明背景；background可省略或设为auto。
- 参数通过提示词交给 Web 图片工具，未添加未经验证的顶层 Web 字段。收到文件后严格校验帧数和各帧时长，不满足时返回错误，不重新生成或用静态PNG充数。
- 尺寸保持现有Web提示语义，不保证精确像素。建议使用auto；首个请求提示256×256，原始GIF实际为410×413。
- GPT同步接口返回`data[].b64_json`及顶层`output_format=gif`，必须省略`response_format`；显式传入`url`或`b64_json`都会返回400。异步任务通过`data[].url`返回下载地址，URL不保证以`.gif`结尾，下载MIME为`image/gif`。图库保留原始文件字节、帧延迟、循环及disposal信息。
- 异步任务查询中的`frame_count`、`frame_duration_ms`回显显式提交的参数；例如请求125ms时该字段仍为125，实际GIF每帧时长为取整后的130ms。省略的帧参数不会新增到任务查询结果中。
- 新模型的生成错误不自动换号重生；账号仍必须通过现有启用、认证状态和生图额度检查，上游拒绝按实际错误返回。

## 已观察的 Web 契约

`POST /backend-api/f/conversation/prepare`可返回HTTP200及`status=ok`、空`conduit_token`。
仅对明确登记的2.5/2.6 Sunburst、Flare名称接受这一成功组合，后续省略`X-Conduit-Token`。
旧`gpt-image-2`、未登记型号或错误/缺失状态保持拒绝。原名路由不代表支持动画；GIF仍只开放给两个2.6型号。
2.5静态图片接入见[GPT Web图片模型](image-web-models.md)。

生成继续使用`POST /backend-api/f/conversation`的SSE，随后读取终态conversation：

```json
{
  "content_type": "image_asset_pointer",
  "asset_pointer": "sediment://animation-primary",
  "mime_type": "image/gif",
  "metadata": {
    "generation": {
      "animation": {
        "rows": 3,
        "columns": 3,
        "frame_count": 9,
        "frame_duration_ms": 200
      },
      "spritesheet": {"asset_pointer": "sediment://static-spritesheet", "mime_type": "image/png"},
      "gif_speed_variants": {"2": "sediment://animation-speed2"}
    }
  }
}
```

上例是使用替代ID的结构示意。只交付主asset_pointer；spritesheet和速度变体不是额外生成结果。
上游可能另给出Python生成的附件、文字下载链接或自报尺寸，这些不能代替原始图片资产和文件验证。

下载继续使用已有conversation attachment接口，无需新增视频接口。GIF通过容器扫描限制帧数和累计画布像素后，
再完整解码验证所有帧，防止只解码首帧便接受截断动画或在解码前分配过多内存。
普通图片请求若意外收到多帧GIF会明确报错，提示改用gif输出，不静默压成首帧。

异步任务持久化帧参数并纳入请求摘要；相同client_task_id变更帧数或时长返回409，重启后保持这一规则。

## 官方资料与验证边界

- [OpenAI图像指南](https://developers.openai.com/api/docs/guides/image-generation)
- [Images生成接口参考](https://developers.openai.com/api/reference/resources/images/methods/generate)

两份公开API文档的帧数上限分别为64和16，帧时长上限也不一致。CFM暂取较保守的参数范围；
这不是对Web全部参数组合或所有账号权限的承诺。真实样本验证默认9帧/200ms，其他边界由本地回归检查。
公开API与Web的参数位置不同，不把官方公开API的字段原样猜测转发给Web。

本轮仅开发和隔离验证，未自动部署，也未修改New API渠道、插件或计费设置。
