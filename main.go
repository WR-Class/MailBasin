package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"icloud-privacy-mail-v2/internal/auth"
	"icloud-privacy-mail-v2/internal/buildinfo"
	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/httpapi"
	"icloud-privacy-mail-v2/internal/store"
)

type launchOptions struct {
	ConfigPath string
	Host       string
	Port       int
	DataPath   string
}

func main() {
	options := launchOptions{}
	showMenu := false
	resetPassword := false
	flag.StringVar(&options.ConfigPath, "config", "config.json", "配置文件路径")
	flag.StringVar(&options.Host, "host", "", "覆盖监听地址")
	flag.IntVar(&options.Port, "port", 0, "覆盖监听端口")
	flag.StringVar(&options.DataPath, "data", "", "覆盖 SQLite 数据库路径")
	flag.BoolVar(&showMenu, "menu", false, "显示交互式启动菜单")
	flag.BoolVar(&resetPassword, "reset-password", false, "离线重置管理员密码后退出")
	flag.Parse()

	// 密码重置是离线运维动作，做完就退出，不进入服务启动流程。
	if resetPassword {
		if err := runResetPassword(options); err != nil {
			fmt.Fprintln(os.Stderr, "重置失败："+err.Error())
			os.Exit(1)
		}
		return
	}

	if (len(os.Args) == 1 && stdinIsTerminal()) || showMenu {
		if !runMenu(&options) {
			return
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := runServer(options, logger); err != nil {
		logger.Error("服务启动失败", "错误", err)
		os.Exit(1)
	}
}

func runServer(options launchOptions, logger *slog.Logger) error {
	cfg, err := effectiveConfig(options)
	if err != nil {
		return fmt.Errorf("加载配置失败：%w", err)
	}
	state, err := store.Open(cfg.DataPath)
	if err != nil {
		return fmt.Errorf("打开数据存储失败：%w", err)
	}
	defer state.Close()

	handler := httpapi.New(cfg, state, logger)
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 全量邮件同步可能需要遍历多个 Apple 主号和完整收件箱，给长任务保留足够的响应时间。
		WriteTimeout:   30 * time.Minute,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handler.StartBackground(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	current := buildinfo.Current()
	logger.Info("MailBasin 已启动", "地址", "http://"+server.Addr, "版本", current.Version, "提交", current.Commit, "配置", options.ConfigPath, "数据", cfg.DataPath)
	err = server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// runResetPassword 离线替换管理员密码。
//
// 密码是单向散列，找不回来只能替换。这里复用 auth 包自己的散列与校验规则，
// 并且就地更新原记录、同时作废全部登录态——删掉重建会把 owner_id 引用变成悬空值。
func runResetPassword(options launchOptions) error {
	cfg, err := effectiveConfig(options)
	if err != nil {
		return fmt.Errorf("加载配置失败：%w", err)
	}
	state, err := store.Open(cfg.DataPath)
	if err != nil {
		return fmt.Errorf("打开数据存储失败：%w", err)
	}
	defer state.Close()

	admin, ok := state.Admin()
	if !ok {
		return fmt.Errorf("还没有管理员账号，请先启动服务并在浏览器完成首次设置")
	}
	fmt.Printf("数据库：%s\n", cfg.DataPath)
	fmt.Printf("管理员：%s\n", admin.Username)
	fmt.Println("将重置该账号的密码，并作废所有已登录的会话。")
	fmt.Println()

	password, err := promptNewPassword()
	if err != nil {
		return err
	}
	if err := auth.ValidateCredentials(admin.Username, password); err != nil {
		return err
	}
	encoded, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("生成密码散列失败：%w", err)
	}
	if !auth.VerifyPassword(password, encoded) {
		return fmt.Errorf("密码散列自检未通过，已放弃写入")
	}

	updated, sessions, err := state.ResetAdminPassword(encoded)
	if err != nil {
		return err
	}
	// 用新散列回读一次，确认落库的确实是刚才那个密码。
	if stored, ok := state.Admin(); !ok || stored.ID != updated.ID || !auth.VerifyPassword(password, stored.PasswordHash) {
		return fmt.Errorf("写入后校验失败，请重试")
	}
	fmt.Println()
	fmt.Printf("已重置 %s 的密码，作废登录态 %d 个。\n", updated.Username, sessions)
	fmt.Println("请用新密码重新登录；若服务正在运行，无需重启即可生效。")
	return nil
}

// promptNewPassword 读取两次新密码并要求一致。
// 支持管道输入，便于脚本化且不会进入 shell 历史；不接受命令行参数传密码，
// 那会把密码留在进程列表里。
func promptNewPassword() (string, error) {
	first, err := readSecret("请输入新密码（至少 8 位）：")
	if err != nil {
		return "", err
	}
	if len([]rune(first)) < 8 {
		return "", fmt.Errorf("密码至少 8 位")
	}
	second, err := readSecret("请再次输入新密码：")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("两次输入的密码不一致")
	}
	return first, nil
}

func readSecret(prompt string) (string, error) {
	if !stdinIsTerminal() {
		// 管道输入：不回显，也不进 shell 历史。
		value, err := readLinePlain(os.Stdin)
		if err != nil {
			return "", err
		}
		return value, nil
	}
	fmt.Print(prompt)
	value, echoDisabled, err := readPasswordNoEcho(os.Stdin)
	fmt.Println()
	if err != nil {
		return "", err
	}
	if !echoDisabled {
		fmt.Println("提示：当前平台无法关闭输入回显，密码在输入过程中可见。")
	}
	return value, nil
}

// stdinReader 必须全局复用：bufio.Reader 一次会读入多行，
// 每次调用都新建实例会让第二次读取直接拿到 EOF（管道输入时尤其明显）。
var stdinReader = bufio.NewReader(os.Stdin)

// readLinePlain 读一行并去掉行尾换行，供各平台的密码读取共用。
func readLinePlain(_ *os.File) (string, error) {
	line, err := stdinReader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("读取输入失败：%w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func effectiveConfig(options launchOptions) (config.Config, error) {
	cfg, err := config.Load(strings.TrimSpace(options.ConfigPath))
	if err != nil {
		return config.Config{}, err
	}
	if strings.TrimSpace(options.Host) != "" {
		cfg.Host = strings.TrimSpace(options.Host)
	}
	if options.Port > 0 {
		cfg.Port = options.Port
	}
	if strings.TrimSpace(options.DataPath) != "" {
		cfg.DataPath = strings.TrimSpace(options.DataPath)
	}
	return cfg, nil
}

func runMenu(options *launchOptions) bool {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println()
		fmt.Println("========================================")
		fmt.Println("  MailBasin 启动菜单")
		fmt.Println("========================================")
		fmt.Println("  1. 使用当前配置启动服务")
		fmt.Println("  2. 自定义地址、端口和数据文件后启动")
		fmt.Println("  3. 查看当前生效配置")
		fmt.Println("  4. 显示常用命令")
		fmt.Println("  5. 重置管理员密码")
		fmt.Println("  0. 退出")
		fmt.Print("请选择 [1]：")
		choice := readMenuLine(reader)
		if choice == "" {
			choice = "1"
		}
		switch choice {
		case "1":
			return true
		case "2":
			customizeLaunchOptions(reader, options)
			return true
		case "3":
			printEffectiveConfig(*options)
		case "4":
			printCommonCommands()
		case "5":
			if err := runResetPassword(*options); err != nil {
				fmt.Println("重置失败：", err)
			}
		case "0", "q", "quit", "exit":
			fmt.Println("已退出。")
			return false
		default:
			fmt.Println("选项不存在，请重新选择。")
		}
	}
}

func customizeLaunchOptions(reader *bufio.Reader, options *launchOptions) {
	fmt.Printf("配置文件 [%s]：", options.ConfigPath)
	if value := readMenuLine(reader); value != "" {
		options.ConfigPath = value
	}
	fmt.Printf("监听地址 [%s]：", displayDefault(options.Host, "使用配置文件"))
	if value := readMenuLine(reader); value != "" {
		options.Host = value
	}
	fmt.Printf("监听端口 [%s]：", displayPort(options.Port))
	if value := readMenuLine(reader); value != "" {
		port, err := strconv.Atoi(value)
		if err == nil && port > 0 && port <= 65535 {
			options.Port = port
		} else {
			fmt.Println("端口格式不正确，将继续使用配置文件中的端口。")
		}
	}
	fmt.Printf("数据库 [%s]：", displayDefault(options.DataPath, "使用配置文件"))
	if value := readMenuLine(reader); value != "" {
		options.DataPath = value
	}
}

func printEffectiveConfig(options launchOptions) {
	cfg, err := effectiveConfig(options)
	if err != nil {
		fmt.Println("读取配置失败：", err)
		return
	}
	fmt.Println()
	fmt.Println("当前生效配置：")
	fmt.Println("  配置文件：", options.ConfigPath)
	fmt.Printf("  服务地址：http://%s:%d\n", cfg.Host, cfg.Port)
	fmt.Println("  数据库：", cfg.DataPath)
	fmt.Println("  邮件监听：", enabledText(cfg.MailWatcherEnabled))
	fmt.Println("  Apple 保活：", enabledText(cfg.AppleAccountKeepAliveEnabled))
}

func printCommonCommands() {
	fmt.Println()
	fmt.Println("常用命令：")
	fmt.Println("  直接启动：go run main.go")
	fmt.Println("  指定配置：go run main.go -config config.json")
	fmt.Println("  显示菜单：go run main.go -menu")
	fmt.Println("  重置密码：go run main.go -reset-password")
	fmt.Println("  开发模式：./scripts/dev.sh")
	fmt.Println("  完整构建：./scripts/build.sh")
}

func readMenuLine(reader *bufio.Reader) string {
	value, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(value) == "" {
		return "0"
	}
	return strings.TrimSpace(value)
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func displayDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func displayPort(port int) string {
	if port <= 0 {
		return "使用配置文件"
	}
	return strconv.Itoa(port)
}

func enabledText(enabled bool) string {
	if enabled {
		return "启用"
	}
	return "停用"
}
