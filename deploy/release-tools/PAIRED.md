# CFM app 与共享 Worker 配套发布

核对日期：2026-11-16。入口是 `paired-deploy.py init --plan reviewed.json --output op`：
它把已审阅的计划和当前模板生成到新的私有操作目录，不连接 Docker、不发布应用。
后续在该目录运行构建、预检和发布。生成目录的末级名称必须等于计划中的
`operation`，且不能已存在；模板及计划从生成起由 `bundle-manifest.json` 固定。

首批使用本模板的是 2026-11-15 的 `42df902` 发布。历史发布记录见 O 机运维仓库的
`OPERATIONS_LOG.md` 和 `operations/`。发布冻结快照与最终归档均由本工具处理。

## 适用边界

- 当前支持既有配套发布拓扑、Linux ARM64、root 运维执行，以及
  `cfm-single-writer-v1`。源码必须为干净、固定的 `main` 或 detached HEAD，候选是基线后继（或 App 已先行 rolling 至该提交、仅将旧 Worker 追平至同一提交）。
- app 与共享 `cfm-browser-worker` 使用同一候选 commit；旧 Worker commit 可以与旧 app 不同，
  必须分别读取并固定。Worker Compose service 保持 `grok-browser`，容量保持 1。
- 仅支持无 SQL schema 迁移、Worker 仅变更镜像的发布。网络、端口、许可、挂载、环境和容量
  变化需要另行实现与审阅，不能只修改计划字段绕过校验。
- 安装的 rolling helper、backup-local 包、前后 state/browser 契约均固定为精确摘要。
  业务源码变更与对应验收规则须一并审阅；生成器不会自动证明两版本兼容。
- 原始快照覆盖文件内容、路径类型、数值属主、权限、mtime、空目录及数据库；冻结时严格保护
  业务数据。激活结果未知后保留实例和现场，不恢复旧业务数据，不重放注册、OAuth 或生成请求。
- 每次远端变更、缓存准备或安装前，按运维仓库 `ogg/OPERATIONS_LOG.md` 登记目的、风险和恢复边界。
  失败、中断及续验记录保留；既有日备份问题不因发布验收成功而自动解决。

## 计划结构

`reviewed.json` 是本批完整 JSON 对象。下面是字段结构，尖括号值必须用本次核对结果替换，
不能直接执行，也不能直接沿用历史批次的 commit、镜像或摘要。

```json
{
  "operation": "cfm-paired-release-20261115",
  "base_commit": "<40位小写Git提交>",
  "candidate_commit": "<另一个40位小写Git提交>",
  "branch": "main",
  "schema_migration": false,
  "worker_change": "image_only",
  "rolling_sha256": "<已安装rolling-deploy.py的64位小写SHA256>",
  "backup_maintenance_lock": "<宿主机共享备份锁的绝对路径>",
  "backup_policy_package": "<已安装且候选携带的backup-local包64位标识>",
  "compatible_contract": {
    "previous": {"state": "<64位SHA256>", "browser": "<64位SHA256>"},
    "candidate": {"state": "<64位SHA256>", "browser": "<64位SHA256>"}
  },
  "state_contract_inputs": ["<审阅后的源码相对文件或目录路径>"],
  "worker": {
    "service": "grok-browser",
    "container": "cfm-browser-worker",
    "architecture": "arm64",
    "python_cloakbrowser": "<x.y.z>",
    "python_playwright": "<x.y.z>",
    "node_cloakbrowser": "<x.y.z>",
    "node_playwright": "<x.y.z>",
    "node_minimum_major": 22,
    "capacity": 1,
    "commit": "<与candidate_commit相同>"
  },
  "previous_worker": {
    "container": "cfm-browser-worker",
    "commit": "<旧Worker的40位Git提交>",
    "image": "<旧Worker的固定镜像tag>",
    "image_id": "sha256:<64位小写镜像ID>"
  },
  "note": "本次变更、兼容依据和明确边界"
}
```

