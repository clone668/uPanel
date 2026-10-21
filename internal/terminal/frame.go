package terminal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// 帧类型闭集（CONTRACTS.md 第 10 节）。
//
// 方向是固定的：
//
//	客户端 → 服务端：input、resize
//	服务端 → 客户端：ready、output、exit、error
//
// “客户端只能发这两类”是服务端的判定，不是前端的自觉：DecodeClientFrame 对任何
// 其他 type 一律拒绝。这条规则的意义在于——如果哪天有人在协议里加了一个
// “run” 之类的帧，它必须先在服务端的闭集里出现，而那时必然会有人问“这东西怎么执行”。
const (
	FrameInput  = "input"
	FrameResize = "resize"
	FrameOutput = "output"
	FrameReady  = "ready"
	FrameExit   = "exit"
	FrameError  = "error"
)

// 帧级错误码。
//
// 它们与 HTTP 错误码（httpx.Code）不是一套东西：一个是“这条 WebSocket 上的
// 这一帧为什么被拒”，一个是“这次 HTTP 请求为什么被拒”。握手之后的失败没有
// 状态码可给，所以必须自己有码，前端才能按码分支而不去匹配文案。
const (
	// CodeFrameInvalid 表示对端发来的帧不合法（未知 type、越界尺寸、非 JSON）。
	CodeFrameInvalid = "FRAME_INVALID"
	// CodeRateLimited 表示输入超过速率上限，连接已被断开。
	CodeRateLimited = "RATE_LIMITED"
)

// WebSocket 关闭码。
//
// 关连接的原因必须能被浏览器看见：只知道“连接断了”的话，一次协议违规、一次超速、
// 一次会话到期和一次网线被拔全长得一样，前端只能笼统地显示“连接已断开”。因此
// 服务端在断开之前发一帧 close，把原因放在关闭码里（应用级 4000—4999 是私有区间，
// 不会与标准码冲突）。
//
// 1000 用于“正常结束”（子进程退出、空闲、到期——这些还会先收到 exit 帧）。
const (
	// CloseCodeNormal 是正常结束。
	CloseCodeNormal = 1000
	// CloseCodeFrameInvalid 是帧不合法（未知 type、越界尺寸、非 JSON、空 input）。
	CloseCodeFrameInvalid = 4001
	// CloseCodeRateLimited 是输入超过速率上限。
	CloseCodeRateLimited = 4002
)

// Frame 是一帧终端消息。
//
// 终端数据用 base64 放进 JSON：WebSocket 本身能传二进制，但这条链路上有两段是
// 文本协议的中继（浏览器的 JS 一侧与服务端日志），用 base64 换来的是“任何一帧都能
// 被原样打印出来看”，代价是三分之一体积。终端流量是敲键盘与滚屏，这个代价可以忽略。
type Frame struct {
	Type    string `json:"type"`
	DataB64 string `json:"data_b64,omitempty"`
	// Cols / Rows 只用于 resize 与 ready。
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// SessionID / Account / AccountUID 只出现在 ready：让客户端知道它连上的是谁。
	SessionID  string `json:"session_id,omitempty"`
	Account    string `json:"account,omitempty"`
	AccountUID uint32 `json:"account_uid,omitempty"`
	// ExitCode 为 nil 表示“被信号终止”或“没有退出码”，不是 0。
	ExitCode *int `json:"exit_code,omitempty"`
	// Signal 是终止会话的信号编号；0 表示没有信号。
	Signal int `json:"signal,omitempty"`
	// Reason 是 exit 帧的结束原因（user、idle、expired、disconnect、shutdown、exit、failed）。
	Reason string `json:"reason,omitempty"`
	// Code / Message 只出现在 error 帧。
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// ServerTime 是服务端产生这一帧的时间（RFC3339 UTC），用于对齐时钟。
	ServerTime string `json:"server_time,omitempty"`
}

// NewOutputFrame 构造一帧终端输出。
func NewOutputFrame(data []byte) Frame {
	return Frame{Type: FrameOutput, DataB64: base64.StdEncoding.EncodeToString(data)}
}

// NewInputFrame 构造一帧终端输入（Web 侧测试与中继使用）。
func NewInputFrame(data []byte) Frame {
	return Frame{Type: FrameInput, DataB64: base64.StdEncoding.EncodeToString(data)}
}

// NewResizeFrame 构造一帧窗口调整。
func NewResizeFrame(cols, rows int) Frame {
	return Frame{Type: FrameResize, Cols: cols, Rows: rows}
}

// EncodeFrame 把帧编码成一条 WebSocket 文本消息。
func EncodeFrame(f Frame) ([]byte, error) {
	body, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("terminal: 帧无法序列化: %w", err)
	}
	return body, nil
}

