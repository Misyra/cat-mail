#!/usr/bin/env bash
# CatMail E2E 一次性后端（供 Playwright webServer 调用，也可单独手动启动）。
#
# 用法：scripts/e2e-server.sh   （前台驻留；由 Playwright 以 SIGTERM 收尾并自动清理）
#
# 行为：
#   - 先构建前端（npx vite build → backend/internal/web/dist，go:embed 输入），
#     保证测试跑到的是当前工作区代码而非陈旧产物；不跑 tsc 类型检查，
#     避免并行改动中的类型层抖动阻塞 E2E（语法错误仍会失败）。
#   - go build -o 临时目录（失败疑似并发编辑，等待重试）。
#   - 启动一次性实例：CATMAIL_DB=临时库 CATMAIL_ADDR=:18100，
#     端口 18100 避开 smoke(18080)/走查(18081)。
#   - 轮询 /api/health 就绪；随后 wait 保持前台（Playwright 同时也会自行轮询 url）。
#   - 收到 TERM/INT/EXIT：杀服务进程并删除临时目录，不污染仓库。
#   - 全程 mock 渠道，不出网（出网行为由测试自身保证不触发）。
#
# 环境变量（均可覆盖）：
#   E2E_PORT（默认 18100）、E2E_ADMIN_USER/PASS（默认 admin/admin123）、
#   E2E_BUILD_RETRIES（默认 3）、E2E_BUILD_RETRY_WAIT（默认 30 秒）。

set -euo pipefail

PORT="${E2E_PORT:-18100}"
BASE="http://127.0.0.1:${PORT}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND_DIR="${REPO_DIR}/frontend"
BACKEND_DIR="${REPO_DIR}/backend"

BUILD_RETRIES="${E2E_BUILD_RETRIES:-3}"
BUILD_RETRY_WAIT="${E2E_BUILD_RETRY_WAIT:-30}"
ADMIN_USER="${E2E_ADMIN_USER:-admin}"
ADMIN_PASS="${E2E_ADMIN_PASS:-admin123}"

TMP=""
SERVER_PID=""
STOPPING=0

cleanup() {
  STOPPING=1
  if [ -n "${SERVER_PID}" ] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null || true
    # 服务短暂退出窗口内等待，避免 zombie/端口残留
    for _ in 1 2 3 4 5; do
      kill -0 "${SERVER_PID}" 2>/dev/null || break
      sleep 0.2
    done
  fi
  # 兜底按端口清理（防止 go 子进程脱离脚本进程树）
  lsof -ti ":${PORT}" 2>/dev/null | xargs kill 2>/dev/null || true
  if [ -n "${TMP}" ]; then
    rm -rf "${TMP}"
  fi
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

die() { echo "[e2e-server] FATAL: $*" >&2; exit 2; }
log() { echo "[e2e-server] $*"; }

command -v go >/dev/null 2>&1 || die "缺少 go"
[ -d "${BACKEND_DIR}/cmd/catmail" ] || die "未找到后端目录 ${BACKEND_DIR}"
[ -f "${FRONTEND_DIR}/package.json" ] || die "未找到前端目录 ${FRONTEND_DIR}"

if lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN 2>/dev/null | grep -q .; then
  die "端口 ${PORT} 已被占用（可能是残留的 e2e 实例）：lsof -ti :${PORT} | xargs kill"
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/catmail-e2e.XXXXXX")"
log "临时目录: ${TMP}  端口: ${PORT}"

# ---------- 构建前端（go:embed 输入；失败疑似并发编辑，等待重试） ----------

frontend_ok=0
i=1
while [ "${i}" -le "${BUILD_RETRIES}" ]; do
  if (cd "${FRONTEND_DIR}" && npx vite build --logLevel warn); then
    frontend_ok=1
    break
  fi
  echo "[e2e-server] vite build 失败（第 ${i}/${BUILD_RETRIES} 次）" >&2
  if [ "${i}" -lt "${BUILD_RETRIES}" ]; then
    echo "[e2e-server] 疑似并发编辑，等待 ${BUILD_RETRY_WAIT}s 后重试..." >&2
    sleep "${BUILD_RETRY_WAIT}"
  fi
  i=$((i + 1))
done
[ "${frontend_ok}" -eq 1 ] || die "前端构建连续 ${BUILD_RETRIES} 次失败"
[ -f "${BACKEND_DIR}/internal/web/dist/index.html" ] || die "前端产物缺失（dist/index.html 不存在）"
log "前端构建完成"

# ---------- 构建后端二进制到临时目录 ----------

BIN="${TMP}/catmail"
backend_ok=0
i=1
while [ "${i}" -le "${BUILD_RETRIES}" ]; do
  if (cd "${BACKEND_DIR}" && go build -o "${BIN}" ./cmd/catmail); then
    backend_ok=1
    break
  fi
  echo "[e2e-server] go build 失败（第 ${i}/${BUILD_RETRIES} 次）" >&2
  if [ "${i}" -lt "${BUILD_RETRIES}" ]; then
    echo "[e2e-server] 疑似并发编辑，等待 ${BUILD_RETRY_WAIT}s 后重试..." >&2
    sleep "${BUILD_RETRY_WAIT}"
  fi
  i=$((i + 1))
done
[ "${backend_ok}" -eq 1 ] || die "后端构建连续 ${BUILD_RETRIES} 次失败"
log "后端构建完成: ${BIN}"

# ---------- 启动一次性实例并轮询就绪 ----------

CATMAIL_ADDR=":${PORT}" \
  CATMAIL_DB="${TMP}/catmail.db" \
  CATMAIL_ADMIN_USER="${ADMIN_USER}" \
  CATMAIL_ADMIN_PASS="${ADMIN_PASS}" \
  "${BIN}" >"${TMP}/catmail.log" 2>&1 &
SERVER_PID=$!

ready=0
n=1
while [ "${n}" -le 120 ]; do
  code=$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${BASE}/api/health" 2>/dev/null || true)
  if [ "${code}" = "200" ]; then
    ready=1
    break
  fi
  kill -0 "${SERVER_PID}" 2>/dev/null || break
  sleep 0.5
  n=$((n + 1))
done
if [ "${ready}" -ne 1 ]; then
  echo "---- 启动日志 ----" >&2
  tail -n 40 "${TMP}/catmail.log" >&2 || true
  die "服务未在 ${BASE} 就绪（进程可能已退出）"
fi
log "服务就绪: ${BASE}（pid ${SERVER_PID}，数据库 ${TMP}/catmail.db）"

# 前台驻留：Playwright 测试结束后向本进程发 SIGTERM → trap 清理。
# 若 go 服务意外退出（含被外部 kill），wait 返回非零导致脚本退出，Playwright 会判定 webServer 失败。
wait "${SERVER_PID}" 2>/dev/null || {
  [ "${STOPPING}" -eq 1 ] || {
    echo "---- 服务日志（意外退出，最后 40 行）----" >&2
    tail -n 40 "${TMP}/catmail.log" >&2 || true
    die "后端进程意外退出"
  }
}