| 字段 | 约束与来源 |
| --- | --- |
| `operation` | `cfm-` 开头，后接小写字母/数字/连字符；对应生成目录名。 |
| `base_commit` / `candidate_commit` | 完整 40 位小写提交；基线来自当前线上 App，候选来自本次审阅后的源码。当 App 已先行 rolling 至候选提交时允许两者相等，此时 `previous_worker.commit` 必须仍异于 `candidate_commit`。 |
| `branch` / `schema_migration` / `worker_change` | 分别固定为 `main`、布尔值 `false`、`image_only`。 |
| `rolling_sha256` | `$CFM_RUNTIME_ROOT/tools/rolling-deploy.py` 的完整 SHA-256；不把模板摘要误当成安装文件摘要。 |
| `backup_maintenance_lock` | 新计划必填。使用宿主机备份作业共用的绝对锁路径，apply 和 finalize 都读取操作包内的同一值。 |
| `backup_policy_package` | 已安装 backup-local 版本目录的 64 位包标识；候选镜像携带的包须精确一致。 |
| `compatible_contract` | `previous` 与 `candidate` 各恰有 `state`、`browser` 两个 64 位摘要；分别与 current/release 中的契约一致，兼容关系须有源码审阅依据。 |
| `state_contract_inputs` | 非空、无重复的源码相对路径列表；不得绝对路径、反斜杠或 `..`。路径须存在，应完整覆盖本批状态及浏览器变更，不能缩小列表规避差异。 |
| `worker` | 固定现有 service/container/架构/容量；四个依赖版本为精确 `x.y.z`，Node 主版本下限为整数且至少 22；commit 与候选相同。 |
| `previous_worker` | 当前 Worker 的容器名、完整 commit、tag 和不可变 `sha256:` 镜像 ID；不从旧 app commit 推算。 |
| `note` | 可选审阅说明。历史计划的 `worker.health_service`、`worker.image` 可作为附加记录；实际候选 tag 由 `build.py` 参数/default 决定并重新验证。 |

版本号来自候选锁文件和离线运行时探针，不能按最新版本推测。前后契约由已固定 rolling helper 的
`source_contract` 规则及线上 `current.json` 核对；`state_contract_inputs` 的逐文件摘要另由
`common.reviewed_state_contract` 记录。计划不能包含凭据或真实账号内容。

## 生成与准备

先使用独立安装器安装模板；它不调用原 `install.sh`，也不覆盖既有 `deploy.sh`、
`rolling-deploy.py` 或业务配置。调用方提供目标 SSH 别名及运行根目录，使用普通 SSH 账号并具备
`sudo -n` 权限。执行前登记运维计划；命令本身不构建、发布、重启容器或清理备份。

```sh
./deploy/release-tools/install-paired.sh --help
./deploy/release-tools/install-paired.sh "$CFM_DEPLOY_HOST" "$CFM_RUNTIME_ROOT"
```

安装器固定生成器、`paired/` 模板、`snapshot_integrity.py`、`paired_phases.py` 和本说明的内容清单。
远端先在普通用户的私有临时目录按固定成员名单解析归档、逐项验证摘要，再由 root 重新读取校验，
写入 `$CFM_RUNTIME_ROOT/tools/paired-deploy/versions/<内容SHA>`。完整验证后原子更新 `current`
相对符号链接；旧版本保留。当前版本或目标版本校验失败时拒绝切换，不覆盖损坏版本。
rolling 工具从同一个 CFM 检出执行 `./deploy/release-tools/install.sh "$CFM_DEPLOY_HOST" "$CFM_RUNTIME_ROOT"` 安装。
多服务宿主机可将 Compose 覆盖文件作为 rolling 安装器的第三个参数。
每次安装 rolling 新版后，重新采集已安装文件的 SHA-256 并填写发布计划。
离线检查可运行 `install-paired.sh --package /private/tmp/cfm-paired-tools.tar.gz`；输出文件必须不存在，
该模式不连接服务器。内容版本标识覆盖全部工具及说明，安装输出会给出精确 SHA。

在本仓库根目录运行；此步骤可在本机执行。示例批次名须与已审阅计划一致。

```sh
python3 deploy/release-tools/paired-deploy.py init \
  --plan reviewed.json \
  --output operations/cfm-paired-release-20261115
```

生产批次直接在部署主机生成。先读取已安装 rolling 脚本的摘要，与本次审阅的工具版本核对后填写计划中的 `rolling_sha256`。
从候选 CFM 检出启动并传入这份计划。生成器读完 JSON 并核对主机工具摘要后，在目标文件系统的临时目录生成工具包，逐项复核清单和文件摘要，再原子改名为正式批次。

```sh
sudo -n sha256sum "$CFM_RUNTIME_ROOT/tools/rolling-deploy.py"
python3 deploy/release.py --root "$CFM_RUNTIME_ROOT" reviewed.json
```

旧批次保留原有文件和时间记录。使用连续入口运行已有批次时，发布器会在执行其中的脚本前复核完整清单和主机工具摘要。构建入口也会检查当前批次的文件。新修订应生成新批次。

