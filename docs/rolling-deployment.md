# 文件状态的单写入者交接

CFM 可配合稳定入口与蓝绿部署工具进行有界排队交接，保留现有 JSON、邮箱 PostgreSQL 与 Redis。候选实例不会在取得独占写入租约前加载配置或业务存储。旧实例先暂停自动调度的新工作，继续提供业务 HTTP；自然排空已有请求、生成流、后台任务及未完成授权，再不可逆封存写入能力、刷新延迟账号反馈并释放租约。新实例读取最终数据后才切流。用户保存的调度开关和下一次执行时间不因发布暂停而改写。

部署工具位于配套运维仓库 `ogg/deploy/gptgrok2api`，详细操作见其中 `ROLLING.md`。普通运行不设置控制变量时仍使用原启动方式。没有交接协议的旧版本，首次协议升级需要维护窗口；不能直接以两个旧程序共享目录来实现蓝绿。

| 环境变量 | 用途 |
| --- | --- |
| `CFM_ROLLING_CONTROL_ADDR` | 开启受控启动；必须为容器回环 `127.0.0.1:<端口>` |
| `CFM_WRITER_LEASE_FILE` | 所有业务实例共享的绝对 flock 路径 |
| `CFM_ROLLING_STATE_FILE` | 本实例私有的绝对持久状态路径，不可由两个槽位共用 |
| `CFM_ROLLING_BOOTSTRAP_ACTIVE` | 仅首次协议升级的原 app 设为 `true`；蓝绿候选始终 `false` |
| `CFM_GATEWAY_STATE_FILE` | 原 app 的稳定入口路由文件；蓝绿业务实例留空 |
| `CFM_GATEWAY_SLOT_URLS` | 逗号分隔的两个明确 HTTP 上游，默认 `http://cfm-blue:8080,http://cfm-green:8080`；本机演练可用回环端口 |

控制协议标签为 `cfm-single-writer-v1`。私有控制服务提供 GET `/status` 及 POST `/activate`、`/pause`、`/resume`、`/freeze`。只有 standby 可激活；封存不可恢复旧缓存。初始化失败保留租约直到进程退出，中断的初始化不会在重启时自动重试。

原 app 可用 POST `/gateway/init` 创建 held 入口；封存后原公开地址只执行转发。路由服务 GET `/gateway/status`，POST `/gateway/hold`、`/gateway/route`、`/gateway/release` 均需提交当前 `epoch`，route 另需 `upstream`。控制地址不发布到宿主机或业务网络；通过 `docker exec` 在实例自身访问。

默认入口排队上限 512、等待上限 60 秒；只排队未提交请求，不读取其请求体，也不自动重放业务请求。队满返回 429，等待过期返回 503。业务 SSE 被完整计入活动请求；仅 GET `/api/register/events` 管理观察流可在交接时断开重连。路由及实例状态原子落盘并 fsync，入口重启保留 held 状态，业务重启只恢复已确认的活动槽位。

候选构造期间不启动后台循环和 Redis 消费；`/readyz` 仅在已激活的业务实例返回成功。独占租约释放前刷新并封存 JSON 账号延迟写入器，停止自动调度及消费者；不能只看 HTTP 请求数判断文件已静止。`cfm-gateway` 二进制提供独立入口实现，当前同地址迁移采用原 app 内嵌入口，以保留现有端口和网络身份。

失败回退只更换程序，不恢复旧数据库/文件。新流量可能已开放、初始化改动数据或控制结果无法确认时，必须保留现场；不能为了快速回退停止新业务、取消任务或恢复旧进程缓存。需要写入迁移、浏览器依赖升级或入口自身升级时应使用独立维护方案。
