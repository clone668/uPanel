package proxies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/nginx"
)

// fakeFacts 是一个可控的 Nginx 现状来源。
//
// 冲突判定的分支大多取决于“读到什么”，把这些分支直接写成事实比去构造真实目录更
// 清楚；真实解析路径另有一条用真采集器跑的用例（TestCheckDomainAgainstRealConfig）。
type fakeFacts struct {
	facts        nginx.Facts
	collectErr   error
	collectCalls int
	invalidated  int
}

func (f *fakeFacts) Snapshot(context.Context) (nginx.Facts, error) { return f.facts, f.collectErr }

func (f *fakeFacts) Collect(context.Context) (nginx.Facts, error) {
	f.collectCalls++
	return f.facts, f.collectErr
}

func (f *fakeFacts) Invalidate() { f.invalidated++ }

// factsWith 造一份“读到了配置”的事实，每个文件带一组 server_name 声明。
func factsWith(names map[string][]string) nginx.Facts {
	f := nginx.Facts{Available: true, Version: "1.24.0", UnitState: "active", ConfigRoot: "/etc/nginx"}
	for path, list := range names {
		file := nginx.ConfigFile{Path: path, Managed: nginx.IsManagedName(filepath.Base(path))}
		for _, line := range list {
			// 一行 server_name 可以写多个名字，按词切成多个声明。
			srv := nginx.ServerBlock{Line: 2, NamesLine: 2}
			for _, name := range strings.Fields(line) {
				srv.Names = append(srv.Names, nginx.Classify(name))
			}
			file.Servers = append(file.Servers, srv)
		}
		f.Files = append(f.Files, file)
	}
	return f
}

// TestCheckDomainStates 走一遍四种结论与它们的依据。
func TestCheckDomainStates(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		facts     *fakeFacts
		domain    string
		wantState ConflictState
		wantInMsg string
	}{
		{
			name:      "别的域名不算冲突",
			facts:     &fakeFacts{facts: factsWith(map[string][]string{"/etc/nginx/conf.d/blog.conf": {"blog.example.com"}})},
			domain:    "shop.example.com",
			wantState: ConflictClear,
		},
		{
			name:      "同一个域名是冲突",
			facts:     &fakeFacts{facts: factsWith(map[string][]string{"/etc/nginx/conf.d/blog.conf": {"blog.example.com"}})},
			domain:    "blog.example.com",
			wantState: ConflictTaken,
			wantInMsg: "blog.conf 第 2 行",
		},
		{
			name:      "通配子域也算占住",
			facts:     &fakeFacts{facts: factsWith(map[string][]string{"/etc/nginx/sites-enabled/x": {"*.example.com"}})},
			domain:    "a.example.com",
			wantState: ConflictTaken,
		},
		{
			name:      "有文件解析失败就判不了",
			facts:     &fakeFacts{facts: nginx.Facts{Available: true, Files: []nginx.ConfigFile{{Path: "/etc/nginx/conf.d/broken.conf", Error: "第 1 行：块没有闭合"}}}},
			domain:    "shop.example.com",
			wantState: ConflictUnknown,
			wantInMsg: "解析失败",
		},
		{
			name:      "正则 server_name 判不了",
			facts:     &fakeFacts{facts: factsWith(map[string][]string{"/etc/nginx/conf.d/re.conf": {"~^www\\d+\\.example\\.com$"}})},
			domain:    "shop.example.com",
			wantState: ConflictUnknown,
			wantInMsg: "无法判定",
		},
		{
			name:      "主机上没有 Nginx 配置",
			facts:     &fakeFacts{facts: nginx.Facts{ConfigMissing: true, Reason: "主机上没有 /etc/nginx 目录"}},
			domain:    "shop.example.com",
			wantState: ConflictClear,
			wantInMsg: "没有 /etc/nginx",
		},
		{
			name:      "配置读不了（不是没装）",
			facts:     &fakeFacts{facts: nginx.Facts{Reason: "读不到配置目录 /etc/nginx：权限不足"}},
			domain:    "shop.example.com",
			wantState: ConflictUnknown,
			wantInMsg: "权限不足",
		},
		{
			name:      "没有连代理",
			facts:     nil,
			domain:    "shop.example.com",
			wantState: ConflictUnknown,
			wantInMsg: "没有连上代理",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t, Options{})
			if tc.facts != nil {
				svc.facts = tc.facts
			}
			report, err := svc.CheckDomain(ctx, tc.domain, "")
			if err != nil {
				t.Fatalf("不该返回错误：%v", err)
			}
			if report.State != tc.wantState {
				t.Fatalf("结论应为 %s，实际 %s（%s）", tc.wantState, report.State, report.Reason)
			}
			if tc.wantInMsg != "" && !strings.Contains(report.Reason, tc.wantInMsg) {
				t.Fatalf("说明里应提到 %q，实际 %q", tc.wantInMsg, report.Reason)
			}
			if report.State != ConflictClear && !report.Blocking() {
				t.Fatal("非 clear 的结论必须阻止写入")
			}
			if report.State == ConflictClear && report.Blocking() {
				t.Fatal("clear 的结论不该阻止写入")
			}
		})
	}
}

