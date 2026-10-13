package handler

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// MarshalYAMLWithIndent marshals a YAML node with 2-space indentation
func MarshalYAMLWithIndent(node *yaml.Node) ([]byte, error) {
	// Sanitize explicit string tags before encoding to prevent !!str from appearing in output
	sanitizeExplicitStringTags(node)

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalWithIndent marshals any value to YAML with 2-space indentation
func MarshalWithIndent(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RemoveUnicodeEscapeQuotes 把序列化结果里被转义的 emoji 还原成字符本身,能去掉的双引号顺手去掉,
// 并给已知的数字字段(端口、间隔等)去引号。
//
// yaml.v3 把 U+10000 以上的字符(emoji、旗帜)一律当成「不可打印」,整条字符串改用双引号并写成
// \U0001F512。这里只动这些双引号标量,而且位置、解码后的值、所在的上下文全部来自解析器,不靠正则猜:
//   - 能原样写成 plain 标量的(放回它所在的上下文再解析一遍,值不变),去掉引号;
//   - 不能的保留引号,只把 \UXXXXXXXX 换成字符本身,其余转义(\\、\"、\t……)一个不碰;
//   - nameserver-policy 下面的一律保留引号。
//
// 最后把结果整个再解析一遍,和原文逐节点比对:只要有一处不同就退回「全部保留引号」的写法,
// 再不同就原文返回。所以这个函数改不了 YAML 的数据 —— 以前是拿正则对序列化结果做字符串手术,
// 去掉引号后 \\ 不再是转义,每写回一次反斜杠翻一倍(iluobei/miaomiaowu#126);流式列表里带
// 逗号 / 方括号的名字、规则里写的 一 这类正则也会被改坏。
func RemoveUnicodeEscapeQuotes(yamlContent string) string {
	return numericQuotesRe.ReplaceAllString(restoreEscapedUnicode(yamlContent), `$1$2: $3`)
}

// 只给预期是数字的字段去引号,不碰 name / server 这类字符串字段。
var numericQuotesRe = regexp.MustCompile(fmt.Sprintf(`(?m)^(\s*)(%s):\s+"(\d+)"`, strings.Join([]string{
	"port", "socks-port", "redir-port", "tproxy-port", "mixed-port", "dns-port",
	"interval", "timeout", "geo-update-interval", "update-interval",
	"size-limit", "size_limit",
	"health-check-interval", "health-check-timeout",
}, "|")))

// quotedToken 是原文里一个带 Unicode 转义的双引号标量。
type quotedToken struct {
	start, end int // 原文里的字节区间,含两端的引号
	value      string
	flow       bool // 在 [ ] 或 { } 里
	isKey      bool
	keepQuotes bool // nameserver-policy 下面的:只还原字符,不去引号
}

func restoreEscapedUnicode(text string) string {
	if !strings.Contains(text, `\U`) && !strings.Contains(text, `\u`) {
		return text
	}
	docs, ok := parseYAMLDocs(text)
	if !ok {
		return text
	}
	lineStart := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lineStart = append(lineStart, i+1)
		}
	}
	var tokens []quotedToken
	for _, doc := range docs {
		if !collectQuotedTokens(doc, text, lineStart, quotedToken{}, &tokens) {
			return text
		}
	}
	if len(tokens) == 0 {
		return text
	}
	build := func(unquote bool) string {
		var b strings.Builder
		last := 0
		for _, tk := range tokens {
			b.WriteString(text[last:tk.start])
			if unquote && !tk.keepQuotes && canBePlainScalar(tk.value, tk.flow, tk.isKey) {
				b.WriteString(tk.value)
			} else {
				b.WriteString(literalizeUnicodeEscapes(text[tk.start:tk.end]))
			}
			last = tk.end
		}
		b.WriteString(text[last:])
		return b.String()
	}
	for _, unquote := range []bool{true, false} {
		out := build(unquote)
		if again, ok := parseYAMLDocs(out); ok && sameYAMLDocs(docs, again) {
			return out
		}
	}
	return text
}

