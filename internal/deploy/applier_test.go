package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/syscmd"
)

// fakeSystemctl 记录每一次 systemctl 调用，并按脚本回答。
//
// 它记录的是**完整 argv**（除程序路径）：断言"先 daemon-reload 再 enable 再 start"
// 这种顺序上的保证，只能靠看调用序列。
type fakeSystemctl struct {
	calls []string
	// state 是 is-active / is-enabled 的回答；其它动词按 ok 回答。
	enabled string
	active  string
	// failOn 里的动词会以退出码 1 收场，用来测失败路径。
	failOn map[string]bool
	// stopFails 模拟"disable --now 跑完单元仍然是 active"（容器没停下来）。
	stopFails bool
	// stopStuck 模拟"stop 跑完单元仍然是 active"（一次更新前备份停不下来）。
	stopStuck bool
	// afterStart 是 start 之后 is-active 的回答；为空时算 "active"
	//（设成 "failed" 就是"compose up 返回 0 但单元没起来"那种现场）。
	afterStart string
	started    bool
	// stdout 按**完整 argv** 给指定的 stdout。
	//
	// docker 的读回（`image inspect --format …`）要的是一段具体输出，塞不进下面那个
	// 只认 systemctl 动词的状态机；用 argv 做键，测试里写什么就答什么。
	stdout map[string]string
	// exitCodes 按完整 argv 指定退出码（模拟 `docker image pull` 失败这类命令失败）。
	exitCodes map[string]int
}

func (f *fakeSystemctl) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	args := strings.Join(spec.Args, " ")
	f.calls = append(f.calls, args)
	res := syscmd.Result{Program: spec.Program}
	if out, ok := f.stdout[args]; ok {
		res.Stdout = out
	}
	if code, ok := f.exitCodes[args]; ok {
		res.ExitCode = code
		res.Stderr = "模拟失败：" + args
		return res, &syscmd.ExitError{Program: spec.Program, ExitCode: code}
	}
	verb := ""
	if len(spec.Args) > 0 {
		verb = spec.Args[0]
	}
	if f.failOn[verb] {
		res.ExitCode = 1
		res.Stderr = "Failed to " + verb + " unit: 模拟失败"
		return res, nil
	}
	switch verb {
	case "is-enabled":
		res.Stdout = f.enabled
		if f.enabled != "enabled" {
			// 真实的 systemctl：disabled / masked / not-found 都退出 1。
			res.ExitCode = 1
		}
	case "enable":
		f.enabled = "enabled"
	case "is-active":
		state := f.active
		if f.started {
			state = "active"
			if f.afterStart != "" {
				state = f.afterStart
			}
		}
		res.Stdout = state
		if state != "active" {
			res.ExitCode = 3
		}
	case "start", "restart":
		f.started = true
	case "stop":
		// stop 之后 is-active 要回答 inactive，否则 StopUnit 会判"停不下来"；
		// stopStuck 就是那个"停不下来"的现场。
		if !f.stopStuck {
			f.active = "inactive"
			f.started = false
		}
	case "disable":
		// disable --now 会停单元（跑 ExecStop 里的 compose down），并把 enabled 变成
		// "disabled"——之后 `is-enabled` 会以**退出码 1** 回答它。这一条不能省：
		// 把它当成"命令执行失败"，删除就永远走不完（真机 boot 段 §B3）。
		f.enabled = "disabled"
		if !f.stopFails {
			f.started = false
		}
	}
	// 真实的 syscmd 对**非零退出**返回 *syscmd.ExitError（"非零退出是结果，不是命令失败"）。
	// 假 runner 早先只填 ExitCode、错误一律 nil，于是"is-enabled 退出 1 = disabled"这类
	// 现场的**形状**在单测里根本不存在——真机 boot 段 `deployment.remove` 删不掉，
	// 而宿主测试全绿，就是被这条假象挡住的。
	if res.ExitCode != 0 {
		return res, &syscmd.ExitError{Program: spec.Program, ExitCode: res.ExitCode}
	}
	return res, nil
}

func (f *fakeSystemctl) called(want string) bool {
	for _, c := range f.calls {
		if c == want {
			return true
		}
	}
	return false
}

