package agent

// 卷管理动作的接线测试（P07.4）。
//
// 这一层自己不判断“能不能导入/挂载”（那是 storage.AssessVolume 的事），它只负责四件事：
// 登记五个动作、按形状拒绝参数、把判决原样映射成预检计划、把预检绑定的目标交给执行器。
// 所以测试也围绕这四件事，不去重复判决层的用例。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"upanel/internal/storage"
)

// fakeVolumeSource 是卷判决 + 执行的测试替身。
//
// 它只实现 AssessVolume 与 ExecuteWrite：volumeSource 就是这两个方法，
// 因此“只接了设备判决、没接卷判决”的接线在这里会直接编译失败。
type fakeVolumeSource struct {
	assessment storage.Assessment
	assessErr  error

	report   storage.WriteReport
	writeErr error

	actions []string
	params  []any
	writes  []storage.WriteRequest
}

func (f *fakeVolumeSource) AssessVolume(_ context.Context, action string, params any) (storage.Assessment, error) {
	f.actions = append(f.actions, action)
	f.params = append(f.params, params)
	if f.assessErr != nil {
		return storage.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeVolumeSource) ExecuteWrite(_ context.Context, req storage.WriteRequest) (storage.WriteReport, error) {
	f.writes = append(f.writes, req)
	if f.writeErr != nil {
		return storage.WriteReport{}, f.writeErr
	}
	return f.report, nil
}

const (
	volTestID      = "vol_0123456789abcdef0123456789abcdef"
	volTestUUID    = "5f1c9a3e-2b64-4c7d-8e10-9a3b5c7d1e2f"
	volTestVersion = "3"
)

// volumeAssessment 是一份“放行且要素齐备”的卷判决。
//
// 卷动作没有 Impact（挂载/卸载不改写盘上数据），但锁与状态版本必须有：
// 预检计划少了它们，执行前复核就无从比较。
func volumeAssessment(action string) storage.Assessment {
	return storage.Assessment{
		Action:              action,
		TargetID:            volTestID,
		DisplayName:         "数据卷 data",
		IdentityFingerprint: "sha256:" + strings.Repeat("ab", 32),
		StateVersion:        "topo:sha256:snap|vol:" + volTestID + "@" + volTestVersion,
		Allowed:             true,
		Blockers:            []storage.Blocker{},
		Warnings:            []storage.Blocker{},
		Locks:               []string{"disk:sdb", "fstab"},
	}
}

// volumeRegistry 登记五个卷动作，返回登记表与假判决源。
func volumeRegistry(t *testing.T) (*Registry, *fakeVolumeSource) {
	t.Helper()
	src := &fakeVolumeSource{assessment: volumeAssessment(storage.ActionVolumeMount)}
	reg := NewRegistry()
	if err := RegisterVolumeActions(reg, src); err != nil {
		t.Fatalf("登记卷动作失败: %v", err)
	}
	return reg, src
}

// TestVolumeActionsRegistered 校验五个动作都登记了，且都带执行器。
//
// “登记了但没有执行器”= 一个永远跑不起来的任务，所以这两件事在这里一起断言。
func TestVolumeActionsRegistered(t *testing.T) {
	reg, _ := volumeRegistry(t)
	want := []string{
		storage.ActionVolumeConfigure,
		storage.ActionVolumeForget,
		storage.ActionVolumeImport,
		storage.ActionVolumeMount,
		storage.ActionVolumeUnmount,
	}
	if got := reg.ActionNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("登记的动作 = %v，期望 %v", got, want)
	}
	for _, name := range want {
		a, ok := reg.Action(name)
		if !ok {
			t.Fatalf("%s 没登记", name)
		}
		if a.Decode == nil || a.Plan == nil || a.Execute == nil {
			t.Errorf("%s 缺少解码/预检/执行器", name)
		}
	}
	if got := reg.ExecutableActionNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ExecutableActionNames = %v，期望 %v", got, want)
	}
}

