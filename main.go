package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	MaxHeaderLength = 1024
	MaxBodyLength   = 512 * 1024

	DefaultSendTimeout = 10 * time.Second // 单个 SMTP 服务器一次投递的默认上限，含拨号、握手、认证、发送
	SMTPProbeTimeout   = 5 * time.Second  // 复用连接前 NOOP 探测的上限
	SMTPIdleTimeout    = 60 * time.Second // worker 空闲超过该时长主动关闭连接
	PrimaryCooldown    = 60 * time.Second // 主 SMTP 失败后优先使用备用的时长

	ShutdownTimeout = 30 * time.Second
	CheckTimeout    = 10 * time.Second
)

var Version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	checkMode := flag.Bool("check", false, "验证配置并测试 SMTP 连接，不发送邮件")
	versionMode := flag.Bool("version", false, "输出版本")
	flag.Parse()

	if *versionMode {
		fmt.Println(Version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}

	if *checkMode {
		if err := checkSMTP(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[FAIL] %v\n", err)
			os.Exit(1)
		}
		fmt.Println("[PASS] SMTP connection successful")
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
