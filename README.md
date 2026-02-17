# 📦 uploader

基于 Go 语言实现的**目录扫描 + 并发上传 + HTTP 上传 + Hash 去重 + 自动统计邮件报告**程序。
程序通过扫描指定目录，将文件并发上传到远程 HTTP 服务器，并支持限速控制、白天/夜晚速率策略、Hash 记录、防重复上传、状态统计输出、定时邮件报告。

---

## ✅ 已实现功能（来自代码真实实现）

* 多目录扫描（`-dirs`，逗号分隔）
* 递归扫描目录
* 文件 SHA256 计算
* Hash 去重（`uploaded_hash.db`）
* 并发上传（worker 模型）
* HTTP 上传接口
* 速率控制：

    * 固定限速（`-rate`）
    * 白天限速（`-day-rate`）
    * 夜晚限速（`-night-rate`）
* 上传队列模型（channel）
* 原子计数器统计（`sync/atomic`）
* 状态打印定时器（5 秒一次）
* 上传状态日志输出
* 成功文件统计
* HTML 邮件报告
* QQ 邮箱 SMTP 发送
* 定时邮件汇总报告
* systemd 服务管理支持
* shell 构建脚本
* shell 重启脚本

---

# 📂 项目结构

```
.
├── main.go        # 主程序
├── build.sh       # 构建脚本
├── restart.sh     # systemd 重启脚本
├── uploader       # 编译生成的二进制文件
└── uploaded_hash.db # 已上传文件hash数据库(运行生成)
```

---

# ⚙️ 编译构建

来自 `build.sh`：

```bash
#!/bin/bash
set -e

APP_NAME="uploader"
MAIN_FILE="main.go"

CGO_ENABLED=0 go build -o "$APP_NAME" "$MAIN_FILE"
```

### 使用方式

```bash
chmod +x build.sh
./build.sh
```

生成文件：

```bash
./uploader
```

---

# 🔁 服务重启

来自 `restart.sh`：

```bash
#!/bin/bash
set -e

SERVICE="uploader"

systemctl stop $SERVICE
sleep 2
systemctl start $SERVICE
systemctl status $SERVICE --no-pager
```

说明：
该脚本用于重启 systemd 中名为 `uploader` 的服务。

---

# ▶️ 程序启动方式

```bash
./uploader -dirs="/data/a,/data/b"
```

或完整参数：

```bash
./uploader \
-dirs="/data/a,/data/b" \
-server="http://127.0.0.1:5244" \
-workers=3 \
-rate=0 \
-day-rate=20 \
-night-rate=80 \
-report-minutes=360
```

---

# 🔧 启动参数完整说明（逐条对应代码）

以下参数全部来自 `flag` 定义：

```go
flag.StringVar(&dirs, "dirs", "", "扫描目录(逗号分隔)")
flag.StringVar(&server, "server", "http://127.0.0.1:5244", "服务器")
flag.IntVar(&workers, "workers", 3, "并发")
flag.IntVar(&rateMB, "rate", 0, "手动限速 MB/s")
flag.IntVar(&dayRateMB, "day-rate", 20, "白天限速 MB/s")
flag.IntVar(&nightRateMB, "night-rate", 80, "夜晚限速 MB/s")
flag.IntVar(&reportMinutes, "report-minutes", 360, "邮件统计分钟")
```

---

## 📁 `-dirs`

**类型：** string
**说明：** 扫描目录路径，支持多个目录，用英文逗号分隔
**功能：**

* 程序会递归扫描目录下所有文件
* 将文件加入上传队列

**示例：**

```bash
-dirs="/data/a,/data/b,/mnt/share"
```

---

## 🌐 `-server`

**类型：** string
**默认值：** `http://127.0.0.1:5244`
**说明：** 上传服务器地址
**功能：**

* 所有文件通过 HTTP POST 上传到该地址

**示例：**

```bash
-server="http://192.168.1.10:8080/upload"
```

---

## ⚙️ `-workers`

**类型：** int
**默认值：** `3`
**说明：** 并发 worker 数量
**作用：**

* 控制同时上传的并发线程数量
* 每个 worker 是一个 goroutine

**示例：**

```bash
-workers=5
```

---

## 🚦 `-rate`

