package deploy

import (
	"errors"
	"net"
	"strings"
	"testing"
)

const (
	testSecretRef     = "sec_0123456789abcdef0123456789abcdef"
	testVolumeID      = "vol_0123456789abcdef0123456789abcdef"
	testOtherVolumeID = "vol_ffffffffffffffffffffffffffffffff"
	// 两个 UUID 都写成真 8-4-4-4-12 形态：启动前核对比的是字符串，
	// 用 "uuid-1" 这种假值试不出“拿登记值和实际值比”到底有没有发生。
	testVolumeUUID      = "4f2a1c3e-9b7d-4a51-8e02-1d6c5b3a9f10"
	testOtherVolumeUUID = "b81d0e77-2c46-4f9a-93d5-7a0e14c2b8f6"
	testDigest          = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// validDefinition 返回一份 **能过全部形状校验** 的定义。每个用例只改一处，
// 这样失败时“改的不是这一处”这件事本身就看得见。
func validDefinition() Definition {
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

// testHostEnv 是一套“这台机器上确实有这些东西”的事实。
func testHostEnv() HostEnv {
	return HostEnv{
		LocalAddresses: []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")},
		Roots: map[string]RootFacts{
			"data": {Path: "/srv/upanel/volumes/media", Writable: true, VolumeID: testVolumeID},
			// 与 data 同一个卷里的只读根：挂载必须落在卷的挂载点之内，
			// 所以它是 media 卷下的一个子目录，而不是另一个顶层目录。
			"backup": {Path: "/srv/upanel/volumes/media/backup", Writable: false, VolumeID: testVolumeID},
			// 一个**不该**被当成挂载来源的根：它在部署数据根之外。
			"etc": {Path: "/etc", Writable: true, VolumeID: testVolumeID},
			// 一个落在部署数据根里、但不在任何受管理卷中的根：门禁查不到它。
			"loose": {Path: "/srv/upanel/volumes/loose", Writable: true},
			// 一个在别的卷里的根。
			"other": {Path: "/srv/upanel/volumes/other", Writable: true, VolumeID: testOtherVolumeID},
		},
		DataBase: "/srv/upanel/volumes",
		Volumes: map[string]VolumeFacts{
			testVolumeID: {
				MountPoint:     "/srv/upanel/volumes/media",
				FilesystemUUID: testVolumeUUID,
				Mounted:        true,
				MountedUUID:    testVolumeUUID,
			},
			testOtherVolumeID: {
				MountPoint:     "/srv/upanel/volumes/other",
				FilesystemUUID: testOtherVolumeUUID,
				Mounted:        true,
				MountedUUID:    testOtherVolumeUUID,
			},
		},
	}
}

// expectInvalid 断言错误是 ValidationError 且 code/field 对得上。
func expectInvalid(t *testing.T, err error, wantCode, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒（%s %s），实际通过了", wantCode, wantField)
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("期望 ValidationError，实际是 %T：%v", err, err)
	}
	if verr.Code != wantCode || verr.Field != wantField {
		t.Fatalf("拒因 = %s/%s，期望 %s/%s（消息：%s）", verr.Code, verr.Field, wantCode, wantField, verr.Message)
	}
	if verr.Message == "" {
		t.Fatal("拒因必须带一句人能照着改的中文说明")
	}
}

// TestNormalizeAcceptsValidDefinition 先钉住“正常的能过”，否则后面每一条拒绝都可能是
// 因为定义本身早就不合法。
func TestNormalizeAcceptsValidDefinition(t *testing.T) {
	got, err := Normalize(validDefinition())
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if got.ImageReference != "nginx:1.27" {
		t.Fatalf("镜像引用 = %q", got.ImageReference)
	}
	// 健康检查补默认值，并且端口落在容器端口上。
	if got.HealthCheck.Port != 8080 || got.HealthCheck.IntervalSeconds != DefaultHealthIntervalSeconds ||
		got.HealthCheck.TimeoutSeconds != DefaultHealthTimeoutSeconds || got.HealthCheck.Retries != DefaultHealthRetries {
		t.Fatalf("健康检查默认值 = %+v", got.HealthCheck)
	}
	// 环境变量按名字排序、挂载按容器路径排序：渲染的确定性从这里开始。
	if got.Environment[0].Name != "APP_MODE" || got.Environment[1].Name != "DB_PASSWORD" {
		t.Fatalf("环境变量没有按名字排序: %+v", got.Environment)
	}
	if got.Mounts[0].ContainerPath != "/usr/share/nginx/html" || got.Mounts[1].ContainerPath != "/var/cache/nginx" {
		t.Fatalf("挂载没有按容器路径排序: %+v", got.Mounts)
	}
	// 相对路径归一化成 "." 表示根自身。
	if got.Mounts[1].RelativePath != "." {
		t.Fatalf("空相对路径应归一化成 \".\"，实际 %q", got.Mounts[1].RelativePath)
	}
}

// TestNormalizeRejects 逐条钉住形状层面的拒绝，包括 STAGES.md §17 点名的那串恶意输入。
func TestNormalizeRejects(t *testing.T) {
	empty := ""
	cases := []struct {
		name   string
		mutate func(d *Definition)
		code   string
		field  string
	}{
		{"名字为空", func(d *Definition) { d.Name = "  " }, CodeMissing, "name"},
		{"名字含大写", func(d *Definition) { d.Name = "WebApp" }, CodeMalformed, "name"},
		{"名字以数字开头", func(d *Definition) { d.Name = "1webapp" }, CodeMalformed, "name"},
		{"名字含点", func(d *Definition) { d.Name = "web.app" }, CodeMalformed, "name"},
		{"名字过长", func(d *Definition) { d.Name = strings.Repeat("a", MaxNameLen+1) }, CodeMalformed, "name"},

		{"镜像为空", func(d *Definition) { d.ImageReference = "" }, CodeMissing, "image_reference"},
		{"镜像仓库名大写", func(d *Definition) { d.ImageReference = "Nginx:1.27" }, CodeMalformed, "image_reference"},
		{"镜像带协议前缀", func(d *Definition) { d.ImageReference = "docker://nginx:1.27" }, CodeMalformed, "image_reference"},
		{"镜像 digest 太短", func(d *Definition) { d.ImageReference = "nginx@sha256:abc" }, CodeMalformed, "image_reference"},
		{"镜像 tag 含斜杠", func(d *Definition) { d.ImageReference = "nginx:1.27/x" }, CodeMalformed, "image_reference"},
		{"镜像带空白", func(d *Definition) { d.ImageReference = "nginx: 1.27" }, CodeMalformed, "image_reference"},
		{"镜像有两个 at", func(d *Definition) {
			d.ImageReference = "nginx@sha256:abc@" + testDigest
		}, CodeMalformed, "image_reference"},

		{"容器端口为 0", func(d *Definition) { d.ContainerPort = 0 }, CodeOutOfRange, "container_port"},
		{"容器端口越界", func(d *Definition) { d.ContainerPort = 65536 }, CodeOutOfRange, "container_port"},
		{"宿主端口为 0", func(d *Definition) { d.HostPort = 0 }, CodeOutOfRange, "host_port"},
		{"宿主端口负数", func(d *Definition) { d.HostPort = -1 }, CodeOutOfRange, "host_port"},

		{"绑定地址为空", func(d *Definition) { d.HostBindAddress = "" }, CodeMissing, "host_bind_address"},
		{"绑定地址是主机名", func(d *Definition) { d.HostBindAddress = "example.com" }, CodeMalformed, "host_bind_address"},
		{"绑定地址带端口", func(d *Definition) { d.HostBindAddress = "127.0.0.1:80" }, CodeMalformed, "host_bind_address"},

		{"环境变量名是数字开头", func(d *Definition) {
			d.Environment = []Env{{Name: "1BAD", Value: &empty}}
		}, CodeMalformed, "environment[0].name"},
		{"环境变量名带中划线", func(d *Definition) {
			d.Environment = []Env{{Name: "APP-MODE", Value: &empty}}
		}, CodeMalformed, "environment[0].name"},
		{"环境变量名为空", func(d *Definition) {
			d.Environment = []Env{{Value: &empty}}
		}, CodeMissing, "environment[0].name"},
		{"环境变量重复", func(d *Definition) {
			d.Environment = []Env{{Name: "A", Value: &empty}, {Name: "A", SecretRef: testSecretRef}}
		}, CodeDuplicate, "environment[1].name"},
		{"值里面放秘密的同时又给 secret_ref", func(d *Definition) {
			d.Environment = []Env{{Name: "A", Value: &empty, SecretRef: testSecretRef}}
		}, CodeNotAllowed, "environment[0]"},
		{"既没有值也没有 secret_ref", func(d *Definition) {
			d.Environment = []Env{{Name: "A"}}
		}, CodeMissing, "environment[0]"},
		{"secret_ref 形状不对", func(d *Definition) {
			d.Environment = []Env{{Name: "A", SecretRef: "sec_nothex"}}
		}, CodeMalformed, "environment[0].secret_ref"},
		{"环境变量值里有 NUL", func(d *Definition) {
			bad := "a\x00b"
			d.Environment = []Env{{Name: "A", Value: &bad}}
		}, CodeMalformed, "environment[0].value"},
		{"环境变量值超长", func(d *Definition) {
			big := strings.Repeat("x", MaxEnvValueBytes+1)
			d.Environment = []Env{{Name: "A", Value: &big}}
		}, CodeOutOfRange, "environment[0].value"},

		{"挂载来源根是绝对路径", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "/etc", ContainerPath: "/data"}}
		}, CodeMalformed, "mounts[0].source_root_id"},
		{"挂载来源根为空", func(d *Definition) {
			d.Mounts = []Mount{{ContainerPath: "/data"}}
		}, CodeMissing, "mounts[0].source_root_id"},
		{"相对路径带 ..", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", RelativePath: "../etc", ContainerPath: "/data"}}
		}, CodeNotAllowed, "mounts[0].relative_path"},
		{"相对路径是绝对路径", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", RelativePath: "/etc", ContainerPath: "/data"}}
		}, CodeNotAllowed, "mounts[0].relative_path"},
		{"相对路径不规范", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", RelativePath: "a//b", ContainerPath: "/data"}}
		}, CodeNotAllowed, "mounts[0].relative_path"},
		{"容器路径不是绝对路径", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "data"}}
		}, CodeMalformed, "mounts[0].container_path"},
		{"容器路径是根", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/"}}
		}, CodeNotAllowed, "mounts[0].container_path"},
		{"容器路径是 /proc", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/proc"}}
		}, CodeNotAllowed, "mounts[0].container_path"},
		{"容器路径在 /proc 下", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/proc/1/root"}}
		}, CodeNotAllowed, "mounts[0].container_path"},
		{"容器路径是 /sys", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/sys/fs/cgroup"}}
		}, CodeNotAllowed, "mounts[0].container_path"},
		{"容器路径是 /dev", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/dev/shm"}}
		}, CodeNotAllowed, "mounts[0].container_path"},
		{"容器路径是 docker.sock", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/var/run/docker.sock"}}
		}, CodeNotAllowed, "mounts[0].container_path"},
		{"容器路径不是规范形式", func(d *Definition) {
			d.Mounts = []Mount{{SourceRootID: "data", ContainerPath: "/data/"}}
		}, CodeMalformed, "mounts[0].container_path"},
		{"容器路径重复", func(d *Definition) {
			d.Mounts = []Mount{
				{SourceRootID: "data", RelativePath: "a", ContainerPath: "/data"},
				{SourceRootID: "data", RelativePath: "b", ContainerPath: "/data"},
			}
		}, CodeDuplicate, "mounts[1].container_path"},
		{"挂载条数过多", func(d *Definition) {
			mounts := make([]Mount, 0, MaxMounts+1)
			for i := 0; i <= MaxMounts; i++ {
				mounts = append(mounts, Mount{SourceRootID: "data", ContainerPath: "/data/" + string(rune('a'+i))})
			}
			d.Mounts = mounts
		}, CodeOutOfRange, "mounts"},

		{"内存上限缺省", func(d *Definition) { d.ResourceLimits.MemoryLimitBytes = 0 }, CodeMissing, "resource_limits.memory_limit_bytes"},
		{"内存上限过小", func(d *Definition) { d.ResourceLimits.MemoryLimitBytes = 1 << 20 }, CodeOutOfRange, "resource_limits.memory_limit_bytes"},
		{"内存上限过大", func(d *Definition) { d.ResourceLimits.MemoryLimitBytes = 1 << 40 }, CodeOutOfRange, "resource_limits.memory_limit_bytes"},
		{"CPU 上限缺省", func(d *Definition) { d.ResourceLimits.CPULimit = "" }, CodeMissing, "resource_limits.cpu_limit"},
		{"CPU 上限不是数字", func(d *Definition) { d.ResourceLimits.CPULimit = "half" }, CodeMalformed, "resource_limits.cpu_limit"},
		{"CPU 上限带单位", func(d *Definition) { d.ResourceLimits.CPULimit = "500m" }, CodeMalformed, "resource_limits.cpu_limit"},
		{"CPU 上限过小", func(d *Definition) { d.ResourceLimits.CPULimit = "0.01" }, CodeOutOfRange, "resource_limits.cpu_limit"},
		{"CPU 上限过大", func(d *Definition) { d.ResourceLimits.CPULimit = "9" }, CodeOutOfRange, "resource_limits.cpu_limit"},

		{"健康检查种类不支持", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "udp"}
		}, CodeNotAllowed, "health_check.kind"},
		{"健康检查缺种类", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Path: "/x"}
		}, CodeMissing, "health_check.kind"},
		{"http 健康检查缺路径", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "http"}
		}, CodeMissing, "health_check.path"},
		{"http 健康检查路径给成 URL", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "http", Path: "http://example.com/"}
		}, CodeMalformed, "health_check.path"},
		{"健康检查探别的端口", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "tcp", Port: 9999}
		}, CodeNotAllowed, "health_check.port"},
		{"健康检查间隔过密", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "tcp", IntervalSeconds: 1}
		}, CodeOutOfRange, "health_check.interval_seconds"},
		{"健康检查超时不小于间隔", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "tcp", IntervalSeconds: 10, TimeoutSeconds: 10}
		}, CodeOutOfRange, "health_check.timeout_seconds"},
		{"健康检查重试次数过大", func(d *Definition) {
			d.HealthCheck = HealthCheck{Enabled: true, Kind: "tcp", Retries: 99}
		}, CodeOutOfRange, "health_check.retries"},

		{"依赖卷形状不对", func(d *Definition) { d.VolumeDependencies = []string{"sda1"} }, CodeMalformed, "volume_dependencies"},
		{"依赖卷过多", func(d *Definition) {
			ids := make([]string, 0, MaxVolumeDependencies+1)
			for i := 0; i <= MaxVolumeDependencies; i++ {
				ids = append(ids, "vol_"+strings.Repeat("0", 30)+string(rune('a'+i/10))+string(rune('a'+i%10)))
			}
			d.VolumeDependencies = ids
		}, CodeOutOfRange, "volume_dependencies"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDefinition()
			tc.mutate(&d)
			_, err := Normalize(d)
			expectInvalid(t, err, tc.code, tc.field)
		})
	}
}

