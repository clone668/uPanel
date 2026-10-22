package containers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Block 是一条阻塞/告警（与 agent 的 Notice 同形，包内不依赖 agent）。
type Block struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 阻塞码闭集。它们是给人看的说明，也是前端判断“为什么点不动”的依据。
const (
	BlockNotFound     = "CONTAINER_NOT_FOUND"
	BlockUnmanaged    = "CONTAINER_NOT_MANAGED"
	BlockState        = "CONTAINER_STATE_CONFLICT"
	BlockUnavailable  = "DOCKER_UNAVAILABLE"
	WarnComposeMember = "COMPOSE_MEMBER"
)

// ControlAssessment 是一次预检判决。
type ControlAssessment struct {
	Container Container
	Allowed   bool
	Blockers  []Block
	Warnings  []Block
	// Expectation 是操作成功后应有的状态（start→running、stop→stopped、restart→running）。
	Expectation string
}

// AssessControl 判断能不能对这个容器做这个操作。
//
// 判决顺序是有意的：先看有没有 Docker，再看容器在不在，再看是不是危险容器，
// 最后才看状态。反过来会出现“状态不适用”挡在“这是 privileged 容器”前面，
// 让人以为换个操作就能接管它。
func AssessControl(ctx context.Context, client *Client, params ControlParams) (ControlAssessment, error) {
	if available, notice := client.Availability(); !available {
		// Docker 不可用不是“预检算错了”，而是一个能说清的拒绝理由：
		// 预检层把 Plan 返回的 error 当成实现失败（500），所以这里返回
		// Allowed=false + 阻塞码，界面据此显示“本机没有 Docker”，
		// 而不是一个查不出原因的 500。
		return ControlAssessment{Blockers: []Block{{Code: BlockUnavailable, Message: notice}}}, nil
	}
	container, err := client.Get(ctx, params.ContainerID)
	if err != nil {
		var notFound *NotFoundError
		if ok := asNotFound(err, &notFound); ok {
			return ControlAssessment{
				Blockers: []Block{{Code: BlockNotFound, Message: fmt.Sprintf(
					"容器 %s 不在本机：可能已经被删除或重建", shortID(params.ContainerID))}},
			}, nil
		}
		var unavailable *UnavailableError
		if errors.As(err, &unavailable) {
			// 套接字还在、守护进程已经死了：这同样是“说清理由的拒绝”，
			// 而不是预检算错了（预检层会把 error 变成 500）。
			return ControlAssessment{Blockers: []Block{{Code: BlockUnavailable, Message: unavailable.Notice}}}, nil
		}
		return ControlAssessment{}, err
	}

	assessment := ControlAssessment{Container: container, Expectation: expectationOf(params.Operation)}

	if container.Risk.Level == RiskHigh {
		blocks := make([]Block, 0, len(container.Risk.Reasons))
		for _, reason := range container.Risk.Reasons {
			blocks = append(blocks, Block{Code: BlockUnmanaged, Message: reason.Message})
		}
		blocks = append([]Block{{Code: BlockUnmanaged, Message: fmt.Sprintf(
			"面板不接管这个容器（%s）：它拥有宿主级的权限，写操作可能直接影响整台机器。请用 docker 命令自行处理",
			container.Name)}}, blocks...)
		assessment.Blockers = blocks
		return assessment, nil
	}

	if !OperationAllowed(container.State, params.Operation) {
		assessment.Blockers = []Block{{Code: BlockState, Message: fmt.Sprintf(
			"容器当前状态是 %s，%s 对它不适用", stateText(container), params.Operation)}}
		return assessment, nil
	}

	if container.Compose != nil && container.Compose.Project != "" {
		assessment.Warnings = []Block{{Code: WarnComposeMember, Message: fmt.Sprintf(
			"这个容器属于 compose 项目 %q：单独 %s 它，项目状态与标签会和你看到的不一致",
			container.Compose.Project, params.Operation)}}
	}
	assessment.Allowed = true
	return assessment, nil
}

// asNotFound 是 errors.As 的小包装：调用点只关心“是不是 404 那一种”。
func asNotFound(err error, target **NotFoundError) bool { return errors.As(err, target) }

// stateText 是给人看的状态描述。
func stateText(c Container) string {
	if c.StateText != "" {
		return c.StateText
	}
	switch c.State {
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	}
	return "unknown（状态读不出来）"
}

// expectationOf 返回操作成功后应有的状态。
func expectationOf(op string) string {
	if op == OpStop {
		return StateStopped
	}
	return StateRunning
}