// newTestApplier 在临时目录里建一个 Applier（单元目录与配置目录分开，像真机上那样）。
func newTestApplier(t *testing.T, fake *fakeSystemctl) (*Applier, string, string) {
	t.Helper()
	root := t.TempDir()
	unitDir := filepath.Join(root, "systemd")
	configDir := filepath.Join(root, "deploy")
	a, err := NewApplier(ApplierOptions{
		Dir:       unitDir,
		ConfigDir: configDir,
		Systemctl: "/usr/bin/systemctl",
		Runner:    fake.Run,
	})
	if err != nil {
		t.Fatalf("构造 Applier 失败：%v", err)
	}
	return a, unitDir, configDir
}

// applyRequest 用 goldenDefinition 造一份完整的请求，配置目录与 Applier 一致。
func applyRequest(t *testing.T, configDir string) ApplyRequest {
	t.Helper()
	d := goldenDefinition()
	rendered, err := Render(context.Background(), d, RenderEnv{
		Host:     testHostEnv(),
		Resolver: &fakeResolver{value: []byte("pass$word")},
	})
	if err != nil {
		t.Fatalf("渲染配置失败：%v", err)
	}
	plan, err := PlanUnitAt(d, rendered, testHostEnv(), configDir)
	if err != nil {
		t.Fatalf("计算单元计划失败：%v", err)
	}
	return ApplyRequest{Plan: plan, Rendered: rendered, OperationID: "op_test"}
}

// goldenUnitIn 把黄金单元里的配置目录换掉（测试里的目录不是 /etc）。
func goldenUnitIn(configDir string) string {
	return strings.ReplaceAll(goldenUnit, DeployConfigDir, filepath.ToSlash(configDir))
}

// TestApplyWritesVerifiesAndReports 是主线用例：写两份文件、按顺序调 systemctl、读回事实。
func TestApplyWritesVerifiesAndReports(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, unitDir, configDir := newTestApplier(t, fake)

	report, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if !report.Written {
		t.Fatal("第一次 apply 应该真的写了文件")
	}
	if !report.Active || !report.Enabled {
		t.Fatalf("读回事实 = enabled %t / active %t，期望都为真", report.Enabled, report.Active)
	}
	if report.DeploymentID != DeploymentIDForName("webapp") {
		t.Fatalf("部署标识 = %q", report.DeploymentID)
	}

	unit, err := os.ReadFile(filepath.Join(unitDir, "upanel-deploy-webapp.service"))
	if err != nil {
		t.Fatalf("单元文件没写出来：%v", err)
	}
	if string(unit) != goldenUnitIn(configDir) {
		t.Fatalf("单元内容不对：\n%s", unit)
	}
	cfg, err := os.ReadFile(filepath.Join(configDir, "webapp.compose.yaml"))
	if err != nil {
		t.Fatalf("配置文件没写出来：%v", err)
	}
	if !strings.Contains(string(cfg), "restart: \"no\"") {
		t.Fatalf("配置里必须写死 restart: \"no\"：\n%s", cfg)
	}

	// 顺序：daemon-reload → enable → start →（回读）is-enabled → is-active。
	want := []string{
		"daemon-reload",
		"enable upanel-deploy-webapp.service",
		"start upanel-deploy-webapp.service",
		"is-enabled upanel-deploy-webapp.service",
		"is-active upanel-deploy-webapp.service",
	}
	if strings.Join(fake.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("systemctl 调用序列 = %v，期望 %v", fake.calls, want)
	}
}

// TestApplyIsIdempotent 确认同一份定义再 apply 一次不重写文件、也不重启。
func TestApplyIsIdempotent(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, _, configDir := newTestApplier(t, fake)
	req := applyRequest(t, configDir)

	if _, err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("第一次 apply 失败：%v", err)
	}
	cfgPath := filepath.Join(configDir, "webapp.compose.yaml")
	before, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	second, err := a.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("第二次 apply 失败：%v", err)
	}
	if second.Written {
		t.Fatal("同一份定义再 apply 一次不该重写文件")
	}
	after, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("配置文件被重写了（mtime 变了）")
	}
	// start 是幂等的：单元已经 active 时它什么都不做，但必须仍然被调用，
	// 因为"上一次 start 失败了、这次要重试"和"已经跑着"在命令层面是同一条。
	if !fake.called("start upanel-deploy-webapp.service") {
		t.Fatalf("第二次 apply 也应该确保单元是启动的：%v", fake.calls)
	}
}

