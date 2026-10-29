package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeResolver 是一个只在内存里放一条秘密的解析器。
//
// 它同时扮演“用途核对”的角色：解出来的每一次都要记下 purpose，测试再断言它只能是
// app_env——秘密顺着 app_env 被塞进别人的容器，是这条路最该防的事。
type fakeResolver struct {
	value    []byte
	err      error
	purposes []string
	refs     []string
}

func (f *fakeResolver) Resolve(_ context.Context, ref string, purpose string) ([]byte, error) {
	f.refs = append(f.refs, ref)
	f.purposes = append(f.purposes, purpose)
	if f.err != nil {
		return nil, f.err
	}
	return f.value, nil
}

// goldenDefinition 是渲染用例的基准定义。它只用公开值与一条秘密。
func goldenDefinition() Definition {
	public := "production"
	return Definition{
		Name:            "webapp",
		ImageReference:  "nginx:1.27",
		ContainerPort:   8080,
		HostBindAddress: "127.0.0.1",
		HostPort:        18080,
		Environment: []Env{
			{Name: "APP_MODE", Value: &public},
			{Name: "DB_PASSWORD", SecretRef: testSecretRef},
		},
		Mounts: []Mount{
			{SourceRootID: "data", RelativePath: "webapp/assets", ContainerPath: "/usr/share/nginx/html", ReadOnly: true},
			{SourceRootID: "data", ContainerPath: "/var/cache/nginx"},
		},
		ResourceLimits:     ResourceLimits{MemoryLimitBytes: 512 << 20, CPULimit: "0.5"},
		HealthCheck:        HealthCheck{Enabled: true, Kind: "http", Path: "/healthz"},
		VolumeDependencies: []string{testVolumeID},
	}
}

// goldenConfig 是期望渲染出的**逐字**配置。
//
// 写成逐字比对而不是若干 contains 断言，是因为这份东西要落盘、要被 compose 读：
// 缩进、引号、`$` 转义、`restart: "no"` 的引号，每一处都是功能。改成 contains 之后，
// 一次缩进退格就能悄悄改变 compose 对它的解释。
const goldenConfig = `# 由 uPanel 渲染，请勿手工修改：改这个文件不会回到面板定义，下一次部署会被覆盖。
# 项目 webapp；服务 webapp；容器端口 8080；宿主 127.0.0.1:18080/tcp
services:
  "webapp":
    image: "nginx:1.27"
    restart: "no"
    ports:
      - target: 8080
        published: "18080"
        host_ip: "127.0.0.1"
        protocol: "tcp"
    environment:
      "APP_MODE": "production"
      "DB_PASSWORD": "pa$$$$word"
    volumes:
      - type: "bind"
        source: "/srv/upanel/volumes/media/webapp/assets"
        target: "/usr/share/nginx/html"
        read_only: true
        bind:
          create_host_path: false
      - type: "bind"
        source: "/srv/upanel/volumes/media"
        target: "/var/cache/nginx"
        read_only: false
        bind:
          create_host_path: false
    mem_limit: 536870912
    cpus: 0.5
`

// TestRenderGolden 是这套渲染的唯一权威读法。
func TestRenderGolden(t *testing.T) {
	resolver := &fakeResolver{value: []byte("pa$$word")}
	out, err := Render(context.Background(), goldenDefinition(), RenderEnv{Host: testHostEnv(), Resolver: resolver})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if string(out.Config) != goldenConfig {
		t.Fatalf("渲染结果与期望不一致。\n收到：\n%s\n期望：\n%s", out.Config, goldenConfig)
	}
	if out.ProjectName != "webapp" {
		t.Fatalf("项目名 = %q", out.ProjectName)
	}
	if len(out.SecretRefs) != 1 || out.SecretRefs[0] != testSecretRef {
		t.Fatalf("秘密引用记录了 %v", out.SecretRefs)
	}
	if len(resolver.purposes) != 1 || resolver.purposes[0] != SecretPurposeAppEnv {
		t.Fatalf("解密用途 = %v，期望只有 %s", resolver.purposes, SecretPurposeAppEnv)
	}
	if out.MountedSources["/var/cache/nginx"] != "/srv/upanel/volumes/media" {
		t.Fatalf("挂载来源记录 = %v", out.MountedSources)
	}
	if !strings.HasPrefix(out.ConfigDigest, "sha256:") || len(out.ConfigDigest) != len("sha256:")+64 {
		t.Fatalf("配置摘要形状不对：%q", out.ConfigDigest)
	}
}

// TestRenderIsDeterministic 确认渲染结果只取决于定义的**内容**，不取决于提交顺序。
func TestRenderIsDeterministic(t *testing.T) {
	a := goldenDefinition()
	b := goldenDefinition()
	// 把两份列表倒过来：规范化会排序，因此渲染必须一模一样。
	b.Environment[0], b.Environment[1] = b.Environment[1], b.Environment[0]
	b.Mounts[0], b.Mounts[1] = b.Mounts[1], b.Mounts[0]

	resolver := &fakeResolver{value: []byte("pa$$word")}
	first, err := Render(context.Background(), a, RenderEnv{Host: testHostEnv(), Resolver: resolver})
	if err != nil {
		t.Fatalf("第一次渲染失败: %v", err)
	}
	second, err := Render(context.Background(), b, RenderEnv{Host: testHostEnv(), Resolver: resolver})
	if err != nil {
		t.Fatalf("第二次渲染失败: %v", err)
	}
	if string(first.Config) != string(second.Config) {
		t.Fatalf("同一份定义换了个提交顺序就渲染出不同字节：\n%s\n---\n%s", first.Config, second.Config)
	}
	if first.ConfigDigest != second.ConfigDigest {
		t.Fatalf("配置摘要不一致：%s vs %s", first.ConfigDigest, second.ConfigDigest)
	}
}

