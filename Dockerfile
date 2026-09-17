FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /gateway /usr/local/bin/feishu-gateway
WORKDIR /data
ENV DATABASE_PATH=/data/gateway.sqlite
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/feishu-gateway"]
