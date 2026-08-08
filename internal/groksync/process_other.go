//go:build !windows

package groksync

import "os/exec"

func configureCommand(*exec.Cmd) {}

func supplementalLeaderCandidates(_ []leaderInfo) []leaderInfo { return nil }
