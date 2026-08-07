//go:build windows || tray

package tray

import (
	"log"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"github.com/hellowind777/hellogrok/internal/dialog"
)

// Controller is implemented by the app main logic.
type Controller interface {
	IsRunning() bool
	Start() error
	Stop() error
	ProxyEnabledOnLaunch() (bool, error)
	SetProxyEnabledOnLaunch(bool) error
	IsAutostart() bool
	SetAutostart(bool) error
	StatusText() string
	StatusDetail() string
	OpenMonitor() error
}

type proxyOperation uint8

const (
	operationIdle proxyOperation = iota
	operationStarting
	operationStopping
)

func operationTitle(operation proxyOperation, dotPhase int) string {
	if dotPhase < 1 || dotPhase > 3 {
		dotPhase = 1
	}
	label := ""
	switch operation {
	case operationStarting:
		label = "启动中"
	case operationStopping:
		label = "停止中"
	default:
		return "启动代理"
	}
	return "启动代理 (" + label + strings.Repeat(".", dotPhase) + ")"
}

// Run blocks and shows the tray (no console UI).
func Run(c Controller, icon []byte, logger *log.Logger) {
	systray.Run(func() {
		onReady(c, icon, logger)
	}, func() {
		_ = c.Stop()
	})
}

func Quit() { systray.Quit() }

func onReady(c Controller, icon []byte, logger *log.Logger) {
	if len(icon) > 0 {
		systray.SetIcon(icon)
	}
	systray.SetTitle("hellogrok")
	systray.SetTooltip("hellogrok")

	mStart := systray.AddMenuItemCheckbox("启动代理", "启动/停止本地路由代理并记忆选择", false)
	mAuto := systray.AddMenuItemCheckbox("开机启动", "登录后启动 hellogrok，并按记忆状态启用代理", c.IsAutostart())
	systray.AddSeparator()
	mMon := systray.AddMenuItem("状态与日志", "打开状态与实时日志窗口")
	mMon.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止代理并退出")

	type opResult struct {
		err      error
		remember bool
	}
	startDone := make(chan opResult, 1)
	stopDone := make(chan opResult, 1)
	tick := time.NewTicker(400 * time.Millisecond)
	busy := false
	operation := operationIdle
	dotPhase := 0

	setRunningUI := func(running bool) {
		busy = false
		operation = operationIdle
		mStart.SetTitle("启动代理")
		mStart.Enable()
		if running {
			mStart.Check()
			mMon.Enable()
			systray.SetTooltip("hellogrok · " + c.StatusText())
		} else {
			mStart.Uncheck()
			mMon.Disable()
			systray.SetTooltip("hellogrok · 已停止")
		}
	}

	setRunningUI(c.IsRunning())
	remembered, rememberErr := c.ProxyEnabledOnLaunch()
	if rememberErr != nil {
		logger.Printf("load remembered proxy state: %v", rememberErr)
		dialog.Info("hellogrok", "读取代理启用状态失败，代理不会自动启动:\n"+rememberErr.Error())
	}
	if rememberErr == nil && remembered && !c.IsRunning() {
		logger.Printf("remembered proxy state is enabled; starting automatically")
		mStart.Check()
		mStart.SetTitle(operationTitle(operationStarting, 1))
		mStart.Disable()
		busy = true
		operation = operationStarting
		dotPhase = 1
		go func() { startDone <- opResult{err: c.Start()} }()
	}

	go func() {
		for {
			select {
			case <-tick.C:
				if operation != operationIdle {
					dotPhase = (dotPhase % 3) + 1
					mStart.SetTitle(operationTitle(operation, dotPhase))
				}

			case <-mStart.ClickedCh:
				if busy {
					continue
				}
				if c.IsRunning() {
					mStart.SetTitle(operationTitle(operationStopping, 1))
					busy = true
					operation = operationStopping
					dotPhase = 1
					go func() { stopDone <- opResult{err: c.Stop(), remember: true} }()
				} else {
					mStart.Check() // immediate checkmark
					mStart.SetTitle(operationTitle(operationStarting, 1))
					busy = true
					operation = operationStarting
					dotPhase = 1
					go func() { startDone <- opResult{err: c.Start(), remember: true} }()
				}

			case r := <-startDone:
				operation = operationIdle
				if r.err != nil {
					logger.Printf("start: %v", r.err)
					mStart.Uncheck()
					mStart.SetTitle("启动代理")
					busy = false
					mMon.Disable()
					systray.SetTooltip("hellogrok 启动失败")
					dialog.Info("hellogrok 启动失败", r.err.Error())
				} else {
					if r.remember {
						if err := c.SetProxyEnabledOnLaunch(true); err != nil {
							logger.Printf("remember proxy enabled: %v", err)
							dialog.Info("hellogrok", "代理已启动，但保存启用状态失败:\n"+err.Error())
						}
					}
					setRunningUI(true)
				}

			case r := <-stopDone:
				operation = operationIdle
				if r.err != nil {
					logger.Printf("stop: %v", r.err)
					dialog.Info("hellogrok 停止失败", "为避免留下失效代理配置，hellogrok 仍保持运行:\n"+r.err.Error())
				} else if r.remember {
					if err := c.SetProxyEnabledOnLaunch(false); err != nil {
						logger.Printf("remember proxy disabled: %v", err)
						dialog.Info("hellogrok", "代理已停止，但保存停用状态失败:\n"+err.Error())
					}
				}
				setRunningUI(c.IsRunning())

			case <-mAuto.ClickedCh:
				next := !c.IsAutostart()
				if err := c.SetAutostart(next); err != nil {
					logger.Printf("autostart: %v", err)
					dialog.Info("hellogrok", "设置开机启动失败:\n"+err.Error())
				} else if next {
					mAuto.Check()
				} else {
					mAuto.Uncheck()
				}

			case <-mMon.ClickedCh:
				if !c.IsRunning() {
					continue
				}
				if err := c.OpenMonitor(); err != nil {
					logger.Printf("open monitor: %v", err)
					dialog.Info("hellogrok", "无法打开状态与日志:\n"+err.Error())
				}

			case <-mQuit.ClickedCh:
				if err := c.Stop(); err != nil {
					logger.Printf("quit deferred: %v", err)
					dialog.Info("hellogrok 无法退出", "为避免留下失效代理配置，hellogrok 仍保持运行:\n"+err.Error())
					setRunningUI(c.IsRunning())
					continue
				}
				tick.Stop()
				systray.Quit()
				return
			}
		}
	}()
}
