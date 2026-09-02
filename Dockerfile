FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
ARG APP_VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.appVersion=${APP_VERSION}" -o cline-proxy .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/cline-proxy .

EXPOSE 3457

# CLINE_PROXY_HOST 会被 main.go configuredHost() 读取，作为默认监听地址
ENV CLINE_PROXY_HOST=0.0.0.0

# 数据目录整体挂载（docker run -v /host/dir:/app/data 即持久化）：
# 单文件 bind mount 无法承载 tmp+rename 原子写（内核拒绝 rename 覆盖挂载点），
# 目录挂载无此限制且宿主机目录不存在时 Docker 自动创建的类型恰好正确
ENV CLINE_DATA_DIR=/app/data

ENTRYPOINT ["/app/cline-proxy"]
# 容器内必须监听 0.0.0.0，否则 -p 端口映射对外不可达
CMD ["-host", "0.0.0.0", "-port", "3457"]
