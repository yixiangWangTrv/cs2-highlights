package demo

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"cs2-highlights/internal/winutil"
)

// maxExtractBytes 单个 demo 的解压上限（防压缩炸弹；CS2 demo 通常 < 1GB）。
const maxExtractBytes = 8 << 30

// archiveExts 支持的压缩包后缀（按长度从长到短匹配，.tar.gz 优先于 .gz）。
var archiveExts = []string{
	".tar.gz", ".tar.bz2", ".tar.xz", ".tgz",
	".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz",
}

// IsArchivePath 判断文件名是否为支持的压缩包（只看后缀，不访问磁盘）。
func IsArchivePath(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range archiveExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// ResolveDemoPath 返回可直接交给 Parse 的 .dem 路径：
//   - 普通 .dem（或其它非压缩包）原样返回
//   - 压缩包解压到同目录的 <名字>_demo/ 下，返回其中体积最大的 .dem
//
// 幂等：已解压且比压缩包新时直接复用，不重复解压。
// zip / gz / tar(.gz) 用 Go 标准库处理；rar / 7z / bz2 / xz 交给
// Windows 自带的 tar.exe（Win10 1803+ 内置 bsdtar，Win11 支持 rar/7z 读取）。
func ResolveDemoPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("demo 路径为空")
	}
	if !IsArchivePath(path) {
		return path, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("压缩包不存在: %w", err)
	}
	outDir := filepath.Join(filepath.Dir(path), archiveStem(path)+"_demo")
	// 复用已解压结果：解压出的 .dem 比压缩包新（同一批文件复制时 mtime 相同也算新）
	if dem := findLargestDem(outDir); dem != "" {
		if dst, err := os.Stat(dem); err == nil && !dst.ModTime().Before(st.ModTime()) {
			return dem, nil
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("创建解压目录失败: %w", err)
	}
	if entries, err := os.ReadDir(outDir); err == nil {
		for _, e := range entries {
			os.RemoveAll(filepath.Join(outDir, e.Name()))
		}
	}
	return extractArchive(path, outDir)
}

// archiveStem 去掉压缩包后缀（并去掉紧跟的 .dem），用作解压目录名。
// match.zip → match；match.dem.gz → match。
func archiveStem(path string) string {
	name := filepath.Base(path)
	lower := strings.ToLower(name)
	for _, ext := range archiveExts {
		if strings.HasSuffix(lower, ext) {
			name = name[:len(name)-len(ext)]
			break
		}
	}
	if strings.HasSuffix(strings.ToLower(name), ".dem") {
		name = name[:len(name)-len(".dem")]
	}
	return name
}

// extractArchive 按格式选择解压实现，返回解压出的 .dem 路径。
func extractArchive(path, outDir string) (string, error) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(path, outDir)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar"), strings.HasSuffix(lower, ".tar.bz2"),
		strings.HasSuffix(lower, ".tar.xz"):
		return extractTar(path, outDir)
	case strings.HasSuffix(lower, ".gz"), strings.HasSuffix(lower, ".bz2"), strings.HasSuffix(lower, ".xz"):
		// 单文件压缩（常见的 match.dem.gz）：gz 用标准库，bz2/xz 交给系统 tar
		if strings.HasSuffix(lower, ".gz") {
			return extractGz(path, outDir)
		}
		return extractWithSystemTar(path, outDir)
	default:
		// rar / 7z
		return extractWithSystemTar(path, outDir)
	}
}

// extractZip 解压 zip 中体积最大的 .dem。
func extractZip(path, outDir string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer r.Close()
	var best *zip.File
	for _, f := range r.File {
		if f.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(f.Name), ".dem") {
			continue
		}
		if best == nil || f.UncompressedSize64 > best.UncompressedSize64 {
			best = f
		}
	}
	if best == nil {
		return "", fmt.Errorf("压缩包内未找到 .dem 文件: %s", filepath.Base(path))
	}
	if best.UncompressedSize64 > maxExtractBytes {
		return "", fmt.Errorf("压缩包内 %s 解压后超过 8GB，已中止", best.Name)
	}
	rc, err := best.Open()
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", best.Name, err)
	}
	defer rc.Close()
	dst := filepath.Join(outDir, filepath.Base(best.Name))
	if err := writeLimitedFile(dst, rc); err != nil {
		return "", err
	}
	return dst, nil
}

