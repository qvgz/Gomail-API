# gomail-api

一个用 Go 语言编写的高性能、安全可靠的 SMTP 中继 API。

特性：工作池、多 SMTP 故障转移、优雅关闭和符合 RFC 规范的头部生成。

# 快速开始

配置文件 /app/config.json

``` json
{
  "http_port": 8080,
  "primary_smtp": {
    "server": "smtp.com",
    "port": 465,
    "email": "alert@yourdomain.com",
    "password": "your_smtp_password",
    "skip_verify": false
  },
  "backup_smtp": null,
  "recipient_groups": {
    "it": [
      "admin@yourdomain.com",
    ]
  },
  "rate_limit_interval": 10, // 发信频率限制（秒），每 10s 只发一封
  "queue_capacity": 2000, // 邮件队列缓冲大小
  "worker_count": 5
}
```

``` bash
docker run --rm --name gomail-api \
  -p 8080:8080 \
  -v ./gomail-api.json:/app/config.json \
  qvgz/gomail-api

# 发送邮件示范 (接口仅支持 POST 方法)
# target_group subject content 是必须参数
curl -d "target_group=it&subject=测试邮件&content=测试内容" http://127.0.0.1:8080/
```

## 命令列表

``` bash
gomail-api
  -check
        仅验证配置并执行连接测试，不启动服务
  -config string
        配置文件路径 (default "config.json")
  -reload
        向运行中的服务发送 SIGHUP 信号以重载配置
  -version
        输出版本信息并退出
```

## 接口

POST / : 发送邮件
GET /healthz : 存活探针
GET /status : 服务内部状态监控

# 注意事项

程序没有鉴权与频率限制，建议通过 Nginx 等网关实现。
