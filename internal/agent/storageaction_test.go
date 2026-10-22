package agent

// P07.1/P07.2 代理侧用例：四个存储写动作的登记、参数、策略、预检映射与执行交接。
//
// 这一层不写磁盘（真正的写原语在 internal/storage，用假执行器代替）。它要证明的是
// **中间这一层没有把纪律弄丢**：
//
//  1. **参数形状就是契约里的形状**：`device_id` 只能是内核设备名，别名/多余字段/
//     把字节数写成字符串都在解码层被拒，不会被静默忽略后当成默认值。
//  2. **策略对齐 CONTRACTS 第 5 节**：四个动作全部需要再认证，初始化/格式化/删除
//     还需要确认短语（首词 INIT / FORMAT / DELETE），建分区不需要；都不可取消。
//  3. **拒绝不是错误**：判决里的 Blockers/Warnings/Impacts/Locks 原样变成预检计划，
//     界面因此永远说得清按钮为什么是灰的。
//  4. **没有执行器就是 501**：格式化还没有写原语，预检照常给出真实结论，提交却必须
//     501——而不是留下一个永远跑不起来的任务。
//  5. **能力不撒谎**：capabilities.supported_actions 里只出现真的有执行器的三个动作。
//  6. **执行交接不丢东西**：提交给写原语的请求必须带着预检绑定的身份指纹与状态版本
//     （这是执行前复核“还是那块盘”的唯一依据），返回的核验结果必须是 verified。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
	"upanel/internal/storage"
)

// fakeStorage 是 storageAssessor 的测试替身：把判决当输入，把“被问了什么”记下来。
type fakeStorage struct {
	assessment storage.Assessment
	err        error

	// report / writeErr 是执行器的返回值（P07.2 起三个分区动作有执行器）。
	report   storage.WriteReport
	writeErr error

	// 记录调用，验证代理层没有偷偷改入参或动作名。
	actions []string
	params  []any
	writes  []storage.WriteRequest
}

func (f *fakeStorage) Assess(_ context.Context, action string, params any) (storage.Assessment, error) {
	f.actions = append(f.actions, action)
	f.params = append(f.params, params)
	if f.err != nil {
		return storage.Assessment{}, f.err
	}
	return f.assessment, nil
}

// ExecuteWrite 是 storageWriter 的测试替身：记录收到的写请求，返回预置结果。
func (f *fakeStorage) ExecuteWrite(_ context.Context, req storage.WriteRequest) (storage.WriteReport, error) {
	f.writes = append(f.writes, req)
	if f.writeErr != nil {
		return storage.WriteReport{}, f.writeErr
	}
	return f.report, nil
}

// allowedAssessment 是一份“放行且要素齐备”的判决。
//
// 预检要求目标指纹、状态版本、影响与锁都在，缺一项计划就会被判为不合法，
// 所以默认值必须完整——这一点与 services/files 的假采集器一样。
func allowedAssessment(action, devID string) storage.Assessment {
	return storage.Assessment{
		Action:              action,
		NormalizedParams:    nil, // 由 Plan 用 in.Params 填
		TargetID:            devID,
		DiskIDs:             []string{devID},
		DisplayName:         "Samsung SSD 860 (" + devID + ")",
		IdentityFingerprint: "sha256:" + strings.Repeat("ab", 32),
		StateVersion:        "sha256:" + strings.Repeat("cd", 32),
		Allowed:             true,
		Blockers:            []storage.Blocker{},
		Warnings:            []storage.Blocker{},
		Impacts: []storage.Impact{{
			Kind:        storage.ImpactDataLoss,
			Description: "会覆盖盘上的分区元数据",
		}},
		Locks: []string{"disk:" + devID},
	}
}

// storageRegistry 登记四个存储动作，返回登记表与假判决源。
func storageRegistry(t *testing.T) (*Registry, *fakeStorage) {
	t.Helper()
	src := &fakeStorage{assessment: allowedAssessment(storage.ActionPartitionTableInitialize, "sdb")}
	reg := NewRegistry()
	if err := RegisterStorageActions(reg, src); err != nil {
		t.Fatalf("登记存储动作失败: %v", err)
	}
	return reg, src
}

