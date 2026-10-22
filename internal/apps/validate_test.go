package apps

import (
	"errors"
	"strings"
	"testing"
)

// 测试用的形状合法的标识。
const (
	testContainerID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDeploymentID = "dep_0123456789abcdef0123456789abcdef"
	testVolumeID     = "vol_0123456789abcdef0123456789abcdef"
)

// validDefinition 返回一份按类型可用的元数据。
func validDefinition(t Type) Definition {
	def := Definition{
		Name:        "示例应用",
		Description: "说明",
		IconKey:     "app",
		Type:        t,
	}
	switch t {
	case TypeLink:
		def.PublicURL = "https://example.com/app"
	case TypeSystemd:
		def.Binding = Binding{Unit: "nginx.service"}
	case TypeDocker:
		def.Binding = Binding{ContainerID: testContainerID}
	case TypeCompose:
		def.Binding = Binding{DeploymentID: testDeploymentID}
	}
	return def
}

// TestNormalizeAcceptsFourTypes 确认四种类型都能过一遍规范化，并给出预期的形态。
func TestNormalizeAcceptsFourTypes(t *testing.T) {
	for _, typ := range []Type{TypeLink, TypeSystemd, TypeDocker, TypeCompose} {
		t.Run(string(typ), func(t *testing.T) {
			got, err := validDefinition(typ).Normalize(NormalizeOptions{})
			if err != nil {
				t.Fatalf("规范化失败: %v", err)
			}
			if got.Type != typ {
				t.Fatalf("类型 = %q，期望 %q", got.Type, typ)
			}
			// 未启用健康检查时，区间与超时也要填上默认值：页面上显示的
			// “30 秒 / 3 秒”必须是真实会用到的值，不能是空白。
			if got.Health.IntervalSeconds != DefaultIntervalSeconds {
				t.Fatalf("默认间隔 = %d", got.Health.IntervalSeconds)
			}
			if got.Health.TimeoutSeconds != DefaultTimeoutSeconds {
				t.Fatalf("默认超时 = %d", got.Health.TimeoutSeconds)
			}
			if got.Health.Scheme != "http" || got.Health.Path != "/" {
				t.Fatalf("默认健康检查 = %+v", got.Health)
			}
		})
	}
}

