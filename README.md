# gomail-api

Go SMTP 中继 API：有界内存队列、工作池、主备 SMTP、连接复用、指数退避重试。

面向内网部署，网关负责访问控制；本服务接收邮件任务并通过 SMTP 发送。

## 运行

配置示例见 `config-example.json`，容器内默认路径为 `/app/config.json`。

```json
{
  "http_port": 8080,
  "primary_smtp": {
    "server": "smtp.example.com",
    "port": 465,
    "email": "alert@yourdomain.com",
    "password": "${SMTP_PASSWORD}",
    "skip_verify": false
  },
  "backup_smtp": null,
  "recipient_groups": {
    "it": ["admin@yourdomain.com"]
  },
  "rate_limit_interval": 1,
  "max_retries": 2,
  "send_timeout": 10,
  "queue_capacity": 1000,
  "worker_count": 4
}
```

```bash
docker run --name gomail-api --restart unless-stopped \
  --stop-timeout 40 \
  -p 127.0.0.1:8080:8080 \
  --env-file .env \
  -v "$PWD/config.json:/app/config.json:ro" \
  qvgz/gomail-api

curl --data-urlencode 'target_group=it' \
  --data-urlencode 'subject=测试邮件' \
  --data-urlencode 'content=测试内容' \
  http://127.0.0.1:8080/
```

| 参数 | 默认值 | 范围/含义 |
| --- | --- | --- |
| `http_port` | 8080 | 1–65535 |
| `queue_capacity` | 1000 | 1–100000；排队、发送中、等待重试的任务总数上限 |
| `worker_count` | 4 | 1–64；并发 SMTP 连接数（主、备各一条/worker） |
| `rate_limit_interval` | 1 | 0–86400 秒；全局发送尝试最小间隔（含重试），0 不限制 |
| `max_retries` | 2 | 0–10；首次失败后的最大重试次数，0 不重试 |
| `send_timeout` | 10 | 1–300 秒；单个 SMTP 服务器单次投递超时，含拨号、握手、认证、发送 |
| `backup_smtp` | 无 | 备用服务器 |

数值参数缺省时使用默认值，显式给出的值按字面生效。配置只接受单个 JSON 对象，未知字段、非法地址、越界参数会被拒绝。
字符串中的 `$VAR`、`${VAR}` 在 JSON 解码后展开，未定义变量视为错误；含 `$` 的字面密码应通过环境变量提供。
修改配置需重启进程。收件组内重复地址会去重；无显示名的地址以邮箱作为显示名。

## 接口

- `POST /`：表单（urlencoded 或 multipart）字段 `target_group`、`subject`、`content` 必填。
  返回 `200 queued`，响应头 `X-Message-ID` 可关联日志。
  请求体超限或编码非法、缺参、主题含控制字符或超长返回 400；组不存在 404；队列满或正在退出 503。
- `GET /healthz`：进程存活。
- `GET /readyz`：未进入退出流程时返回 200；不检测 SMTP 可达性。
- `GET /status`：队列（`pending`、`ready`、`retry_waiting`、`in_flight`、`capacity`）与累计投递计数（`accepted`、`sent`、`failed`、`retries`、`discarded`）。

请求体最大 512 KiB，主题和组名最长 1024 字节。

## 投递

- 每次尝试先主后备，各有 `send_timeout` 预算；默认配置下单次尝试最长 20 秒，小于 30 秒退出期限。
- 主 SMTP 失败后 60 秒内所有 worker 优先使用备用 SMTP，仍会回退尝试主 SMTP；主 SMTP 成功即解除。
- 失败后间隔 `rate_limit_interval` 重试，最多 `max_retries` 次，仍未成功记为 `failed`。
  等待重试的任务不占用 worker，但占用队列容量；队列满时返回 503。
- 连接复用：复用前 5 秒 NOOP 探测，失效即重建；worker 空闲 60 秒主动断开。
- 重试和切换服务器时 `Message-ID` 与 `Date` 不变。DATA 已获确认后的清理失败不会重投。
- 日志为 JSON（stdout），记录 `mail queued`、`send failed, retrying`、`send success`、`send failed`，以 `message_id` 关联，不采样。

## 退出

收到 SIGTERM/SIGINT 后拒绝新任务、关闭 HTTP 监听，继续投递（含重试）直到队列清空。
HTTP 关闭与排空共用 **30 秒**期限；超时则取消发送，未完成任务计入 `discarded` 并以非零状态退出。
容器停止期限应大于 30 秒（示例为 40 秒）。

## 可靠性边界

队列在内存中：进程崩溃、SIGKILL、OOM、退出超时会丢失未完成任务。需要持久化与死信重放时，应在前面接入持久消息队列。
SMTP 接收后、确认前断线无法判断是否已投递，重试可能产生重复邮件；本服务优先不丢件，不保证恰好一次。

465 使用隐式 TLS，587 强制 STARTTLS，其他端口机会式 STARTTLS。
建议监控 `failed`、`retries`、`queue.pending`、进程重启和 SMTP 配额。

## 命令

```text
gomail-api
  -check GROUP    通过主、备 SMTP（若配置）分别向收件组 GROUP 实际发送一封测试邮件
  -config PATH    配置路径，默认 config.json
  -version        输出版本
```

`-check` 主备各自独立发送、互不切换，超时为 `send_timeout`；任一失败则退出码为 1，并输出错误与完整 SMTP 协议日志（不含认证数据）。

## 开发验证

```bash
go test -race -timeout 120s ./...
go vet ./...
go build -trimpath -o /tmp/gomail-api .
```

测试使用本机模拟 SMTP 与虚拟时间（`testing/synctest`），不发送真实邮件。Go 版本以 `go.mod` 为准。
