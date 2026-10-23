package certificates

// 证书上传的**暂存区**（P10.3）。
//
// CONTRACTS.md 第 9 节：POST /certificates/uploads 只暂存输入，10 分钟到期，绑定会话；
// 导入生效必须执行 certificate.import 任务。这一条把“上传一个文件”和“让证书生效”
// 分成两次动作，理由与 P06.2 的上传完全一样：上传只说明“我手里有这个文件”，
// 导入才说明“我决定让这个域名以后用这张证书”。
//
// 暂存放在**内存**里，不落盘：
//
//   - 里面的内容是公钥证书（最多 1 MiB），本来就要写进 nginx 配置里被读取；
//   - 落盘要多一套清理、权限、残留的考虑，而它只有 10 分钟寿命；
//   - 面板进程重启后暂存消失，用户重传一次即可——这比“磁盘上留着一份没人认领的
//     PEM”要干净。
//
// 绑定的是 (用户, 会话绑定哈希)：同一个人换一个浏览器就要重新上传，这不麻烦，
// 但它保证一个被截获的上传标识在别的会话里用不了。

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"upanel/internal/tlscert"
)

const (
	// StageTTL 是暂存的寿命（CONTRACTS.md：10 分钟）。
	StageTTL = 10 * time.Minute

	// MaxStagedUploads 是同时存在的暂存条目上限。它挡的是“同一个人不停上传”
	// 把内存吃满：每条最多 1 MiB，200 条就是 200 MiB 的上限。
	MaxStagedUploads = 200

	stageIDPrefix = "crtup_"
)

// 暂存相关的错误。
var (
	// ErrStageNotFound 表示暂存不存在、已过期，或不属于这个会话。
	//
	// 三种情况**故意合并成同一个错误**：告诉调用方“这个 id 存在但不是你的”等于
	// 确认了一个 id 的有效性。10 分钟、随机 128 位，本来就不可能猜中，但不给回音
	// 是最省事的做法。
	ErrStageNotFound = errors.New("certificates: 上传已过期或不存在")
	// ErrStageFull 表示暂存区满了。
	ErrStageFull = errors.New("certificates: 未处理的证书上传太多，请稍后再试")
	// ErrStageTooLarge 表示证书超过 1 MiB。
	ErrStageTooLarge = errors.New("certificates: 证书文件超过 1 MiB")
)

// StagedUpload 是一次已上传、待导入的证书。
type StagedUpload struct {
	ID string
	// UserID / SessionHash 是归属，用来拒绝跨会话使用。
	UserID      int64
	SessionHash string
	// Info 是上传时解析出来的事实（预览用）。
	Info tlscert.Info
	// PEM 是原始字节。取用时返回**副本**，避免调用方改动暂存内容。
	PEM       []byte
	CreatedAt time.Time
	ExpiresAt time.Time
}

// StageStore 是暂存区。并发安全；零值不可用，用 NewStageStore。
type StageStore struct {
	mu    sync.Mutex
	now   func() time.Time
	ttl   time.Duration
	max   int
	items map[string]*StagedUpload
}

// NewStageStore 构造暂存区。now 为 nil 时用 time.Now。
func NewStageStore(now func() time.Time) *StageStore {
	if now == nil {
		now = time.Now
	}
	return &StageStore{
		now:   now,
		ttl:   StageTTL,
		max:   MaxStagedUploads,
		items: make(map[string]*StagedUpload),
	}
}

// Put 解析并暂存一份证书。调用方必须已经做完体积检查（1 MiB）。
func (s *StageStore) Put(userID int64, sessionHash string, pemBytes []byte) (StagedUpload, error) {
	if len(pemBytes) == 0 {
		return StagedUpload{}, tlscert.ErrNoCertificate
	}
	if len(pemBytes) > tlscert.MaxCertBytes {
		return StagedUpload{}, ErrStageTooLarge
	}
	if sessionHash == "" {
		return StagedUpload{}, fmt.Errorf("certificates: 暂存必须绑定会话")
	}
	info, err := tlscert.ParseCertificate(pemBytes)
	if err != nil {
		return StagedUpload{}, err
	}
	id, err := newStageID()
	if err != nil {
		return StagedUpload{}, err
	}
	now := s.now().UTC()
	item := StagedUpload{
		ID:          id,
		UserID:      userID,
		SessionHash: sessionHash,
		Info:        info,
		PEM:         append([]byte(nil), pemBytes...),
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if len(s.items) >= s.max {
		return StagedUpload{}, ErrStageFull
	}
	stored := item
	stored.PEM = append([]byte(nil), item.PEM...)
	s.items[id] = &stored
	out := item
	out.PEM = append([]byte(nil), item.PEM...)
	return out, nil
}

// Take 取走一份暂存并**销毁**它。
//
// 取走即销毁：导入是一次性的，预检展开之后参数里已经有内容了（任务会带着它跑），
// 留着这份副本只会让“这份证书还能被用几次”变成一个需要回答的问题。
func (s *StageStore) Take(id string, userID int64, sessionHash string) (StagedUpload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.sweepLocked(now)
	item, ok := s.items[id]
	if !ok {
		return StagedUpload{}, ErrStageNotFound
	}
	if item.UserID != userID || item.SessionHash != sessionHash || !now.Before(item.ExpiresAt) {
		return StagedUpload{}, ErrStageNotFound
	}
	delete(s.items, id)
	out := *item
	out.PEM = append([]byte(nil), item.PEM...)
	return out, nil
}

// Len 返回当前暂存条目数（测试与自检用）。
func (s *StageStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now().UTC())
	return len(s.items)
}

// sweepLocked 清掉过期的条目。调用方必须持有锁。
func (s *StageStore) sweepLocked(now time.Time) {
	for id, item := range s.items {
		if !now.Before(item.ExpiresAt) {
			delete(s.items, id)
		}
	}
}

func newStageID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("certificates: 生成上传标识失败: %w", err)
	}
	return stageIDPrefix + hex.EncodeToString(buf), nil
}

// ValidStageID 判断上传标识的形状（形状不对时直接 404，不必进锁）。
func ValidStageID(id string) bool {
	if !strings.HasPrefix(id, stageIDPrefix) || len(id) != len(stageIDPrefix)+32 {
		return false
	}
	for _, r := range id[len(stageIDPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
