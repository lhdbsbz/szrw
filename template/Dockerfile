# syntax=docker/dockerfile:1
# 运行镜像：宿主编排层（scripts/build-host.sh）在容器里产出 main，bake 把它拷进来。
# 平台与 attestation 策略在 docker-bake.hcl（不在这里）。
FROM docker.m.daocloud.io/library/alpine:latest

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata

ENV TZ=Asia/Shanghai
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

WORKDIR /app

COPY main .

EXPOSE 8080
CMD ["./main"]
