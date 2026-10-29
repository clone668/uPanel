package deploy

// 面板侧的健康探测（P15.3；PLAN.md 第 12 节“检查健康”）。
//
// 为什么不在 Compose 里写 healthcheck：容器内的 healthcheck 需要一条能在镜像里跑起来
// 的命令，而面板不知道镜像里有什么。`CMD-SHELL wget -q -O- …` 在 slim 镜像上永远
// unhealthy，还会往容器里塞一条 shell 命令（又多一条注入面）。所以探测由**面板自己**
// 从宿主侧打一次 HTTP 或 TCP，探的就是它对外公开的那个地址与端口。
//
// 探测的三条纪律：
//
//	探的是公开入口    宿主绑定地址 + 宿主端口，不是容器端口。容器端口通不通是
//	                 Docker 的事，管理员关心的是"我从外面能不能连上"。
//	不知道就当不知道  连不上、超时、状态码不合预期，都是**事实**，照样写进结论；
//	                 只有"连探测目标都没拼出来"才返回错误。
//	有预算           探测最多花 budget（默认 HealthProbeBudget）：一次更新不该因为
//	                 一个永远不健康的容器挂到操作超时，最后连"哪一步卡住了"都说不清。
//	                 启动宽限期内不算失败（容器刚起时还没监听是正常的）。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// HealthProbeBudget 是一次健康探测的总时限。
	//
	// 它同时是"这次更新会不会拖住操作"的上限：操作本身有 10 分钟的总预算
	// （agent.OperationTimeout），探测占掉 2 分钟，剩下的留给停单元、备份、拉镜像
	// 与重启。定义里的 interval × retries 超过这个数时按预算截断，并在结论里说明
	// 截断了——不要假装按定义探满了。
	HealthProbeBudget = 120 * time.Second

	// HealthProbeMinAttemptTimeout 是单次探测的最小超时。
	HealthProbeMinAttemptTimeout = time.Second

	// healthProbeBodyLimit 是 HTTP 探测最多读多少响应体（读完就关，只为拿到状态码）。
	healthProbeBodyLimit = 4 << 10
)

// HealthTarget 是一次探测要打的地址（从定义里算出来，不含秘密）。
type HealthTarget struct {
	// Kind 是 http / tcp。
	Kind string
	// Address 是宿主侧的字面 IP（通配地址在拼的时候换成回环）。
	Address string
	// Port 是**宿主端口**（发布的那个），不是容器端口。
	Port int
	// Path 是 http 探测的路径。
	Path string
	// Timeout 是单次尝试的超时。
	Timeout time.Duration
	// Interval 是两次尝试之间的间隔。
	Interval time.Duration
	// Retries 是失败后最多再试几次。
	Retries int
	// StartPeriod 是启动宽限期：这段时间内的失败不算失败。
	StartPeriod time.Duration
}

// URL 拼出 http 探测的地址（tcp 探测没有 URL，返回空串）。
func (t HealthTarget) URL() string {
	if t.Kind != HealthCheckHTTP {
		return ""
	}
	path := t.Path
	if path == "" {
		path = "/"
	}
	return "http://" + net.JoinHostPort(t.Address, strconv.Itoa(t.Port)) + path
}

// HealthProbe 是一次探测的结论。
//
// 它**不是**布尔值：一次探测要说清"试了几次、每次都怎么了、最后那次看到什么"。
// 只说 unhealthy 的结论，人还得自己去猜是没起来、还是路径写错了。
type HealthProbe struct {
	// Checked 为 false 表示压根没探测（定义里没开健康检查）。
	Checked bool
	// Healthy 是最终判断。
	Healthy bool
	// Attempts 是真正发出去的探测次数。
	Attempts int
	// StatusCode 是最后一次拿到的状态码（没拿到为 nil）。
	StatusCode *int
	// Address / URL 是实际探的地址（通配地址在这里显示成回环）。
	Address string
	URL     string
	// LastError 是最后一次失败的原因（成功时为空）。
	LastError string
	// Truncated 表示预算不足以按定义探满（interval × retries），结论是按预算截断的。
	Truncated bool
	// StartPeriodRespected 表示失败发生在启动宽限期内，因此不算失败。
	StartPeriodRespected bool
	// DurationMS 是这次探测花掉的毫秒数。
	DurationMS int64
}

// HealthProber 是探测能力（生产实现是 DefaultHealthProber，测试用假件）。
//
// 用一个接口而不是直接调 net/http：更新流程里"探测失败 → 回切"是一条必须验的路径，
// 而真去等一个不健康的端口要几十秒。
type HealthProber interface {
	Probe(ctx context.Context, target HealthTarget, budget time.Duration) HealthProbe
}

