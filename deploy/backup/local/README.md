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

## 浏览器运行时范围

日恢复点保留 `data/browser-worker` 的目录边界和 Adobe 认证资料，但不复制可重建的
`cloakbrowser` Chromium 运行时。核心 helper 会校验其顶层标记、`chromium-<版本>`
目录、普通文件/目录和 `chrome`/`chromedriver`，拒绝链接、特殊文件或未知状态；目录
枚举也不会把运行时标记当作认证目录。

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

目录枚举时，同一目录及共同祖先只在首次遇到时采集元数据，避免每个业务文件重复读取
同一父目录。仍遍历全部允许业务树、检查成员类型并保留空目录；枚举后的第二次读取与
核心快照前后的完整目录身份比较保持，包含 `data` 根目录的纳秒 mtime/ctime。

每轮日采集使用核心 helper 的 `snapshot_once()`；最多尝试 3 个完整窗口，每轮重建媒体
副本及目录基线，轮间等待 1 秒。文件读取/复制、目录检查、核心/SQL/Redis 采集中的明确
并发变化统一为 `SnapshotChanged`，不再让旧目录基线包住核心内部的多次重试。每次重试
仅丢弃本轮 `.staging`；未知范围、链接、权限、损坏格式和容量错误不重试。连续写入时
仍可能耗尽次数，不能把有限重试当作应用级协调快照或保证必定成功。

归档完整读到 gzip EOF，并逐项验证文件摘要、目录和权限。数据库从已验证归档取出，
在 `network=none`、无生产挂载、有 CPU/内存上限的临时 PostgreSQL/Redis 容器恢复。
生产容器身份、镜像、状态、健康、重启、挂载和网络前后保持。需要至少 12 GiB 初始空闲，
归档前另检查归档容量及 2 GiB 余量；低 CPU/IO 优先级由入口及调度单元共同保证。

## 输出与失败处理

成功批次位于 `/opt/gptgrok2api/backups/daily-<UTC时间>/`，保留 `backup.tar.gz`、
`manifest.json`、`checksums.json`、`recovery-point.json`、`catalog-entry.json` 和 `complete`。
原始临时采集目录仅供本轮使用，完整验证后清理。先写登记材料，最后原子写入 `complete`；
缺少完成标记或存在 `failed.json` 的批次不参与成功恢复点保留规则。

新的瞬时并发失败若发生在归档前，且容器身份保持、仅有本轮暂存，可在私有策略目录记录
有界失败证据后清除本轮暂存和空目录。后续采集冷却 60 分钟，北京时间每天最多 6 次失败
调用；协调器在冷却或达到当日限额时结束本轮，不重复复制媒体，也不进入清理。证据写入、
暂存清理或容器检查失败，以及已有归档、未知异常和其他残留，仍留下受保护失败标记。

已有失败目录不改写、不移动、不删除。仅含合法旧版 `failed.json` 的历史目录，在出现
时间更晚且重新完整核验通过的成功恢复点后，可不再全局阻断未来日采集；旧目录仍受保护，
不成为合格恢复点。没有更新恢复点、标记损坏、链接或任何额外产物时继续阻断。
冷却期间仍登记恢复点目录并产生小型维护日志；真正开始新日采集前，若存在上述旧标记，
需完整读取并复验一个更新恢复点，可能额外读取数 GiB。证明不能仅靠缓存的成功布尔值替代。
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
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/backup/local -p 'test_*.py'
```

2026-11-13：16 项离线测试通过，覆盖元数据/空目录、gzip 尾部截断、内容篡改、链接与路径
逃逸、媒体复制期核心合法变化、核心窗口内媒体/目录变化与原核心采集器拒绝变化、锁冲突、失败包重试保护、登记写失败、完成标记顺序及跨项目参数拒绝。测试不接触
生产或运行 Docker；实际服务采集与隔离恢复需由当次运维安装验收记录证明。

2026-11-18：本机备份全部 76 项、核心采集 20 项离线测试通过。新增完整窗口第二轮稳定
成功、持续变化有限失败、原子替换/删除、未知错误不重试、冷却与跨日预算、瞬时留证、
未知残留保护、旧标记字节/身份不变、更新发布/日恢复点完整复验及数据库恢复证据拒绝测试。
本次仅修改后续方法，尚未安装至线上或重新处理历史备份。

## 发布日的媒体恢复

定时任务每天建立独立的完整恢复点，覆盖生成图片、视频、输入参考图及同时点的核心状态。新发布归档通过 `complete.media_included=false` 标记精简范围。保留策略分别计算最近三个成功发布和最近七个自然日的含媒体恢复点，同日优先保留独立日备份。恢复历史媒体时从相应日备份的 `backup.tar.gz` 读取 `payload/service/data/files` 及 `payload/service/data/log_input_images`，按同批 `manifest.json` 校验。
