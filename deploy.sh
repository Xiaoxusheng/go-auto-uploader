#!/bin/bash
# ==========================================
# 分布式文件上传与录制系统 - 高性能一键部署脚本
# ==========================================

# 开启严格模式：遇到任何错误立刻终止退出，防止引起连锁故障
set -e

# ------------------------------------------
# 函数：log_info
# 功能：打印带有绿色的标准信息日志，提高控制台输出的维护性和可读性
# ------------------------------------------
function log_info() {
    echo -e "\033[32m[INFO] $1\033[0m"
}

# ------------------------------------------
# 函数：log_error
# 功能：打印带有红色的错误日志，用于醒目地提示部署过程中的异常拦截
# ------------------------------------------
function log_error() {
    echo -e "\033[31m[ERROR] $1\033[0m"
}

# ------------------------------------------
# 函数：check_env
# 功能：执行部署前置条件断言，检查当前是否拥有 root 权限以及 Golang 编译环境是否正常就绪
# ------------------------------------------
function check_env() {
    log_info "正在进行系统环境检查..."

    # systemd 注册服务需要系统最高权限
    if [ "$EUID" -ne 0 ]; then
        log_error "权限不足！请使用 root 权限运行此脚本 (例如: sudo ./deploy.sh)"
        exit 1
    fi

    # 检查本机是否已安装 Go 编译器
    if ! command -v go &> /dev/null; then
        log_error "未检测到 Go 环境，请先安装 Golang"
        exit 1
    fi
    log_info "环境检查通过：Root 权限与 Go 编译器已就绪。"
}

# ------------------------------------------
# 函数：build_project
# 功能：在当前源码目录下执行构建，使用极致优化参数生成名为 uploader 的二进制可执行程序
# ------------------------------------------
function build_project() {
    log_info "开始使用极致优化方案编译 Go 核心程序..."

    # 核心优化：-ldflags="-s -w" 用于剔除 DWARF 调试信息与符号表，有效减少体积并加速系统内存装载
    go build -ldflags="-s -w" -o uploader .

    # 赋予二进制文件可执行权限
    chmod +x uploader
    log_info "项目编译成功，已生成高性能 uploader 可执行程序。"
}

# ------------------------------------------
# 函数：setup_systemd
# 功能：动态提取当前运行所在绝对路径，生成适配本机的 systemd 服务配置以实现进程守护和开机自启
# ------------------------------------------
function setup_systemd() {
    log_info "正在注册 systemd 守护进程服务..."

    local CURRENT_DIR=$(pwd)
    local SERVICE_FILE="/etc/systemd/system/uploader.service"
    local DOWNLOAD_DIR="$CURRENT_DIR/downloads"

    # 确保默认的监控目录存在
    mkdir -p "$DOWNLOAD_DIR"

    # 动态写入系统服务配置，全面继承防系统强杀与自动恢复机制
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=File Auto Uploader Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple

# 动态映射当前工作绝对路径
ExecStart=$CURRENT_DIR/uploader \\
  -dirs=$DOWNLOAD_DIR \\
  -workers=5 \\
  -rate=10 \\
  -server=http://127.0.0.1:5244 \\
  -web-port=8888
WorkingDirectory=$CURRENT_DIR

# 异常自动重启保护
Restart=always
RestartSec=10

# 日志输出接管
StandardOutput=journal
StandardError=journal

# 资源限制策略（极致优化：避免大文件吞吐时被系统 OOM 误杀）
LimitNOFILE=1048576
OOMScoreAdjust=-1000

[Install]
WantedBy=multi-user.target
EOF
    log_info "systemd 服务配置文件已动态生成至: $SERVICE_FILE"
}

# ------------------------------------------
# 函数：start_service
# 功能：热重载系统服务配置队列，将其设置为开机自启并立即拉起后端上传录制服务
# ------------------------------------------
function start_service() {
    log_info "正在重载系统配置并拉起 uploader 服务..."

    systemctl daemon-reload
    systemctl enable uploader.service
    systemctl restart uploader.service

    log_info "========================================================="
    log_info "✨ 部署大功告成！分布式文件上传与录制引擎现已在后台全速运行。"
    log_info "您可以使用以下命令追踪实时日志："
    log_info "journalctl -u uploader.service -f"
    log_info "========================================================="
}

# ------------------------------------------
# 函数：main
# 功能：核心生命周期调度入口，按逻辑顺序组织并调用上述各个步骤模块
# ------------------------------------------
function main() {
    log_info "欢迎使用一键部署向导..."
    check_env
    build_project
    setup_systemd
    start_service
}

# 触发主控执行器
main