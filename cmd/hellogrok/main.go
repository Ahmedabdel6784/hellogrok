package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/autostart"
	"github.com/hellowind777/hellogrok/internal/cfgpatch"
	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/console"
	"github.com/hellowind777/hellogrok/internal/logui"
	"github.com/hellowind777/hellogrok/internal/logview"
	"github.com/hellowind777/hellogrok/internal/openpath"
	"github.com/hellowind777/hellogrok/internal/prefs"
	"github.com/hellowind777/hellogrok/internal/proxy"
)

func main() {
	dataDir := appinfo.DataDir()
	logPath := appinfo.LogPath()

	if len(os.Args) > 1 && os.Args[1] == "logview" {
		_ = os.MkdirAll(dataDir, 0o700)
		_ = console.Show("hellogrok 日志")
		f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND, 0o600)
		if f != nil {
			_ = f.Close()
		}
		if err := logview.Run(logPath); err != nil {
			fmt.Fprintln(os.Stderr, "打开日志失败:", err)
			fmt.Println("按 Enter 退出...")
			_, _ = fmt.Scanln()
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] != "start" {
		runUtilityCommand(os.Args[1:], dataDir, logPath)
		return
	}
	if len(os.Args) == 1 && !hasDefaultUI {
		runDefault(nil, nil)
		return
	}

	cli := len(os.Args) > 1
	_ = os.MkdirAll(dataDir, 0o700)

	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		if cli {
			log.Fatal(err)
		}
		lf = nil
	}

	var logWriter io.Writer
	if cli {
		if lf != nil {
			logWriter = io.MultiWriter(os.Stderr, lf)
		} else {
			logWriter = os.Stderr
		}
	} else {
		if lf != nil {
			logWriter = lf
		} else {
			logWriter = io.Discard
		}
		console.Hide()
	}
	if lf != nil {
		defer lf.Close()
	}

	logger := log.New(logWriter, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("[hellogrok] ")

	app := &App{
		logger:  logger,
		logFile: lf,
		dataDir: dataDir,
		logPath: logPath,
		server:  proxy.New(logger),
	}

	logger.Printf("application ready (log will reset when proxy starts)")

	if cli {
		if err := runForeground(app, logger); err != nil {
			logger.Printf("foreground run failed: %v", err)
			os.Exit(1)
		}
		return
	}

	runDefaultWithSignals(app, logger)
}

func runDefaultWithSignals(app *App, logger *log.Logger) {
	sigc := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigc:
			logger.Printf("signal %s received; restoring config and stopping", sig)
			if err := app.Stop(); err != nil {
				logger.Printf("signal stop failed: %v", err)
			}
			requestDefaultExit()
		case <-done:
		}
	}()
	runDefault(app, logger)
	close(done)
	signal.Stop(sigc)
}

func runUtilityCommand(args []string, dataDir, logPath string) {
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Printf("%s %s\n", appinfo.Name, appinfo.Version)
	case "restore":
		if err := ensureFacadeIdle(net.JoinHostPort(cfgpatch.ProxyHost, cfgpatch.ProxyPort)); err != nil {
			fmt.Fprintln(os.Stderr, "restore config:", err)
			os.Exit(1)
		}
		n, err := cfgpatch.Restore(config.ConfigPath(), cfgpatch.StatePath(dataDir))
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore config:", err)
			os.Exit(1)
		}
		fmt.Printf("restored %d proxy-managed setting(s)\n", n)
	case "routes":
		models, err := config.LoadModels(config.ConfigPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "load config:", err)
			os.Exit(1)
		}
		routes, err := config.BuildRoutes(models)
		if err != nil {
			fmt.Fprintln(os.Stderr, "build routes:", err)
			os.Exit(1)
		}
		for _, route := range routes {
			fmt.Printf("channel=%s host=%s backend=%s model=%s backend_search=%t auth=%s\n",
				route.ChannelID, route.Host, route.APIBackend, route.WireModel, route.SupportsBackendSearch, routeAuthStatus(route))
		}
	case "log":
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "create data directory:", err)
			os.Exit(1)
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create log:", err)
			os.Exit(1)
		}
		_ = f.Close()
		fmt.Println(logPath)
		if err := openpath.Open(logPath); err != nil {
			fmt.Fprintln(os.Stderr, "open log:", err)
			os.Exit(1)
		}
	case "autostart":
		runAutostartCommand(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
	default:
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func ensureFacadeIdle(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("local facade %s is active; stop it before restoring", address)
	}
	return listener.Close()
}

