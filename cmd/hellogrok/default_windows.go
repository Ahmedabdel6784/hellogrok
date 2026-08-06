//go:build windows

package main

import (
	_ "embed"
	"log"

	"github.com/hellowind777/hellogrok/internal/tray"
)

//go:embed icon.ico
var trayIcon []byte

const hasDefaultUI = true

func runDefault(app *App, logger *log.Logger) {
	tray.Run(app, trayIcon, logger)
}

func requestDefaultExit() { tray.Quit() }
