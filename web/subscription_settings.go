package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// subscriptionSettingsFile 是页面上可编辑的订阅器配置。
//
// 为什么必须落盘：旧实现里维护者清单只从环境变量读（PROXY_SUBSCRIPTION_MAINTAINERS），
// 页面上既看不到也改不了，增删一个维护者要改 compose 再重启容器。供给源是会变动的
// 东西，把它钉在部署配置里等于每次调整都要走一次发布。
const subscriptionSettingsFile = "proxy-subscriptions.json"

type subscriptionSettings struct {
	// Maintainers 维护者域名清单。所有项默认启用；Disabled 里的才是被停用的。
	// 用「停用名单」而不是「启用名单」，是为了让新增维护者默认生效——
	// 用户加了一个地址却忘了勾启用，是最容易犯的错。
	Maintainers []string `json:"maintainers"`
	Disabled    []string `json:"disabled,omitempty"`
	Converter   string   `json:"converter,omitempty"`
}

// loadSubscriptionSettings 载入页面配置；文件不存在时用环境变量默认值初始化。
func (a *app) loadSubscriptionSettings() {
	settings := subscriptionSettings{Maintainers: envSubscriptionMaintainers()}
	if data, err := os.ReadFile(filepath.Join(a.dataDir, subscriptionSettingsFile)); err == nil {
		var stored subscriptionSettings
		if json.Unmarshal(data, &stored) == nil {
			cleaned, _ := normalizeMaintainerList(stored.Maintainers)
			if len(cleaned) > 0 || stored.Maintainers != nil {
				settings.Maintainers = cleaned
			}
			disabled, _ := normalizeMaintainerList(stored.Disabled)
			settings.Disabled = disabled
			if converter := strings.TrimRight(strings.TrimSpace(stored.Converter), "/"); converter != "" {
				settings.Converter = converter
			}
		}
	}
	a.subCfgMu.Lock()
	a.subSettings = settings
	a.subCfgMu.Unlock()
}

// saveSubscriptionSettingsLocked 落盘。调用方须持 subCfgMu 写锁。
func (a *app) saveSubscriptionSettingsLocked() error {
	data, err := json.MarshalIndent(a.subSettings, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, subscriptionSettingsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// envSubscriptionMaintainers 环境变量里的维护者清单（首次初始化的种子）。
func envSubscriptionMaintainers() []string {
	hosts, _ := normalizeMaintainerList(subscriptionMaintainers())
	return hosts
}

// normalizeMaintainerList 清洗维护者清单：小写、去尾点、去重、限长、限数量。
func normalizeMaintainerList(raw []string) ([]string, []string) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	rejected := make([]string, 0)
	for _, item := range raw {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(item)), ".")
		if host == "" {
			continue
		}
		if !validCandidateHostname(host) {
			rejected = append(rejected, item)
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
		if len(out) >= maxSubscriptionMaintainers {
			break
		}
	}
	return out, rejected
}

// maxSubscriptionMaintainers 维护者数量上限。太多会让一轮采集的拉取时间
// 线性膨胀，而收益递减——真实可用的第三方维护者远没有这么多。
const maxSubscriptionMaintainers = 24

// activeSubscriptionMaintainers 返回当前启用的维护者（排除停用项）。
func (a *app) activeSubscriptionMaintainers() []string {
	a.subCfgMu.Lock()
	defer a.subCfgMu.Unlock()
	disabled := make(map[string]struct{}, len(a.subSettings.Disabled))
	for _, host := range a.subSettings.Disabled {
		disabled[host] = struct{}{}
	}
	out := make([]string, 0, len(a.subSettings.Maintainers))
	for _, host := range a.subSettings.Maintainers {
		if _, skip := disabled[host]; skip {
			continue
		}
		out = append(out, host)
	}
	return out
}

// subscriptionConverterURL 当前生效的订阅转换器地址。
func (a *app) subscriptionConverterURL() string {
	a.subCfgMu.Lock()
	defer a.subCfgMu.Unlock()
	if converter := strings.TrimRight(strings.TrimSpace(a.subSettings.Converter), "/"); converter != "" {
		return converter
	}
	return defaultSubscriptionConverter()
}

