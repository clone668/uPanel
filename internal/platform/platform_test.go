package platform

import (
	"errors"
	"runtime"
	"testing"
)

func TestParseModeRejectsUnknown(t *testing.T) {
	for _, in := range []string{"", "Production", "prod", "MOCK", "demo", " mock"} {
		if _, err := ParseMode(in); err == nil {
			t.Fatalf("ParseMode(%q) 应当失败", in)
		}
	}
	for _, in := range []string{"production", "mock"} {
		m, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q) 失败: %v", in, err)
		}
		if m.String() != in {
			t.Fatalf("ParseMode(%q) = %q", in, m.String())
		}
	}
}

func TestCheckModeMockAlwaysAllowed(t *testing.T) {
	if err := CheckMode(ModeMock); err != nil {
		t.Fatalf("mock 模式在当前平台应可用: %v", err)
	}
}

func TestCheckModeProductionMatchesPlatformFacts(t *testing.T) {
	// 该断言在 Linux（CI/VM）与非 Linux（开发宿主）上都必须成立：
	// production 要么可用，要么以 ErrProductionUnsupported 明确拒绝，不允许其他失败方式。
	err := CheckMode(ModeProduction)
	if osFacts.productionSupported {
		if err != nil {
			t.Fatalf("productionSupported=true 但 CheckMode 返回错误: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("productionSupported=false 时 CheckMode(ModeProduction) 必须返回错误")
	}
	if !errors.Is(err, ErrProductionUnsupported) {
		t.Fatalf("错误必须包装 ErrProductionUnsupported，实际: %v", err)
	}
}

func TestCheckModeRejectsInvalidMode(t *testing.T) {
	if err := CheckMode(Mode("demo")); err == nil {
		t.Fatal("非法模式必须报错")
	}
}

func TestCurrentFactsConsistent(t *testing.T) {
	info := Current()
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatalf("Current() = %s/%s，期望 %s/%s", info.OS, info.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if info.SupportsPrivilegedOps != osFacts.supportsPrivilegedOps {
		t.Fatal("Info 与 osFacts 不一致")
	}
	if !info.MockAvailable {
		t.Fatal("mock 模式在所有平台都应可用")
	}
	if runtime.GOOS == "linux" && !info.SupportsUnixSocket {
		t.Fatal("Linux 必须支持 Unix socket")
	}
	if runtime.GOOS != "linux" && info.SupportsUnixSocket {
		t.Fatal("非 Linux 平台不应声称支持 Unix socket")
	}
}

func TestCheckAgentRequiresLinux(t *testing.T) {
	err := CheckAgent()
	if runtime.GOOS == "linux" {
		// 本测试通常在非 root 下运行，因此期望 root 检查失败；
		// 在 root 下则应当通过。
		if IsRoot() && err != nil {
			t.Fatalf("root 下 CheckAgent 应通过: %v", err)
		}
		if !IsRoot() && (err == nil || err.Error() != "upanel-agent 必须以 root 运行") {
			t.Fatalf("非 root 下应拒绝并说明原因，实际: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("非 Linux 平台必须拒绝启动代理")
	}
}