// TestBindingIsDiscriminatedByType 是判别模型的核心用例：绑定必须与类型严格配对。
func TestBindingIsDiscriminatedByType(t *testing.T) {
	cases := []struct {
		name string
		def  Definition
		want string // 错误信息里应出现的关键词
	}{
		{
			name: "link 带 unit",
			def:  Definition{Name: "x", Type: TypeLink, PublicURL: "https://example.com", Binding: Binding{Unit: "nginx.service"}},
			want: "没有运行绑定",
		},
		{
			name: "systemd 带 container_id",
			def:  Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "nginx.service", ContainerID: testContainerID}},
			want: "只能有 unit",
		},
		{
			name: "docker 带 unit",
			def:  Definition{Name: "x", Type: TypeDocker, Binding: Binding{Unit: "nginx.service", ContainerID: testContainerID}},
			want: "只能有 container_id",
		},
		{
			name: "docker 用名称顶替完整 ID",
			def:  Definition{Name: "x", Type: TypeDocker, Binding: Binding{ContainerID: "my-container"}},
			want: "完整 ID",
		},
		{
			name: "docker 给短 ID",
			def:  Definition{Name: "x", Type: TypeDocker, Binding: Binding{ContainerID: testContainerID[:12]}},
			want: "完整 ID",
		},
		{
			name: "compose 带 container_id",
			def:  Definition{Name: "x", Type: TypeCompose, Binding: Binding{ContainerID: testContainerID, DeploymentID: testDeploymentID}},
			want: "只能有 deployment_id",
		},
		{
			name: "compose 引用外部项目",
			def:  Definition{Name: "x", Type: TypeCompose, Binding: Binding{DeploymentID: "docker-compose"}},
			want: "面板创建的部署标识",
		},
		{
			name: "systemd 不带后缀",
			def:  Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "nginx"}},
			want: "确切的服务单元名",
		},
		{
			name: "systemd 带路径穿越",
			def:  Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "a..service"}},
			want: "确切的服务单元名",
		},
		{
			name: "systemd 是定时器",
			def:  Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "backup.timer"}},
			want: "确切的服务单元名",
		},
		{
			name: "未知类型",
			def:  Definition{Name: "x", Type: Type("k8s")},
			want: "应用类型必须是",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.def.Normalize(NormalizeOptions{})
			if err == nil {
				t.Fatal("期望拒绝，实际通过")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("期望 *ValidationError，实际 %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestContainerIDIsNormalized 确认大写十六进制被归一化成小写（唯一索引依赖规范形态）。
func TestContainerIDIsNormalized(t *testing.T) {
	def := Definition{
		Name:    "x",
		Type:    TypeDocker,
		Binding: Binding{ContainerID: strings.ToUpper(testContainerID)},
	}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if got.Binding.ContainerID != testContainerID {
		t.Fatalf("容器 ID = %q，期望小写形态", got.Binding.ContainerID)
	}
}

// TestPublicURLRules 覆盖浏览器入口地址的边界。
func TestPublicURLRules(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		opts    NormalizeOptions
		wantErr string
	}{
		{name: "https 正常", url: "https://panel.example.com/app", wantErr: ""},
		{name: "http 也允许", url: "http://10.0.0.10:8080/", wantErr: ""},
		{name: "允许带路径与查询", url: "https://example.com/a/b?x=1#frag", wantErr: ""},
		{name: "空表示没有入口", url: "", wantErr: ""},
		{name: "拒绝 javascript", url: "javascript:alert(1)", wantErr: "只接受 http 或 https"},
		{name: "拒绝 data", url: "data:text/html,<b>x</b>", wantErr: "只接受 http 或 https"},
		{name: "拒绝 file", url: "file:///etc/passwd", wantErr: "只接受 http 或 https"},
		{name: "拒绝带凭据", url: "https://user:pass@example.com/", wantErr: "不能包含用户名或密码"},
		{name: "拒绝没有主机", url: "https:///path", wantErr: "缺少主机名"},
		{name: "生产模式拒绝回环", url: "http://127.0.0.1:8080/", wantErr: "本机回环地址"},
		{name: "生产模式拒绝 localhost", url: "http://localhost:3000/", wantErr: "本机回环地址"},
		{name: "生产模式拒绝 IPv6 回环", url: "http://[::1]:3000/", wantErr: "本机回环地址"},
		{name: "生产模式拒绝 0.0.0.0", url: "http://0.0.0.0:3000/", wantErr: "不是浏览器能访问的地址"},
		{
			name: "本机开发模式允许回环",
			url:  "http://127.0.0.1:8080/",
			opts: NormalizeOptions{AllowLoopbackPublicURL: true},
		},
		{name: "拒绝空白字符", url: "https://example.com/a b", wantErr: "空白或控制字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{Name: "x", Type: TypeLink, PublicURL: tc.url}
			// link 必须有入口：空地址要按“必须填写入口”报错，不算 URL 规则失败。
			_, err := def.Normalize(tc.opts)
			if tc.url == "" {
				if err == nil {
					t.Fatal("link 没有入口时必须拒绝")
				}
				if !strings.Contains(err.Error(), "必须填写入口地址") {
					t.Fatalf("错误信息 = %v", err)
				}
				return
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望拒绝（%s），实际通过", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestHealthCheckTargetMustBeDiscoveredID 是本阶段最重要的一条 SSRF 约束：
// 探测目标只能是代理发现的目标标识，主机名/IP/URL 一律写不进去。
func TestHealthCheckTargetMustBeDiscoveredID(t *testing.T) {
	base := Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "nginx.service"}}

	for _, bad := range []string{
		"127.0.0.1",
		"169.254.169.254",
		"localhost:8080",
		"http://127.0.0.1:8080/",
		"local_",      // 前缀后必须有内容
		"local_Upper", // 只允许小写
		"local_0123456789012345678901234567890123456789012345678901", // 过长
		"host_1", // 前缀不在 local/cntr 内
	} {
		def := base
		def.Health = HealthCheck{Enabled: true, TargetID: bad, Port: 80}
		if _, err := def.Normalize(NormalizeOptions{}); err == nil {
			t.Fatalf("目标 %q 必须被拒绝", bad)
		} else if !strings.Contains(err.Error(), "探测目标") {
			t.Fatalf("目标 %q 的错误信息 = %v", bad, err)
		}
	}

	def := base
	def.Health = HealthCheck{Enabled: true, TargetID: "local_3f2a1b", Port: 8080}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("合法目标被拒绝: %v", err)
	}
	if got.Health.TargetID != "local_3f2a1b" {
		t.Fatalf("目标 = %q", got.Health.TargetID)
	}
	// 空集合表示默认 200—399，落库与返回都是 []，不是 null。
	if got.Health.ExpectedStatusCodes != nil {
		t.Fatalf("期望状态码 = %v，期望 nil（默认区间）", got.Health.ExpectedStatusCodes)
	}
}

// TestHealthCheckEnablingRequiresTargetAndPort 确认“启用但没目标”被拒绝。
func TestHealthCheckEnablingRequiresTargetAndPort(t *testing.T) {
	base := Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "nginx.service"}}

	def := base
	def.Health = HealthCheck{Enabled: true, Port: 80}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("启用健康检查但没有目标必须拒绝")
	}

	def = base
	def.Health = HealthCheck{Enabled: true, TargetID: "cntr_abc123"}
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("启用健康检查但没有端口必须拒绝")
	}
}