// DefaultHealthProber 用真的网络去探。
type DefaultHealthProber struct {
	// Client 为空时用不跟随重定向、禁 keep-alive 的客户端：重定向会把一次探测
	// 变成对任意外部地址的请求，而部署的健康检查只该看它自己。
	Client *http.Client
	// Now / Sleep 可注入（测试用来把等待变成零成本）。
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Probe 按定义里的节奏探测，最多花 budget。
func (p DefaultHealthProber) Probe(ctx context.Context, target HealthTarget, budget time.Duration) HealthProbe {
	now := p.Now
	if now == nil {
		now = time.Now
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	out := HealthProbe{Checked: true, Address: target.Address, URL: target.URL()}
	started := now()
	defer func() { out.DurationMS = now().Sub(started).Milliseconds() }()

	if budget <= 0 {
		budget = HealthProbeBudget
	}
	deadline := started.Add(budget)

	attemptTimeout := target.Timeout
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultHealthTimeoutSeconds * time.Second
	}
	attempts := target.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastFailure time.Time
	for i := 0; i < attempts; i++ {
		if i > 0 {
			gap := target.Interval
			if gap <= 0 {
				gap = DefaultHealthIntervalSeconds * time.Second
			}
			if now().Add(gap).After(deadline) {
				// 预算不够再等一次了：如实记下"没探满"，不假装按定义探过。
				out.Truncated = true
				break
			}
			if err := sleep(ctx, gap); err != nil {
				out.LastError = "探测被中断：" + err.Error()
				break
			}
		}
		if now().After(deadline) {
			out.Truncated = true
			break
		}
		out.Attempts++
		code, err := p.once(ctx, target, attemptTimeout)
		if code != nil {
			out.StatusCode = code
		}
		if err == nil {
			out.Healthy = true
			out.LastError = ""
			return out
		}
		out.LastError = err.Error()
		lastFailure = now()
	}

	// 启动宽限期内的失败不算失败：容器刚起来还没监听，这是正常的。
	if target.StartPeriod > 0 && !lastFailure.IsZero() && lastFailure.Sub(started) <= target.StartPeriod {
		out.StartPeriodRespected = true
		out.Healthy = true
	}
	return out
}

// once 探测一次，返回状态码（tcp 没有状态码时返回 nil）与错误。
func (p DefaultHealthProber) once(ctx context.Context, target HealthTarget, timeout time.Duration) (*int, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch target.Kind {
	case HealthCheckTCP:
		var dialer net.Dialer
		conn, err := dialer.DialContext(attemptCtx, "tcp", net.JoinHostPort(target.Address, strconv.Itoa(target.Port)))
		if err != nil {
			return nil, fmt.Errorf("连不上 %s：%v", net.JoinHostPort(target.Address, strconv.Itoa(target.Port)), err)
		}
		_ = conn.Close()
		return nil, nil
	case HealthCheckHTTP:
		client := p.Client
		if client == nil {
			client = defaultProbeClient()
		}
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, target.URL(), nil)
		if err != nil {
			return nil, fmt.Errorf("拼不出探测请求：%v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求 %s 失败：%v", target.URL(), err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, healthProbeBodyLimit))
		code := resp.StatusCode
		if code < 200 || code >= 400 {
			return &code, fmt.Errorf("状态码 %d 不在 200—399 之间", code)
		}
		return &code, nil
	}
	return nil, fmt.Errorf("未知的探测类型 %q（只能是 http 或 tcp）", target.Kind)
}

// defaultProbeClient 是一次探测用的 HTTP 客户端。
//
// 不跟随重定向：一次健康检查变成对重定向目标的请求，等于让部署自己决定面板去打谁。
// 不复用连接：探测的间隔是秒到分钟级，复用一条可能已经被对端关掉的连接只会
// 让"第一次必失败"变成常态。
func defaultProbeClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

// sleepCtx 是可被取消的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// probeAddress 把定义里的绑定地址换成探测用的地址。
//
// 通配地址（0.0.0.0 / ::）意味着"所有网卡"，从宿主上探它要挑一个具体地址：
// 回环。这不是妥协——绑通配的部署必然也在回环上应答，否则它哪儿都不应答。
func probeAddress(bind string) (string, error) {
	addr := strings.TrimSpace(bind)
	if addr == "" {
		return "", fmt.Errorf("部署没有绑定地址，探不到它的入口")
	}
	if ip := net.ParseIP(addr); ip != nil && ip.IsUnspecified() {
		return "127.0.0.1", nil
	}
	return addr, nil
}

// healthTargetOf 从定义里算出一份探测目标。
//
// 端口恒为**宿主端口**：面板发布的是这一个端口，探容器端口等于探一个从外面
// 根本连不上的东西（§8.6 的 health_check.port 也只能是容器端口，但它描述的是
// 容器内监听的那一个，映射到宿主就是 host_port）。
func healthTargetOf(def Definition) (HealthTarget, error) {
	hc := def.HealthCheck
	if !hc.Enabled {
		return HealthTarget{}, fmt.Errorf("这份定义没有启用健康检查")
	}
	addr, err := probeAddress(def.HostBindAddress)
	if err != nil {
		return HealthTarget{}, err
	}
	timeout := time.Duration(hc.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultHealthTimeoutSeconds * time.Second
	}
	interval := time.Duration(hc.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = DefaultHealthIntervalSeconds * time.Second
	}
	retries := hc.Retries
	if retries <= 0 {
		retries = DefaultHealthRetries
	}
	return HealthTarget{
		Kind:        hc.Kind,
		Address:     addr,
		Port:        def.HostPort,
		Path:        hc.Path,
		Timeout:     timeout,
		Interval:    interval,
		Retries:     retries,
		StartPeriod: time.Duration(hc.StartPeriodSeconds) * time.Second,
	}, nil
}
