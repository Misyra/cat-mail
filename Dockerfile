# CatMail 多阶段构建：
#   1) node 阶段：npm ci + vite build，前端产物输出到 backend/internal/web/dist
#   2) go 阶段：CGO_ENABLED=0 编译纯静态单二进制（SQLite 走 glebarez 纯 Go 驱动，无 CGO）
#   3) alpine 运行阶段：非 root 运行，SQLite 固定落盘 /data
#
# 运行镜像选型：alpine（而非 scratch）。
#   - alpine 自带 busybox wget，可直接做 HEALTHCHECK 探测 GET /api/health；
#   - 补装 ca-certificates（发信渠道要调 HTTPS API）与 tzdata（定时任务按本地时区）；
#   - scratch 更小（约省 8MB），但没有 shell/wget，健康检查需另引静态探针二进制，
#     且同样要手工塞 ca-certificates，运维成本大于体积收益，故不采用。

# ---------- 1. 前端构建 ----------
FROM node:22-alpine AS frontend
# 默认走 npmmirror 加速，可用 --build-arg NPM_REGISTRY=https://registry.npmjs.org 覆盖
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm config set registry "${NPM_REGISTRY}" && npm ci
COPY frontend/ ./
# vite outDir 为 ../backend/internal/web/dist，产物落在 /src/backend/internal/web/dist
RUN npm run build

# ---------- 2. 后端编译 ----------
FROM golang:1.27-alpine AS backend
# 默认走 goproxy.cn 加速，可用 --build-arg GOPROXY=https://proxy.golang.org,direct 覆盖
ARG GOPROXY=https://goproxy.cn,direct
# 版本注入（E8 自动更新）：--build-arg VERSION=vX.Y.Z 注入 internal/version.Version，
# 未传默认 dev（doctor 与 /api/update/check 据此展示/比较版本号）。
ARG VERSION=dev
ENV GOPROXY=${GOPROXY}
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# 注入前端产物（go:embed 要求 dist 存在；.dockerignore 已排除本地旧产物，始终用本次构建的）
COPY --from=frontend /src/backend/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X catmail/internal/version.Version=${VERSION}" \
    -o /out/catmail ./cmd/catmail

# ---------- 3. 运行镜像 ----------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S catmail \
    && adduser -S -G catmail -u 1000 catmail \
    && mkdir -p /data && chown catmail:catmail /data
WORKDIR /app
COPY --from=backend /out/catmail /app/catmail
# 数据库路径固定到 /data：config.go 的默认值 ./data/catmail.db 相对工作目录，
# 这里用 CATMAIL_DB 环境变量覆盖，使数据落点与 WORKDIR 解耦、与挂载卷一致。
ENV CATMAIL_ADDR=":8080" \
    CATMAIL_DB="/data/catmail.db"
USER catmail
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/api/health || exit 1
ENTRYPOINT ["/app/catmail"]