// TestHealthCheckRanges 覆盖间隔、超时、路径与状态码集合。
func TestHealthCheckRanges(t *testing.T) {
	base := Definition{Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "a.service"}, Health: HealthCheck{Enabled: true, TargetID: "local_ab", Port: 80}}

	cases := []struct {
		name    string
		mutate  func(*HealthCheck)
		wantErr string
	}{
		{"间隔太小", func(h *HealthCheck) { h.IntervalSeconds = 15 }, "探测间隔"},
		{"间隔太大", func(h *HealthCheck) { h.IntervalSeconds = 600 }, "探测间隔"},
		{"超时为 20 秒", func(h *HealthCheck) { h.TimeoutSeconds = 20 }, "超时"},
		{"路径不以斜杠开头", func(h *HealthCheck) { h.Path = "health" }, "以单个 / 开头"},
		{"路径是完整 URL", func(h *HealthCheck) { h.Path = "/x://y" }, "不能是完整 URL"},
		{"协议不是 http", func(h *HealthCheck) { h.Scheme = "tcp" }, "只允许 http 或 https"},
		{"端口越界", func(h *HealthCheck) { h.Port = 70000 }, "端口必须"},
		{"状态码越界", func(h *HealthCheck) { h.ExpectedStatusCodes = []int{200, 42} }, "状态码必须"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := base
			h := base.Health
			tc.mutate(&h)
			def.Health = h
			_, err := def.Normalize(NormalizeOptions{})
			if err == nil {
				t.Fatal("期望拒绝，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), tc.wantErr)
			}
		})
	}

	// 边界组合：间隔取下限 30 秒、超时取上限 10 秒。这一组同时说明“超时必须小于间隔”
	// 在合法区间内恒成立（10 < 30），validate.go 里那条判断因此是防御性的。
	edge := base
	edge.Health.IntervalSeconds = MinIntervalSeconds
	edge.Health.TimeoutSeconds = MaxTimeoutSeconds
	if _, err := edge.Normalize(NormalizeOptions{}); err != nil {
		t.Fatalf("边界组合应通过: %v", err)
	}
}

// TestHealthCheckStatusCodesAreSortedAndDeduped 确认集合被规范化。
func TestHealthCheckStatusCodesAreSortedAndDeduped(t *testing.T) {
	def := Definition{
		Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "a.service"},
		Health: HealthCheck{
			Enabled: true, TargetID: "local_ab", Port: 80,
			ExpectedStatusCodes: []int{404, 200, 404, 200},
		},
	}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if len(got.Health.ExpectedStatusCodes) != 2 ||
		got.Health.ExpectedStatusCodes[0] != 200 || got.Health.ExpectedStatusCodes[1] != 404 {
		t.Fatalf("状态码集合 = %v", got.Health.ExpectedStatusCodes)
	}
}

// TestVolumeDependenciesAreValidatedAndDeduped 确认依赖卷按代理侧的标识形状校验。
func TestVolumeDependenciesAreValidatedAndDeduped(t *testing.T) {
	def := Definition{
		Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "a.service"},
		VolumeDependencies: []string{testVolumeID, " " + testVolumeID + " ", testVolumeID},
	}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if len(got.VolumeDependencies) != 1 || got.VolumeDependencies[0] != testVolumeID {
		t.Fatalf("依赖卷 = %v", got.VolumeDependencies)
	}

	for _, bad := range []string{"", "vol_short", "/dev/sdb1", "vol_0123456789ABCDEF0123456789ABCDEF"} {
		def := Definition{
			Name: "x", Type: TypeSystemd, Binding: Binding{Unit: "a.service"},
			VolumeDependencies: []string{bad},
		}
		if _, err := def.Normalize(NormalizeOptions{}); err == nil {
			t.Fatalf("依赖卷 %q 必须被拒绝", bad)
		}
	}
}

