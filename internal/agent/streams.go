package agent

// P05.3：受限流许可（CONTRACTS.md 第 15 节）。
//
// 契约把流式数据传输拆成两步：
//
//	POST /internal/v1/streams      → 创建许可：用途 + 参数，绑定成一次性的 id；
//	GET  /internal/v1/streams/:id  → 单次受限流：再次核验对端凭据，按用途开流。
//
// 为什么不在一个请求里直接把流转起来：许可把“要做什么”和“开始传”分开，于是
// 参数校验、用途白名单、时长/条数/字节限额都在**发头之前**完成，而且许可只能用一次。
// 后续的文件上传下载与终端（P06）会复用同一条通道，所以这里按“用途”抽象，
// 而不是写一个只服务于日志的专用接口。
//
// 许可只存在内存里：它的寿命是 30 秒，跨重启保留一个“别人可能已经拿到的凭据”
// 没有任何好处，反而让“一次性”变成一句空话。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/journal"
)

const (
	// StreamPurposeLogs 是日志实时流的用途名。
	StreamPurposeLogs = "logs.stream"
	// streamPermitTTL 是许可的有效期。它只需要覆盖“Web 收到 id 后立刻发起 GET”
	// 这一小段时间；给得越长，泄漏的 id 越有用。
	streamPermitTTL = 30 * time.Second
	// streamMaxPermits 是同时存活的许可上限。许可是一次性的、30 秒过期，
	// 正常情况下不会有几个；这个上限是用来兜住“有人疯狂创建许可”的。
	streamMaxPermits = 32
	// streamWriteTimeout 是单次写流的时间上限。代理的 http.Server 有 15 秒
	// WriteTimeout，长连接必须每次写之前自己往后推截止时间。
	streamWriteTimeout = 10 * time.Second
)

var (
	// ErrStreamPermitNotFound 表示没有这个许可（不存在、已用过或已过期）。
	// 三种情况对外统一成同一个错误：区分它们只会告诉调用方“你的 id 曾经存在过”。
	ErrStreamPermitNotFound = errors.New("agent: 流许可不存在或已失效")
	// ErrStreamPermitLimit 表示存活许可过多。
	ErrStreamPermitLimit = errors.New("agent: 流许可数量超过上限")
	// ErrStreamPurposeUnknown 表示用途不在白名单里。
	ErrStreamPurposeUnknown = errors.New("agent: 未登记的流用途")
	// errStreamWriteFailed 表示对端已经写不动了（断开或超时）。
	errStreamWriteFailed = errors.New("agent: 流写入失败")
)

// streamPermit 是一张一次性的流许可。
type streamPermit struct {
	ID        string
	Purpose   string
	Params    journal.Params
	CreatedAt time.Time
	ExpiresAt time.Time
}

// streamStore 是许可表。全部操作都在锁内完成，且每次都顺手清掉过期的许可。
type streamStore struct {
	mu      sync.Mutex
	permits map[string]streamPermit
	max     int
	now     func() time.Time
	// 允许测试注入随机源，生产固定用 crypto/rand。
	newID func() (string, error)
}

func newStreamStore() *streamStore {
	return &streamStore{
		permits: make(map[string]streamPermit),
		max:     streamMaxPermits,
		now:     func() time.Time { return time.Now().UTC() },
		newID:   randomStreamID,
	}
}

// randomStreamID 生成 128 位随机 id（hex，32 字符）。
func randomStreamID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("agent: 生成流许可 id 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// create 登记一张许可。
func (s *streamStore) create(purpose string, params journal.Params) (streamPermit, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.permits) >= s.max {
		return streamPermit{}, ErrStreamPermitLimit
	}
	id, err := s.newID()
	if err != nil {
		return streamPermit{}, err
	}
	permit := streamPermit{
		ID:        id,
		Purpose:   purpose,
		Params:    params,
		CreatedAt: now,
		ExpiresAt: now.Add(streamPermitTTL),
	}
	s.permits[id] = permit
	return permit, nil
}

// consume 取走一张许可：它是**一次性**的，取走之后同一个 id 再用就是不存在。
func (s *streamStore) consume(id string) (streamPermit, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	permit, ok := s.permits[id]
	if !ok {
		return streamPermit{}, ErrStreamPermitNotFound
	}
	delete(s.permits, id)
	return permit, nil
}

// pruneLocked 清掉过期许可。
func (s *streamStore) pruneLocked(now time.Time) {
	for id, permit := range s.permits {
		if !now.Before(permit.ExpiresAt) {
			delete(s.permits, id)
		}
	}
}

// count 返回当前存活许可数量（仅测试使用）。
func (s *streamStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.permits)
}

// streamRequest 是 POST /internal/v1/streams 的请求体。
//
// 只有用途与参数：没有路径、没有 argv、没有凭据。用途决定参数怎么解释，
// 所以未登记的用途连参数都不会被解码。
type streamRequest struct {
	Purpose string          `json:"purpose"`
	Params  json.RawMessage `json:"params"`
}

