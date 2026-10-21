package agent

import (
	"context"
	"encoding/json"
	"errors"

	"upanel/internal/journal"
)

// paramsRejected 把“journal 判定的参数错误”标成 agent 的参数错误，**不改写消息**。
//
// 两个要求同时成立：queries.go 要用 errors.Is(err, ErrParamsInvalid) 判 400；
// 而消息里只能有一次来源说明（journal 已经写好了“journal: 查询参数不合法: 细节”）。
// 用 `fmt.Errorf("%w: %v", …)` 两个都做不到——它一定往消息里塞第二层前缀。
type paramsRejected struct{ err error }

func (e paramsRejected) Error() string { return e.err.Error() }

func (e paramsRejected) Unwrap() error { return e.err }

func (e paramsRejected) Is(target error) bool { return target == ErrParamsInvalid }

// LogsSource 是日志读取端在 agent 层需要的最小接口（生产实现是 *journal.Querier）。
//
// 与 servicesSource/processesSource 同理：agent 层只关心“参数解码 + 转交”，
// 真正的 journalctl 调用与限额在 internal/journal 里；测试可以给假实现验证
// 参数校验与错误映射，而不必在开发宿主上跑 journalctl（宿主根本没有）。
//
// 导出是因为 cmd/upanel-agent 要把它塞进 Options 与 ProductionDeps；
// 方法集仍然只有“查一页”与“开一条流”两件事。
type LogsSource interface {
	Query(ctx context.Context, params journal.Params) (journal.Page, error)
	Stream(ctx context.Context, params journal.Params, opts journal.StreamOptions, emit journal.EmitFunc) (journal.StreamEnd, error)
}

// LogsQueryName 是日志查询在登记表里的名字（名字本身由 internal/journal 定义，
// 因为 Web 侧也要用它去调用同一个查询）。
const LogsQueryName = journal.QueryName

// newLogsQuery 登记 logs.query：按 unit、时间、级别与游标读取 journal。
//
// 参数校验分两步：Decode 里做与时间无关的形状校验（数据源白名单、unit 形状、
// 级别闭集、游标字符集、条数上限），时间窗口由 internal/journal 在拿到“现在”之后
// 补齐并再次校验。这样写错的参数是 400，而不是执行到一半变成 500。
func newLogsQuery(src LogsSource) Query {
	return Query{
		Name: LogsQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params journal.Params
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Validate(); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, decoded any) (any, error) {
			params, ok := decoded.(journal.Params)
			if !ok {
				return nil, ErrParamsInvalid
			}
			page, err := src.Query(ctx, params)
			if err != nil {
				// 时间窗口与游标错误在 Query 里才出现（要拿“现在”和 journalctl 的
				// 真实反馈），这里统一翻成参数类错误，让上层回 400 而不是 500。
				//
				// 用 paramsRejected 而不是 `%w: %v`：后者会在 journal 已经写好的
				// “journal: 查询参数不合法: …” 前面再叠一层 “agent: 参数不合法: ”，
				// 一路上被 Web 再叠第三次（真机载荷 err-future.json 就是这么来的）。
				// 同一件事说三遍不只是难看：它让人以为参数错发生在代理层，于是去查代理。
				if errors.Is(err, journal.ErrInvalidParams) || errors.Is(err, journal.ErrCursorRejected) {
					return nil, paramsRejected{err: err}
				}
				return nil, err
			}
			return page, nil
		},
	}
}
