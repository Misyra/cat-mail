#!/usr/bin/env bash
# CatMail 集成冒烟测试（可重复执行，幂等）。
#
# 用法：scripts/smoke.sh
# 依赖：go、curl、jq、lsof、python3。
#
# 行为：
#   - 全部临时产物（数据目录、二进制、日志、cookie）放在 mktemp -d 目录，
#     结束时清理，不污染仓库；端口固定 18080（启动前确认空闲）。
#     辅助端口：webhook 接收器 18110（S13）、管理白名单第二实例 18120（S17）、
#     SMTP TLS 实例 18121 + SMTP 明文 18525 + 隐式 TLS 18465（S21）、
#     安全加固实例 18122 + 回执接收器 18130（S22），
#     均启动前确认空闲、用完即杀，trap 兜底。
#   - 场景：health / 登录 / 未鉴权 401 / 建 mock 渠道 / ApiKey / /v1/send /
#     渠道测试发送扣额度 / 日额度 override=1 后测试 429 / /emails 字符串 to /
#     发送日志与 CSV 导出 / 设置读写回一致 / /api 未知路径 JSON 404 /
#     并发额度压测（S12）/ 失败通知链路（S13）/ TOTP 两步验证（S14）/
#     备份与日志清理（S15）/ 渠道复制与日志重发（S16）/
#     metrics 与管理 IP 白名单（S17）/ 审计日志（S18）/
#     邮件模板（S19）/ 送达事件与抑制（S20）/ SMTP 入口 TLS（S21）/
#     安全加固三件套（S22）。
#   - 每步输出 PASS/FAIL，FAIL 不中断；任一 FAIL 最终退出码非零。
#     新增场景依赖的端点若尚未合并（404），按 SKIP 处理并标注归属波次，
#     不计失败（终验复跑时转 PASS）。
#   - 全程 mock 渠道，不出网（webhook 接收器也只监听 127.0.0.1）。
# 环境变量（均可覆盖）：SMOKE_BUILD_RETRIES（默认 3）、SMOKE_BUILD_RETRY_WAIT
# （默认 60 秒，go build 失败疑似并发编辑时等待后重试）、SMOKE_CURL_MAX_TIME（默认 15）。

set -euo pipefail

PORT=18080
BASE="http://127.0.0.1:${PORT}"
PORT_HOOK=18110      # S13 本地 webhook 接收器
PORT2=18120          # S17 管理 IP 白名单第二实例
BASE2="http://127.0.0.1:${PORT2}"
PORT_TLS_HTTP=18121  # S21 SMTP TLS 实例的 HTTP 监听
BASE_TLS="http://127.0.0.1:${PORT_TLS_HTTP}"
PORT_TLS_SMTP=18525  # S21 SMTP 明文监听（STARTTLS 广播 / require 策略）
PORT_TLS_IMPLICIT=18465  # S21 SMTP 隐式 TLS 监听（465 风格）
PORT_SEC_HTTP=18122  # S22 安全加固实例（CATMAIL_SECRET_KEY）
BASE_SEC="http://127.0.0.1:${PORT_SEC_HTTP}"
PORT_HOOK2=18130     # S22 回执签名接收器
HOOK_URL="http://127.0.0.1:${PORT_HOOK}/hook"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_DIR="${REPO_DIR}/backend"

BUILD_RETRIES="${SMOKE_BUILD_RETRIES:-3}"
BUILD_RETRY_WAIT="${SMOKE_BUILD_RETRY_WAIT:-60}"
CURL_MAX_TIME="${SMOKE_CURL_MAX_TIME:-15}"

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
TMP=""
SERVER_PID=""
EXTRA_PIDS=""  # 辅助进程：webhook 接收器 / 白名单第二实例 / trusted-proxies 反例

cleanup() {
  for pid in "${SERVER_PID}" ${EXTRA_PIDS}; do
    [ -n "${pid}" ] && kill "${pid}" 2>/dev/null || true
  done
  # 兜底按端口清理（pkill 匹配不到 "./catmail" 相对路径进程，故用 lsof）
  for p in "${PORT}" "${PORT_HOOK}" "${PORT2}" "${PORT_TLS_HTTP}" \
    "${PORT_TLS_SMTP}" "${PORT_TLS_IMPLICIT}" "${PORT_SEC_HTTP}" "${PORT_HOOK2}"; do
    lsof -ti ":${p}" 2>/dev/null | xargs kill 2>/dev/null || true
  done
  if [ "${FAIL_COUNT}" -gt 0 ] && [ -n "${TMP}" ] && [ -f "${TMP}/catmail.log" ]; then
    echo "---- catmail 服务日志（有失败项，附最后 60 行）----" >&2
    tail -n 60 "${TMP}/catmail.log" >&2 || true
  fi
  [ -n "${TMP}" ] && rm -rf "${TMP}"
  return 0
}
trap cleanup EXIT

die() { echo "FATAL: $*" >&2; exit 2; }

pass() { printf '  PASS  %s\n' "$1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { printf '  FAIL  %s\n' "$1"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
skip() { printf '  SKIP  %s\n' "$1"; SKIP_COUNT=$((SKIP_COUNT + 1)); }
section() { printf '\n== %s ==\n' "$1"; }

expect_eq() { # 名称 期望 实际
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1（期望 [$2]，实际 [$3]）"
  fi
}

expect_contains() { # 名称 子串 文本
  if printf '%s' "$3" | grep -qF -- "$2"; then
    pass "$1"
  else
    fail "$1（未在响应中找到 [$2]，实际 [$(printf '%s' "$3" | head -c 300)]）"
  fi
}

expect_not_contains() { # 名称 子串 文本
  if printf '%s' "$3" | grep -qiF -- "$2"; then
    fail "$1（不应出现 [$2]）"
  else
    pass "$1"
  fi
}

# int_or_0 把任意值清洗成非负整数（空串/null/非法值 → 0），避免算术求值崩溃。
int_or_0() {
  case "${1:-}" in
    '' | *[!0-9]*) echo 0 ;;
    *) echo "$1" ;;
  esac
}

# do_curl [curl 参数...]：执行请求并填充 STATUS / BODY / CONTENT_TYPE。
do_curl() {
  local hdr="${TMP}/headers.txt"
  STATUS=$(curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMP}/body.txt" -D "${hdr}" \
    -w '%{http_code}' "$@" 2>"${TMP}/curl_err.txt") || STATUS="000"
  BODY=$(cat "${TMP}/body.txt" 2>/dev/null || true)
  CONTENT_TYPE=$(grep -i '^content-type:' "${hdr}" 2>/dev/null | head -n1 | tr -d '\r' |
    sed 's/^[Cc]ontent-[Tt]ype: *//')
}

# jget <jq 参数...>：对 BODY 求值（支持 --arg 透传），失败/缺失时输出空串。
jget() {
  printf '%s' "${BODY:-}" | jq -r "$@" 2>/dev/null || true
}

# channel_field <字段名>：GET /api/channels 后取本冒烟渠道的字段值。
channel_field() {
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/channels"
  jget --arg n "${CH_NAME}" '(.items[] | select(.name == $n)) | .'"$1"
}

# burst_channel_field <字段名>：同上，取 S12 并发压测渠道（smoke-burst）的字段值。
burst_channel_field() {
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/channels"
  jget --arg n "smoke-burst" '(.items[] | select(.name == $n)) | .'"$1"
}

