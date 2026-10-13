package handler

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

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

// RemoveUnicodeEscapeQuotes removes quotes from strings that contain Unicode escape sequences
// and converts the escape sequences back to actual Unicode characters (like emoji).
// For known numeric fields (port, interval, etc.), removes quotes to ensure proper numeric type.
// Note: nameserver-policy values are preserved with quotes to maintain DNS policy format.
func RemoveUnicodeEscapeQuotes(yamlContent string) string {
	// 使用占位符保留原始nameserver-policy配置
	var nameserverPolicyBlock string
	nameserverPolicyRe := regexp.MustCompile(`(?ms)^(nameserver-policy:\s*\n)((?:[ \t]+.+\n?)*)`)
	yamlContent = nameserverPolicyRe.ReplaceAllStringFunc(yamlContent, func(match string) string {
		nameserverPolicyBlock = match
		return "___NAMESERVER_POLICY_PLACEHOLDER___\n"
	})

	// Step 1: Remove quotes from strings that contain Unicode escape sequences
	// Pattern: "...\U000XXXXX..." or "...\uXXXX..."
	// But keep quotes if the unquoted string would start with YAML special characters
	quotedUnicodeRe := regexp.MustCompile(`"([^"]*\\[Uu][0-9A-Fa-f]{4,8}[^"]*)"`)
	result := quotedUnicodeRe.ReplaceAllStringFunc(yamlContent, func(match string) string {
		// Get the content without quotes
		content := strings.Trim(match, `"`)

		// 去引号前必须按 YAML 双引号规则完整解码：\\、\"、\t 等转义在去掉引号后会变成字面量，
		// 只还原 \U 而直接去引号，会让 \ 每次写回都翻倍（\.exe -> \\.exe -> \\\\.exe ...）
		var decoded string
		if err := yaml.Unmarshal([]byte(match), &decoded); err != nil || !isSafePlainScalar(decoded) {
			// 解码失败或不能作为 plain 标量：保留双引号，仅还原 Unicode 转义
			return `"` + convertUnicodeEscapes(content) + `"`
		}

		// Safe to remove quotes
		return decoded
	})

	// Step 2: Convert ALL Unicode escapes back to actual characters (quoted or not)
	// \U0001F4B0 -> 💰, \u4E2D -> 中, \U0001F1ED\U0001F1F0 -> 🇭🇰
	result = convertUnicodeEscapes(result)

	// Step 3: Remove quotes from numeric values for known numeric fields
	// Only unquote fields that are expected to be numbers to avoid changing string-typed fields like name/server.
	numericFields := []string{
		"port", "socks-port", "redir-port", "tproxy-port", "mixed-port", "dns-port",
		"interval", "timeout", "geo-update-interval", "update-interval",
		"size-limit", "size_limit",
		"health-check-interval", "health-check-timeout",
	}
	numericFieldsPattern := fmt.Sprintf(`(?m)^(\s*)(%s):\s+"(\d+)"`, strings.Join(numericFields, "|"))
	numericQuotesRe := regexp.MustCompile(numericFieldsPattern)
	result = numericQuotesRe.ReplaceAllString(result, `$1$2: $3`)

	// 回复nameserver-policy配置
	if nameserverPolicyBlock != "" {
		result = strings.Replace(result, "___NAMESERVER_POLICY_PLACEHOLDER___\n", nameserverPolicyBlock, 1)
	}

	return result
}

// isSafePlainScalar 判断字符串去掉引号作为 plain 标量写出后，能否被原样解析回同一个字符串
func isSafePlainScalar(s string) bool {
	if s == "" || strings.ContainsAny(s, "\n\r\t") {
		return false
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte("k: "+s), &parsed); err != nil {
		return false
	}
	v, ok := parsed["k"].(string)
	return ok && v == s
}

// convertUnicodeEscapes converts Unicode escape sequences to actual characters
func convertUnicodeEscapes(s string) string {
	escapeRe := regexp.MustCompile(`\\U([0-9A-Fa-f]{8})|\\u([0-9A-Fa-f]{4})`)
	return escapeRe.ReplaceAllStringFunc(s, func(escapeSeq string) string {
		var codepoint int
		if strings.HasPrefix(escapeSeq, `\U`) {
			fmt.Sscanf(escapeSeq, `\U%X`, &codepoint)
		} else {
			fmt.Sscanf(escapeSeq, `\u%X`, &codepoint)
		}
		return string(rune(codepoint))
	})
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
