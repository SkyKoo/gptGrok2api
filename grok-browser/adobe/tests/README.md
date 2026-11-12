# 离线真实浏览器表单回归

`regional-consent-dom.mjs` 使用真实 Chromium 和合成页面，直接运行
`BrowserRegistration`、`PrivateRunStore` 和自动流程。它不属于普通 `*.test.mjs`
单元测试清单，必须显式执行：

```sh
node tests/regional-consent-dom.mjs
```

需要已有 Playwright 依赖和匹配的 Chromium 缓存。可通过 `PLAYWRIGHT_MODULE` 指定
已有依赖入口，`CHROMIUM_EXECUTABLE_PATH` 指定缓存浏览器；不下载浏览器或安装宿主依赖。
推荐在 `--network none` 的临时 Docker 容器执行，只读挂载源码及依赖，赋予独立临时目录。

所有请求都由测试拦截，只允许读取当前合成表单，其他 URL 或方法使断言失败；不会访问
Adobe、发码或创建真实账号。合成提交仅修改本页状态，不能代表真实上游注册成功。

验证范围：

- 韩国标记在 checkbox 自身和包裹 label 的完整批准流程；批准前不勾选，已选项不重复操作，
  只操作两项必选，营销保持原值，最终仅提交一次。
- 缺失、重复组/标记、重复输入、禁用、错误类型、未知必选和国家变化时阻断提交。
- 原有日本/泰国资料页及密码页到资料页流程、实际月份值。
- 泰国月份列表延迟挂载并重建原生 select 时等待完整列表；月份 3/4 分别提交值 2/3。
  永不加载时返回固定字段诊断并保持未提交。

`REGION_DOM_CASE` 可限定单个场景；不存在的名字会失败，不会报告零项通过。
`REGION_DOM_REPORT` 可写 JSON 结果，包含逐场景结果、浏览器版本、所有生产模块 SHA256、
测试脚本 SHA256 和执行期间源码未变校验。测试正常结束后清理临时会话目录。
