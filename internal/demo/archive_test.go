package demo

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestIsArchivePath(t *testing.T) {
	cases := map[string]bool{
		"match.dem":     false,
		"match.zip":     true,
		"MATCH.ZIP":     true,
		"match.rar":     true,
		"match.7z":      true,
		"match.dem.gz":  true,
		"match.tar.gz":  true,
		"match.tgz":     true,
		"match.dem.bak": false,
	}
	for name, want := range cases {
		if got := IsArchivePath(name); got != want {
			t.Errorf("IsArchivePath(%q) = %v，期望 %v", name, got, want)
		}
	}
}

func TestResolveDemoPathPlain(t *testing.T) {
	dir := t.TempDir()
	dem := filepath.Join(dir, "a.dem")
	if err := os.WriteFile(dem, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveDemoPath(dem)
	if err != nil || got != dem {
		t.Fatalf("ResolveDemoPath = %q, %v；期望原样返回 %q", got, err, dem)
	}
}

func TestResolveDemoPathZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "match.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 嵌套目录与两个 .dem：应选体积更大的那个
	w, _ := zw.Create("demos/small.dem")
	w.Write([]byte("small"))
	w2, _ := zw.Create("demos/match.dem")
	w2.Write(bytes.Repeat([]byte("D"), 4096))
	zw.Close()
	f.Close()

	got, err := ResolveDemoPath(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "match_demo", "match.dem")
	if got != want {
		t.Fatalf("解压路径 = %q，期望 %q", got, want)
	}
	data, _ := os.ReadFile(got)
	if len(data) != 4096 {
		t.Fatalf("解压内容长度 = %d，期望 4096（应选最大的 .dem）", len(data))
	}
	// 幂等复用：第二次直接返回同一路径
	if again, err := ResolveDemoPath(zipPath); err != nil || again != got {
		t.Fatalf("二次 ResolveDemoPath = %q, %v；期望复用 %q", again, err, got)
	}
}

func TestResolveDemoPathGz(t *testing.T) {
	dir := t.TempDir()
	gzPath := filepath.Join(dir, "match.dem.gz")
	f, err := os.Create(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	gw.Write([]byte("demdata"))
	gw.Close()
	f.Close()

	got, err := ResolveDemoPath(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "match_demo", "match.dem"); got != want {
		t.Fatalf("解压路径 = %q，期望 %q", got, want)
	}
	data, _ := os.ReadFile(got)
	if string(data) != "demdata" {
		t.Fatalf("解压内容 = %q，期望 demdata", data)
	}
}

func TestResolveDemoPathTarGz(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "match.tar.gz")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	body := []byte("tardem")
	tw.WriteHeader(&tar.Header{Name: "x/match.dem", Mode: 0o644, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	gw.Close()
	f.Close()

	got, err := ResolveDemoPath(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "match_demo", "match.dem"); got != want {
		t.Fatalf("解压路径 = %q，期望 %q", got, want)
	}
	if data, _ := os.ReadFile(got); string(data) != "tardem" {
		t.Fatalf("解压内容 = %q，期望 tardem", data)
	}
}

func TestResolveDemoPathZipWithoutDem(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "readme.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("no demo"))
	zw.Close()
	f.Close()
	if _, err := ResolveDemoPath(zipPath); err == nil {
		t.Fatal("压缩包内没有 .dem 时应报错")
	}
}
