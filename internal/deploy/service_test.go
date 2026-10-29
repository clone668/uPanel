package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// newTestService 造一个目录都在临时目录里的 Service。
//
// 健康探测默认换成假件：goldenDefinition 开了健康检查，真去探一个没人监听的宿主端口
// 会按定义里的节奏重试几十秒——单测不该为一个假现场等那么久。
func newTestService(t *testing.T, fake *fakeSystemctl, resolver Resolver) (*Service, *Applier, string) {
	t.Helper()
	applier, _, configDir := newTestApplier(t, fake)
	if resolver == nil {
		resolver = &fakeResolver{value: []byte("pass$word")}
	}
	svc, err := NewService(ServiceOptions{
		Applier:  applier,
		Resolver: resolver,
		Env:      func(context.Context) (HostEnv, error) { return testHostEnv(), nil },
		Prober:   &fakeHealthProber{},
	})
	if err != nil {
		t.Fatalf("构造 Service 失败：%v", err)
	}
	return svc, applier, configDir
}

// TestAssessApplyHappyPath 是预检的主线：允许、摘要稳定、单元名对得上。
func TestAssessApplyHappyPath(t *testing.T) {
	svc, _, _ := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)

	assessment, err := svc.AssessApply(context.Background(), ApplyParams{Definition: goldenDefinition()})
	if err != nil {
		t.Fatalf("预检失败：%v", err)
	}
	if !assessment.Allowed {
		t.Fatalf("应该允许，实际阻塞：%v", assessment.Blockers)
	}
	if assessment.DeploymentID != DeploymentIDForName("webapp") {
		t.Fatalf("部署标识 = %q", assessment.DeploymentID)
	}
	if assessment.UnitName != "upanel-deploy-webapp.service" {
		t.Fatalf("单元名 = %q", assessment.UnitName)
	}
	if !strings.HasPrefix(assessment.ConfigDigest, "sha256:") {
		t.Fatalf("配置摘要 = %q", assessment.ConfigDigest)
	}
	if assessment.Existing {
		t.Fatal("磁盘上还什么都没有，不该报“已存在”")
	}
	if len(assessment.Claims) != 1 || assessment.Claims[0].Port != 18080 {
		t.Fatalf("端口声明 = %v", assessment.Claims)
	}
	if len(assessment.VolumeMountPoints) != 1 || assessment.VolumeMountPoints[0] != "/srv/upanel/volumes/media" {
		t.Fatalf("卷挂载点 = %v", assessment.VolumeMountPoints)
	}
	// 健康探测发生在**执行**阶段（P15.3）：预检不该再声称“这一版不探测”，
	// 也不该提前给一条与现场无关的告警。
	for _, warning := range assessment.Warnings {
		if warning.Code == "health_probe_pending" {
			t.Fatalf("健康探测已经接线，不该再有 pending 告警：%v", assessment.Warnings)
		}
	}
}

// TestAssessApplyRefusesUpdate 确认"带着 deployment_id 再 apply"被明确拒绝。
//
// 这一条挡住的是"把一次更新伪装成一次部署"：更新要拉镜像、记 digest、按选择备份、
// 检查健康、划回切边界，这些在 P15.2 里一件都还没做。
func TestAssessApplyRefusesUpdate(t *testing.T) {
	svc, _, _ := newTestService(t, &fakeSystemctl{}, nil)
	id := DeploymentIDForName("webapp")
	_, err := svc.AssessApply(context.Background(), ApplyParams{DeploymentID: &id, Definition: goldenDefinition()})
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != "update_not_implemented" {
		t.Fatalf("期望 update_not_implemented，实际 %v", err)
	}

	// 标识与名字对不上时先报这个：一个错的 id 说明客户端自己就糊涂了。
	other := DeploymentIDForName("otherapp")
	_, err = svc.AssessApply(context.Background(), ApplyParams{DeploymentID: &other, Definition: goldenDefinition()})
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "deployment_id" {
		t.Fatalf("期望 deployment_id 形状错误，实际 %v", err)
	}
}

