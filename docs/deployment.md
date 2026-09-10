# 部署说明

## Linux systemd

示例单元（与线上一致，路径按实际修改）：

```ini
[Unit]
Description=File Auto Uploader Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/home/upload/uploader \
  -dirs=/path/to/live1,/path/to/live2 \
  -workers=5 \
  -server=http://127.0.0.1:5244 \
  -web-port=8888
WorkingDirectory=/home/upload
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

```bash
# 交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-w -s" -o uploader .

systemctl stop uploader
cp uploader /home/upload/uploader
systemctl start uploader
journalctl -u uploader -f
```

工作目录需有：`config.json`、可选 `font.ttf`（水印）、ffmpeg 在 PATH。

## Windows

```powershell
go build -o uploader.exe .
.\uploader.exe -dirs=D:\live -web-port=8888
```

可注册为服务（nssm / WinSW），工作目录指向 exe 所在目录。

## 依赖

| 组件 | 用途 | 可选 |
|------|------|------|
| ffmpeg | 录制 / 截图 / TS→MP4 | 录制功能必填 |
| docker | 外置 DouyinLiveRecorder | 仅外置引擎 |
| SMTP/微信/TG/QQ | 通知 | 可关 |

## 升级

1. 备份二进制与 `config.json`
2. 替换二进制
3. `systemctl restart uploader`
4. 确认 `journalctl` 无 Fatal，Web 端口可访问

配置与数据文件格式保持向后兼容；升级勿删 `uploaded_hash.db`。