// TestNormalizeHealthCheckDisabledClearsFields 确认“关掉的健康检查”不会把 half 份配置带进库里。
func TestNormalizeHealthCheckDisabledClearsFields(t *testing.T) {
	d := validDefinition()
	d.HealthCheck = HealthCheck{Enabled: false, Kind: "http", Path: "/healthz", IntervalSeconds: 30}
	got, err := Normalize(d)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if got.HealthCheck.Enabled || got.HealthCheck.Kind != "" || got.HealthCheck.Path != "" {
		t.Fatalf("关闭的健康检查必须整体清零，实际 %+v", got.HealthCheck)
	}
}

// TestNormalizeImageReferenceForms 钉住镜像引用的几种合法形态与补全规则。
func TestNormalizeImageReferenceForms(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nginx", "nginx:latest"},
		{"nginx:1.27", "nginx:1.27"},
		{"library/nginx:1.27", "library/nginx:1.27"},
		{"ghcr.io/owner/app:v1.2", "ghcr.io/owner/app:v1.2"},
		{"localhost:5000/app:dev", "localhost:5000/app:dev"},
		{"nginx@" + testDigest, "nginx@" + testDigest},
		{"nginx:1.27@" + testDigest, "nginx:1.27@" + testDigest},
		{"  nginx:1.27  ", "nginx:1.27"},
	}
	for _, tc := range cases {
		ref, err := parseImageReference(tc.in)
		if err != nil {
			t.Fatalf("%q 应当合法，却被拒：%v", tc.in, err)
		}
		if got := ref.String(); got != tc.want {
			t.Fatalf("%q 规范化成 %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeVolumeDependenciesDedup 确认依赖卷去重且保持顺序。
func TestNormalizeVolumeDependenciesDedup(t *testing.T) {
	other := "vol_ffffffffffffffffffffffffffffffff"
	d := validDefinition()
	d.VolumeDependencies = []string{other, testVolumeID, other}
	got, err := Normalize(d)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if len(got.VolumeDependencies) != 2 || got.VolumeDependencies[0] != other {
		t.Fatalf("依赖卷去重/顺序不对: %v", got.VolumeDependencies)
	}
}

// TestCheckHost 逐条钉住“定义自己答不了、必须问宿主”的那部分。
func TestCheckHost(t *testing.T) {
	norm := func(t *testing.T) Definition {
		t.Helper()
		d, err := Normalize(validDefinition())
		if err != nil {
			t.Fatalf("基准定义规范化失败: %v", err)
		}
		return d
	}

	t.Run("基准通过", func(t *testing.T) {
		if err := CheckHost(norm(t), testHostEnv()); err != nil {
			t.Fatalf("宿主事实齐备时不该被拒：%v", err)
		}
	})

	t.Run("绑定不是本机地址", func(t *testing.T) {
		d := norm(t)
		d.HostBindAddress = "198.51.100.7"
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeNotAllowed, "host_bind_address")
	})

	t.Run("绑定本机地址通过", func(t *testing.T) {
		d := norm(t)
		d.HostBindAddress = "192.0.2.10"
		if err := CheckHost(d, testHostEnv()); err != nil {
			t.Fatalf("本机地址应允许：%v", err)
		}
	})

	t.Run("未开允许时不许绑通配地址", func(t *testing.T) {
		d := norm(t)
		d.HostBindAddress = "0.0.0.0"
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeNotAllowed, "host_bind_address")
	})

	t.Run("显式放开后允许通配地址", func(t *testing.T) {
		d := norm(t)
		d.HostBindAddress = "0.0.0.0"
		env := testHostEnv()
		env.AllowWildcard = true
		if err := CheckHost(d, env); err != nil {
			t.Fatalf("放开后应允许：%v", err)
		}
	})

	t.Run("回环永远允许", func(t *testing.T) {
		d := norm(t)
		d.HostBindAddress = "::1"
		if err := CheckHost(d, testHostEnv()); err != nil {
			t.Fatalf("回环地址应允许：%v", err)
		}
	})

	t.Run("文件根没登记", func(t *testing.T) {
		d := norm(t)
		d.Mounts[0].SourceRootID = "ghost"
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeUnknownRoot, "mounts[0].source_root_id")
	})

	t.Run("文件根在部署数据根之外", func(t *testing.T) {
		d := norm(t)
		d.Mounts = []Mount{{SourceRootID: "etc", ContainerPath: "/host-etc", ReadOnly: true}}
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeNotAllowed, "mounts[0].source_root_id")
	})

	t.Run("只读根不能以读写方式挂", func(t *testing.T) {
		d := norm(t)
		d.Mounts = []Mount{{SourceRootID: "backup", ContainerPath: "/backup"}}
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeNotAllowed, "mounts[0].read_only")
	})

	t.Run("只读根以只读方式挂可以", func(t *testing.T) {
		d := norm(t)
		d.Mounts = []Mount{{SourceRootID: "backup", ContainerPath: "/backup", ReadOnly: true}}
		if err := CheckHost(d, testHostEnv()); err != nil {
			t.Fatalf("只读挂只读根应允许：%v", err)
		}
	})

	t.Run("没有部署数据根时任何挂载都拒", func(t *testing.T) {
		d := norm(t)
		env := testHostEnv()
		env.DataBase = ""
		expectInvalid(t, CheckHost(d, env), CodeNotAllowed, "mounts")
	})

	t.Run("依赖卷不在登记表里", func(t *testing.T) {
		d := norm(t)
		d.VolumeDependencies = []string{"vol_11111111111111111111111111111111"}
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeVolumeMissing, "volume_dependencies")
	})

	t.Run("文件根不在任何受管理卷里", func(t *testing.T) {
		d := norm(t)
		d.Mounts = []Mount{{SourceRootID: "loose", ContainerPath: "/data"}}
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeNotAllowed, "mounts[0].source_root_id")
	})

	t.Run("文件根所在的卷没登记", func(t *testing.T) {
		d := norm(t)
		d.Mounts = []Mount{{SourceRootID: "other", ContainerPath: "/data"}}
		env := testHostEnv()
		delete(env.Volumes, testOtherVolumeID)
		expectInvalid(t, CheckHost(d, env), CodeVolumeMissing, "mounts[0].source_root_id")
	})

	t.Run("文件根所在的卷没写进依赖", func(t *testing.T) {
		// 卷在登记表里、挂载也合法，但 volume_dependencies 里没有它：门禁守的是被声明的
		// 依赖卷，容器吃的却是这个根。少了这一条，“卷缺失不启动”就断在半路。
		d := norm(t)
		d.Mounts = []Mount{{SourceRootID: "other", ContainerPath: "/data"}}
		expectInvalid(t, CheckHost(d, testHostEnv()), CodeVolumeMissing, "mounts[0].source_root_id")
	})

	t.Run("卷表缺失等于卷不在", func(t *testing.T) {
		d := norm(t)
		env := testHostEnv()
		env.Volumes = nil
		expectInvalid(t, CheckHost(d, env), CodeVolumeMissing, "volume_dependencies")
	})
}

