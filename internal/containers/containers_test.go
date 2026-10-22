package containers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// 一个 64 位容器 ID（测试里反复用到）。
const (
	testID  = "3f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"
	testID2 = "9a8b7c6d5e4f30211203f4e5d6c7b8a9908172635445362718091a2b3c4d5e6f"
)

// psLine 拼一行 ps 输出。
func psLine(fields ...string) string { return strings.Join(fields, fs) }

func TestParsePS(t *testing.T) {
	out := strings.Join([]string{
		psLine(testID, "web", "nginx:1.27", "running", "Up 2 hours", "2026-10-21 10:00:00 +0000 UTC",
			"0.0.0.0:8080->80/tcp, :::8080->80/tcp", ""),
		psLine(testID2, "/db,alias", "postgres:16", "exited", "Exited (0) 3 hours ago", "2026-10-20 09:00:00 +0000 UTC",
			"", "com.docker.compose.project=shop,com.docker.compose.service=db,secret=abc"),
	}, "\n")
	rows, err := parsePS(out)
	if err != nil {
		t.Fatalf("parsePS: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 = %d，期望 2", len(rows))
	}
	if rows[0].Name != "web" || rows[0].State != "running" {
		t.Errorf("第一行 = %+v", rows[0])
	}
	if len(rows[0].Ports) != 2 || rows[0].Ports[0].HostPort != 8080 || rows[0].Ports[0].ContainerPort != 80 {
		t.Errorf("端口 = %+v", rows[0].Ports)
	}
	// 多名字取第一个，并去掉前导斜杠。
	if rows[1].Name != "db" {
		t.Errorf("名字 = %q，期望 db", rows[1].Name)
	}
	if rows[1].Compose == nil || rows[1].Compose.Project != "shop" || rows[1].Compose.Service != "db" {
		t.Errorf("compose 标签 = %+v", rows[1].Compose)
	}
}

func TestParsePSRejectsWrongColumnCount(t *testing.T) {
	if _, err := parsePS("a" + fs + "b"); err == nil {
		t.Fatal("列数不对时必须报错，不能静默给半份清单")
	}
}

func TestParseComposeLabelsDropsUnknownLabels(t *testing.T) {
	ref := parseComposeLabels("com.docker.compose.project=shop,db_password=hunter2,foo=bar")
	if ref == nil || ref.Project != "shop" {
		t.Fatalf("ref = %+v", ref)
	}
	// 非 compose 键不该出现在任何字段里（它们被丢弃，而不是被转发）。
	if strings.Contains(ref.Project+ref.Service+ref.WorkingDir+ref.ConfigFiles, "hunter2") {
		t.Fatal("第三方标签被带出来了")
	}
	if parseComposeLabels("") != nil || parseComposeLabels("<no value>") != nil {
		t.Fatal("没有 compose 标签时应返回 nil")
	}
}

func TestParseInspect(t *testing.T) {
	mounts := `[{"Type":"volume","Name":"shopdata","Source":"/var/lib/docker/volumes/shopdata/_data","Destination":"/data"}]`
	line := strings.Join([]string{
		testID, "2026-10-21T10:00:00.123456789Z", "running", "2026-10-21T10:00:05.000000000Z",
		"2", "unless-stopped", "false", "bridge", "", "",
		"1500000000", "536870912", mounts, `["NET_ADMIN"]`, "sha256:abc",
		"shop", "web", "/srv/shop", "/srv/shop/compose.yaml", "False", "1",
	}, fs)
	facts, err := parseInspect(line + "\n")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	f, ok := facts[testID]
	if !ok {
		t.Fatal("没有解析出容器")
	}
	if f.State != "running" || f.RestartCount != 2 || f.NanoCPUs != 1500000000 || f.Memory != 536870912 {
		t.Errorf("facts = %+v", f)
	}
	if len(f.Mounts) != 1 || f.Mounts[0].Name != "shopdata" {
		t.Errorf("挂载 = %+v", f.Mounts)
	}
	if f.Compose == nil || f.Compose.Project != "shop" || f.Compose.Number != 1 {
		t.Errorf("compose = %+v", f.Compose)
	}
}

