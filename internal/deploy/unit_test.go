package deploy

import (
	"context"
	"strings"
	"testing"
)

// goldenUnit 是 goldenDefinition 对应的**逐字**包装单元。
//
// 逐字比对而不是若干 contains：这份文本要落进 /etc/systemd/system，缩进、`;` 注释行
// 的位置、BindsTo 与 After 的成对出现、ExecStart 的每一个参数，都是“卷没挂上就不启动”
// 这条保证的一部分。改成 contains 之后，一次误删 BindsTo 也能过。
const goldenUnit = `; uPanel 受管理部署：webapp（dep_ed8e30bcbbf594e511e8c5a08eb65d9c）
;
; 这个文件由 uPanel 生成，每次 deployment.apply 整篇重写。不要手工修改：
; 手工改动会在下一次部署时被覆盖，而覆盖后的行为以面板上的定义为准。
;
; 四条纪律，改单元的人一眼要能看见：
;   生命周期  容器自己的 restart 是 no。开机、启动、停止都只有本单元一条路径。
;             让 docker 在开机时拉容器会绕过下面的卷依赖（PLAN §11 规则 4）。
;   卷依赖    BindsTo + After 逐卷绑定 mount unit：卷没挂上，本单元起不来；
;             运行中卷失联，systemd 停掉本单元，ExecStop 把项目收下来（规则 3/5）。
;   不建目录  本单元不创建任何目录，配置里 create_host_path 为 false。
;             绑定源不存在时 compose 直接失败——挂载坏了必须看起来像坏了（规则 6）。
;   无 shell   ExecStart/ExecStop 只有固定的 docker compose 调用，没有变量拼接、
;             没有命令替换。单元里能改的东西越少，越不可能被改成别的动作。

[Unit]
Description=uPanel 受管理部署：webapp
Documentation=file:/usr/local/share/doc/upanel/DEPLOY.md
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service
RequiresMountsFor=/srv/upanel/volumes/media
BindsTo=srv-upanel-volumes-media.mount
After=srv-upanel-volumes-media.mount

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/docker compose -p webapp -f /etc/upanel/deploy/webapp.compose.yaml up -d --remove-orphans --no-build
ExecStop=/usr/bin/docker compose -p webapp -f /etc/upanel/deploy/webapp.compose.yaml down --remove-orphans
TimeoutStartSec=600
TimeoutStopSec=300
;
; up -d --remove-orphans：项目里多出来的容器（改了定义、名字对不上）随这次启动清掉，
; 否则旧容器会继续占着端口，而面板上看到的是“新版本已部署”。
; --no-build：镜像只来自 registry，永远不在本机构建——没有 Dockerfile，也没有构建上下文。

[Install]
WantedBy=multi-user.target
`

// goldenPlan 用 goldenDefinition 渲染出配置，再折成单元计划。
func goldenPlan(t *testing.T) UnitPlan {
	t.Helper()
	d := goldenDefinition()
	env := testHostEnv()
	rendered, err := Render(context.Background(), d, RenderEnv{
		Host:     env,
		Resolver: &fakeResolver{value: []byte("pass$word")},
	})
	if err != nil {
		t.Fatalf("渲染配置失败：%v", err)
	}
	plan, err := PlanUnit(d, rendered, env)
	if err != nil {
		t.Fatalf("计算单元计划失败：%v", err)
	}
	return plan
}

