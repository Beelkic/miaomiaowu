package handler

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 一次「读进来 → 写回去」:解析成节点,按写文件的方式序列化,再过 RemoveUnicodeEscapeQuotes。
func yamlWriteBack(t *testing.T, src string) string {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, src)
	}
	out, err := MarshalYAMLWithIndent(&n)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return RemoveUnicodeEscapeQuotes(string(out))
}

func yamlData(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, text)
	}
	return v
}

// iluobei/miaomiaowu#126:规则里同时有反斜杠和 emoji 时,每写回一次反斜杠翻一倍。
func TestRemoveUnicodeEscapeQuotesKeepsBackslashes(t *testing.T) {
	text := "rules:\n  - PROCESS-NAME-REGEX,^(foo)\\.exe$,DIRECT\n  - PROCESS-NAME-REGEX,^(foo)\\.exe$,🔒 国内服务\n"
	want := yamlData(t, text)
	for i := 1; i <= 5; i++ {
		text = yamlWriteBack(t, text)
		if got := yamlData(t, text); !reflect.DeepEqual(got, want) {
			t.Fatalf("第 %d 次写回后数据变了:\n%s", i, text)
		}
	}
	// 不光数据要对,写法也要保持:emoji 还原成字符、不留引号
	if !strings.Contains(text, "  - PROCESS-NAME-REGEX,^(foo)\\.exe$,🔒 国内服务\n") {
		t.Fatalf("带 emoji 的规则应当原样写成 plain 标量:\n%s", text)
	}
}

func TestRemoveUnicodeEscapeQuotesPreservesData(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// 写回后的文本里应当出现的片段(空 = 只检查数据不变)
		contains string
	}{
		{"普通的 emoji 名字去掉引号", "proxies:\n  - name: 🇭🇰 香港 01\n    type: ss\n", "name: 🇭🇰 香港 01"},
		{"流式列表里名字带方括号:保留引号", "proxy-groups:\n  - name: 🚀 节点选择\n    proxies: [\"🇭🇰 HK [IEPL]\", DIRECT]\n", `["🇭🇰 HK [IEPL]", DIRECT]`},
		{"流式列表里名字带逗号:保留引号", "proxy-groups:\n  - name: 🚀 节点选择\n    proxies: [\"🇭🇰 香港, 01\", DIRECT]\n", `["🇭🇰 香港, 01", DIRECT]`},
		{"流式列表里普通 emoji 名字去掉引号", "proxies: [香港, \"🇭🇰 香港\", DIRECT]\n", "[香港, 🇭🇰 香港, DIRECT]"},
		{"规则里写的 \\u 正则不动", "rules:\n  - DOMAIN-REGEX,^[\\u4e00-\\u9fa5]+\\.cn$,DIRECT\n", `^[\u4e00-\u9fa5]+\.cn$`},
		{"\\u 正则加 emoji", "rules:\n  - DOMAIN-REGEX,^[\\u4e00-\\u9fa5]+$,🔒 国内服务\n", `^[\u4e00-\u9fa5]+$,🔒 国内服务`},
		{"值里有冒号加空格:保留引号", "a: \"🔒 x: y\"\n", `"🔒 x: y"`},
		{"值里有空格加井号:保留引号", "a: \"🔒 x #y\"\n", `"🔒 x #y"`},
		{"值里有双引号", "a: \"🔒 say \\\"hi\\\"\"\n", ""},
		{"值里有制表符:保留引号和转义", "a: \"🔒\\tx\"\n", `"🔒\tx"`},
		{"以特殊字符开头:保留引号", "a: \"- 🔒\"\nb: \"[🔒]\"\nc: \"*🔒\"\n", `"- 🔒"`},
		{"首尾有空格:保留引号", "a: \" 🔒 \"\n", `" 🔒 "`},
		{"emoji 做键", "🔒 国内服务: 1\n\"🔒: x\": 2\n", "🔒 国内服务: 1"},
		{"英格兰旗帜(标签字符)", "a: 🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F 英格兰\n", ""},
		{"块标量里的内容不动", "script: |\n  echo \"\\U0001F512\"\nname: 🔒\n", "  echo \"\\U0001F512\"\n"},
		{"注释里的引号不干扰", "a: 🔒 x # 说明 \"引号\"\nb: \"🔒 [y]\" # \"再来\"\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := yamlData(t, c.src)
			text := c.src
			for i := 1; i <= 3; i++ {
				text = yamlWriteBack(t, text)
				if got := yamlData(t, text); !reflect.DeepEqual(got, want) {
					t.Fatalf("第 %d 次写回后数据变了:\n%s", i, text)
				}
			}
			if strings.Contains(text, `\U0001F`) && !strings.Contains(c.src, `\U0001F`) {
				t.Fatalf("emoji 没有还原成字符:\n%s", text)
			}
			if c.contains != "" && !strings.Contains(text, c.contains) {
				t.Fatalf("写回后应当包含 %q:\n%s", c.contains, text)
			}
		})
	}
}

