package api

// network.confirm 的展开（P14.2）。
//
// 这里只证明一件事：**到达地址不可能由调用方决定**。
//   - 前端给了 arrived_on → 400（未知字段），不是"忽略掉再覆盖"；
//   - 连接上没有本地地址 → 500 且不转发给代理（宁可这次确认失败，120 秒后自动回滚）；
//   - 正常请求 → 代理收到的是从这条连接解出来的地址，写成与网卡地址同一种写法。

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/network"
)

const testRollbackToken = "nrb_0123456789abcdef0123456789abcdef"

// confirmParams 是调用方发来的那一份（只有两个字段）。
func confirmParams(iface, token string) string {
	return `{"interface_id":"` + iface + `","token":"` + token + `"}`
}

func decodeConfirm(t *testing.T, raw json.RawMessage) network.ConfirmParams {
	t.Helper()
	var out network.ConfirmParams
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("展开后的确认参数无法解析: %v（%s）", err, raw)
	}
	return out
}

// TestConfirmExpansionInjectsArrivedOn：到达地址来自连接，并且是 IPv4 点分写法。
func TestConfirmExpansionInjectsArrivedOn(t *testing.T) {
	f, agent := expandFixture(t)

	rec := f.doAsFrom(t, f.cookie, f.csrf, "192.168.1.50", http.MethodPost,
		"/api/v1/operations/preflight",
		preflightBody(network.ActionConfirm, confirmParams("ens4", testRollbackToken)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	if len(agent.calls) != 1 {
		t.Fatalf("代理收到 %d 次预检，期望 1", len(agent.calls))
	}
	got := decodeConfirm(t, agent.calls[0].Params)
	if got.ArrivedOn != "192.168.1.50" {
		t.Fatalf("到达地址 = %q，期望 192.168.1.50", got.ArrivedOn)
	}
	if got.InterfaceID != "ens4" || got.Token != testRollbackToken {
		t.Fatalf("调用方给的字段被改动了：%+v", got)
	}
}

// TestConfirmExpansionRejectsClientArrivedOn：这个字段不归调用方。
func TestConfirmExpansionRejectsClientArrivedOn(t *testing.T) {
	f, agent := expandFixture(t)

	rec := f.doAsFrom(t, f.cookie, f.csrf, "192.168.1.50", http.MethodPost,
		"/api/v1/operations/preflight",
		preflightBody(network.ActionConfirm,
			`{"interface_id":"ens4","token":"`+testRollbackToken+`","arrived_on":"192.168.1.50"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（到达地址不归调用方）", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeValidationError) {
		t.Fatalf("错误码 = %q", code)
	}
	if len(agent.calls) != 0 {
		t.Fatal("参数不合法时不该打扰代理")
	}
}

// TestConfirmExpansionRejectsBadParams：缺网卡名、缺/坏回滚标识都在 Web 侧就拒。
func TestConfirmExpansionRejectsBadParams(t *testing.T) {
	cases := []string{
		`{"token":"` + testRollbackToken + `"}`, // 缺 interface_id
		`{"interface_id":"","token":"` + testRollbackToken + `"}`,
		`{"interface_id":"ens4"}`,                   // 缺 token
		`{"interface_id":"ens4","token":"abc"}`,     // 形状不对
		`{"interface_id":"ens4","token":"nrb_XYZ"}`, // 大写不在允许字符里
	}
	for _, params := range cases {
		f, agent := expandFixture(t)
		rec := f.doAsFrom(t, f.cookie, f.csrf, "192.168.1.50", http.MethodPost,
			"/api/v1/operations/preflight", preflightBody(network.ActionConfirm, params))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400", params, rec.Code)
		}
		if len(agent.calls) != 0 {
			t.Fatalf("%s 不该转发给代理", params)
		}
	}
}

// TestConfirmExpansionRequiresLocalAddress：拿不到"你从哪连过来"就不许确认。
//
// 这一条的方向很重要：确认失败只意味着 120 秒后自动回滚（现场回到原样），
// 而没有凭据的确认成功意味着新配置被永久留下——两个错误的代价不对称。
func TestConfirmExpansionRequiresLocalAddress(t *testing.T) {
	f, agent := expandFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody(network.ActionConfirm, confirmParams("ens4", testRollbackToken)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（我们这一侧取不到凭据）", rec.Code)
	}
	if len(agent.calls) != 0 {
		t.Fatal("没有到达地址就不该转发给代理")
	}
}

// TestRequestLocalAddressNormalizes：`::ffff:` 与 IPv6 zone 都归一成与网卡地址一致的写法。
//
// 不归一的话，双栈监听下的确认会永远失败（代理拿 `192.168.1.50` 与
// `::ffff:192.168.1.50` 比字符串）。失败的方向是自动回滚——管理员好不容易在新地址上
// 连上，却怎么也确认不了。
func TestRequestLocalAddressNormalizes(t *testing.T) {
	cases := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("192.168.1.50"), Port: 443}, "192.168.1.50"},
		{&net.TCPAddr{IP: net.ParseIP("::ffff:192.168.1.50"), Port: 443}, "192.168.1.50"},
		{&net.TCPAddr{IP: net.ParseIP("fd00::1"), Port: 443}, "fd00::1"},
		{&net.TCPAddr{IP: net.ParseIP("fe80::1"), Zone: "ens4", Port: 443}, "fe80::1"},
	}
	for _, c := range cases {
		req := newRequestWithLocalAddr(t, c.addr)
		got, err := requestLocalAddress(req)
		if err != nil {
			t.Fatalf("%v: %v", c.addr, err)
		}
		if got != c.want {
			t.Fatalf("%v → %q，期望 %q", c.addr, got, c.want)
		}
	}
}

// TestRequestLocalAddressWithoutKey：上下文里没有这个键时必须报错，不回空串。
func TestRequestLocalAddressWithoutKey(t *testing.T) {
	req := newRequestWithLocalAddr(t, nil)
	if got, err := requestLocalAddress(req); err == nil {
		t.Fatalf("没有本地地址应当报错，得到 %q", got)
	}
}

func newRequestWithLocalAddr(t *testing.T, addr net.Addr) *gin.Context {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations/preflight", nil)
	if addr != nil {
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}