func routeAuthStatus(route config.Route) string {
	if strings.TrimSpace(route.APIKey) != "" {
		return "channel-owned"
	}
	for name, value := range route.ExtraHeaders {
		if (strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Api-Key")) &&
			strings.TrimSpace(value) != "" {
			return "channel-owned"
		}
	}
	if route.DynamicAuth {
		return "auth-provider"
	}
	return "missing"
}

func runForeground(app *App, logger *log.Logger) error {
	if err := app.Start(); err != nil {
		return err
	}
	logger.Printf("running; status: %s", app.StatusDetail())

	// SIGINT/SIGTERM must restore base_url (no orphaned proxy URLs).
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	<-sigc
	signal.Stop(sigc)
	logger.Printf("signal received; restoring config and stopping")
	return app.Stop()
}

func runAutostartCommand(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hellogrok autostart [enable|disable|status]")
		os.Exit(2)
	}
	switch args[0] {
	case "enable":
		if err := setAutostart(true); err != nil {
			fmt.Fprintln(os.Stderr, "enable autostart:", err)
			os.Exit(1)
		}
		fmt.Println("autostart enabled")
	case "disable":
		if err := setAutostart(false); err != nil {
			fmt.Fprintln(os.Stderr, "disable autostart:", err)
			os.Exit(1)
		}
		fmt.Println("autostart disabled")
	case "status":
		if autostart.Enabled() {
			fmt.Println("enabled")
		} else {
			fmt.Println("disabled")
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: hellogrok autostart [enable|disable|status]")
		os.Exit(2)
	}
}

func setAutostart(enabled bool) error {
	if hasDefaultUI {
		return autostart.SetUI(enabled)
	}
	return autostart.Set(enabled)
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "hellogrok %s\n", appinfo.Version)
	fmt.Fprintln(w, "usage: hellogrok <command>")
	fmt.Fprintln(w, "  version               print the application version")
	fmt.Fprintln(w, "  start                 run proxy in foreground; Ctrl+C/SIGTERM restores config")
	fmt.Fprintln(w, "  restore               recover proxy-managed config after an unclean exit")
	fmt.Fprintln(w, "  routes                list configured upstream routes without credentials")
	fmt.Fprintln(w, "  autostart <action>    enable, disable, or inspect login autostart")
	fmt.Fprintln(w, "  log                   print and open the log file")
	fmt.Fprintln(w, "  logview               follow the log in the current terminal")
}

// App implements tray.Controller.
type App struct {
	logger  *log.Logger
	logFile *os.File
	dataDir string
	logPath string
	server  *proxy.Server

	mu         sync.Mutex
	running    bool
	lastError  string
	patchedIDs []string

	cfgMu  sync.Mutex
	prefMu sync.Mutex
}

func (a *App) resetSessionLog() {
	if a.logFile != nil {
		_ = a.logFile.Truncate(0)
		_, _ = a.logFile.Seek(0, 0)
	} else if a.logPath != "" {
		_ = os.WriteFile(a.logPath, nil, 0o600)
	}
	if a.logger != nil {
		a.logger.Printf("======== session start %s ========", time.Now().Format("2006-01-02 15:04:05"))
	}
}

func (a *App) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *App) StatusText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return "已停止"
	}
	return "运行中"
}

