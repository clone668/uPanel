package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 探测结果里的 HTTP 状态枚举。与 apps 包的 http_state 同词同义（这里不能 import
// apps：apps 是元数据层，发现层不该依赖它）。
const (
	// HTTPOK 表示最近一次探测命中了期望状态码。
	HTTPOK = "ok"
	// HTTPError 表示探测失败，或状态码不在期望集合内。
	HTTPError = "error"
	// HTTPUnknown 表示这一轮没有得出可用的结论（例如目标已经不在了）。
	HTTPUnknown = "unknown"
)

// 探测的固定限额。
const (
	// maxProbeBodyBytes 是探测时读取的响应体上限。健康检查只关心状态码，读回来的
	// 字节直接丢掉——但也要读一点并关闭，否则连接会挂着。
	maxProbeBodyBytes = 64 << 10
	// MaxProbeTimeoutSeconds 与 apps 包的 timeout_seconds 上限一致（CONTRACTS §8）。
	MaxProbeTimeoutSeconds = 10
	// MinProbeTimeoutSeconds 是下限。
	MinProbeTimeoutSeconds = 1
)

// ErrProbeParams 表示探测参数不合法（调用方的问题，接口层应回 400）。
var ErrProbeParams = errors.New("discovery: 探测参数不合法")

// ProbeParams 是一次探测请求。字段与应用元数据里的 health_check 一一对应。
type ProbeParams struct {
	// TargetID 必须是本机发现出来的目标（local_… / cntr_…）。
	TargetID string `json:"target_id"`
	// Scheme 只允许 http / https。
	Scheme string `json:"scheme"`
	// Port 是目标端口，1—65535。
	Port int `json:"port"`
	// Path 必须以 / 开头，不含 ://。
	Path string `json:"path"`
	// ExpectedStatusCodes 为空表示默认 200—399。
	ExpectedStatusCodes []int `json:"expected_status_codes"`
	// TimeoutSeconds 是单次超时，1—10。
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Normalize 补齐默认值（与 apps 包的默认值一致）。
func (p ProbeParams) Normalize() ProbeParams {
	if p.Scheme == "" {
		p.Scheme = "http"
	}
	if p.Path == "" {
		p.Path = "/"
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 3
	}
	if p.ExpectedStatusCodes == nil {
		p.ExpectedStatusCodes = []int{}
	}
	return p
}

// Validate 校验参数。
//
// 这里**不**校验 target_id 是否真的存在：那要看发现结果，是下一层的判断。
// 这里只挡形状——形状不对说明调用方在拼一个不该拼的值。
func (p ProbeParams) Validate() error {
	if !ValidTargetID(p.TargetID) {
		return fmt.Errorf("%w：target_id 必须是 local_/cntr_ 开头的本机目标", ErrProbeParams)
	}
	if p.Scheme != "http" && p.Scheme != "https" {
		return fmt.Errorf("%w：scheme 只能是 http 或 https", ErrProbeParams)
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("%w：port 必须在 1—65535 之间", ErrProbeParams)
	}
	if !strings.HasPrefix(p.Path, "/") || strings.Contains(p.Path, "://") {
		return fmt.Errorf("%w：path 必须以 / 开头且不能是完整 URL", ErrProbeParams)
	}
	if strings.HasPrefix(p.Path, "//") {
		// "//host/path" 是协议相对 URL 的开头。拼在 http://127.0.0.1:port 后面虽然
		// 仍然指向本机，但它是一条谁看了都以为是别的主机的路径，直接在入口挡掉。
		return fmt.Errorf("%w：path 不能以 // 开头", ErrProbeParams)
	}
	if p.TimeoutSeconds < MinProbeTimeoutSeconds || p.TimeoutSeconds > MaxProbeTimeoutSeconds {
		return fmt.Errorf("%w：timeout_seconds 必须在 %d—%d 之间",
			ErrProbeParams, MinProbeTimeoutSeconds, MaxProbeTimeoutSeconds)
	}
	if len(p.ExpectedStatusCodes) > 8 {
		return fmt.Errorf("%w：expected_status_codes 最多 8 个", ErrProbeParams)
	}
	for _, code := range p.ExpectedStatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("%w：状态码 %d 不在 100—599 之间", ErrProbeParams, code)
		}
	}
	return nil
}