// TestVolumeActionPolicies 逐条对照 CONTRACTS.md 第 5 节的 R 列。
//
// 判据是“这一步会不会让别人正在用的东西出事”：卸载会让正在读写的服务失败，
// 改配置会改开机行为，退出管理会撤掉受管条目——三个都要再认证。
// 导入与挂载不动既有行为，不要再认证（但仍然要管理员权限，那由 Web 层保证）。
func TestVolumeActionPolicies(t *testing.T) {
	reg, _ := volumeRegistry(t)
	cases := []struct {
		action  string
		reauth  bool
		phrase  bool
		whyText string
	}{
		{storage.ActionVolumeImport, false, false, "只登记，不格式化"},
		{storage.ActionVolumeMount, false, false, "可逆，且要核对 UUID"},
		{storage.ActionVolumeUnmount, true, false, "会让正在用这个卷的服务失败"},
		{storage.ActionVolumeConfigure, true, false, "改写开机行为"},
		{storage.ActionVolumeForget, true, false, "撤销管理记录并删 fstab 条目"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			a, ok := reg.Action(tc.action)
			if !ok {
				t.Fatal("动作没登记")
			}
			policy, err := a.resolvePolicy(nil)
			if err != nil {
				t.Fatalf("取策略失败: %v", err)
			}
			if policy.RequiresReauth != tc.reauth {
				t.Errorf("RequiresReauth = %v，期望 %v（%s）", policy.RequiresReauth, tc.reauth, tc.whyText)
			}
			// 短语只用于初始化/格式化/删分区这类不可逆操作：卷动作全都可以回头。
			if policy.RequiresPhrase != tc.phrase {
				t.Errorf("RequiresPhrase = %v，期望 %v", policy.RequiresPhrase, tc.phrase)
			}
			if policy.Cancellable {
				t.Error("卷动作不可取消：fstab 替换与 mount 发起都要走到结果")
			}
		})
	}
}

// TestVolumeActionDecode 校验参数解码：合法输入被规范化，非法输入在形状层被拒。
func TestVolumeActionDecode(t *testing.T) {
	reg, _ := volumeRegistry(t)

	t.Run("导入参数被规范化", func(t *testing.T) {
		p, err := decodeFor(t, reg, storage.ActionVolumeImport,
			`{"device_id":" sdb1 ","slug":" data-1 ","display_name":" 数据卷 "}`)
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		got := p.(storage.VolumeImportParams)
		if got.DeviceID != "sdb1" || got.Slug != "data-1" || got.DisplayName != "数据卷" {
			t.Errorf("规范化结果 = %+v", got)
		}
	})

	t.Run("挂载参数被规范化", func(t *testing.T) {
		p, err := decodeFor(t, reg, storage.ActionVolumeMount,
			`{"volume_id":" `+volTestID+` ","version":" `+volTestVersion+` "}`)
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		got := p.(storage.VolumeParams)
		if got.VolumeID != volTestID || got.Version != volTestVersion {
			t.Errorf("规范化结果 = %+v", got)
		}
	})

	t.Run("改配置的四个开关都可以不给", func(t *testing.T) {
		// 全空不是空操作：它表示“按当前配置重写一遍受管块”，
		// 用来修好别人手工改过的条目。
		p, err := decodeFor(t, reg, storage.ActionVolumeConfigure,
			`{"volume_id":"`+volTestID+`","version":"1"}`)
		if err != nil {
			t.Fatalf("全空配置被拒: %v", err)
		}
		got := p.(storage.VolumeConfigureParams)
		if got.Automount != nil || got.ReadOnly != nil || got.Nodev != nil || got.Nosuid != nil {
			t.Errorf("没给的开关不该被补成非 nil：%+v", got)
		}
	})

	bad := []struct {
		name   string
		action string
		raw    string
	}{
		{"空设备名", storage.ActionVolumeImport, `{"device_id":"","slug":"data"}`},
		{"路径穿越设备名", storage.ActionVolumeImport, `{"device_id":"../sdb","slug":"data"}`},
		{"空卷名", storage.ActionVolumeImport, `{"device_id":"sdb1","slug":""}`},
		{"卷名带大写", storage.ActionVolumeImport, `{"device_id":"sdb1","slug":"Data"}`},
		{"卷名带斜杠", storage.ActionVolumeImport, `{"device_id":"sdb1","slug":"a/b"}`},
		{"卷名过长", storage.ActionVolumeImport,
			`{"device_id":"sdb1","slug":"` + strings.Repeat("a", 33) + `"}`},
		{"控制字符显示名", storage.ActionVolumeImport,
			`{"device_id":"sdb1","slug":"data","display_name":"a\u0007b"}`},
		{"导入多余字段", storage.ActionVolumeImport,
			`{"device_id":"sdb1","slug":"data","force":true}`},
		{"卷标识形状不对", storage.ActionVolumeMount, `{"volume_id":"vol_NOTHEX","version":"1"}`},
		{"卷标识大写", storage.ActionVolumeUnmount,
			`{"volume_id":"vol_0123456789ABCDEF0123456789abcdef","version":"1"}`},
		{"缺版本", storage.ActionVolumeUnmount, `{"volume_id":"` + volTestID + `"}`},
		{"版本不是数字", storage.ActionVolumeForget,
			`{"volume_id":"` + volTestID + `","version":"v1"}`},
		{"版本过长", storage.ActionVolumeForget,
			`{"volume_id":"` + volTestID + `","version":"` + strings.Repeat("9", 21) + `"}`},
		{"改配置缺版本", storage.ActionVolumeConfigure, `{"volume_id":"` + volTestID + `"}`},
		{"改配置开关类型不对", storage.ActionVolumeConfigure,
			`{"volume_id":"` + volTestID + `","version":"1","automount":"yes"}`},
		{"尾部内容", storage.ActionVolumeMount,
			`{"volume_id":"` + volTestID + `","version":"1"}{}`},
		{"不是对象", storage.ActionVolumeMount, `["` + volTestID + `"]`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeFor(t, reg, tc.action, tc.raw); err == nil {
				t.Fatalf("非法参数被接受：%s", tc.raw)
			} else if !errors.Is(err, ErrParamsInvalid) {
				t.Fatalf("错误应是 ErrParamsInvalid，实际 %v", err)
			}
		})
	}
}

