package scripts

// 审计回归测试。

import (
	"context"
	"strings"
	"testing"
)

// Run 曾把 MaxRuntimeSeconds<=0 悄悄换成一分钟、对超出上限的值照单全收：
// 预检（AssessRun）与参数校验（RunParams.Validate）都在上游，而 Run 是最后
// 还能拒绝的一道。执行前的重判不能少。
func TestRunRejectsInvalidRuntime(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\ntrue\n")

	for _, secs := range []int{0, -5, MaxRuntimeSecondsLimit + 1} {
		_, err := h.mod.Run(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: secs})
		if err == nil {
			t.Fatalf("max_runtime_seconds=%d 必须被拒绝", secs)
		}
		if !strings.Contains(err.Error(), "max_runtime_seconds") {
			t.Fatalf("max_runtime_seconds=%d 的错误应当点名这个字段，收到：%v", secs, err)
		}
	}
	if h.run.call != 0 {
		t.Fatalf("被拒绝的运行不该碰到执行器，实际调了 %d 次", h.run.call)
	}
}
