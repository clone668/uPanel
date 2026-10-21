package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"upanel/internal/logging"
)

// Entry 是一条日志在 API 里的形状。
//
// 字段刻意少：只给出定位、分级与可显示文本。原始 journal 字段（_AUDIT_LOGINUID、
// _BOOT_ID、_MACHINE_ID 等）一律不下发——它们对排查有用，但也把整机指纹一起带出去了，
// 而本阶段的产品需求是“按 unit/时间/级别查日志”，不是 journal 字段浏览器。
type Entry struct {
	// Cursor 是 journald 的 __CURSOR：翻页与断线续传都靠它，它是**不透明**的。
	Cursor string `json:"cursor"`
	// Timestamp 是 __REALTIME_TIMESTAMP（UTC）。
	Timestamp time.Time `json:"timestamp"`
	// Level 是 syslog 级别；读不出来时是 "unknown"，不冒充 info。
	Level Level `json:"level"`
	// Message 是脱敏、截断后的消息文本。不可显示时为空串（见 Binary）。
	Message string `json:"message"`
	// Truncated 表示消息超过单条上限被截断（截断必须显式标记）。
	Truncated bool `json:"truncated"`
	// Redacted 表示这条消息里有内容被秘密过滤规则抹掉了。
	// 它是“抹过”的事实，不是“已经干净”的保证（PLAN.md 第 14 节）。
	Redacted bool `json:"redacted"`
	// Binary 表示 journald 没有给这条消息一个可显示的文本（MESSAGE 为 null 或字节数组）。
	// 实测 systemd 255 对超过约 8 KiB 的单条消息就是这么处理的：内容存在，但不是字符串。
	Binary bool `json:"binary"`
	// Unit 是 _SYSTEMD_UNIT；内核日志与套接字写入的条目可能没有。
	Unit string `json:"unit,omitempty"`
	// Identifier 是 SYSLOG_IDENTIFIER（程序自报的名字）。
	Identifier string `json:"identifier,omitempty"`
	// Comm 是 _COMM（内核记录的可执行名）。
	Comm string `json:"comm,omitempty"`
	// PID 是 _PID；字段缺失时为 null，而不是 0（0 是合法 PID 的邻居，容易被误读）。
	PID *int `json:"pid"`
}

// Page 是一页日志查询结果。
type Page struct {
	Source string `json:"source"`
	Unit   string `json:"unit,omitempty"`
	Level  Level  `json:"level,omitempty"`
	// From/To 是本次实际生效的时间窗口（已补齐默认值），用来让页面说清“查的是哪一段”。
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Entries 按时间**倒序**（最新在前）。翻页靠 NextCursor 往更早走。
	Entries []Entry `json:"entries"`
	// NextCursor 为 null 表示没有更早的条目了。
	NextCursor *string `json:"next_cursor"`
	// Truncated 表示这一页不是“查询结果的全部”，而是被条数或字节上限截断的。
	Truncated bool `json:"truncated"`
	// TruncatedReason 说明截断原因：page_bytes 表示是字节上限，空表示是条数上限。
	TruncatedReason string `json:"truncated_reason,omitempty"`
	// Scanned 是实际解析过的条数（含被丢弃的重复游标条目），用于诊断“是不是扫太多了”。
	Scanned int `json:"scanned"`
	// DurationMS 是这次查询花掉的时间。
	DurationMS int64 `json:"duration_ms"`
}

// rawEntry 是 journalctl -o json 的单行结构。只声明用得到的字段：
// 未知字段不影响解码，而声明越少，未来 journald 加字段时这里越不需要改。
type rawEntry struct {
	Cursor     string          `json:"__CURSOR"`
	Realtime   string          `json:"__REALTIME_TIMESTAMP"`
	Priority   string          `json:"PRIORITY"`
	Message    json.RawMessage `json:"MESSAGE"`
	Identifier string          `json:"SYSLOG_IDENTIFIER"`
	PID        string          `json:"_PID"`
	Comm       string          `json:"_COMM"`
	Unit       string          `json:"_SYSTEMD_UNIT"`
}

// parseEntry 把一行 journalctl JSON 解成 Entry。
func parseEntry(line []byte) (Entry, error) {
	var raw rawEntry
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{}, fmt.Errorf("journal: 无法解析 journalctl 输出: %w", err)
	}
	if raw.Cursor == "" {
		// 没有游标的条目没法翻页，也没法续传。宁可整次查询失败，
		// 也不给出“看起来正常但翻不了页”的结果。
		return Entry{}, fmt.Errorf("journal: journalctl 输出缺少 __CURSOR")
	}
	ts, err := parseRealtime(raw.Realtime)
	if err != nil {
		return Entry{}, err
	}

	entry := Entry{
		Cursor:     raw.Cursor,
		Timestamp:  ts,
		Level:      LevelFromPriority(raw.Priority),
		Unit:       raw.Unit,
		Identifier: raw.Identifier,
		Comm:       raw.Comm,
	}
	if pid, ok := parsePID(raw.PID); ok {
		entry.PID = &pid
	}
	message, binary := parseMessage(raw.Message)
	entry.Binary = binary
	entry.Message, entry.Truncated = truncateMessage(message)
	entry.Message, entry.Redacted = logging.Redact(entry.Message)
	return entry, nil
}

// parseRealtime 解析 __REALTIME_TIMESTAMP（微秒级 Unix 时间戳字符串）。
func parseRealtime(value string) (time.Time, error) {
	micros, err := strconv.ParseInt(value, 10, 64)
	if err != nil || micros <= 0 {
		return time.Time{}, fmt.Errorf("journal: __REALTIME_TIMESTAMP %q 不是有效的微秒时间戳", value)
	}
	return time.UnixMicro(micros).UTC(), nil
}

// parsePID 解析 _PID：缺失或不是整数时返回 ok=false（对应 null）。
func parsePID(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// parseMessage 解析 MESSAGE。
//
// 实测（systemd 255，本机 VM）：单条消息超过约 8 KiB 时 journald 在 JSON 输出里给的是
// null；非 UTF-8 内容会被导出成字节数组。两种情况都不是“没有消息”，但也都不能当成
// 文本展示，因此返回空串 + binary=true，而不是编一段占位文本冒充原文。
func parseMessage(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	// null 必须先判：json.Unmarshal(null, &string) 是**成功**的（把 null 当成“不设置”），
	// 于是它会安静地返回空串，把“journald 没给文本”伪装成“这是一条空消息”。
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, false
	}
	// 字节数组：非文本内容，同样按“不可显示”处理。
	return "", true
}

// truncateMessage 把消息截到单条上限，并把截断事实返回给调用方。
//
// 按 rune 边界截断：切在 UTF-8 中间会造出非法字节，前端拿到就是一条乱码行。
func truncateMessage(message string) (string, bool) {
	if len(message) <= MaxMessageBytes {
		return message, false
	}
	cut := MaxMessageBytes
	for cut > 0 && !utf8StartsRune(message[cut]) {
		cut--
	}
	return message[:cut], true
}

// utf8StartsRune 判断 b 是否是一个 UTF-8 字符的起始字节。
func utf8StartsRune(b byte) bool { return b&0xC0 != 0x80 }