// TestMountUnitName 逐字符钉住 systemd-escape --path 的等价实现。
//
// 这条链上任何一处不一致都不会报“依赖没生效”，而是 BindsTo 指向一个不存在的单元、
// 整个部署起不来。所以每个字符都要对。
func TestMountUnitName(t *testing.T) {
	ok := []struct{ in, want string }{
		{"/srv/upanel/volumes/media", "srv-upanel-volumes-media.mount"},
		// 段内的 `-` 必须转义：不转义时它与 `/` 拼出来的单元名无法区分。
		{"/srv/upanel/volumes/my-app", `srv-upanel-volumes-my\x2dapp.mount`},
		// 空格与其它符号按 \xNN 转义。
		{"/srv/data set", `srv-data\x20set.mount`},
		{"/srv/a.b_c:d", "srv-a.b_c:d.mount"},
		// 结尾多一个斜杠是同一个挂载点。
		{"/srv/upanel/volumes/media/", "srv-upanel-volumes-media.mount"},
	}
	for _, c := range ok {
		got, err := MountUnitName(c.in)
		if err != nil {
			t.Fatalf("%q 应该能拼出单元名，却报错：%v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q 拼成 %q，期望 %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "srv/upanel", "/", "/srv//upanel", "/srv/./upanel", "/srv/../upanel"} {
		if got, err := MountUnitName(bad); err == nil {
			t.Fatalf("%q 不该拼出单元名，却得到 %q", bad, got)
		}
	}
}

// TestUnitNameAndPaths 确认单元名与配置路径都由名字派生、且坏名字拼不出来。
func TestUnitNameAndPaths(t *testing.T) {
	unit, err := UnitName("webapp")
	if err != nil || unit != "upanel-deploy-webapp.service" {
		t.Fatalf("单元名 = %q（%v）", unit, err)
	}
	pathWant := UnitDir + "/upanel-deploy-webapp.service"
	if got, err := UnitPath("webapp"); err != nil || got != pathWant {
		t.Fatalf("单元路径 = %q（%v），期望 %q", got, err, pathWant)
	}
	cfgWant := DeployConfigDir + "/webapp.compose.yaml"
	if got, err := ConfigPath("webapp"); err != nil || got != cfgWant {
		t.Fatalf("配置路径 = %q（%v），期望 %q", got, err, cfgWant)
	}
	for _, bad := range []string{"", "Webapp", "1web", "web app", "../etc", "web/app", "webapp\n"} {
		if _, err := UnitName(bad); err == nil {
			t.Fatalf("部署名 %q 不该能拼进单元名", bad)
		}
		if _, err := ConfigPath(bad); err == nil {
			t.Fatalf("部署名 %q 不该能拼进配置路径", bad)
		}
	}
}

// TestRenderUnitGolden 逐字比对包装单元。
func TestRenderUnitGolden(t *testing.T) {
	b, err := RenderUnit(goldenPlan(t))
	if err != nil {
		t.Fatalf("渲染单元失败：%v", err)
	}
	if string(b) != goldenUnit {
		t.Fatalf("单元内容与黄金样本不一致：\n--- 实际 ---\n%s\n--- 期望 ---\n%s", b, goldenUnit)
	}
}

// TestRenderUnitBindsEveryVolume 确认每一个依赖卷都有 RequiresMountsFor 与 BindsTo/After。
//
// 缺一条的表现不是报错，而是“那个卷没挂上时部署照样起来”——所以这条从计划的
// 挂载点列表出发逐个核对，而不是数一数有几行。
func TestRenderUnitBindsEveryVolume(t *testing.T) {
	d := goldenDefinition()
	// 再加一个根与一个依赖卷：两个卷都要被绑上。
	d.Mounts = append(d.Mounts, Mount{SourceRootID: "other", ContainerPath: "/other"})
	d.VolumeDependencies = append(d.VolumeDependencies, testOtherVolumeID)

	env := testHostEnv()
	rendered, err := Render(context.Background(), d, RenderEnv{
		Host:     env,
		Resolver: &fakeResolver{value: []byte("x")},
	})
	if err != nil {
		t.Fatalf("渲染配置失败：%v", err)
	}
	plan, err := PlanUnit(d, rendered, env)
	if err != nil {
		t.Fatalf("计算单元计划失败：%v", err)
	}
	if len(plan.VolumeMountPoints) != 2 {
		t.Fatalf("期望两个卷挂载点，实际 %v", plan.VolumeMountPoints)
	}
	b, err := RenderUnit(plan)
	if err != nil {
		t.Fatalf("渲染单元失败：%v", err)
	}
	text := string(b)
	want := "RequiresMountsFor=/srv/upanel/volumes/media /srv/upanel/volumes/other\n"
	if !strings.Contains(text, want) {
		t.Fatalf("缺少按路径排序的 RequiresMountsFor：%s", text)
	}
	for _, mp := range plan.VolumeMountPoints {
		unit, err := MountUnitName(mp)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{"BindsTo=" + unit + "\n", "After=" + unit + "\n"} {
			if !strings.Contains(text, line) {
				t.Fatalf("卷 %s 缺少 %q", mp, strings.TrimSpace(line))
			}
		}
	}
}

// TestRenderUnitHasNoShell 确认单元里没有任何“可被改写成别的动作”的东西。
func TestRenderUnitHasNoShell(t *testing.T) {
	b, err := RenderUnit(goldenPlan(t))
	if err != nil {
		t.Fatalf("渲染单元失败：%v", err)
	}
	text := string(b)
	for _, banned := range []string{
		"/bin/sh", "/bin/bash", "sh -c", "bash -c", "$(", "`", "${", "eval ",
		"/var/run/docker.sock", "docker.sock", "--privileged", "sudo ",
	} {
		if strings.Contains(text, banned) {
			t.Fatalf("单元里不该出现 %q：\n%s", banned, text)
		}
	}
	// 唯一的可执行文件是写死路径的 docker。
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "ExecStart=") && !strings.HasPrefix(line, "ExecStop=") {
			continue
		}
		if !strings.HasPrefix(line, "ExecStart="+DockerBinary+" ") && !strings.HasPrefix(line, "ExecStop="+DockerBinary+" ") {
			t.Fatalf("Exec 行必须以 %s 开头：%s", DockerBinary, line)
		}
	}
}