// ProbeResult 是一次探测的结论。
type ProbeResult struct {
	TargetID  string    `json:"target_id"`
	CheckedAt time.Time `json:"checked_at"`
	Scheme    string    `json:"scheme"`
	Port      int       `json:"port"`
	Path      string    `json:"path"`
	// URL 是实际请求的地址。它永远是 http(s)://127.0.0.1:port/path 的形状：
	// 地址由代理自己拼，调用方给的只有端口与路径。含密码或查询串的 URL 不可能出现。
	URL     string `json:"url"`
	Address string `json:"address"`
	// TargetFound 表示这个 target_id 在本次发现结果里存在。false 时 HTTPState
	// 是 unknown：面板不认识这个目标，不能断言“服务挂了”。
	TargetFound bool `json:"target_found"`
	// StatusCode 是拿到的状态码；没有拿到（连接失败、超时）时为 null。
	StatusCode *int   `json:"status_code"`
	HTTPState  string `json:"http_state"`
	DurationMS int64  `json:"duration_ms"`
	// Error 是失败原因（中文 + 原始错误），成功时为空。
	Error string `json:"error,omitempty"`
}

// Valid 判断一条结论是否带着“实际探的是哪里”。
//
// 代理总会填这几个字段（它们是拼 URL 的原料）。缺了说明拿到的是半条结论，
// 调用方不该拿定义里的值把它补齐——那会把“不知道探的是哪儿”写成“探过了”。
func (r ProbeResult) Valid() error {
	if !ValidTargetID(r.TargetID) {
		return errors.New("缺少 target_id")
	}
	if r.Scheme != "http" && r.Scheme != "https" {
		return fmt.Errorf("scheme %q 不合法", r.Scheme)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("port %d 不合法", r.Port)
	}
	if !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("path %q 不合法", r.Path)
	}
	if r.HTTPState != HTTPOK && r.HTTPState != HTTPError && r.HTTPState != HTTPUnknown {
		return fmt.Errorf("http_state %q 不合法", r.HTTPState)
	}
	return nil
}

// Probe 对发现出来的目标做一次 HTTP 健康探测。
//
// 四件事按顺序做：校验参数 → 解析目标（发现结果里得真有它）→ 拼一个只指向 127.0.0.1
// 的 URL → 发一次不跟随重定向的 GET。返回的 error 只用于“参数不合法”，其余一切失败
// 都写进 ProbeResult（那是被观测对象的事实，不是调用错误）。
func (c *Collector) Probe(ctx context.Context, params ProbeParams) (ProbeResult, error) {
	params = params.Normalize()
	if err := params.Validate(); err != nil {
		return ProbeResult{}, err
	}

	// address 与 url 一律留到“真的要发请求”那一刻才填：没发出去的探测没有请求地址，
	// 预先填一个 127.0.0.1 会让一条 error/unknown 看起来像真的连过了。
	result := ProbeResult{
		TargetID:  params.TargetID,
		CheckedAt: c.deps.Now().UTC().Truncate(time.Second),
		Scheme:    params.Scheme,
		Port:      params.Port,
		Path:      params.Path,
		HTTPState: HTTPUnknown,
	}

	snap := c.Snapshot(ctx)
	target, found := snap.Targets[params.TargetID]
	if !found {
		state, text := missingTargetVerdict(snap, params.TargetID)
		result.HTTPState = state
		result.Error = text
		// 没有真的发出请求，所以 address/url 保持空串（见构造处注释）。
		return result, nil
	}
	result.TargetFound = true

	// 容器：应用的端口必须是容器真的发布出来的宿主机端口。容器端口与宿主机端口
	// 是两回事，配错了在这里就说清楚，而不是让一次 connection refused 去背锅。
	if target.Kind == KindDocker {
		if len(target.PublishedPorts) == 0 {
			result.HTTPState = HTTPError
			result.Error = fmt.Sprintf("容器 %s 没有把端口发布到宿主机回环上，无法探测", target.Name)
			return result, nil
		}
		if !containsInt(target.PublishedPorts, params.Port) {
			result.HTTPState = HTTPError
			result.Error = fmt.Sprintf("容器 %s 没有把宿主机端口 %d 发布出来（已发布：%s）",
				target.Name, params.Port, joinPorts(target.PublishedPorts))
			return result, nil
		}
	}

	result.Address = "127.0.0.1"
	if target.Address != "" {
		result.Address = target.Address
	}
	result.URL = fmt.Sprintf("%s://%s:%d%s", params.Scheme, result.Address, params.Port, params.Path)

	client := newProbeClient(time.Duration(params.TimeoutSeconds) * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, result.URL, nil)
	if err != nil {
		result.HTTPState = HTTPError
		result.Error = "构造请求失败：" + err.Error()
		return result, nil
	}
	// 请求体里不夹带任何面板凭据：健康检查是匿名请求，别把会话 cookie 送到被测
	// 服务上去。
	req.Header.Set("User-Agent", "upanel-healthcheck/1")
	req.Header.Set("Accept", "*/*")

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	result.DurationMS = elapsed.Milliseconds()
	if err != nil {
		result.HTTPState = HTTPError
		result.Error = probeErrorText(err)
		return result, nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBodyBytes))
		_ = resp.Body.Close()
	}()

	code := resp.StatusCode
	result.StatusCode = &code
	if statusExpected(code, params.ExpectedStatusCodes) {
		result.HTTPState = HTTPOK
		return result, nil
	}
	result.HTTPState = HTTPError
	result.Error = fmt.Sprintf("状态码 %d 不在期望集合里（%s）", code, describeExpected(params.ExpectedStatusCodes))
	return result, nil
}

