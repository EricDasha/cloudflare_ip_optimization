package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readCandidateCache(t *testing.T, dir string) proxyCandidateSnapshot {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, proxyCandidateCacheFile))
	if err != nil {
		t.Fatalf("read candidate cache: %v", err)
	}
	var got proxyCandidateSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("candidate cache is not valid JSON: %v", err)
	}
	return got
}

func assertNoTempResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

// persistCandidates 必须落「当前最新快照」，而不是调用方手里那份可能已过期的副本。
func TestPersistCandidatesWritesLatestSnapshot(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.candidates = proxyCandidateSnapshot{IPs: []string{"1.1.1.1"}, SourceByIP: map[string]string{"1.1.1.1": "user"}}
	if err := a.persistCandidates(); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	a.candidateMu.Lock()
	a.candidates = proxyCandidateSnapshot{IPs: []string{"8.8.8.8"}, SourceByIP: map[string]string{"8.8.8.8": "cfdata"}}
	a.candidateMu.Unlock()
	if err := a.persistCandidates(); err != nil {
		t.Fatalf("second persist: %v", err)
	}
	got := readCandidateCache(t, a.dataDir)
	if len(got.IPs) != 1 || got.IPs[0] != "8.8.8.8" || got.SourceByIP["8.8.8.8"] != "cfdata" {
		t.Fatalf("persisted snapshot is stale: %#v", got)
	}
	assertNoTempResidue(t, a.dataDir)
}

// 并发保存曾经共用固定 path+".tmp"：这里验证 atomic 写之后文件始终合法且无残留。
func TestPersistCandidatesConcurrentWriters(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.0.%d", i+1)
			a.candidateMu.Lock()
			a.candidates = proxyCandidateSnapshot{IPs: []string{ip}, SourceByIP: map[string]string{ip: "user"}}
			a.candidateMu.Unlock()
			if err := a.persistCandidates(); err != nil {
				t.Errorf("persist %s: %v", ip, err)
			}
		}(i)
	}
	wg.Wait()
	if got := readCandidateCache(t, a.dataDir); len(got.IPs) != 1 {
		t.Fatalf("cache has %d IPs, want 1", len(got.IPs))
	}
	assertNoTempResidue(t, a.dataDir)
}