# totp_code <base32 secret>：按 RFC 6238 生成 6 位 TOTP 码（python3 参考实现）。
# 算法来源：RFC 6238 TOTP = RFC 4226 HOTP 截断算法，计数器取 Unix 时间除以
# 30 秒窗口；HMAC-SHA1 摘要末字节低 4 位为偏移，取 4 字节去符号位对 10^6 取模，
# 左补零到 6 位。与 pquerna/otp 默认参数（SHA1/6 位/30s，服务端 ±1 窗口校验）
# 对齐，用于 S14 两步验证的验证码生成。
totp_code() {
  python3 - "$1" <<'PY'
import base64, hashlib, hmac, struct, sys, time
secret = sys.argv[1]
key = base64.b32decode(secret + "=" * ((8 - len(secret) % 8) % 8), casefold=True)
counter = int(time.time()) // 30
h = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
o = h[19] & 0x0F
print("%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000))
PY
}

# ---------- 前置检查 ----------

command -v jq >/dev/null 2>&1 || die "缺少 jq"
command -v go >/dev/null 2>&1 || die "缺少 go"
command -v python3 >/dev/null 2>&1 || die "缺少 python3（S13 webhook 接收器 / S14 TOTP 码生成依赖）"
[ -d "${BACKEND_DIR}/cmd/catmail" ] || die "未找到后端目录 ${BACKEND_DIR}"

if lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN 2>/dev/null | grep -q .; then
  die "端口 ${PORT} 已被占用，请先释放（lsof -ti :${PORT} | xargs kill）"
fi
for aux_port in "${PORT_HOOK}" "${PORT2}" "${PORT_TLS_HTTP}" "${PORT_TLS_SMTP}" \
  "${PORT_TLS_IMPLICIT}" "${PORT_SEC_HTTP}" "${PORT_HOOK2}"; do
  if lsof -nP -iTCP:"${aux_port}" -sTCP:LISTEN 2>/dev/null | grep -q .; then
    die "辅助端口 ${aux_port} 已被占用，请先释放（lsof -ti :${aux_port} | xargs kill）"
  fi
done

TMP="$(mktemp -d "${TMPDIR:-/tmp}/catmail-smoke.XXXXXX")"
echo "CatMail 集成冒烟测试"
echo "  临时目录: ${TMP}"
echo "  目标端口: ${PORT}"

# ---------- 构建（失败疑似并发编辑，等待后重试） ----------

section "S0 构建后端 go build ./cmd/catmail"
build_ok=0
i=1
while [ "${i}" -le "${BUILD_RETRIES}" ]; do
  if (cd "${BACKEND_DIR}" && go build -o "${TMP}/catmail" ./cmd/catmail); then
    build_ok=1
    break
  fi
  echo "  go build 失败（第 ${i}/${BUILD_RETRIES} 次）" >&2
  if [ "${i}" -lt "${BUILD_RETRIES}" ]; then
    echo "  疑似并发编辑，等待 ${BUILD_RETRY_WAIT}s 后重试..." >&2
    sleep "${BUILD_RETRY_WAIT}"
  fi
  i=$((i + 1))
done
if [ "${build_ok}" -ne 1 ]; then
  die "go build 连续 ${BUILD_RETRIES} 次失败，无法继续（可能是并行改动尚未收敛）"
fi
pass "go build 成功 → ${TMP}/catmail"

# ---------- 启动服务并等待就绪 ----------

mkdir -p "${TMP}/data"
CATMAIL_ADDR=":${PORT}" \
  CATMAIL_DB="${TMP}/data/catmail.db" \
  CATMAIL_ADMIN_USER=admin \
  CATMAIL_ADMIN_PASS=admin123 \
  "${TMP}/catmail" >"${TMP}/catmail.log" 2>&1 &
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
echo "服务已就绪：${BASE}（pid ${SERVER_PID}，数据目录 ${TMP}/data）"

# ---------- S1 健康检查 ----------

section "S1 GET /api/health"
do_curl "${BASE}/api/health"
expect_eq "health 返回 200" "200" "${STATUS}"
expect_contains 'health 返回 {"ok":true}' '"ok":true' "${BODY}"

# ---------- S2 登录拿会话 cookie ----------

section "S2 POST /api/auth/login（admin）"
do_curl -c "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' \
  "${BASE}/api/auth/login"
expect_eq "登录返回 200" "200" "${STATUS}"
expect_contains '登录响应含 username=admin' '"username":"admin"' "${BODY}"
if [ -f "${TMP}/cookies.txt" ] && grep -q 'catmail_session' "${TMP}/cookies.txt"; then
  pass "会话 cookie catmail_session 已写入"
else
  fail "会话 cookie catmail_session 未写入"
fi

# ---------- S3 未带会话访问管理接口 → 401 ----------

section "S3 未鉴权访问管理接口"
do_curl "${BASE}/api/channels"
expect_eq "无会话 GET /api/channels → 401" "401" "${STATUS}"
expect_contains "401 错误体 type=unauthorized" '"unauthorized"' "${BODY}"

# ---------- S4 建 mock 渠道 + ApiKey ----------

section "S4 创建 mock 渠道与 ApiKey"
CH_NAME="smoke-mock"
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-mock","provider":"mock","from_email":"smoke@example.com","from_name":"Smoke Tester"}' \
  "${BASE}/api/channels"
expect_eq "POST /api/channels → 200" "200" "${STATUS}"
CH_ID="$(int_or_0 "$(jget '.id')")"
if [ "${CH_ID}" -gt 0 ]; then
  pass "渠道已创建 id=${CH_ID}"
else
  fail "未取得渠道 id（响应 [$(printf '%s' "${BODY}" | head -c 300)]）"
fi
expect_eq "from_email 回显" "smoke@example.com" "$(jget '.from_email')"
expect_eq "provider=mock" "mock" "$(jget '.provider')"

do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-key"}' \
  "${BASE}/api/keys"
expect_eq "POST /api/keys → 200" "200" "${STATUS}"
API_KEY="$(jget '.key')"
if [ -n "${API_KEY}" ] && [ "${API_KEY}" != "null" ]; then
  pass "ApiKey 已创建（明文仅本次返回）"
else
  fail "未取得 ApiKey 明文"
fi

# ---------- S5 /v1/send 指定 mock 渠道发送 ----------

section "S5 POST /v1/send（Bearer ApiKey，指定 mock 渠道）"
SUBJECT="smoke-send-$(date +%s)"
do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
  -d "{\"from\":\"smoke@example.com\",\"to\":[\"rcpt@example.com\"],\"subject\":\"${SUBJECT}\",\"html\":\"<p>smoke</p>\",\"channel\":\"${CH_NAME}\"}" \
  "${BASE}/v1/send"
expect_eq "/v1/send 返回 200" "200" "${STATUS}"
expect_eq "accepted=true" "true" "$(jget '.accepted')"
MSG_ID_V1="$(jget '.id')"
if [ -n "${MSG_ID_V1}" ] && [ "${MSG_ID_V1}" != "null" ]; then
  pass "返回 message id=${MSG_ID_V1}"
else
  fail "未返回 message id"
fi

# ---------- S6 渠道测试发送成功 → used_day/used_month 各 +1 ----------

section "S6 渠道测试发送扣额度（used_day/used_month 各 +1）"
USED_DAY_BEFORE="$(int_or_0 "$(channel_field 'used_day')")"
USED_MONTH_BEFORE="$(int_or_0 "$(channel_field 'used_month')")"
expect_eq "前置：S5 的发送已计入额度 used_day=1" "1" "${USED_DAY_BEFORE}"
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"to":"rcpt@example.com"}' \
  "${BASE}/api/channels/${CH_ID}/test"
expect_eq "POST /api/channels/:id/test → 200" "200" "${STATUS}"
expect_eq "测试发送 ok=true" "true" "$(jget '.ok')"
USED_DAY_AFTER="$(int_or_0 "$(channel_field 'used_day')")"
USED_MONTH_AFTER="$(int_or_0 "$(channel_field 'used_month')")"
expect_eq "used_day +1（${USED_DAY_BEFORE} → ${USED_DAY_AFTER}）" \
  "$((USED_DAY_BEFORE + 1))" "${USED_DAY_AFTER}"
expect_eq "used_month +1（${USED_MONTH_BEFORE} → ${USED_MONTH_AFTER}）" \
  "$((USED_MONTH_BEFORE + 1))" "${USED_MONTH_AFTER}"

# ---------- S7 日额度 override=1 再测试 → 429 ----------

section "S7 日额度 override=1 后测试发送 → 429"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d '{"daily_limit_override":1}' \
  "${BASE}/api/channels/${CH_ID}"
expect_eq "PUT daily_limit_override=1 → 200" "200" "${STATUS}"
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"to":"rcpt@example.com"}' \
  "${BASE}/api/channels/${CH_ID}/test"
expect_eq "额度用尽时测试发送 → 429" "429" "${STATUS}"
expect_contains "429 文案含额度用尽" "额度已用尽" "${BODY}"
USED_DAY_REJECT="$(int_or_0 "$(channel_field 'used_day')")"
expect_eq "被拒测试不消耗额度 used_day 仍=${USED_DAY_AFTER}" "${USED_DAY_AFTER}" "${USED_DAY_REJECT}"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d '{"daily_limit_override":0}' \
  "${BASE}/api/channels/${CH_ID}"
expect_eq "恢复 override=0 → 200" "200" "${STATUS}"

# ---------- S8 POST /emails（to 为字符串） ----------

section "S8 POST /emails（Resend 兼容，to 为字符串）"
SUBJECT2="smoke-emails-$(date +%s)"
do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
  -d "{\"to\":\"string-rcpt@example.com\",\"subject\":\"${SUBJECT2}\",\"html\":\"<p>compat</p>\",\"channel\":\"${CH_NAME}\"}" \
  "${BASE}/emails"
expect_eq "POST /emails（to 字符串）返回 200" "200" "${STATUS}"
expect_eq "accepted=true" "true" "$(jget '.accepted')"
MSG_ID_EMAILS="$(jget '.id')"
if [ -n "${MSG_ID_EMAILS}" ] && [ "${MSG_ID_EMAILS}" != "null" ]; then
  pass "返回 message id=${MSG_ID_EMAILS}"
else
  fail "未返回 message id"
fi

# ---------- S9 发送日志列表与 CSV 导出 ----------

section "S9 发送日志列表与 CSV 导出"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/logs?page_size=100"
expect_eq "GET /api/logs → 200" "200" "${STATUS}"
expect_contains "日志含 /v1/send 记录" "${MSG_ID_V1}" "${BODY}"
expect_contains "日志含 /emails 记录" "${MSG_ID_EMAILS}" "${BODY}"
expect_contains "日志含测试发送记录 is_test=true" '"is_test":true' "${BODY}"

do_curl -b "${TMP}/cookies.txt" "${BASE}/api/logs/export"
expect_eq "GET /api/logs/export → 200" "200" "${STATUS}"
expect_contains "Content-Type 为 text/csv" "text/csv" "${CONTENT_TYPE}"
expect_contains "CSV 含表头「时间」" "时间" "${BODY}"
expect_contains "CSV 含 /v1/send 主题" "${SUBJECT}" "${BODY}"
expect_contains "CSV 含 /emails 主题" "${SUBJECT2}" "${BODY}"

# ---------- S10 设置读写回一致 ----------

section "S10 设置接口：改 log_retention_days 再读回"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/settings"
expect_eq "GET /api/settings → 200" "200" "${STATUS}"
CUR_RETENTION="$(int_or_0 "$(jget '.log_retention_days')")"
if [ "${CUR_RETENTION}" -eq 42 ]; then NEW_RETENTION=43; else NEW_RETENTION=42; fi
PUT_BODY="$(printf '%s' "${BODY}" | jq -c '{
  rotation: .rotation,
  max_attempts: .max_attempts,
  cooldown_seconds: .cooldown_seconds,
  consecutive_failure_threshold: .consecutive_failure_threshold,
  log_retention_days: (if .log_retention_days == 42 then 43 else 42 end)
}')"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d "${PUT_BODY}" "${BASE}/api/settings"
expect_eq "PUT /api/settings → 200" "200" "${STATUS}"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/settings"
expect_eq "读回 log_retention_days=${NEW_RETENTION}" "${NEW_RETENTION}" \
  "$(int_or_0 "$(jget '.log_retention_days')")"

# ---------- S11 /api 未知路径 → JSON 404 ----------

section "S11 /api 未知路径返回 JSON 404"
do_curl "${BASE}/api/definitely-not-a-real-endpoint"
expect_eq "GET /api/definitely-not-a-real-endpoint → 404" "404" "${STATUS}"
expect_contains "404 响应为统一 JSON 错误体" "接口不存在" "${BODY}"
expect_contains "Content-Type 为 application/json" "application/json" "${CONTENT_TYPE}"
expect_not_contains "不回落到 SPA index.html" "<html" "${BODY}"

# ---------- S12 并发额度压测（30 条并发 /v1/send → 恰好 10 成功 20 个 429） ----------

section "S12 并发 30 条 /v1/send：日额度 override=10 的 mock 渠道不超发不少发"
# 独立新渠道：S5/S6 已使 smoke-mock 的 used_day=2，起始计数非 0 会破坏
# "恰好 10 个成功"的精确断言；override 建号时直接带上。
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-burst","provider":"mock","from_email":"smoke@example.com","daily_limit_override":10,"monthly_limit_override":0}' \
  "${BASE}/api/channels"
expect_eq "POST /api/channels（smoke-burst）→ 200" "200" "${STATUS}"
BURST_CH_ID="$(int_or_0 "$(jget '.id')")"
if [ "${BURST_CH_ID}" -gt 0 ]; then
  pass "压测渠道已创建 id=${BURST_CH_ID}（override=10，used_day 从 0 起算）"
else
  fail "未取得压测渠道 id（响应 [$(printf '%s' "${BODY}" | head -c 300)]）"
fi