func (a *App) StatusDetail() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		if a.lastError != "" {
			return "已停止。上次错误: " + a.lastError
		}
		return "代理未运行。勾选「启动代理」后：\n" +
			"· 记忆代理启用状态；下次打开托盘时自动恢复\n" +
			"· 校验并临时补全全部显式自定义模型的代理必需字段\n" +
			"· 管理 base_url/api_base_url、api_backend 和有效 supports_backend_search\n" +
			"· true 使用当前渠道 hosted search；false/缺省使用 Build 客户端搜索模型\n" +
			"· 管理 [features].backend_tools 和 web_fetch 开关\n" +
			"· 已配置子代理但缺省 enabled 时临时启用；显式 false 保持不变\n" +
			"· 自动适配 Responses、Anthropic Messages 和 Chat Completions 搜索字段\n" +
			"· 不创建、不选择、不修改任何 [models].web_search 搜索模型\n" +
			"· 透传请求 + 响应补字段；渠道 api_key 防 OAuth 抢鉴权\n" +
			"· 写入后回读验证；启动失败或停止时精确恢复全部原值"
	}
	patched := append([]string(nil), a.patchedIDs...)
	sort.Strings(patched)
	list := "(无)"
	if len(patched) > 0 {
		list = strings.Join(patched, ", ")
	}
	return "运行中（逐渠道 Responses 外观层 + 搜索能力分流 + 响应补字段）。\n" +
		"· 本地: http://" + a.server.PathAddr + "/c/<渠道>/responses\n" +
		"· 上游保留原 base_url/api_base_url 路径前缀\n" +
		"· supports_backend_search=true: 当前渠道 hosted search\n" +
		"· supports_backend_search=false/缺省: 优先 [models].web_search，否则仅在有效 xAI 登录/API 凭据下使用官方默认搜索模型\n" +
		"· 抓取模式: Build 本地 web_fetch（不依赖独立搜索模型）\n" +
		"· 子代理: 缺省 enabled 已按 Build 预期临时修复，停止时精确恢复\n" +
		"· 协议: Responses web_search / Messages server tool / Chat 按模型适配搜索字段\n" +
		"· hellogrok 不创建、不选择、不修改搜索模型\n" +
		"· 全部自定义渠道预先进入代理，无首次切换竞态\n" +
		"· 必需配置已通过 TOML 回读校验\n" +
		"· 当前已改写: " + list + "\n" +
		"· 停止时恢复所有代理管理字段"
}

func (a *App) OpenMonitor() error {
	if a.logPath != "" {
		f, err := os.OpenFile(a.logPath, os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_ = f.Close()
		}
	}
	return logui.Open(a.logPath, func() (string, string) {
		return a.StatusText(), a.StatusDetail()
	})
}

