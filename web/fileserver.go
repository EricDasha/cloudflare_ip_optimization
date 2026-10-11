package main

import (
	"bufio"
	"embed"
	"encoding/csv"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed static/*
var staticFS embed.FS

func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/cfnat", "/pool", "/cfdata", "/files":
			http.ServeFileFS(w, r, sub, "index.html")
			return
		default:
			fileServer.ServeHTTP(w, r)
		}
	})
}

type fileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	Kind    string `json:"kind"`
}

func (a *app) handleFiles(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files := []fileInfo{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !allowedFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
			Kind:    strings.TrimPrefix(strings.ToLower(filepath.Ext(e.Name())), "."),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime > files[j].ModTime })
	writeJSON(w, map[string]any{"files": files, "dataCenters": a.dataCenters()})
}

func (a *app) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/file/")
	name = filepath.Base(name)
	if !allowedFile(name) {
		http.Error(w, "file denied", http.StatusBadRequest)
		return
	}
	path := filepath.Join(a.dataDir, name)
	http.ServeFile(w, r, path)
}

func (a *app) dataCenters() []string {
	path := filepath.Join(a.dataDir, "ip.csv")
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for i, row := range rows {
		if i == 0 || len(row) < 2 {
			continue
		}
		dc := strings.TrimSpace(row[1])
		if dc != "" && !seen[dc] {
			seen[dc] = true
			out = append(out, dc)
		}
	}
	sort.Strings(out)
	return out
}

// vlessTemplateFileName 返回 VLESS outbound 模板的基名。
// 文件接口绝不能奉还模板——内含真实 UUID/SNI/path。README 与 DESIGN 的
// 安全承诺「API 不返回模板内容」由此闸兑现：旧实现仅凭扩展名白名单放行
// .json，而模板恰是 .json，承诺曾是一纸空文（A1 定案）。
func vlessTemplateFileName() string {
	custom := strings.TrimSpace(env("PROXY_VLESS_TEMPLATE", ""))
	if custom == "" {
		return "vless-probe-outbound.json"
	}
	return strings.ToLower(filepath.Base(custom))
}

func allowedFile(name string) bool {
	if name != filepath.Base(name) {
		return false
	}
	lower := strings.ToLower(name)
	// 模板与 sing-box 探针临时配置（.vless-probe-*.json，0600、探完即删，
	// 但崩溃残留可能滞留）一律拒收——凭证文件不进文件接口。
	if lower == vlessTemplateFileName() || strings.HasPrefix(lower, ".vless-probe-") {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".csv", ".txt", ".json", ".log":
		return true
	default:
		return false
	}
}