# 30 条并发：curl 后台任务同时发（指定 channel 排除种子示例渠道的 failover 干扰），
# 每条状态码落独立文件，wait 后汇总。条件 UPDATE 原子预留保证恰好 10 个 200。
# 注意：只 wait curl 的 pid（无参 wait 会连 SERVER_PID 一起等，而服务进程永不退出）。
BURST_DIR="${TMP}/burst"
mkdir -p "${BURST_DIR}"
BURST_PIDS=""
for i in $(seq 1 30); do
  # -w 带换行：无换行时 cat *.code 会拼成一行，grep '^200$' 逐行计数全为 0
  curl -sS --max-time "${CURL_MAX_TIME}" -o /dev/null -w '%{http_code}\n' \
    -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
    -d "{\"from\":\"smoke@example.com\",\"to\":[\"rcpt@example.com\"],\"subject\":\"smoke-burst-${i}\",\"text\":\"hi\",\"channel\":\"smoke-burst\"}" \
    "${BASE}/v1/send" >"${BURST_DIR}/$i.code" 2>/dev/null &
  BURST_PIDS="${BURST_PIDS} $!"
done
wait ${BURST_PIDS} || true
BURST_200="$(cat "${BURST_DIR}"/*.code | grep -c '^200$' || true)"
BURST_429="$(cat "${BURST_DIR}"/*.code | grep -c '^429$' || true)"
BURST_OTHER=$((30 - BURST_200 - BURST_429))
expect_eq "并发 30 条：200 恰好 10 个（不超发）" "10" "${BURST_200}"
expect_eq "并发 30 条：429 恰好 20 个（超额拒绝）" "20" "${BURST_429}"
expect_eq "并发 30 条：无其它状态码（000/5xx 视为传输或服务异常）" "0" "${BURST_OTHER}"
BURST_USED_DAY="$(int_or_0 "$(burst_channel_field 'used_day')")"
expect_eq "压测渠道 used_day 终值=10（不多不少）" "10" "${BURST_USED_DAY}"

# ---------- S13 失败通知链路 ----------

section "S13 失败通知链路（本地 webhook 接收器 + mock 渠道 401 自动禁用）"
# 接收器：python3 内置 http.server，监听 127.0.0.1:18110，POST body 原样追加
# 落文件（每条一行 JSON），GET 仅作就绪探针；用完即杀（trap 兜底）。
WEBHOOK_FILE="${TMP}/webhook_recv.jsonl"
WEBHOOK_PID=""
WEBHOOK_RECV_FILE="${WEBHOOK_FILE}" python3 - >"${TMP}/webhook_server.log" 2>&1 <<'PY' &
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def _ok(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')
    def do_GET(self):
        self._ok()
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        with open(os.environ["WEBHOOK_RECV_FILE"], "ab") as f:
            f.write(self.rfile.read(n) + b"\n")
        self._ok()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", 18110), H).serve_forever()
PY
WEBHOOK_PID=$!
EXTRA_PIDS="${EXTRA_PIDS} ${WEBHOOK_PID}"

hook_ready=0
for i in $(seq 1 40); do
  code=$(curl -s --max-time 1 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT_HOOK}/ping" 2>/dev/null || true)
  if [ "${code}" = "200" ]; then hook_ready=1; break; fi
  kill -0 "${WEBHOOK_PID}" 2>/dev/null || break
  sleep 0.25
done
expect_eq "webhook 接收器就绪（127.0.0.1:${PORT_HOOK}）" "1" "${hook_ready}"

# 开启通知双通道：站内 + webhook（URL 指向接收器），事件白名单只放行
# channel_disabled（S13 的 502 终态失败事件 send_failed 不应进任何通道）。
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/settings"
NOTIFY_PUT_BODY="$(printf '%s' "${BODY}" | jq -c 'del(.weak_admin_password)
  | .notify_inapp_enabled = true
  | .notify_webhook_enabled = true
  | .notify_webhook_url = "'"${HOOK_URL}"'"
  | .notify_events = "channel_disabled"')"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d "${NOTIFY_PUT_BODY}" "${BASE}/api/settings"
expect_eq "PUT /api/settings 开启 webhook 通知（events=channel_disabled）→ 200" "200" "${STATUS}"

# fail_auth 魔法值渠道：mock 适配器对该 APIKey 模拟 401 → 触发自动禁用事件。
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-notify","provider":"mock","from_email":"smoke@example.com","api_key":"fail_auth"}' \
  "${BASE}/api/channels"
expect_eq "POST /api/channels（smoke-notify，api_key=fail_auth 魔法值）→ 200" "200" "${STATUS}"
do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
  -d '{"from":"smoke@example.com","to":["rcpt@example.com"],"subject":"smoke-notify-fail","html":"<p>notify</p>","channel":"smoke-notify"}' \
  "${BASE}/v1/send"
expect_eq "fail_auth 渠道发送 → 502（候选穷尽）" "502" "${STATUS}"

# 401 → 立即 AUTO_DISABLED（status=2）+ 落自动禁用原因（发送同步路径内完成）。
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/channels"
expect_eq "smoke-notify 已被自动禁用 status=2" "2" \
  "$(printf '%s' "${BODY}" | jq -r '(.items[] | select(.name == "smoke-notify")) | .status' 2>/dev/null)"
expect_contains "auto_disabled_reason 已记录（401 认证失败）" "401" \
  "$(printf '%s' "${BODY}" | jq -r '(.items[] | select(.name == "smoke-notify")) | .auto_disabled_reason' 2>/dev/null)"

# webhook 异步投递：安全加固三件套 B 改造后走 outbox 落库投递，由 scheduler
# 每分钟驱动（cron * * * * *，秒 0 触发）——入队后最坏等 60s 才到下一轮，
# 轮询窗口须覆盖一个完整周期（75s），且接收器在轮询期间保持存活、轮询成功后再杀。
hook_hit=0
for i in $(seq 1 75); do
  if [ -f "${WEBHOOK_FILE}" ] && grep -q '"event":"channel_disabled"' "${WEBHOOK_FILE}" 2>/dev/null; then
    hook_hit=1
    break
  fi
  sleep 1
done
expect_eq "接收器收到 event=channel_disabled 的 JSON" "1" "${hook_hit}"
expect_contains "webhook payload 含 source=catmail" '"source":"catmail"' \
  "$(cat "${WEBHOOK_FILE}" 2>/dev/null || true)"
expect_not_contains "事件白名单生效：未投递 send_failed 事件" '"event":"send_failed"' \
  "$(cat "${WEBHOOK_FILE}" 2>/dev/null || true)"

# 站内通知：列表含该条 → 未读 ≥1 → 标记已读 → 归 0。
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/notifications?page_size=20"
expect_eq "GET /api/notifications → 200" "200" "${STATUS}"
expect_contains "通知列表含 channel_disabled 事件" '"event":"channel_disabled"' "${BODY}"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/notifications/unread_count"
expect_eq "GET /api/notifications/unread_count → 200" "200" "${STATUS}"
UNREAD="$(int_or_0 "$(jget '.count')")"
if [ "${UNREAD}" -ge 1 ]; then
  pass "未读数 ≥1（实际 ${UNREAD}）"
else
  fail "未读数 ≥1（实际 ${UNREAD}）"
fi
do_curl -b "${TMP}/cookies.txt" -X POST -H 'Content-Type: application/json' \
  -d '{}' "${BASE}/api/notifications/read"
expect_eq "POST /api/notifications/read → 200" "200" "${STATUS}"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/notifications/unread_count"
expect_eq "标记已读后未读数归 0" "0" "$(int_or_0 "$(jget '.count')")"

# 还原通知设置（避免后续场景向已杀掉的接收器投递）；接收器用完即杀。
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/settings"
NOTIFY_PUT_BODY="$(printf '%s' "${BODY}" | jq -c 'del(.weak_admin_password)
  | .notify_inapp_enabled = false
  | .notify_webhook_enabled = false
  | .notify_webhook_url = ""
  | .notify_events = ""')"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d "${NOTIFY_PUT_BODY}" "${BASE}/api/settings"
expect_eq "还原通知设置为关闭 → 200" "200" "${STATUS}"
if [ -n "${WEBHOOK_PID}" ] && kill -0 "${WEBHOOK_PID}" 2>/dev/null; then
  kill "${WEBHOOK_PID}" 2>/dev/null || true
  wait "${WEBHOOK_PID}" 2>/dev/null || true
fi
WEBHOOK_PID=""

# ---------- S14 TOTP 两步验证 ----------

section "S14 TOTP 两步验证（setup/enable → 登录两步 → 恢复码单次有效 → disable）"
TOTP_JAR="${TMP}/totp-cookies.txt"
# 本节用独立 cookie jar，全程与主会话隔离；enable/disable 会 bump 用户纪元，
# 结尾用主 jar 重新登录（既验证「普通登录恢复」又为 S15+ 恢复主会话）。
do_curl -c "${TOTP_JAR}" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' \
  "${BASE}/api/auth/login"
expect_eq "S14 前置：密码登录（独立 jar）→ 200" "200" "${STATUS}"
do_curl -b "${TOTP_JAR}" -X POST "${BASE}/api/auth/totp/setup"
if [ "${STATUS}" = "404" ]; then
  skip "S14 TOTP 整节（待 E1 落地后终验复跑）：/api/auth/totp/* 尚未合并"
else
  expect_eq "POST /api/auth/totp/setup → 200" "200" "${STATUS}"
  TOTP_SECRET="$(jget '.secret')"
  if [ -n "${TOTP_SECRET}" ] && [ "${TOTP_SECRET}" != "null" ]; then
    pass "返回 base32 secret（仅本次响应可见）"
  else
    fail "未取得 TOTP secret"
  fi
  expect_contains "otpauth_url 指向 totp" "otpauth://totp/" "$(jget '.otpauth_url')"

  TOTP_CODE="$(totp_code "${TOTP_SECRET}")"
  case "${TOTP_CODE}" in
    [0-9][0-9][0-9][0-9][0-9][0-9]) pass "RFC 6238 算法生成 6 位验证码" ;;
    *) fail "TOTP 码非 6 位数字（[${TOTP_CODE}]）" ;;
  esac
  ENABLE_BODY="$(jq -cn --arg code "${TOTP_CODE}" '{code: $code}')"
  # enable 会 bump 用户纪元并重签当前会话 cookie：-c 把新 cookie 写回 jar，
  # 否则后续 logout 携带旧纪元 token 必然 401
  do_curl -c "${TOTP_JAR}" -b "${TOTP_JAR}" -X POST -H 'Content-Type: application/json' \
    -d "${ENABLE_BODY}" "${BASE}/api/auth/totp/enable"
  expect_eq "POST /api/auth/totp/enable → 200" "200" "${STATUS}"
  RECOVERY_CODE="$(jget '.recovery_codes[0]')"
  if [ -n "${RECOVERY_CODE}" ] && [ "${RECOVERY_CODE}" != "null" ]; then
    pass "enable 返回恢复码（明文仅此一次）"
  else
    fail "未取得恢复码"
  fi
  expect_eq "恢复码共 8 个" "8" "$(jget '.recovery_codes | length')"

  do_curl -b "${TOTP_JAR}" -X POST "${BASE}/api/auth/logout"
  expect_eq "logout → 200" "200" "${STATUS}"
  do_curl -c "${TOTP_JAR}" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' \
    "${BASE}/api/auth/login"
  expect_eq "已启用后密码登录 → 200 且 totp_required=true" "200" "${STATUS}"
  expect_contains "登录响应含 totp_required=true" '"totp_required":true' "${BODY}"
  TOTP_TICKET="$(jget '.ticket')"
  if [ -n "${TOTP_TICKET}" ] && [ "${TOTP_TICKET}" != "null" ]; then
    pass "返回两步登录 ticket（5 分钟短票据）"
  else
    fail "未取得两步登录 ticket"
  fi

  TOTP_CODE="$(totp_code "${TOTP_SECRET}")"
  TOTP_BODY="$(jq -cn --arg ticket "${TOTP_TICKET}" --arg code "${TOTP_CODE}" '{ticket: $ticket, code: $code}')"
  do_curl -c "${TOTP_JAR}" -b "${TOTP_JAR}" -X POST -H 'Content-Type: application/json' \
    -d "${TOTP_BODY}" "${BASE}/api/auth/login/totp"
  expect_eq "POST /api/auth/login/totp（TOTP 码）→ 200" "200" "${STATUS}"
  expect_contains "两步登录响应 username=admin" '"username":"admin"' "${BODY}"

  do_curl -b "${TOTP_JAR}" -X POST "${BASE}/api/auth/logout"
  do_curl -c "${TOTP_JAR}" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' \
    "${BASE}/api/auth/login"
  expect_eq "再次密码登录拿 ticket" "200" "${STATUS}"
  TOTP_TICKET="$(jget '.ticket')"
  RC_BODY="$(jq -cn --arg ticket "${TOTP_TICKET}" --arg code "${RECOVERY_CODE}" '{ticket: $ticket, code: $code}')"
  do_curl -c "${TOTP_JAR}" -b "${TOTP_JAR}" -X POST -H 'Content-Type: application/json' \
    -d "${RC_BODY}" "${BASE}/api/auth/login/totp"
  expect_eq "恢复码登录 → 200" "200" "${STATUS}"
  expect_contains "恢复码登录成功 username=admin" '"username":"admin"' "${BODY}"

  do_curl -b "${TOTP_JAR}" -X POST "${BASE}/api/auth/logout"
  do_curl -c "${TOTP_JAR}" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' \
    "${BASE}/api/auth/login"
  TOTP_TICKET="$(jget '.ticket')"
  do_curl -b "${TOTP_JAR}" -X POST -H 'Content-Type: application/json' \
    -d "${RC_BODY}" "${BASE}/api/auth/login/totp"
  expect_eq "同一恢复码二次使用 → 401（单次有效，命中即删）" "401" "${STATUS}"
  # 恢复码已被消费：改用 TOTP 码重新登录取会话，供 disable 使用（票据仍有效）
  TOTP_CODE="$(totp_code "${TOTP_SECRET}")"
  TOTP_BODY="$(jq -cn --arg ticket "${TOTP_TICKET}" --arg code "${TOTP_CODE}" '{ticket: $ticket, code: $code}')"
  do_curl -c "${TOTP_JAR}" -b "${TOTP_JAR}" -X POST -H 'Content-Type: application/json' \
    -d "${TOTP_BODY}" "${BASE}/api/auth/login/totp"
  expect_eq "重新以 TOTP 码登录取会话 → 200" "200" "${STATUS}"

  DISABLE_BODY="$(jq -cn --arg pw "admin123" '{password: $pw}')"
  do_curl -b "${TOTP_JAR}" -X POST -H 'Content-Type: application/json' \
    -d "${DISABLE_BODY}" "${BASE}/api/auth/totp/disable"
  expect_eq "POST /api/auth/totp/disable（凭密码）→ 200" "200" "${STATUS}"
  expect_contains "disable 响应 ok=true" '"ok":true' "${BODY}"

  do_curl -c "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' \
    "${BASE}/api/auth/login"
  expect_eq "解绑后普通登录恢复 → 200" "200" "${STATUS}"
  expect_contains "响应 username=admin（不再要求两步）" '"username":"admin"' "${BODY}"
  expect_not_contains "登录不再返回 totp_required" "totp_required" "${BODY}"
