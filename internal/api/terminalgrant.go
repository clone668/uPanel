package api

// 终端连接许可（P06.3，CONTRACTS.md 第 10 节）。
//
// 为什么需要它：WebSocket 握手是 GET，浏览器无法为它加自定义请求头，所以
// 升级请求带不了 X-UPanel-CSRF。把“领许可”拆成一次**带 CSRF 的 POST**，
// 再让许可随子协议进握手，等于给这次升级配了一张一次性、30 秒、绑定
// 用户与会话的门票：
//
//   - 只存在 Web 进程内存里，不落库、不写日志、不进 URL；
//   - 单次使用：无论校验成功还是失败，用过即作废（失败也删，防止探测式重放）；
//   - 数量有上限：许可不可累积，坏掉的前端不能靠反复领取把内存撑大。

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	// terminalGrantTTL 是连接许可的有效期。CONTRACTS.md 第 10 节写明 30 秒：
	// 够一次握手往返，不够挂在那里等别人来用。
	terminalGrantTTL = 30 * time.Second
	// terminalGrantMax 是同时存在的许可上限。
	//
	// 全局会话上限是 2、每用户 1，正常页面最多同时握着 1—2 张许可；32 已经
	// 宽出两个数量级，因此这里到顶只可能是客户端在刷接口，不是容量不够。
	terminalGrantMax = 32
	// terminalGrantIDBytes 是许可 id 的随机字节数（16 字节 → 32 位十六进制）。
	terminalGrantIDBytes = 16
)

// errGrantStoreFull 表示许可数量已达上限。
var errGrantStoreFull = errors.New("终端连接许可数量已达上限")

// terminalGrant 是一张一次性连接许可。
type terminalGrant struct {
	sessionID string
	userID    int64
	expiresAt time.Time
}

// terminalGrants 是进程内的许可表。
type terminalGrants struct {
	mu    sync.Mutex
	items map[string]terminalGrant
	// now 只为测试可替换；生产路径用 time.Now。
	now func() time.Time
}

// newTerminalGrants 构造许可表。
func newTerminalGrants() *terminalGrants {
	return &terminalGrants{items: make(map[string]terminalGrant), now: time.Now}
}

// issue 签发一张许可，返回 id 与到期时间。
//
// 先清过期项再判上限：一张过期的许可不该占着位置。
func (g *terminalGrants) issue(userID int64, sessionID string) (string, time.Time, error) {
	if userID <= 0 || sessionID == "" {
		return "", time.Time{}, errors.New("终端许可缺少归属信息")
	}
	id, err := newGrantID()
	if err != nil {
		return "", time.Time{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweepLocked(now)
	if len(g.items) >= terminalGrantMax {
		return "", time.Time{}, errGrantStoreFull
	}
	expiresAt := now.Add(terminalGrantTTL)
	g.items[id] = terminalGrant{sessionID: sessionID, userID: userID, expiresAt: expiresAt}
	return id, expiresAt, nil
}

// consume 校验并作废一张许可。
//
// 三个条件全中才算通过：存在、未过期、归属匹配。无论结果如何都先删掉——
// 许可是一次性凭据，失败的那次重放不该留下可以继续试的余量。
func (g *terminalGrants) consume(id string, userID int64, sessionID string) bool {
	if !validGrantID(id) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	grant, ok := g.items[id]
	if !ok {
		return false
	}
	delete(g.items, id)
	if g.now().After(grant.expiresAt) {
		return false
	}
	return grant.userID == userID && grant.sessionID == sessionID
}

// sweepLocked 清掉过期项。调用方必须持有 g.mu。
func (g *terminalGrants) sweepLocked(now time.Time) {
	for id, grant := range g.items {
		if now.After(grant.expiresAt) {
			delete(g.items, id)
		}
	}
}

// newGrantID 生成 32 位小写十六进制的许可 id。
//
// 用 crypto/rand 而不是自增或时间戳：许可 id 本身就是“持有即可用”的凭据，
// 可预测的 id 等于没有许可这一层。
func newGrantID() (string, error) {
	buf := make([]byte, terminalGrantIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// validGrantID 判断许可 id 的形态：32 位小写十六进制。
//
// 形态先判一次，是为了让明显畸形的子协议令牌连查表都不做：
// 与 session_id 同一形状，但两者用途不同，不共用命名。
func validGrantID(id string) bool {
	if len(id) != terminalGrantIDBytes*2 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
