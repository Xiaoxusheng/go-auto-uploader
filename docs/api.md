# API 摘要

所有 `/api/*` 与 `/ws/*` 需登录（除 login / pubkey / exchange）。

可选载荷加密：`EnableEncryption` 时 body 为 `{"encrypted":"..."}`。

## 认证

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/auth/login` | `{username,password}` → token |
| POST | `/api/v1/auth/logout` | 注销 |
| GET | `/api/v1/sec/pubkey` | RSA 公钥 |
| POST | `/api/v1/sec/exchange` | 交换 AES Session |

Header：`Authorization: Bearer <token>` 或 `?token=`

## 控制与状态

| 方法 | 路径 |
|------|------|
| GET | `/api/v1/status` |
| GET | `/api/v1/tasks/live` |
| GET | `/api/v1/tasks/history` |
| GET | `/api/v1/tasks/queue` |
| POST | `/api/v1/control/start\|pause\|stop\|relogin\|rescan` |
| POST | `/api/v1/control/clear-fail-queue\|retry-fail-queue\|clear-success-queue` |
| GET | `/api/v1/dirs/status` |
| GET/PUT | `/api/v1/config` |

## 录制

| 方法 | 路径 |
|------|------|
| GET | `/api/v1/recorder/status` |
| GET | `/api/v1/recorder/control?action=start\|stop\|restart` |
| GET | `/api/v1/recorder/logs` |
| GET/POST | `/api/v1/builtin_recorder/*` |
| GET | `/ws/live` |

## 统一响应（加密关闭时）

```json
{ "code": 0, "message": "", "data": {} }
```

加密开启时 data 被包进 `{"encrypted":"..."}`。

## WebSocket

`/ws/live?token=...`

消息：`{ "type": "...", "payload": {} }`

常见 type：`systemStatus` `queueStatus` `uploadProgress` `taskDone` `statsTrend` `builtinTasks` `systemAlert`
