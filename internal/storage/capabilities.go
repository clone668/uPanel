package storage

// 设备能力（P07.1）。
//
// CONTRACTS.md 第 6.1 节：`capabilities` 在 P04.2 恒为空数组（那时没有任何写动作登记），
// 从 P07 起按实际情况填充，**不得返回伪 available**。所以这里的取值不是人写死的，
// 而是把预检判决原样投影到每台设备上：能做的动作、被什么挡住、类型上根本不支持。
//
// 界面据此决定按钮的可点状态；真正的放行判断在预检（POST /operations/preflight），
// 那里的判决永远是新鲜的，这里的只是缓存了 30 秒的展示用快照。

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// 能力状态闭集（CONTRACTS.md 第 6 节）。
const (
	// CapabilityAvailable：现在可以做，且不会被预检拒绝。
	CapabilityAvailable = "available"
	// CapabilityBlocked：动作本身适用于这种设备，但当前状态下被拒绝
	// （受保护、已挂载、被占用、身份不可靠、分区表不支持……）。
	CapabilityBlocked = "blocked"
	// CapabilityUnsupported：动作与这种设备类型无关（例如对整盘格式化、对分区初始化分区表），
	// 或者根本看不到这台设备。这类动作连按钮都不该出现。
	CapabilityUnsupported = "unsupported"
)

// DefaultCapabilityTTL 是设备能力的缓存时长。
//
// 与容量同样是 30 秒：占用与签名是“现在”的观测值，而能力列表只是展示用的投影，
// 每次都重算会让 5 秒的扫描循环反复遍历 /proc 和读 /dev。真正需要新鲜度的判断
// 在预检里，那里每次点击都重新算。
const DefaultCapabilityTTL = 30 * time.Second

// capabilityActions 是投影到设备上的动作与顺序（固定顺序让界面稳定）。
var capabilityActions = []string{
	ActionPartitionTableInitialize,
	ActionPartitionCreate,
	ActionPartitionDelete,
	ActionFilesystemFormat,
}

// DeviceCapabilities 计算一台设备上四个存储动作的状态。
//
// 用的是与预检完全相同的判决函数，因此“界面按钮亮着但预检拒绝”这种情况不会发生；
// 反过来也不成立——能力有 30 秒缓存，而预检没有。
func DeviceCapabilities(ctx context.Context, topo Topology, devID string, env Env) []Capability {
	out := make([]Capability, 0, len(capabilityActions))
	for _, action := range capabilityActions {
		in := capabilityInput(action, devID)
		verdict := assess(ctx, topo, in, env)
		out = append(out, capabilityFor(action, verdict.Blockers))
	}
	return out
}

// capabilityInput 构造“还选好参数”的判决输入。
//
// 建分区没有具体区间时只回答“这块盘上现在能不能建分区”（不校验对齐与重叠）；
// 格式化没有选定文件系统时只回答“这个分区现在能不能被格式化”。
func capabilityInput(action, devID string) assessmentInput {
	switch action {
	case ActionPartitionTableInitialize:
		return assessmentInput{action: action, deviceID: devID, tableType: TableTypeGPT}
	default:
		return assessmentInput{action: action, deviceID: devID}
	}
}

// capabilityFor 把阻塞项投影成能力状态。
func capabilityFor(action string, blockers []Blocker) Capability {
	caps := Capability{Action: action, State: CapabilityAvailable}
	if len(blockers) == 0 {
		return caps
	}
	for _, b := range blockers {
		switch b.Code {
		case NoticeUnsupportedKind, NoticeDeviceMissing, NoticeTopologyUnavailable:
			caps.State = CapabilityUnsupported
			caps.Reason = b.Message
			return caps
		}
	}
	caps.State = CapabilityBlocked
	caps.Reason = joinReasons(blockers, 2)
	return caps
}

// joinReasons 把前 n 条阻塞项的理由拼成一句，多出来的用计数带过。
func joinReasons(blockers []Blocker, n int) string {
	parts := make([]string, 0, n+1)
	for i, b := range blockers {
		if i == n {
			parts = append(parts, "另有 "+strconv.Itoa(len(blockers)-n)+" 项限制")
			break
		}
		parts = append(parts, b.Message)
	}
	return strings.Join(parts, "；")
}

