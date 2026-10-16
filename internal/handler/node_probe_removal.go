package handler

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"miaomiaowu/internal/storage"
)

// 节点探测 → 自动从订阅里移除不可用节点。
//
// 探测判定不可用(连续 nodeProbeFailK 次失败)并持续满设定分钟数后,把节点标记为
// Removed;恢复后自动放回。**不动 enabled 字段**:用户手动禁用和探测自动摘掉是两回事,
// 混在一起会出现"用户手动禁用的节点因为探测恢复被自动打开"。
//
// 订阅侧两条路分别处理:
//   - 模板订阅 / 聚合订阅每次拉取都从节点表动态生成 —— 直接在节点列表里过滤。
//   - 静态订阅文件节点写死在文件里 —— 下发时从 YAML 里剔,文件本身不改。
//
// 两条路都有同一条保险:不会把一个代理组剔空(mihomo 对空组直接报错起不来),
// 也不会把全部节点剔光 —— 全挂了等于没信息,照常下发总比给客户端一份空配置强。

const (
	// settingNodeProbeRemoveEnabled 「不可用节点自动从订阅移除」开关。默认关。
	settingNodeProbeRemoveEnabled = "node_probe_remove_enabled"
	// settingNodeProbeRemoveMinutes 掉线满 N 分钟才移除;0 = 判定不可用即移除。
	settingNodeProbeRemoveMinutes = "node_probe_remove_minutes"
)

// nodeProbeRemoveConfig 一轮探测开始时读一次,整轮共用。
type nodeProbeRemoveConfig struct {
	Enabled bool
	Minutes int
}

func loadNodeProbeRemoveConfig(ctx context.Context, repo *storage.TrafficRepository) nodeProbeRemoveConfig {
	var cfg nodeProbeRemoveConfig
	if repo == nil {
		return cfg
	}
	v, _ := repo.GetSystemSetting(ctx, settingNodeProbeRemoveEnabled)
	cfg.Enabled = v == "1"
	raw, _ := repo.GetSystemSetting(ctx, settingNodeProbeRemoveMinutes)
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
		cfg.Minutes = n
	}
	return cfg
}

// nodeProbeStoreGlobal 订阅处理器拿到探测结果的入口。探测存储在 main 里创建,
// 订阅处理器的构造函数很多、改签名要牵一串,所以和 GetNotifier 一样走包级访问。
var nodeProbeStoreGlobal atomic.Pointer[NodeProbeStore]

// SetNodeProbeStore 注册探测存储,供订阅生成时查询被摘掉的节点。
func SetNodeProbeStore(s *NodeProbeStore) {
	nodeProbeStoreGlobal.Store(s)
}

// probeRemovedNodes 当前被摘掉的节点(ID → 名字);功能未启用或没有时为空。
func probeRemovedNodes() map[int64]string {
	s := nodeProbeStoreGlobal.Load()
	if s == nil {
		return nil
	}
	return s.RemovedNodes()
}

// filterProbeRemovedNodes 从动态生成用的节点列表里去掉被摘掉的节点。
// 若会把启用的节点全部去掉,则原样返回。
func filterProbeRemovedNodes(nodes []storage.Node) []storage.Node {
	removed := probeRemovedNodes()
	if len(removed) == 0 {
		return nodes
	}
	out := make([]storage.Node, 0, len(nodes))
	enabledKept := 0
	for _, n := range nodes {
		if _, drop := removed[n.ID]; drop && n.Enabled {
			continue
		}
		out = append(out, n)
		if n.Enabled {
			enabledKept++
		}
	}
	if enabledKept == 0 {
		return nodes
	}
	return out
}

// removeProbeDownProxiesFromYAML 从静态订阅的 YAML 根节点里剔掉被摘掉的节点,返回剔掉的个数。
//
// 顺序:先算出「文件里有、且被摘掉」的候选;再把会把某个代理组剔空、被别的节点
// dialer-proxy 引用、或被规则直接引用的名字从候选里拿掉(反复直到稳定);最后
// 才真正从 proxies 和各组成员里删。收缩后候选若仍等于全部节点则什么都不做。
func removeProbeDownProxiesFromYAML(rootMap *yaml.Node) int {
	removedByID := probeRemovedNodes()
	if len(removedByID) == 0 || rootMap == nil || rootMap.Kind != yaml.MappingNode {
		return 0
	}
	removedNames := make(map[string]bool, len(removedByID))
	for _, name := range removedByID {
		if name != "" {
			removedNames[name] = true
		}
	}
	return removeProxiesByNameFromYAML(rootMap, removedNames)
}