func TestParseInspectMissingLabelPlaceholder(t *testing.T) {
	line := strings.Join([]string{
		testID, "2026-10-21T10:00:00Z", "exited", "0001-01-01T00:00:00Z",
		"0", "no", "false", "bridge", "", "",
		"0", "0", "[]", "null", "sha256:abc",
		"<no value>", "<no value>", "<no value>", "<no value>", "<no value>", "<no value>",
	}, fs)
	facts, err := parseInspect(line + "\n")
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if facts[testID].Compose != nil {
		t.Fatalf("缺失标签不该被当成项目名：%+v", facts[testID].Compose)
	}
	if facts[testID].CapAdd != nil {
		t.Fatalf("null 的 capability 列表应保持为空")
	}
}

func TestAssessRisk(t *testing.T) {
	cases := []struct {
		name string
		in   InspectFacts
		want string
		code string
	}{
		{name: "普通容器", in: InspectFacts{NetworkMode: "bridge", PidMode: ""}, want: RiskNone},
		{name: "privileged", in: InspectFacts{Privileged: true}, want: RiskHigh, code: RiskPrivileged},
		{name: "宿主网络", in: InspectFacts{NetworkMode: "host"}, want: RiskHigh, code: RiskHostNetwork},
		{name: "宿主 PID", in: InspectFacts{PidMode: "host"}, want: RiskHigh, code: RiskHostPID},
		{name: "宿主 IPC", in: InspectFacts{IpcMode: "host"}, want: RiskHigh, code: RiskHostIPC},
		{name: "危险 capability（带前缀）", in: InspectFacts{CapAdd: []string{"CAP_SYS_ADMIN"}}, want: RiskHigh, code: RiskDangerousCap},
		{name: "危险 capability（不带前缀）", in: InspectFacts{CapAdd: []string{"SYS_PTRACE"}}, want: RiskHigh, code: RiskDangerousCap},
		{name: "挂了 docker.sock", in: InspectFacts{Mounts: []Mount{{
			Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}}}, want: RiskHigh, code: RiskDockerSock},
		{name: "挂了宿主根目录", in: InspectFacts{Mounts: []Mount{{
			Type: "bind", Source: "/", Destination: "/host"}}}, want: RiskHigh, code: RiskHostRootMount},
		{name: "普通 bind 挂载不算危险", in: InspectFacts{Mounts: []Mount{{
			Type: "bind", Source: "/srv/data", Destination: "/data"}}}, want: RiskNone},
		{name: "卷挂载不算危险", in: InspectFacts{Mounts: []Mount{{
			Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Destination: "/data"}}}, want: RiskNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AssessRisk(tc.in)
			if got.Level != tc.want {
				t.Fatalf("level = %q，期望 %q（%+v）", got.Level, tc.want, got.Reasons)
			}
			if tc.code == "" {
				return
			}
			found := false
			for _, r := range got.Reasons {
				if r.Code == tc.code {
					found = true
					if strings.TrimSpace(r.Message) == "" {
						t.Errorf("原因 %s 没有说明", tc.code)
					}
				}
			}
			if !found {
				t.Fatalf("没有给出原因 %s：%+v", tc.code, got.Reasons)
			}
		})
	}
}

func TestControlCapabilities(t *testing.T) {
	running := Container{State: StateRunning, Risk: Risk{Level: RiskNone}}
	if got := running.ControlCapabilities(); !got.Managed || got.Start || !got.Stop || !got.Restart {
		t.Fatalf("running = %+v", got)
	}
	stopped := Container{State: StateStopped, Risk: Risk{Level: RiskNone}}
	if got := stopped.ControlCapabilities(); !got.Managed || !got.Start || got.Stop || !got.Restart {
		t.Fatalf("stopped = %+v", got)
	}
	unknown := Container{State: StateUnknown, Risk: Risk{Level: RiskNone}}
	if got := unknown.ControlCapabilities(); !got.Managed || got.Start || got.Stop || got.Restart {
		t.Fatalf("unknown = %+v", got)
	}
	// 危险容器：三项全部 false，且不是“管理着但都点不动”。
	risky := Container{State: StateRunning, Risk: Risk{Level: RiskHigh}}
	if got := risky.ControlCapabilities(); got.Managed || got.Start || got.Stop || got.Restart {
		t.Fatalf("高风险容器 = %+v", got)
	}
}