下列命令在部署主机的 root 运维 shell 中执行。源码目录先固定到计划中的候选提交；此处不执行
`git pull`，以免构建时改变已经审阅的身份。构建至少要求 12 GiB 可用空间；发布预检使用流式 direct 快照，按业务目录大小加 4 GiB 归档余量和历史最大数据库导出预留计算空间；发布预检还要求
至少 `3 × 当前 data/logs 字节数 + 2 GiB`。不能自动清理未知备份来满足条件。

```sh
cfm_root="$CFM_RUNTIME_ROOT"
cfm_op="$cfm_root/operations/cfm-paired-release-20261115"
cfm_source="$(git rev-parse --show-toplevel)"

python3 "$cfm_op/build.py" --root "$cfm_root" --operation-dir "$cfm_op" \
  --source "$cfm_source"
```

默认 tag 为 `gptgrok2api:sha-<候选前12位>`、`cfm-browser-worker:sha-<候选前12位>`。
已有同一候选镜像时会自动探测并复用；复用仍检查不可变 ID、ARM64、revision、app 协议及
Worker 源码/运行时探针，不启动真实浏览器。已存在但身份不匹配的 tag 会停止，只有明确传入
`--force-build` 才允许覆盖。输出为 `release.json`、`worker-manifest.json` 和 `build-proof.json`；
已存在构建证据时停止核对，不覆盖重建。

随后核对 Worker 配置并准备回退所需缓存，再采集紧邻发布的基线：

```sh
python3 "$cfm_op/worker-release.py" --root "$cfm_root" --operation-dir "$cfm_op" \
  --candidate-manifest "$cfm_op/worker-manifest.json" --action inspect
python3 "$cfm_op/worker-release.py" --root "$cfm_root" --operation-dir "$cfm_op" \
  --candidate-manifest "$cfm_op/worker-manifest.json" --action prepare-cache

python3 "$cfm_op/preflight.py" --root "$cfm_root" --operation-dir "$cfm_op" \
  --release "$cfm_op/release.json" --worker-manifest "$cfm_op/worker-manifest.json" \
  --output "$cfm_op/baseline.json"

python3 "$cfm_op/release.py" --root "$cfm_root" --operation-dir "$cfm_op" plan \
  --release "$cfm_op/release.json" --worker-manifest "$cfm_op/worker-manifest.json" \
  --baseline "$cfm_op/baseline.json"
```

每个操作目录会追加 `pipeline-events.jsonl`，并在 coordinator 结束时写入
`pipeline-timing.json`。它覆盖构建、镜像复用、Worker 准备、预检、plan、交接、归档和最终验收，
因此“发布开始”从操作目录首次建立时计时，不再只看 `prepared` 之后的窗口。相同候选的 ARM64
镜像若已通过不可变 ID、架构、OCI revision 和协议标签核对，`build.py` 自动复用，不重复构建；
需要构建时 App 和 Worker 并行构建，镜像准备和 Worker manifest 也并行执行，单个镜像默认 900 秒超时，失败只保留证据，不自动重建。

`pipeline-events.jsonl` 的首条记录在 `paired-deploy.py init` 开始时写入，所有前进阶段共享同一个
1200 秒截止时间。build、Worker 准备、预检、plan、apply、归档和最终验收在启动前检查剩余时间，
每个连续入口子进程也受当前剩余时间和自身上限的较小值约束；预算耗尽后停止继续前进并保留操作证据，不能通过
分开执行命令把总耗时重新计时。若流量已经恢复且状态为 `archive_pending` 或 `success`，`finalize`
只允许归档续作和只读验收，继续使用 `init` 时建立的同一个 1200 秒截止时间，不重新计入交接或切流，也不能通过重复调用延长该窗口。

交接失败后的必要恢复由 `release.py` 在内部执行：它可以清除前进 deadline，以恢复已 hold 的入口或旧 writer，
但 coordinator 不再追加固定恢复宽限。前进预算因此保持严格；若恢复结果未知，保留现场并按状态证据人工处理。

`coordinator.py` 的 App/Worker 交接使用一个共享的自然排空窗口，默认 600 秒。入口 hold 前后的两次
排空检查共用同一个 deadline，不再各自拥有 600 秒或历史上的 1200 秒。达到 deadline 会安全停止并按
原状态进入回退或 `needs_attention`，不强杀任务。切流后两个互不依赖的只读验收并行执行。完整归档
回读生成的 `archive-report.json` 与归档身份、manifest 摘要和压缩包 SHA 绑定，后续验收在身份未变时
复用这份证明，旧批次缺少该证明时自动回退完整归档读取。