// TestRenderUnitNeedsMountUnitsFirst 确认单元被设成开机启动——
// 否则 restart=no 就意味着“机器重启后应用永远回不来”，而面板上看起来一切正常。
func TestRenderUnitNeedsMountUnitsFirst(t *testing.T) {
	b, err := RenderUnit(goldenPlan(t))
	if err != nil {
		t.Fatalf("渲染单元失败：%v", err)
	}
	text := string(b)
	for _, want := range []string{"\n[Install]\nWantedBy=multi-user.target\n", "Type=oneshot\n", "RemainAfterExit=yes\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("单元缺少 %q", strings.TrimSpace(want))
		}
	}
}

// TestRenderUnitIsDeterministic 打乱输入顺序，单元字节必须一样。
//
// 单元文件是“这份定义此刻的形态”，内容一变 systemd 就会重启单元。
// 顺序敏感会让每次保存都把应用重启一遍。
func TestRenderUnitIsDeterministic(t *testing.T) {
	first, err := RenderUnit(goldenPlan(t))
	if err != nil {
		t.Fatalf("渲染单元失败：%v", err)
	}

	d := goldenDefinition()
	// 反过来摆：两个挂载与依赖顺序都换一遍。
	d.Mounts[0], d.Mounts[1] = d.Mounts[1], d.Mounts[0]
	d = mustNormalize(t, d)
	env := testHostEnv()
	rendered, err := Render(context.Background(), d, RenderEnv{
		Host:     env,
		Resolver: &fakeResolver{value: []byte("pass$word")},
	})
	if err != nil {
		t.Fatalf("渲染配置失败：%v", err)
	}
	plan, err := PlanUnit(d, rendered, env)
	if err != nil {
		t.Fatalf("计算单元计划失败：%v", err)
	}
	second, err := RenderUnit(plan)
	if err != nil {
		t.Fatalf("渲染单元失败：%v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("同一份定义渲染出两个单元：\n--- A ---\n%s\n--- B ---\n%s", first, second)
	}
}

// TestPlanUnitRejectsRootOutsideVolume 确认“卷在、但根不在这个卷的挂载点里”被拒。
//
// 这种不一致正是“门禁守 A 卷、容器吃 B 目录”的入口：单元会绑在 media 的 mount unit 上，
// 而绑定源在别处，卷没挂上时容器照样起来。
func TestPlanUnitRejectsRootOutsideVolume(t *testing.T) {
	d := goldenDefinition()
	env := testHostEnv()
	// 根的路径挪到别的卷下面，但卷标识仍然是原来那个。
	env.Roots["data"] = RootFacts{Path: "/srv/upanel/volumes/other/data", Writable: true, VolumeID: testVolumeID}

	rendered, err := Render(context.Background(), d, RenderEnv{
		Host:     env,
		Resolver: &fakeResolver{value: []byte("x")},
	})
	if err == nil {
		// Render 里的 CheckHost 应该已经拦下；万一没拦，PlanUnit 必须拦。
		if _, planErr := PlanUnit(d, rendered, env); planErr == nil {
			t.Fatal("根不在它登记的卷的挂载点里，却被接受了")
		}
		return
	}
	expectInvalid(t, err, CodeNotAllowed, "mounts[0].source_root_id")
}

// TestPlanUnitRejectsVolumeWithoutMountPoint 确认没有挂载点的卷不能当依赖。
//
// 没有挂载点就拼不出 mount unit，BindsTo 也就无从谈起——那种“依赖”只是数据库里
// 一行好看的字，挡不住任何东西。
func TestPlanUnitRejectsVolumeWithoutMountPoint(t *testing.T) {
	d := goldenDefinition()
	env := testHostEnv()
	vol := env.Volumes[testVolumeID]
	vol.MountPoint = ""
	env.Volumes[testVolumeID] = vol

	_, err := Render(context.Background(), d, RenderEnv{
		Host:     env,
		Resolver: &fakeResolver{value: []byte("x")},
	})
	expectInvalid(t, err, CodeNotAllowed, "volume_dependencies")
}

// TestComposeArgv 钉住 compose 的 argv 形状。
func TestComposeArgv(t *testing.T) {
	argv, err := ComposeArgv("webapp", "/etc/upanel/deploy/webapp.compose.yaml", "up", "-d")
	if err != nil {
		t.Fatalf("拼 argv 失败：%v", err)
	}
	want := []string{DockerBinary, "compose", "-p", "webapp", "-f", "/etc/upanel/deploy/webapp.compose.yaml", "up", "-d"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %v，期望 %v", argv, want)
	}

	if _, err := ComposeArgv("Webapp", "/etc/x.yaml", "up"); err == nil {
		t.Fatal("坏名字不该能进 argv")
	}
	for _, badPath := range []string{"", "etc/x.yaml", "/etc/x y.yaml", "/etc/x\"y.yaml", "/etc/$(id).yaml"} {
		if _, err := ComposeArgv("webapp", badPath, "up"); err == nil {
			t.Fatalf("配置路径 %q 不该能进 argv", badPath)
		}
	}
	if _, err := ComposeArgv("webapp", "/etc/x.yaml"); err == nil {
		t.Fatal("没有动词的 compose 调用不该拼出来")
	}
}

// TestCheckVolumesReady 确认启动前的 UUID 核对覆盖三种情形：没挂、挂错盘、读不到。
func TestCheckVolumesReady(t *testing.T) {
	d := goldenDefinition()

	if problems := CheckVolumesReady(d, testHostEnv()); len(problems) != 0 {
		t.Fatalf("卷都准备好了却报问题：%v", problems)
	}

	env := testHostEnv()
	vol := env.Volumes[testVolumeID]
	vol.Mounted = false
	env.Volumes[testVolumeID] = vol
	if problems := CheckVolumesReady(d, env); len(problems) != 1 ||
		!strings.Contains(problems[0], "没有挂载") {
		t.Fatalf("没挂载应报一条问题，实际 %v", problems)
	}

	env = testHostEnv()
	vol = env.Volumes[testVolumeID]
	vol.MountedUUID = testOtherVolumeUUID // 挂的是另一块盘
	env.Volumes[testVolumeID] = vol
	problems := CheckVolumesReady(d, env)
	if len(problems) != 1 || !strings.Contains(problems[0], "另一块文件系统") {
		t.Fatalf("UUID 不同应报一条问题，实际 %v", problems)
	}

	env = testHostEnv()
	vol = env.Volumes[testVolumeID]
	vol.MountedUUID = ""
	env.Volumes[testVolumeID] = vol
	problems = CheckVolumesReady(d, env)
	if len(problems) != 1 || !strings.Contains(problems[0], "读不到") {
		t.Fatalf("读不到 UUID 应报一条问题，实际 %v", problems)
	}

	delete(env.Volumes, testVolumeID)
	if problems := CheckVolumesReady(d, env); len(problems) != 1 ||
		!strings.Contains(problems[0], "不在受管理卷登记表里") {
		t.Fatalf("卷不在登记表里应报一条问题，实际 %v", problems)
	}
}

// mustNormalize 是测试里“这份定义本身合法”的断言 + 规范化。
func mustNormalize(t *testing.T, d Definition) Definition {
	t.Helper()
	out, err := Normalize(d)
	if err != nil {
		t.Fatalf("定义不合法：%v", err)
	}
	return out
}
