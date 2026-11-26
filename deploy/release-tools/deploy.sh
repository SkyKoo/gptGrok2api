#!/usr/bin/env bash
# 以普通用户拉取代码，使用 sudo 构建并发布 CFM；滚动布局只切换 app。
set -euo pipefail

usage() {
  cat <<'HELP'
用法：deploy.sh [deploy|start|apply|rollback|rolling|status|--help]
  deploy    前台执行：快进拉取 origin/main，构建 ARM64 镜像，备份并发布（默认全链路预算 1200 秒）
  start     后台启动一次 deploy，SSH 断开后仍可用 status 查询
  apply     使用当前镜像重新应用工具模板，执行同样的备份和验收
  rollback  恢复最近一次发布前的程序版本，保留当前业务数据
  rolling   prepare/bootstrap/deploy/status；应用双实例交接，沿用共享依赖
  status    查看发布阶段、阶段耗时、进程状态、版本和容器健康状态

在部署主机上以普通用户运行，不要 sudo 运行整个脚本。
发布失败会优先恢复旧镜像和配置；若新版本已经产生业务写入，则保护性延后自动回滚。
可用 `GPTGROK2API_TOTAL_TIMEOUT_SECONDS` 调整预算，但不能超过 1200 秒。
默认前台发布会把完整输出交给当前终端；长发布建议使用 start，再用 status 查询。
HELP
}

action="${1:-deploy}"
case "$action" in
  -h|--help) usage; exit 0 ;;
  deploy|start|apply|rollback|rolling|status) ;;
  *) usage >&2; exit 2 ;;
