package packages

import (
	"context"
	"encoding/json"
	"time"
)

// 本文件回答一个只属于电源动作的问题：**它到底发生了没有**。
//
// 重启与关机的受理结果都是 accepted（CONTRACTS.md 4.3）：代理把 `systemctl reboot`
// 交出去、拿到退出码，它能说的就到这里。真正的那次重启发生在命令之后、发生在代理
// 进程消失之后，所以"请求已受理"永远不等于"已经重启"。
//
// 唯一能把话说全的证据是 boot_id——内核每次开机换一个。于是任务被读回时，如果这是
// system.power 且结果是 accepted，就去问代理"你**此刻**的 boot_id 是哪个"，与下发前
// 记下的那个比：
//
//   - 不同 → confirmed：这次重启确实发生过。
//   - 相同 → unchanged：请求已受理，但机器还在原来那次开机上——它可能还在倒计时，
//     也可能被取消、或者重启被别的东西挡了下来。不猜是哪种。
//   - 代理连不上 → unreachable：符合关机后的预期，但**不构成关机成功的证据**。
//   - 代理在、boot_id 读不出来 → unknown：什么都不说。
//
// 关机之后没有任何可用证据能证明电源真的切断了：代理死了，面板连电源灯都看不到。
// 所以对关机只能说"已下发、连接已断开、最终状态无法确认"，一个字都不能多。

// 核对状态的闭集。四个值对应四种能站得住的结论，没有第五种"应该差不多"。
const (
	// PowerCheckConfirmed 表示 boot_id 变了：重启确实发生过。
	PowerCheckConfirmed = "confirmed"
	// PowerCheckUnchanged 表示 boot_id 没变：请求已受理，但重启/关机还没发生。
	PowerCheckUnchanged = "unchanged"
	// PowerCheckUnreachable 表示代理此刻连不上。对关机是预期之内，但不是证据。
	PowerCheckUnreachable = "unreachable"
	// PowerCheckUnknown 表示查不清（boot_id 读不出来、任务里没记下基准）。
	PowerCheckUnknown = "unknown"
)

// PowerActionName 是电源动作在任务表里的 action 名（CONTRACTS.md 5）。
const PowerActionName = "system.power"

// powerOutcomeAccepted 是"命令已交出去"这个结论的字面值（CONTRACTS.md 4.3 的闭集）。
// 同上：不 import 代理侧包，只认这个字符串。
const powerOutcomeAccepted = "accepted"

// 电源操作名。与代理侧 power.OpReboot / power.OpPoweroff 同一个闭集，
// 但这里不 import 代理侧包：面板二进制不该为了两个字符串把 systemctl 那套拖进来。
const (
	PowerOperationReboot   = "reboot"
	PowerOperationPoweroff = "poweroff"
)

// PowerCheckTimeout 是一次核对自己允许花的时间。
//
// 这是一条**附带**结论，不是任务的真相。任务查询页面每两秒轮询一次，让一次核对
// 把轮询拖住是本末倒置；代理刚重启时连接可能不是立刻失败而是挂到超时。
const PowerCheckTimeout = 5 * time.Second

// PowerCheckView 挂在单个任务视图的 power_check 字段上。
//
// 只有 system.power 且结果为 accepted 的任务才有这一节；其余任务是 null——
// null 说的是"这个问题对这条任务不成立"，不是"没查到"。
type PowerCheckView struct {
	// Operation 是 reboot / poweroff，回显任务自己声称做的是哪件事。
	Operation string `json:"operation"`
	// State 是闭集里的一个值（见上面四个常量）。
	State string `json:"state"`
	// Changed 是 boot_id_before 与 boot_id_now 的比对结果。读不到时为 false，
	// 必须配合 State 看：unreachable/unknown 下它不代表"没变"。
	Changed bool `json:"changed"`
	// BootIDBefore 是下发那一刻代理看到的 boot_id（任务里记下的），可能为空。
	BootIDBefore string `json:"boot_id_before"`
	// BootIDNow 是这次核对读到的 boot_id，读不到时为空。
	BootIDNow string `json:"boot_id_now"`
	// CheckedAt 是核对时间。
	CheckedAt time.Time `json:"checked_at"`
	// Message 是给页面直接用的一句话，已按操作与状态选好措辞。
	Message string `json:"message"`
}

