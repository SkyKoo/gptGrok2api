# CFM 双实例切换

CFM 保留 JSON、PostgreSQL 邮箱库和 Redis 队列。双实例是蓝绿程序槽位，业务状态始终只有一个写入者。用户已接受交接时新请求短暂排队；已有业务请求、生成流、异步任务和未完成授权先自然结束。长任务或持续请求无法排空时延期发布，旧服务继续处理请求。

日常发布通过 `GPTGROK2API_SOURCE_DIR` 指定独立、干净的源码检出，通过
`GPTGROK2API_EXPECTED_ORIGIN` 指定已审阅的 Git 远端。

## 运行方式

原 `gptgrok2api-app-1` 在首次交接后成为稳定入口，保留回环 8000 端口、Docker 网络和宿主机覆盖配置中的共享别名。蓝绿实例不发布宿主机端口、不领取共享别名；后台浏览器、PostgreSQL、Redis、反向代理和其他服务不随普通发布重建。控制接口仅绑定容器自身的 `127.0.0.1:9091`。

2026-11-11，共享浏览器容器已更名为 `cfm-browser-worker`，Compose 服务仍为 `grok-browser`，
两个 DNS 别名均保留。发布工具依据 `current.json` 的 `grok_browser_container` 检查实际容器；
没有该字段的旧部署继续使用 `gptgrok2api-grok-browser-1`，未知名称拒绝。旧布局发布器会保存
实际容器名称，后续 bootstrap/rolling 均沿用该身份，不因普通 app 发布重建共享 Worker。

候选实例只开启监听及控制接口，取得数据目录的独占 `flock` 前不加载配置、JSON、数据库或队列。流程是：

1. 构建固定提交的 ARM64 app 镜像，验证 revision、协议标签、镜像 ID、共享依赖和磁盘空间。候选保持 standby。
2. 暂停旧实例自动调度及队列消费的新工作，不修改用户保存的开关/下次时间；HTTP 继续服务，已接收任务自然完成。在线预复制数据与日志。
3. 入口原子检查业务请求归零后短暂排队；旧实例不可逆封存后台写入能力，再释放写入租约。仅管理页注册状态观察流会断开重连，业务 SSE 必须完整结束。
4. 完成冻结增量、PostgreSQL 自定义格式/全局对象备份和 Redis SAVE/RDB 副本。候选取得租约、读取最终数据，仍暂停后台工作；严格比较全部业务文件与邮箱数据库行/结构摘要。
5. 入口持久化路由后开放 HTTP，再恢复候选后台调度；旧槽位只在已封存且空闲后停止。备份压缩和完整读取校验在恢复流量后完成，旧/新哈希静态资源共存以支持已打开的页面。

入口最多排队 512 个请求，单请求最多等待 60 秒；控制交接预算默认 25 秒，上限 30 秒。发布入口还使用 60–1200 秒的全链路预算，覆盖滚动工具从候选预检到最终归档的前进路径，并在 `rolling-status.json` 和发布回执中记录阶段耗时与剩余预算。写入者冻结后，快照、数据库和业务文件保护校验使用入口等待预算减 5 秒的独立数据窗口（当前通常为 55 秒），并在回执中记录实际耗时；任一阶段超时都会进入异常处理，由切流和数据校验结果决定回退或保留现场。全链路预算耗尽时停止前进并保留可核对现场，恢复流程仍按安全边界执行。数据窗口从冻结后重新计时，不扣除此前排空与控制交接耗时，因此总持流时间可能超过单请求等待预算。不读取和转发排队中的请求体，不自动重放 POST，不强杀运行任务。排队满返回 429，超过等待预算返回 503；用户/客户端自身较短超时、主机/入口故障和超预算恢复仍可能影响请求，因此不能将有限排队表述为任何故障下都无错误。

## 首次迁移前提

迁移前的线上版本 `21af195` 没有写入租约/交接协议。**首次升级到支持协议的镜像仍需一次旧机制的维护发布**，不能凭脚本消除旧进程原本不具备交接能力造成的维护窗口。旧布局仍须满足这些前提。`compose.yaml` 已添加协议启动环境；首次升级时原容器仍作为业务实例，启动成功会创建精确校验的私有协议标记。