// extractGz 解压单文件 gzip（match.dem.gz → match.dem）。
func extractGz(path, outDir string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	gz, err := gzip.NewReader(src)
	if err != nil {
		return "", fmt.Errorf("打开 gzip 失败: %w", err)
	}
	defer gz.Close()
	name := filepath.Base(path)
	name = name[:len(name)-len(".gz")]
	if !strings.EqualFold(filepath.Ext(name), ".dem") {
		name += ".dem"
	}
	dst := filepath.Join(outDir, name)
	if err := writeLimitedFile(dst, gz); err != nil {
		return "", err
	}
	return dst, nil
}

// extractTar 流式解压 tar(.gz/.bz2/.xz) 中第一个 .dem（tar 流不可回退，
// 不做 zip 那样的"选最大"；对战 demo 压缩包通常只含一个 .dem）。
func extractTar(path, outDir string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	var rd io.Reader = src
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return "", fmt.Errorf("打开 gzip 失败: %w", err)
		}
		defer gz.Close()
		rd = gz
	} else if strings.HasSuffix(lower, ".tar.bz2") || strings.HasSuffix(lower, ".tar.xz") {
		return extractWithSystemTar(path, outDir)
	}
	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("读取 tar 失败: %w", err)
		}
		if hdr.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(hdr.Name), ".dem") {
			continue
		}
		if hdr.Size > maxExtractBytes {
			return "", fmt.Errorf("压缩包内 %s 解压后超过 8GB，已中止", hdr.Name)
		}
		dst := filepath.Join(outDir, filepath.Base(hdr.Name))
		if err := writeLimitedFile(dst, tr); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("压缩包内未找到 .dem 文件: %s", filepath.Base(path))
}

// extractWithSystemTar 用 Windows 自带的 tar.exe 解压 rar/7z/bz2/xz。
// Win10 1803+ 内置 bsdtar；Win11 的 libarchive 可直接读 rar/7z。
func extractWithSystemTar(path, outDir string) (string, error) {
	tarExe := systemTar()
	if tarExe == "" {
		return "", fmt.Errorf("系统未提供 tar.exe，无法解压 %s；请手动解压后选择 .dem", filepath.Base(path))
	}
	cmd := exec.Command(tarExe, "-xf", path, "-C", outDir)
	cmd.SysProcAttr = winutil.NoWindow()
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("解压 %s 失败（系统 tar.exe 可能不支持该格式，请手动解压后选择 .dem）: %v\n%s",
			filepath.Base(path), err, strings.TrimSpace(string(out)))
	}
	if dem := findLargestDem(outDir); dem != "" {
		return dem, nil
	}
	return "", fmt.Errorf("压缩包内未找到 .dem 文件: %s", filepath.Base(path))
}

// systemTar 返回系统 tar.exe 路径（Windows 优先 System32，避免 PATH 里的
// Git/MSYS tar 干扰），找不到时返回空。
func systemTar() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "tar.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("tar"); err == nil {
		return p
	}
	return ""
}

// findLargestDem 递归查找目录下体积最大的 .dem，找不到返回空。
func findLargestDem(dir string) string {
	var best string
	var bestSize int64 = -1
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".dem") {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > bestSize {
			best, bestSize = p, info.Size()
		}
		return nil
	})
	return best
}

// writeLimitedFile 把 r 写入 dst，超过解压上限时中止并删除半成品。
func writeLimitedFile(dst string, r io.Reader) error {
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(r, maxExtractBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("解压写入失败: %w", err)
	}
	if n > maxExtractBytes {
		os.Remove(dst)
		return fmt.Errorf("解压后文件超过 8GB，已中止")
	}
	return nil
}
