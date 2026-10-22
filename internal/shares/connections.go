package shares

// 共享连接状态（P08.2）：读 `smbstatus --json`，按共享名过滤出树连接。
//
// 为什么要读 smbstatus 而不是自己记账：面板不知道客户端什么时候挂断、什么时候换了
// 机器。连接状态只有 Samba 自己知道，面板转述它，比面板猜它可靠。
//
// 三条从真机探测里得出的结论，直接决定了这段代码的形状（证据见
// docs/evidence/P08/2026-10-22-p08.2/probe2-output.txt、probe3-output.txt）：
//
//  1. **空列表有多种含义。** smbd 停掉之后 `smbstatus --json` 仍然以退出码 0 返回
//     `{"sessions":{},"tcons":{},"open_files":{}}`——“没人连接”与“服务没在跑”在输出
//     上完全一样。所以报告里必须带上服务状态，否则页面会把“服务停了”显示成一切正常。
//  2. **树连接说共享，会话说人。** tcons 里有 service/machine/connected_at，
//     而 username/dialect/signing 在 sessions 里，只能靠 session_id 关联起来。
//  3. **打开文件按路径归并。** open_files 以绝对路径为键，里面带 service_path；
//     tcon 里没有路径，因此“这条连接上开了几个文件”只能靠共享目录去匹配。

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// connectionsTimeout 是读一次连接状态的预算。
//
// 比配置校验短：smbstatus 只读共享内存里的会话表，慢说明 smbd 那边卡住了，
// 等下去也不会变好，页面倒是不该跟着卡 20 秒。
const connectionsTimeout = 10 * time.Second

// smbStatusDoc 是 `smbstatus --json` 的解析结构。
//
// 字段全部按真机输出声明。数值型的 id 在 JSON 里是**字符串**（"1879008955"），
// 所以这里一律用 string——用 int 会在换版本时静默变成零值，而零值会让所有会话
// 关联到同一条连接上。
type smbStatusDoc struct {
	Timestamp string                       `json:"timestamp"`
	Version   string                       `json:"version"`
	Sessions  map[string]smbStatusSession  `json:"sessions"`
	Tcons     map[string]smbStatusTcon     `json:"tcons"`
	OpenFiles map[string]smbStatusOpenFile `json:"open_files"`
}

type smbStatusSession struct {
	Username       string          `json:"username"`
	Groupname      string          `json:"groupname"`
	RemoteMachine  string          `json:"remote_machine"`
	Hostname       string          `json:"hostname"`
	SessionDialect string          `json:"session_dialect"`
	Signing        smbStatusCipher `json:"signing"`
}

type smbStatusTcon struct {
	Service     string          `json:"service"`
	TconID      string          `json:"tcon_id"`
	SessionID   string          `json:"session_id"`
	Machine     string          `json:"machine"`
	ConnectedAt string          `json:"connected_at"`
	Signing     smbStatusCipher `json:"signing"`
}

type smbStatusCipher struct {
	Cipher string `json:"cipher"`
	Degree string `json:"degree"`
}

type smbStatusOpenFile struct {
	ServicePath string                     `json:"service_path"`
	Filename    string                     `json:"filename"`
	Opens       map[string]json.RawMessage `json:"opens"`
}

// SigningText 把签名信息渲染成人读的一行。
//
// 拼法是照抄 smbstatus -p 的写法（partial(AES-128-GMAC)），这样面板上显示的东西
// 和用户在机器上敲命令看到的对得上，不需要在脑子里做一次翻译。
func (c smbStatusCipher) SigningText() string {
	switch {
	case c.Degree == "" || c.Degree == "none":
		return "none"
	case c.Cipher == "" || c.Cipher == "-":
		return c.Degree
	default:
		return c.Degree + "(" + c.Cipher + ")"
	}
}

// ParseSmbStatus 解析 smbstatus --json 的输出。
func ParseSmbStatus(data []byte) (smbStatusDoc, error) {
	var doc smbStatusDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return smbStatusDoc{}, err
	}
	return doc, nil
}

// assembleConnections 把 smbstatus 的输出归并成某一份共享上的连接列表。
//
// dir 是这份共享的目录（用来把 open_files 归并到连接上）。dir 为空时不猜：
// OpenFiles 保持 0，由调用方补一条说明——凭空按名字匹配有可能把别的共享上的
// 打开文件算到这一份头上。
func assembleConnections(doc smbStatusDoc, shareName, dir string) []Connection {
	openByPath := make(map[string]int, len(doc.OpenFiles))
	for _, f := range doc.OpenFiles {
		openByPath[strings.TrimRight(f.ServicePath, "/")] += len(f.Opens)
	}

	out := make([]Connection, 0, 4)
	for _, tcon := range doc.Tcons {
		if !strings.EqualFold(tcon.Service, shareName) {
			continue
		}
		conn := Connection{
			Share:       tcon.Service,
			Machine:     tcon.Machine,
			ConnectedAt: tcon.ConnectedAt,
			Signing:     tcon.Signing.SigningText(),
		}
		if session, ok := doc.Sessions[tcon.SessionID]; ok {
			conn.Username = session.Username
			conn.Dialect = session.SessionDialect
			// 树连接的签名常常是 none（签名在会话层协商），会话层才是有信息的那一侧。
			// 两处都不空的时候取树连接的：那是这条连接实际的取值。
			if conn.Signing == "none" {
				conn.Signing = session.Signing.SigningText()
			}
			if conn.Machine == "" {
				conn.Machine = session.RemoteMachine
			}
		}
		if dir != "" {
			conn.OpenFiles = openByPath[strings.TrimRight(dir, "/")]
		}
		out = append(out, conn)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ConnectedAt != out[j].ConnectedAt {
			return out[i].ConnectedAt < out[j].ConnectedAt
		}
		return out[i].Machine < out[j].Machine
	})
	return out
}