fi

# ---------- S15 备份与日志清理 ----------

section "S15 备份（创建/列表/下载/删除/路径穿越）与日志清理（purge）"
do_curl -b "${TMP}/cookies.txt" -X POST "${BASE}/api/backups"
if [ "${STATUS}" = "404" ]; then
  skip "S15 备份整节（待 E2 落地后终验复跑）：/api/backups 与 /api/logs/purge 尚未合并"
else
  expect_eq "POST /api/backups → 200" "200" "${STATUS}"
  BK1="$(jget '.name')"
  if [ -n "${BK1}" ] && [ "${BK1}" != "null" ]; then
    pass "备份已创建 ${BK1}（size=$(jget '.size')）"
  else
    fail "未取得备份文件名"
  fi
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/backups"
  expect_eq "GET /api/backups → 200" "200" "${STATUS}"
  expect_contains "列表含新建备份" "${BK1}" "${BODY}"
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/backups/${BK1}/download"
  expect_eq "GET /api/backups/:name/download → 200" "200" "${STATUS}"
  # 文件头 16 字节须为 SQLite 魔数 "SQLite format 3\0"（hex：53514c…3300）
  BK_MAGIC="$(head -c 16 "${TMP}/body.txt" 2>/dev/null | od -An -tx1 | tr -d ' \n')"
  expect_eq "文件头为 SQLite 魔数（SQLite format 3\\0）" "53514c69746520666f726d6174203300" "${BK_MAGIC}"
  expect_contains "Content-Disposition 带备份文件名" "${BK1}" \
    "$(grep -i '^content-disposition:' "${TMP}/headers.txt" 2>/dev/null | head -n1 | tr -d '\r')"

  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/logs?page_size=1"
  TOTAL_BEFORE="$(int_or_0 "$(jget '.total')")"
  do_curl -b "${TMP}/cookies.txt" -X POST -H 'Content-Type: application/json' \
    -d '{"mode":"before_days","days":3650}' "${BASE}/api/logs/purge"
  expect_eq "purge mode=before_days days=3650 → 200" "200" "${STATUS}"
  expect_eq "日志均新于 3650 天 → deleted=0（不误删）" "0" "$(int_or_0 "$(jget '.deleted')")"
  # 实际删除路径：按渠道清空 S12 压测渠道的发送日志（恰好 10 条成功发送）
  do_curl -b "${TMP}/cookies.txt" -X POST -H 'Content-Type: application/json' \
    -d "{\"mode\":\"channel\",\"channel_id\":${BURST_CH_ID}}" "${BASE}/api/logs/purge"
  expect_eq "purge mode=channel（smoke-burst）→ 200" "200" "${STATUS}"
  PURGE_DELETED="$(int_or_0 "$(jget '.deleted')")"
  expect_eq "smoke-burst 渠道日志恰好删除 10 条" "10" "${PURGE_DELETED}"
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/logs?page_size=1"
  expect_eq "日志总数 ${TOTAL_BEFORE} → $((TOTAL_BEFORE - PURGE_DELETED))" \
    "$((TOTAL_BEFORE - PURGE_DELETED))" "$(int_or_0 "$(jget '.total')")"

  do_curl -b "${TMP}/cookies.txt" -X POST "${BASE}/api/backups"
  expect_eq "再建一档 → 200" "200" "${STATUS}"
  BK2="$(jget '.name')"
  if [ -n "${BK2}" ] && [ "${BK2}" != "${BK1}" ]; then
    pass "新档名时间戳推进（${BK2} ≠ ${BK1}）"
  else
    fail "新档名与旧档重名或为空（[${BK2}]）"
  fi
  do_curl -b "${TMP}/cookies.txt" -X DELETE "${BASE}/api/backups/${BK2}"
  expect_eq "DELETE /api/backups/:name（裁剪）→ 200" "200" "${STATUS}"
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/backups"
  expect_not_contains "列表不再含已删备份" "${BK2}" "${BODY}"

  # 路径穿越：--path-as-is 保留 ../ 原样发出；文件名白名单一步拒绝，不得泄文件
  do_curl --path-as-is -b "${TMP}/cookies.txt" \
    "${BASE}/api/backups/../../etc/passwd"
  if [ "${STATUS}" = "400" ] || [ "${STATUS}" = "404" ]; then
    pass "穿越请求 /api/backups/../../etc/passwd 被拒绝（${STATUS}）"
  else
    fail "穿越请求未拒绝（实际 ${STATUS}）"
  fi
  expect_not_contains "响应体不含 /etc/passwd 内容" "root:" "${BODY}"
fi

# ---------- S16 渠道复制与日志重发 ----------

section "S16 渠道复制（副本停用 + 重名后缀）与日志重发（新 id + 30 秒防重）"
do_curl -b "${TMP}/cookies.txt" -X POST "${BASE}/api/channels/${CH_ID}/duplicate"
if [ "${STATUS}" = "404" ]; then
  skip "S16 渠道复制/日志重发整节（待 E6 落地后终验复跑）：duplicate/resend 尚未合并"
