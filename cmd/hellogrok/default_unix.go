//go:build !windows && !tray

package main

import (
	"fmt"
	"log"
	"os"
)

const hasDefaultUI = false

func runDefault(_ *App, _ *log.Logger) {
	fmt.Fprintln(os.Stderr, "hellogrok has no default background UI on this platform.")
	printUsage(os.Stderr)
}

func requestDefaultExit() {}