// 执行的固定时限。
const (
	// controlTimeout 是单个 docker start/stop/restart 的时限。stop 默认有 10 秒
	// 优雅期，start 要拉起进程，给 90 秒足够；超时由 syscmd 杀掉进程组。
	controlTimeout = 90 * time.Second
	// stopGraceSeconds 是 stop/restart 的优雅期（秒）。显式给出而不是依赖默认值：
	// docker 的默认值可以在 daemon.json 里改，而任务结果里写的 10 秒必须是真的。
	stopGraceSeconds = "10"
	// verifyTimeout 是操作后等状态稳定下来的上限。
	verifyTimeout = 15 * time.Second
	// verifyInterval 是轮询间隔。
	verifyInterval = 500 * time.Millisecond
)

// ControlResult 是执行结果。
type ControlResult struct {
	ContainerID string
	Operation   string
	ExitCode    int
	Stdout      string
	Stderr      string
	Duration    time.Duration
	Before      Container
	After       Container
	Expectation string
	Verified    bool
	// VerifyNote 说明验证为什么没通过（没通过时才填）。
	VerifyNote string
}

// ExecuteControl 执行一次启停，并**读回状态**判断成功。
//
// 退出码 0 不算成功：docker start 对一个正在退出的容器也会返回 0。只有读回的状态
// 与期望一致才算 verified；读不回来就是 unknown，而不是“大概是好了”。
func ExecuteControl(ctx context.Context, client *Client, params ControlParams,
	before Container, progress func(step string, fields map[string]any)) (ControlResult, error) {

	result := ControlResult{
		ContainerID: params.ContainerID,
		Operation:   params.Operation,
		Expectation: expectationOf(params.Operation),
		Before:      before,
	}
	if progress != nil {
		progress("docker."+params.Operation, map[string]any{
			"container_id": shortID(params.ContainerID),
			"from_state":   before.State,
		})
	}

	args := []string{params.Operation}
	if params.Operation == OpStop || params.Operation == OpRestart {
		args = append(args, "--time", stopGraceSeconds)
	}
	args = append(args, params.ContainerID)

	res, err := client.run(ctx, controlTimeout, args...)
	if err != nil {
		return result, err
	}
	result.ExitCode = res.ExitCode
	result.Stdout = strings.TrimSpace(res.Stdout)
	result.Stderr = strings.TrimSpace(res.Stderr)
	result.Duration = res.Duration
	if res.ExitCode != 0 {
		result.VerifyNote = fmt.Sprintf("docker %s 退出码 %d：%s", params.Operation, res.ExitCode,
			firstLine(firstNonEmptyStr(result.Stderr, result.Stdout)))
		return result, nil
	}

	// 读回状态：这是唯一算数的证据。
	deadline := time.Now().Add(verifyTimeout)
	var last Container
	for {
		after, err := client.Get(ctx, params.ContainerID)
		if err != nil {
			var notFound *NotFoundError
			if asNotFound(err, &notFound) {
				result.VerifyNote = "容器在操作之后消失了（可能被删除或重建）"
				return result, nil
			}
			return result, err
		}
		last = after
		if after.State == result.Expectation {
			result.After = after
			result.Verified = true
			return result, nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			result.After = last
			result.VerifyNote = "等待状态变化时被取消"
			return result, nil
		case <-time.After(verifyInterval):
		}
	}
	result.After = last
	result.VerifyNote = fmt.Sprintf("%s 秒内状态没有变成 %s，读回的是 %s",
		verifyTimeout.Round(time.Second), result.Expectation, stateText(last))
	return result, nil
}

// firstNonEmptyStr 返回第一个非空字符串。
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// AssessControl 是包级 AssessControl 的方法形式，方便接口化（代理层的依赖是接口）。
func (c *Client) AssessControl(ctx context.Context, params ControlParams) (ControlAssessment, error) {
	return AssessControl(ctx, c, params)
}

// ExecuteControl 是包级 ExecuteControl 的方法形式。
func (c *Client) ExecuteControl(ctx context.Context, params ControlParams, before Container,
	progress func(step string, fields map[string]any)) (ControlResult, error) {
	return ExecuteControl(ctx, c, params, before, progress)
}

// StateVersion 返回容器状态的快照版本（预检绑定它，执行前必须与读回的一致）。
//
// 用的是“身份 + 状态 + 启动时间 + 重启次数”：窗口内重启一次，StartedAt 与
// RestartCount 都会变，旧预检因此自然失效，不需要额外的时间戳比较。
func StateVersion(c Container) string {
	sum := sha256.Sum256([]byte(c.ID + "|" + c.State + "|" + c.StartedAt + "|" +
		strconv.Itoa(c.RestartCount)))
	return hex.EncodeToString(sum[:16])
}