// streamResponse 是创建许可的响应。不返回任何“能用它读什么”的细节：
// 参数已经绑在许可里，GET 时不需要（也不允许）再传一遍。
type streamResponse struct {
	StreamID  string    `json:"stream_id"`
	Purpose   string    `json:"purpose"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleStreams 处理 POST /internal/v1/streams。
func (s *Server) handleStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 POST")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return
	}
	body, ok := readLimitedBody(w, r, maxQueryRequestBytes)
	if !ok {
		return
	}
	var req streamRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	params, err := s.decodeStreamParams(req.Purpose, req.Params)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrStreamPurposeUnknown) {
			// 未登记的用途：这是“代理没有这个能力”，不是参数写错。
			status = http.StatusNotImplemented
		}
		httpx.WriteError(w, r, status, httpx.CodeValidationError, err.Error())
		return
	}

	permit, err := s.streams.create(req.Purpose, params)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrStreamPermitLimit) {
			status = http.StatusTooManyRequests
		}
		s.opts.Logger.Warn("创建流许可失败", "purpose", req.Purpose, "error", err,
			"request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, status, httpx.CodeResourceBusy, "创建流许可失败")
		return
	}
	s.opts.Logger.Info("创建流许可",
		"purpose", permit.Purpose,
		"stream_id", permit.ID,
		"expires_at", permit.ExpiresAt,
		"request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusCreated, streamResponse{
		StreamID:  permit.ID,
		Purpose:   permit.Purpose,
		ExpiresAt: permit.ExpiresAt,
	})
}

// decodeStreamParams 按用途解码参数。用途不在白名单内时**不解码参数**。
func (s *Server) decodeStreamParams(purpose string, raw json.RawMessage) (journal.Params, error) {
	switch purpose {
	case StreamPurposeLogs:
		if s.opts.Logs == nil {
			return journal.Params{}, fmt.Errorf("%w: %q", ErrStreamPurposeUnknown, purpose)
		}
		var params journal.Params
		if err := DecodeParams(raw, &params); err != nil {
			return journal.Params{}, err
		}
		if err := params.Validate(); err != nil {
			return journal.Params{}, err
		}
		return params, nil
	default:
		return journal.Params{}, fmt.Errorf("%w: %q", ErrStreamPurposeUnknown, purpose)
	}
}

// handleStreamByID 处理 GET /internal/v1/streams/:id。
//
// 再次核验对端凭据这一步由 gate 在最外层完成（与所有代理路由相同）：能走到这里的
// 请求必然来自 upanel-web 的 UID。许可本身是单次凭据，取走即作废。
func (s *Server) handleStreamByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/internal/v1/streams/")
	if id == "" || strings.Contains(id, "/") {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "流许可不存在")
		return
	}
	permit, err := s.streams.consume(id)
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "流许可不存在或已失效")
		return
	}

	switch permit.Purpose {
	case StreamPurposeLogs:
		s.serveLogStream(w, r, permit)
	default:
		// 用途在白名单里才会被创建，走到这里说明登记与分发不一致，属于编程错误。
		s.opts.Logger.Error("流许可的用途没有对应的分发实现", "purpose", permit.Purpose, "stream_id", permit.ID)
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "流用途没有实现")
	}
}

// streamEvent 是 NDJSON 流里的一行。
//
// 三种 type：entry（一条日志）、end（流正常结束，带原因）、error（流出错）。
// 用 NDJSON 而不是 JSON 数组：数组必须等到结束才能闭合，而流的价值就是“边到边看”。
type streamEvent struct {
	Type    string         `json:"type"`
	Entry   *journal.Entry `json:"entry,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Code    string         `json:"code,omitempty"`
	Message string         `json:"message,omitempty"`
}

// serveLogStream 把 journal 跟随流写成 NDJSON。
func (s *Server) serveLogStream(w http.ResponseWriter, r *http.Request, permit streamPermit) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 立刻把响应头推出去：流在安静系统上可能几十秒没有第一条日志，
	// 不 flush 的话调用方的 “等待响应头” 计时器会先把连接掐掉
	// （实测：Web 侧报 “timeout awaiting response headers”，空闲上限根本没机会生效）。
	if rc != nil {
		_ = rc.Flush()
	}

	write := func(ev streamEvent) bool {
		body, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		body = append(body, '\n')
		if rc != nil {
			// 每次写都往后推截止时间：代理的 WriteTimeout 是绝对的，
			// 不推的话一条长流会在 15 秒时被整体砍断。
			_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		}
		if _, err := w.Write(body); err != nil {
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}

	started := time.Now()
	end, err := s.opts.Logs.Stream(r.Context(), permit.Params, journal.StreamOptions{}, func(entry journal.Entry) error {
		if !write(streamEvent{Type: "entry", Entry: &entry}) {
			return errStreamWriteFailed
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStreamWriteFailed) && r.Context().Err() == nil {
			s.opts.Logger.Warn("日志流异常结束",
				"stream_id", permit.ID, "error", err, "request_id", httpx.RequestID(r))
			write(streamEvent{Type: "error", Code: "STREAM_FAILED", Message: err.Error()})
			return
		}
		end = journal.StreamEndClientClosed
	}
	// 结束原因一定要写出去：客户端据此决定“重连”还是“停”，
	// 而不是靠“连接断了”去猜（猜错的表现就是无限重连或该重连时不重连）。
	write(streamEvent{Type: "end", Reason: string(end)})
	s.opts.Logger.Info("日志流结束",
		"stream_id", permit.ID,
		"reason", string(end),
		"duration_ms", time.Since(started).Milliseconds(),
		"request_id", httpx.RequestID(r))
}
