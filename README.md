# gomail-api

Go SMTP 中继 API，支持有界邮件队列、工作池、主备 SMTP、连接复用、失败重试和配置热重载。

## 运行

配置示例见 `config-example.json`，生产配置默认为 `/app/config.json`。

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
  "rate_limit_interval": 10,
  "queue_capacity": 2000,
  "worker_count": 5
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

环境变量在 JSON 解码后展开，变量值中的引号和反斜杠不会破坏配置。
支持 `$VAR` 和 `${VAR}`，未定义的变量会导致配置检查失败。
变量值不会再次展开；包含 `$` 的字面密码应通过环境变量提供。
配置仅接受一个 JSON 对象，未知字段、非法地址和越界参数会被拒绝。

| 参数 | 默认值 | 范围/含义 |
| --- | --- | --- |
| `http_port` | 8080 | 1–65535；0 使用默认值 |
| `queue_capacity` | 1000 | 1–100000；0 使用默认值；按内存预算设置 |
| `worker_count` | CPU 数量 × 2，上限 1024 | 1–1024；0 使用默认值 |
| `rate_limit_interval` | 0 | 0–86400 秒；每个 worker 的任务启动间隔，0 不限制 |
| `backup_smtp` | 无 | 主 SMTP 失败时使用备用服务器 |

多个 worker 可以并行发送，因此 `rate_limit_interval` 不是全局发送限速。
重试采用独立的 3 秒间隔。SMTP 服务器的连接数和发送配额应与 worker 数量匹配。
同组重复的实际收件地址会去重，已有显示名保留；无显示名时使用邮箱地址，避免部分客户端直接显示开头的 `<`。

## 接口

- `POST /`（兼容其他 POST 路径）：表单参数 `target_group`、`subject`、`content` 必填。
  返回 `200 queued` 表示邮件已进入内存队列，响应头 `X-Message-ID` 可关联投递日志。
- `GET /healthz`：进程存活检查。
- `GET /readyz`：配置已加载且未进入退出流程时返回 200；不检测外部 SMTP 的可达性。
- `GET /status`：运行时长、队列、进程和投递统计。

请求体最大 512 KiB（包括表单编码），主题和组名最长 1024 字节。
主题包含控制字符或只有空白时返回 400；不存在的收件组返回 404；队列满或正在退出返回 503。
HTTP 请求头最大约 32 KiB，设有请求头、读取、写入和空闲连接超时。

`/status` 的 `delivery` 字段包含 `accepted`、`sent`、`failed`、`retries`、`discarded`、`in_flight`。
计数自进程启动累计：`sent` 表示 SMTP 已确认接收，`failed` 表示任务最终失败，
`discarded` 表示退出超时后未处理的排队任务，`in_flight` 包括限速/重试等待中的任务。
运行中的统计为并发快照，不是事务快照。

## 投递与退出

发送失败后等待 **3 秒**，最多重试 **5 次**，含首次共 6 次尝试。
每次尝试先使用主 SMTP，失败后尝试备用 SMTP（若配置且本次超时尚未耗尽）。
每次尝试共用独立的 60 秒上限；单次拨号、握手和连接 I/O 默认 30 秒超时。
握手、认证、探测和正文发送都响应取消，失败连接及时关闭。

SMTP 连接复用前刷新超时并探测，失效连接自动重建。
同一任务重试或切换 SMTP 时保留 `Message-ID` 和邮件日期，便于追踪。
SMTP 已确认收信后的 `RSET` 失败只清理连接，不再次投递。
重试日志记录次数和间隔，最终记录 `send success` 或 `send failed`，通过 `message_id` 关联。
日志不采样，便于统计每一次投递结果。

收到 SIGTERM/SIGINT 后拒绝新任务、关闭 HTTP 监听并处理已入队任务。
HTTP 关闭和队列排空共用 **30 秒**期限；超时则取消正在进行的发送与等待，记录未处理队列数量并以错误退出。
容器停止期限应大于 30 秒，例如示例中的 40 秒。

## 配置重载与命令

```text
gomail-api
  -check          验证配置并测试主备 SMTP 连接，不发送邮件
  -config PATH    指定配置路径，默认 config.json
  -reload         向运行实例发送 SIGHUP
  -version        输出版本
```

`-check` 的主备连接检查共用 10 秒超时。
重载 SMTP、收件组和发送间隔时整体校验后原子替换；失败继续使用旧配置。
`http_port`、`queue_capacity`、`worker_count` 变更需要重启，热重载会拒绝整份配置，避免部分生效。
已入队任务保留入队时的收件人，在任务开始时使用当前 SMTP 配置。

PID 文件 `/tmp/gomail-api.pid` 使用进程锁防止重复实例覆盖，退出后保留文件并释放锁。
`-reload` 会检查锁状态，拒绝根据已退出进程留下的 PID 文件发送信号。
同一文件系统命名空间中仅允许一个实例；多副本应使用独立容器。

## 生产边界

当前队列保存在内存中。`queued` 不代表持久化或最终送达：崩溃、SIGKILL、节点故障和退出超时都可能丢失未完成任务。
需要崩溃恢复、投递审计和死信重放时，应在前面接入持久消息队列，或另行实现可靠的持久化任务存储。
重试耗尽的任务当前只记录日志和失败计数，不保存到死信队列。

SMTP 在接收邮件后、返回确认前断线时无法确定是否已投递，重试仍可能重复。
固定 `Message-ID` 便于追踪，不能保证收件服务器去重；本服务不承诺恰好一次投递，也不保证邮件已进入收件箱。

API 无内置鉴权或全局请求限流。生产应通过网关实现认证、请求配额和访问限制，避免公网裸露。
使用有效 SMTP 证书并保持 `skip_verify: false`。465 使用隐式 TLS，587 强制 STARTTLS，其他端口保持机会式 STARTTLS 行为。
配置和凭据应以只读文件或环境变量注入；Docker 构建上下文排除本地 `config.json` 和 `.env`。
监控失败/重试计数、队列占用、进程重启和 SMTP 配额；容量须结合实际峰值、内存及 SMTP 吞吐压测。

## 开发验证

使用 `go.mod` 指定的 Go 版本，从包构建（拆分文件后不再使用 `go build main.go`）：

```bash
go test -race -timeout 60s ./...
go vet ./...
go build -trimpath -o /tmp/gomail-api .
```

测试使用本机模拟 SMTP，无需生产凭据，不发送真实邮件。
Go 最低版本为 1.27.2，`golang.org/x/text` 使用 v0.41.0，chi 使用 v5.3.0。
GitHub Actions 仅保留 Linux amd64/arm64 镜像构建和发布流程，不运行额外的 CI 检查或安全扫描。