// TestApplyRefusesDifferentDefinition 确认"改定义再 apply"被拒。
//
// 这一条是 P15.2 与 P15.3 的边界：更新要拉镜像、记 digest、按选择备份、检查健康，
// 还要有回切边界。让 deployment.apply 顺手做掉，等于把一次更新伪装成一次部署。
func TestApplyRefusesDifferentDefinition(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, _, configDir := newTestApplier(t, fake)
	req := applyRequest(t, configDir)
	if _, err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("第一次 apply 失败：%v", err)
	}
	cfgPath := filepath.Join(configDir, "webapp.compose.yaml")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// 同一名字、换一个镜像：配置摘要必然不同。
	changed := validDefinition()
	changed.ImageReference = "nginx:1.28"
	changed = mustNormalize(t, changed)
	rendered, err := Render(context.Background(), changed, RenderEnv{
		Host:     testHostEnv(),
		Resolver: &fakeResolver{value: []byte("pass$word")},
	})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	plan, err := PlanUnitAt(changed, rendered, testHostEnv(), configDir)
	if err != nil {
		t.Fatalf("单元计划失败：%v", err)
	}
	if _, err := a.Apply(context.Background(), ApplyRequest{Plan: plan, Rendered: rendered}); !errors.Is(err, ErrDeploymentExists) {
		t.Fatalf("期望 ErrDeploymentExists，实际 %v", err)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("被拒绝的 apply 不该动磁盘上的配置")
	}
}

// TestApplyRestartsWhenUnitAlreadyOnDisk 守住真机 P15.2 §5 那个现场。
//
// 上一轮崩溃留下一个 active 的包装单元，配置目录却不存在（或者配置被删）。
// oneshot + RemainAfterExit 的单元在 active 状态下再 start 是空操作：ExecStart 不会
// 重跑，刚写下去的配置永远不会被读到——单元读回 active，容器一个都没有。
// 盘上已经有这个单元、这次又真的写了东西时，唯一能让新内容生效的动词是 restart。
func TestApplyRestartsWhenUnitAlreadyOnDisk(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "active"}
	a, unitDir, configDir := newTestApplier(t, fake)
	req := applyRequest(t, configDir)

	// 造出"单元在盘上、内容与计划不同"的现场：先写一份别的内容。
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(unitDir, "upanel-deploy-webapp.service")
	if err := os.WriteFile(stale, []byte("[Unit]\nDescription=上一轮留下的\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := a.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if !report.Written {
		t.Fatal("单元内容与计划不同，这次应该真的写了文件")
	}
	if !report.Restarted {
		t.Fatal("单元本来就在盘上又写了新内容，应该用 restart")
	}
	if fake.called("start upanel-deploy-webapp.service") {
		t.Fatalf("不该再调 start（对 active 的 oneshot 是空操作）：%v", fake.calls)
	}
	if !fake.called("restart upanel-deploy-webapp.service") {
		t.Fatalf("restart 没被调用：%v", fake.calls)
	}
}

// TestApplyStartsFreshUnit 反过来确认：盘上没有这个单元时用的是 start，不是 restart。
func TestApplyStartsFreshUnit(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, _, configDir := newTestApplier(t, fake)

	report, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if report.Restarted {
		t.Fatal("新建的单元不该走 restart")
	}
	if !fake.called("start upanel-deploy-webapp.service") {
		t.Fatalf("新建时应该调 start：%v", fake.calls)
	}
}

// TestApplyFailsWhenUnitNotActive 确认"compose up 返回 0"不算成功。
func TestApplyFailsWhenUnitNotActive(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive", afterStart: "failed"}
	a, _, configDir := newTestApplier(t, fake)

	_, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err == nil {
		t.Fatal("单元不是 active 时 apply 必须失败")
	}
	if !strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("失败消息要给出下一步该看什么：%v", err)
	}
}

