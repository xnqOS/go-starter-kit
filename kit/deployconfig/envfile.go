package deployconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// Entry is one KEY=VALUE line, in file order.
type Entry struct {
	Name, Value string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// Parse reads a Docker Compose env_file exactly as Compose does (checked
// against `docker compose config`):
//
//   - lines end with LF or CRLF; inside quotes CR and LF are kept as they are
//   - unquoted: "$$" is a literal "$", only " #" (space, hash) starts a
//     comment ("\t#" is part of the value), surrounding blanks are trimmed,
//     no backslash escapes
//   - 'single quoted': literal and may span lines, except that \' does not
//     close the quote; Parse reports a backslash before the closing quote
//   - "double quoted": \\ \" \$ \n \r \t escapes and "$$", may span lines
//
// Anything whose meaning Parse cannot know or reproduce, such as $VAR
// (Compose substitutes it from the shell that runs the deployment), an
// unterminated quote or a line without "=", is reported in problems, with
// its line number. A file with problems must not be rewritten: the result
// would silently differ from what Compose reads today.
func Parse(content string) (entries []Entry, problems []string) {
	p := &envParser{src: content, line: 1}
	for {
		p.skip(" \t\r\n")
		if p.done() {
			return p.entries, p.problems
		}
		if p.peek() == '#' {
			p.restOfLine()
			continue
		}
		if !p.entry() {
			return p.entries, p.problems // unterminated quote: the rest cannot be trusted
		}
	}
}

type envParser struct {
	src      string
	i, line  int
	entries  []Entry
	problems []string
}

func (p *envParser) done() bool { return p.i >= len(p.src) }
func (p *envParser) peek() byte { return p.src[p.i] }

func (p *envParser) skip(chars string) {
	for !p.done() && strings.IndexByte(chars, p.peek()) >= 0 {
		if p.peek() == '\n' {
			p.line++
		}
		p.i++
	}
}

// restOfLine consumes up to (not including) the newline and returns it
// without the CR of a CRLF line ending.
func (p *envParser) restOfLine() string {
	start := p.i
	for !p.done() && p.peek() != '\n' {
		p.i++
	}
	return strings.TrimSuffix(p.src[start:p.i], "\r")
}

func (p *envParser) fail(line int, format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf("第 %d 行：", line)+fmt.Sprintf(format, args...))
}

// entry parses one KEY=VALUE; false means the rest of the file is unusable.
func (p *envParser) entry() bool {
	line := p.line
	head := p.src[p.i:]
	eq := strings.IndexAny(head, "=\n")
	if eq < 0 || head[eq] == '\n' {
		p.fail(line, "「%s」没有 =（Compose 会从运行部署命令的 shell 里取这个变量）", strings.TrimSpace(p.restOfLine()))
		return true
	}
	name := strings.TrimSpace(strings.TrimPrefix(head[:eq], "export "))
	p.i += eq + 1
	if !envName.MatchString(name) {
		p.fail(line, "「%s」不是合法的变量名", name)
		p.restOfLine()
		return true
	}
	p.skip(" \t")
	var value string
	switch {
	case !p.done() && p.peek() == '\'':
		end := strings.IndexByte(p.src[p.i+1:], '\'')
		if end < 0 {
			p.fail(line, "%s 的单引号没有闭合", name)
			return false
		}
		value = p.src[p.i+1 : p.i+1+end]
		if strings.HasSuffix(value, "\\") {
			// Compose reads \' as an escaped quote, so the value goes on past
			// this quote; too easy to get wrong to reproduce here.
			p.fail(line, "%s 的单引号里有 \\'（或值以 \\ 结尾），Compose 不把它当结束引号；改成双引号，反斜杠写成 \\\\", name)
			return false
		}
		p.line += strings.Count(value, "\n")
		p.i += end + 2
	case !p.done() && p.peek() == '"':
		var ok, closed bool
		if value, ok, closed = p.doubleQuoted(name, line); !closed {
			return false
		} else if !ok {
			p.restOfLine()
			return true
		}
	default:
		raw := p.restOfLine()
		if i := strings.Index(raw, " #"); i >= 0 {
			raw = raw[:i]
		}
		var ok bool
		if value, ok = dollars(strings.TrimRight(raw, " \t")); !ok {
			p.fail(line, "%s 的值里有 $，Compose 会把它当成变量替换；要表示字面的 $，改成单引号，例如 %s='…'", name, name)
			return true
		}
		p.entries = append(p.entries, Entry{name, value})
		return true
	}
	if rest := strings.TrimSpace(p.restOfLine()); rest != "" && !strings.HasPrefix(rest, "#") {
		p.fail(p.line, "%s 的引号后面还有内容「%s」", name, rest)
		return true
	}
	p.entries = append(p.entries, Entry{name, value})
	return true
}

func (p *envParser) doubleQuoted(name string, line int) (value string, ok, closed bool) {
	var b strings.Builder
	ok = true
	for p.i++; !p.done(); p.i++ {
		c := p.peek()
		switch {
		case c == '"':
			p.i++
			return b.String(), ok, true
		case c == '\\' && p.i+1 < len(p.src):
			p.i++
			switch e := p.peek(); e {
			case '\\', '"', '$':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				if ok {
					p.fail(p.line, "%s 的值里有不支持的转义 \\%c；改成单引号", name, e)
				}
				ok = false
			}
		case c == '$':
			if p.i+1 < len(p.src) && p.src[p.i+1] == '$' {
				b.WriteByte('$')
				p.i++
				continue
			}
			if ok {
				p.fail(p.line, "%s 的值里有 $，Compose 会把它当成变量替换；要表示字面的 $，写成 \\$ 或改成单引号", name)
			}
			ok = false
		default:
			if c == '\n' {
				p.line++
			}
			b.WriteByte(c)
		}
	}
	p.fail(line, "%s 的双引号没有闭合", name)
	return "", false, false
}

// dollars resolves "$$" in an unquoted value; any other "$" is a variable
// Compose would substitute, so the value cannot be known here.
func dollars(raw string) (string, bool) {
	if !strings.Contains(raw, "$") {
		return raw, true
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '$' {
			b.WriteByte(raw[i])
			continue
		}
		if i+1 < len(raw) && raw[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		return "", false
	}
	return b.String(), true
}

// quote writes value so that Compose reads back exactly value: bare when
// nothing in it is special, 'single quoted' (literal) when it has no
// apostrophe or backslash (a backslash before the closing quote would escape
// it), otherwise "double quoted" with \\ \" \$ \n \r \t escaped.
func quote(value string) string {
	if value == "" || (!strings.ContainsAny(value, "'\"#\\$\n\r\t") && strings.TrimSpace(value) == value) {
		return value
	}
	if !strings.ContainsAny(value, "'\\\r") {
		return "'" + value + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value) + `"`
}