// TestNameAndDescriptionLimits 覆盖名称与说明的边界。
func TestNameAndDescriptionLimits(t *testing.T) {
	base := Definition{Name: "x", Type: TypeLink, PublicURL: "https://example.com"}

	def := base
	def.Name = "   "
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("空名称必须拒绝")
	}

	def = base
	def.Name = strings.Repeat("名", MaxNameLen+1)
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("超长名称必须拒绝")
	}

	def = base
	def.Name = "带\n换行"
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("名称里的换行必须拒绝")
	}

	def = base
	def.Description = strings.Repeat("字", MaxDescriptionLen+1)
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("超长说明必须拒绝")
	}

	def = base
	def.Description = " 多行\n说明\t可以 "
	if _, err := def.Normalize(NormalizeOptions{}); err != nil {
		t.Fatalf("多行说明应被接受: %v", err)
	}
}

// TestIconKeyMustBeBuiltin 确认图标只允许内置预设。
func TestIconKeyMustBeBuiltin(t *testing.T) {
	def := Definition{Name: "x", Type: TypeLink, PublicURL: "https://example.com", IconKey: "Docker"}
	got, err := def.Normalize(NormalizeOptions{})
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if got.IconKey != "docker" {
		t.Fatalf("图标 = %q，期望小写归一", got.IconKey)
	}

	def.IconKey = "https://evil.example.com/x.svg"
	if _, err := def.Normalize(NormalizeOptions{}); err == nil {
		t.Fatal("远程图标地址必须拒绝")
	}
}

// TestBindingJSONIsCanonical 确认落库形态固定，唯一索引才有意义。
func TestBindingJSONIsCanonical(t *testing.T) {
	cases := []struct {
		typ  Type
		bind Binding
		want string
	}{
		{TypeLink, Binding{}, "{}"},
		{TypeSystemd, Binding{Unit: "nginx.service"}, `{"unit":"nginx.service"}`},
		{TypeDocker, Binding{ContainerID: testContainerID}, `{"container_id":"` + testContainerID + `"}`},
		{TypeCompose, Binding{DeploymentID: testDeploymentID}, `{"deployment_id":"` + testDeploymentID + `"}`},
	}
	for _, tc := range cases {
		got, err := bindingJSON(tc.typ, tc.bind)
		if err != nil {
			t.Fatalf("%s: 序列化失败: %v", tc.typ, err)
		}
		if got != tc.want {
			t.Fatalf("%s: 序列化 = %s，期望 %s", tc.typ, got, tc.want)
		}
		back, err := parseBinding(tc.typ, got)
		if err != nil {
			t.Fatalf("%s: 反序列化失败: %v", tc.typ, err)
		}
		if back != tc.bind {
			t.Fatalf("%s: 往返 = %+v，期望 %+v", tc.typ, back, tc.bind)
		}
	}

	// 严格解析：多一个键就是数据损坏。
	if _, err := parseBinding(TypeSystemd, `{"unit":"a.service","container_id":"x"}`); err == nil {
		t.Fatal("多余的键必须报错")
	}
	if _, err := parseBinding(TypeLink, `{"unit":"a.service"}`); err == nil {
		t.Fatal("link 的绑定不是空对象时必须报错")
	}
}

// TestHealthJSONRoundTrip 确认健康检查的落库形态能读回来。
func TestHealthJSONRoundTrip(t *testing.T) {
	in := HealthCheck{
		Enabled: true, TargetID: "local_ab", Scheme: "https", Port: 8443,
		Path: "/healthz", ExpectedStatusCodes: []int{200, 204},
		IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	raw, err := healthJSON(in)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back, err := parseHealth(raw)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back.TargetID != in.TargetID || back.Port != in.Port || back.Scheme != in.Scheme ||
		back.Path != in.Path || back.IntervalSeconds != in.IntervalSeconds ||
		back.TimeoutSeconds != in.TimeoutSeconds || len(back.ExpectedStatusCodes) != 2 {
		t.Fatalf("往返结果 = %+v", back)
	}
	// nil 集合落成 []，读回来是空切片而不是 nil——两者语义相同，写法只有一个。
	if strings.Contains(raw, "null") {
		t.Fatalf("规范 JSON 里不应出现 null: %s", raw)
	}
}
