package main

import (
	"fmt"
	"testing"
)

// 维护者清单清洗：小写、去尾点、去重、拒收非法主机名。
func TestNormalizeMaintainerList(t *testing.T) {
	cleaned, rejected := normalizeMaintainerList([]string{
		"A.COM", "a.com.", "a.com", "  b.com  ", "not a host", "", "c.com",
	})
	want := []string{"a.com", "b.com", "c.com"}
	if len(cleaned) != len(want) {
		t.Fatalf("cleaned = %v, want %v", cleaned, want)
	}
	for i := range want {
		if cleaned[i] != want[i] {
			t.Fatalf("cleaned = %v, want %v", cleaned, want)
		}
	}
	if len(rejected) != 1 {
		t.Fatalf("rejected = %v, want one bad host", rejected)
	}
}

// 上限之外的条目被截断，不是被拒——多出来的静默丢弃会让页面显示与实际不符，
// 所以这里只断言长度，不要求 rejected 记账。
func TestNormalizeMaintainerListCapsCount(t *testing.T) {
	raw := make([]string, 0, maxSubscriptionMaintainers+5)
	for i := 0; i < maxSubscriptionMaintainers+5; i++ {
		raw = append(raw, "h"+itoa(i)+".example.com")
	}
	cleaned, _ := normalizeMaintainerList(raw)
	if len(cleaned) != maxSubscriptionMaintainers {
		t.Fatalf("cleaned len = %d, want %d", len(cleaned), maxSubscriptionMaintainers)
	}
}

// 新增的维护者默认启用；停用名单只影响列出的那些。
func TestActiveSubscriptionMaintainersRespectsDisabled(t *testing.T) {
	a := newInventoryTestApp(t)
	a.subSettings = subscriptionSettings{
		Maintainers: []string{"a.com", "b.com", "c.com"},
		Disabled:    []string{"b.com"},
	}
	got := a.activeSubscriptionMaintainers()
	want := []string{"a.com", "c.com"}
	if len(got) != len(want) {
		t.Fatalf("active = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active = %v, want %v", got, want)
		}
	}
}

// 增删改是一组合动作，最终必须一致落盘：清单与停用名单不能各说各话。
func TestUpdateSubscriptionSettingsAddRemoveEnableDisable(t *testing.T) {
	a := newInventoryTestApp(t)
	a.subSettings = subscriptionSettings{Maintainers: []string{"a.com", "b.com"}}

	added, removed, enabled, disabled, rejected, err := a.updateSubscriptionSettings(
		[]string{"C.COM", "a.com"}, // c 新增，a 已存在不重复
		[]string{"b.com"},          // 删 b
		nil,
		[]string{"a.com"}, // 停 a
		"")
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if len(added) != 1 || added[0] != "c.com" {
		t.Fatalf("added = %v", added)
	}
	if len(removed) != 1 || removed[0] != "b.com" {
		t.Fatalf("removed = %v", removed)
	}
	if len(disabled) != 1 || disabled[0] != "a.com" {
		t.Fatalf("disabled = %v", disabled)
	}
	if len(enabled) != 0 || len(rejected) != 0 {
		t.Fatalf("enabled=%v rejected=%v", enabled, rejected)
	}

	// 重新启用 a。
	_, _, enabled, _, _, err = a.updateSubscriptionSettings(nil, nil, []string{"a.com"}, nil, "")
	if err != nil {
		t.Fatalf("enable failed: %v", err)
	}
	if len(enabled) != 1 || enabled[0] != "a.com" {
		t.Fatalf("enabled = %v", enabled)
	}
	if got := a.activeSubscriptionMaintainers(); len(got) != 2 {
		t.Fatalf("active after re-enable = %v, want a.com + c.com", got)
	}

	// 落盘必须能被重新载入。
	b := &app{dataDir: a.dataDir}
	b.loadSubscriptionSettings()
	if len(b.subSettings.Maintainers) != 2 {
		t.Fatalf("persisted maintainers = %v", b.subSettings.Maintainers)
	}
	if len(b.subSettings.Disabled) != 0 {
		t.Fatalf("persisted disabled = %v", b.subSettings.Disabled)
	}
}

