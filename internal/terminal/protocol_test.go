package terminal

// 协议层用例：客户端帧的严格性、输入限速、会话 id 与限额。
//
// 这些是**代理侧真正执行**的规则，与平台无关，因此在任何开发机上都能跑。
// PTY、降权、超时回收需要 Linux 与真实账号，由 internal/terminal 的 Linux
// 用例与 Ubuntu VM 验收覆盖；这里不冒充。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestDecodeClientFrameAcceptsInputAndResize 只有 input 与 resize 能过。
func TestDecodeClientFrameAcceptsInputAndResize(t *testing.T) {
	in, err := EncodeFrame(NewInputFrame([]byte("ls\n")))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	frame, err := DecodeClientFrame(in)
	if err != nil {
		t.Fatalf("解 input 帧失败: %v", err)
	}
	if frame.Type != FrameInput {
		t.Fatalf("type = %q", frame.Type)
	}
	data, err := frame.InputData()
	if err != nil || string(data) != "ls\n" {
		t.Fatalf("输入数据 = %q，err = %v", data, err)
	}

	resize, err := EncodeFrame(NewResizeFrame(120, 40))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	frame, err = DecodeClientFrame(resize)
	if err != nil {
		t.Fatalf("解 resize 帧失败: %v", err)
	}
	if frame.Cols != 120 || frame.Rows != 40 {
		t.Fatalf("尺寸 = %dx%d", frame.Cols, frame.Rows)
	}
}

// TestDecodeClientFrameRejectsServerFrames 客户端不得发送服务端帧。
//
// 这条用例对应 PLAN.md 第 4 节：终端没有“执行”这类帧。如果哪天有人往协议里
// 加了一个能表达“运行这个”的帧，它必须先过这里，而这里只放行两条。
func TestDecodeClientFrameRejectsServerFrames(t *testing.T) {
	for _, payload := range []string{
		`{"type":"output","data_b64":"aGk="}`,
		`{"type":"ready"}`,
		`{"type":"exit","exit_code":0}`,
		`{"type":"error","code":"X"}`,
		`{"type":"run","command":"id"}`,
		`{"type":"exec","data_b64":"aWQ="}`,
		`{}`,
		`{"type":""}`,
	} {
		if _, err := DecodeClientFrame([]byte(payload)); err == nil {
			t.Fatalf("%s 被接受了", payload)
		} else if !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%s 的错误 = %v，期望 ErrInvalidParams", payload, err)
		}
	}
}

// TestDecodeClientFrameRejectsMalformedFrames 畸形帧全部拒绝，且错误不回显内容。
func TestDecodeClientFrameRejectsMalformedFrames(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"非 JSON", `not json`},
		{"尾部多余内容", `{"type":"input","data_b64":"aGk="}{"type":"input"}`},
		{"未知字段", `{"type":"input","data_b64":"aGk=","cmd":"id"}`},
		{"base64 不合法", `{"type":"input","data_b64":"!!"}`},
		{"空输入", `{"type":"input","data_b64":""}`},
		{"尺寸为 0", `{"type":"resize","cols":0,"rows":24}`},
		{"尺寸过大", `{"type":"resize","cols":501,"rows":24}`},
		{"尺寸为负", `{"type":"resize","cols":80,"rows":-1}`},
	}
	for _, tc := range cases {
		_, err := DecodeClientFrame([]byte(tc.payload))
		if err == nil {
			t.Fatalf("%s：被接受了", tc.name)
		}
		if !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%s：错误 = %v", tc.name, err)
		}
		// 错误文案里不能出现原始载荷：那可能是一段粘贴的口令。
		if strings.Contains(err.Error(), "aGk=") {
			t.Fatalf("%s：错误回显了帧内容: %v", tc.name, err)
		}
	}
}

// TestDecodeClientFrameSizeLimit 单帧上限是 64 KiB 数据（base64 后加信封余量）。
func TestDecodeClientFrameSizeLimit(t *testing.T) {
	big := NewInputFrame(make([]byte, MaxFramePayload+1))
	payload, err := EncodeFrame(big)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if _, err := DecodeClientFrame(payload); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("超限帧的错误 = %v", err)
	}

	// 刚好 64 KiB 的输入是合法的。
	exact := NewInputFrame(make([]byte, MaxFramePayload))
	payload, err = EncodeFrame(exact)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if _, err := DecodeClientFrame(payload); err != nil {
		t.Fatalf("64 KiB 输入被拒绝了: %v", err)
	}
}

