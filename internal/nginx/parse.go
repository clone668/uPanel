// Package nginx 读主机上 Nginx 的**现状**：装没装、在不在跑、有哪些配置文件和 server 块。
//
// 它只读，不写、不测、不 reload——那些是 P10.2 的动作，必须走预检 → 任务链路。
// 这个包存在的原因是：面板要判断“这个域名能不能给我用”，就必须先知道机器上已经
// 有哪些 server_name。靠用户自己说“没冲突”不是判断，是许愿。
//
// 三条纪律：
//
//  1. **读不到就说读不到，不猜。** 文件解析失败、目录读不了、nginx 没装，各有一种
//     明确的表示（Reason / Error / Available），绝不用“没找到 server_name”冒充
//     “这个域名没人用”。后者会让人把面板写到别人的站点上。
//  2. **只管自己命名规则下的文件。** `upanel-<domain>.conf` 是面板的文件（Managed），
//     其余一律是用户的文件：可以读、可以据以判定冲突，但永远不写、不删、不改名。
//  3. **不跟随 include。** 只扫 nginx.conf 与两个已知目录（conf.d、sites-enabled）里的
//     文件，逐个解析。include 指向别的目录时会漏掉那里的 server_name——这条局限写在
//     Facts.Warnings 里随结果一起返回，而不是埋在这段注释里。
package nginx

import (
	"strings"
	"unicode/utf8"
)

// directive 是配置树里的一个节点：要么是一条简单指令（有 Args、无 Block），
// 要么是一个块（有 Block，例如 `server { … }`、`location / { … }`）。
type directive struct {
	Name     string
	Args     []string
	Block    []directive
	HasBlock bool
	// Line 是指令名所在的行号，用来把结论落回具体位置（“哪个文件的第几行声明了
	// 这个域名”）。没有行号，冲突报告就只能说“某个文件里有”，等于没说。
	Line int
}

// maxParseDepth 是配置嵌套深度上限。
//
// 用来挡住畸形输入（例如几千个未闭合的 `{`）把解析器推进栈溢出；真实配置最深也就
// server → location → if 三层。
const maxParseDepth = 32

// maxDirectiveArgs 是单条指令的参数个数上限。真实的 server_name 可以有一串域名，
// 几百个已经很多了；超过这个数说明文件不是配置。
const maxDirectiveArgs = 512

// parseError 是解析失败的原因，带行号。
type parseError struct {
	Line int
	Msg  string
}

func (e *parseError) Error() string {
	if e.Line > 0 {
		return "第 " + itoa(e.Line) + " 行：" + e.Msg
	}
	return e.Msg
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// parseConfig 把一份 Nginx 配置解析成指令树。
//
// 只做配置语法本身：注释、引号、块。不解释任何指令的含义——理解 `server_name` 的是
// 调用方（collect），这样加一条“还关心哪个指令”不需要改解析器。
//
// 失败是**明确的失败**：返回 nil 与 *parseError，而不是一份“能读多少读多少”的树。
// 半个配置树的危险在于它看起来像全的。
func parseConfig(src string) ([]directive, error) {
	p := &parser{src: src, line: 1}
	nodes, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.src) {
		// parseBlock 只在遇到 '}' 时提前返回；顶层多出来的 '}' 是畸形配置。
		return nil, &parseError{Line: p.line, Msg: "多余的 }"}
	}
	return nodes, nil
}

type parser struct {
	src  string
	pos  int
	line int
}

// parseBlock 解析到文件结尾或配对的 '}' 为止。
func (p *parser) parseBlock(depth int) ([]directive, error) {
	if depth > maxParseDepth {
		return nil, &parseError{Line: p.line, Msg: "嵌套太深"}
	}
	var out []directive
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			if depth > 0 {
				return nil, &parseError{Line: p.line, Msg: "块没有闭合"}
			}
			return out, nil
		}
		if p.src[p.pos] == '}' {
			if depth == 0 {
				// 交给调用方判断（顶层多余）。
				return out, nil
			}
			p.pos++
			return out, nil
		}

		node, err := p.parseDirective(depth)
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
}

// parseDirective 读一条完整指令：`name args…;` 或 `name args… { … }`。
func (p *parser) parseDirective(depth int) (directive, error) {
	p.skipSpace()
	name, err := p.readWord()
	if err != nil {
		return directive{}, err
	}
	if name == "" {
		return directive{}, &parseError{Line: p.line, Msg: "指令名为空"}
	}
	node := directive{Name: name, Line: p.line}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return directive{}, &parseError{Line: p.line, Msg: "指令没有结束（缺 ; 或 {）"}
		}
		switch p.src[p.pos] {
		case ';':
			p.pos++
			return node, nil
		case '{':
			p.pos++
			children, err := p.parseBlock(depth + 1)
			if err != nil {
				return directive{}, err
			}
			node.Block = children
			node.HasBlock = true
			return node, nil
		case '}':
			return directive{}, &parseError{Line: p.line, Msg: "指令没有结束就遇到了 }"}
		}
		arg, err := p.readWord()
		if err != nil {
			return directive{}, err
		}
		if len(node.Args) >= maxDirectiveArgs {
			return directive{}, &parseError{Line: p.line, Msg: "指令参数过多"}
		}
		node.Args = append(node.Args, arg)
	}
}

// skipSpace 跳过空白与注释。
func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\r':
			p.pos++
		case '\n':
			p.line++
			p.pos++
		case '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// readWord 读一个参数：带引号的字符串，或不含分隔符的裸词。
func (p *parser) readWord() (string, error) {
	if p.pos >= len(p.src) {
		return "", nil
	}
	quote := p.src[p.pos]
	if quote == '"' || quote == '\'' {
		p.pos++
		var b strings.Builder
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c == '\\' && p.pos+1 < len(p.src) {
				p.pos++
				b.WriteByte(p.src[p.pos])
				p.pos++
				continue
			}
			if c == quote {
				p.pos++
				return b.String(), nil
			}
			if c == '\n' {
				p.line++
			}
			b.WriteByte(c)
			p.pos++
		}
		return "", &parseError{Line: p.line, Msg: "引号没有闭合"}
	}

	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '{' || c == '}' {
			break
		}
		p.pos++
	}
	word := p.src[start:p.pos]
	if !utf8.ValidString(word) {
		return "", &parseError{Line: p.line, Msg: "配置里有不是 UTF-8 的字节"}
	}
	return word, nil
}
