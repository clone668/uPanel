package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDPassthrough(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderRequestID, "abc-123_ok.id")
	if got := RequestID(r); got != "abc-123_ok.id" {
		t.Fatalf("合法 request id 应被原样保留，实际 %q", got)
	}
}

func TestRequestIDRejectsUnsafeValues(t *testing.T) {
	bad := []string{
		strings.Repeat("a", maxRequestIDLen+1),
		"has space",
		"line\nbreak",
		"<script>",
		"中文",
	}
	for _, v := range bad {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(HeaderRequestID, v)
		got := RequestID(r)
		if got == v {
			t.Fatalf("不安全的 request id %q 不应被复用", v)
		}
		if len(got) == 0 {
			t.Fatal("必须生成新的 request id")
		}
	}
}

func TestRequestIDGeneratedIsUnique(t *testing.T) {
	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if RequestID(r1) == RequestID(r2) {
		t.Fatal("两次生成不应相同")
	}
}

func TestWriteDataEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderRequestID, "rid-1")

	WriteData(w, r, http.StatusOK, map[string]string{"status": "ok"})

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if got := w.Header().Get(HeaderRequestID); got != "rid-1" {
		t.Fatalf("响应头 request id = %q", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	var body struct {
		Data      map[string]string `json:"data"`
		RequestID string            `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.RequestID != "rid-1" {
		t.Fatalf("body.request_id = %q", body.RequestID)
	}
	if body.Data["status"] != "ok" {
		t.Fatalf("data = %v", body.Data)
	}
}

func TestWriteErrorEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	WriteError(w, r, http.StatusNotFound, CodeNotFound, "资源不存在")

	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d", w.Code)
	}
	var body ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Error.Code != CodeNotFound || body.Error.Message != "资源不存在" {
		t.Fatalf("错误体 = %+v", body.Error)
	}
	if body.RequestID == "" {
		t.Fatal("错误响应必须带 request_id")
	}
	if strings.Contains(w.Body.String(), "goroutine") {
		t.Fatal("错误响应不得包含堆栈")
	}
}