正式执行前应另行登记 `OPERATIONS_LOG.md` 计划，确认发布版本、活动任务、备份、资源、维护窗口及回退条件。首次协议升级和同镜像 bootstrap 是两步独立操作；协议升级未通过时不运行 bootstrap。工具安装本身不会重启服务。

在支持协议的同一提交/同一镜像已完成维护升级并健康运行后：

```bash
# 部署主机普通用户；所有占位值都需替换为已核对的实际值。
"$CFM_RUNTIME_ROOT/tools/deploy.sh" rolling prepare \
  --source "$GPTGROK2API_SOURCE_DIR" --image '已运行的固定镜像tag' \
  --output /tmp/cfm-bootstrap.json
"$CFM_RUNTIME_ROOT/tools/deploy.sh" rolling bootstrap --release /tmp/cfm-bootstrap.json
"$CFM_RUNTIME_ROOT/tools/deploy.sh" status
```

bootstrap 用当前版本启动 blue，在原地址排队并交接；原进程从此只转发请求，重启也不会加载旧业务缓存或取得写入租约。业务槽位和入口的角色/路由均通过落盘、fsync 的状态恢复；中断的初始化不会自动重试。

## 日常发布与兼容性

CFM 核心备份脚本由业务仓库 `deploy/backup/cfm_core_backup.py` 维护，1Panel 固定调用
`$CFM_RUNTIME_ROOT/tools/backup-core.py`。prepare 将脚本的固定提交摘要加入发布清单；
deploy 在切换前从该提交读回校验，成功后原子安装。已有备份入口而新提交缺失脚本时拒绝
发布。修改持久化布局时须同时更新该脚本及恢复测试，面板中的调用命令无需改动。

bootstrap 成功后，以下命令采用双实例布局。执行 `deploy` 或 `start` 前，先将已审阅的
检出路径和远端分别设置为环境变量 `GPTGROK2API_SOURCE_DIR` 和 `GPTGROK2API_EXPECTED_ORIGIN`。

```bash
"$CFM_RUNTIME_ROOT/tools/deploy.sh" deploy
# 或后台启动及查询
"$CFM_RUNTIME_ROOT/tools/deploy.sh" start
"$CFM_RUNTIME_ROOT/tools/deploy.sh" status
```

普通发布只构建 app；不重建共享 Grok Worker。源码必须是干净 main，固定 commit 与本次镜像标签一致。状态/浏览器源码指纹变化会拒绝自动切换：先审查新旧 JSON、邮箱 schema、Redis 任务及浏览器接口的双向兼容性。兼容改动可提供只包含下列两个精确指纹对象的私有 JSON，使用显式入口：

```json
{"previous":{"state":"旧指纹","browser":"旧指纹"},"candidate":{"state":"新指纹","browser":"新指纹"}}
```

```bash
"$CFM_RUNTIME_ROOT/tools/deploy.sh" rolling prepare \
  --source "$GPTGROK2API_SOURCE_DIR" --image '已构建的固定镜像tag' --output /tmp/cfm-release.json
"$CFM_RUNTIME_ROOT/tools/deploy.sh" rolling deploy --release /tmp/cfm-release.json \
  --compatible-contract /tmp/cfm-reviewed-compatibility.json
```

兼容清单不放宽启动前后数据比较。需要写入迁移、重建浏览器/数据库/Redis、修改入口基础设施的版本，应另行安排受控维护及双向兼容方案。旧 `apply`、旧 `rollback` 和直接旧 `deploy.py` 在滚动布局或交接状态下被拒绝，避免误重建稳定入口。程序回退以已验证兼容的旧镜像生成新 release（base_commit 为当前版本），通过同一交接流程进行；不会恢复历史数据。

## 失败处理与备份

每日媒体策略启用后，完成标记中的 `media_included: false` 表示发布归档只覆盖核心数据。图像、视频和输入参考图从每日完整备份恢复。写入者交接的冻结清单仍校验共享媒体的内容摘要。

新发布在冻结时直接写入一个 `snapshot.tar.gz`。程序回滚成功后，`current.json.backup` 沿用发布前的恢复点。发布器校验该恢复点的归档、PostgreSQL 和 Redis 文件摘要，再清理本次已回滚批次的同类文件，结果写入该批次的 `rollback-cleanup.json`。恢复点缺失、文件异常或状态待确认时，保留现场供复核。

