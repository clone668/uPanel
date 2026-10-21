package files

// Web 侧的暂存上传端（P06.2，CONTRACTS.md 第 11 节）。
//
// Web 进程不碰文件系统：它把字节流转发给代理，由代理写进根内 .upanel/staging。
// 这一层因此只做三件事，和读路径一样：
//   - 参数先按形状校验（400 在本地给，不必为一个语法错误开一次 IPC）；
//   - 回包必须自洽（id/根/路径/大小/状态对不上就是代理不可用，不是“差不多”）；
//   - 把代理的拒绝原样翻成 Web 的错误码（一次“路径越界”不该变成“代理不可用”）。
//
// 为什么上传能力单独一个接口：Agent 的读方法（Query/CreateStream/OpenStream）
// 是 P06.1 的读取端在用，上传是反方向的一次性写入。把两者塞进同一个接口，
// 会让每个只想读文件的测试假实现都必须实现四个写方法；而分开之后，
// “这个实现能不能上传”这件事是可判定的，不能时给 501 而不是假装成功。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"upanel/internal/agentclient"
)

// UploadAgent 是暂存上传需要的代理能力。生产实现仍是 *agentclient.Client——
// 它同时满足 Agent 与 UploadAgent，两个接口在这里只是把能力分开声明。
type UploadAgent interface {
	CreateStagedUpload(ctx context.Context, params any) ([]byte, error)
	GetStagedUpload(ctx context.Context, uploadID string) ([]byte, error)
	DeleteStagedUpload(ctx context.Context, uploadID string) ([]byte, error)
	PutStagedUploadContent(ctx context.Context, uploadID string, body io.Reader, size int64) ([]byte, error)
}

// uploadAgent 取回上传能力。没有时返回 ErrUnsupported：接口层会翻成 501，
// 页面显示“当前部署不支持上传”，而不是一个空白的成功。
func (f *Fetcher) uploadAgent() (UploadAgent, error) {
	agent, ok := f.agent.(UploadAgent)
	if !ok {
		return nil, fmt.Errorf("%w: 代理客户端没有提供暂存上传能力", ErrUnsupported)
	}
	return agent, nil
}

// 生产实现必须同时满足两个接口：接线在 cmd/upanel-web/main.go，检查放在编译期。
var _ UploadAgent = (*agentclient.Client)(nil)

// CreateUpload 创建一次暂存上传。
func (f *Fetcher) CreateUpload(ctx context.Context, params CreateUploadParams) (UploadInfo, error) {
	if err := params.Validate(); err != nil {
		return UploadInfo{}, err
	}
	agent, err := f.uploadAgent()
	if err != nil {
		return UploadInfo{}, err
	}
	raw, err := agent.CreateStagedUpload(ctx, params)
	if err != nil {
		return UploadInfo{}, f.mapError(err, "创建上传暂存")
	}
	info, err := f.decodeUpload(raw)
	if err != nil {
		return UploadInfo{}, err
	}
	// 代理回的必须是**这次声明的那份上传**：换根、换路径或换了大小都意味着
	// 界面接下来会把字节传到一个别的地方去。
	if info.RootID != params.RootID || info.Path != params.Path {
		return UploadInfo{}, fmt.Errorf("%w: 代理创建的上传是 %s:%q，与请求的 %s:%q 不一致",
			ErrAgentUnavailable, info.RootID, info.Path, params.RootID, params.Path)
	}
	if info.SizeBytes != params.SizeBytes {
		return UploadInfo{}, fmt.Errorf("%w: 代理创建的上传声明 %d 字节，与请求的 %d 不一致",
			ErrAgentUnavailable, info.SizeBytes, params.SizeBytes)
	}
	if info.ExpectedVersion != params.ExpectedVersion {
		return UploadInfo{}, fmt.Errorf("%w: 代理创建的上传记录的 expected_version 与请求不一致", ErrAgentUnavailable)
	}
	if info.State != UploadStateOpen {
		return UploadInfo{}, fmt.Errorf("%w: 代理创建的上传状态是 %q，不是 open", ErrAgentUnavailable, info.State)
	}
	return info, nil
}

