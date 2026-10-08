package smoke

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"
)

// Error is a parse or validation error of a smoke file. Line is 1-based (0
// when the error is about the file as a whole); Key is the dotted path of
// the offending key ("wait.timeout", "checks[2].status"), empty when the
// error is not about a key. Path, when set (Load sets it), names the
// file in place of the bare FileName.
type Error struct {
	Line int
	Key  string
	Msg  string
	Path string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(cmp.Or(e.Path, FileName))
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d", e.Line)
	}
	if e.Key != "" {
		b.WriteString(": " + e.Key)
	}
	b.WriteString(": " + e.Msg)
	return b.String()
}

func errAt(line int, key, format string, args ...any) *Error {
	return &Error{Line: line, Key: key, Msg: fmt.Sprintf(format, args...)}
}

type nodeKind int

const (
	scalarNode nodeKind = iota
	mapNode
	listNode
)

func (k nodeKind) String() string {
	switch k {
	case mapNode:
		return "map"
	case listNode:
		return "list"
	}
	return "scalar"
}

// node is one value of the parsed YAML subset.
type node struct {
	kind    nodeKind
	line    int
	value   string     // scalarNode
	entries []mapEntry // mapNode, in file order
	items   []*node    // listNode
}

type mapEntry struct {
	key   string
	line  int
	value *node
}

// srcLine is one logical (non-blank, comment-stripped) line of the file.
type srcLine struct {
	num    int
	indent int
	text   string // without indentation, comment and trailing spaces
}

var keyRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*):(?:\s|$)`)

// splitKey splits "key: rest" into key and the trimmed rest; ok is false
// when s doesn't start with a key.
func splitKey(s string) (key, rest string, ok bool) {
	m := keyRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(s[len(m[1])+1:]), true
}

// isListItem reports whether a line's text is a block-list item.
func isListItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

// parseYAML parses data, which must be in the subset documented in the
// package doc, into a tree whose root is a map (empty for a file with no
// content).
func parseYAML(data []byte) (*node, error) {
	lines, err := splitLines(string(data))
	if err != nil {
		return nil, err
	}
	p := &parser{lines: lines}
	root := &node{kind: mapNode, line: 1}
	if len(lines) == 0 {
		return root, nil
	}
	if first := lines[0]; first.indent != 0 {
		return nil, errAt(first.num, "", "unexpected indentation: the top level must start at column 1")
	}
	root, err = p.parseMap(0, "", true)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if isListItem(l.text) {
			return nil, errAt(l.num, "", "unexpected list item: the top level must be a map of keys")
		}
		return nil, errAt(l.num, "", "unexpected indentation")
	}
	return root, nil
}

// splitLines turns the file into logical lines: comments, blank lines and
// trailing spaces dropped, tabs in indentation and document markers
// rejected.
func splitLines(src string) ([]srcLine, error) {
	src = strings.TrimPrefix(src, "\ufeff")
	var out []srcLine
	for i, raw := range strings.Split(src, "\n") {
		num := i + 1
		raw = strings.TrimSuffix(raw, "\r")
		body := strings.TrimLeft(raw, " \t")
		lead := raw[:len(raw)-len(body)]
		text := strings.TrimRight(stripComment(body), " \t")
		if strings.Contains(lead, "\t") && text != "" {
			key, _, _ := splitKey(text)
			return nil, errAt(num, key, "tab used for indentation (indent with spaces)")
		}
		if text == "" {
			continue
		}
		if len(lead) == 0 && (text == "---" || text == "..." || strings.HasPrefix(text, "--- ") || strings.HasPrefix(text, "%")) {
			return nil, errAt(num, "", "document markers and directives (%q) are not supported: a smoke file is a single document", text)
		}
		out = append(out, srcLine{num: num, indent: len(lead), text: text})
	}
	return out, nil
}

// stripComment cuts a "#" comment off s: a "#" at the start or after a
// space, outside quotes. A quote opens only at the start of a token.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"':
			if c == '\\' {
				i++
			} else if c == '"' {
				quote = 0
			}
		case quote == '\'':
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		case (c == '"' || c == '\'') && (i == 0 || strings.IndexByte(" \t{,[:", s[i-1]) >= 0):
			quote = c
		}
	}
	return s
}

type parser struct {
	lines []srcLine
	pos   int
}

func (p *parser) peek() (srcLine, bool) {
	if p.pos >= len(p.lines) {
		return srcLine{}, false
	}
	return p.lines[p.pos], true
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// parseMap reads the "key: value" lines at exactly indent. nested says
// whether a key with an empty value may own a block (map or list) below it;
// it is true only for the top-level map, which is what limits the nesting.
func (p *parser) parseMap(indent int, path string, nested bool) (*node, error) {
	first, _ := p.peek()
	m := &node{kind: mapNode, line: first.num}
	seen := map[string]bool{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return m, nil
		}
		if l.indent > indent {
			return nil, errAt(l.num, "", "unexpected indentation")
		}
		if isListItem(l.text) {
			return m, nil // the caller decides whether a list may be here
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, errAt(l.num, path, "expected \"key: value\", got %q", l.text)
		}
		full := joinKey(path, key)
		if seen[key] {
			return nil, errAt(l.num, full, "duplicate key")
		}
		seen[key] = true
		p.pos++
		var val *node
		var err error
		if rest != "" {
			val, err = parseInline(rest, l.num, full)
		} else {
			val, err = p.parseBlockValue(l, indent, full, nested)
		}
		if err != nil {
			return nil, err
		}
		m.entries = append(m.entries, mapEntry{key: key, line: l.num, value: val})
	}
}

// parseBlockValue parses what follows "key:" with nothing after it: a block
// map or list on the following lines, or an empty scalar.
func (p *parser) parseBlockValue(owner srcLine, indent int, path string, nested bool) (*node, error) {
	next, ok := p.peek()
	isList := ok && isListItem(next.text) && next.indent >= indent
	if !ok || (!isList && next.indent <= indent) {
		return &node{kind: scalarNode, line: owner.num}, nil
	}
	if !nested {
		return nil, errAt(next.num, path, "nested too deep: only one level of nested maps and lists is supported")
	}
	if isList {
		return p.parseList(next.indent, path)
	}
	return p.parseMap(next.indent, path, false)
}

// parseList reads the "- item" lines at exactly indent.
func (p *parser) parseList(indent int, path string) (*node, error) {
	first, _ := p.peek()
	list := &node{kind: listNode, line: first.num}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return list, nil
		}
		if l.indent > indent {
			return nil, errAt(l.num, path, "unexpected indentation")
		}
		if !isListItem(l.text) {
			return list, nil
		}
		itemPath := fmt.Sprintf("%s[%d]", path, len(list.items)+1)
		if l.text == "-" {
			return nil, errAt(l.num, itemPath, "empty list item")
		}
		after := l.text[1:]
		rest := strings.TrimLeft(after, " ")
		var item *node
		var err error
		if _, _, isKey := splitKey(rest); isKey {
			// A block map starting on the dash line: rewrite the line as
			// its first entry and read the continuation lines with it.
			itemIndent := l.indent + 1 + len(after) - len(rest)
			p.lines[p.pos] = srcLine{num: l.num, indent: itemIndent, text: rest}
			item, err = p.parseMap(itemIndent, itemPath, false)
			if err == nil {
				if n, ok := p.peek(); ok && n.indent > indent && n.indent != itemIndent {
					err = errAt(n.num, itemPath, "unexpected indentation")
				} else if ok && n.indent == itemIndent && isListItem(n.text) {
					err = errAt(n.num, itemPath, "nested too deep: a list inside a list item is not supported")
				}
			}
		} else {
			p.pos++
			item, err = parseInline(rest, l.num, itemPath)
			if n, ok := p.peek(); err == nil && ok && n.indent > indent {
				err = errAt(n.num, itemPath, "unexpected indentation (multi-line values are not supported)")
			}
		}
		if err != nil {
			return nil, err
		}
		list.items = append(list.items, item)
	}
}

// parseInline parses a value written on the key's (or dash's) line: a flow
// map or a scalar.
func parseInline(s string, line int, path string) (*node, error) {
	if strings.HasPrefix(s, "{") {
		return parseFlowMap(s, line, path)
	}
	v, err := parseScalar(s, line, path, false)
	if err != nil {
		return nil, err
	}
	return &node{kind: scalarNode, line: line, value: v}, nil
}

// parseScalar parses a plain, single- or double-quoted scalar. inFlow
// forbids flow indicators inside plain scalars.
func parseScalar(s string, line int, path string, inFlow bool) (string, error) {
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '"':
		return parseDoubleQuoted(s, line, path)
	case '\'':
		return parseSingleQuoted(s, line, path)
	case '&', '*':
		return "", errAt(line, path, "anchors and aliases are not supported")
	case '!':
		return "", errAt(line, path, "tags are not supported")
	case '|', '>':
		return "", errAt(line, path, "block scalars (| and >) are not supported")
	case '[':
		return "", errAt(line, path, "flow sequences ([...]) are not supported: use a block list")
	case '{':
		return "", errAt(line, path, "a map where a scalar is expected")
	case '@', '`':
		return "", errAt(line, path, "a plain value cannot start with %q", s[0])
	}
	if inFlow && strings.ContainsAny(s, "{}[]") {
		return "", errAt(line, path, "flow indicators ({ } [ ]) inside a flow map value must be quoted")
	}
	return s, nil
}

