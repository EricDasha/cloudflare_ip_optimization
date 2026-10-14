package main

import (
	"slices"
	"testing"
)

func TestCFnatArgsIncludesStickyFlag(t *testing.T) {
	on := defaultCFnatConfig()
	on.Sticky = true
	if !slices.Contains(on.args(), "-sticky=true") {
		t.Fatalf("args() 缺少 -sticky=true: %v", on.args())
	}
	off := defaultCFnatConfig()
	off.Sticky = false
	if !slices.Contains(off.args(), "-sticky=false") {
		t.Fatalf("args() 缺少 -sticky=false: %v", off.args())
	}
}

// 布尔旗标必须以「-flag=值」单参形式出现：Go flag 对 bool 不消费后随参数，
// "-flag true" 会被当位置参数截断后续解析。
func TestCFnatBoolFlagsUseEqualsForm(t *testing.T) {
	args := defaultCFnatConfig().args()
	for i, arg := range args {
		if arg == "-random" || arg == "-tls" || arg == "-sticky" {
			t.Fatalf("布尔旗标 %q 未使用 = 形式（下标 %d）: %v", arg, i, args)
		}
	}
	for _, want := range []string{"-random=", "-tls=", "-sticky="} {
		found := false
		for _, arg := range args {
			if len(arg) >= len(want) && arg[:len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("args() 缺少 %s 前缀的布尔旗标: %v", want, args)
		}
	}
}

func TestDefaultCFnatStickyFromEnv(t *testing.T) {
	t.Setenv("CFNAT_STICKY", "false")
	if defaultCFnatConfig().Sticky {
		t.Fatal("CFNAT_STICKY=false 未生效")
	}
	t.Setenv("CFNAT_STICKY", "true")
	if !defaultCFnatConfig().Sticky {
		t.Fatal("CFNAT_STICKY=true 未生效")
	}
}