// nameserver-policy 下面的保留引号,只把 emoji 还原成字符。
func TestRemoveUnicodeEscapeQuotesNameserverPolicyKeepsQuotes(t *testing.T) {
	src := "dns:\n  nameserver-policy:\n    geosite:cn: https://doh.pub/dns-query#🔒 国内服务\n  fallback:\n    - https://dns.google/dns-query#🚀 节点选择\n"
	text := yamlWriteBack(t, src)
	if !reflect.DeepEqual(yamlData(t, text), yamlData(t, src)) {
		t.Fatalf("数据变了:\n%s", text)
	}
	if !strings.Contains(text, `geosite:cn: "https://doh.pub/dns-query#🔒 国内服务"`) {
		t.Fatalf("nameserver-policy 下面应当保留引号:\n%s", text)
	}
	if !strings.Contains(text, "- https://dns.google/dns-query#🚀 节点选择") {
		t.Fatalf("其它地方照常去引号:\n%s", text)
	}
}

func TestRemoveUnicodeEscapeQuotesNumericFields(t *testing.T) {
	got := RemoveUnicodeEscapeQuotes("proxies:\n  - name: \"\\U0001F512 a\"\n    port: \"443\"\n    server: \"1234\"\n")
	if !strings.Contains(got, "port: 443\n") || !strings.Contains(got, `server: "1234"`) || !strings.Contains(got, "name: 🔒 a\n") {
		t.Fatalf("数字字段去引号、字符串字段保留:\n%s", got)
	}
}

// 一段文本里有多个文档时,每个文档都处理。
func TestRemoveUnicodeEscapeQuotesMultipleDocuments(t *testing.T) {
	got := RemoveUnicodeEscapeQuotes("a: \"\\U0001F512 一\"\n---\nb: \"\\U0001F512 二\"\n")
	if got != "a: 🔒 一\n---\nb: 🔒 二\n" {
		t.Fatalf("两个文档都应当还原:\n%s", got)
	}
}

// 不是合法 YAML 的文本原样返回,不硬改。
func TestRemoveUnicodeEscapeQuotesInvalidYAMLUntouched(t *testing.T) {
	src := "a: \"\\U0001F512\n  b: [c\n"
	if got := RemoveUnicodeEscapeQuotes(src); got != src {
		t.Fatalf("不合法的 YAML 应当原样返回:\n%s", got)
	}
}

// 随机拼字符串(emoji、反斜杠、引号、逗号、方括号、冒号、井号、\u 字面量……),放进块列表、流式列表、
// 映射的键和值里,连续写回三轮:数据不能变,文本不能越写越长。
func TestRemoveUnicodeEscapeQuotesRandomRoundTrip(t *testing.T) {
	pieces := []string{"🔒", "🇭🇰", "国内", "a", "B", "0", " ", `\`, `\.`, `"`, "'", ",", "[", "]", "{", "}", ": ", ":", " #", "#", "-", "- ", "?", "!", "&", "*", "|", ">", "%", "@", `\u4e00`, `\U0001F512`, "\t", "true", "~"}
	rng := rand.New(rand.NewSource(7))
	str := func() string {
		var b strings.Builder
		for i, n := 0, 1+rng.Intn(6); i < n; i++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		return b.String()
	}
	scalar := func() *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: str()} }
	key := func(name string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name} }
	for round := 0; round < 2000; round++ {
		doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			key("rules"), {Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar(), scalar()}},
			key("proxies"), {Kind: yaml.SequenceNode, Style: yaml.FlowStyle, Content: []*yaml.Node{scalar(), scalar()}},
			key("map"), {Kind: yaml.MappingNode, Content: []*yaml.Node{scalar(), scalar()}},
			key("flow"), {Kind: yaml.MappingNode, Style: yaml.FlowStyle, Content: []*yaml.Node{scalar(), scalar()}},
		}}
		raw, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		want := yamlData(t, string(raw))
		text := string(raw)
		firstLen := 0
		for i := 1; i <= 3; i++ {
			text = yamlWriteBack(t, text)
			if got := yamlData(t, text); !reflect.DeepEqual(got, want) {
				t.Fatalf("第 %d 组第 %d 次写回后数据变了\n原文:\n%s\n写回:\n%s", round, i, raw, text)
			}
			if i == 1 {
				firstLen = len(text)
			} else if len(text) != firstLen {
				t.Fatalf("第 %d 组第 %d 次写回后文本长度变了(%d → %d):\n%s", round, i, firstLen, len(text), text)
			}
		}
	}
}