// memoEnv 在一次能力填充里复用签名与占用结果。
//
// 一台设备有四个动作，每个动作都要问“有人打开着吗”“盘上是什么签名”。
// 同一次扫描里这两件事的答案不会变，问四遍只是把 /proc 走四遍。
type memoEnv struct {
	env  Env
	ctx  context.Context
	sigs map[string]probeOutcome
	busy map[string]busyOutcome
}

type probeOutcome struct {
	sig Signature
	err error
}

type busyOutcome struct {
	procs []BusyProcess
	err   error
}

func newMemoEnv(ctx context.Context, env Env) *memoEnv {
	return &memoEnv{
		env:  env,
		ctx:  ctx,
		sigs: map[string]probeOutcome{},
		busy: map[string]busyOutcome{},
	}
}

// Probe 实现 SignatureProber。
func (m *memoEnv) Probe(_ context.Context, devID string, sizeBytes uint64) (Signature, error) {
	if out, ok := m.sigs[devID]; ok {
		return out.sig, out.err
	}
	sig, err := m.env.Signatures.Probe(m.ctx, devID, sizeBytes)
	m.sigs[devID] = probeOutcome{sig: sig, err: err}
	return sig, err
}

// Scan 实现 BusyScanner。
func (m *memoEnv) Scan(_ context.Context, devID string) ([]BusyProcess, error) {
	if out, ok := m.busy[devID]; ok {
		return out.procs, out.err
	}
	procs, err := m.env.Busy.Scan(m.ctx, devID)
	m.busy[devID] = busyOutcome{procs: procs, err: err}
	return procs, err
}

// asEnv 把记忆层包成判决用的 Env。
func (m *memoEnv) asEnv() Env {
	return Env{Signatures: m, Busy: m}
}

// cachedCapability 是一台设备的能力缓存。
type cachedCapability struct {
	caps []Capability
	at   time.Time
	// version 是计算这份能力时的拓扑版本。拓扑内容变了就必须重算：
	// 分区表、挂载、保护标记都可能是刚变的。
	version string
}

// fillCapabilities 给快照里的每台设备填上能力列表（30 秒缓存）。
//
// 没有配置探针时**什么都不填**（保持空数组）：那表示“代理还不知道能做什么”，
// 不是“什么都能做”。P04.2 的空数组语义在没接线的情况下继续保持。
func (c *Collector) fillCapabilities(ctx context.Context, topo *Topology, now time.Time) {
	if c.env.Signatures == nil && c.env.Busy == nil {
		return
	}
	memo := newMemoEnv(ctx, c.env)
	env := memo.asEnv()
	seen := make(map[string]bool, len(topo.Nodes))

	for i := range topo.Nodes {
		dev := &topo.Nodes[i]
		// 一律按设备 id 算、按 id 缓存：判决函数也是按 id 查设备的，
		// 用 Name 只是碰巧现在两者相同。
		seen[dev.ID] = true
		if caps, ok := c.cachedCapabilitiesFor(dev.ID, topo.SnapshotVersion, now); ok {
			dev.Capabilities = caps
			continue
		}
		caps := DeviceCapabilities(ctx, *topo, dev.ID, env)
		dev.Capabilities = caps
		c.storeCapabilities(dev.ID, caps, topo.SnapshotVersion, now)
	}
	c.pruneCapabilities(seen)
}

func (c *Collector) cachedCapabilitiesFor(devID, version string, now time.Time) ([]Capability, bool) {
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	entry, ok := c.deviceCaps[devID]
	if !ok || entry.version != version {
		return nil, false
	}
	age := now.Sub(entry.at)
	if age < 0 || age >= c.capabilityTTL {
		return nil, false
	}
	return append([]Capability{}, entry.caps...), true
}

func (c *Collector) storeCapabilities(devID string, caps []Capability, version string, now time.Time) {
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	c.deviceCaps[devID] = cachedCapability{
		caps:    append([]Capability{}, caps...),
		at:      now,
		version: version,
	}
}

func (c *Collector) pruneCapabilities(seen map[string]bool) {
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	for id := range c.deviceCaps {
		if !seen[id] {
			delete(c.deviceCaps, id)
		}
	}
}