func parseYAMLDocs(text string) ([]*yaml.Node, bool) {
	dec := yaml.NewDecoder(strings.NewReader(text))
	var docs []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err == io.EOF {
			return docs, true
		}
		if err != nil {
			return nil, false
		}
		docs = append(docs, &n)
	}
}

// collectQuotedTokens 按文档顺序收集带 Unicode 转义的双引号标量;ctx 带着这个节点所在的上下文。
// 解析器给的位置和原文对不上(不该发生)时返回 false,调用方就什么都不改。
func collectQuotedTokens(n *yaml.Node, text string, lineStart []int, ctx quotedToken, out *[]quotedToken) bool {
	if n.Kind == yaml.ScalarNode {
		if n.Style&yaml.DoubleQuotedStyle == 0 {
			return true
		}
		start, ok := yamlByteOffset(text, lineStart, n.Line, n.Column)
		if !ok || text[start] != '"' {
			return false
		}
		end := start + 1
		for end < len(text) && text[end] != '"' {
			if text[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(text) {
			return false
		}
		end++
		if raw := text[start:end]; literalizeUnicodeEscapes(raw) != raw {
			ctx.start, ctx.end, ctx.value = start, end, n.Value
			*out = append(*out, ctx)
		}
		return true
	}
	ctx.flow = ctx.flow || n.Style&yaml.FlowStyle != 0
	for i, c := range n.Content {
		child := ctx
		child.isKey = n.Kind == yaml.MappingNode && i%2 == 0
		if n.Kind == yaml.MappingNode && i%2 == 1 && n.Content[i-1].Value == "nameserver-policy" {
			child.keepQuotes = true
		}
		if !collectQuotedTokens(c, text, lineStart, child, out) {
			return false
		}
	}
	return true
}

// yamlByteOffset 把解析器给的行列(从 1 起,列按字符数)换成字节偏移。
func yamlByteOffset(text string, lineStart []int, line, column int) (int, bool) {
	if line < 1 || line > len(lineStart) {
		return 0, false
	}
	off := lineStart[line-1]
	for c := 1; c < column; c++ {
		if off >= len(text) {
			return 0, false
		}
		_, size := utf8.DecodeRuneInString(text[off:])
		off += size
	}
	return off, off < len(text)
}

// literalizeUnicodeEscapes 处理一个双引号标量的原文(含引号):把 \UXXXXXXXX、\uXXXX 换成字符本身。
// 转义是一对一对读的,所以 \\u4e00(一个反斜杠后面跟着 u4e00 四个字)不会被误认成转义。
func literalizeUnicodeEscapes(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			b.WriteByte(raw[i])
			continue
		}
		digits := 0
		switch raw[i+1] {
		case 'U':
			digits = 8
		case 'u':
			digits = 4
		}
		if digits > 0 && i+2+digits <= len(raw) {
			if cp, err := strconv.ParseUint(raw[i+2:i+2+digits], 16, 32); err == nil && writableYAMLRune(rune(cp)) {
				b.WriteRune(rune(cp))
				i += 1 + digits
				continue
			}
		}
		b.WriteByte(raw[i])
		b.WriteByte(raw[i+1])
		i++
	}
	return b.String()
}

// writableYAMLRune 判断一个字符能不能不转义直接写进 YAML:U+10000 以上的都可以(规范允许,
// emoji、旗帜用到的标签字符都在这里);基本平面里只放行可见字符,控制符、BOM 之类保持转义。
func writableYAMLRune(r rune) bool {
	if !utf8.ValidRune(r) {
		return false
	}
	if r >= 0x10000 {
		return true
	}
	return r > 0x7E && r != 0xFEFF && unicode.IsPrint(r)
}