**类型：** int
**单位：** MB/s
**默认值：** `0`
**说明：** 手动固定限速
**规则：**

* `0` 表示关闭固定限速
* 若设置为 >0，则优先生效

**示例：**

```bash
-rate=5   # 每个 worker 最大 5MB/s
```

---

## 🌞 `-day-rate`

**类型：** int
**单位：** MB/s
**默认值：** `20`
**说明：** 白天限速
**逻辑：**

* 在白天时间段使用该速率限制

---

## 🌙 `-night-rate`

**类型：** int
**单位：** MB/s
**默认值：** `80`
**说明：** 夜间限速
**逻辑：**

* 在夜间时间段使用该速率限制

---

## 📊 `-report-minutes`

**类型：** int
**单位：** 分钟
**默认值：** `360`
**说明：** 邮件统计发送周期
**功能：**

* 每 N 分钟发送一次上传统计邮件报告
* 邮件内容为 HTML 格式
* 汇总上传文件数量、大小、状态等信息

**示例：**

```bash
-report-minutes=60   # 每 60 分钟发送一次报告
```

---

# 📧 邮件系统配置（来自代码硬编码）

```go
mailFrom     = ""   // 发件人邮箱
mailAuthCode = ""    // SMTP授权码
mailTo       = ""   // 收件人邮箱
```

### 含义说明：

| 变量             | 含义            |
| -------------- | ------------- |
| `mailFrom`     | 邮件发送人邮箱地址     |
| `mailAuthCode` | QQ邮箱 SMTP 授权码 |
| `mailTo`       | 邮件接收人邮箱       |

### SMTP 配置（代码中写死）：

```go
smtp.qq.com:587
```

### 发送方式：

```go
smtp.SendMail("smtp.qq.com:587", auth, mailFrom, []string{mailTo}, msg)
```

---

# 📬 邮件内容类型

代码中实现的是：

* HTML 格式邮件
* 主题示例：`📦 上传成功报告`
* 定时发送
* 统计型邮件（汇总上传信息）

---

# 📄 Hash 去重机制

```go
hashFile = "uploaded_hash.db"
```

说明：

* 所有已上传文件的 SHA256 会记录到该文件
* 下次扫描时如果 hash 已存在 → 跳过上传
* 防止重复上传

---

# 📊 运行状态监控输出

定时打印（每 5 秒）：

```go
log.Printf("[QUEUE][STATUS] 运行:%s 等待:%d 工作中:%d 并发:%d",
	time.Since(start).Truncate(time.Second),
	atomic.LoadInt64(&queueCount),
	atomic.LoadInt64(&activeWorker),
	workers,
)
```

含义：

* 运行时间
* 队列等待数量
* 当前工作 worker 数
* 并发配置值

---

# 🧠 运行逻辑说明（真实结构）

```
目录扫描
   ↓
任务队列(channel)
   ↓
worker 并发池
   ↓
HTTP 上传
   ↓
计算 hash
   ↓
记录 uploaded_hash.db
   ↓
统计数据
   ↓
定时生成 HTML 报告
   ↓
SMTP 邮件发送
```

---

# ⚠️ 代码中已存在但需注意的内容

```go
username = "admin"
password = "LilKmxNF"
otpCode  = "123456"
```

说明：
当前代码中存在**硬编码账号/密码/OTP**变量，但未看到业务逻辑使用，属于**预留字段**或**调试遗留变量**。

---

# 🧪 最小运行示例

```bash
./uploader -dirs="/tmp"
```

---

# 📌 技术定位

这是一个：

* 目录扫描程序
* 并发上传工具
* HTTP 上传客户端
* 文件自动上传器
* 定时邮件统计程序

不是 Web 服务，不是管理平台，不是 Dashboard，不是 API Server。

---

# 📜 License

无 License 声明（当前代码未包含 License 文件）

---

# 👤 说明

本 README **完全基于真实代码生成**：

* 参数 = `flag` 定义
* 邮件系统 = smtp 实现
* Hash = sha256 实现
* 统计 = atomic 计数器
* 并发 = goroutine + channel
* 构建 = build.sh
* 重启 = restart.sh

**无虚构模块，无脑补系统，无宣传架构**。

---