// TestClaims 确认端口声明来自定义本身（P15.2 的冲突检测与执行锁都读它）。
func TestClaims(t *testing.T) {
	d := validDefinition()
	claims := d.Claims()
	if len(claims) != 1 {
		t.Fatalf("期望一条端口声明，实际 %d 条", len(claims))
	}
	if claims[0].Address != "127.0.0.1" || claims[0].Port != 18080 || claims[0].Protocol != ProtocolTCP {
		t.Fatalf("端口声明 = %+v", claims[0])
	}
	d.HostPort = 0
	if got := d.Claims(); got != nil {
		t.Fatalf("端口非法时不该给出声明，实际 %+v", got)
	}
}

// TestVolumeOfPath：路径归哪个卷，规则是“最长挂载点优先”。
//
// 两条约束必须同时成立：
//   - 嵌套挂载点时选里面那块（外层盘兜不住内层卷里的数据）；
//   - 不是绝对路径、或者是 `/` 的卷一律不参与（那种卷挡不住任何东西，见 volumeMountPoint）。
func TestVolumeOfPath(t *testing.T) {
	volumes := map[string]VolumeFacts{
		"vol_root":  {MountPoint: "/srv/upanel/volumes"},
		"vol_media": {MountPoint: "/srv/upanel/volumes/media"},
		"vol_slash": {MountPoint: "/"},
		"vol_rel":   {MountPoint: "srv/upanel/volumes/rel"},
		"vol_empty": {MountPoint: "   "},
	}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"根挂载点自身", "/srv/upanel/volumes", "vol_root"},
		{"内层挂载点自身", "/srv/upanel/volumes/media", "vol_media"},
		{"内层卷里的子目录", "/srv/upanel/volumes/media/webapp/assets", "vol_media"},
		{"外层卷里的其它目录", "/srv/upanel/volumes/other/app", "vol_root"},
		{"不在任何卷里", "/etc", ""},
		{"相对路径", "srv/upanel/volumes/media", ""},
		{"前缀相近但不是子目录", "/srv/upanel/volumes/media2", "vol_root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VolumeOfPath(c.path, volumes); got != c.want {
				t.Fatalf("VolumeOfPath(%q) = %q，期望 %q", c.path, got, c.want)
			}
		})
	}
	// `/` 与相对路径的卷单独验一次：它们不进匹配表，所以 /srv 只能落到 vol_root。
	only := map[string]VolumeFacts{"vol_slash": {MountPoint: "/"}, "vol_rel": {MountPoint: "srv/upanel/volumes/rel"}}
	if got := VolumeOfPath("/srv/upanel/volumes/media", only); got != "" {
		t.Fatalf("不能拿 `/` 当兜底的卷，实际 %q", got)
	}
	if got := VolumeOfPath("/srv/upanel/volumes/media", map[string]VolumeFacts{}); got != "" {
		t.Fatalf("空卷表 = %q", got)
	}
}