// 删除一个不存在的维护者不该报错，也不该影响其它项。
func TestUpdateSubscriptionSettingsRemoveMissingIsNoop(t *testing.T) {
	a := newInventoryTestApp(t)
	a.subSettings = subscriptionSettings{Maintainers: []string{"a.com"}}
	_, removed, _, _, _, err := a.updateSubscriptionSettings(nil, []string{"zzz.com"}, nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if len(a.subSettings.Maintainers) != 1 {
		t.Fatalf("maintainers = %v", a.subSettings.Maintainers)
	}
}

// 转换器必须是 https://，否则拒收——订阅内容会带着 token 出去，
// 明文 http 等于把 token 交给路上每一个人。
func TestUpdateSubscriptionSettingsRejectsInsecureConverter(t *testing.T) {
	a := newInventoryTestApp(t)
	a.subSettings = subscriptionSettings{Maintainers: []string{"a.com"}}
	_, _, _, _, _, err := a.updateSubscriptionSettings(nil, nil, nil, nil, "http://evil.example.com/sub")
	if err == nil {
		t.Fatal("insecure converter accepted")
	}
	if a.subSettings.Converter != "" {
		t.Fatalf("converter was stored despite rejection: %q", a.subSettings.Converter)
	}
}

// 上限之外的新增必须报错，而不是静默丢弃。
func TestUpdateSubscriptionSettingsEnforcesLimit(t *testing.T) {
	a := newInventoryTestApp(t)
	list := make([]string, 0, maxSubscriptionMaintainers)
	for i := 0; i < maxSubscriptionMaintainers; i++ {
		list = append(list, "h"+itoa(i)+".example.com")
	}
	a.subSettings = subscriptionSettings{Maintainers: list}
	_, _, _, _, _, err := a.updateSubscriptionSettings([]string{"extra.example.com"}, nil, nil, nil, "")
	if err == nil {
		t.Fatal("adding beyond the cap did not error")
	}
}

// 快照必须如实反映启用状态，供页面渲染。
func TestSubscriptionSettingsSnapshot(t *testing.T) {
	a := newInventoryTestApp(t)
	a.subSettings = subscriptionSettings{
		Maintainers: []string{"a.com", "b.com"},
		Disabled:    []string{"b.com"},
	}
	snap := a.subscriptionSettingsSnapshot()
	items, ok := snap["maintainers"].([]map[string]any)
	if !ok || len(items) != 2 {
		t.Fatalf("maintainers snapshot = %v", snap["maintainers"])
	}
	if items[0]["enabled"] != true || items[1]["enabled"] != false {
		t.Fatalf("enabled flags wrong: %v", items)
	}
	if snap["converter"] == "" {
		t.Fatal("converter should fall back to the default")
	}
}

// 遇维护者上限必须整单拒绝、不落盘：旧代码报 400 的同时已把部分
// 添加与先执行的删除保存了。
func TestUpdateSubscriptionSettingsLimitIsAtomic(t *testing.T) {
	maintainers := make([]string, 0, maxSubscriptionMaintainers)
	for i := 0; i < maxSubscriptionMaintainers; i++ {
		maintainers = append(maintainers, fmt.Sprintf("m%d.example.com", i))
	}
	a := &app{dataDir: t.TempDir(), subSettings: subscriptionSettings{Maintainers: maintainers}}
	// 已满 24 个：一次加 2 个、同时删 1 个、同时 disable 1 个——
	// 整单都必须被拒绝，内存与落盘都不变。
	a.subSettings.Disabled = []string{"m1.example.com"}
	_, _, _, _, _, err := a.updateSubscriptionSettings(
		[]string{"new1.example.com", "new2.example.com"}, []string{"m0.example.com"}, nil, []string{"m2.example.com"}, "")
	if err != errSubscriptionMaintainerLimit {
		t.Fatalf("err = %v, want maintainer limit error", err)
	}
	if len(a.subSettings.Maintainers) != maxSubscriptionMaintainers {
		t.Fatalf("maintainers = %d, want %d (unchanged)", len(a.subSettings.Maintainers), maxSubscriptionMaintainers)
	}
	m0Found := false
	for _, host := range a.subSettings.Maintainers {
		if host == "new1.example.com" || host == "new2.example.com" {
			t.Fatalf("%s was added despite limit error", host)
		}
		if host == "m0.example.com" {
			m0Found = true
		}
	}
	if !m0Found {
		t.Fatalf("m0.example.com was removed despite limit error")
	}
	if len(a.subSettings.Disabled) != 1 || a.subSettings.Disabled[0] != "m1.example.com" {
		t.Fatalf("disabled changed despite limit error: %v", a.subSettings.Disabled)
	}
}
