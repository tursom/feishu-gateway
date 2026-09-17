FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 gateway \
    && mkdir -p /data /locks /run/secrets && chown gateway:gateway /data /locks
COPY --from=build /gateway /usr/local/bin/feishu-gateway
USER gateway
WORKDIR /data
ENV HOST=0.0.0.0 PORT=8787 DATABASE_PATH=/data/gateway.sqlite FEISHU_LOCK_DIR=/locks
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/feishu-gateway"]