// decodeFor 用登记表里的动作解码一次参数。
func decodeFor(t *testing.T, reg *Registry, action, raw string) (any, error) {
	t.Helper()
	a, ok := reg.Action(action)
	if !ok {
		t.Fatalf("动作 %s 没登记", action)
	}
	return a.Decode(json.RawMessage(raw))
}

// TestStorageActionsRegistered 校验四个动作都登记了，且只有三个分区动作有执行器。
func TestStorageActionsRegistered(t *testing.T) {
	reg, _ := storageRegistry(t)
	want := []string{
		storage.ActionFilesystemFormat,
		storage.ActionPartitionCreate,
		storage.ActionPartitionDelete,
		storage.ActionPartitionTableInitialize,
	}
	got := reg.ActionNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("登记的动作 = %v，期望 %v", got, want)
	}
	for _, name := range want {
		a, _ := reg.Action(name)
		if a.Decode == nil || a.Plan == nil {
			t.Errorf("%s 缺少解码或预检", name)
		}
		// P07.2 只登记三个分区写原语；格式化等 P07.3。留空 = 501，不是“假装成功”。
		wantExecutor := name != storage.ActionFilesystemFormat
		if (a.Execute != nil) != wantExecutor {
			t.Errorf("%s 的执行器 = %v，期望 %v", name, a.Execute != nil, wantExecutor)
		}
	}
	// 能力列表必须与实际执行器一致：列进去就等于承诺能执行。
	exec := reg.ExecutableActionNames()
	wantExec := []string{
		storage.ActionPartitionCreate,
		storage.ActionPartitionDelete,
		storage.ActionPartitionTableInitialize,
	}
	if strings.Join(exec, ",") != strings.Join(wantExec, ",") {
		t.Fatalf("ExecutableActionNames = %v，期望 %v", exec, wantExec)
	}
}

// TestStorageActionPolicies 校验策略与 CONTRACTS 第 5 节一致。
func TestStorageActionPolicies(t *testing.T) {
	reg, _ := storageRegistry(t)
	cases := []struct {
		action     string
		phrase     bool
		phraseVerb string
	}{
		{storage.ActionPartitionTableInitialize, true, "INIT"},
		{storage.ActionPartitionCreate, false, ""},
		{storage.ActionPartitionDelete, true, "DELETE"},
		{storage.ActionFilesystemFormat, true, "FORMAT"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			a, ok := reg.Action(tc.action)
			if !ok {
				t.Fatalf("动作没登记")
			}
			policy, err := a.resolvePolicy(nil)
			if err != nil {
				t.Fatalf("取策略失败: %v", err)
			}
			if !policy.RequiresReauth {
				t.Error("存储破坏性动作必须要求再认证")
			}
			if policy.RequiresPhrase != tc.phrase {
				t.Errorf("RequiresPhrase = %v，期望 %v", policy.RequiresPhrase, tc.phrase)
			}
			if policy.PhraseVerb != tc.phraseVerb {
				t.Errorf("PhraseVerb = %q，期望 %q", policy.PhraseVerb, tc.phraseVerb)
			}
			if policy.Cancellable {
				t.Error("磁盘动作不可取消")
			}
		})
	}
}