// TestAssessApplyBlocksOnUnmountedVolume 确认卷没挂上时不许部署。
func TestAssessApplyBlocksOnUnmountedVolume(t *testing.T) {
	applier, _, _ := newTestApplier(t, &fakeSystemctl{})
	env := testHostEnv()
	vol := env.Volumes[testVolumeID]
	vol.Mounted = false
	env.Volumes[testVolumeID] = vol
	svc, err := NewService(ServiceOptions{
		Applier:  applier,
		Resolver: &fakeResolver{value: []byte("x")},
		Env:      func(context.Context) (HostEnv, error) { return env, nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	assessment, err := svc.AssessApply(context.Background(), ApplyParams{Definition: goldenDefinition()})
	if err != nil {
		t.Fatalf("预检本身不该报错：%v", err)
	}
	if assessment.Allowed {
		t.Fatal("卷没挂上时不能允许部署")
	}
	if len(assessment.Blockers) != 1 || assessment.Blockers[0].Code != "volume_not_ready" {
		t.Fatalf("阻塞原因 = %v", assessment.Blockers)
	}
}

// TestAssessApplyBlocksOnSecretFailure 确认秘密读不到时整份配置不产出。
func TestAssessApplyBlocksOnSecretFailure(t *testing.T) {
	svc, _, _ := newTestService(t, &fakeSystemctl{}, &fakeResolver{err: ErrSecretUnavailable})

	assessment, err := svc.AssessApply(context.Background(), ApplyParams{Definition: goldenDefinition()})
	if err != nil {
		t.Fatalf("预检本身不该报错：%v", err)
	}
	if assessment.Allowed || assessment.ConfigDigest != "" {
		t.Fatalf("秘密读不到时不该给出摘要：%+v", assessment)
	}
	if len(assessment.Blockers) != 1 || assessment.Blockers[0].Code != "secret_unavailable" {
		t.Fatalf("阻塞原因 = %v", assessment.Blockers)
	}
}

// TestExecuteApplyWritesAndVerifies 从预检走到落地：文件在、单元 active、报告完整。
func TestExecuteApplyWritesAndVerifies(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, _, configDir := newTestService(t, fake, nil)

	assessment, err := svc.AssessApply(context.Background(), ApplyParams{Definition: goldenDefinition()})
	if err != nil || !assessment.Allowed {
		t.Fatalf("预检失败：%v %v", err, assessment.Blockers)
	}
	out, err := svc.ExecuteApply(context.Background(), ApplyParams{Definition: goldenDefinition()}, assessment.StateVersion, nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if out.Idempotent {
		t.Fatal("第一次落地不该算幂等重放")
	}
	if !out.Report.Active || !out.Report.Enabled {
		t.Fatalf("读回事实 = %+v", out.Report)
	}
	if _, err := os.Stat(filepath.Join(configDir, "webapp.compose.yaml")); err != nil {
		t.Fatalf("配置文件没写出来：%v", err)
	}

	// 同一份定义再走一遍：允许、幂等、不重写。
	second, err := svc.AssessApply(context.Background(), ApplyParams{Definition: goldenDefinition()})
	if err != nil || !second.Allowed {
		t.Fatalf("重放预检失败：%v %v", err, second.Blockers)
	}
	if !second.Existing || second.StateVersion != second.ConfigDigest {
		t.Fatalf("重放时版本令牌应该等于第一次的摘要：%+v", second)
	}
	replay, err := svc.ExecuteApply(context.Background(), ApplyParams{Definition: goldenDefinition()}, second.StateVersion, nil)
	if err != nil {
		t.Fatalf("重放失败：%v", err)
	}
	if !replay.Idempotent {
		t.Fatal("同一份定义重放应该不写文件")
	}
}

// TestExecuteApplyProbesPublicEntry：部署成功之后从宿主侧探一次公开入口。
//
// 探的是宿主端口而不是容器端口（端口映射、反代、监听地址都可能让里面 200、外面打不开），
// 而"单元 active"与"应用能应答"是两件事——探不通要说出来，不能默默算成功。
func TestExecuteApplyProbesPublicEntry(t *testing.T) {
	fake := &fakeSystemctl{enabled: "enabled", active: "inactive"}
	svc, _, _ := newTestService(t, fake, nil)
	prober := &fakeHealthProber{}
	svc.prober = prober

	out, err := svc.ExecuteApply(context.Background(), ApplyParams{Definition: goldenDefinition()}, "", nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if !out.Health.Checked || !out.Health.Healthy {
		t.Fatalf("应探通：%+v", out.Health)
	}
	if len(prober.targets) != 1 || prober.targets[0].Port != 18080 {
		t.Fatalf("探测目标 = %+v，期望宿主端口 18080", prober.targets)
	}
	for _, warning := range out.Warnings {
		if strings.Contains(warning, "健康探测") {
			t.Fatalf("探通了不该有健康告警：%v", out.Warnings)
		}
	}

	// 探不通：部署照样落地，但必须留一条告警（结论由上层决定，这里不下修）。
	svc2, _, _ := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)
	svc2.prober = &fakeHealthProber{results: []HealthProbe{
		{Checked: true, Healthy: false, Attempts: 4, LastError: "连不上 127.0.0.1:18080"},
	}}
	unhealthy, err := svc2.ExecuteApply(context.Background(), ApplyParams{Definition: goldenDefinition()}, "", nil)
	if err != nil {
		t.Fatalf("探不通不该让部署失败：%v", err)
	}
	if !unhealthy.Report.Active {
		t.Fatalf("部署仍然要落地：%+v", unhealthy.Report)
	}
	if len(unhealthy.Warnings) == 0 || !strings.Contains(unhealthy.Warnings[0], "健康探测没探通") {
		t.Fatalf("告警 = %v", unhealthy.Warnings)
	}
}

// TestExecuteApplyRejectsStaleState 确认预检之后现场变了就拒绝执行。
func TestExecuteApplyRejectsStaleState(t *testing.T) {
	svc, _, _ := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)
	_, err := svc.ExecuteApply(context.Background(),
		ApplyParams{Definition: goldenDefinition()}, "sha256:"+strings.Repeat("0", 64), nil)
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != "stale" {
		t.Fatalf("期望 stale，实际 %v", err)
	}
}

// TestExecuteApplyRefusesChangedDefinition 确认磁盘上已有别的版本时阻塞。
func TestExecuteApplyRefusesChangedDefinition(t *testing.T) {
	svc, _, configDir := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)
	if _, err := svc.ExecuteApply(context.Background(),
		ApplyParams{Definition: goldenDefinition()}, "", nil); err != nil {
		t.Fatalf("首次落地失败：%v", err)
	}
	before, err := os.ReadFile(filepath.Join(configDir, "webapp.compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	changed := validDefinition()
	changed.ImageReference = "nginx:1.28"
	assessment, err := svc.AssessApply(context.Background(), ApplyParams{Definition: changed})
	if err != nil {
		t.Fatalf("预检不该报错：%v", err)
	}
	if assessment.Allowed {
		t.Fatal("定义变了却仍允许 apply")
	}
	if assessment.Blockers[0].Code != "deployment_exists" {
		t.Fatalf("阻塞原因 = %v", assessment.Blockers)
	}
	if _, err := svc.ExecuteApply(context.Background(), ApplyParams{Definition: changed}, "", nil); err == nil {
		t.Fatal("执行阶段也该拒绝")
	}
	after, err := os.ReadFile(filepath.Join(configDir, "webapp.compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("被拒绝的请求不该改动磁盘上的配置")
	}
}

// TestAssessRemoveUnknownDeployment 确认找不到就是找不到。
func TestAssessRemoveUnknownDeployment(t *testing.T) {
	svc, _, _ := newTestService(t, &fakeSystemctl{}, nil)
	assessment, err := svc.AssessRemove(context.Background(), RemoveParams{DeploymentID: DeploymentIDForName("ghost")})
	if err != nil {
		t.Fatalf("预检不该报错：%v", err)
	}
	if assessment.Allowed || assessment.Blockers[0].Code != "no_such_deployment" {
		t.Fatalf("期望 no_such_deployment，实际 %+v", assessment)
	}
	if _, err := svc.ExecuteRemove(context.Background(), RemoveParams{DeploymentID: DeploymentIDForName("ghost")}, "", nil); err == nil {
		t.Fatal("执行阶段也该拒绝")
	}
}

// TestExecuteRemoveKeepsData 走完删除：单元与配置没了、业务目录还在。
func TestExecuteRemoveKeepsData(t *testing.T) {
	svc, applier, configDir := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)
	if _, err := svc.ExecuteApply(context.Background(), ApplyParams{Definition: goldenDefinition()}, "", nil); err != nil {
		t.Fatalf("落地失败：%v", err)
	}
	dataDir := filepath.Join(t.TempDir(), "media", "webapp")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id := DeploymentIDForName("webapp")
	assessment, err := svc.AssessRemove(context.Background(), RemoveParams{DeploymentID: id})
	if err != nil || !assessment.Allowed {
		t.Fatalf("删除预检失败：%v %v", err, assessment.Blockers)
	}
	if assessment.ProjectName != "webapp" || assessment.UnitName != "upanel-deploy-webapp.service" {
		t.Fatalf("删除预检 대상 = %+v", assessment)
	}
	out, err := svc.ExecuteRemove(context.Background(), RemoveParams{DeploymentID: id}, assessment.StateVersion, nil)
	if err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if !out.Report.UnitRemoved || !out.Report.ConfigRemoved {
		t.Fatalf("删除报告 = %+v", out.Report)
	}
	if _, err := os.Stat(filepath.Join(configDir, "webapp.compose.yaml")); !os.IsNotExist(err) {
		t.Fatal("配置应该被删掉")
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("业务目录必须保留：%v", err)
	}
	if state, err := applier.Inspect("webapp"); err != nil || state.ConfigExists || state.UnitExists {
		t.Fatalf("删除之后现场应该是空的：%+v（%v）", state, err)
	}
}

// TestAssessmentLocks：锁是 compose 项目 + 端口 + 卷，去重且已排序。
//
// 排序是代理侧锁管理器的硬要求（死锁避免靠固定顺序），去重是给“依赖卷写了两遍”
// 这种合法定义准备的——重复的锁名会让同一个操作把自己排在队里。
func TestAssessmentLocks(t *testing.T) {
	d := validDefinition()
	assessment := ApplyAssessment{
		Definition:  d,
		ProjectName: d.Name,
		Claims:      d.Claims(),
	}
	locks := assessment.Locks()
	want := []string{
		LockName(d.Name),
		PortLockName(d.Claims()[0]),
		VolumeLockName(testVolumeID),
	}
	sort.Strings(want)
	if strings.Join(locks, ",") != strings.Join(want, ",") {
		t.Fatalf("锁 = %v，期望 %v", locks, want)
	}
	if !sort.StringsAreSorted(locks) {
		t.Fatalf("锁必须先排序：%v", locks)
	}

	// 两遍同一个卷只留一把。
	d.VolumeDependencies = append(d.VolumeDependencies, testVolumeID)
	duplicated := ApplyAssessment{Definition: d, ProjectName: d.Name, Claims: d.Claims()}
	if got := len(duplicated.Locks()); got != len(want) {
		t.Fatalf("重复的依赖卷不该多出一把锁：%v", duplicated.Locks())
	}

	// 删除只锁 compose 项目：它释放端口、不动卷。
	remove := RemoveAssessment{ProjectName: d.Name}
	if got := remove.Locks(); len(got) != 1 || got[0] != LockName(d.Name) {
		t.Fatalf("删除的锁 = %v", got)
	}
	if got := (RemoveAssessment{}).Locks(); got != nil {
		t.Fatalf("没有部署名时不该有锁：%v", got)
	}
}

// TestAssessApplyBindsStateVersionOnCreate 确认**新建**部署的预检也给出非空版本令牌。
//
// 代理层对一条 allowed 的预检要求 StateVersion 非空（agent/preflight.go 的
// validatePlanResult）：空字符串等于“没有绑定状态快照”，整条预检会被判成实现错误，
// 用户拿到 500 而不是一份部署计划。这一条在真机 P15.2 验收 §5 上真实发生过——
// deployment.apply 的预检全部 500，直到把“新建没有旧版本”从空令牌改成绑定这一次
// 将要落盘的摘要。
//
// 令牌不只是“非空”：它必须抓得住漂移。预检之后换了镜像，同一份令牌必须被拒。
func TestAssessApplyBindsStateVersionOnCreate(t *testing.T) {
	svc, _, configDir := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)

	assessment, err := svc.AssessApply(context.Background(), ApplyParams{Definition: goldenDefinition()})
	if err != nil || !assessment.Allowed {
		t.Fatalf("预检失败：%v %v", err, assessment.Blockers)
	}
	if assessment.Existing {
		t.Fatal("磁盘上还什么都没有，不该报“已存在”")
	}
	if assessment.StateVersion == "" {
		t.Fatal("新建部署的版本令牌为空：这条预检会被代理层判成不合法")
	}
	if assessment.StateVersion != assessment.ConfigDigest {
		t.Fatalf("新建时该绑定这一次要落盘的摘要：令牌 %q，摘要 %q",
			assessment.StateVersion, assessment.ConfigDigest)
	}

	// 换了定义（镜像不同），预检仍然允许（还是新建），但令牌必须变。
	other := goldenDefinition()
	other.ImageReference = "nginx:1.28"
	changed, err := svc.AssessApply(context.Background(), ApplyParams{Definition: other})
	if err != nil || !changed.Allowed {
		t.Fatalf("换镜像的预检不该被拒：%v %v", err, changed.Blockers)
	}
	if changed.StateVersion == assessment.StateVersion {
		t.Fatal("定义变了，版本令牌没变：这条绑定抓不到任何漂移")
	}
	var svcErr *ServiceError
	if _, err := svc.ExecuteApply(context.Background(),
		ApplyParams{Definition: other}, assessment.StateVersion, nil); !errors.As(err, &svcErr) ||
		svcErr.Code != "stale" {
		t.Fatalf("拿着旧令牌执行新定义应该是 stale，实际 %v", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "webapp.compose.yaml")); !os.IsNotExist(err) {
		t.Fatalf("被拒的执行不该落盘：%v", err)
	}
}

// TestAssessRemoveBindsVersionWhenConfigMissing 确认“单元还在、配置被人删了”时
// 删除预检仍然给出非空令牌。
//
// 这也是真机上的同一条规矩：allowed 的预检不能带空 StateVersion。盘上没有摘要
// 可绑时令牌是 unit-only——一个如实的字面量，而不是编一个假摘要把“配置不见了”
// 盖住。
func TestAssessRemoveBindsVersionWhenConfigMissing(t *testing.T) {
	svc, _, configDir := newTestService(t, &fakeSystemctl{enabled: "enabled", active: "inactive"}, nil)
	ctx := context.Background()
	params := ApplyParams{Definition: goldenDefinition()}
	assessment, err := svc.AssessApply(ctx, params)
	if err != nil || !assessment.Allowed {
		t.Fatalf("预检失败：%v %v", err, assessment.Blockers)
	}
	if _, err := svc.ExecuteApply(ctx, params, assessment.StateVersion, nil); err != nil {
		t.Fatalf("落地失败：%v", err)
	}

	// 有人手工删了渲染配置：单元文件还在，配置不在。
	if err := os.Remove(filepath.Join(configDir, "webapp.compose.yaml")); err != nil {
		t.Fatalf("删不掉夹具配置：%v", err)
	}

	id := DeploymentIDForName("webapp")
	remove, err := svc.AssessRemove(ctx, RemoveParams{DeploymentID: id})
	if err != nil {
		t.Fatalf("删除预检本身不该报错：%v", err)
	}
	if !remove.Allowed {
		t.Fatalf("配置不在时删除仍然该允许：%v", remove.Blockers)
	}
	if remove.StateVersion != "unit-only" {
		t.Fatalf("版本令牌 = %q，期望 unit-only", remove.StateVersion)
	}
	if _, err := svc.ExecuteRemove(ctx, RemoveParams{DeploymentID: id}, remove.StateVersion, nil); err != nil {
		t.Fatalf("带着 unit-only 令牌删除失败：%v", err)
	}
}