func TestApplyFailsWhenUnitReadBackDisabled(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, _, configDir := newTestApplier(t, fake)
	a.run = func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
		res, err := fake.Run(ctx, spec, limits)
		if len(spec.Args) > 0 && spec.Args[0] == "enable" {
			// 模拟并发 disable（或 enable 持久化失败）：命令本身返回 0，读回才是事实。
			fake.enabled = "disabled"
		}
		return res, err
	}

	report, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err == nil {
		t.Fatal("单元读回 disabled 时 apply 不能报成功")
	}
	if report.Enabled {
		t.Fatal("失败报告不能保留 enabled=true")
	}
	if !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("错误应说明读回未启用：%v", err)
	}
}

func TestAtomicWriteReportsDirectorySyncFailure(t *testing.T) {
	original := syncDir
	syncDir = func(string) error { return errors.New("injected fsync failure") }
	t.Cleanup(func() { syncDir = original })

	path := filepath.Join(t.TempDir(), "webapp.compose.yaml")
	err := writeFileAtomic(path, []byte("new config"), configFileMode)
	if err == nil || !strings.Contains(err.Error(), "同步目录") {
		t.Fatalf("目录 fsync 失败必须上抛，实际 %v", err)
	}
	// rename 已经发生，不能假装整个写入没有生效；错误消息也必须暴露这个部分成功状态。
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "new config" {
		t.Fatalf("复现前置状态不成立：内容 %q，错误 %v", got, readErr)
	}
}

// TestApplyRepairsDriftedUnit 确认被人改过的单元会被改回我们生成的那一份。
func TestApplyRepairsDriftedUnit(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, unitDir, configDir := newTestApplier(t, fake)

	// 先落一份"被人手工改过"的单元：多了一条 ExecStartPre。
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(goldenUnitIn(configDir), "[Service]\n", "[Service]\nExecStartPre=/bin/true\n", 1)
	if err := os.WriteFile(filepath.Join(unitDir, "upanel-deploy-webapp.service"), []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}
	if !report.Written {
		t.Fatal("单元被改过时必须重写，报告里要看得到")
	}
	got, err := os.ReadFile(filepath.Join(unitDir, "upanel-deploy-webapp.service"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenUnitIn(configDir) {
		t.Fatalf("单元没有被改回生成的形态：\n%s", got)
	}
}

// TestInspectToleratesDisabledAnswer 钉住"退出码就是答案"这一条。
//
// `systemctl is-enabled` 对 disabled 退出 1、`is-active` 对 inactive 退出 3。把它们当
// 执行失败，`Inspect` 会在任何被停掉的部署上直接报错（预检、删除都走它）。
func TestInspectToleratesDisabledAnswer(t *testing.T) {
	fake := &fakeSystemctl{enabled: "disabled", active: "inactive"}
	a, unitDir, _ := newTestApplier(t, fake)
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "upanel-deploy-webapp.service"), []byte(goldenUnitIn("/etc/upanel/deploy")), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := a.Inspect("webapp")
	if err != nil {
		t.Fatalf("disabled + inactive 是状态，不是错误：%v", err)
	}
	if !state.UnitExists {
		t.Fatal("单元文件在盘上，UnitExists 应该是 true")
	}
}

// TestRemoveStopsAndKeepsData 是删除的主线：停单元、删两个文件、**不碰**业务目录。
func TestRemoveStopsAndKeepsData(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, unitDir, configDir := newTestApplier(t, fake)
	req := applyRequest(t, configDir)
	report, err := a.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}

	// 业务目录（受管理卷下的数据）与一份"别的部署"的单元，删除之后都必须还在。
	dataDir := filepath.Join(t.TempDir(), "media", "webapp")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherUnit := filepath.Join(unitDir, "upanel-deploy-otherapp.service")
	if err := os.WriteFile(otherUnit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake.calls = nil
	fake.started = false
	rm, err := a.Remove(context.Background(), RemoveRequest{
		DeploymentID: report.DeploymentID,
		Version:      report.ConfigDigest,
	})
	if err != nil {
		t.Fatalf("remove 失败：%v", err)
	}
	if rm.ConfigDigest != report.ConfigDigest {
		t.Fatalf("删除报告里的版本 = %q，期望 %q", rm.ConfigDigest, report.ConfigDigest)
	}
	if !rm.Stopped || !rm.UnitRemoved || !rm.ConfigRemoved {
		t.Fatalf("删除报告不完整：%+v", rm)
	}
	if want := "disable --now upanel-deploy-webapp.service"; !fake.called(want) {
		t.Fatalf("删除必须先停单元（%s），实际调用 %v", want, fake.calls)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "upanel-deploy-webapp.service")); !os.IsNotExist(err) {
		t.Fatal("单元文件应该被删掉")
	}
	if _, err := os.Stat(filepath.Join(configDir, "webapp.compose.yaml")); !os.IsNotExist(err) {
		t.Fatal("配置文件应该被删掉")
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("业务目录必须还在（删除部署保留数据）：%v", err)
	}
	if _, err := os.Stat(otherUnit); err != nil {
		t.Fatalf("别的部署的单元必须还在：%v", err)
	}
	// 删除之后再删一次：找不到就是找不到，不是崩溃。
	if _, err := a.Remove(context.Background(), RemoveRequest{DeploymentID: report.DeploymentID}); !errors.Is(err, ErrNoDeployment) {
		t.Fatalf("第二次删除期望 ErrNoDeployment，实际 %v", err)
	}
}