首次 `preflight.py` 会完整读取 `data`/`logs` 并检查浏览器 profile 锁，记录 `business_bytes`。随后
`plan` 和 `apply` 只复核已冻结的业务大小、磁盘余量、容器/路由/Worker/备份策略和文件身份，不再对
约数万业务文件重复执行同样的递归遍历。真正的冻结快照仍会重新校验文件集合、链接、身份、归档和
数据库一致性，前置扫描不会替代冻结校验。

发布预检不再启动隔离候选或复制生产数据。候选初始化、唯一写入者租约和数据不变性在一次发布维护
窗口内验证：先自然排空并冻结旧实例，随后使用正式快照工具完成业务文件、数据库、权限和归档校验，
再激活暂停的候选并执行 state guard；`prepare-cache` 在
维护窗口前执行，不切换服务镜像。

## 唯一一次交接与最终验收

启用每日媒体备份后，新发布归档包含账号、任务、配置、媒体说明及数据库导出。生成图片、视频和日志输入图的文件内容由每日完整恢复点保存，切流校验仍覆盖这些共享文件。恢复媒体时使用对应日期的日备份，旧版完整发布归档沿用原格式。

新批次共用 rolling 工具的流式归档和回滚清理逻辑。恢复旧 App 和 Worker 后，发布器先验证当前恢复点，再退役本次失败备份中的归档和数据库导出，并保留发布记录。`archive_pending` 和 `needs_attention` 批次保留全部现场供后续处理。

`plan` 通过且前置证据保持一致后，只执行一次：

```sh
python3 "$cfm_op/coordinator.py" apply-and-verify \
  --root "$cfm_root" --operation-dir "$cfm_op"
```

常规发布可直接使用连续入口，跳过独立 `plan` 的重复证明。它会在同一操作目录中依次完成构建、Worker
准备、预检、交接和最终验收，所有阶段共享 `init` 时刻的 1200 秒预算；发生已确认的
`archive_pending` 或最终只读验收中断时，只在剩余预算内自动续做归档。`run` 可以直接接收计划并原子创建操作目录，
也可以用 `--operation-dir` 续跑既有目录。已有目录不会被覆盖，不能同时传入两种模式的参数。需要人工审阅计划时仍可单独运行 `plan`。

```sh
python3 "$cfm_root/tools/paired-deploy/current/paired-deploy.py" run \
  --plan reviewed.json --output "$cfm_op" --root "$cfm_root" --source "$cfm_source"

# 已创建的目录只允许续跑，不会重新初始化
python3 "$cfm_root/tools/paired-deploy/current/paired-deploy.py" run \
  --operation-dir "$cfm_op" --root "$cfm_root" --source "$cfm_source"
```

coordinator 按全局备份锁 → 项目备份锁 → 应用维护锁的顺序协调操作，并采集其他服务基线。
流程先在线预复制，再暂停后台调度、自然排空、hold 入口、冻结唯一写入者，再完成最终快照、数据库
备份及原始快照的严格校验/fsync。原始备份封存后才切换 Worker、激活 app、校验冻结状态，
恢复流量和调度，最后压缩归档并独立复读验证。

2026-11-16 精简了重复检查，关键边界如下：

- 在线预复制遇到浏览器临时链接/特殊文件时，记录 `online_precopy_deferred`。旧实例自然排空后、
  网关 hold 之前会重试预复制，把完整文件复制放在仍可服务的阶段；重试失败立即停止，冻结复制
  仍严格处理，磁盘、权限和未知错误不延后。预复制不能作为已完成备份的证明。
- 冻结复制先用逐文件身份检查完成副本写入，布局校验复用这次源文件读取，只对冻结副本计算一次
  内容摘要；state guard 复用同进程绑定的摘要。Worker 切换已由自身的封存探针、容器身份和配置
  验收覆盖，候选激活后仅保留一次切流前的业务文件身份与数据库精确比较，不再为 Worker 后边界
  重复遍历数十万条业务记录。副本的完整读取、文件集合、目录权限和内容检查均保留。
- 维护窗口开始时创建受保护的 `data/cfm-maintenance` 标记，阻止新的巡检/调度轮次进入；
  已在运行的业务任务仍按自然排空和 `SealDeployment` 的活动计数收尾。预检只记录 xAI 队列快照，
  冻结后才执行一次空队列门禁，避免同一业务轮询状态形成两个发布门槛。该标记属于运行时文件，
  完整冻结归档会保留并校验它，业务状态 guard 会排除它，因此切流前清除标记不会形成伪业务变更；
  外部标记或内容被替换时仍停止并保留现场。
- 冻结和候选激活后的两个状态边界各只扫描一次业务表，表数据和 schema dump 共用存活的
  PostgreSQL 导出快照。每表保存行数与类型稳定的 SHA-256 摘要，拒绝重复行身份；正式数据库
  备份导出仍单独执行。guard 使用 `cfm-state-unchanged-v2`，不混用旧版基线。