// canBePlainScalar 判断 v 去掉引号后,在它所在的位置上能不能被原样解析回来。直接让解析器来答:
// 不同位置规则不一样(流式列表里逗号、方括号是分隔符,块列表里不是),自己列规则总有漏的。
func canBePlainScalar(v string, flow, isKey bool) bool {
	if v == "" || strings.TrimSpace(v) != v {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7F || r == 0x85 || r == 0x2028 || r == 0x2029 || r == 0xFEFF {
			return false
		}
	}
	var probe string
	switch {
	case isKey && flow:
		probe = "{" + v + ": 0}"
	case isKey:
		probe = v + ": 0"
	case flow:
		probe = "[" + v + ", 0]"
	default:
		probe = "k: " + v
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(probe), &n); err != nil || len(n.Content) != 1 {
		return false
	}
	root := n.Content[0]
	wantKind := yaml.MappingNode
	at := 1
	if isKey {
		at = 0
	} else if flow {
		wantKind, at = yaml.SequenceNode, 0
	}
	if root.Kind != wantKind || len(root.Content) != 2 {
		return false
	}
	got := root.Content[at]
	return got.Kind == yaml.ScalarNode && got.Tag == "!!str" && got.Value == v
}

// sameYAMLDocs 比较两次解析的结果:结构、类型、值都一样(写法不同不算)。
func sameYAMLDocs(a, b []*yaml.Node) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameYAMLNode(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameYAMLNode(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || a.Tag != b.Tag || a.Value != b.Value || a.Anchor != b.Anchor || len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		if !sameYAMLNode(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// yaml.Unmarshal 方法会丢失数字前面的0, 先转为yaml.Node, 再转为map
func yamlNodeToMap(node *yaml.Node) (map[string]interface{}, error) {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) > 0 {
			return yamlNodeToMap(node.Content[0])
		}
		return nil, nil
	}

	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node, got %v", node.Kind)
	}

	result := make(map[string]interface{})
	for i := 0; i < len(node.Content); i += 2 {
		if i+1 >= len(node.Content) {
			break
		}
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]

		key := keyNode.Value
		value, err := yamlNodeToValue(valueNode)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

// 转换为对应的 Go 类型值, 0开头的值保留其原始字符串格式
func yamlNodeToValue(node *yaml.Node) (interface{}, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		// 对于带引号的字符串，保持字符串格式
		if node.Tag == "!!str" || node.Style == yaml.DoubleQuotedStyle || node.Style == yaml.SingleQuotedStyle {
			return node.Value, nil
		}
		if looksLikeNumericStringWithLeadingZero(node.Value) {
			return node.Value, nil
		}
		// 对于其他标量，使用标准解析
		var value interface{}
		if err := node.Decode(&value); err != nil {
			return node.Value, nil // 解码失败时返回原始字符串
		}
		return value, nil

	case yaml.SequenceNode:
		var result []interface{}
		for _, child := range node.Content {
			value, err := yamlNodeToValue(child)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil

	case yaml.MappingNode:
		return yamlNodeToMap(node)

	case yaml.AliasNode:
		return yamlNodeToValue(node.Alias)

	default:
		return nil, nil
	}
}

// 判断yaml节点的value是否是0开头的数字
func looksLikeNumericStringWithLeadingZero(s string) bool {
	if len(s) < 2 {
		return false
	}
	// 以 0 开头且后续都是数字的字符串（如 "045678"）
	if s[0] == '0' && len(s) > 1 && s[1] >= '0' && s[1] <= '9' {
		for _, c := range s[1:] {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// sanitizeExplicitStringTags removes explicit !!str tags from scalar nodes by clearing
// the TaggedStyle bit. This prevents the YAML encoder from emitting literal !!str tags
// in the output, which can cause parsing errors in some YAML clients.
//
// The function recursively walks the entire node tree and normalizes any scalar nodes
// that have explicit string tags (!!str or tag:yaml.org,2002:str). After clearing the
// TaggedStyle bit, the encoder will use implicit typing and add quotes automatically
// when needed, maintaining semantic correctness while improving compatibility.
func sanitizeExplicitStringTags(node *yaml.Node) {
	if node == nil {
		return
	}

	// Clear TaggedStyle for scalar nodes with explicit string tags
	if node.Kind == yaml.ScalarNode && isExplicitStringTag(node.Tag) {
		node.Style &^= yaml.TaggedStyle
	}

	// Recursively process all child nodes
	for _, child := range node.Content {
		sanitizeExplicitStringTags(child)
	}
}

// isExplicitStringTag checks if the given YAML tag represents an explicit string type
func isExplicitStringTag(tag string) bool {
	return tag == "!!str" || tag == "tag:yaml.org,2002:str"
}