// TestCheckDomainReportsEvidence 确认结论带着证据：哪个文件、第几行、原文是什么。
func TestCheckDomainReportsEvidence(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	svc.facts = &fakeFacts{facts: factsWith(map[string][]string{
		"/etc/nginx/conf.d/shop.conf": {"shop.example.com www.shop.example.com"},
	})}

	report, err := svc.CheckDomain(context.Background(), "Shop.Example.com.", "")
	if err != nil {
		t.Fatalf("不该返回错误：%v", err)
	}
	if report.State != ConflictTaken {
		t.Fatalf("应为 taken，实际 %s", report.State)
	}
	if len(report.Matches) != 1 {
		t.Fatalf("应有一条证据，实际 %d 条", len(report.Matches))
	}
	hit := report.Matches[0]
	if hit.File != "/etc/nginx/conf.d/shop.conf" || hit.Line != 2 || hit.Name != "shop.example.com" {
		t.Fatalf("证据不对：%+v", hit)
	}
	if hit.Managed {
		t.Fatal("shop.conf 不是面板的文件")
	}
	// 域名归一之后再判：大小写和尾点不该影响结论。
	if report.Domain != "shop.example.com" {
		t.Fatalf("报告里的域名应是归一化后的，实际 %q", report.Domain)
	}
}

// TestCheckDomainPanelRecord 确认面板自己的记录也算“占用”，但改自己不算。
func TestCheckDomainPanelRecord(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	svc.facts = &fakeFacts{facts: factsWith(nil)}

	site, err := svc.Create(context.Background(), validDefinition())
	if err != nil {
		t.Fatalf("创建应该成功：%v", err)
	}
	report, err := svc.CheckDomain(context.Background(), site.Domain, "")
	if err != nil {
		t.Fatalf("不该返回错误：%v", err)
	}
	if report.State != ConflictTaken || report.ExistingSiteID != site.ID {
		t.Fatalf("同域名应报 taken 并指出已有记录，实际 %+v", report)
	}

	report, err = svc.CheckDomain(context.Background(), site.Domain, site.ID)
	if err != nil {
		t.Fatalf("不该返回错误：%v", err)
	}
	if report.State != ConflictClear {
		t.Fatalf("改自己不该算冲突，实际 %s", report.State)
	}
}

// TestCheckDomainReadFailure 确认“读失败”是错误而不是结论。
func TestCheckDomainReadFailure(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	svc.facts = &fakeFacts{collectErr: errors.New("ssh 掉了")}

	_, err := svc.CheckDomain(context.Background(), "shop.example.com", "")
	if !errors.Is(err, ErrFactsUnavailable) {
		t.Fatalf("应为 ErrFactsUnavailable，实际 %v", err)
	}
}

// TestCheckDomainRejectsBadDomain 确认冲突检查先做域名归一。
func TestCheckDomainRejectsBadDomain(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	svc.facts = &fakeFacts{facts: factsWith(nil)}

	_, err := svc.CheckDomain(context.Background(), "https://shop.example.com", "")
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("应为校验错误，实际 %v", err)
	}
}

// TestCheckDomainAgainstRealConfig 用真采集器读一份真配置，确认判定链路上没有
// “假事实”和“真解析”之间的缝隙。
func TestCheckDomainAgainstRealConfig(t *testing.T) {
	root := t.TempDir()
	confRoot := filepath.Join(root, "etc-nginx")
	if err := os.MkdirAll(filepath.Join(confRoot, "conf.d"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(confRoot, "nginx.conf"),
		[]byte("http { include /etc/nginx/conf.d/*.conf; }\n"), 0o644); err != nil {
		t.Fatalf("写主配置失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(confRoot, "conf.d", "blog.conf"),
		[]byte("server {\n    listen 80;\n    server_name blog.example.com;\n}\n"), 0o644); err != nil {
		t.Fatalf("写站点配置失败：%v", err)
	}

	svc, _ := newTestService(t, Options{
		Facts: nginx.New(nginx.Options{
			ConfigRoot: confRoot,
			NginxPath:  filepath.Join(root, "nginx-bin"),
		}),
	})
	report, err := svc.CheckDomain(context.Background(), "blog.example.com", "")
	if err != nil {
		t.Fatalf("不该返回错误：%v", err)
	}
	if report.State != ConflictTaken {
		t.Fatalf("应为 taken，实际 %s（%s）", report.State, report.Reason)
	}
	if len(report.Matches) != 1 || report.Matches[0].Line != 3 {
		t.Fatalf("证据应为 blog.conf 第 3 行，实际 %+v", report.Matches)
	}
}