func TestOperationAllowed(t *testing.T) {
	cases := []struct {
		state, op string
		want      bool
	}{
		{StateRunning, OpStart, false},
		{StateRunning, OpStop, true},
		{StateRunning, OpRestart, true},
		{StateStopped, OpStart, true},
		{StateStopped, OpStop, false},
		{StateStopped, OpRestart, true},
		{StateUnknown, OpStart, false},
		{StateUnknown, OpStop, false},
		{StateUnknown, OpRestart, false},
	}
	for _, tc := range cases {
		if got := OperationAllowed(tc.state, tc.op); got != tc.want {
			t.Errorf("%s %s = %v，期望 %v", tc.state, tc.op, got, tc.want)
		}
	}
}

func TestMapState(t *testing.T) {
	cases := map[string]string{
		"running":    StateRunning,
		"exited":     StateStopped,
		"created":    StateStopped,
		"dead":       StateStopped,
		"paused":     StateUnknown,
		"restarting": StateUnknown,
		"":           StateUnknown,
	}
	for in, want := range cases {
		if got := MapState(in); got != want {
			t.Errorf("MapState(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestParseLogOutput(t *testing.T) {
	params := LogParams{ContainerID: testID, Tail: 3}
	out := strings.Join([]string{
		"2026-10-22T10:00:00.000000001Z first",
		"2026-10-22T10:00:01.000000001Z second",
		"2026-10-22T10:00:02.000000001Z third",
	}, "\n")
	entries, lines, truncated := parseLogOutput(out, params)
	if lines != 3 {
		t.Fatalf("lines = %d，期望 3", lines)
	}
	if truncated {
		t.Fatal("三行没超上限，不该标截断")
	}
	if len(entries) != 3 {
		t.Fatalf("条数 = %d", len(entries))
	}
	// 从新到旧。
	if entries[0].Message != "third" || entries[2].Message != "first" {
		t.Fatalf("顺序 = %+v", entries)
	}
}

func TestParseLogOutputDedupesCursorBoundary(t *testing.T) {
	params := LogParams{ContainerID: testID, Tail: 10, Cursor: "2026-10-22T10:00:01.000000001Z"}
	out := strings.Join([]string{
		"2026-10-22T10:00:00.000000001Z older",
		"2026-10-22T10:00:01.000000001Z boundary",
	}, "\n")
	entries, _, _ := parseLogOutput(out, params)
	if len(entries) != 1 || entries[0].Message != "older" {
		t.Fatalf("游标边界应被去重，实际 %+v", entries)
	}
}

func TestParseLogOutputMarksTruncatedLines(t *testing.T) {
	long := strings.Repeat("x", MaxLogLineBytes+10)
	params := LogParams{ContainerID: testID, Tail: 10}
	entries, _, _ := parseLogOutput("2026-10-22T10:00:00Z "+long+"\n", params)
	if len(entries) != 1 || !entries[0].Truncated {
		t.Fatalf("超长行应标记截断：%+v", entries)
	}
	if len(entries[0].Message) > MaxLogLineBytes+32 {
		t.Fatalf("截断后仍然过长：%d 字节", len(entries[0].Message))
	}
}

func TestParseLogOutputRedactsSecrets(t *testing.T) {
	params := LogParams{ContainerID: testID, Tail: 10}
	entries, _, _ := parseLogOutput(
		"2026-10-22T10:00:00Z connection to db with password=hunter2thing\n", params)
	if len(entries) != 1 {
		t.Fatalf("条数 = %d", len(entries))
	}
	if strings.Contains(entries[0].Message, "hunter2thing") || !entries[0].Redacted {
		t.Fatalf("密码没有被抹掉：%q（redacted=%v）", entries[0].Message, entries[0].Redacted)
	}
}

func TestLogParamsValidate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ok := []LogParams{
		{ContainerID: testID},
		{ContainerID: testID, Tail: 1},
		{ContainerID: testID, Tail: MaxLogLines},
		{ContainerID: testID, Since: "2026-10-22T10:00:00Z", Until: "2026-10-22T11:00:00Z"},
		{ContainerID: testID, Cursor: "2026-10-22T10:00:00.123456789Z"},
	}
	for _, p := range ok {
		if err := p.Validate(now); err != nil {
			t.Errorf("应当通过：%+v → %v", p, err)
		}
		if p.Tail == 0 {
			t.Errorf("Tail 应补默认值：%+v", p)
		}
	}
	bad := []LogParams{
		{ContainerID: "abc"},
		{ContainerID: testID, Tail: -1},
		{ContainerID: testID, Tail: MaxLogLines + 1},
		{ContainerID: testID, Since: "2026-10-22"},
		{ContainerID: testID, Until: "2026-10-22T13:00:00Z"},
		{ContainerID: testID, Since: "2026-10-22T11:00:00Z", Until: "2026-10-22T10:00:00Z"},
		{ContainerID: testID, Cursor: "not-a-time"},
	}
	for _, p := range bad {
		if err := p.Validate(now); err == nil {
			t.Errorf("应当被拒绝：%+v", p)
		}
	}
}

func TestProjectIDSplitsByName(t *testing.T) {
	a := ProjectID("shop")
	b := ProjectID("shop")
	c := ProjectID("blog")
	if a != b {
		t.Fatal("同一个项目名必须得到同一个 id")
	}
	if a == c {
		t.Fatal("不同项目名不该撞 id")
	}
	if !strings.HasPrefix(a, "dep_") || len(a) != len("dep_")+32 {
		t.Fatalf("id 形状不对：%q", a)
	}
}

func TestControlParamsNormalize(t *testing.T) {
	p := ControlParams{ContainerID: "  " + testID + " ", Operation: " START "}
	if err := p.Normalize(); err != nil {
		t.Fatalf("规范化失败：%v", err)
	}
	if p.ContainerID != testID || p.Operation != OpStart {
		t.Fatalf("规范化结果 = %+v", p)
	}
	for _, bad := range []ControlParams{
		{ContainerID: "abc", Operation: OpStart},
		{ContainerID: testID, Operation: "kill"},
		{ContainerID: testID, Operation: ""},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("应当被拒绝：%+v", bad)
		}
	}
}

func TestValidContainerIDRejectsShortAndNames(t *testing.T) {
	for _, id := range []string{"3f1a2b3c4d5e", "web", testID + "a", strings.ToUpper(testID), ""} {
		if ValidContainerID(id) {
			t.Errorf("%q 不该被当成完整容器 ID", id)
		}
	}
	if !ValidContainerID(testID) {
		t.Fatal("合法 ID 被拒绝")
	}
}

// TestAssessControlWithoutDockerIsBlocked 固定“Docker 不可用”的表达方式：
// 它是 Allowed=false + 阻塞码 DOCKER_UNAVAILABLE，而不是一个 error。
// 预检层把 Plan 的 error 当成实现失败（500），所以这里返回 error 会让
// “本机没装 docker”变成一句查不出原因的 500。
func TestAssessControlWithoutDockerIsBlocked(t *testing.T) {
	client := &Client{Program: "/usr/bin/docker", Socket: "/nonexistent/upanel-test.sock"}
	assessment, err := client.AssessControl(context.Background(),
		ControlParams{ContainerID: testID, Operation: OpStop})
	if err != nil {
		t.Fatalf("Docker 不可用时不该返回 error：%v", err)
	}
	if assessment.Allowed {
		t.Fatal("Docker 不可用时必须拒绝")
	}
	if len(assessment.Blockers) != 1 || assessment.Blockers[0].Code != BlockUnavailable {
		t.Fatalf("阻塞项 = %+v，期望一条 %s", assessment.Blockers, BlockUnavailable)
	}
	if !strings.Contains(assessment.Blockers[0].Message, "Docker") {
		t.Errorf("说明里应当指出是 Docker 的问题：%q", assessment.Blockers[0].Message)
	}
}

// TestInspectFormatHasNoTemplateMethodCalls 钉住一条真机上学到的边界：
// docker 29 的 `inspect --format` 把模板数据当成 interface{}，模板里任何方法调用
// （例如 `.State.StartedAt.Format "..."`）都会报
// “can't evaluate field Format in type interface {}”，整份容器清单读不出来。
// 验收 VM 上第一遍就是这么挂的，所以这里用一条静态断言把它钉死。
func TestInspectFormatHasNoTemplateMethodCalls(t *testing.T) {
	for _, banned := range []string{".Format", "printf", "time."} {
		if strings.Contains(inspectFormat, banned) {
			t.Fatalf("inspect 模板里不该出现 %q：%s", banned, inspectFormat)
		}
	}
	// 该取的固定字段一个都不能少：少一个就会出现“某个字段永远是空”的假数据。
	for _, want := range []string{
		"{{.Id}}", "{{.State.Status}}", "{{.State.StartedAt}}", "{{.RestartCount}}",
		"{{.HostConfig.Privileged}}", "{{.HostConfig.NetworkMode}}", "{{.HostConfig.PidMode}}",
		"{{json .Mounts}}", "{{json .HostConfig.CapAdd}}",
		`{{index .Config.Labels "com.docker.compose.project"}}`,
	} {
		if !strings.Contains(inspectFormat, want) {
			t.Errorf("inspect 模板缺少 %s", want)
		}
	}
	// 环境变量与完整标签映射绝不能出现在格式串里：不请求就不会泄漏。
	if strings.Contains(inspectFormat, "Config.Env") {
		t.Fatal("inspect 模板不该请求 Config.Env")
	}
}

// fakeDocker 记录每次 docker 调用的 argv，并按顺序回放固定结果。
//
// 翻页要按窗口反复试，只有能看见每次的 argv，才能证明“窗口在加宽/收窄”是真的，
// 而不是碰巧拿到一页就当成对了。
type fakeDocker struct {
	calls   [][]string
	replies []syscmd.Result
}

func (f *fakeDocker) exec(_ context.Context, _ time.Duration, args ...string) (syscmd.Result, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(f.replies) == 0 {
		return syscmd.Result{}, nil
	}
	res := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return res, nil
}

// newFakeClient 造一个“docker 存在、套接字在、执行被替换”的客户端。
func newFakeClient(f *fakeDocker) *Client {
	return &Client{
		Program: "/usr/bin/docker",
		Socket:  "/nonexistent/upanel-test.sock",
		Stat:    func(string) (os.FileInfo, error) { return nil, nil },
		Exec:    f.exec,
	}
}

// argAfter 返回 --flag 后面的值。
func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	t.Fatalf("argv 里没有 %s：%v", flag, args)
	return ""
}

