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
		// 查询实现自己给出判决时（P06.1 起：文件模块的越界/不存在/非文本/版本冲突），
		// 按它说的状态码与码回。压成 500 会让 Web 把它翻译成 AGENT_UNAVAILABLE，
		// 于是“路径越界”显示成“代理不可用”——那是在骗调用方去查错地方。
		if carrier, ok := statusCarrierOf(err); ok {
			status, code := carrier.HTTPStatus()
			s.opts.Logger.Warn("查询被拒绝", "query", req.Query, "status", status,
				"code", code, "error", err, "request_id", httpx.RequestID(r))
			httpx.WriteError(w, r, status, code, err.Error())
			return
		}
		// 参数类错误（时间窗口、游标）是调用方的问题，必须是 400：
		// 否则 Web 侧只能把它当成“代理不可用”，把用户的参数错误报成 503。
		// 查询实现用 ErrParamsInvalid 表达这一点（见 logsquery.go）。
		if errors.Is(err, ErrParamsInvalid) {
			s.opts.Logger.Warn("查询参数不合法", "query", req.Query, "error", err,
				"request_id", httpx.RequestID(r))
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
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

// decodeStrictObjectLimit 是带自定义上限的 decodeStrictObject。
//
// 预检的信封里装着**任意动作**的参数，所以它的上限必须是“最大的逐动作上限”
// 而不是默认的 64 KiB：file.write_text 的参数可以带 1 MiB 内容（CONTRACTS.md
// 第 567 行：传输层因此放宽到 2 MiB）。信封放宽不会放宽动作：解码完信封之后
// 仍然按 action.ParamsLimitBytes 逐个核对，那才是唯一的判定处。
func decodeStrictObjectLimit(body []byte, dst any, limit int) error {
	if len(body) == 0 {
		return errors.New("请求体不能为空")
	}
	return DecodeParamsLimit(body, dst, limit)
}

func jsonContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}
