package gui

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHandleParseZip 解析请求收到压缩包时应先自动解压：
// 假 demo 内容会解析失败，但错误必须是"解析 demo 失败"而不是"解压失败"，
// 证明解压成功并进入了 demo 解析阶段。
func TestHandleParseZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "match.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("match.dem")
	w.Write([]byte("PBDEMS2 fake"))
	zw.Close()
	f.Close()

	body, _ := json.Marshal(parseReq{DemoPath: zipPath})
	rec := httptest.NewRecorder()
	handleParse(rec, httptest.NewRequest("POST", "/api/parse", bytes.NewReader(body)))
	var resp parseResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v\n%s", err, rec.Body.String())
	}
	if !strings.Contains(resp.Err, "解析 demo 失败") {
		t.Fatalf("错误 = %q，期望进入解析阶段（解析 demo 失败）", resp.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, "match_demo", "match.dem")); err != nil {
		t.Fatalf("压缩包未被解压: %v", err)
	}
}

// TestHandleParseMissingArchive 不存在的压缩包应给出明确错误。
func TestHandleParseMissingArchive(t *testing.T) {
	body, _ := json.Marshal(parseReq{DemoPath: filepath.Join(t.TempDir(), "nope.zip")})
	rec := httptest.NewRecorder()
	handleParse(rec, httptest.NewRequest("POST", "/api/parse", bytes.NewReader(body)))
	var resp parseResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Err, "压缩包不存在") {
		t.Fatalf("错误 = %q，期望提示压缩包不存在", resp.Err)
	}
}

// TestHandleBrowseShowsArchives 文件浏览器应同时列出 .dem 与支持的压缩包。
func TestHandleBrowseShowsArchives(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.dem", "b.zip", "c.rar", "d.7z", "e.dem.gz", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	handleBrowse(rec, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(dir), nil))
	var resp struct {
		Entries []browseItem `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v\n%s", err, rec.Body.String())
	}
	got := map[string]browseItem{}
	for _, e := range resp.Entries {
		got[e.Name] = e
	}
	for _, name := range []string{"a.dem", "b.zip", "c.rar", "d.7z", "e.dem.gz"} {
		if _, ok := got[name]; !ok {
			t.Errorf("浏览器未列出 %s", name)
		}
	}
	if _, ok := got["notes.txt"]; ok {
		t.Error("浏览器不应列出 notes.txt")
	}
	if !got["b.zip"].IsArchive || got["a.dem"].IsArchive {
		t.Error("IsArchive 标记不正确")
	}
}

// TestHandleBrowseReturnsModTime 文件浏览器要为目录与文件都返回修改时间，
// 前端据此展示时间（文件名看不出新旧，选 demo 时需要按时间判断）。
func TestHandleBrowseReturnsModTime(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	dem := filepath.Join(dir, "a.dem")
	if err := os.WriteFile(dem, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Windows 上目录时间也可能被其它操作影响，分别设置为一个明确的历史时间
	want := time.Date(2026, 10, 20, 13, 45, 0, 0, time.Local)
	for _, p := range []string{sub, dem} {
		if err := os.Chtimes(p, want, want); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	handleBrowse(rec, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(dir), nil))
	var resp struct {
		Entries []browseItem `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v\n%s", err, rec.Body.String())
	}
	got := map[string]browseItem{}
	for _, e := range resp.Entries {
		got[e.Name] = e
	}
	for _, name := range []string{"sub", "a.dem"} {
		e, ok := got[name]
		if !ok {
			t.Fatalf("浏览器未列出 %s", name)
		}
		if e.ModTime != want.Unix() {
			t.Errorf("%s ModTime = %d，期望 %d", name, e.ModTime, want.Unix())
		}
	}
}
