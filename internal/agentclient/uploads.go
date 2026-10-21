package agentclient

// 暂存上传的客户端（P06.2）。
//
// 为什么上传不走 streams：流许可的语义是“代理**读**一次给你”（logs、download），
// 而上传是反方向的、一次性的写入。硬把它塞进许可机会让“用途”这个字段同时表示
// 读与写，而用途决定的是权限——两种方向共用一个开关是最容易出错的地方。
// 所以代理另开了 /internal/v1/uploads 三个接口，这里的三个方法就是它们的全部客户端。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// UploadsPath 是暂存上传的内部路径前缀。
const UploadsPath = "/internal/v1/uploads"

// uploadPath 返回单个上传的路径。
//
// upload_id 的形状由 files.ValidUploadID 先行校验（`up_` + 32 位小写十六进制），
// 这里再做一次 PathEscape：拼进 URL 的东西哪怕只有一次来自别处，也不该由它决定路径形状。
func uploadPath(uploadID string) string {
	return UploadsPath + "/" + url.PathEscape(uploadID)
}

// CreateStagedUpload 创建一个暂存上传（声明根、目标路径与字节数）。
func (c *Client) CreateStagedUpload(ctx context.Context, params any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, UploadsPath, params)
}

// GetStagedUpload 查询一个暂存上传的进度与状态。
func (c *Client) GetStagedUpload(ctx context.Context, uploadID string) ([]byte, error) {
	if uploadID == "" {
		return nil, errors.New("agentclient: upload_id 不能为空")
	}
	return c.do(ctx, http.MethodGet, uploadPath(uploadID), nil)
}

// DeleteStagedUpload 放弃一个暂存上传并释放它占用的预留。
func (c *Client) DeleteStagedUpload(ctx context.Context, uploadID string) ([]byte, error) {
	if uploadID == "" {
		return nil, errors.New("agentclient: upload_id 不能为空")
	}
	return c.do(ctx, http.MethodDelete, uploadPath(uploadID), nil)
}

// PutStagedUploadContent 把内容流式送进暂存区，返回代理给出的事后状态。
//
// 这里刻意用**没有整体超时**的客户端：一次 2 GiB 的上传在慢盘上可能远超十秒，
// 而“十秒到了就断开”会把一个正在正常传输的上传变成一条 500。
// 时限由调用方通过 ctx 决定（Web 侧用请求的 ctx，浏览器断开就跟着放弃）。
//
// size 必须与实际字节数一致：代理按它做过预留，也会在收完后核对，
// 所以我们把 ContentLength 也照实报出去——一个错的 Content-Length 不该被悄悄容忍。
func (c *Client) PutStagedUploadContent(ctx context.Context, uploadID string, body io.Reader, size int64) ([]byte, error) {
	if uploadID == "" {
		return nil, errors.New("agentclient: upload_id 不能为空")
	}
	if body == nil {
		return nil, errors.New("agentclient: 上传内容不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://agent"+uploadPath(uploadID)+"/content", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, &UnavailableError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeEnvelope(resp)
}