// TestRemoveRefusesWrongVersion 确认版本令牌对不上时一个字都不动。
func TestRemoveRefusesWrongVersion(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, unitDir, configDir := newTestApplier(t, fake)
	report, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}

	if _, err := a.Remove(context.Background(), RemoveRequest{
		DeploymentID: report.DeploymentID,
		Version:      "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("期望 ErrVersionMismatch，实际 %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "upanel-deploy-webapp.service")); err != nil {
		t.Fatal("被拒绝的删除不该删单元")
	}
	if _, err := os.Stat(filepath.Join(configDir, "webapp.compose.yaml")); err != nil {
		t.Fatal("被拒绝的删除不该删配置")
	}
}

// TestRemoveRefusesWhileActive 确认停不下来就不删文件。
func TestRemoveRefusesWhileActive(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, unitDir, configDir := newTestApplier(t, fake)
	report, err := a.Apply(context.Background(), applyRequest(t, configDir))
	if err != nil {
		t.Fatalf("apply 失败：%v", err)
	}

	// disable --now 之后单元仍然是 active：容器没停下来。
	fake.started = true
	fake.stopFails = true
	if _, err := a.Remove(context.Background(), RemoveRequest{
		DeploymentID: report.DeploymentID,
		Version:      report.ConfigDigest,
	}); !errors.Is(err, ErrUnitStillActive) {
		t.Fatalf("期望 ErrUnitStillActive，实际 %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "upanel-deploy-webapp.service")); err != nil {
		t.Fatal("单元还在跑的时候不能删单元文件——那会留下一个没人管的部署")
	}
}

// TestFindIgnoresForeignUnits 确认扫描只认我们自己生成的单元名。
func TestFindIgnoresForeignUnits(t *testing.T) {
	fake := &fakeSystemctl{}
	a, unitDir, _ := newTestApplier(t, fake)
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"nginx.service",
		"upanel-sch_0123456789abcdef0123456789abcdef.timer",
		"upanel-deploy-.service",
		"upanel-deploy-Bad_Name.service",
		"upanel-deploy-webapp.timer",
	} {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Find(DeploymentIDForName("webapp")); !errors.Is(err, ErrNoDeployment) {
		t.Fatalf("目录里没有我们的单元时应报 ErrNoDeployment，实际 %v", err)
	}
	// 放一个合法的进来就能找到。
	if err := os.WriteFile(filepath.Join(unitDir, "upanel-deploy-webapp.service"), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := a.Find(DeploymentIDForName("webapp"))
	if err != nil || name != "webapp" {
		t.Fatalf("Find = %q（%v），期望 webapp", name, err)
	}
}

// TestRemoveToleratesMissingConfig 确认"单元在、配置不在"的现场还能收干净。
func TestRemoveToleratesMissingConfig(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	a, unitDir, configDir := newTestApplier(t, fake)
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "upanel-deploy-webapp.service"), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rm, err := a.Remove(context.Background(), RemoveRequest{
		DeploymentID: DeploymentIDForName("webapp"),
		Version:      "sha256:deadbeef",
	})
	if err != nil {
		t.Fatalf("配置不在时删除应该能收干净，实际 %v", err)
	}
	if rm.ConfigRemoved || !rm.UnitRemoved {
		t.Fatalf("报告应说明只删了单元：%+v", rm)
	}
}
