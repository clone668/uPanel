package scripts

// 受管理脚本的规则（P12.4；CONTRACTS.md 第 5、12、14.2 节）。
//
// 这一层只回答四件事：脚本**能不能被接受**（名字与内容的边界）、它**落盘到哪**
// （固定目录下的 <id>.sh）、**内容指纹是多少**（运行前要与磁盘上的字节比对）、
// 以及**以谁的身份跑**（upanel-shell，永远不是 root）。
//
// 两条刻意的取舍：
//
//	内容不加密     它是管理员自己上传的文本，不是秘密。加密不会让它更安全，
//	               只会让"GET /scripts/:id 把内容还给管理员"变成一条要先解密的路。
//	               真正的边界在降权执行那一步，不在这份字节上。
//	路径不可指定   文件名只由 id 拼出来（<id>.sh），目录是常量。调用方给不了路径，
//	               因此"写一个 cron 文件"或"覆盖 /etc/passwd"在这里没有入口。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 动作名（CONTRACTS.md 第 5 节，冻结）。
const (
	// ActionScriptApply 新建或修改一条受管理脚本。
	ActionScriptApply = "script.apply"
	// ActionScriptDelete 删除一条受管理脚本。
	ActionScriptDelete = "script.delete"
	// ActionScriptRun 以 upanel-shell 身份运行一条受管理脚本。
	ActionScriptRun = "script.run"
)

const (
	// DefaultDir 是受管理脚本的固定目录（root 拥有，upanel-shell 只读可执行）。
	DefaultDir = "/var/lib/upanel-agent/scripts"

	// MaxContentBytes 是脚本正文的字节上限（CONTRACTS §12：最多 64 KiB）。
	MaxContentBytes = 64 << 10

	// MaxNameLen 是脚本名的字符数上限。
	MaxNameLen = 80

	// MaxRuntimeSecondsLimit 是单次脚本运行的时长上限（24 小时）。
	//
	// 与计划的 max_runtime_seconds 同一个数：计划把上限带进 script.run，
	// 两处必须一致，否则"计划说 2 小时、脚本却被允许跑到明天"。
	MaxRuntimeSecondsLimit = 24 * 60 * 60

	// ShellUser 是脚本的执行身份。
	//
	// 它是安装时创建的**真实 shell 账号**（有 /bin/bash、口令锁定、不在 sudo/docker/disk
	// 组里），也是受限终端的账号。脚本以它运行，因此永远拿不到 root：
	// CONTRACTS §5 的 script.run 那一行写死了这一点。
	ShellUser = "upanel-shell"

	// Interpreter 是固定解释器。
	//
	// 只有 /bin/sh，不接受调用方指定解释器或参数。一个能选解释器的"脚本"就是
	// 一个能选择 shell 二进制的入口。
	Interpreter = "/bin/sh"
)

// FileMode 是落盘文件的权限：root 拥有、组可读可执行（组是 upanel-shell 的主组）。
//
// 0750 而不是 0755：其他本机用户不该读到受管理脚本的内容；而 upanel-shell 需要
// 读与执行，所以属组给到它。
const FileMode = 0o750

// DirMode 是脚本目录自身的权限：root 拥有、属组 upanel-shell。
//
// 目录只给属组 x，文件才给组 r：upanel-shell 必须能"穿过"目录才打得开 <id>.sh，
// 而 0750 里的 r 让它也能列出目录——那本来就是要跑的那批脚本，不算多给。
const DirMode = 0o750

var (
	idPattern   = regexp.MustCompile(`^scr_[0-9a-f]{32}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidID 判断脚本 id 形态。文件名由它拼出来，所以这里严格。
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidHash 判断内容指纹形态。
func ValidHash(h string) bool { return hashPattern.MatchString(h) }

// Input 是 script.apply 的输入（名字与正文）。
type Input struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// ValidationError 是一条带字段名的校验失败。
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, msg string) error { return &ValidationError{Field: field, Message: msg} }

// Normalize 只做一件事：把 Windows 换行统一成 LF，并去掉名字首尾空白。
//
// 换行统一是必要的：CRLF 的脚本在 /bin/sh 下会因为行尾的 \r 报
// `not found`，而那看起来像"脚本没上传成功"。
func (in Input) Normalize() Input {
	content := strings.ReplaceAll(in.Content, "\r\n", "\n")
	return Input{Name: strings.TrimSpace(in.Name), Content: content}
}

// Validate 校验名字与正文。
func (in Input) Validate() error {
	name := strings.TrimSpace(in.Name)
	if n := len([]rune(name)); n == 0 {
		return invalid("name", "不能为空")
	} else if n > MaxNameLen {
		return invalid("name", fmt.Sprintf("最多 %d 个字符，收到 %d 个", MaxNameLen, n))
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return invalid("name", "不能含控制字符")
	}
	if in.Content == "" {
		return invalid("content", "不能为空")
	}
	if len(in.Content) > MaxContentBytes {
		return invalid("content", fmt.Sprintf("最多 %d 字节，收到 %d 字节", MaxContentBytes, len(in.Content)))
	}
	if !utf8.ValidString(in.Content) {
		return invalid("content", "必须是 UTF-8 文本")
	}
	// NUL 不能在 shell 脚本里出现，而且它会让"文件里的字节"与"库里的字节"在
	// 某些工具下看起来一样、执行起来不一样。
	if strings.IndexByte(in.Content, 0) >= 0 {
		return invalid("content", "不能含 NUL 字节")
	}
	return nil
}

// Hash 返回正文的 SHA-256 十六进制值。
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Path 返回脚本的落盘路径。路径只由固定目录与 id 拼成。
func Path(dir, id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("脚本 id %q 不合法，不能拼进路径", id)
	}
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("脚本目录必须是绝对路径，当前为 %q", dir)
	}
	return filepath.Join(dir, id+".sh"), nil
}
