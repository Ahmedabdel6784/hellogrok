//go:build windows || tray

package tray

import "testing"

func TestOperationTitleKeepsStartingAndStoppingDistinct(t *testing.T) {
	tests := []struct {
		name      string
		operation proxyOperation
		dots      int
		want      string
	}{
		{name: "idle", operation: operationIdle, dots: 1, want: "启动代理"},
		{name: "starting", operation: operationStarting, dots: 2, want: "启动代理 (启动中..)"},
		{name: "stopping", operation: operationStopping, dots: 3, want: "启动代理 (停止中...)"},
		{name: "invalid dot phase", operation: operationStopping, dots: 0, want: "启动代理 (停止中.)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := operationTitle(test.operation, test.dots); got != test.want {
				t.Fatalf("operationTitle() = %q, want %q", got, test.want)
			}
		})
	}
}
