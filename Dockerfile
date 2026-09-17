# Sirius 运行镜像：三阶段构建（前端 → Go → 运行层）。
#
# 前端与后端都从源码构建，因此 `docker compose up --build` 一条命令就够，
# 不要求先在宿主机装 node 或提交 web/dist。
# 运行层只带编译产物，不含 Go 工具链与 node_modules。

# ---- 阶段 1：构建前端 ----
FROM node:24-alpine AS web
WORKDIR /web
# 先只拷依赖清单，源码变动时这层仍能命中缓存。
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- 阶段 2：构建后端 ----
FROM golang:1.26-alpine AS build
WORKDIR /src
# 先只拷依赖清单，源码变动时这层仍能命中缓存。
# 用 go.* 而不是 "go.mod go.sum"：当前是零依赖、没有 go.sum，
# 写死 go.sum 会因匹配不到文件而构建失败；go.* 至少匹配 go.mod，
# 将来加了依赖也会自动带上 go.sum。
COPY go.* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# 纯静态二进制：运行层是 alpine，不依赖 glibc。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sirius ./cmd/sirius

# ---- 阶段 3：运行层 ----
FROM alpine:3.21
# ca-certificates：调用 AMKR 需要 TLS 根证书（AMKR 可能在 HTTPS 后面）。
# tzdata：日志时间戳需要正确时区。
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 sirius
WORKDIR /app
COPY --from=build /out/sirius /usr/local/bin/sirius
COPY --from=web /web/dist /app/web/dist

USER sirius
# 容器内必须绑 0.0.0.0 才能接收映射进来的流量（否则 -p 转发进不去）。
# 注意这与 AGENTS.md §4 不冲突：暴露面由 compose 的端口映射限定在**宿主回环**，
# 详见 docker-compose.yml 里 ports 的写法与说明。
ENV SIRIUS_ADDR=0.0.0.0:8080 \
    SIRIUS_STATIC_DIR=/app/web/dist \
    SIRIUS_LOG_LEVEL=info

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sirius"]