else
  expect_eq "POST /api/channels/:id/duplicate → 200" "200" "${STATUS}"
  expect_eq "副本名 = smoke-mock-副本" "smoke-mock-副本" "$(jget '.name')"
  expect_eq "副本默认停用 status=1（manual_disabled）" "1" "$(jget '.status')"
  expect_eq "副本字段继承 provider=mock" "mock" "$(jget '.provider')"
  expect_eq "副本字段继承 from_email" "smoke@example.com" "$(jget '.from_email')"
  expect_eq "副本运行态归零 used_day=0" "0" "$(int_or_0 "$(jget '.used_day')")"
  do_curl -b "${TMP}/cookies.txt" -X POST "${BASE}/api/channels/${CH_ID}/duplicate"
  expect_eq "再次 duplicate → 200" "200" "${STATUS}"
  expect_eq "重名后缀依次追加 → smoke-mock-副本-2" "smoke-mock-副本-2" "$(jget '.name')"

  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/logs?page_size=100"
  RESEND_LOG_ID="$(printf '%s' "${BODY}" | jq -r --arg m "${MSG_ID_V1}" \
    '(.items[] | select(.message_id == $m)) | .id' 2>/dev/null | head -n1)"
  if [ -n "${RESEND_LOG_ID}" ] && [ "${RESEND_LOG_ID}" != "null" ]; then
    pass "定位 S5 日志 id=${RESEND_LOG_ID}（message_id=${MSG_ID_V1}）"
  else
    fail "未定位到 S5 日志"
  fi
  do_curl -b "${TMP}/cookies.txt" -X POST "${BASE}/api/logs/${RESEND_LOG_ID}/resend"
  expect_eq "POST /api/logs/:id/resend → 200" "200" "${STATUS}"
  expect_eq "重发 accepted=true" "true" "$(jget '.accepted')"
  RESEND_MSG_ID="$(jget '.id')"
  if [ -n "${RESEND_MSG_ID}" ] && [ "${RESEND_MSG_ID}" != "${MSG_ID_V1}" ]; then
    pass "重发生成新 message_id=${RESEND_MSG_ID}（≠原 ${MSG_ID_V1}）"
  else
    fail "重发 message_id 缺失或与原相同（[${RESEND_MSG_ID}]）"
  fi
  do_curl -b "${TMP}/cookies.txt" -X POST "${BASE}/api/logs/${RESEND_LOG_ID}/resend"
  expect_eq "30 秒内二次重发 → 429" "429" "${STATUS}"
  expect_contains "429 文案含 30 秒内已重发" "30 秒内已重发" "${BODY}"
fi

# ---------- S17 metrics 与管理 IP 白名单 ----------

section "S17 GET /metrics 与管理 IP 白名单（第二实例 18120）"
do_curl "${BASE}/metrics"
expect_eq "GET /metrics → 200" "200" "${STATUS}"
expect_contains "Content-Type 为 Prometheus 文本格式" "text/plain" "${CONTENT_TYPE}"
expect_contains "含 catmail_sends_total 样本行" "catmail_sends_total{" "${BODY}"
expect_contains "含 catmail_channels 样本行" "catmail_channels{" "${BODY}"
expect_contains "中文 HELP 文案正常（UTF-8）" "全量发送计数" "${BODY}"
expect_contains "中文 label 值正常（副本渠道名入 gauge）" 'channel="smoke-mock-副本"' "${BODY}"

# 第二实例：CATMAIL_ADMIN_CIDR=10.255.255.0/24 使本机 127.0.0.1 不在白名单，
# 模拟「外部来源」访问——管理接口与 /metrics 403，公开探针 /api/health 200。
mkdir -p "${TMP}/data2"
CATMAIL_ADDR=":${PORT2}" \
  CATMAIL_DB="${TMP}/data2/catmail.db" \
  CATMAIL_ADMIN_CIDR="10.255.255.0/24" \
  CATMAIL_ADMIN_USER=admin \
  CATMAIL_ADMIN_PASS=admin123 \
  "${TMP}/catmail" >"${TMP}/catmail2.log" 2>&1 &
SERVER2_PID=$!
EXTRA_PIDS="${EXTRA_PIDS} ${SERVER2_PID}"
ready2=0
for i in $(seq 1 60); do
  code=$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${BASE2}/api/health" 2>/dev/null || true)
  if [ "${code}" = "200" ]; then ready2=1; break; fi
  kill -0 "${SERVER2_PID}" 2>/dev/null || break
  sleep 0.5
done
expect_eq "第二实例就绪（health 200，端口 ${PORT2}）" "1" "${ready2}"
do_curl "${BASE2}/api/channels"
expect_eq "白名单外访问管理接口 /api/channels → 403" "403" "${STATUS}"
do_curl -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' "${BASE2}/api/auth/login"
expect_eq "白名单外登录也拦截 → 403" "403" "${STATUS}"
do_curl "${BASE2}/api/health"
expect_eq "/api/health 不受白名单约束 → 200" "200" "${STATUS}"
do_curl "${BASE2}/metrics"
expect_eq "/metrics 同受白名单约束 → 403" "403" "${STATUS}"
if [ -n "${SERVER2_PID}" ] && kill -0 "${SERVER2_PID}" 2>/dev/null; then
  kill "${SERVER2_PID}" 2>/dev/null || true
  wait "${SERVER2_PID}" 2>/dev/null || true
fi
SERVER2_PID=""

# CATMAIL_TRUSTED_PROXIES 非法值：config.Load 快速失败，进程启动即退出（exit≠0）
CATMAIL_ADDR=":${PORT2}" \
  CATMAIL_DB="${TMP}/data2/catmail.db" \
  CATMAIL_TRUSTED_PROXIES="not-a-cidr" \
  "${TMP}/catmail" >"${TMP}/catmail3.log" 2>&1 &
TP_PID=$!
EXTRA_PIDS="${EXTRA_PIDS} ${TP_PID}"
tp_exited=0
for i in $(seq 1 10); do
  kill -0 "${TP_PID}" 2>/dev/null || { tp_exited=1; break; }
  sleep 0.5
done
TP_RC=0
wait "${TP_PID}" 2>/dev/null || TP_RC=$?
if [ "${tp_exited}" = "1" ] && [ "${TP_RC}" -ne 0 ]; then
  pass "非法 CATMAIL_TRUSTED_PROXIES 启动即退出（exit=${TP_RC}）"
else
  kill "${TP_PID}" 2>/dev/null || true
  fail "非法 CATMAIL_TRUSTED_PROXIES 未使进程退出（exited=${tp_exited}，exit=${TP_RC}）"
fi

# ---------- S18 审计日志 ----------

section "S18 管理操作审计（settings 变更 → GET /api/audit）"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/settings"
AUDIT_PUT_BODY="$(printf '%s' "${BODY}" | jq -c 'del(.weak_admin_password)
  | .log_retention_days = (if .log_retention_days == 42 then 43 else 42 end)')"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d "${AUDIT_PUT_BODY}" "${BASE}/api/settings"
expect_eq "PUT /api/settings（制造 settings 变更）→ 200" "200" "${STATUS}"
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/audit?page_size=20&action=settings_update"
if [ "${STATUS}" = "404" ]; then
  skip "S18 审计断言（待 E4 落地后终验复跑）：GET /api/audit 尚未合并"
else
  expect_eq "GET /api/audit?action=settings_update → 200" "200" "${STATUS}"
  expect_contains "审计含 settings_update 记录" '"action":"settings_update"' "${BODY}"
  expect_contains "审计记录操作者 admin" '"username":"admin"' "${BODY}"
fi

# ---------- S19 邮件模板 ----------

section "S19 邮件模板（变量自动提取 → 模板发送渲染 → unknown_template / body_conflict → PUT/DELETE 生命周期）"
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-tpl","subject":"订单 {{order_id}} {{extra}} 发货通知","html":"<p>亲爱的 {{user.name}}，订单 {{order_id}} 已发货。</p>","text":"亲爱的 {{user.name}}，订单 {{order_id}} 已发货。"}' \
  "${BASE}/api/templates"
if [ "${STATUS}" = "404" ]; then
  skip "S19 邮件模板整节（待终验复跑）：/api/templates 尚未合并"
else
  expect_eq "POST /api/templates → 200" "200" "${STATUS}"
  TPL_ID="$(int_or_0 "$(jget '.id')")"
  if [ "${TPL_ID}" -gt 0 ]; then
    pass "模板已创建 id=${TPL_ID}"
  else
    fail "未取得模板 id（响应 [$(printf '%s' "${BODY}" | head -c 300)]）"
  fi
  # 变量清单是内容派生数据：请求未传 variables 时按 subject/html/text 自动提取
  TPL_VARS="$(jget '.variables | join(",")')"
  expect_contains "变量自动提取：含 order_id" "order_id" "${TPL_VARS}"
  expect_contains "变量自动提取：含嵌套点路径 user.name" "user.name" "${TPL_VARS}"
  expect_contains "变量自动提取：含 extra" "extra" "${TPL_VARS}"

  # 模板发送：请求不带 subject/html/text（正文与主题由模板渲染）；
  # 仅提供 order_id 与 user.name，extra 缺失 → 响应 missing_variables 可见
  do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
    -d "{\"from\":\"smoke@example.com\",\"to\":[\"tpl@example.com\"],\"template_id\":${TPL_ID},\"variables\":{\"order_id\":\"10086\",\"user\":{\"name\":\"小明\"}},\"channel\":\"${CH_NAME}\"}" \
    "${BASE}/v1/send"
  expect_eq "模板发送（无 subject/html/text）→ 200" "200" "${STATUS}"
  expect_eq "模板发送 accepted=true" "true" "$(jget '.accepted')"
  expect_contains "响应含 missing_variables 字段" '"missing_variables"' "${BODY}"
  expect_contains "missing_variables 列出未提供的 extra" "extra" \
    "$(jget '.missing_variables | join(",")')"
  TPL_MSG_ID="$(jget '.id')"
  if [ -n "${TPL_MSG_ID}" ] && [ "${TPL_MSG_ID}" != "null" ]; then
    pass "模板发送返回 message id=${TPL_MSG_ID}"
  else
    fail "模板发送未返回 message id"
  fi

  # 发送日志：subject 为渲染结果（变量注入、无未渲染占位符），template_id 回显
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/logs?page_size=20&q=tpl@example.com"
  expect_eq "GET /api/logs?q=tpl@example.com → 200" "200" "${STATUS}"
  LOG_ROW="$(printf '%s' "${BODY}" | jq -c --arg m "${TPL_MSG_ID}" \
    '(.items[] | select(.message_id == $m))' 2>/dev/null || true)"
  expect_contains "日志定位到模板发送记录" '"template_id":'"${TPL_ID}" "${LOG_ROW}"
  LOG_SUBJECT="$(printf '%s' "${LOG_ROW}" | jq -r '.subject' 2>/dev/null || true)"
  expect_contains "SendLog subject 已渲染（order_id=10086 注入）" "订单 10086" "${LOG_SUBJECT}"
  expect_not_contains "SendLog subject 无未渲染占位符" "{{" "${LOG_SUBJECT}"

  # 未知模板：400 unknown_template（错误体风格与统一错误体一致）
  do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
    -d '{"from":"smoke@example.com","to":["tpl@example.com"],"template_id":999999,"channel":"smoke-mock"}' \
    "${BASE}/v1/send"
  expect_eq "未知模板发送 → 400" "400" "${STATUS}"
  expect_contains "错误类型 type=unknown_template" '"unknown_template"' "${BODY}"

  # body 冲突：template_id 与自带 html/text 互斥 → 400 body_conflict
  do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
    -d "{\"from\":\"smoke@example.com\",\"to\":[\"tpl@example.com\"],\"template_id\":${TPL_ID},\"html\":\"<p>conflict</p>\",\"channel\":\"${CH_NAME}\"}" \
    "${BASE}/v1/send"
  expect_eq "template_id + html 同带 → 400" "400" "${STATUS}"
  expect_contains "错误类型 type=body_conflict" '"body_conflict"' "${BODY}"

  # 生命周期：PUT 更新（变量清单随内容重提取）→ 详情 → DELETE → 详情 404
  do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
    -d '{"subject":"smoke-tpl-v2 {{new_var}} 通知"}' \
    "${BASE}/api/templates/${TPL_ID}"
  expect_eq "PUT /api/templates/:id → 200" "200" "${STATUS}"
  expect_contains "PUT 后 subject 已更新" "smoke-tpl-v2" "$(jget '.subject')"
  expect_contains "variables 随内容重提取（含 new_var）" "new_var" \
    "$(jget '.variables | join(",")')"
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/templates/${TPL_ID}"
  expect_eq "GET /api/templates/:id → 200" "200" "${STATUS}"
  do_curl -b "${TMP}/cookies.txt" -X DELETE "${BASE}/api/templates/${TPL_ID}"
  expect_eq "DELETE /api/templates/:id → 200" "200" "${STATUS}"
  expect_contains "DELETE 响应 ok=true" '"ok":true' "${BODY}"
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/templates/${TPL_ID}"
  expect_eq "删除后 GET 详情 → 404" "404" "${STATUS}"