// TestVolumePlanMapping 校验判决到预检计划的映射：拒绝原因、锁、目标都原样带上。
func TestVolumePlanMapping(t *testing.T) {
	src := &fakeVolumeSource{assessment: storage.Assessment{
		Action:              storage.ActionVolumeUnmount,
		TargetID:            volTestID,
		DisplayName:         "数据卷 data",
		IdentityFingerprint: "sha256:disk-sdb1",
		StateVersion:        "topo:sha256:snap|vol:" + volTestID + "@1|fstab:-|obs:unmounted",
		Allowed:             false,
		Blockers: []storage.Blocker{
			{Code: storage.NoticeVolumeBusy, Message: "卷正在被使用：/srv/upanel/volumes/data"},
		},
		Warnings: []storage.Blocker{
			{Code: storage.NoticeIdentityLowConfidence, Message: "设备身份依据较弱"},
		},
		Locks: []string{"disk:sdb", "fstab"},
	}}
	reg := NewRegistry()
	if err := RegisterVolumeActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	plan := mustPreflight(t, srv, storage.ActionVolumeUnmount,
		`{"volume_id":"`+volTestID+`","version":"1"}`)

	if plan.Allowed {
		t.Fatal("有占用者的卸载被判为允许")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != storage.NoticeVolumeBusy {
		t.Errorf("阻塞项 = %+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != storage.NoticeIdentityLowConfidence {
		t.Errorf("告警 = %+v", plan.Warnings)
	}
	if len(plan.Locks) != 2 || plan.Locks[1] != "fstab" {
		t.Errorf("资源锁 = %v，卷动作必须连 fstab 一起锁", plan.Locks)
	}
	if plan.Target.DisplayName == "" || plan.Target.IdentityFingerprint == "" || plan.Target.StateVersion == "" {
		t.Errorf("目标 = %+v，三项都应存在", plan.Target)
	}
	if !plan.RequiresReauth {
		t.Error("卸载要求再认证，策略没有传到计划里")
	}
	if plan.RequiresPhrase {
		t.Error("卷动作不该要确认短语")
	}
	if len(src.actions) != 1 || src.actions[0] != storage.ActionVolumeUnmount {
		t.Fatalf("判决源被调用了 %v", src.actions)
	}
	if p, ok := src.params[0].(storage.VolumeParams); !ok || p.VolumeID != volTestID {
		t.Fatalf("判决源收到的参数 = %#v", src.params[0])
	}
}

// TestVolumePlanFailureIsNotBlocked 校验判决本身出错时是预检失败，而不是“被拒绝”：
// 两者混起来会让真实的故障看起来像保护生效。
func TestVolumePlanFailureIsNotBlocked(t *testing.T) {
	src := &fakeVolumeSource{assessErr: errors.New("登记表读不出来")}
	reg := NewRegistry()
	if err := RegisterVolumeActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	rec := postPreflight(t, srv, testWebUID, preflightBody(
		storage.ActionVolumeMount, `{"volume_id":"`+volTestID+`","version":"1"}`))
	if rec.Code == 201 {
		t.Fatalf("判决失败却返回了 201: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PREFLIGHT_FAILED") &&
		!strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Fatalf("错误码不像预检失败: %s", rec.Body.String())
	}
}

// TestVolumeExecuteHandsTargetToWriter 校验执行交接：
//
//   - 交给写原语的请求带着**预检时**的身份指纹与状态版本，不是执行时重算的；
//   - 参数就是预检落库的规范化参数，动作名没有被改写；
//   - 写原语报 verified 之后，操作落到 succeeded，observed_state 里是读回的真实状态。
func TestVolumeExecuteHandsTargetToWriter(t *testing.T) {
	src := &fakeVolumeSource{
		assessment: volumeAssessment(storage.ActionVolumeMount),
		report: storage.WriteReport{
			Observed: map[string]any{
				"volume_id": volTestID,
				"slug":      "data",
				"mounted":   true,
				"uuid":      volTestUUID,
			},
			Data: map[string]any{"volume_id": volTestID, "slug": "data"},
		},
	}
	reg := NewRegistry()
	if err := RegisterVolumeActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	plan := mustPreflight(t, srv, storage.ActionVolumeMount,
		`{"volume_id":"`+volTestID+`","version":"`+volTestVersion+`"}`)
	if !plan.Allowed {
		t.Fatalf("允许的挂载被拒：%+v", plan.Blockers)
	}

	operationID := opID("volume-exec")
	rec := submitOperation(t, srv, operationSubmitBody(
		operationID, plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != 201 {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	done := waitForStage(t, srv, operationID, "succeeded")
	if done.Stage != "succeeded" {
		t.Fatalf("阶段 = %q，期望 succeeded", done.Stage)
	}

	if len(src.writes) != 1 {
		t.Fatalf("写原语被调用了 %d 次", len(src.writes))
	}
	w := src.writes[0]
	if w.Action != storage.ActionVolumeMount {
		t.Errorf("动作 = %q", w.Action)
	}
	if w.ExpectedIdentity != plan.Target.IdentityFingerprint {
		t.Errorf("身份指纹 = %q，期望预检里的 %q", w.ExpectedIdentity, plan.Target.IdentityFingerprint)
	}
	if w.ExpectedStateVersion != plan.Target.StateVersion {
		t.Errorf("状态版本 = %q，期望预检里的 %q", w.ExpectedStateVersion, plan.Target.StateVersion)
	}
	if w.OperationID != operationID {
		t.Errorf("operation_id = %q，期望 %q（对账靠它）", w.OperationID, operationID)
	}
	p, ok := w.Params.(storage.VolumeParams)
	if !ok || p.VolumeID != volTestID || p.Version != volTestVersion {
		t.Fatalf("写原语收到的参数 = %#v", w.Params)
	}
}

// TestVolumeImportDecodeKeepsDisplayNameEmpty 校验显示名可以为空（页面回退到卷名），
// 但空卷名不行——卷名派生挂载点。
func TestVolumeImportDecodeKeepsDisplayNameEmpty(t *testing.T) {
	reg, _ := volumeRegistry(t)
	raw, _ := json.Marshal(map[string]string{"device_id": "sdb1", "slug": "data"})
	p, err := decodeFor(t, reg, storage.ActionVolumeImport, string(raw))
	if err != nil {
		t.Fatalf("没给显示名就被拒了: %v", err)
	}
	if got := p.(storage.VolumeImportParams); got.DisplayName != "" {
		t.Errorf("显示名 = %q，不该被编一个默认值", got.DisplayName)
	}
}