// powerJobResult 是任务 result 里本文件关心的那一小部分。
//
// 只取两个字段：结果是代理写的，这里不去解析整份 observed_state，也不需要。
type powerJobResult struct {
	Outcome       string `json:"outcome"`
	ObservedState struct {
		Operation    string `json:"operation"`
		BootIDBefore string `json:"boot_id_before"`
	} `json:"observed_state"`
}

// PowerJobCheck 给一条已完成的电源任务补一次核对；不适用时返回 nil。
//
// 永远不返回 error：这是任务的附加说明，读不到就说读不到，绝不能让"重启任务查询
// 因为附加说明失败而报错"——那会让管理员在最需要看结论的时候什么都看不到。
func (s *Service) PowerJobCheck(ctx context.Context, action string, result json.RawMessage) *PowerCheckView {
	if action != PowerActionName {
		return nil
	}
	var res powerJobResult
	if err := json.Unmarshal(result, &res); err != nil {
		// 结果不是本文件认识的样子（失败任务是空对象）：没有可核对的东西。
		return nil
	}
	if res.Outcome != powerOutcomeAccepted {
		return nil
	}
	op := res.ObservedState.Operation
	if op != PowerOperationReboot && op != PowerOperationPoweroff {
		return nil
	}
	view := &PowerCheckView{
		Operation:    op,
		BootIDBefore: res.ObservedState.BootIDBefore,
		CheckedAt:    s.now().UTC(),
	}
	if view.BootIDBefore == "" {
		// 下发前没读到 boot_id 的话，这次核对从一开始就没有基准。
		view.State = PowerCheckUnknown
		view.Message = "任务里没有记下下发前的 boot_id，无法比对——这次" + operationNoun(op) + "到底有没有发生，面板回答不了。"
		return view
	}

	probeCtx, cancel := context.WithTimeout(ctx, PowerCheckTimeout)
	defer cancel()
	state, err := s.source.PowerState(probeCtx)
	if err != nil {
		view.State = PowerCheckUnreachable
		view.Message = unreachableMessage(op)
		return view
	}
	if state.BootIDError != "" {
		view.State = PowerCheckUnknown
		view.Message = "读不到当前 boot_id（" + state.BootIDError + "），这次" + operationNoun(op) + "无法确认。"
		return view
	}
	if state.BootID == "" {
		view.State = PowerCheckUnknown
		view.Message = "代理没有给出当前 boot_id，这次" + operationNoun(op) + "无法确认。"
		return view
	}

	view.BootIDNow = state.BootID
	view.Changed = state.BootID != view.BootIDBefore
	if view.Changed {
		view.State = PowerCheckConfirmed
		view.Message = confirmedMessage(op, view.BootIDBefore, state.BootID)
		return view
	}
	view.State = PowerCheckUnchanged
	view.Message = unchangedMessage(op, view.BootIDBefore)
	return view
}

// operationNoun 把操作名变成中文名词，只用在拼句子里。
func operationNoun(op string) string {
	if op == PowerOperationPoweroff {
		return "关机"
	}
	return "重启"
}

func confirmedMessage(op, before, now string) string {
	segs := "boot_id 从 " + shortBootID(before) + " 变成 " + shortBootID(now)
	if op == PowerOperationPoweroff {
		return "机器已经回来过：" + segs + "。这说明那次关机之后它重新启动过；" +
			"但电源当时是否真的切断，面板给不出证据，只能由现场回答。"
	}
	return "重启已确认：" + segs + "。"
}

func unchangedMessage(op, before string) string {
	if op == PowerOperationPoweroff {
		return "代理仍在原来的 boot_id（" + shortBootID(before) + "）上：关机还没有发生——" +
			"可能还在倒计时，也可能已被取消。"
	}
	return "代理仍在原来的 boot_id（" + shortBootID(before) + "）上：重启还没有发生。" +
		"它可能还在倒计时，也可能已被取消。"
}

func unreachableMessage(op string) string {
	if op == PowerOperationPoweroff {
		return "代理已不可达。这与下发关机之后的预期一致，但面板无法据此确认电源已切断——" +
			"最终状态需要现场确认。"
	}
	return "代理此刻连不上，读不到 boot_id：重启可能正在进行，也可能根本没起来。" +
		"这一步不构成重启成功的证据，等它回来再看。"
}

// shortBootID 把 36 字符的 boot_id 截短到前 8 位，只用于人读的句子里。
func shortBootID(id string) string {
	const keep = 8
	if len(id) <= keep {
		return id
	}
	return id[:keep]
}