fi

# ---------- S20 送达事件与抑制 ----------

section "S20 送达事件与抑制（webhook token 轮换 → 事件摄取 → 自动抑制 → recipient_suppressed → 解除）"
do_curl -b "${TMP}/cookies.txt" -H 'Content-Type: application/json' \
  -d '{"name":"smoke-events","provider":"mock","from_email":"smoke@example.com"}' \
  "${BASE}/api/channels"
expect_eq "POST /api/channels（smoke-events）→ 200" "200" "${STATUS}"
CH_EV_ID="$(int_or_0 "$(jget '.id')")"
CH_EV_TOKEN="$(jget '.webhook_token')"
if [ "${CH_EV_ID}" -gt 0 ] && [ -n "${CH_EV_TOKEN}" ] && [ "${CH_EV_TOKEN}" != "null" ]; then
  pass "渠道已建且响应携带 webhook_token（创建即生成）"
else
  fail "未取得渠道 id 或 webhook_token（响应 [$(printf '%s' "${BODY}" | head -c 300)]）"
fi
# 前置对齐：S10 的最小 PUT 请求体经 putSettings 整表绑定（非指针字段缺省=零值）
# 会把未携带的 suppression_auto_enabled 清零——按全量 PUT 恢复默认开启，保证
# hard bounce 自动入库断言只依赖本节行为。
do_curl -b "${TMP}/cookies.txt" "${BASE}/api/settings"
SUP_PUT_BODY="$(printf '%s' "${BODY}" | jq -c 'del(.weak_admin_password)
  | .suppression_auto_enabled = true')"
do_curl -b "${TMP}/cookies.txt" -X PUT -H 'Content-Type: application/json' \
  -d "${SUP_PUT_BODY}" "${BASE}/api/settings"
expect_eq "PUT /api/settings 开启 suppression_auto_enabled → 200" "200" "${STATUS}"
do_curl -b "${TMP}/cookies.txt" -X POST -H 'Content-Type: application/json' \
  -d '{"rotate":true}' "${BASE}/api/channels/${CH_EV_ID}/webhook-token"
if [ "${STATUS}" = "404" ]; then
  skip "S20 送达事件整节（待终验复跑）：/api/channels/:id/webhook-token 尚未合并"
else
  expect_eq "POST /api/channels/:id/webhook-token（rotate=true）→ 200" "200" "${STATUS}"
  expect_contains "返回 webhook_url 相对路径" "/webhooks/mail-events/" "$(jget '.webhook_url')"
  CH_EV_TOKEN2="$(jget '.webhook_token')"
  if [ -n "${CH_EV_TOKEN2}" ] && [ "${CH_EV_TOKEN2}" != "${CH_EV_TOKEN}" ]; then
    pass "token 已轮换（新 token ≠ 旧 token）"
  else
    fail "token 未轮换或为空（[${CH_EV_TOKEN2}]）"
  fi

  # 旧 token 立即失效（token 唯一索引精确匹配）
  do_curl -H 'Content-Type: application/json' \
    -d '{"event":"delivered","recipient":"evt-ok@example.com"}' \
    "${BASE}/webhooks/mail-events/${CH_EV_TOKEN}"
  expect_eq "旧 token 投递 → 404（轮换即失效）" "404" "${STATUS}"

  # 新 token 投递（generic 平台形态：无签名头走 URL token 校验路径）
  do_curl -H 'Content-Type: application/json' \
    -d '{"event":"delivered","recipient":"evt-ok@example.com","message_id":"<evt-msg-1@example.com>"}' \
    "${BASE}/webhooks/mail-events/${CH_EV_TOKEN2}"
  expect_eq "delivered 事件投递 → 200" "200" "${STATUS}"
  expect_contains "投递响应 ok=true" '"ok":true' "${BODY}"
  do_curl -H 'Content-Type: application/json' \
    -d '{"event":"hard_bounce","recipient":"evt-sup@example.com","message_id":"evt-msg-2"}' \
    "${BASE}/webhooks/mail-events/${CH_EV_TOKEN2}"
  expect_eq "hard_bounce 事件投递 → 200" "200" "${STATUS}"

  # 事件入库：管理端查询两条（delivered + bounced/hard）
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/mail-events?channel_id=${CH_EV_ID}&page_size=10"
  expect_eq "GET /api/mail-events → 200" "200" "${STATUS}"
  expect_eq "渠道事件两条（total=2）" "2" "$(int_or_0 "$(jget '.total')")"
  expect_contains "含 delivered 事件" '"event":"delivered"' "${BODY}"
  expect_contains "含 bounced 事件（hard_bounce 归一化）" '"event":"bounced"' "${BODY}"
  expect_eq "bounced 事件 bounce_type=hard" "hard" \
    "$(printf '%s' "${BODY}" | jq -r '(.items[] | select(.event == "bounced")) | .bounce_type' 2>/dev/null)"

  # 抑制列表自动入库（hard bounce 默认开启 suppression_auto_enabled）
  do_curl -b "${TMP}/cookies.txt" "${BASE}/api/suppressions?q=evt-sup@example.com"
  expect_eq "GET /api/suppressions?q= → 200" "200" "${STATUS}"
  expect_eq "自动入库恰好一条（total=1）" "1" "$(int_or_0 "$(jget '.total')")"
  expect_eq "抑制地址小写归一" "evt-sup@example.com" "$(jget '.items[0].email')"
  expect_eq "抑制原因 reason=bounce（事件自动入库）" "bounce" "$(jget '.items[0].reason')"
  expect_eq "来源渠道 channel_name=smoke-events" "smoke-events" "$(jget '.items[0].channel_name')"
  SUP_ID="$(int_or_0 "$(jget '.items[0].id')")"

  # 发送拦截：rotator.Send 单点 → 400 invalid_request + code=recipient_suppressed
  do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
    -d '{"from":"smoke@example.com","to":["evt-sup@example.com"],"subject":"smoke-suppressed","text":"hi","channel":"smoke-events"}' \
    "${BASE}/v1/send"
  expect_eq "带抑制地址发送 → 400" "400" "${STATUS}"
  expect_contains "错误码 code=recipient_suppressed" '"recipient_suppressed"' "${BODY}"
  expect_contains "错误信息列出命中收件人" "evt-sup@example.com" "${BODY}"

  # 解除后同地址可再发送
  do_curl -b "${TMP}/cookies.txt" -X DELETE "${BASE}/api/suppressions/${SUP_ID}"
  expect_eq "DELETE /api/suppressions/:id → 200" "200" "${STATUS}"
  do_curl -H "Authorization: Bearer ${API_KEY}" -H 'Content-Type: application/json' \
    -d '{"from":"smoke@example.com","to":["evt-sup@example.com"],"subject":"smoke-unsuppressed","text":"hi","channel":"smoke-events"}' \
    "${BASE}/v1/send"
  expect_eq "解除抑制后同地址发送 → 200" "200" "${STATUS}"
  expect_eq "解除后 accepted=true" "true" "$(jget '.accepted')"
fi

# ---------- S21 SMTP 入口 TLS ----------

section "S21 SMTP 入口 TLS（隐式 TLS 握手 / 明文广播 STARTTLS / require 下未升级 AUTH → 530）"
S21_OK=1
if ! command -v openssl >/dev/null 2>&1; then
  skip "S21 SMTP TLS 整节：缺少 openssl（无法生成自签证书）"
  S21_OK=0
fi
if [ "${S21_OK}" = "1" ]; then
  # 自签证书（ECDSA/RSA 皆可，握手侧 InsecureSkipVerify 不校验链）
  if openssl req -x509 -newkey rsa:2048 -sha256 -days 2 -nodes \
    -subj "/CN=127.0.0.1" \
    -keyout "${TMP}/smtp-tls-key.pem" -out "${TMP}/smtp-tls-cert.pem" >/dev/null 2>&1; then
    pass "自签证书已生成（openssl，CN=127.0.0.1，写入临时目录）"
  else
    skip "S21 SMTP TLS 整节：openssl 自签证书生成失败"
    S21_OK=0
  fi
fi
if [ "${S21_OK}" = "1" ]; then
  # 第二实例：明文 SMTP + 隐式 TLS SMTP 同进程，policy=require（须先配证书）
  mkdir -p "${TMP}/data-tls"
  CATMAIL_ADDR=":${PORT_TLS_HTTP}" \
    CATMAIL_DB="${TMP}/data-tls/catmail.db" \
    CATMAIL_SMTP_ADDR=":${PORT_TLS_SMTP}" \
    CATMAIL_SMTP_TLS_ADDR=":${PORT_TLS_IMPLICIT}" \
    CATMAIL_SMTP_CERT_FILE="${TMP}/smtp-tls-cert.pem" \
    CATMAIL_SMTP_KEY_FILE="${TMP}/smtp-tls-key.pem" \
    CATMAIL_SMTP_TLS_POLICY=require \
    CATMAIL_ADMIN_USER=admin \
    CATMAIL_ADMIN_PASS=admin123 \
    "${TMP}/catmail" >"${TMP}/catmail-tls.log" 2>&1 &
  TLS_PID=$!
  EXTRA_PIDS="${EXTRA_PIDS} ${TLS_PID}"
  ready_tls=0
  for i in $(seq 1 60); do
    code=$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${BASE_TLS}/api/health" 2>/dev/null || true)
    if [ "${code}" = "200" ]; then ready_tls=1; break; fi
    kill -0 "${TLS_PID}" 2>/dev/null || break
    sleep 0.5
  done
  if [ "${ready_tls}" = "1" ]; then
    pass "TLS 实例就绪（HTTP ${PORT_TLS_HTTP} + SMTP ${PORT_TLS_SMTP}/${PORT_TLS_IMPLICIT}）"
  else
    fail "TLS 实例未就绪（配置校验失败或端口冲突，见 ${TMP}/catmail-tls.log）"
    S21_OK=0
  fi