// removeProxiesByNameFromYAML 实际的剔除逻辑;和探测存储解耦,便于测试。
func removeProxiesByNameFromYAML(rootMap *yaml.Node, removedNames map[string]bool) int {
	var proxiesNode, groupsNode, rulesNode *yaml.Node
	for i := 0; i+1 < len(rootMap.Content); i += 2 {
		switch rootMap.Content[i].Value {
		case "proxies":
			proxiesNode = rootMap.Content[i+1]
		case "proxy-groups":
			groupsNode = rootMap.Content[i+1]
		case "rules":
			rulesNode = rootMap.Content[i+1]
		}
	}
	if proxiesNode == nil || proxiesNode.Kind != yaml.SequenceNode {
		return 0
	}

	// 候选:文件里有、且被摘掉的
	candidates := make(map[string]bool)
	total := 0
	for _, p := range proxiesNode.Content {
		if p.Kind != yaml.MappingNode {
			continue
		}
		total++
		if name := yamlMapGet(p, "name"); removedNames[name] {
			candidates[name] = true
		}
	}
	if len(candidates) == 0 {
		return 0
	}

	// 反复收缩候选,直到没有新的"必须保留"
	for changed := true; changed; {
		changed = false
		keep := func(name string) {
			if candidates[name] {
				delete(candidates, name)
				changed = true
			}
		}
		// 代理组不能被剔空
		if groupsNode != nil && groupsNode.Kind == yaml.SequenceNode {
			for _, g := range groupsNode.Content {
				members := yamlMapSeq(g, "proxies")
				if members == nil || len(members.Content) == 0 {
					continue
				}
				allGone := true
				for _, m := range members.Content {
					if !candidates[m.Value] {
						allGone = false
						break
					}
				}
				if allGone {
					for _, m := range members.Content {
						keep(m.Value)
					}
				}
			}
		}
		// 留下的节点若 dialer-proxy 指向候选,候选得留
		for _, p := range proxiesNode.Content {
			if p.Kind != yaml.MappingNode || candidates[yamlMapGet(p, "name")] {
				continue
			}
			if dp := yamlMapGet(p, "dialer-proxy"); dp != "" {
				keep(dp)
			}
		}
		// 规则直接把节点名当策略用的,候选得留
		if rulesNode != nil && rulesNode.Kind == yaml.SequenceNode {
			for _, r := range rulesNode.Content {
				for _, part := range strings.Split(r.Value, ",") {
					keep(strings.TrimSpace(part))
				}
			}
		}
	}
	// 收缩完还要剔光全部节点(比如文件里根本没有分组):不动
	if len(candidates) == 0 || len(candidates) >= total {
		return 0
	}

	// 真正删除
	kept := proxiesNode.Content[:0]
	for _, p := range proxiesNode.Content {
		if p.Kind == yaml.MappingNode && candidates[yamlMapGet(p, "name")] {
			continue
		}
		kept = append(kept, p)
	}
	proxiesNode.Content = kept
	if groupsNode != nil && groupsNode.Kind == yaml.SequenceNode {
		for _, g := range groupsNode.Content {
			members := yamlMapSeq(g, "proxies")
			if members == nil {
				continue
			}
			mk := members.Content[:0]
			for _, m := range members.Content {
				if candidates[m.Value] {
					continue
				}
				mk = append(mk, m)
			}
			members.Content = mk
		}
	}
	return len(candidates)
}

// yamlMapSeq 取映射节点里 key 对应的序列节点;不存在或不是序列返回 nil。
func yamlMapSeq(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			if v := node.Content[i+1]; v.Kind == yaml.SequenceNode {
				return v
			}
			return nil
		}
	}
	return nil
}