// TestRenderDigestChangesWithContent 确认摘要真的跟着内容走（不是常量）。
func TestRenderDigestChangesWithContent(t *testing.T) {
	base, err := Render(context.Background(), goldenDefinition(),
		RenderEnv{Host: testHostEnv(), Resolver: &fakeResolver{value: []byte("pa$$word")}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	changed := goldenDefinition()
	changed.ResourceLimits.MemoryLimitBytes = 1024 << 20
	other, err := Render(context.Background(), changed,
		RenderEnv{Host: testHostEnv(), Resolver: &fakeResolver{value: []byte("pa$$word")}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if base.ConfigDigest == other.ConfigDigest {
		t.Fatal("改了内存上限，配置摘要却没变")
	}
}

// TestRenderSecretFailureIsFatal 确认秘密读不到时整份配置渲染不出来。
//
// 这一条的失败模式最隐蔽：如果这里退回成空串，容器会带着一个空口令正常起来，
// 面板显示“部署成功”，然后数据库报一个查不出原因的认证错误。
func TestRenderSecretFailureIsFatal(t *testing.T) {
	t.Run("解析器报错", func(t *testing.T) {
		resolver := &fakeResolver{err: errors.New("秘密已销毁")}
		_, err := Render(context.Background(), goldenDefinition(),
			RenderEnv{Host: testHostEnv(), Resolver: resolver})
		if !errors.Is(err, ErrSecretUnavailable) {
			t.Fatalf("期望 ErrSecretUnavailable，实际 %v", err)
		}
	})
	t.Run("没有解析器", func(t *testing.T) {
		_, err := Render(context.Background(), goldenDefinition(), RenderEnv{Host: testHostEnv()})
		if !errors.Is(err, ErrSecretUnavailable) {
			t.Fatalf("期望 ErrSecretUnavailable，实际 %v", err)
		}
	})
	t.Run("秘密里带 NUL", func(t *testing.T) {
		resolver := &fakeResolver{value: []byte("a\x00b")}
		_, err := Render(context.Background(), goldenDefinition(),
			RenderEnv{Host: testHostEnv(), Resolver: resolver})
		if !errors.Is(err, ErrSecretUnavailable) {
			t.Fatalf("期望 ErrSecretUnavailable，实际 %v", err)
		}
	})
}

// TestRenderContainsNoDangerousKeys 确认渲染结果里不存在那些“写进来就等于交出宿主”的键。
//
// 它们写不出来是因为结构体里没有字段；这条断言守的是“以后有人加回去”那一天。
func TestRenderContainsNoDangerousKeys(t *testing.T) {
	out, err := Render(context.Background(), goldenDefinition(),
		RenderEnv{Host: testHostEnv(), Resolver: &fakeResolver{value: []byte("x")}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	config := string(out.Config)
	for _, forbidden := range []string{
		"privileged", "devices:", "network_mode", "cap_add", "pid:", "ipc:",
		"security_opt", "userns_mode", "docker.sock", "build:", "env_file",
		"extends", "restart: always", "restart: unless-stopped", "restart: on-failure",
		"/etc", "/root", "/proc", "/sys", "/dev",
	} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("渲染结果里出现了 %q：\n%s", forbidden, config)
		}
	}
	// restart 必须是带引号的字符串 "no"：裸 no 在 YAML 1.1 里是布尔 false。
	if !strings.Contains(config, `    restart: "no"`) {
		t.Fatalf("restart 没有渲染成带引号的 \"no\"：\n%s", config)
	}
}

// TestRenderEscapesComposeInterpolation 单独钉住 `$` 的处理，因为它最容易在
// “只是加了点提示文案”的改动里被破坏。
func TestRenderEscapesComposeInterpolation(t *testing.T) {
	public := "${HOME}"
	d := goldenDefinition()
	d.Environment = []Env{{Name: "TRICKY", Value: &public}}
	out, err := Render(context.Background(), d, RenderEnv{Host: testHostEnv()})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(string(out.Config), `"TRICKY": "$${HOME}"`) {
		t.Fatalf("环境变量里的 $ 没有转义成 $$，compose 会去宿主环境里取值：\n%s", out.Config)
	}
}

// TestRenderRejectsHostFactsMissing 确认渲染会先过一遍宿主事实——渲染不是“只要形状对
// 就能出字节”。
func TestRenderRejectsHostFactsMissing(t *testing.T) {
	env := testHostEnv()
	env.Volumes = nil
	_, err := Render(context.Background(), goldenDefinition(),
		RenderEnv{Host: env, Resolver: &fakeResolver{value: []byte("x")}})
	expectInvalid(t, err, CodeVolumeMissing, "volume_dependencies")
}

// TestRenderHealthCheckNotRendered 确认健康检查只存在于定义里，不渲染成容器内命令。
func TestRenderHealthCheckNotRendered(t *testing.T) {
	out, err := Render(context.Background(), goldenDefinition(),
		RenderEnv{Host: testHostEnv(), Resolver: &fakeResolver{value: []byte("x")}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	for _, forbidden := range []string{"healthcheck", "CMD-SHELL", "curl", "wget"} {
		if strings.Contains(string(out.Config), forbidden) {
			t.Fatalf("健康检查被渲染进了容器（%q）：\n%s", forbidden, out.Config)
		}
	}
}