fi
if [ "${S21_OK}" = "1" ]; then
  # 等 SMTP 明文监听给出 220 问候（与 HTTP 就绪间可能有亚秒级错位）
  smtp_up=0
  for i in $(seq 1 30); do
    b220="$(python3 -c "
import socket
try:
    s = socket.create_connection(('127.0.0.1', ${PORT_TLS_SMTP}), timeout=2)
    print(s.recv(64)[:3].decode())
    s.close()
except Exception:
    pass" 2>/dev/null || true)"
    if [ "${b220}" = "220" ]; then smtp_up=1; break; fi
    sleep 0.5
  done
  expect_eq "SMTP 明文监听就绪（220 问候）" "1" "${smtp_up}"

  # 1) 465 风格隐式 TLS：TLS 握手 + 220 问候 + EHLO 250
  TLS_HS="$(python3 - "${PORT_TLS_IMPLICIT}" <<'PY'
import socket, ssl, sys
port = int(sys.argv[1])
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
try:
    raw = socket.create_connection(("127.0.0.1", port), timeout=10)
    with ctx.wrap_socket(raw) as s:
        banner = s.recv(1024).decode("utf-8", "replace")
        s.sendall(b"EHLO smoke\r\n")
        resp = s.recv(4096).decode("utf-8", "replace")
    print("1" if banner.startswith("220") and resp.startswith("250") else "0")
except Exception:
    print("0")
PY
)"
  expect_eq "隐式 TLS 监听握手可用（TLS + 220 + EHLO 250）" "1" "${TLS_HS}"

  # 2) 明文监听 EHLO 多行应答广播 STARTTLS（证书已配置即广播，RFC 3207）
  STARTTLS_ADV="$(python3 - "${PORT_TLS_SMTP}" <<'PY'
import socket, sys
port = int(sys.argv[1])
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=10)
    f = s.makefile("rb")
    banner = f.readline().decode("utf-8", "replace")
    s.sendall(b"EHLO smoke\r\n")
    lines = []
    while True:
        ln = f.readline()
        if not ln:
            break
        lines.append(ln.decode("utf-8", "replace"))
        if len(ln) < 4 or ln[3:4] == b" ":  # 末行：code 后为空格（续行为 -）
            break
    s.close()
    ok = banner.startswith("220") and any("STARTTLS" in l for l in lines)
    print("1" if ok else "0")
except Exception:
    print("0")
PY
)"
  expect_eq "明文监听 EHLO 广播 STARTTLS" "1" "${STARTTLS_ADV}"

  # 3) policy=require：未 STARTTLS 先 AUTH → 530 5.7.0 Must issue a STARTTLS first
  REQUIRE_530="$(python3 - "${PORT_TLS_SMTP}" <<'PY'
import base64, socket, sys
port = int(sys.argv[1])
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=10)
    f = s.makefile("rb")
    f.readline()  # 220 banner
    s.sendall(b"EHLO smoke\r\n")
    while True:
        ln = f.readline()
        if not ln or ln[3:4] == b" ":
            break
    s.sendall(b"AUTH PLAIN " + base64.b64encode(b"\x00smoke\x00not-a-key") + b"\r\n")
    reply = f.readline().decode("utf-8", "replace")
    s.close()
    print("1" if reply.startswith("530") else "0")
except Exception:
    print("0")
PY
)"
  expect_eq "require 策略：明文未升级先 AUTH → 530" "1" "${REQUIRE_530}"
fi
if [ -n "${TLS_PID:-}" ] && kill -0 "${TLS_PID}" 2>/dev/null; then
  kill "${TLS_PID}" 2>/dev/null || true
  wait "${TLS_PID}" 2>/dev/null || true
fi
TLS_PID=""

# ---------- S22 安全加固三件套 ----------

section "S22 安全加固三件套（SECRET_KEY 密文落库 / 密码重置联动 / 回执签名 / 会话管理，独立实例 ${PORT_SEC_HTTP}）"
SEC_KEY="$(python3 -c 'import os, base64; print(base64.b64encode(os.urandom(32)).decode())' 2>/dev/null || true)"
if [ -z "${SEC_KEY}" ]; then
  skip "S22 整节：无法生成 CATMAIL_SECRET_KEY（python3 base64）"
else
  mkdir -p "${TMP}/data-sec"
  CATMAIL_ADDR=":${PORT_SEC_HTTP}" \
    CATMAIL_DB="${TMP}/data-sec/catmail.db" \
    CATMAIL_SECRET_KEY="${SEC_KEY}" \
    CATMAIL_ADMIN_USER=admin \
    CATMAIL_ADMIN_PASS=admin123 \
    "${TMP}/catmail" >"${TMP}/catmail-sec.log" 2>&1 &
  SEC_PID=$!
  EXTRA_PIDS="${EXTRA_PIDS} ${SEC_PID}"
  ready_sec=0
  for i in $(seq 1 60); do
    code=$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "${BASE_SEC}/api/health" 2>/dev/null || true)
    if [ "${code}" = "200" ]; then ready_sec=1; break; fi
    kill -0 "${SEC_PID}" 2>/dev/null || break
    sleep 0.5
  done
  if [ "${ready_sec}" = "1" ]; then
    pass "安全加固实例就绪（CATMAIL_SECRET_KEY 已配置，端口 ${PORT_SEC_HTTP}）"
  else
    fail "安全加固实例未就绪（SECRET_KEY 非法或端口冲突，见 ${TMP}/catmail-sec.log）"
  fi
  do_curl -c "${TMP}/cookies-sec.txt" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' "${BASE_SEC}/api/auth/login"
  expect_eq "加密实例 admin 登录 → 200" "200" "${STATUS}"

  # A. 渠道凭据加密落库：明文入参 → 库中 api_key 仅密文（enc:v1: 前缀，无明文残留）
  SEC_PLAIN_SECRET="sk-plain-smoke-9f3ac2"
  do_curl -b "${TMP}/cookies-sec.txt" -H 'Content-Type: application/json' \
    -d "{\"name\":\"smoke-enc\",\"provider\":\"mock\",\"from_email\":\"smoke@example.com\",\"api_key\":\"${SEC_PLAIN_SECRET}\"}" \
    "${BASE_SEC}/api/channels"
  expect_eq "加密实例建渠道（api_key 明文入参）→ 200" "200" "${STATUS}"
  ENC_KEY_VAL="$(python3 - "${TMP}/data-sec/catmail.db" <<'PY'
import sqlite3, sys, time
val = ""
for _ in range(10):  # WAL 并发读兜底重试（busy_timeout 双保险）
    try:
        conn = sqlite3.connect(sys.argv[1], timeout=5)
        row = conn.execute(
            "SELECT api_key FROM channels WHERE name = 'smoke-enc'").fetchone()
        val = row[0] if row and row[0] is not None else ""
        conn.close()
        break
    except Exception:
        time.sleep(0.5)
print(val)
PY
)"
  expect_contains "渠道 api_key 落库为密文（enc:v1: 前缀）" "enc:v1:" "${ENC_KEY_VAL}"
  expect_not_contains "库中 api_key 无明文残留" "${SEC_PLAIN_SECRET}" "${ENC_KEY_VAL}"

  # B. 密码重置联动三件套：目标用户旧会话 401 + 其名下 Key 停用（disabled_reason）
  do_curl -b "${TMP}/cookies-sec.txt" -H 'Content-Type: application/json' \
    -d '{"username":"smokeops","password":"ops-pass-123"}' "${BASE_SEC}/api/users"
  if [ "${STATUS}" = "404" ]; then
    skip "S22 账号联动子段：/api/users 尚未合并"
  else
    expect_eq "POST /api/users（smokeops）→ 200" "200" "${STATUS}"
    U2_ID="$(int_or_0 "$(jget '.id')")"
    do_curl -c "${TMP}/cookies-ops.txt" -H 'Content-Type: application/json' \
      -d '{"username":"smokeops","password":"ops-pass-123"}' "${BASE_SEC}/api/auth/login"
    expect_eq "smokeops 登录 → 200" "200" "${STATUS}"
    do_curl -b "${TMP}/cookies-ops.txt" -H 'Content-Type: application/json' \
      -d '{"name":"smoke-ops-key"}' "${BASE_SEC}/api/keys"
    expect_eq "smokeops 名下建 Key → 200" "200" "${STATUS}"
    U2_KEY_ID="$(int_or_0 "$(jget '.id')")"
    U2_KEY_PLAIN="$(jget '.key')"
    if [ "${U2_KEY_ID}" -gt 0 ] && [ -n "${U2_KEY_PLAIN}" ] && [ "${U2_KEY_PLAIN}" != "null" ]; then
      pass "smokeops Key 已创建 id=${U2_KEY_ID}（created_by=smokeops）"
    else
      fail "未取得 smokeops Key 明文"
    fi

    do_curl -b "${TMP}/cookies-sec.txt" -X PUT -H 'Content-Type: application/json' \
      -d '{"new_password":"ops-new-pass-456"}' "${BASE_SEC}/api/users/${U2_ID}/password"
    expect_eq "PUT /api/users/:id/password → 200" "200" "${STATUS}"
    do_curl -b "${TMP}/cookies-ops.txt" "${BASE_SEC}/api/auth/me"
    expect_eq "重置后 smokeops 旧会话 → 401（纪元+注册表吊销）" "401" "${STATUS}"
    do_curl -c "${TMP}/cookies-ops2.txt" -H 'Content-Type: application/json' \
      -d '{"username":"smokeops","password":"ops-new-pass-456"}' "${BASE_SEC}/api/auth/login"
    expect_eq "smokeops 新密码登录 → 200" "200" "${STATUS}"

    do_curl -b "${TMP}/cookies-sec.txt" "${BASE_SEC}/api/keys"
    expect_eq "联动停用 enabled=false" "false" \
      "$(printf '%s' "${BODY}" | jq -r --argjson id "${U2_KEY_ID}" \
        '(.items[] | select(.id == $id)) | (.enabled | tostring)' 2>/dev/null)"
    expect_eq "disabled_reason=password_reset" "password_reset" \
      "$(printf '%s' "${BODY}" | jq -r --argjson id "${U2_KEY_ID}" \
        '(.items[] | select(.id == $id)) | .disabled_reason' 2>/dev/null)"
    do_curl -H "Authorization: Bearer ${U2_KEY_PLAIN}" -H 'Content-Type: application/json' \
      -d '{"from":"smoke@example.com","to":["sec@example.com"],"subject":"sec-should-fail","text":"hi"}' \
      "${BASE_SEC}/v1/send"
    expect_eq "被联动停用的 Key 调用 /v1/send → 401" "401" "${STATUS}"
  fi

  # C. 回执签名：本地接收器 + Key 配 webhook → outbox 投递 → HMAC-SHA256 验签
  RECEIPT_FILE="${TMP}/receipt_recv.jsonl"
  RECEIPT_RECV_FILE="${RECEIPT_FILE}" RECEIPT_PORT="${PORT_HOOK2}" \
    python3 - >"${TMP}/receipt_server.log" 2>&1 <<'PY' &