// PutUploadContent 把 body 完整送进暂存区。
//
// size 是这次传输的声明长度，必须与 CreateUpload 时的 size_bytes 一致：
// 代理按声明做过容量预留，也会在收完后核对实收字节；这里先对一次，
// 免得把一次注定失败的传输发出去。
func (f *Fetcher) PutUploadContent(ctx context.Context, uploadID string, expected UploadInfo, body io.Reader, size int64) (UploadInfo, error) {
	if !ValidUploadID(uploadID) {
		return UploadInfo{}, fmt.Errorf("%w: upload_id 形状不合法", ErrInvalidParams)
	}
	if body == nil {
		return UploadInfo{}, fmt.Errorf("%w: 上传内容不能为空", ErrInvalidParams)
	}
	if size <= 0 || uint64(size) != expected.SizeBytes {
		return UploadInfo{}, fmt.Errorf("%w: 本次传输声明 %d 字节，与上传声明的 %d 不一致",
			ErrInvalidParams, size, expected.SizeBytes)
	}
	if expected.UploadID != uploadID {
		return UploadInfo{}, fmt.Errorf("%w: 上传记录与 upload_id %q 不一致", ErrInvalidParams, uploadID)
	}
	agent, err := f.uploadAgent()
	if err != nil {
		return UploadInfo{}, err
	}
	raw, err := agent.PutStagedUploadContent(ctx, uploadID, body, size)
	if err != nil {
		return UploadInfo{}, f.mapError(err, "上传内容")
	}
	info, err := f.decodeUpload(raw)
	if err != nil {
		return UploadInfo{}, err
	}
	if info.UploadID != uploadID {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的上传是 %q，与请求的 %q 不一致",
			ErrAgentUnavailable, info.UploadID, uploadID)
	}
	// 收完就该是 received 且字节数刚好等于声明值。少一个字节也是失败：
	// 后面 file.commit_upload 会按“实收 == 声明”放行，这一层不替它放宽。
	if info.State != UploadStateReceived {
		if info.State == UploadStateFailed && info.Error != "" {
			return UploadInfo{}, fmt.Errorf("%w: 上传失败: %s", ErrAgentUnavailable, info.Error)
		}
		return UploadInfo{}, fmt.Errorf("%w: 上传收完后状态是 %q，不是 received", ErrAgentUnavailable, info.State)
	}
	if info.ReceivedBytes != info.SizeBytes {
		return UploadInfo{}, fmt.Errorf("%w: 上传收完 %d 字节，声明 %d 字节", ErrAgentUnavailable, info.ReceivedBytes, info.SizeBytes)
	}
	return info, nil
}

// GetUpload 查询一次暂存上传。
func (f *Fetcher) GetUpload(ctx context.Context, uploadID string) (UploadInfo, error) {
	if !ValidUploadID(uploadID) {
		return UploadInfo{}, fmt.Errorf("%w: upload_id 形状不合法", ErrInvalidParams)
	}
	agent, err := f.uploadAgent()
	if err != nil {
		return UploadInfo{}, err
	}
	raw, err := agent.GetStagedUpload(ctx, uploadID)
	if err != nil {
		return UploadInfo{}, f.mapError(err, "查询上传暂存")
	}
	info, err := f.decodeUpload(raw)
	if err != nil {
		return UploadInfo{}, err
	}
	if info.UploadID != uploadID {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的上传是 %q，与请求的 %q 不一致",
			ErrAgentUnavailable, info.UploadID, uploadID)
	}
	return info, nil
}

// DeleteUpload 放弃一次暂存上传（不影响目标文件：目标只有在提交之后才变）。
func (f *Fetcher) DeleteUpload(ctx context.Context, uploadID string) error {
	if !ValidUploadID(uploadID) {
		return fmt.Errorf("%w: upload_id 形状不合法", ErrInvalidParams)
	}
	agent, err := f.uploadAgent()
	if err != nil {
		return err
	}
	if _, err := agent.DeleteStagedUpload(ctx, uploadID); err != nil {
		return f.mapError(err, "删除上传暂存")
	}
	return nil
}

// decodeUpload 解出一条上传记录，并核对状态闭集与字节数的自洽。
func (f *Fetcher) decodeUpload(raw []byte) (UploadInfo, error) {
	var info UploadInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的上传记录不是合法 JSON: %v", ErrAgentUnavailable, err)
	}
	if !ValidUploadID(info.UploadID) {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的 upload_id 形状不合法", ErrAgentUnavailable)
	}
	switch info.State {
	case UploadStateOpen, UploadStateReceived, UploadStateFailed:
	default:
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的上传状态 %q 不在闭集内", ErrAgentUnavailable, info.State)
	}
	if info.ReceivedBytes > info.SizeBytes {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的 received_bytes %d 超过 size_bytes %d",
			ErrAgentUnavailable, info.ReceivedBytes, info.SizeBytes)
	}
	if info.ExpiresAt.IsZero() || info.CreatedAt.IsZero() {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的上传记录缺少 created_at 或 expires_at", ErrAgentUnavailable)
	}
	if info.ExpiresAt.Before(info.CreatedAt) {
		return UploadInfo{}, fmt.Errorf("%w: 代理返回的上传记录 expires_at 早于 created_at", ErrAgentUnavailable)
	}
	return info, nil
}