旧实例封存前失败：恢复旧后台准入/入口，保留原服务。封存后、开放新流量前失败：在确认没有新业务及数据未异常变化时，用旧镜像新建实例并读取当前数据恢复；不会复活旧内存缓存。候选初始化改变业务数据/schema、无法确认租约/控制结果或新流量已开放时：返回 `needs_attention`，保留实例、数据和入口状态供核对，不冒险自动回退。入口仍 held 时请求只能在预算内等待，须及时检查，不能重复执行未知结果的切换。

进度：`rolling-status.json` 及本次 `backups/rolling-*/release.json`。旧版本指针：`previous.json`。备份含 data/logs、.env、Compose、current.json、PostgreSQL dump/globals、Redis RDB；只有压缩包完整读到 EOF 且写入 checksums.json/complete 后才标为完整。原始快照随所属恢复点保留；已启用的项目自有保留工具可在最终验收后清理过期的完整恢复点，规则见下节。`business-before.json` 仅存文件/数据库哈希，运行状态只输出任务计数；快照、发布配置和运行清单仍含敏感内容，按私有权限保护。业务文件比较只精确排除三个协议文件，不忽略账号、OAuth 或图片目录。

## 项目自有本机备份保留入口

CFM 源码维护 `deploy/backup/local/` 中的四个 Python 模块及 `manifest.json`。
`prepare` 从固定 Git commit 读取整包，核对服务身份、完整文件集合、SHA-256 和 Python
语法，并将清单摘要写入 release。执行发布时重新从同一 commit 核对；已有本机保留包而
候选提交缺少包时拒绝发布。旧维护发布入口由 `deploy.sh` 传递 `--source` 完成同样校验。

成功发布把代码保存到 `$CFM_RUNTIME_ROOT/tools/backup-local/versions/<清单摘要>/`，
通过 `current` 符号链接原子切换；`dependencies.json` 是维护锁内生成的部署配置，
绑定本次已同步的核心备份脚本摘要和实际协调锁。运行清单与审计另存
`$CFM_RUNTIME_ROOT/backup-policy/`，不修改历史冻结的一次性发布脚本。

启用此包的新发布先写 `retention_requires_final_verification`。滚动入口在维护锁内
再次核对当前 commit/镜像、活动写入租约、路由、readyz 和共享依赖健康，随后写入本批
`retention-ready.json`。缺少匹配最终验收标记时，当前服务的日备份和清理均暂停。
只有入口完成验收并释放维护锁后，才异步启动
`gptgrok2api-backup-retention.service`；任务排队失败记录 deferred，不回滚已经健康的应用。
直接调用 `RollingRelease.deploy()` 的外部流程须在自己的最终验收结束后补齐该标记。

普通恢复点取“最近三次成功发布 + 北京时间最近七个日历日每天一个”的并集，
当前回滚依赖、必要迁移点、未知或未完成备份另行保护。首次单写协议 bootstrap 恢复点
保留，直到现行协议的独立恢复演练能替代它后再审阅退役。保留点的原始快照不单独删除。
实际启用、首次清理及定时任务状态以运维日志为准。

## 本机验证

```bash
python3 -m unittest discover -s deploy/release-tools -p 'test_*.py'
# 仅在已有本机 postgres:17-alpine、redis:7-alpine 镜像时显式启用：
CFM_BACKUP_DOCKER=1 python3 -m unittest discover -s deploy/release-tools -p test_backup_docker.py
# 在 CFM 源仓库编译后，使用本机临时回环端口及虚构配置：
python3 deploy/release-tools/rehearse-local.py /path/to/gptgrok2api
```

演练覆盖原入口转为转发器、blue→green、24 个并发排队配置写入、25 项配置持久保留、拒绝第二个写入者、初始化不改业务文件、入口与活动实例重启恢复。独立无网络 PostgreSQL/Redis 容器另验证了行/结构摘要、实际 pg_dump→pg_restore 恢复、Redis RDB 完整性和归档校验，测试容器已删除。Go 测试另覆盖延迟账号反馈刷盘与封存、业务 SSE 完整结束、POST 只提交一次、取消/限流/等待超时、路由 CAS、遥测关闭、后台任务封存及 Redis 消费归还。首次线上迁移另验证了当时主机的 Docker/ARM64、共享依赖、数据比较、租约、路由和公网链路；后续发布仍须按当时数据规模与资源状态核对完整冻结预算。