esac
[[ "$action" == rolling || $# -le 1 ]] || { usage >&2; exit 2; }
[[ $(uname -s) == Linux && $EUID -ne 0 ]] || {
  echo '请在部署主机上以普通用户运行，不要 sudo 运行整个脚本。' >&2
  exit 1
}

tools_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="${GPTGROK2API_SOURCE_DIR:-$HOME/projects/gptGrok2api-publish}"
runtime_root="${GPTGROK2API_RUNTIME_DIR:-}"
if [[ -z "$runtime_root" && ${tools_dir##*/} == tools ]]; then
  runtime_root="${tools_dir%/tools}"
fi
[[ "$runtime_root" == /* && "$runtime_root" != / ]] || {
  echo '请设置 GPTGROK2API_RUNTIME_DIR 为服务运行目录的绝对路径。' >&2
  exit 2
}

# One wall-clock budget covers preparation, build, handoff and verification.
# A timeout stops before traffic is changed or leaves the rolling tool to
# perform its own inspectable safe failure. It never authorizes a forced stop
# of an active application container.
pipeline_started="$(date +%s)"
pipeline_budget="${GPTGROK2API_TOTAL_TIMEOUT_SECONDS:-1200}"
[[ "$pipeline_budget" =~ ^[0-9]+$ && "$pipeline_budget" -ge 60 && "$pipeline_budget" -le 1200 ]] || {
  echo 'GPTGROK2API_TOTAL_TIMEOUT_SECONDS 必须是 60 到 1200 秒之间的整数。' >&2
  exit 1
}
command -v timeout >/dev/null || { echo '部署主机缺少 timeout 命令，请安装 coreutils。' >&2; exit 1; }
pipeline_elapsed() { echo $(( $(date +%s) - pipeline_started )); }
pipeline_remaining() { echo $(( pipeline_budget - $(pipeline_elapsed) )); }
check_pipeline_budget() {
  local remaining
  remaining="$(pipeline_remaining)"
  (( remaining > 0 )) || {
    echo "发布已达到全链路预算 ${pipeline_budget}s，保留现场并停止。" >&2
    exit 1
  }
}
run_budgeted() {
  local requested="$1"
  shift
  check_pipeline_budget
  local remaining budget
  remaining="$(pipeline_remaining)"
  budget="$requested"
  (( budget > remaining )) && budget="$remaining"
  timeout --signal=TERM --kill-after=30 "$budget" "$@"
  check_pipeline_budget
}

if [[ "$action" == rolling ]]; then
  shift
  rolling_action="${1:-}"
  if [[ "$rolling_action" == prepare ]]; then
    # Direct prepare is read-only, so the shell budget can safely bound it.
    run_budgeted "$(pipeline_remaining)" sudo -n python3 "$tools_dir/rolling-deploy.py" \
      --root "$runtime_root" "$@"
    exit $?
  fi
  if [[ "$rolling_action" == deploy || "$rolling_action" == bootstrap ]]; then
    # A caller-supplied rolling timeout cannot restart or extend the shell's
    # end-to-end budget. Strip it and pass the actual remaining seconds.
    rolling_args=()
    skip_total_timeout_value=0
    for rolling_arg in "$@"; do
      if (( skip_total_timeout_value )); then
        skip_total_timeout_value=0
        continue
      fi
      if [[ "$rolling_arg" == --total-timeout ]]; then
        skip_total_timeout_value=1
        continue
      fi
      [[ "$rolling_arg" == --total-timeout=* ]] && continue
      rolling_args+=("$rolling_arg")
    done
    check_pipeline_budget
    set -- "${rolling_args[@]}" --total-timeout "$(pipeline_remaining)"
  fi
  exec sudo -n python3 "$tools_dir/rolling-deploy.py" --root "$runtime_root" "$@"
fi
layout="$(run_budgeted "$(pipeline_remaining)" sudo -n python3 "$tools_dir/rolling-deploy.py" --root "$runtime_root" layout)"
if [[ "$action" == status && "$layout" != legacy ]]; then
  exec sudo -n python3 "$tools_dir/rolling-deploy.py" --root "$runtime_root" status
fi
if [[ "$action" != status && "$layout" == switching ]]; then
  echo '上次交接状态未确认，请先检查 rolling status，保留现有实例。' >&2; exit 1
fi
if [[ "$action" != status && "$layout" == legacy ]]; then
  run_budgeted "$(pipeline_remaining)" sudo -n python3 "$tools_dir/rolling-deploy.py" --root "$runtime_root" guard-legacy
fi
if [[ "$layout" == rolling && ( "$action" == apply || "$action" == rollback ) ]]; then
  echo '滚动布局禁止旧 apply/rollback；回退应以旧兼容镜像执行 rolling deploy，保留当前数据。' >&2; exit 1
fi

# status 必须能在发布锁被占用时执行，因此不进入仓库，也不获取 Git 锁。
if [[ "$action" == status ]]; then
  exec sudo -n python3 "$tools_dir/deploy.py" --root "$runtime_root" status
fi

cd "$repo"
lock_path="$(git rev-parse --git-path gptgrok2api-deploy.lock)"
exec 9>"$lock_path"
flock -n 9 || { echo '已有 GPTGrok2API 操作正在执行。' >&2; exit 1; }
sudo -n true

# 后台模式只负责拉起一个独立进程；真实发布仍由同一个 deploy 分支执行，
# 发布脚本自身的 maintenance.lock 会继续防止重复切换。
if [[ "$action" == start ]]; then
  # 后台发布自行获取锁；启动器不能把锁继承给子进程。
  flock -u 9
  exec 9>&-
  log_dir="${GPTGROK2API_CLIENT_LOG_DIR:-$HOME/.cache/gptgrok2api}"
  mkdir -p "$log_dir"
  log_file="$log_dir/deploy-$(date -u +%Y%m%dT%H%M%SZ).log"
  nohup env GPTGROK2API_DETACHED=1 GPTGROK2API_SOURCE_DIR="$repo" \
    GPTGROK2API_RUNTIME_DIR="$runtime_root" "$tools_dir/deploy.sh" deploy \
    >"$log_file" 2>&1 < /dev/null &
  pid=$!
  printf '%s\n' "发布已后台启动：pid=$pid" "日志：$log_file" \
    "查询：$tools_dir/deploy.sh status"
  exit 0
fi

if [[ "$action" == deploy ]]; then
  [[ $(uname -m) == aarch64 ]] || { echo '本工具仅用于 ARM64 O 机器。' >&2; exit 1; }
  origin="$(git remote get-url origin)"
  case "$origin" in
    git@github.com:SkyKoo/gptGrok2api.git|https://github.com/SkyKoo/gptGrok2api.git) ;;
    *) echo "origin 与预期仓库不一致：$origin" >&2; exit 1 ;;
  esac
  [[ $(git branch --show-current) == main && -z $(git status --porcelain) ]] || {
    echo '需要干净的 main 工作树，工具不会覆盖本地修改。' >&2
    exit 1
  }
  git_timeout="${GPTGROK2API_GIT_TIMEOUT_SECONDS:-180}"
  build_timeout="${GPTGROK2API_BUILD_TIMEOUT_SECONDS:-900}"
  [[ "$git_timeout" =~ ^[0-9]+$ && "$git_timeout" -ge 1 && "$git_timeout" -le 1200 ]] || {
    echo 'GPTGROK2API_GIT_TIMEOUT_SECONDS 必须是 1 到 1200 秒之间的整数。' >&2; exit 1;
  }
  [[ "$build_timeout" =~ ^[0-9]+$ && "$build_timeout" -ge 1 && "$build_timeout" -le 1200 ]] || {
    echo 'GPTGROK2API_BUILD_TIMEOUT_SECONDS 必须是 1 到 1200 秒之间的整数。' >&2; exit 1;
  }
  run_budgeted "$git_timeout" env GIT_TERMINAL_PROMPT=0 git pull --ff-only origin main
  commit="$(git rev-parse HEAD)"
  [[ "$commit" == "$(git rev-parse origin/main)" ]] || {
    echo '本地提交不是 origin/main，请先处理。' >&2
    exit 1
  }
  image="gptgrok2api:sha-${commit:0:12}"
  worker_image="gptgrok2api-grok-browser:sha-${commit:0:12}"
  build_dir="$(mktemp -d /tmp/gptgrok2api-build.XXXXXX)"
  trap 'rm -rf -- "$build_dir"' EXIT
  # Keep source preparation inside the same wall-clock budget as pull/build.
  # pipefail makes a truncated archive fail instead of looking successful.
  run_budgeted "$git_timeout" bash -o pipefail -c \
    'git archive "$1" | tar -x -C "$2"' _ "$commit" "$build_dir"
  check_pipeline_budget

  image_identity() {
    sudo -n docker image inspect "$1" --format '{{.Id}}|{{.Os}}|{{.Architecture}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "org.opencontainers.image.source"}}|{{index .Config.Labels "io.cfm.rolling.protocol"}}' 2>/dev/null
  }
  image_present() {
    sudo -n docker image inspect "$1" >/dev/null 2>&1
  }
  verify_image_identity() {
    local tag="$1" require_protocol="$2" info id os arch revision source protocol
    info="$(image_identity "$tag")" || {
      echo "无法读取已有镜像身份：$tag" >&2
      return 1
    }
    IFS='|' read -r id os arch revision source protocol <<<"$info"
    [[ "$id" == sha256:* && "$os" == linux && "$arch" == arm64 \
      && "$revision" == "$commit" && "$source" == "$origin" ]] || {
      echo "已有镜像标签身份不匹配，拒绝覆盖：$tag" >&2
      return 1
    }
    if [[ "$require_protocol" == 1 && "$protocol" != cfm-single-writer-v1 ]]; then
      echo "已有镜像缺少已审查的滚动协议标签，拒绝复用：$tag" >&2
      return 1
    fi
    printf '%s\n' "$id"
  }
  if [[ "$layout" != rolling ]]; then
    # The legacy entry has two independent images. Start both builds together
    # under the same wall-clock budget, reusing only exact ARM64/revision tags.
    build_failed=0
    build_pids=()
    if image_present "$image"; then
      app_id="$(verify_image_identity "$image" 0)"
      echo "复用已核验的不可变 App 镜像 $image ($app_id)"
    else
      echo "并行构建提交 $commit 的 App 镜像 $image"
      run_budgeted "$build_timeout" sudo -n docker build --platform linux/arm64 --target app \
        --label "org.opencontainers.image.revision=$commit" \
        --label "org.opencontainers.image.source=$origin" \
        -t "$image" "$build_dir" &
      build_pids+=("$!")
    fi
    if image_present "$worker_image"; then
      worker_id="$(verify_image_identity "$worker_image" 0)"
      echo "复用已核验的不可变 Worker 镜像 $worker_image ($worker_id)"
    else
      echo "并行构建提交 $commit 的 Worker 镜像 $worker_image"
      run_budgeted "$build_timeout" sudo -n docker build --platform linux/arm64 \
        --label "org.opencontainers.image.revision=$commit" \
        --label "org.opencontainers.image.source=$origin" \
        -f "$build_dir/grok-browser/Dockerfile" -t "$worker_image" "$build_dir" &
      build_pids+=("$!")
    fi
    for pid in "${build_pids[@]}"; do
      if ! wait "$pid"; then build_failed=1; fi
    done
    (( build_failed == 0 )) || exit 1
    app_id="$(verify_image_identity "$image" 0)"
    worker_id="$(verify_image_identity "$worker_image" 0)"
    echo "旧布局镜像身份已固定：app=$app_id worker=$worker_id"
    check_pipeline_budget
    remaining="$(pipeline_remaining)"
    run_budgeted "$remaining" sudo -n python3 "$tools_dir/deploy.py" --root "$runtime_root" deploy \
      --image "$image" --worker-image "$worker_image" --commit "$commit" --template "$tools_dir/compose.yaml" --source "$repo"
    check_pipeline_budget
    echo "发布全链路耗时 $(pipeline_elapsed)s（预算 ${pipeline_budget}s）"
    exit 0
  fi

  if image_present "$image"; then
    app_id="$(verify_image_identity "$image" 1)"
    echo "复用已核验的不可变 App 镜像 $image ($app_id)"
  else
    echo "构建提交 $commit → $image（旧服务继续运行）"
    run_budgeted "$build_timeout" sudo -n docker build --platform linux/arm64 --target app \
      --label "org.opencontainers.image.revision=$commit" \
      --label "org.opencontainers.image.source=$origin" \
      -t "$image" "$build_dir"
    app_id="$(verify_image_identity "$image" 1)"
    echo "App 镜像身份已固定：$app_id"
  fi
  check_pipeline_budget
  remaining="$(pipeline_remaining)"
  run_budgeted "$remaining" sudo -n python3 "$tools_dir/rolling-deploy.py" --root "$runtime_root" prepare \
    --source "$repo" --image "$image" --output "$build_dir/rolling-release.json"
  check_pipeline_budget
  remaining="$(pipeline_remaining)"
  run_budgeted "$remaining" sudo -n python3 "$tools_dir/rolling-deploy.py" --root "$runtime_root" deploy \
    --release "$build_dir/rolling-release.json" --total-timeout "$remaining"
  check_pipeline_budget
  echo "发布全链路耗时 $(pipeline_elapsed)s（预算 ${pipeline_budget}s）"
  exit 0
else
  run_budgeted "$(pipeline_remaining)" sudo -n python3 "$tools_dir/deploy.py" --root "$runtime_root" "$action"
fi