- Worker switch 已执行完整验收。应用激活后使用 `verify-ready`，只复用绑定该运行容器、
  镜像、启动、配置、发布计划的成功证据，单次复查健康、空闲容量和无浏览器；身份、退出、
  OOM 或服务合同确定不符立即失败，不再为不可收敛的容量/健康不符轮询等待。`worker-error*.json`
  只记录结构化错误类别。
- Worker 切换后的依赖容器/网关身份检查不再单独重复执行；冻结状态 guard 和 Worker 完整验收完成后，
  只在候选激活、切流前保留一次最终依赖检查。数据库、Redis、旧 App 身份与维护路由仍必须匹配基线，
  任何最终边界异常仍会阻止切流。
- 最终验收在一次完整归档读回中同时计算压缩包 SHA。`raw-identity.json` 与 manifest 和
  checksums 绑定，只有同一内核启动、路径及 dev/inode/mode/uid/gid/size/mtime/ctime 全部
  一致才免原始副本内容重读；旧备份、重启或身份变化回退完整扫描，内容不符仍失败。
  独立最终验收始终完整读取归档，不只信任 `complete` 标记。
- `release-state.json` 的 `timings` 记录预复制、排空、冻结复制、数据库导出、冻结状态和一次
  切流前状态边界、Worker 切换/就绪和归档收尾耗时；`drain_waits` 只记录排空期间变化的非零计数和等待时长，
  `state-*.json` 包含内容读取量和数据库耗时，不记录原始行。

这些改动只进入新生成的工具包；旧的冻结发布包及其基线不改写。实现及本地验证见
运维仓库的 `operations/cfm-release-optimization-20261116/README.md`。

自然排空默认预算为 600 秒，不能强制终止业务任务来缩短；镜像构建、磁盘 IO、
数据库备份、Worker 就绪及公网验收仍有各自耗时。普通 `deploy.sh deploy` 以 1200 秒作为从
Git 准备开始到最终验收的全链路安全预算，旧布局的 App/Worker 构建并行进行，相同提交的已核验
ARM64 镜像自动复用。达到预算会安全停止并保留现场，不能把预算表述为强杀或保证任何情况下成功。
完整归档放到恢复流量后，只缩短其对维护窗口的占用。

最终验收包括当前镜像身份、健康、业务只读接口、原始备份/归档、前端资源和其他服务。
只有所有验收通过，才向本轮备份写入 `retention-ready.json`；该标记仅表示本恢复点具备保留策略
准入证据，不等同于执行清理或修复独立日备份任务。

静态资源从候选容器一次读取明确的 `index.html` 和平铺 JS/CSS 文件，宿主解析 tar 而不落盘解包。
拒绝链接、特殊文件、重复或越界路径；归档上限 64 MiB、单文件上限 16 MiB、最多 4096 成员。
仅沿当前 index 的 JS/CSS 依赖图访问本机/公网，最多同时 4 个 HTTP GET、逐字节比较、拒绝重定向；
每个请求超时 30 秒，不重试。gateway Docker health 最多等待 45 秒收敛，单次 inspect 最多 5 秒，
每轮身份、配置、重启、运行和 OOM 状态仍须一致；数据库不享有健康放宽。

## 中断后的查询与续验

先读取原批次状态；不因断线、超时或命令非零退出而重放 `apply-and-verify`。

```sh
python3 "$cfm_op/release.py" --root "$cfm_root" --operation-dir "$cfm_op" status
```

| 已确认状态 | 后续动作 |
| --- | --- |
| `archive_pending`，且流量、唯一写入者及原实例退休均有确认 | 原始快照保留，业务可能已经正常恢复；运行 `finalize` 校验身份并续做归档与最终验收。 |
| `success`，但最终验收中断、未写准入标记 | 运行 `finalize` 续验；旧失败记录保留，新证据使用新文件名。 |
| 已有 `retention-ready.json` | `finalize` 重新核对原有准入身份与证据，不重做交接。 |
| `needs_attention`、切流结果未知或其他未完成状态 | 先只读核对真实实例、租约、路由与日志；不能把它当作允许续归档或重放 apply 的证据。 |

```sh
python3 "$cfm_op/coordinator.py" finalize \
  --root "$cfm_root" --operation-dir "$cfm_op"
```

`finalize` 不重新激活、切路由或回滚数据。归档失败保留原始快照和已恢复服务，继续标为
`archive_pending`，不准入保留清理。不要删除/改写 `release-state.json`、冻结摘要或失败文件来绕过状态检查。