import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def _ok(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')
    def do_GET(self):
        self._ok()
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        row = json.dumps({
            "body": self.rfile.read(n).decode("utf-8", "replace"),
            "sig": self.headers.get("X-CatMail-Signature", ""),
        })
        with open(os.environ["RECEIPT_RECV_FILE"], "ab") as f:
            f.write(row.encode() + b"\n")
        self._ok()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", int(os.environ["RECEIPT_PORT"])), H).serve_forever()
PY
  RECEIPT_PID=$!
  EXTRA_PIDS="${EXTRA_PIDS} ${RECEIPT_PID}"
  receipt_ready=0
  for i in $(seq 1 40); do
    code=$(curl -s --max-time 1 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT_HOOK2}/ping" 2>/dev/null || true)
    if [ "${code}" = "200" ]; then receipt_ready=1; break; fi
    kill -0 "${RECEIPT_PID}" 2>/dev/null || break
    sleep 0.25
  done
  expect_eq "回执接收器就绪（127.0.0.1:${PORT_HOOK2}）" "1" "${receipt_ready}"

  do_curl -b "${TMP}/cookies-sec.txt" -H 'Content-Type: application/json' \
    -d '{"name":"smoke-rcpt-key"}' "${BASE_SEC}/api/keys"
  expect_eq "创建回执 Key → 200" "200" "${STATUS}"
  RCPT_KEY_ID="$(int_or_0 "$(jget '.id')")"
  RCPT_KEY_PLAIN="$(jget '.key')"
  do_curl -b "${TMP}/cookies-sec.txt" -X PUT -H 'Content-Type: application/json' \
    -d "{\"webhook_url\":\"http://127.0.0.1:${PORT_HOOK2}/hook\",\"webhook_events\":\"send_accepted\"}" \
    "${BASE_SEC}/api/keys/${RCPT_KEY_ID}"
  expect_eq "PUT /api/keys/:id 配置回执 webhook → 200" "200" "${STATUS}"
  RCPT_SECRET="$(jget '.webhook_secret')"
  if [ -n "${RCPT_SECRET}" ] && [ "${RCPT_SECRET}" != "null" ]; then
    pass "回执签名密钥已回显（创建 Key 自动生成）"
  else
    fail "未取得回执签名密钥 webhook_secret"
  fi
  # 轮换签名密钥（轮换后投递按新值签名——验签用轮换后的值）
  do_curl -b "${TMP}/cookies-sec.txt" -X POST "${BASE_SEC}/api/keys/${RCPT_KEY_ID}/webhook-secret"
  if [ "${STATUS}" = "404" ]; then
    skip "POST /api/keys/:id/webhook-secret 尚未合并（沿用创建时密钥验签）"
  else
    expect_eq "POST /api/keys/:id/webhook-secret 轮换 → 200" "200" "${STATUS}"
    RCPT_SECRET="$(jget '.webhook_secret')"
  fi

  do_curl -H "Authorization: Bearer ${RCPT_KEY_PLAIN}" -H 'Content-Type: application/json' \
    -d '{"from":"smoke@example.com","to":["rcpt-sec@example.com"],"subject":"smoke-sec-receipt","text":"hi","channel":"smoke-enc"}' \
    "${BASE_SEC}/v1/send"
  expect_eq "回执 Key 发送 → 200（send_accepted 入 outbox）" "200" "${STATUS}"

  # outbox 由 scheduler 每分钟驱动投递：轮询接收器落文件，最长 90s（覆盖一个
  # 完整 cron 周期 + 余量）。接收器落盘的是 {"body":"<转义 JSON>","sig":...} 行，
  # event 名以裸子串形态出现（body 内引号已被 JSON 转义），故 grep 用裸事件名。
  receipt_hit=0
  for i in $(seq 1 90); do
    if [ -f "${RECEIPT_FILE}" ] && grep -q 'send_accepted' "${RECEIPT_FILE}" 2>/dev/null; then
      receipt_hit=1
      break
    fi
    sleep 1
  done
  expect_eq "outbox 回执投递到达接收器（最长 90s）" "1" "${receipt_hit}"
  # 验签：X-CatMail-Signature: t=<unix秒>,v1=hex(HMAC-SHA256(secret, "<t>."+body))
  SIG_OK="$(python3 - "${RECEIPT_FILE}" "${RCPT_SECRET}" <<'PY'
import hashlib, hmac, json, sys
path, secret = sys.argv[1], sys.argv[2]
ok = "0"
try:
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            row = json.loads(line)
            body, sig = row.get("body", ""), row.get("sig", "")
            parts = dict(p.split("=", 1) for p in sig.split(",") if "=" in p)
            t, v1 = parts.get("t", ""), parts.get("v1", "")
            if not t or not v1:
                continue
            mac = hmac.new(secret.encode(), (t + "." + body).encode(),
                           hashlib.sha256).hexdigest()
            if hmac.compare_digest(mac, v1):
                ok = "1"
                break
except Exception:
    ok = "0"
print(ok)
PY
)"
  expect_eq '回执签名验签通过（HMAC-SHA256(secret, t+"."+body)，X-CatMail-Signature）' "1" "${SIG_OK}"
  # envelope 结构断言（python 解析转义 body：source=catmail 且 event=send_accepted）
  ENV_OK="$(python3 - "${RECEIPT_FILE}" <<'PY'
import json, sys
ok = "0"
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            env = json.loads(json.loads(line).get("body", ""))
            if env.get("source") == "catmail" and env.get("event") == "send_accepted":
                ok = "1"
                break
except Exception:
    ok = "0"
print(ok)
PY
)"
  expect_eq "回执 envelope source=catmail event=send_accepted" "1" "${ENV_OK}"

  do_curl -b "${TMP}/cookies-sec.txt" "${BASE_SEC}/api/webhook-deliveries?status=sent&page_size=20"
  if [ "${STATUS}" = "404" ]; then
    skip "GET /api/webhook-deliveries 尚未合并"
  else
    expect_eq "GET /api/webhook-deliveries?status=sent → 200" "200" "${STATUS}"
    SENT_HIT="$(printf '%s' "${BODY}" | jq -r \
      '(.items[] | select(.event_type == "send_accepted" and .status == "sent")) | .id' \
      2>/dev/null | head -n1)"
    if [ -n "${SENT_HIT}" ]; then
      pass "outbox 有 status=sent 的 send_accepted 投递记录（id=${SENT_HIT}）"
    else
      fail "outbox 无 status=sent 的 send_accepted 投递记录"
    fi
  fi

  # D. 会话注册表：列表 current 标记 → revoke-others 踢其余 → 旧 cookie 即时 401
  do_curl -c "${TMP}/cookies-sec2.txt" -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"admin123"}' "${BASE_SEC}/api/auth/login"
  expect_eq "admin 第二会话登录 → 200" "200" "${STATUS}"
  do_curl -b "${TMP}/cookies-sec.txt" "${BASE_SEC}/api/sessions"
  if [ "${STATUS}" = "404" ]; then
    skip "S22 会话管理子段：/api/sessions 尚未合并"
  else
    expect_eq "GET /api/sessions → 200" "200" "${STATUS}"
    expect_contains "会话列表含当前会话 current=true" '"current":true' "${BODY}"
    SEC_SESSIONS_N="$(int_or_0 "$(jget '.items | length')")"
    if [ "${SEC_SESSIONS_N}" -ge 2 ]; then
      pass "admin 活跃会话 ≥2（实际 ${SEC_SESSIONS_N}）"
    else
      fail "admin 活跃会话 ≥2（实际 ${SEC_SESSIONS_N}）"
    fi
    do_curl -b "${TMP}/cookies-sec.txt" -X POST -H 'Content-Type: application/json' \
      -d '{}' "${BASE_SEC}/api/sessions/revoke-others"
    expect_eq "POST /api/sessions/revoke-others → 200" "200" "${STATUS}"
    REVOKED_N="$(int_or_0 "$(jget '.revoked')")"
    if [ "${REVOKED_N}" -ge 1 ]; then
      pass "revoke-others 下线其余会话 ≥1（实际 ${REVOKED_N}）"
    else
      fail "revoke-others 下线其余会话 ≥1（实际 ${REVOKED_N}）"
    fi
    do_curl -b "${TMP}/cookies-sec2.txt" "${BASE_SEC}/api/auth/me"
    expect_eq "被下线会话旧 cookie → 401" "401" "${STATUS}"
    do_curl -b "${TMP}/cookies-sec.txt" "${BASE_SEC}/api/sessions"
    expect_eq "当前会话不受 revoke-others 影响 → 200" "200" "${STATUS}"
  fi

  # E. 通知侧签名密钥轮换端点（防整表单保存误清的独立变更入口）
  do_curl -b "${TMP}/cookies-sec.txt" -X POST "${BASE_SEC}/api/settings/notify/webhook-secret"
  if [ "${STATUS}" = "404" ]; then
    skip "POST /api/settings/notify/webhook-secret 尚未合并"
  else
    expect_eq "POST /api/settings/notify/webhook-secret → 200" "200" "${STATUS}"
    expect_contains "响应回显新密钥 webhook_secret" '"webhook_secret"' "${BODY}"
  fi

  if [ -n "${RECEIPT_PID}" ] && kill -0 "${RECEIPT_PID}" 2>/dev/null; then
    kill "${RECEIPT_PID}" 2>/dev/null || true
    wait "${RECEIPT_PID}" 2>/dev/null || true
  fi
  RECEIPT_PID=""
  if [ -n "${SEC_PID}" ] && kill -0 "${SEC_PID}" 2>/dev/null; then
    kill "${SEC_PID}" 2>/dev/null || true
    wait "${SEC_PID}" 2>/dev/null || true
  fi
  SEC_PID=""
fi

printf '\n== 汇总 ==\n'
printf '通过 %d 项，失败 %d 项，跳过 %d 项（SKIP=端点未合并，待终验复跑）\n' \
  "${PASS_COUNT}" "${FAIL_COUNT}" "${SKIP_COUNT}"
if [ "${FAIL_COUNT}" -gt 0 ]; then
  exit 1
fi
exit 0
