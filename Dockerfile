# syntax=docker/dockerfile:1
# 在构建机的架构上交叉编译，多架构镜像不需要 QEMU 模拟
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /he-ipv6-pool .

# 静态二进制，只做转发、不发起 TLS，不需要 CA 证书和 shell
FROM scratch
COPY --from=build /he-ipv6-pool /he-ipv6-pool
ENTRYPOINT ["/he-ipv6-pool"]
CMD ["-c", "/etc/he-ipv6-pool/config.json"]
