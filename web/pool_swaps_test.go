package main

import (
	"reflect"
	"testing"
	"time"
)

// 替补已在位（考试期间别的路径把它换进来了）时不再重复进池，
// 病号仍摘掉；同一轮两个病号共用一个替补时，第二个也不再重复进池。
func TestMergePoolSwapsSkipsAlreadyPresentReplacement(t *testing.T) {
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"}
	victims := map[string]bool{"2.2.2.2": true, "3.3.3.3": true}
	// 2.2.2.2 的替补 4.4.4.4 已在池里（并发换入）；3.3.3.3 的替补是新 IP。
	replacements := map[string]string{"2.2.2.2": "4.4.4.4", "3.3.3.3": "5.5.5.5"}
	next, removed, swapped, held := mergePoolSwaps(ips, victims, replacements, true)
	// 4.4.4.4 已在位，留在它原来的位置（末尾），不重复进池；
	// 5.5.5.5 接替 3.3.3.3 的位置。
	if !reflect.DeepEqual(next, []string{"1.1.1.1", "5.5.5.5", "4.4.4.4"}) {
		t.Fatalf("next = %v, want [1.1.1.1 5.5.5.5 4.4.4.4]（4.4.4.4 不重复）", next)
	}
	if !reflect.DeepEqual(removed, []string{"2.2.2.2", "3.3.3.3"}) {
		t.Fatalf("removed = %v, want 两个病号都被摘掉", removed)
	}
	if len(swapped) != 2 || len(held) != 0 {
		t.Fatalf("swapped = %v held = %v, want 2 条换人记录、0 暂留", swapped, held)
	}
}

// 两个病号共用一个替补：第一个带进来，第二个不再重复进池。
func TestMergePoolSwapsSharedReplacementOnce(t *testing.T) {
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	victims := map[string]bool{"1.1.1.1": true, "2.2.2.2": true}
	replacements := map[string]string{"1.1.1.1": "9.9.9.9", "2.2.2.2": "9.9.9.9"}
	next, removed, _, _ := mergePoolSwaps(ips, victims, replacements, true)
	if !reflect.DeepEqual(next, []string{"9.9.9.9", "3.3.3.3"}) {
		t.Fatalf("next = %v, want [9.9.9.9 3.3.3.3]（替补只进一次）", next)
	}
	if !reflect.DeepEqual(removed, []string{"1.1.1.1", "2.2.2.2"}) {
		t.Fatalf("removed = %v, want 两个病号都被摘掉", removed)
	}
}

// 无替补时验活暂留（keepUnresolved=true）、关闭席摘掉（false）。
func TestMergePoolSwapsUnresolvedHandling(t *testing.T) {
	ips := []string{"1.1.1.1", "2.2.2.2"}
	victims := map[string]bool{"2.2.2.2": true}
	next, _, _, held := mergePoolSwaps(ips, victims, nil, true)
	if !reflect.DeepEqual(next, ips) || !reflect.DeepEqual(held, []string{"2.2.2.2"}) {
		t.Fatalf("验活无替补应暂留：next = %v held = %v", next, held)
	}
	next, removed, _, held := mergePoolSwaps(ips, victims, nil, false)
	if !reflect.DeepEqual(next, []string{"1.1.1.1"}) || len(held) != 0 {
		t.Fatalf("关闭席无替补应摘掉：next = %v held = %v", next, held)
	}
	if !reflect.DeepEqual(removed, []string{"2.2.2.2"}) {
		t.Fatalf("removed = %v, want [2.2.2.2]", removed)
	}
}

// 正常换人：非病号保留，病号换成替补，顺序不变。
func TestMergePoolSwapsNormal(t *testing.T) {
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	victims := map[string]bool{"2.2.2.2": true}
	replacements := map[string]string{"2.2.2.2": "9.9.9.9"}
	next, removed, swapped, held := mergePoolSwaps(ips, victims, replacements, true)
	if !reflect.DeepEqual(next, []string{"1.1.1.1", "9.9.9.9", "3.3.3.3"}) {
		t.Fatalf("next = %v, want [1.1.1.1 9.9.9.9 3.3.3.3]", next)
	}
	if !reflect.DeepEqual(removed, []string{"2.2.2.2"}) || len(swapped) != 1 || len(held) != 0 {
		t.Fatalf("removed = %v swapped = %v held = %v", removed, swapped, held)
	}
}

// 端到端：关闭席的替补已在池里时，提交后池里不能出现重复 IP。
func TestApplyDisabledBenchChangesSkipsAlreadyPresentSwap(t *testing.T) {
	a := &app{
		dataDir: t.TempDir(),
		activePool: proxyActivePool{
			IPs:       []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"},
			Disabled:  []string{"1.1.1.1"},
			UpdatedAt: time.Now(),
		},
	}
	// 1.1.1.1 是关闭弱成员，替补 2.2.2.2 已在池里（别的路径刚换入）。
	a.applyDisabledBenchChanges(nil,
		map[string]string{"1.1.1.1": "2.2.2.2"},
		map[string]string{"1.1.1.1": "替补更快"},
		time.Now())
	got := a.proxyActivePoolSnapshot()
	want := []string{"2.2.2.2", "3.3.3.3"}
	if !reflect.DeepEqual(got.IPs, want) {
		t.Fatalf("提交后池 = %v, want %v（2.2.2.2 不重复、1.1.1.1 已摘）", got.IPs, want)
	}
}