func parseDoubleQuoted(s string, line int, path string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			if rest := strings.TrimSpace(s[i+1:]); rest != "" {
				return "", errAt(line, path, "unexpected text %q after a quoted value", rest)
			}
			return b.String(), nil
		case '\\':
			if i+1 >= len(s) {
				return "", errAt(line, path, "unterminated double-quoted value")
			}
			i++
			switch s[i] {
			case '"', '\\':
				b.WriteByte(s[i])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return "", errAt(line, path, "unsupported escape \\%c in a double-quoted value", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errAt(line, path, "unterminated double-quoted value")
}

func parseSingleQuoted(s string, line int, path string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		if rest := strings.TrimSpace(s[i+1:]); rest != "" {
			return "", errAt(line, path, "unexpected text %q after a quoted value", rest)
		}
		return b.String(), nil
	}
	return "", errAt(line, path, "unterminated single-quoted value")
}

// parseFlowMap parses a one-line "{k: v, ...}" map with scalar values.
func parseFlowMap(s string, line int, path string) (*node, error) {
	if !strings.HasSuffix(s, "}") {
		return nil, errAt(line, path, "a flow map must close with } on the same line")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	m := &node{kind: mapNode, line: line}
	if inner == "" {
		return m, nil
	}
	parts, err := splitFlow(inner, line, path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errAt(line, path, "empty entry in a flow map")
		}
		key, rest, ok := splitKey(part)
		if !ok {
			return nil, errAt(line, path, "expected \"key: value\" in a flow map, got %q", part)
		}
		full := joinKey(path, key)
		if seen[key] {
			return nil, errAt(line, full, "duplicate key")
		}
		seen[key] = true
		v, err := parseScalar(rest, line, full, true)
		if err != nil {
			return nil, err
		}
		m.entries = append(m.entries, mapEntry{key: key, line: line, value: &node{kind: scalarNode, line: line, value: v}})
	}
	return m, nil
}

// splitFlow splits the inside of a flow map on commas outside quotes.
func splitFlow(s string, line int, path string) ([]string, error) {
	var parts []string
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"':
			if c == '\\' {
				i++
			} else if c == '"' {
				quote = 0
			}
		case quote == '\'':
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
		case c == ',':
			parts = append(parts, s[start:i])
			start = i + 1
		case (c == '"' || c == '\'') && (i == 0 || strings.IndexByte(" ,:", s[i-1]) >= 0):
			quote = c
		}
	}
	if quote != 0 {
		return nil, errAt(line, path, "unterminated quoted value in a flow map")
	}
	return append(parts, s[start:]), nil
}
