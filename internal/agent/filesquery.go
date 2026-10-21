package agent

// P06.1：文件模块在代理侧的登记（CONTRACTS.md 第 11 节）。
//
// 三条纪律：
//
//  1. **根只能来自配置**。参数里只有 root_id 与根内相对路径；没有绝对路径字段，
//     也没有“临时开放一个目录”的入口。未登记的 root_id 一律 404。
//
//  2. **判决不在这里猜**。越界、不存在、非文本、版本冲突都由 internal/files 判定，
//     并用 files.HTTPStatusOf 给出状态码与错误码；本层只负责参数解码与转交。
//     这样同一个拒绝在 files.list、files.text 与 files.download 流里
//     是同一个码，前端与验收脚本都只断言一处。
//
//  3. **下载走流**。文件内容不进查询接口的 JSON：查询接口的响应会被整段读进内存，
//     而下载的对象恰恰是“可能很大”。下载因此登记成 files.download 流用途，
//     沿用 P05.3 的一次性许可通道。

import (
	"context"
	"encoding/json"
	"errors"

	"upanel/internal/files"
	"upanel/internal/httpx"
)

// FilesSource 是文件读取端在代理层需要的最小接口（生产实现是 *files.Manager）。
//
// 导出是因为 cmd/upanel-agent 要把它塞进 Options 与 ProductionDeps；
// 方法集仍然只有“列根、列目录、读文本、开下载”四件事。
type FilesSource interface {
	Roots(ctx context.Context) files.RootsResult
	List(ctx context.Context, params files.ListParams) (files.ListPage, error)
	Text(ctx context.Context, params files.TextParams) (files.TextFile, error)
	OpenDownload(ctx context.Context, params files.DownloadParams) (*files.Download, error)
}

// 查询名与用途名由 internal/files 定义：Web 侧要用同一批字符串去调用查询。
const (
	// FilesRootsQueryName 是列根的查询名。
	FilesRootsQueryName = files.QueryRoots
	// FilesListQueryName 是列目录的查询名。
	FilesListQueryName = files.QueryList
	// FilesTextQueryName 是读文本的查询名。
	FilesTextQueryName = files.QueryText
)

// statusCarrier 让查询/流的实现自己给出 HTTP 判决。
//
// P06.1 的第一个使用者是文件模块：路径越界、文件不存在、不是文本、版本冲突
// 各有各的状态码与错误码。代理不按错误文本猜判决，只问“你这次拒绝对应哪个状态码”。
// 没实现这个接口的错误仍然走原来的映射（参数类 400、其余 500）。
type statusCarrier interface {
	HTTPStatus() (int, httpx.Code)
}

// statusCarrierOf 在错误链里找判决。
func statusCarrierOf(err error) (statusCarrier, bool) {
	var carrier statusCarrier
	if errors.As(err, &carrier) {
		return carrier, true
	}
	return nil, false
}

// newFilesRootsQuery 登记 files.roots：返回已登记的根及其可用性/容量。
func newFilesRootsQuery(src FilesSource) Query {
	return Query{
		Name: FilesRootsQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 无参数。空结构 + 严格解码让多余字段（包括 {"path":"/etc"}）被拒绝，
			// 而不是被静默忽略。
			var params files.RootsParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Roots(ctx), nil
		},
	}
}

// newFilesListQuery 登记 files.list。
func newFilesListQuery(src FilesSource) Query {
	return Query{
		Name: FilesListQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params files.ListParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Validate(); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, decoded any) (any, error) {
			params, ok := decoded.(files.ListParams)
			if !ok {
				return nil, ErrParamsInvalid
			}
			page, err := src.List(ctx, params)
			if err != nil {
				return nil, files.AsHTTPError(err)
			}
			return page, nil
		},
	}
}

// newFilesTextQuery 登记 files.text。
func newFilesTextQuery(src FilesSource) Query {
	return Query{
		Name: FilesTextQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params files.TextParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			if err := params.Validate(); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, decoded any) (any, error) {
			params, ok := decoded.(files.TextParams)
			if !ok {
				return nil, ErrParamsInvalid
			}
			text, err := src.Text(ctx, params)
			if err != nil {
				return nil, files.AsHTTPError(err)
			}
			return text, nil
		},
	}
}