// newProbeClient 构造一个只走回环、不跟随重定向、不用环境代理的客户端。
//
// 三件事都是刻意的：
//   - Proxy 置空：环境里的 HTTP_PROXY 会让“探测本机”变成一个发往外部的请求，
//     而且目标地址会先经过一个第三方。探测路径上不该有代理。
//   - 不跟随重定向：期望状态码是 3xx 时应当看到 3xx，而不是被带到别处再报 200。
//   - TLS 用系统信任根：CONTRACTS §8 明确不允许在未配置受信 CA 的前提下关闭校验。
//     自签证书的服务会探测失败，并报出 x509 的原因——那是事实，不是 bug。
func newProbeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// statusExpected 判断状态码是否命中期望集合；空集合表示 200—399。
func statusExpected(code int, expected []int) bool {
	if len(expected) == 0 {
		return code >= 200 && code <= 399
	}
	return containsInt(expected, code)
}

func describeExpected(expected []int) string {
	if len(expected) == 0 {
		return "默认 200—399"
	}
	parts := make([]string, 0, len(expected))
	for _, code := range expected {
		parts = append(parts, strconv.Itoa(code))
	}
	return strings.Join(parts, ", ")
}

// missingTargetVerdict 决定“目标不在快照里”该怎么记。
//
// 这有两种非常不同的情况，混成一句“不知道”就是撒谎：
//
//	发现本身没看成本机      → unknown（这一轮什么都没看到，不能当作“不在”）
//	看成了，目标就是不在      → error（服务停了、容器没了、单元不听了；对“这个入口还
//	                          在不在”而言，结论就是“不在”）
//
// 中间那档（partial）要看是哪个来源失败了：失败来源管不着的目标，缺席仍然是结论。
// 把它写成 unknown 的后果很具体——一个已经挂掉的站点会永远显示“暂不确定”。
func missingTargetVerdict(snap Snapshot, targetID string) (string, string) {
	wantDocker := strings.HasPrefix(targetID, TargetContainerPrefix)

	switch snap.Status {
	case StatusError:
		return HTTPUnknown, fmt.Sprintf("本轮发现没有成功（%s），无法确认目标 %s 是否还在",
			firstNonEmpty(snap.Error, "没有可用来源"), targetID)
	case StatusPartial:
		// partial：只看“可能装着这个目标”的来源。容器目标归 docker，其余归本机监听
		// 与单元。那些来源里只要有一个没采成，目标缺席就不能算结论。
		for _, src := range snap.Sources {
			if src.Status == StatusOK {
				continue
			}
			srcDocker := src.Source == SourceDocker
			if srcDocker != wantDocker {
				continue
			}
			return HTTPUnknown, fmt.Sprintf("来源 %s 本轮没有采集成功（%s），无法确认目标 %s 是否还在",
				src.Source, firstNonEmpty(src.Error, "原因未知"), targetID)
		}
	}

	return HTTPError, fmt.Sprintf("目标 %s 不在本机当前的监听/容器列表里（服务可能已停止或不再监听）",
		targetID)
}

// probeErrorText 把传输层错误说成人话，同时保留原始原因。
func probeErrorText(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时：" + err.Error()
	case errors.As(err, &netErr) && netErr.Timeout():
		return "请求超时：" + err.Error()
	}
	return "请求失败：" + err.Error()
}

func containsInt(values []int, want int) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ", ")
}
