package main

import (
	"flag"
	"fmt"
	"net/mail"
	"os"
	"time"

	"go.uber.org/zap"
)

const (
	MaxHeaderLength = 1024
	MaxBodyLength   = 1024 * 512
	PidFileName     = "/tmp/gomail-api.pid"
	SMTPTimeout     = 30 * time.Second
	RetryInterval   = 3 * time.Second
	MaxSendRetries  = 5
	SendTimeout     = 60 * time.Second
	ShutdownTimeout = 30 * time.Second
)

type EmailTask struct {
	TargetGroup    string
	RecipientAddrs []*mail.Address
	Subject        string
	Content        string
	MessageID      string
	CreatedAt      time.Time
}

var Version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	checkMode := flag.Bool("check", false, "验证配置并测试连接")
	reloadMode := flag.Bool("reload", false, "向运行中的服务发送重载信号")
	versionMode := flag.Bool("version", false, "查看版本")
	flag.Parse()

	if *versionMode {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *reloadMode {
		if err := performReload(); err != nil {
			fmt.Fprintf(os.Stderr, "重载失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("重载信号已发送。")
		os.Exit(0)
	}

	cfg, err := loadAndValidateConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}

	if *checkMode {
		if err := runDeepCheck(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[FAIL] %v\n", err)
			os.Exit(1)
		}
		fmt.Println("[PASS] SMTP connection successful")
		return
	}

	app := newApp(cfg, *configPath)
	if err := app.run(); err != nil {
		app.logger.Fatal("server exited with error", zap.Error(err))
	}
}