// hasArg 判断 argv 里有没有某个开关。
func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestLogsOlderPageUsesWindowWithoutTail 往回翻页只能用时间窗。
//
// docker 的 `--tail` 是“整个流的最后 N 行”，而 `--until` 在 tail 之后再过滤：
// 两个开关一起用会把比游标早的行全丢掉（真机第二页只剩边界那一条，去重后变成空页）。
func TestLogsOlderPageUsesWindowWithoutTail(t *testing.T) {
	f := &fakeDocker{replies: []syscmd.Result{
		{Stdout: "2026-10-22T19:59:00.000000000Z\n"}, // inspect {{.Created}}
		{Stdout: "2026-10-22T19:59:58.000000000Z line-1\n" +
			"2026-10-22T19:59:59.000000000Z line-2\n"},
	}}
	c := newFakeClient(f)
	params := LogParams{ContainerID: testID, Tail: 5, Cursor: "2026-10-22T20:00:00.000000000Z"}
	if err := params.Validate(time.Date(2026, 10, 22, 20, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	page, err := c.Logs(context.Background(), params)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("docker 调用次数 = %d，期望 2（一次 inspect 取创建时间、一次 logs）", len(f.calls))
	}
	if !hasArg(f.calls[0], "inspect") {
		t.Fatalf("第一次调用应当是 inspect：%v", f.calls[0])
	}
	logsArgs := f.calls[1]
	if hasArg(logsArgs, "--tail") {
		t.Fatalf("往回翻页的 argv 里不能有 --tail：%v", logsArgs)
	}
	if got := argAfter(t, logsArgs, "--until"); got != "2026-10-22T20:00:00Z" {
		t.Errorf("--until = %q，期望游标时间", got)
	}
	if since := argAfter(t, logsArgs, "--since"); since >= "2026-10-22T20:00:00Z" {
		t.Errorf("--since = %q，必须早于游标", since)
	}
	if len(page.Entries) != 2 || page.Entries[0].Message != "line-2" || page.Entries[1].Message != "line-1" {
		t.Fatalf("条目 = %+v，期望从新到旧的两条", page.Entries)
	}
	if page.Truncated {
		t.Error("窗口已覆盖到容器创建时间，不该说“更早的还有”")
	}
}

// TestLogsOlderPageWidensWhenWindowIsTooQuiet 安静的容器：窗口里没有行就把窗口放大。
func TestLogsOlderPageWidensWhenWindowIsTooQuiet(t *testing.T) {
	f := &fakeDocker{replies: []syscmd.Result{
		{Stdout: "2026-10-22T10:00:00.000000000Z\n"}, // 容器创建得很早
		{Stdout: ""}, // 15 分钟窗口内没有日志
		{Stdout: "2026-10-22T19:00:00.000000000Z old-line\n"},
	}}
	c := newFakeClient(f)
	params := LogParams{ContainerID: testID, Tail: 5, Cursor: "2026-10-22T20:00:00.000000000Z"}
	if err := params.Validate(time.Date(2026, 10, 22, 20, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	page, err := c.Logs(context.Background(), params)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(f.calls) != 4 {
		// inspect → 15 分钟窗口（空）→ 2 小时窗口（一行，仍不满一页）→ 16 小时窗口（覆盖容器创建）。
		t.Fatalf("docker 调用次数 = %d，期望 4", len(f.calls))
	}
	first := argAfter(t, f.calls[1], "--since")
	second := argAfter(t, f.calls[2], "--since")
	if second >= first {
		t.Fatalf("第二次 --since = %q 不比第一次 %q 更早：窗口没有加宽", second, first)
	}
	if len(page.Entries) != 1 || page.Entries[0].Message != "old-line" {
		t.Fatalf("条目 = %+v", page.Entries)
	}
}

// TestLogsOlderPageShrinksWhenOutputTruncated 话多的容器：输出撞上限时窗口要收窄，
// 因为被丢掉的是窗口末尾（最接近游标）的行——缺了它们的页没有意义。
func TestLogsOlderPageShrinksWhenOutputTruncated(t *testing.T) {
	f := &fakeDocker{replies: []syscmd.Result{
		{Stdout: "2026-10-22T10:00:00.000000000Z\n"},
		{Stdout: "…", StdoutTruncated: true},
		{Stdout: "2026-10-22T19:59:59.000000000Z line-2\n2026-10-22T19:59:59.500000000Z line-1\n"},
	}}
	c := newFakeClient(f)
	params := LogParams{ContainerID: testID, Tail: 2, Cursor: "2026-10-22T20:00:00.000000000Z"}
	if err := params.Validate(time.Date(2026, 10, 22, 20, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	page, err := c.Logs(context.Background(), params)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	first := argAfter(t, f.calls[1], "--since")
	second := argAfter(t, f.calls[2], "--since")
	if second <= first {
		t.Fatalf("第二次 --since = %q 不比第一次 %q 更晚：窗口没有收窄", second, first)
	}
	if len(page.Entries) != 2 || !page.Truncated {
		t.Fatalf("条目 = %+v，truncated = %v；满一页时应当标 truncated", page.Entries, page.Truncated)
	}
}

// TestParseLogOutputCapsAtTail 往回翻页时一页可能捞到几千行，必须按 tail 截住。
func TestParseLogOutputCapsAtTail(t *testing.T) {
	lines := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		lines = append(lines, "2026-10-22T10:00:0"+string(rune('0'+i))+"Z line-"+string(rune('0'+i)))
	}
	out := strings.Join(lines, "\n")
	params := LogParams{ContainerID: testID, Tail: 3}
	entries, total, truncated := parseLogOutput(out, params)
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(entries))
	}
	if total != 10 {
		t.Fatalf("总行数 = %d，期望 10", total)
	}
	if !truncated {
		t.Error("被 tail 截住时必须标 truncated")
	}
	if entries[0].Message != "line-9" {
		t.Fatalf("第一条 = %q，期望最新的 line-9", entries[0].Message)
	}
}

// TestDaemonDownIsUnavailable 套接字在、守护进程死了：这是 DOCKER_UNAVAILABLE，
// 不是“docker ps 失败了”。两者的处置完全不同（去起 Docker vs 去查面板）。
func TestDaemonDownIsUnavailable(t *testing.T) {
	res := syscmd.Result{
		ExitCode: 1,
		Stderr:   "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
	}
	err := dockerFailure("docker ps", res)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("错误 = %v，期望 UnavailableError", err)
	}
	if !strings.Contains(unavailable.Notice, "Docker") {
		t.Errorf("说明文字 = %q，应当说清是 Docker 的问题", unavailable.Notice)
	}
}

// TestListDegradesWhenDaemonDies 清单接口在守护进程半路死掉时降级为
// 200 + available=false + notice，而不是 503。
func TestListDegradesWhenDaemonDies(t *testing.T) {
	f := &fakeDocker{replies: []syscmd.Result{{
		ExitCode: 1,
		Stderr:   "cannot connect to the Docker daemon at unix:///var/run/docker.sock: connect: connection refused",
	}}}
	c := newFakeClient(f)
	resp, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List 返回错误 %v；清单接口应当降级而不是报错", err)
	}
	if resp.Available {
		t.Error("available 必须是 false")
	}
	if resp.Notice == "" {
		t.Error("降级时必须给出 notice，不能只说“没有容器”")
	}
	if len(resp.Containers) != 0 {
		t.Errorf("降级时不该编出容器：%+v", resp.Containers)
	}
}

// TestAssessControlDaemonDiesMidwayIsBlocked 预检途中守护进程死掉：
// 仍然是 allowed=false + DOCKER_UNAVAILABLE，而不是一个 500。
func TestAssessControlDaemonDiesMidwayIsBlocked(t *testing.T) {
	f := &fakeDocker{replies: []syscmd.Result{{
		ExitCode: 1,
		Stderr:   "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
	}}}
	c := newFakeClient(f)
	assessment, err := AssessControl(context.Background(), c,
		ControlParams{ContainerID: testID, Operation: OpStop})
	if err != nil {
		t.Fatalf("AssessControl 返回错误 %v；这应当是阻塞项而不是实现失败", err)
	}
	if assessment.Allowed {
		t.Error("allowed 必须是 false")
	}
	if len(assessment.Blockers) != 1 || assessment.Blockers[0].Code != BlockUnavailable {
		t.Fatalf("阻塞项 = %+v，期望一条 %s", assessment.Blockers, BlockUnavailable)
	}
}