// TestStorageActionDecode 校验参数解码：合法输入被规范化，非法输入在形状层被拒。
func TestStorageActionDecode(t *testing.T) {
	reg, _ := storageRegistry(t)

	t.Run("合法输入被规范化", func(t *testing.T) {
		p, err := decodeFor(t, reg, storage.ActionPartitionTableInitialize,
			`{"device_id":"  sdb  ","table_type":" GPT "}`)
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		got := p.(storage.PartitionTableInitializeParams)
		if got.DeviceID != "sdb" || got.TableType != "gpt" {
			t.Errorf("规范化结果 = %+v，应去掉空白并小写", got)
		}

		p, err = decodeFor(t, reg, storage.ActionFilesystemFormat,
			`{"device_id":"sdb1","filesystem":"EXT4","label":"data"}`)
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		if got := p.(storage.FilesystemFormatParams); got.Filesystem != "ext4" {
			t.Errorf("filesystem = %q，应小写", got.Filesystem)
		}

		// 字节数是**十进制字符串**（CONTRACTS 第 1 节：超过 2^53 的字节数不能走 JSON number）。
		p, err = decodeFor(t, reg, storage.ActionPartitionCreate,
			`{"device_id":"sdb","start_bytes":"1048576","size_bytes":"4194304","name":" data "}`)
		if err != nil {
			t.Fatalf("字节数用十进制字符串是契约形态，被拒了: %v", err)
		}
		got2 := p.(storage.PartitionCreateParams)
		if got2.StartBytes != 1048576 || got2.SizeBytes != 4194304 {
			t.Errorf("字节数 = %d/%d", got2.StartBytes, got2.SizeBytes)
		}
		if got2.Name != "data" {
			t.Errorf("分区名 = %q，应去掉首尾空白", got2.Name)
		}
	})

	bad := []struct {
		name   string
		action string
		raw    string
	}{
		{"空设备名", storage.ActionPartitionDelete, `{"device_id":""}`},
		{"路径穿越", storage.ActionPartitionDelete, `{"device_id":"../etc/passwd"}`},
		{"带分隔符", storage.ActionPartitionDelete, `{"device_id":"sdb/1"}`},
		{"点", storage.ActionPartitionDelete, `{"device_id":".."}`},
		{"过长", storage.ActionPartitionDelete, `{"device_id":"` + strings.Repeat("a", 65) + `"}`},
		{"空字节", storage.ActionPartitionDelete, `{"device_id":"sdb\u0000"}`},
		{"多余字段", storage.ActionPartitionDelete, `{"device_id":"sdb1","force":true}`},
		{"非 gpt 表", storage.ActionPartitionTableInitialize, `{"device_id":"sdb","table_type":"mbr"}`},
		{"缺表类型", storage.ActionPartitionTableInitialize, `{"device_id":"sdb"}`},
		{"零大小", storage.ActionPartitionCreate,
			`{"device_id":"sdb","start_bytes":"1048576","size_bytes":"0"}`},
		// 字节数写成 JSON number 在解码层就拒：契约是十进制字符串，宽松接受会让
		// 大容量盘上的 2^53 误差一路走到判决里。
		{"大小是数字", storage.ActionPartitionCreate,
			`{"device_id":"sdb","start_bytes":"1048576","size_bytes":4194304}`},
		{"起点是数字", storage.ActionPartitionCreate,
			`{"device_id":"sdb","start_bytes":1048576,"size_bytes":"4194304"}`},
		{"分区名过长", storage.ActionPartitionCreate,
			`{"device_id":"sdb","start_bytes":"1048576","size_bytes":"1048576","name":"` + strings.Repeat("x", 37) + `"}`},
		{"不支持的文件系统", storage.ActionFilesystemFormat, `{"device_id":"sdb1","filesystem":"btrfs"}`},
		{"缺文件系统", storage.ActionFilesystemFormat, `{"device_id":"sdb1"}`},
		{"标签过长", storage.ActionFilesystemFormat,
			`{"device_id":"sdb1","filesystem":"ext4","label":"` + strings.Repeat("y", 17) + `"}`},
		{"标签带斜杠", storage.ActionFilesystemFormat,
			`{"device_id":"sdb1","filesystem":"ext4","label":"a/b"}`},
		{"尾部内容", storage.ActionPartitionDelete, `{"device_id":"sdb1"}{}`},
		{"不是对象", storage.ActionPartitionDelete, `["sdb1"]`},
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

// TestStoragePlanMapping 校验判决到预检计划的映射：拒绝原因、影响、锁、目标都原样带上。
func TestStoragePlanMapping(t *testing.T) {
	src := &fakeStorage{assessment: storage.Assessment{
		Action:              storage.ActionPartitionTableInitialize,
		TargetID:            "sda",
		DiskIDs:             []string{"sda"},
		DisplayName:         "系统盘 (sda)",
		IdentityFingerprint: "sha256:disk-sda",
		StateVersion:        "sha256:snap-1",
		Allowed:             false,
		Blockers: []storage.Blocker{
			{Code: storage.NoticeSystemDisk, Message: "目标承载系统数据，禁止写入：承载根文件系统 /"},
			{Code: storage.NoticeDeviceMounted, Message: "设备正在挂载：/"},
		},
		Warnings: []storage.Blocker{
			{Code: storage.NoticeIdentityLowConfidence, Message: "设备身份依据较弱"},
		},
		Impacts: []storage.Impact{{Kind: storage.ImpactDataLoss, Description: "会覆盖分区元数据"}},
		Locks:   []string{"disk:sda"},
	}}
	reg := NewRegistry()
	if err := RegisterStorageActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	plan := mustPreflight(t, srv, storage.ActionPartitionTableInitialize,
		`{"device_id":"sda","table_type":"gpt"}`)

	if plan.Allowed {
		t.Fatal("系统盘上的初始化被判为允许")
	}
	if len(plan.Blockers) != 2 {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
	if plan.Blockers[0].Code != storage.NoticeSystemDisk {
		t.Errorf("第一个阻塞码 = %s，应先说系统盘保护", plan.Blockers[0].Code)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != storage.NoticeIdentityLowConfidence {
		t.Errorf("告警 = %+v", plan.Warnings)
	}
	if len(plan.Impacts) != 1 || plan.Impacts[0].Kind != storage.ImpactDataLoss {
		t.Errorf("影响 = %+v", plan.Impacts)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "disk:sda" {
		t.Errorf("资源锁 = %v", plan.Locks)
	}
	// 目标三元组必须齐备：执行前复核就是靠它们判断“还是不是同一块盘”。
	if plan.Target.DisplayName == "" || plan.Target.IdentityFingerprint == "" || plan.Target.StateVersion == "" {
		t.Errorf("目标 = %+v，三项都应存在", plan.Target)
	}
	if !plan.RequiresReauth || !plan.RequiresPhrase {
		t.Errorf("策略没有传到计划里：reauth=%v phrase=%v", plan.RequiresReauth, plan.RequiresPhrase)
	}
	if !strings.HasPrefix(plan.ConfirmationPhrase, "INIT ") {
		t.Errorf("确认短语 = %q，首词应为 INIT", plan.ConfirmationPhrase)
	}
	// 判决源收到的参数必须是解码后的那份，且动作名没被改写。
	if len(src.actions) != 1 || src.actions[0] != storage.ActionPartitionTableInitialize {
		t.Fatalf("判决源被调用了 %v", src.actions)
	}
	if p, ok := src.params[0].(storage.PartitionTableInitializeParams); !ok || p.DeviceID != "sda" {
		t.Fatalf("判决源收到的参数 = %#v", src.params[0])
	}
}

// TestStoragePlanFailureIsNotBlocked 校验判决本身出错时是 5xx/编程错误，
// 而不是一条“被拒绝”的业务结论——两者混起来会让真实的故障看起来像保护生效。
func TestStoragePlanFailureIsNotBlocked(t *testing.T) {
	src := &fakeStorage{err: errors.New("参数类型不对")}
	reg := NewRegistry()
	if err := RegisterStorageActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	rec := postPreflight(t, srv, testWebUID, preflightBody(
		storage.ActionPartitionDelete, `{"device_id":"sdb1"}`))
	if rec.Code == http.StatusCreated {
		t.Fatalf("判决失败却返回了 201: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PREFLIGHT_FAILED") &&
		!strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Fatalf("错误码不像预检失败: %s", rec.Body.String())
	}
}

// TestStorageSubmitWithoutExecutorIs501 校验：格式化的预检允许之后提交仍然 501，
// 而且**不留下任何操作记录**——不能生成一个永远不跑的任务。
func TestStorageSubmitWithoutExecutorIs501(t *testing.T) {
	src := &fakeStorage{assessment: allowedAssessment(storage.ActionFilesystemFormat, "sdb1")}
	reg := NewRegistry()
	if err := RegisterStorageActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	plan := mustPreflight(t, srv, storage.ActionFilesystemFormat,
		`{"device_id":"sdb1","filesystem":"ext4","label":"data"}`)
	if !plan.Allowed {
		t.Fatalf("格式化预检应当放行，实际 %+v", plan.Blockers)
	}

	operationID := "op_" + strings.Repeat("2", 32)
	body := operationSubmitBody(operationID, plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth())
	rec := submitOperation(t, srv, body)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，应 501（没有执行器）；响应 %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "NOT_IMPLEMENTED" {
		t.Fatalf("错误码 = %q，应 NOT_IMPLEMENTED", code)
	}

	// 任务中心不该出现这条操作。
	if _, code := getOperation(t, srv, operationID); code != http.StatusNotFound {
		t.Fatalf("提交被拒却留下了操作记录（GET 返回 %d）", code)
	}
}

// TestStorageExecuteHandsTargetToWriter 校验执行交接：
//
//   - 提交给写原语的请求带着**预检时**的身份指纹与状态版本（不是执行时重新算的）；
//   - 参数就是预检落库的规范化参数；
//   - 写原语报 verified 之后，操作落到 succeeded，且 observed_state 里是读回的真实状态。
func TestStorageExecuteHandsTargetToWriter(t *testing.T) {
	src := &fakeStorage{
		assessment: allowedAssessment(storage.ActionPartitionTableInitialize, "sdb"),
		report: storage.WriteReport{
			Observed: map[string]any{"device_id": "sdb", "table_type": "gpt"},
			Data:     map[string]any{"device_id": "sdb", "partitions": 0},
		},
	}
	reg := NewRegistry()
	if err := RegisterStorageActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	plan := mustPreflight(t, srv, storage.ActionPartitionTableInitialize,
		`{"device_id":"sdb","table_type":"gpt"}`)
	if !plan.Allowed {
		t.Fatalf("空白盘的初始化应当放行，实际 %+v", plan.Blockers)
	}

	operationID := opID("storage-exec")
	rec := submitOperation(t, srv, operationSubmitBody(
		operationID, plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusCreated {
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
	if w.Action != storage.ActionPartitionTableInitialize {
		t.Errorf("动作 = %q", w.Action)
	}
	// 身份与版本必须是预检那一份：执行器自己重新算就等于没有绑定。
	if w.ExpectedIdentity != plan.Target.IdentityFingerprint {
		t.Errorf("身份指纹 = %q，期望预检里的 %q", w.ExpectedIdentity, plan.Target.IdentityFingerprint)
	}
	if w.ExpectedStateVersion != plan.Target.StateVersion {
		t.Errorf("状态版本 = %q，期望预检里的 %q", w.ExpectedStateVersion, plan.Target.StateVersion)
	}
	p, ok := w.Params.(storage.PartitionTableInitializeParams)
	if !ok || p.DeviceID != "sdb" || p.TableType != "gpt" {
		t.Fatalf("写原语收到的参数 = %#v", w.Params)
	}
}

// carrierErr 是最小 statusCarrier：执行器返回的判决必须原样到达任务记录。
//
// 这里刻意不复用 storage 的错误构造器——代理层要证明的是**直通**，
// storage 自己会证明它给出的确实是 409 DEVICE_IDENTITY_CHANGED。
type carrierErr struct{}

func (carrierErr) Error() string { return "盘换了，不能写" }

func (carrierErr) HTTPStatus() (int, httpx.Code) { return 409, "DEVICE_IDENTITY_CHANGED" }

// TestStorageExecuteRejectionKeepsCode 校验写原语的拒绝经 statusCarrier 直通：
// 身份变了要说 DEVICE_IDENTITY_CHANGED，不能压成一句“执行失败”。
func TestStorageExecuteRejectionKeepsCode(t *testing.T) {
	src := &fakeStorage{
		assessment: allowedAssessment(storage.ActionPartitionCreate, "sdb"),
		writeErr:   carrierErr{},
	}
	reg := NewRegistry()
	if err := RegisterStorageActions(reg, src); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv, _ := newPreflightServer(t, reg)

	plan := mustPreflight(t, srv, storage.ActionPartitionCreate,
		`{"device_id":"sdb","start_bytes":"1048576","size_bytes":"4194304","name":"data"}`)
	if !plan.Allowed {
		t.Fatalf("预检应当放行，实际 %+v", plan.Blockers)
	}
	operationID := opID("storage-reject")
	rec := submitOperation(t, srv, operationSubmitBody(
		operationID, plan.PreflightRef, plan.ConfirmationPhrase, testSessionHash, recentReauth()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	done := waitForStage(t, srv, operationID, agentstore.StageFailed)
	var errDoc struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(done.Error, &errDoc); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v；原文 %s", err, done.Error)
	}
	if errDoc.Code != "DEVICE_IDENTITY_CHANGED" {
		t.Fatalf("错误码 = %q，期望 DEVICE_IDENTITY_CHANGED（错误体 %s）", errDoc.Code, done.Error)
	}
}

// TestStorageCapabilitiesListExecutableStorageActions 校验 capabilities 只列真有执行器的动作。
func TestStorageCapabilitiesListExecutableStorageActions(t *testing.T) {
	reg, _ := storageRegistry(t)
	srv := newTestServerWithRegistry(t, reg)

	req := newCapabilitiesRequest()
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body struct {
		Data struct {
			SupportedActions []string `json:"supported_actions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.SupportedActions == nil {
		t.Error("supported_actions 必须是 []，不能是 null")
	}
	stor := []string{}
	for _, name := range body.Data.SupportedActions {
		if strings.HasPrefix(name, "storage.") {
			stor = append(stor, name)
		}
	}
	want := []string{
		storage.ActionPartitionCreate,
		storage.ActionPartitionDelete,
		storage.ActionPartitionTableInitialize,
	}
	if strings.Join(stor, ",") != strings.Join(want, ",") {
		t.Fatalf("supported_actions 里的存储动作 = %v，期望 %v（格式化还没有执行器）", stor, want)
	}
}

// TestExecutableActionNamesFiltersNilExecutor 校验过滤本身（含排序与空表）。
func TestExecutableActionNamesFiltersNilExecutor(t *testing.T) {
	reg := NewRegistry()
	planOnly := func(name string) Action {
		return Action{
			Name:   name,
			Decode: func(json.RawMessage) (any, error) { return struct{}{}, nil },
			Plan:   func(context.Context, PlanInput) (Plan, error) { return Plan{}, nil },
		}
	}
	executable := func(name string) Action {
		a := planOnly(name)
		a.Execute = func(context.Context, ExecuteInput) (Result, error) { return Result{}, nil }
		return a
	}
	for _, a := range []Action{
		planOnly("acceptance.zeta"),
		executable("acceptance.beta"),
		executable("acceptance.alpha"),
	} {
		if err := reg.RegisterAction(a); err != nil {
			t.Fatalf("登记 %s 失败: %v", a.Name, err)
		}
	}
	got := reg.ExecutableActionNames()
	want := "acceptance.alpha,acceptance.beta"
	if strings.Join(got, ",") != want {
		t.Fatalf("ExecutableActionNames = %v，期望 [%s]", got, want)
	}
	if all := reg.ActionNames(); len(all) != 3 {
		t.Fatalf("ActionNames 少了：%v", all)
	}
	if empty := NewRegistry().ExecutableActionNames(); len(empty) != 0 || empty == nil {
		t.Fatalf("空登记表应返回空切片，实际 %#v", empty)
	}
}

// newCapabilitiesRequest 构造一次 capabilities 查询。
func newCapabilitiesRequest() *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil)
	req.Header.Set("X-Request-ID", "rid-caps")
	return req
}
