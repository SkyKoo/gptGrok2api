# CFM 核心备份

`cfm_core_backup.py` 由本项目维护；新增持久化数据、修改目录或数据库结构时，应同时更新
本脚本、恢复说明和测试。1Panel 只调用服务器固定入口：

```sh
/usr/bin/python3 /opt/gptgrok2api/tools/backup-core.py
```

O 机器通过 ogg 仓库的发布工具把固定发布提交中的本脚本同步到该路径。发布前核验摘要和
Python 语法，成功切换后原子替换；备份与发布共用 maintenance.lock。已有备份配置而新版
缺少脚本时拒绝发布，不能静默继续使用不匹配的旧数据清单。

## 范围

- `.env`、Compose、版本/槽位配置、access.json 和 secrets。
- data 顶层 JSON/加密 JSON（含 Adobe 重新登录任务元数据 adobe_relogin_tasks.json.enc），以及 image_tasks、video_tasks、grok_media_reviews、adobe_jobs。
- Adobe 注册恢复记录、加密账号与上游任务句柄，以及 browser-worker/adobe 中的私有 state.json 和浏览器认证存储（Cookies、Local State、Local/Session Storage、IndexedDB）。缓存、历史、锁文件与截图不入备份；活跃 profile 拒绝采集。
- 同批 PostgreSQL 自定义格式导出/全局角色，以及 Redis BGSAVE 的 RDB 副本。
- 排除图片、参考图日志、大日志、历史备份和运行锁；这不是完整媒体灾备。

文件名、目录、大小或格式不符合预期时失败。前后文件元数据/内容及 PostgreSQL 表摘要必须
一致，发现变化时最多重采三次；持续变化则不上云。这是在线持久化状态备份，不是数据库、
Redis、外部服务之间的事务原子快照。生产服务不会被暂停或重启。

## 1Panel 接入

私有状态位于 `/opt/1panel/backup/ogg-cfm-core/`，权限 0700。
`settings.json` 保存 `upload_job_id`、首次隔离恢复开关和脚本摘要，密码由已有原生目录任务
保存。加密上传辅助任务保持 Disable，由完整快照的采集器手动触发；目录来源固定为
`/opt/1panel/backup/ogg-cfm-core/cfm-core`。主 Shell 任务每日北京时间 03:50 执行，保留
策略由辅助目录任务负责，Google Drive 保留 14 份。

每轮下载云端文件并检查 MD5、解密、gzip CRC/EOF、逐文件 SHA-256/权限/所有者。
只有验证成功才清理本轮暂存目录。pending.json 或暂存目录存在时拒绝新一轮，避免未知结果
被覆盖；应先核对 1Panel 日志和云端记录再恢复。日志不会打印账号、令牌或密码。

## 恢复

需要云端加密归档和独立保存的恢复密码；先解密再解压。保留 `.env` 中的 OAuth/Adobe 解密密钥。Adobe 账号和任务使用相同的应用密钥派生加密密钥，丢失密钥无法恢复。浏览器认证文件只进入已加密的备份归档，保持原目录权限。
在无外部网络的隔离环境先恢复 PostgreSQL 与 Redis，并核对 manifest.json；不要直接覆盖
生产卷。检查注册、生成和授权任务是否已在上游完成，决定如何处置待执行队列后再启动应用
和工作者，不能盲目重放。图片/参考文件需另从媒体备份恢复。

首次验收会在无网络、无生产卷的临时 PostgreSQL/Redis 容器中恢复读取并核对表摘要，之后
删除测试容器。每天仅做完整文件和 dump 校验，避免重复运行恢复演练。

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/backup -p 'test_*.py'
```

Adobe 恢复时先核对主任务记录和 adobe_jobs 加密句柄；已提交任务只能恢复轮询与下载。
没有持久化句柄且提交结果未知的账号保留占用，不能根据余额未变化释放并重新生成。
浏览器 profile 文件恢复到共享 Worker 的 `/data/adobe`，不可让 Grok/Adobe 同时打开同一 profile。
此分支仅更新备份实现和离线测试，未改动 1Panel 任务或线上备份入口。
