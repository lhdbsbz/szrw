# syntax=docker/dockerfile:1
# 框架工具链镜像：Go + Node + protoc + 公开 protoc 插件 + 质量工具。
#
# 为什么在 szrw-codegen 而不是某个产品仓：它是「用这套框架的每个项目」共用的东西。
# 产品仓只消费 szrw-builder:latest，不再各自维护一份 Dockerfile。
#
# 镜像只提供工具，不含任何项目路径/入口脚本：
#   docker run --rm -v "$PWD:/work" -w /work szrw-builder:latest bash -c '...'
#
# 构建：./scripts/build-builder.sh   （或 docker build -t szrw-builder:latest -f Dockerfile.builder .）
FROM ubuntu:24.04
ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl wget unzip xz-utils git perl \
    && rm -rf /var/lib/apt/lists/*

# Go 补丁版升上去不影响产物；1.27.2 起修掉 html/template 的 stdlib 漏洞（GO-2026-6599/6600 等）。
ARG GO_VERSION=1.27.2
RUN wget -qO /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

ARG NODE_VERSION=24.21.0
RUN wget -qO /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
    && tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz \
    && rm /tmp/node.tar.xz \
    && npm config set registry https://registry.npmmirror.com \
    && npm config set fetch-retries 5 \
    && npm config set fetch-retry-mintimeout 20000 \
    && npm config set fetch-retry-maxtimeout 120000 \
    && npm config set fetch-timeout 300000

# 只装 protoc 本体：google/api/*.proto 这类注解依赖由各项目随仓携带
# （IDE 只能索引磁盘上的文件，塞进镜像会让编辑器爆红而构建却是绿的）。
ARG PROTOC_VERSION=36.1
RUN wget -qO /tmp/protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip" \
    && unzip -q /tmp/protoc.zip -d /usr/local \
    && chmod +x /usr/local/bin/protoc \
    && rm /tmp/protoc.zip

ENV PATH="/usr/local/go/bin:/go/bin:${PATH}"
ENV GOPATH="/go"
ENV GOPROXY="https://goproxy.cn,direct"
ENV GOPRIVATE="github.com/lhdbsbz"
ENV TZ=Asia/Shanghai

# 公开 protoc 插件（框架自己的插件由使用方在运行时从挂载源码 go install）
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest \
    && go install github.com/mitchellh/protoc-gen-go-json@latest \
    && go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest

# 质量闸门工具：框架要求 gofmt/go vet/staticcheck/deadcode/govulncheck 都干净，
# 工具随镜像走，项目不需要在自己环境里装。
RUN go install honnef.co/go/tools/cmd/staticcheck@latest \
    && go install golang.org/x/tools/cmd/goimports@latest \
    && go install golang.org/x/tools/cmd/deadcode@latest \
    && go install golang.org/x/vuln/cmd/govulncheck@latest

# 不带 ENTRYPOINT/WORKDIR：镜像只提供工具，入口与工作目录由调用方决定。
