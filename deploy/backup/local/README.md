# CFM 本机日恢复点

本目录由 CFM 项目维护。`oracle-backup-daily.py` 仅处理 `/opt/gptgrok2api`，调用本项目
已部署的 `/opt/gptgrok2api/tools/backup-core.py`；依赖文件只允许当前项目，入口没有跨项目
服务选择参数。发布工具负责同步固定提交中的脚本及依赖摘要，1Panel 或 systemd 只调用
项目固定入口。调度、保留目录登记及历史备份清理由本目录的维护入口协调。

## 调用与同步契约

```sh
# 默认生成本项目的只读目录/保留计划。
/usr/bin/python3 -B /opt/gptgrok2api/tools/backup-local/current/oracle-backup-maintenance.py
# 经已批准的策略，补齐当日日恢复点并执行受保护轮换。
/usr/bin/python3 -B /opt/gptgrok2api/tools/backup-local/current/oracle-backup-maintenance.py --execute
```


固定 `current` 指向经审阅的不可变代码版本目录，四个 Python 模块和版本 manifest 随项目
同步。协调入口解析真实版本目录，再向 daily 子进程传同目录 `dependencies.json` 的绝对
路径；日常通过协调入口检查当天覆盖，避免直接调用 daily 重复生成完整备份。
`oracle-backup-daily.py --dependency-config <已解析的版本目录>/dependencies.json --execute`
仅用于受控首次验收或排障。私有状态及审计位于 `/opt/gptgrok2api/backup-policy/`，权限 0700。
调度单元每 15 分钟检查小型完成元数据；北京时间 05:20 后缺当日有效发布/日恢复点才采集，
成功验证后轮换。发布成功验收后触发本项目的维护单元。调度安装与启用状态以实际运维记录为准。

`dependencies.json` 使用 `ogg-daily-dependencies-v1`，`services` 仅包含 `gptgrok2api`；
其 `helpers` 将上述固定 helper 路径映射到经审阅版本的 SHA-256，`locks` 包含现有
`maintenance.lock`、`f4cb547-build.lock` 和核心备份的
`/opt/1panel/backup/ogg-cfm-core/backup.lock`。文件必须归 root 所有且不可被组/其他用户写入。
helper 版本改变时重新核对接口、范围和测试，再更新摘要；不能自动信任任意变化。

## 本机保留规则

保留最近 3 次成功发布对应的完整恢复点，以及北京时间最近 7 个自然日每天最新的 1 个
恢复点，按并集去重；始终至少保留 3 个经验证恢复点。当前回滚依赖、必要迁移点、未知
或中断批次单独保护。旧数据中缺少的日期不会伪造补齐，日常采集从启用后补足未来日期。

发布切换成功但最终验收未完成时，缺少 `retention-ready.json` 会阻止本轮日采集与清理。
新备份必须经过完整性及恢复验证，轮换再持部署锁重核引用、文件身份与归档；任何变化
停止删除。退出保留集合时清理整份恢复点，保留点内的原始 snapshot 不单独删除。
历史配置、未知用途及迁移点需逐项确认退出条件；以新的独立恢复验证证明替代后再调整
保护清单，不按目录年龄自动放行。删除前的小型私有审计材料保存在 `backup-policy/runs/`。

## 数据范围与一致性

包含核心备份当前允许的 JSON/加密 JSON、账号及任务状态、配置和 secrets，Adobe
浏览器认证资料，`data/files`、`data/log_input_images`，以及 PostgreSQL dump/globals
和 Redis RDB。保存允许业务树及认证存储树的空目录、UID/GID、权限和纳秒 mtime。
不纳入大日志、浏览器缓存/历史、活动锁和网关运行协议文件；完整范围写入每批 `scope`。

先流式复制媒体并验证内容 SHA-256，再进入核心快照窗口。媒体复制期间允许业务核心
正常更新；核心窗口仍沿用项目采集器对核心文件和 SQL 的前后稳定性检查，允许业务/认证
目录的集合、权限和完整身份也必须跨该窗口保持。窗口结束后用全部媒体的设备号/inode、
大小、权限/所有者及纳秒 mtime/ctime 核对已复制清单，避免再次读取大量媒体延长核心
稳定性要求；增删、替换和重设 mtime 的改写均会拒绝。业务继续运行，活跃浏览器资料、
核心窗口内变化或未知目录使本轮失败；任务不会暂停应用以强行满足校验。这是在线持久化状态恢复点，不是 PostgreSQL、Redis、外部任务之间
的分布式事务快照。新业务目录必须同步更新核心脚本、日备份范围和恢复测试。

归档完整读到 gzip EOF，并逐项验证文件摘要、目录和权限。数据库从已验证归档取出，
在 `network=none`、无生产挂载、有 CPU/内存上限的临时 PostgreSQL/Redis 容器恢复。
生产容器身份、镜像、状态、健康、重启、挂载和网络前后保持。需要至少 12 GiB 初始空闲，
归档前另检查归档容量及 2 GiB 余量；低 CPU/IO 优先级由入口及调度单元共同保证。

## 输出与失败处理

成功批次位于 `/opt/gptgrok2api/backups/daily-<UTC时间>/`，保留 `backup.tar.gz`、
`manifest.json`、`checksums.json`、`recovery-point.json`、`catalog-entry.json` 和 `complete`。
原始临时采集目录仅供本轮使用，完整验证后清理。先写登记材料，最后原子写入 `complete`；
缺少完成标记或存在 `failed.json` 的批次不参与成功恢复点保留规则。

失败保留已生成归档和小型失败标记，清理仅限本轮新建的暂存/恢复目录。存在未处置的失败
或不完整日备份时，后续运行拒绝新建包，避免每轮失败继续占用磁盘。运维应先检查失败原因、
归档价值和证据，再登记处理并将该失败批次移出自动日备份目录；不得把唯一可恢复资料当缓存。
本入口不执行历史轮换；任一采集、验证或恢复失败时，维护协调入口必须停止本轮清理。

## 恢复

先在私有隔离目录展开 `backup.tar.gz`，对照外置 manifest 校验。`payload/service/`
对应服务配置、核心状态和媒体，`payload/database/` 包含 PostgreSQL 与 Redis 副本。
保留 `.env` 和 secrets 中的加密密钥，核对原 UID/GID/权限与空目录；先恢复到独立数据库和
数据目录，验证业务读写。检查上游注册、生成和授权任务的真实终态，禁止盲目恢复队列后
重复执行外部操作。程序回滚与历史数据恢复分别执行，不覆盖已产生的新业务数据。

## 本地验证

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/backup/local -p 'test_daily_backup.py'
```

2026-11-13：16 项离线测试通过，覆盖元数据/空目录、gzip 尾部截断、内容篡改、链接与路径
逃逸、媒体复制期核心合法变化、核心窗口内媒体/目录变化与原核心采集器拒绝变化、锁冲突、失败包重试保护、登记写失败、完成标记顺序及跨项目参数拒绝。测试不接触
生产或运行 Docker；实际服务采集与隔离恢复需由当次运维安装验收记录证明。