// TestInputLimiter 字节与帧两个维度都要限：正常打字永远进不去限额。
func TestInputLimiter(t *testing.T) {
	const t0 = int64(1_000_000_000)
	limiter := NewInputLimiter()

	// 正常打字：每 100 ms 一个 8 字节的按键，跑 3 秒。
	now := t0
	for i := 0; i < 30; i++ {
		if !limiter.Consume(8, now) {
			t.Fatalf("第 %d 次正常输入被限速", i)
		}
		now += int64(100 * time.Millisecond)
	}

	// 帧数洪水：桶满时 200 帧/秒；一秒内第 201 帧必须被拒。
	limiter = NewInputLimiter()
	now = t0
	accepted := 0
	for i := 0; i < MaxInputFramesPerSecond+1; i++ {
		if limiter.Consume(1, now) {
			accepted++
		}
	}
	if accepted != MaxInputFramesPerSecond {
		t.Fatalf("一秒内接受的帧数 = %d，期望 %d", accepted, MaxInputFramesPerSecond)
	}

	// 字节洪水：一次 256 KiB 可以（桶容量就是一秒额度），再来 1 字节不行。
	limiter = NewInputLimiter()
	if !limiter.Consume(MaxInputBytesPerSecond, t0) {
		t.Fatalf("一整个桶的字节被拒了")
	}
	if limiter.Consume(1, t0) {
		t.Fatalf("桶已空却仍接受输入")
	}
	// 时间过去 1 秒后额度补齐。
	if !limiter.Consume(MaxInputBytesPerSecond, t0+int64(time.Second)) {
		t.Fatalf("一秒后额度没有补齐")
	}
}

// TestIsValidSessionID 会话 id 形态：32 位小写十六进制。
func TestIsValidSessionID(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef"
	if !IsValidSessionID(good) {
		t.Fatalf("%q 应当合法", good)
	}
	for _, bad := range []string{
		"",
		"0123456789ABCDEF0123456789ABCDEF",
		good[:31],
		good + "0",
		"0123456789abcdef0123456789abcdeg",
		"../0123456789abcdef0123456789ab",
	} {
		if IsValidSessionID(bad) {
			t.Fatalf("%q 不该合法", bad)
		}
	}
}

// TestLimitsCannotBeLoosened 配置只能收紧，不能放宽契约上限。
func TestLimitsCannotBeLoosened(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatalf("默认限额不合法: %v", err)
	}

	tight := DefaultLimits()
	tight.IdleTimeout = 60 * time.Second
	tight.MaxDuration = 5 * time.Minute
	tight.DisconnectGrace = 5 * time.Second
	if err := tight.Validate(); err != nil {
		t.Fatalf("收紧后的限额被拒: %v", err)
	}

	for name, mutate := range map[string]func(*Limits){
		"空闲时限放宽":  func(l *Limits) { l.IdleTimeout = ContractIdleTimeout + time.Second },
		"最长会话放宽":  func(l *Limits) { l.MaxDuration = ContractMaxDuration + time.Second },
		"断流宽限放宽":  func(l *Limits) { l.DisconnectGrace = ContractDisconnectGrace + time.Second },
		"单用户上限放宽": func(l *Limits) { l.MaxPerUser = ContractMaxPerUser + 1 },
		"全局上限放宽":  func(l *Limits) { l.MaxSessions = ContractMaxSessions + 1 },
		"时限为零":    func(l *Limits) { l.IdleTimeout = 0 },
		"单用户多于全局": func(l *Limits) { l.MaxSessions = 1; l.MaxPerUser = 2 },
	} {
		loose := DefaultLimits()
		mutate(&loose)
		if err := loose.Validate(); err == nil {
			t.Fatalf("%s：不合法限额被接受了", name)
		}
	}
}

// TestParamsValidate 窗口尺寸限 1—500，且必须显式给出。
func TestParamsValidate(t *testing.T) {
	if err := (Params{Cols: 80, Rows: 24}).Validate(); err != nil {
		t.Fatalf("默认尺寸被拒: %v", err)
	}
	for _, p := range []Params{
		{Cols: 0, Rows: 24},
		{Cols: 80, Rows: 0},
		{Cols: 501, Rows: 24},
		{Cols: 80, Rows: 501},
		{Cols: -1, Rows: 24},
	} {
		if err := p.Validate(); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%+v 的错误 = %v", p, err)
		}
	}
}

// TestHTTPStatusOf 错误到状态码的映射是**唯一一份**，两侧共用。
func TestHTTPStatusOf(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{ErrInvalidParams, 400},
		{ErrNotFound, 404},
		{ErrNotOwner, 403},
		{ErrLimitReached, 409},
		{ErrAlreadyAttached, 409},
		{ErrClosed, 409},
		{ErrUnsupported, 501},
		{ErrAccountInvalid, 501},
	}
	for _, tc := range cases {
		got, ok := HTTPStatusOf(tc.err)
		if !ok {
			t.Fatalf("%v 没有被映射", tc.err)
		}
		if got.Status != tc.status {
			t.Fatalf("%v 的状态码 = %d，期望 %d", tc.err, got.Status, tc.status)
		}
	}
	// 未分类的错误不给兜底判决：一个没被分类的失败应该看起来像失败。
	if _, ok := HTTPStatusOf(errors.New("其它错误")); ok {
		t.Fatalf("未分类错误被映射了")
	}
	if _, ok := HTTPStatusOf(nil); ok {
		t.Fatalf("nil 被映射了")
	}
}
