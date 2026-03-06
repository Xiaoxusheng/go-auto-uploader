# ==========================================
# 第一阶段：编译阶段 (Builder)
# ==========================================
FROM golang:1.21-alpine AS builder

# 设置环境变量：使用国内代理加速依赖下载，关闭 CGO 保证跨平台完美兼容
ENV GOPROXY=https://goproxy.cn,direct \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /build

# 复制所有源代码到容器中
COPY . .

# 初始化模块并下载依赖（加入容错：如果本地没有 go.mod，则自动生成）
RUN if [ ! -f go.mod ]; then go mod init auto-uploader; fi && go mod tidy

# 编译 Go 核心程序，通过 -ldflags 剥离调试信息，大幅减小二进制文件体积
RUN go build -ldflags="-s -w" -o uploader .

# ==========================================
# 第二阶段：运行环境 (Runtime)
# ==========================================
FROM alpine:latest

# 安装底层必须的所有神兵利器：
# ffmpeg: 核心录制和无损抽帧引擎
# chromium: chromedp 所需的无头浏览器环境（解析抖音）
# tzdata: 系统时区数据
# ca-certificates: 解决 HTTPS 请求 SSL 证书问题
RUN apk update && \
    apk add --no-cache \
    ffmpeg \
    chromium \
    tzdata \
    ca-certificates && \
    # 强制将容器时区设置为上海
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

# 将第一阶段编译好的极速二进制程序复制到系统命令目录
COPY --from=builder /build/uploader /usr/local/bin/uploader

# ✨ 【架构神来之笔】：将运行目录固定为 /data
# 以后代码里写死的所有 "./downloads", "config.json" 都会乖乖生成在容器的 /data 里，方便宿主机一键挂载提取！
WORKDIR /data

# 暴露控制台 Web 端口
EXPOSE 8080

# 启动引擎：并默认把底层监听目录指向 /data/downloads
ENTRYPOINT ["uploader", "-dirs", "/data/downloads"]