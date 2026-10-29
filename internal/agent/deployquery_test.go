package agent

// 部署现场查询的接线测试（P15.4）：deployments.list / deployments.get。
//
// 这一层只测三件事：参数按形状拒绝、"没有这份部署"是 404 而不是 500、
// 以及**没有实现端就不登记**（capabilities 不许撒谎）。现场怎么读、
// 记录坏了怎么办，在 internal/deploy/inspect_test.go 里各自有测试。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"upanel/internal/deploy"
	"upanel/internal/httpx"
)

// fakeDeployInspector 是 DeployInspector 的替身：按名字给现场或给错。
type fakeDeployInspector struct {
	list    deploy.InspectionList
	listErr error
	items   map[string]deploy.Inspection
	errs    map[string]error
	asked   []string
}

func (f *fakeDeployInspector) List(context.Context) (deploy.InspectionList, error) {
	if f.listErr != nil {
		return deploy.InspectionList{}, f.listErr
	}
	return f.list, nil
}

func (f *fakeDeployInspector) Inspect(_ context.Context, name string) (deploy.Inspection, error) {
	f.asked = append(f.asked, name)
	if err, ok := f.errs[name]; ok {
		return deploy.Inspection{}, err
	}
	item, ok := f.items[name]
	if !ok {
		return deploy.Inspection{}, deploy.ErrNoDeployment
	}
	return item, nil
}

func deployQuerySource() *fakeDeployInspector {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	item := deploy.Inspection{
		Name:          "webapp",
		DeploymentID:  deploy.DeploymentIDForName("webapp"),
		ProjectName:   "webapp",
		UnitName:      "upanel-deploy-webapp.service",
		ConfigPresent: true,
		ConfigDigest:  "sha256:" + repeatHex("a", 64),
		Version:       "sha256:" + repeatHex("a", 64),
		RecordPresent: true,
		Record: &deploy.DeploymentRecord{
			Schema:       deploy.RecordSchema,
			DeploymentID: deploy.DeploymentIDForName("webapp"),
			Name:         "webapp",
			Definition:   deploy.Definition{Name: "webapp", ImageReference: "nginx:1.27"},
			ConfigDigest: "sha256:" + repeatHex("a", 64),
			Image: deploy.ImageRecord{Requested: "nginx:1.27", Digest: "sha256:" + repeatHex("b", 64),
				ConfigDigest: "sha256:" + repeatHex("a", 64), ResolvedAt: at},
			UpdatedAt: at,
		},
		Unit: deploy.UnitState{Known: true, Enabled: true, Active: true},
	}
	return &fakeDeployInspector{
		list:  deploy.InspectionList{Deployments: []deploy.Inspection{item}},
		items: map[string]deploy.Inspection{"webapp": item},
		errs: map[string]error{
			"broken": errors.New("deploy: 读取部署配置目录失败: 权限不足"),
		},
	}
}

func repeatHex(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func deployQueryServer(t *testing.T, src DeployInspector) *Server {
	t.Helper()
	reg, err := NewProductionRegistry(ProductionDeps{DeployInspector: src})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	return newTestServerWithRegistry(t, reg)
}

// TestDeploymentsListQuery 列表原样返回，含 skip 与截断标记。
func TestDeploymentsListQuery(t *testing.T) {
	src := deployQuerySource()
	src.list.Skipped = []deploy.SkippedEntry{{Name: "Bad_Name", Reason: "名字不合规"}}
	srv := deployQueryServer(t, src)
	rec := postQuery(t, srv, `{"query":"`+deploy.QueryListName+`"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deploy.InspectionList `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Deployments) != 1 || body.Data.Deployments[0].Name != "webapp" {
		t.Fatalf("列表 = %+v", body.Data.Deployments)
	}
	if body.Data.Deployments[0].Version == "" {
		t.Fatal("version 空了：更新与删除都带不回令牌")
	}
	if len(body.Data.Skipped) != 1 || body.Data.Skipped[0].Name != "Bad_Name" {
		t.Fatalf("skipped = %+v", body.Data.Skipped)
	}
}

// TestDeploymentsListQueryRejectsParams 只读查询带了参数就是 400，不是静默忽略。
func TestDeploymentsListQueryRejectsParams(t *testing.T) {
	srv := deployQueryServer(t, deployQuerySource())
	for _, raw := range []string{
		`{"query":"` + deploy.QueryListName + `","params":{"refresh":true}}`,
		`{"query":"` + deploy.QueryListName + `","params":{"path":"/etc"}}`,
		`{"query":"` + deploy.QueryListName + `","params":[]}`,
	} {
		rec := postQuery(t, srv, raw, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400", raw, rec.Code)
		}
	}
}

// TestDeploymentsGetQuery 单条：正常、400、404。
func TestDeploymentsGetQuery(t *testing.T) {
	src := deployQuerySource()
	srv := deployQueryServer(t, src)

	rec := postQuery(t, srv, `{"query":"`+deploy.QueryGetName+`","params":{"name":"webapp"}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deploy.Inspection `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Name != "webapp" || body.Data.Record == nil || !body.Data.Unit.Enabled {
		t.Fatalf("现场 = %+v", body.Data)
	}

	// 没有这份部署：404，不是 500。压成 500 会让 Web 把"已经删掉了"翻译成"代理不可用"。
	rec = postQuery(t, srv, `{"query":"`+deploy.QueryGetName+`","params":{"name":"nothing"}}`, testWebUID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if errBody.Error.Code != string(httpx.CodeNotFound) {
		t.Fatalf("错误码 = %q", errBody.Error.Code)
	}

	// 名字不合规：400（参数问题），在解码阶段就拒。
	for _, raw := range []string{
		`{"query":"` + deploy.QueryGetName + `","params":{"name":"Bad Name"}}`,
		`{"query":"` + deploy.QueryGetName + `","params":{"name":"../etc/passwd"}}`,
		`{"query":"` + deploy.QueryGetName + `","params":{"name":""}}`,
		`{"query":"` + deploy.QueryGetName + `","params":{}}`,
	} {
		rec := postQuery(t, srv, raw, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400", raw, rec.Code)
		}
	}
	// 这些都没进到实现端：参数不合规不该读盘。
	for _, name := range src.asked {
		if name != "webapp" && name != "nothing" {
			t.Fatalf("非法名字被带进了实现端：%q", name)
		}
	}
}

// TestDeploymentsGetQueryInternalErrorIs500 读盘失败是 500，不是 404：
// 那是"读不出来"，不是"没有这份部署"。
func TestDeploymentsGetQueryInternalErrorIs500(t *testing.T) {
	srv := deployQueryServer(t, deployQuerySource())
	rec := postQuery(t, srv, `{"query":"`+deploy.QueryGetName+`","params":{"name":"broken"}}`, testWebUID)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（%s）", rec.Code, rec.Body.String())
	}
}

// TestDeploymentQueriesNotRegisteredWithoutSource 没有实现端就不登记：
// capabilities 因此不会报告一个点开就报错的入口。
func TestDeploymentQueriesNotRegisteredWithoutSource(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range reg.QueryNames() {
		if name == deploy.QueryListName || name == deploy.QueryGetName {
			t.Fatalf("没有读取端却登记了 %s", name)
		}
	}
	withSource, err := NewProductionRegistry(ProductionDeps{DeployInspector: deployQuerySource()})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	seen := map[string]bool{}
	for _, name := range withSource.QueryNames() {
		seen[name] = true
	}
	if !seen[deploy.QueryListName] || !seen[deploy.QueryGetName] {
		t.Fatalf("有了读取端却没登记两条查询：%v", withSource.QueryNames())
	}
}
