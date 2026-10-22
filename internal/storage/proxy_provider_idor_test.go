package storage

import (
	"context"
	"testing"
)

// GitHub issue #131:按外部订阅 ID 列代理集合配置时必须按用户名过滤,
// 否则任何登录用户枚举 ID 就能读到别人的配置(含上游请求头)。
func TestListProxyProviderConfigsBySubscriptionScopedToOwner(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	mk := func(user, name string) int64 {
		subID, err := repo.CreateExternalSubscription(ctx, ExternalSubscription{
			Username: user, Name: name, URL: "https://example.com/" + name,
		})
		if err != nil {
			t.Fatalf("建外部订阅 %s: %v", name, err)
		}
		if _, err := repo.CreateProxyProviderConfig(ctx, &ProxyProviderConfig{
			Username: user, ExternalSubscriptionID: subID, Name: name + "-provider", Type: "http",
			Header: `{"Authorization":["secret-" + name]}`, ProcessMode: "client",
		}); err != nil {
			t.Fatalf("建代理集合配置 %s: %v", name, err)
		}
		return subID
	}
	victimSub := mk("victim", "v")
	attackerSub := mk("attacker", "a")

	got, err := repo.ListProxyProviderConfigsBySubscription(ctx, victimSub, "attacker")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("攻击者用受害者的订阅 ID 不该查到任何配置,实得 %d 条", len(got))
	}
	got, err = repo.ListProxyProviderConfigsBySubscription(ctx, victimSub, "victim")
	if err != nil || len(got) != 1 || got[0].Username != "victim" {
		t.Fatalf("所有者本人应查到自己的 1 条配置: %v %+v", err, got)
	}
	got, _ = repo.ListProxyProviderConfigsBySubscription(ctx, attackerSub, "attacker")
	if len(got) != 1 {
		t.Fatalf("攻击者查自己的订阅应得 1 条,实得 %d", len(got))
	}
}
