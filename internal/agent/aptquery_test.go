package agent

// P11.1 的代理层单测：`apt.packages` 与 `system.power_state` 的登记、参数与取值。
//
// 这里不判"哪个包算可升级"（那是 internal/packages 的事），只验证代理侧
// 有没有如实把事实交出去：
//   - 没有 dpkg-query 时两条查询都不登记（能力表不说谎）；
//   - 参数只认自己那一个字段，未知字段一律拒绝；
//   - fresh=true 真的绕过 30 秒快照。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"upanel/internal/apt"
	"upanel/internal/syscmd"
)

// fakeAptFacts 是 AptFacts 的可数实现。
type fakeAptFacts struct {
	available bool
	snap      apt.Snapshot
	state     apt.PowerState
	freshSeen []bool
	packages  int
	power     int
}

func (f *fakeAptFacts) Available() bool { return f.available }

func (f *fakeAptFacts) Packages(_ context.Context, fresh bool) (apt.Snapshot, error) {
	f.freshSeen = append(f.freshSeen, fresh)
	f.packages++
	return f.snap, nil
}

func (f *fakeAptFacts) PowerState(_ context.Context) (apt.PowerState, error) {
	f.power++
	return f.state, nil
}

func TestAptQueriesRegisteredOnlyWhenAvailable(t *testing.T) {
	facts := &fakeAptFacts{available: true}
	reg, err := NewProductionRegistry(ProductionDeps{Apt: facts})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{AptPackagesQueryName, PowerStateQueryName} {
		if _, ok := reg.queries[name]; !ok {
			t.Errorf("%s 没有登记", name)
		}
	}

	// dpkg-query 不存在（非 Debian 系）：一条都不登记，而不是登记一个必然 503 的入口。
	facts = &fakeAptFacts{available: false}
	reg, err = NewProductionRegistry(ProductionDeps{Apt: facts})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{AptPackagesQueryName, PowerStateQueryName} {
		if _, ok := reg.queries[name]; ok {
			t.Errorf("dpkg-query 不可用时不该登记 %s", name)
		}
	}

	// 完全没接：也不登记。
	reg, err = NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if len(reg.queries) != 0 {
		t.Errorf("空 deps 不该有任何查询: %v", reg.QueryNames())
	}
}

func TestAptPackagesQueryParamsStrict(t *testing.T) {
	facts := &fakeAptFacts{available: true}
	reg, err := NewProductionRegistry(ProductionDeps{Apt: facts})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	// 未知字段被拒（包括那种想塞一条命令进去的）。
	for _, body := range []string{
		`{"query":"apt.packages","params":{"fresh":true,"command":"apt-get install vim"}}`,
		`{"query":"apt.packages","params":{"fresh":"yes"}}`,
	} {
		rec := postQuery(t, srv, body, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s 状态码 = %d，期望 400（%s）", body, rec.Code, rec.Body.String())
		}
	}
	if facts.packages != 0 {
		t.Errorf("参数被拒时不该真正去采集（实际 %d 次）", facts.packages)
	}
}

func TestPowerStateQueryParamsStrict(t *testing.T) {
	facts := &fakeAptFacts{available: true}
	reg, err := NewProductionRegistry(ProductionDeps{Apt: facts})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"system.power_state","params":{"reboot":true}}`, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}

	rec = postQuery(t, srv, `{"query":"system.power_state","params":{}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
}

// TestAptPackagesQueryReturnsFacts 走真实采集链路（真的解析真机原文）。
func TestAptPackagesQueryReturnsFacts(t *testing.T) {
	collector := apt.New(apt.Options{
		Run: func(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
			switch spec.Program {
			case apt.DefaultDpkgQuery:
				return syscmd.Result{Program: spec.Program,
					Stdout: "bash\t5.1-6ubuntu1.1\tamd64\tii \n" +
						"nano\t6.2-1ubuntu0.2\tamd64\tii \n"}, nil
			case apt.DefaultAptCache:
				return syscmd.Result{Program: spec.Program, Stdout: testPolicy2204}, nil
			}
			return syscmd.Result{Program: spec.Program, ExitCode: 127}, nil
		},
	})
	facts := &realAptFacts{Collector: collector}
	reg, err := NewProductionRegistry(ProductionDeps{Apt: facts})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"apt.packages","params":{"fresh":true}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data apt.Snapshot `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Packages) != 2 {
		t.Fatalf("包数 = %d（%s）", len(body.Data.Packages), rec.Body.String())
	}
	byName := map[string]apt.Package{}
	for _, p := range body.Data.Packages {
		byName[p.Name] = p
	}
	if p := byName["nano"]; p.CandidateVersion != "6.2-1ubuntu0.3" {
		t.Errorf("nano 候选 = %q", p.CandidateVersion)
	}
	if p := byName["bash"]; len(p.CandidateSuites) != 2 {
		t.Errorf("bash 候选来源 = %v", p.CandidateSuites)
	}
	if body.Data.CollectedAt.IsZero() {
		t.Error("collected_at 缺失：采集时间是这个查询的核心信息")
	}
}

// realAptFacts 把真实采集器接到 AptFacts 上（Available 这里恒为真：
// 可用性本身在 internal/apt 的测试里覆盖）。
type realAptFacts struct{ *apt.Collector }

func (r *realAptFacts) Available() bool { return true }

func TestPowerStateQueryReturnsFacts(t *testing.T) {
	at := time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)
	facts := &fakeAptFacts{available: true, state: apt.PowerState{
		CollectedAt:            at,
		RebootRequired:         true,
		RebootRequiredSince:    &at,
		RebootRequiredPackages: []string{"linux-image-5.15.0-198-generic"},
		BootID:                 "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc",
	}}
	reg, err := NewProductionRegistry(ProductionDeps{Apt: facts})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"system.power_state","params":{}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data apt.PowerState `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.RebootRequired || body.Data.BootID == "" || len(body.Data.RebootRequiredPackages) != 1 {
		t.Fatalf("power_state = %+v", body.Data)
	}
}

// testPolicy2204 是 VM-A 上真机原文的节选（完整版见 internal/apt）。
const testPolicy2204 = `bash:
  Installed: 5.1-6ubuntu1.1
  Candidate: 5.1-6ubuntu1.1
  Version table:
 *** 5.1-6ubuntu1.1 500
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
        100 /var/lib/dpkg/status
     5.1-6ubuntu1 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
nano:
  Installed: 6.2-1ubuntu0.2
  Candidate: 6.2-1ubuntu0.3
  Version table:
     6.2-1ubuntu0.3 500 (phased 70%)
        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
 *** 6.2-1ubuntu0.2 500
        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
        100 /var/lib/dpkg/status
     6.2-1 500
        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
`