// DecodeClientFrame 解码一条**客户端**发来的帧。
//
// 严格解码：未知字段、尾部多余内容、未知 type、超限的输入长度、越界的窗口尺寸，
// 全部拒绝。错误信息里不回显原始 base64（那可能是一段粘贴的口令）。
func DecodeClientFrame(payload []byte) (Frame, error) {
	if len(payload) > MaxFrameBytes {
		return Frame{}, fmt.Errorf("%w: 单帧超过 %d 字节", ErrInvalidParams, MaxFrameBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var f Frame
	if err := dec.Decode(&f); err != nil {
		return Frame{}, fmt.Errorf("%w: 帧不是合法的 JSON 对象: %v", ErrInvalidParams, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return Frame{}, fmt.Errorf("%w: 帧在首个对象之后仍有内容", ErrInvalidParams)
	}

	switch f.Type {
	case FrameInput:
		data, err := f.InputData()
		if err != nil {
			return Frame{}, err
		}
		if len(data) == 0 {
			// 空输入没有意义：它会让空闲计时器被“什么都没敲”刷新。
			return Frame{}, fmt.Errorf("%w: input 帧没有数据", ErrInvalidParams)
		}
		return Frame{Type: FrameInput, DataB64: f.DataB64}, nil
	case FrameResize:
		p := Params{Cols: f.Cols, Rows: f.Rows}
		if err := p.Validate(); err != nil {
			return Frame{}, err
		}
		return Frame{Type: FrameResize, Cols: f.Cols, Rows: f.Rows}, nil
	case "":
		return Frame{}, fmt.Errorf("%w: 帧缺少 type", ErrInvalidParams)
	default:
		// 包括 output / ready / exit / error：客户端没有资格发送服务端帧。
		return Frame{}, fmt.Errorf("%w: 客户端不得发送 %q 帧", ErrInvalidParams, f.Type)
	}
}

// InputData 解出 input 帧里的字节，并核对长度上限。
func (f Frame) InputData() ([]byte, error) {
	if f.DataB64 == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(f.DataB64)
	if err != nil {
		return nil, fmt.Errorf("%w: base64 解码失败: %v", ErrInvalidParams, err)
	}
	if len(data) > MaxFramePayload {
		return nil, fmt.Errorf("%w: 单帧数据超过 %d 字节", ErrInvalidParams, MaxFramePayload)
	}
	return data, nil
}

// InputLimiter 是输入速率的令牌桶。
//
// 两个维度分开算：字节数与帧数。只限字节数挡不住“每帧 1 字节、每秒几万帧”；
// 只限帧数挡不住“一帧 64 KiB、每秒 200 帧”。桶是连续的（按时间补齐），
// 因此正常打字（连同一次较大的粘贴）永远不会碰到上限。
type InputLimiter struct {
	bytesPerSecond float64
	framesPerSec   float64
	bytes          float64
	frames         float64
	last           int64 // 单调时钟纳秒，见 SetNow
	filled         bool
}

// NewInputLimiter 构造一个满桶的限速器。
func NewInputLimiter() *InputLimiter {
	return &InputLimiter{
		bytesPerSecond: MaxInputBytesPerSecond,
		framesPerSec:   MaxInputFramesPerSecond,
	}
}

// Consume 判断这一帧是否在限额内；超限时返回 false，调用方必须断开连接。
//
// 参数 nowNanos 由调用方给单调时钟（time.Now().UnixNano() 在 Go 里带单调部分，
// 但这里只要求“单调递增”，测试传假值也能用）。
func (l *InputLimiter) Consume(bytes int, nowNanos int64) bool {
	if !l.filled {
		l.bytes = l.bytesPerSecond
		l.frames = l.framesPerSec
		l.last = nowNanos
		l.filled = true
	} else if nowNanos > l.last {
		elapsed := float64(nowNanos-l.last) / float64(1e9)
		l.bytes += elapsed * l.bytesPerSecond
		l.frames += elapsed * l.framesPerSec
		if l.bytes > l.bytesPerSecond {
			l.bytes = l.bytesPerSecond
		}
		if l.frames > l.framesPerSec {
			l.frames = l.framesPerSec
		}
		l.last = nowNanos
	}
	// 一次最多消费 1 帧与 len 字节：桶容量就是“1 秒的额度”，
	// 因此允许的突发上限是明确的，而不是一个随实现变化的值。
	if l.frames < 1 || l.bytes < float64(bytes) {
		return false
	}
	l.frames--
	l.bytes -= float64(bytes)
	return true
}
