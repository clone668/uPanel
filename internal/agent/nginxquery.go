package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"upanel/internal/nginx"
	"upanel/internal/syscmd"
)

// NginxStateQueryName 是 Nginx 现状查询（P10.1）。
//
// 它回答“这台机器上的 Nginx 装没装、在不在跑、有哪些配置文件、里面声明了哪些
// server_name”。判断“这个域名能不能给我用”不在这里做——那属于 Web 侧的
// internal/proxies。代理只给事实，面板给判断，两边不互相猜。
const NginxStateQueryName = "nginx.state"

// maxNginxVersionTimeout 是 `nginx -v` 的时限。它只是读一个版本字符串，
// 给 5 秒已经宽到能覆盖一台忙机器。
//
// 单位必须写在常量里：syscmd.Limits.Timeout 是 time.Duration，无单位的 5 会被
// 当成 5 纳秒，命令还没启动就已经超时——那样读到的是“读版本失败”，
// 而不是“这台机器没有 nginx”。
const maxNginxVersionTimeout = 5 * time.Second

// nginxStateParams 是 nginx.state 的参数。
//
// fresh=true 时**不走缓存**：写操作前的冲突判断必须看见用户刚刚手工加进去的配置，
// 而缓存窗口里的“没有冲突”正是最危险的那句谎话。页面刷新用 fresh=false，
// 5 秒内拿到同一份结果，同一次请求里前后两个域名依据的是同一时刻的事实。
type nginxStateParams struct {
	Fresh bool `json:"fresh"`
}

// newNginxStateQuery 登记 nginx.state。
func newNginxStateQuery(collector *nginx.Collector) Query {
	return Query{
		Name: NginxStateQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params nginxStateParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(nginxStateParams)
			if !ok {
				return nil, fmt.Errorf("%w: nginx.state 参数类型不匹配", ErrParamsInvalid)
			}
			if p.Fresh {
				return collector.Collect(ctx)
			}
			return collector.Snapshot(ctx)
		},
	}
}

// NginxVersionProbe 用 `nginx -v` 读版本号。
//
// `nginx -v` 把版本写在 stderr 上（这是 nginx 自己的行为，不是我们读错了流），
// 格式固定为 `nginx version: nginx/1.24.0`。退出码非零就说明这个二进制不可用，
// 返回错误而不是编一个版本号。
func NginxVersionProbe(path string) nginx.VersionProbe {
	return func(ctx context.Context) (string, error) {
		res, err := syscmd.Run(ctx, syscmd.Spec{Program: path, Args: []string{"-v"}}, syscmd.Limits{
			Timeout:   maxNginxVersionTimeout,
			MaxStdout: 4 << 10,
			MaxStderr: 4 << 10,
		})
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("nginx -v 退出码 %d：%s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		line := res.Stderr
		if strings.TrimSpace(line) == "" {
			line = res.Stdout
		}
		return parseNginxVersion(line)
	}
}

// parseNginxVersion 从 `nginx version: nginx/1.24.0` 里取出版本号。
func parseNginxVersion(line string) (string, error) {
	const marker = "nginx/"
	i := strings.Index(line, marker)
	if i < 0 {
		return "", fmt.Errorf("认不出 nginx -v 的输出：%q", strings.TrimSpace(line))
	}
	rest := line[i+len(marker):]
	rest = strings.TrimSpace(rest)
	end := len(rest)
	for j := 0; j < len(rest); j++ {
		c := rest[j]
		if (c < '0' || c > '9') && c != '.' {
			end = j
			break
		}
	}
	version := rest[:end]
	if version == "" {
		return "", fmt.Errorf("nginx -v 的输出里没有版本号：%q", strings.TrimSpace(line))
	}
	return version, nil
}