// subscriptionSettingsSnapshot 供接口与 /api/status 输出的配置快照。
func (a *app) subscriptionSettingsSnapshot() map[string]any {
	a.subCfgMu.Lock()
	defer a.subCfgMu.Unlock()
	disabled := make(map[string]bool, len(a.subSettings.Disabled))
	for _, host := range a.subSettings.Disabled {
		disabled[host] = true
	}
	items := make([]map[string]any, 0, len(a.subSettings.Maintainers))
	for _, host := range a.subSettings.Maintainers {
		items = append(items, map[string]any{"host": host, "enabled": !disabled[host]})
	}
	converter := a.subSettings.Converter
	if converter == "" {
		converter = defaultSubscriptionConverter()
	}
	return map[string]any{
		"maintainers": items,
		"converter":   converter,
		"tokenSet":    subscriptionToken() != "",
	}
}

// updateSubscriptionSettings 处理页面提交的增删改。
//
// 三种动作合在一个函数里，因为它们的存储形态是同一个：加一行、删一行、
// 切一个开关，最终都落到「维护者清单 + 停用清单」这两个切片上。
// 每次都整份校验后落盘，不留半改状态。
func (a *app) updateSubscriptionSettings(add []string, remove []string, enable []string, disable []string, converter string) (added, removed, enabled, disabled, rejected []string, err error) {
	a.subCfgMu.Lock()
	defer a.subCfgMu.Unlock()

	current := append([]string(nil), a.subSettings.Maintainers...)
	disabledSet := make(map[string]struct{}, len(a.subSettings.Disabled))
	for _, host := range a.subSettings.Disabled {
		disabledSet[host] = struct{}{}
	}

	removeSet := make(map[string]struct{}, len(remove))
	for _, host := range remove {
		removeSet[strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")] = struct{}{}
	}
	if len(removeSet) > 0 {
		kept := make([]string, 0, len(current))
		for _, host := range current {
			if _, drop := removeSet[host]; drop {
				removed = append(removed, host)
				delete(disabledSet, host)
				continue
			}
			kept = append(kept, host)
		}
		current = kept
	}

	if len(add) > 0 {
		cleaned, bad := normalizeMaintainerList(add)
		rejected = append(rejected, bad...)
		existing := make(map[string]struct{}, len(current))
		for _, host := range current {
			existing[host] = struct{}{}
		}
		for _, host := range cleaned {
			if _, ok := existing[host]; ok {
				continue
			}
			if len(current) >= maxSubscriptionMaintainers {
				// 遇上限立即返回、不落盘：旧代码只 break 出添加循环，
				// 随后仍执行 enable/disable 并保存——调用方只看到 400，
				// 实际却已部分生效（含先执行的 remove），重试会重复操作。
				err = errSubscriptionMaintainerLimit
				return
			}
			current = append(current, host)
			existing[host] = struct{}{}
			added = append(added, host)
		}
	}

	for _, host := range enable {
		key := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if _, ok := disabledSet[key]; ok {
			delete(disabledSet, key)
			enabled = append(enabled, key)
		}
	}
	for _, host := range disable {
		key := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		found := false
		for _, existing := range current {
			if existing == key {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if _, ok := disabledSet[key]; !ok {
			disabledSet[key] = struct{}{}
			disabled = append(disabled, key)
		}
	}

	stored := subscriptionSettings{Maintainers: current}
	for host := range disabledSet {
		stored.Disabled = append(stored.Disabled, host)
	}
	if strings.TrimSpace(converter) != "" {
		trimmed := strings.TrimRight(strings.TrimSpace(converter), "/")
		if !strings.HasPrefix(trimmed, "https://") {
			err = errSubscriptionConverterScheme
			return
		}
		stored.Converter = trimmed
	} else {
		stored.Converter = a.subSettings.Converter
	}
	previous := a.subSettings
	a.subSettings = stored
	if saveErr := a.saveSubscriptionSettingsLocked(); saveErr != nil {
		a.subSettings = previous
		err = saveErr
		return
	}
	log.Printf("subscription settings updated: +%d -%d enable=%d disable=%d total=%d",
		len(added), len(removed), len(enabled), len(disabled), len(current))
	return
}