func (a *App) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return nil
	}
	a.lastError = ""

	cfgPath := config.ConfigPath()
	stPath := cfgpatch.StatePath(a.dataDir)

	// Own the facade address before touching config. A second instance must not
	// mistake the active instance's rewrite state for an orphan and restore it.
	if err := a.server.ReservePath(); err != nil {
		return a.abortStart(fmt.Errorf("reserve local facade: %w", err))
	}
	a.resetSessionLog()

	// Orphan recovery: a previous unclean exit may leave proxy URLs in config.
	// Recovery must succeed before loading routes or applying new changes.
	a.cfgMu.Lock()
	n, err := cfgpatch.Restore(cfgPath, stPath)
	a.cfgMu.Unlock()
	if err != nil {
		return a.abortStart(fmt.Errorf("restore config before start: %w", err))
	}
	if n > 0 {
		a.logger.Printf("orphan restore: %d proxy-managed setting(s) recovered before start", n)
	}

	// Load models AFTER orphan restore for auth/routes.
	models, err := config.LoadModels(cfgPath)
	if err != nil {
		return a.abortStart(fmt.Errorf("load models: %w", err))
	}
	for _, model := range models {
		if cfgpatch.IsProxyURL(model.BaseURL) || cfgpatch.IsProxyURL(model.APIBaseURL) {
			return a.abortStart(fmt.Errorf("model %q still points to the local facade but no restorable origin is available; restore the original custom URL before starting", model.ID))
		}
	}
	routes, err := config.BuildRoutes(models)
	if err != nil {
		return a.abortStart(fmt.Errorf("build routes: %w", err))
	}
	if len(routes) == 0 {
		return a.abortStart(fmt.Errorf("no explicit custom model endpoints found"))
	}
	a.server.SetRoutes(routes)
	if err := a.server.ServePath(); err != nil {
		return a.abortStart(fmt.Errorf("start local facade: %w", err))
	}
	a.logger.Printf("channel facade on http://%s/c/<channel>/responses", a.server.PathAddr)

	// Rewrite every explicit endpoint before Grok can load a direct URL. Waiting
	// for session discovery races the first request after a model switch.
	a.cfgMu.Lock()
	targets := make([]cfgpatch.Target, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.BaseURL) == "" && strings.TrimSpace(model.APIBaseURL) == "" {
			continue
		}
		targets = append(targets, cfgpatch.Target{
			ID:                    model.ID,
			APIBaseURL:            strings.TrimSpace(model.APIBaseURL) != "",
			SupportsBackendSearch: model.SupportsBackendSearch,
		})
	}
	res, err := cfgpatch.ApplyTargets(cfgPath, stPath, targets)
	a.cfgMu.Unlock()
	if err != nil {
		// ApplyTargets rolls back failures after its state file is committed.
		// Retry restoration here as a lifecycle-level fallback.
		a.cfgMu.Lock()
		_, restoreErr := cfgpatch.Restore(cfgPath, stPath)
		a.cfgMu.Unlock()
		if restoreErr != nil {
			err = fmt.Errorf("%w; fallback config rollback failed: %v", err, restoreErr)
		}
		return a.abortStart(fmt.Errorf("rewrite config: %w", err))
	}
	a.patchedIDs = append([]string(nil), res.Targets...)
	sort.Strings(a.patchedIDs)
	a.logger.Printf("config rewrite all: base=%d api_base=%d api_backend=%d backend_search=%d backend_tools=%d web_fetch=%d subagents_enabled=%d targets=%v",
		res.BaseURLs, res.APIBaseURLs, res.APIBackends, res.BackendSearch, res.BackendTools, res.WebFetch, res.SubagentsEnabled, res.Targets)
	a.logger.Printf("config validation passed: backend_tools=true web_fetch=true backend_search=materialized subagent_defaults=repaired-if-needed responses_targets=%d", res.ValidatedTargets)
	for _, model := range models {
		if strings.TrimSpace(model.BaseURL) == "" {
			continue
		}
		backend := strings.TrimSpace(model.APIBackend)
		if backend == "" {
			backend = "chat_completions"
		}
		switch backend {
		case "responses":
			a.logger.Printf("channel facade: model=%s upstream=responses canonical Responses passthrough", model.ID)
		case "messages":
			a.logger.Printf("channel facade: model=%s upstream=messages bidirectional Responses conversion", model.ID)
		case "chat_completions":
			a.logger.Printf("channel facade: model=%s upstream=chat_completions bidirectional Responses conversion", model.ID)
		default:
			a.logger.Printf("search adapter unavailable: model=%s api_backend=%s", model.ID, backend)
		}
		if model.SupportsBackendSearch {
			a.logger.Printf("channel search: model=%s supports_backend_search=true mode=hosted-current-channel", model.ID)
		} else {
			a.logger.Printf("channel search: model=%s supports_backend_search=false mode=client-web_search configured-model-or-authenticated-official-default", model.ID)
		}
	}

	a.running = true
	a.logger.Printf("started path=%s mode=per-channel-responses-facade+auth-isolate", a.server.PathAddr)
	return nil
}

func (a *App) abortStart(err error) error {
	a.server.Stop()
	a.server = proxy.New(a.logger)
	a.lastError = err.Error()
	return err
}

func (a *App) Stop() error {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return nil
	}
	a.server.Stop()
	a.server = proxy.New(a.logger)

	stPath := cfgpatch.StatePath(a.dataDir)
	a.cfgMu.Lock()
	n, err := cfgpatch.Restore(config.ConfigPath(), stPath)
	a.cfgMu.Unlock()
	if err != nil {
		a.logger.Printf("config restore failed: %v", err)
	} else {
		a.logger.Printf("config restore: %d proxy-managed setting(s) restored", n)
	}

	a.running = false
	a.patchedIDs = nil
	a.logger.Printf("stopped")
	a.mu.Unlock()
	return err
}

func (a *App) IsAutostart() bool         { return autostart.Enabled() }
func (a *App) SetAutostart(v bool) error { return autostart.SetUI(v) }

func (a *App) ProxyEnabledOnLaunch() (bool, error) {
	a.prefMu.Lock()
	defer a.prefMu.Unlock()
	return prefs.ProxyEnabled(prefs.Path(a.dataDir))
}

func (a *App) SetProxyEnabledOnLaunch(enabled bool) error {
	a.prefMu.Lock()
	defer a.prefMu.Unlock()
	return prefs.SetProxyEnabled(prefs.Path(a.dataDir), enabled)
}
