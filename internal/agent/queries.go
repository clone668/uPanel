package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"upanel/internal/httpx"
)

// 查询请求体的上限：参数上限加上信封字段的余量。
const maxQueryRequestBytes = MaxParamsBytes + 4<<10

// queryRequest 是 POST /internal/v1/queries 的请求体（CONTRACTS.md 第 15 节）。
//
// 只有 query 枚举与 params，**没有** command/argv/path 之类的自由字段：
// 未登记的 query 名一律拒绝，参数按该查询自己的 DTO 严格解码。
type queryRequest struct {
	Query  string          `json:"query"`
	Params json.RawMessage `json:"params"`
}

// handleQueries 执行一个已登记的结构化只读查询。
func (s *Server) handleQueries(w http.ResponseWriter, r *http.Request) {
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

	var req queryRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	if req.Query == "" {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, "query 不能为空")
		return
	}

	q, found := s.registry.Query(req.Query)
	if !found {
		// 明确拒绝未登记的名字：不存在“默认分支”或“按前缀猜一个实现”。
		s.opts.Logger.Warn("拒绝未登记的查询", "query", req.Query, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("未登记的查询 %q", req.Query))
		return
	}

	params, err := q.Decode(req.Params)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	result, err := q.Run(r.Context(), params)
	if err != nil {
		// P02.3 还没有生产查询，这里的映射在第一个真实查询落地（P04）时再细化；
		// 当前如实记录失败并返回内部错误，不把失败伪装成成功。
		s.opts.Logger.Error("查询执行失败", "query", req.Query, "error", err, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "查询执行失败")
		return
	}

	httpx.WriteData(w, r, http.StatusOK, result)
}

// readLimitedBody 读取有上限的请求体。超限返回 413，读取失败返回 400。
func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.CodeValidationError,
				fmt.Sprintf("请求体超过上限 %d 字节", limit))
			return nil, false
		}
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, "读取请求体失败")
		return nil, false
	}
	return body, true
}

// decodeStrictObject 按“拒绝未知字段”的方式解码一个 JSON 对象。
func decodeStrictObject(body []byte, dst any) error {
	if len(body) == 0 {
		return errors.New("请求体不能为空")
	}
	return DecodeParams(body, dst)
}

func jsonContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}
