package handler

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"miaomiaowu/internal/storage"
)

func parseRootMap(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Content[0]
}

func dumpYAML(t *testing.T, n *yaml.Node) string {
	t.Helper()
	out, err := yaml.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestRemoveProxiesByNameFromYAML(t *testing.T) {
	src := `
proxies:
  - {name: A, type: ss, server: a, port: 1}
  - {name: B, type: ss, server: b, port: 1, dialer-proxy: C}
  - {name: C, type: ss, server: c, port: 1}
  - {name: D, type: ss, server: d, port: 1}
proxy-groups:
  - name: PROXY
    type: select
    proxies: [A, B, C, D, DIRECT]
  - name: ONLY-B
    type: select
    proxies: [B]
rules:
  - DOMAIN-SUFFIX,x.com,A
  - MATCH,PROXY
`
	root := parseRootMap(t, src)
	// A 被规则直接引用、B 是 ONLY-B 的唯一成员、C 被留下的 B 用 dialer-proxy 引用:三个都不能剔;
	// 只有 D 真正被剔掉。
	n := removeProxiesByNameFromYAML(root, map[string]bool{"A": true, "B": true, "C": true, "D": true})
	if n != 1 {
		t.Fatalf("应只剔掉 D,实际剔掉 %d 个", n)
	}
	out := dumpYAML(t, root)
	for _, keep := range []string{"name: A", "name: B", "name: C"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%s 不该被剔掉:\n%s", keep, out)
		}
	}
	if strings.Contains(out, "name: D") || !strings.Contains(out, "proxies: [A, B, C, DIRECT]") {
		t.Errorf("D 应从 proxies 和分组里消失,DIRECT 等非节点成员不受影响:\n%s", out)
	}
}

func TestRemoveProxiesByNameFromYAML_NeverEmptiesEverything(t *testing.T) {
	src := `
proxies:
  - {name: A, type: ss, server: a, port: 1}
  - {name: B, type: ss, server: b, port: 1}
proxy-groups:
  - {name: PROXY, type: select, proxies: [A, B]}
`
	root := parseRootMap(t, src)
	if n := removeProxiesByNameFromYAML(root, map[string]bool{"A": true, "B": true}); n != 0 {
		t.Fatalf("全部节点都不可用时应原样下发,实际剔掉 %d 个", n)
	}
	// 只剔一个:分组还剩 B,允许
	if n := removeProxiesByNameFromYAML(root, map[string]bool{"A": true}); n != 1 {
		t.Fatalf("应剔掉 A,实际 %d", n)
	}
	out := dumpYAML(t, root)
	if strings.Contains(out, "name: A") || !strings.Contains(out, "name: B") {
		t.Fatalf("剔除结果不对:\n%s", out)
	}
}

func TestFilterProbeRemovedNodes(t *testing.T) {
	store := NewNodeProbeStore(0)
	SetNodeProbeStore(store)
	defer SetNodeProbeStore(nil)

	nodes := []storage.Node{
		{ID: 1, NodeName: "A", Enabled: true},
		{ID: 2, NodeName: "B", Enabled: true},
		{ID: 3, NodeName: "C", Enabled: false},
	}
	// 没有摘掉的:原样
	if got := filterProbeRemovedNodes(nodes); len(got) != 3 {
		t.Fatalf("无摘除时不该过滤,得到 %d 个", len(got))
	}
	store.Record(1, 0, false, "master")
	store.MarkRemoved(1, "A", true)
	got := filterProbeRemovedNodes(nodes)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("应只去掉 A,得到 %+v", got)
	}
	// 启用的节点会被全部去掉:不过滤
	store.Record(2, 0, false, "master")
	store.MarkRemoved(2, "B", true)
	if got := filterProbeRemovedNodes(nodes); len(got) != 3 {
		t.Fatalf("会剔光启用节点时应原样返回,得到 %d 个", len(got))
	}
	// 功能关闭:全部放回
	store.ClearRemoved()
	if len(store.RemovedNodes()) != 0 {
		t.Fatal("ClearRemoved 后不该还有摘掉的节点")
	}
}

func TestReconcileRemoval(t *testing.T) {
	store := NewNodeProbeStore(0)
	s := &NodeProbeScheduler{store: store}
	target := storage.ProbeTargetNode{ID: 9, NodeName: "N"}

	// 两次失败 → 阈值 0 分钟 → 立刻摘掉
	store.Record(9, 0, false, "master")
	st := store.Record(9, 0, false, "master")
	s.reconcileRemoval(target, st, nodeProbeRemoveConfig{Enabled: true, Minutes: 0})
	if got := store.RemovedNodes(); got[9] != "N" {
		t.Fatalf("应被摘掉,实际 %v", got)
	}

	// 阈值 30 分钟:刚掉线不到 30 分钟 → 不摘。先放回再验证。
	store.MarkRemoved(9, "N", false)
	st, _ = store.Snapshot(9)
	s.reconcileRemoval(target, st, nodeProbeRemoveConfig{Enabled: true, Minutes: 30})
	if len(store.RemovedNodes()) != 0 {
		t.Fatal("掉线未满阈值不该被摘掉")
	}
	// 把采样时间倒拨 31 分钟,模拟已掉线很久
	store.mu.Lock()
	for i := range store.data[9].Samples {
		store.data[9].Samples[i].At = time.Now().Add(-31 * time.Minute).Unix()
	}
	store.mu.Unlock()
	st, _ = store.Snapshot(9)
	s.reconcileRemoval(target, st, nodeProbeRemoveConfig{Enabled: true, Minutes: 30})
	if len(store.RemovedNodes()) != 1 {
		t.Fatal("掉线满阈值应被摘掉")
	}

	// 恢复 → 放回
	st = store.Record(9, 40, true, "master")
	s.reconcileRemoval(target, st, nodeProbeRemoveConfig{Enabled: true})
	if len(store.RemovedNodes()) != 0 {
		t.Fatal("恢复后应放回订阅")
	}

	// 功能关闭时即使不可用也要放回
	store.Record(9, 0, false, "master")
	store.Record(9, 0, false, "master")
	store.MarkRemoved(9, "N", true)
	st, _ = store.Snapshot(9)
	s.reconcileRemoval(target, st, nodeProbeRemoveConfig{Enabled: false})
	if len(store.RemovedNodes()) != 0 {
		t.Fatal("功能关闭后应放回订阅")
	}
}
