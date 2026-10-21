package agentclient

// P05.3：受限流客户端。
//
// 与普通调用有两点不同，两点都必须显式处理：
//
//  1. **不能套用单次超时**。Client 的 http.Client.Timeout 覆盖到“读完响应体”，
//     套在一条实时日志流上等于给它设了 10 秒寿命。这里另起一个共享同一 Transport、
//     但没有整体超时的客户端；真正的时间上限由服务端（流的最长时长/空闲时长）决定。
//  2. **不能一次性读完**。响应是 NDJSON，一行一条，必须逐行交给调用方，
//     否则“实时”就变成了“等它结束”。
//
// 帧的大小另有上限：一条被伪造的超长帧不该把 Web 进程的内存吃掉。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	// streamPurposeLogs 是日志实时流的用途名（与代理侧一致）。
	streamPurposeLogs = "logs.stream"
	// maxStreamFrameBytes 是单帧上限：一条日志消息上限 8 KiB，JSON 外壳与转义留足余量。
	maxStreamFrameBytes = 256 << 10
)

// LogsStreamPurpose 返回日志流的用途名，供内部调用方使用。
func LogsStreamPurpose() string { return streamPurposeLogs }

// StreamPermit 是 POST /internal/v1/streams 的响应：一次性许可。
type StreamPermit struct {
	StreamID  string `json:"stream_id"`
	Purpose   string `json:"purpose"`
	ExpiresAt string `json:"expires_at"`
}

// CreateStream 申请一张流许可。params 由用途决定，客户端不做额外解释。
func (c *Client) CreateStream(ctx context.Context, purpose string, params any) (StreamPermit, error) {
	if purpose == "" {
		return StreamPermit{}, errors.New("agentclient: 流用途不能为空")
	}
	body := struct {
		Purpose string `json:"purpose"`
		Params  any    `json:"params"`
	}{Purpose: purpose, Params: params}
	if params == nil {
		body.Params = struct{}{}
	}
	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/streams", body)
	if err != nil {
		return StreamPermit{}, err
	}
	var permit StreamPermit
	if err := json.Unmarshal(raw, &permit); err != nil {
		return StreamPermit{}, &UnavailableError{Err: fmt.Errorf("流许可响应无法解析: %w", err)}
	}
	if permit.StreamID == "" || permit.Purpose == "" || permit.ExpiresAt == "" {
		return StreamPermit{}, &UnavailableError{Err: errors.New("流许可响应缺少必需字段")}
	}
	return permit, nil
}

// StreamFrame 是 NDJSON 流里的一行。
type StreamFrame struct {
	// Type 是 entry、end 或 error。
	Type string `json:"type"`
	// Entry 只在 Type=entry 时有内容，保留原始字节交给上层解析。
	Entry json.RawMessage `json:"entry"`
	// Reason 是 Type=end 时的结束原因。
	Reason string `json:"reason"`
	// Code/Message 是 Type=error 时的错误。
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StreamReader 逐帧读取一条受限流。
type StreamReader struct {
	body   io.ReadCloser
	reader *bufio.Reader
	// closed 保证 Close 只关一次（defer 与正常结束路径都会调它）。
	closed bool
}

// OpenStream 用许可打开流。许可是一次性的：这里失败就没有第二次机会，
// 调用方必须重新 CreateStream，而不是拿同一个 id 重试。
func (c *Client) OpenStream(ctx context.Context, streamID string) (*StreamReader, error) {
	if streamID == "" {
		return nil, errors.New("agentclient: 流 id 不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://agent/internal/v1/streams/"+streamID, nil)
	if err != nil {
		return nil, fmt.Errorf("agentclient: 构造流请求失败: %w", err)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, &UnavailableError{Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
			return nil, &UnavailableError{Err: fmt.Errorf("流请求返回 %d 且响应不是错误信封", resp.StatusCode)}
		}
		return nil, &Error{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	return &StreamReader{body: resp.Body, reader: bufio.NewReaderSize(resp.Body, 64<<10)}, nil
}

// Next 读下一帧。流正常结束时返回 io.EOF。
func (r *StreamReader) Next() (StreamFrame, error) {
	line, err := r.readLine()
	if err != nil {
		return StreamFrame{}, err
	}
	if len(bytes.TrimSpace(line)) == 0 {
		// 空行不是帧，继续读。
		return r.Next()
	}
	var frame StreamFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		return StreamFrame{}, &UnavailableError{Err: fmt.Errorf("流帧无法解析: %w", err)}
	}
	if frame.Type == "" {
		return StreamFrame{}, &UnavailableError{Err: errors.New("流帧缺少 type")}
	}
	return frame, nil
}

// readLine 读一行，并对单行长度设上限。
func (r *StreamReader) readLine() ([]byte, error) {
	line, err := r.reader.ReadBytes('\n')
	if int64(len(line)) > maxStreamFrameBytes {
		return nil, &UnavailableError{Err: fmt.Errorf("流帧超过上限 %d 字节", maxStreamFrameBytes)}
	}
	if err != nil && len(line) == 0 {
		return nil, err
	}
	// 读到 EOF 但还有残行：交给上层按帧解析（解析失败即视为协议错误）。
	return line, nil
}

// Close 关闭流。关闭后服务端会看到连接断开并释放子进程。
func (r *StreamReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.body.Close()
}
