# DECISIONS

1. **Cookie 手动填，不做扫码登录（v1）**：BilibiliSettings 三个字段（sessdata/bili_jct/dedeuserid）早已预留；扫码登录工作量大，列为后续增强。
2. **自研 Go 客户端**（用户拍板）：新包 internal/bilibili，仿 biliup-rs 协议：preupload(r=upos,os=upos,upcdn=bda2) → upos init/chunk/complete（X-Upos-Auth）→ add/v3 提交；cover/up 表单 base64。
3. **全自动但带限速**：默认最小间隔 10 分钟、每日上限 20 条、失败退避重试 3 次（风控 code 601 等同样走退避）。
4. **默认分区 tid=129（舞蹈）**：项目实测场景是跳舞直播；分区/标签/模板全可配。
5. **投稿元数据持久化到 highlight_status.json**（扩展字段，旧数据兼容），队列单独落 dataDir/bili_publish.json。
6. **在裁切成功点 enqueue，不回头扫历史**：MVP 不做「历史高光补投」；队列 ID = 产物绝对路径，天然去重。
7. **高光 mp4 照常走网盘上传管线**，投稿成功不删源（生命周期不变）。
8. **worker 挂 App 生命周期**：Run() 里 go publishLoop()，与 highlightLoop 同级；不引入额外依赖。