// Connections 返回某一份共享此刻的连接状态。
//
// 返回值里的 Service 字段是这段代码的重点：空列表只在“服务确实在跑”时才意味着
// “没有人连着”。服务没跑的时候我们会给出空列表**加**一条说明，页面因此不可能
// 把停服的机器画成健康状态。
func (e *Engine) Connections(ctx context.Context, shareID string) (ConnectionsReport, error) {
	rec, found, err := e.opts.Store.GetShare(ctx, shareID)
	if err != nil {
		return ConnectionsReport{}, err
	}
	if !found {
		return ConnectionsReport{}, notFound("共享 %s 不存在", shareID)
	}

	report := ConnectionsReport{
		ShareID:     rec.ID,
		ShareName:   rec.Definition.Name,
		Service:     e.serviceState(ctx),
		Connections: []Connection{},
		CollectedAt: e.opts.Now().UTC().Truncate(time.Second),
	}

	state := report.Service
	if state == ServiceStateInactive {
		report.Notices = append(report.Notices, Blocker{
			Code:    NoticeSMBServiceInactive,
			Message: "smbd 不在运行：连接列表为空是因为服务没在提供访问，而不是因为确实没有人连",
		})
	} else if state == ServiceStateUnknown {
		report.Notices = append(report.Notices, Blocker{
			Code:    NoticeSMBServiceUnknown,
			Message: "问不出 smbd 的运行状态：下面的连接列表可能不完整",
		})
	}

	tool := e.tools(ctx).Smbstatus
	if tool == "" {
		report.Notices = append(report.Notices, Blocker{
			Code:    NoticeConnectionStatusUnavailable,
			Message: "没找到 smbstatus：读不到连接状态（配置与共享本身不受影响）",
		})
		return report, nil
	}

	res, runErr := e.run(ctx, syscmd.Spec{Program: tool, Args: []string{"--json"}}, connectionsTimeout)
	if runErr != nil {
		// 超时之类：没有输出可解析，明确说读不到，而不是给一个空列表。
		report.Notices = append(report.Notices, Blocker{
			Code:    NoticeConnectionStatusFailed,
			Message: "读取连接状态失败：" + runErr.Error(),
		})
		return report, nil
	}

	doc, parseErr := ParseSmbStatus([]byte(res.Stdout))
	if parseErr != nil {
		return report, commandFailed(
			"smbstatus 的输出解析失败（%s）：%v；输出前 200 字节：%q",
			FailConnectionsRead, parseErr, truncateForMessage(res.Stdout))
	}
	if stderrText := strings.TrimSpace(res.Stderr); stderrText != "" && len(doc.Tcons) == 0 {
		// smbstatus 对非 root 会打印一行提示后返回空表。把它说出来，
		// 否则“需要权限”会被显示成“没有连接”。
		report.Notices = append(report.Notices, Blocker{
			Code:    NoticeConnectionStatusFailed,
			Message: "smbstatus 报告：" + firstLineOrSelf(stderrText),
		})
	}

	dir := e.shareDirQuiet(ctx, rec)
	if dir == "" {
		report.Notices = append(report.Notices, Blocker{
			Code:    NoticeConnectionStatusPartial,
			Message: "算不出这份共享的目录（卷不在受管理卷里？）：连接可以列出，但打开文件数无法归并，显示为 0",
		})
	}
	report.Connections = assembleConnections(doc, rec.Definition.Name, dir)
	if report.Connections == nil {
		report.Connections = []Connection{}
	}
	return report, nil
}

// shareDirQuiet 尽力算出共享目录；算不出来就返回空串（不报错）。
//
// 连接状态查询是只读的，不该因为“卷被移除”而整个失败：能列出谁连着谁，对排障
// 仍然有用。代价是打开文件数归并不了，这一点由调用方明确说出来。
func (e *Engine) shareDirQuiet(ctx context.Context, rec ShareRecord) string {
	vol, found, err := e.volumeFor(ctx, rec.Definition.VolumeID)
	if err != nil || !found {
		return ""
	}
	mountPath := vol.MountPath
	if mountPath == "" {
		mountPath = storage.MountPathOf(vol.Slug)
	}
	dir, err := ShareDir(mountPath, rec.Definition.RelativePath)
	if err != nil {
		return ""
	}
	return dir
}

// firstLineOrSelf 取第一行非空文本。
func firstLineOrSelf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateForMessage(line)
		}
	}
	return ""
}
