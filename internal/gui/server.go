// Package gui 提供本地 Web 图形界面：
// 双击 cs2hi.exe（或 cs2hi gui）启动内置 HTTP 服务并自动打开浏览器，
// 用户在浏览器中选择 demo、指定玩家、勾选集锦片段、配置渲染参数并查看进度。
package gui

import (
	"embed"
	"fmt"
	webview "github.com/jchv/go-webview2"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// Run 启动 GUI 服务（阻塞直到服务停止）。
// port 为 0 时自动选择可用端口；native 为 true 时打开原生 WebView2 窗口
// （否则打开系统默认浏览器）。WebView2 Runtime 为 Win10/11 自带，
// WebView2Loader.dll 内嵌于二进制，保持单文件分发。
func Run(port int, native bool) error {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// 静态资源加 no-cache：WebView2 会缓存 CSS/JS，更新界面必须禁用缓存
	fileSrv := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		fileSrv.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/api/parse", handleParse)
	mux.HandleFunc("/api/detect", handleDetect)
	mux.HandleFunc("/api/render", handleRender)
	mux.HandleFunc("/api/task", handleTask)
	mux.HandleFunc("/api/cancel", handleCancel)
	mux.HandleFunc("/api/video", handleVideo)
	mux.HandleFunc("/api/opendir", handleOpenDir)
	mux.HandleFunc("/api/browse", handleBrowse)
	mux.HandleFunc("/api/videos", handleVideos)
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/api/thumb", handleThumb)

	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("启动 GUI 服务失败: %w", err)
	}
	url := fmt.Sprintf("http://%s", ln.Addr())

	if native && runtime.GOOS == "windows" {
		// 原生窗口：HTTP 服务在 goroutine 中运行，主线程进入窗口消息循环
		redirectOutput()
		go srv.Serve(ln)
		fmt.Printf("cs2hi 图形界面已启动（原生窗口）%s\n", url)
		runWindow(url)
		return nil
	}
	fmt.Printf("cs2hi 图形界面已启动: %s\n（浏览器未自动打开时请手动访问该地址）\n", url)
	openBrowser(url)
	return srv.Serve(ln)
}

// runWindow 打开原生 WebView2 窗口并进入消息循环（阻塞直到窗口关闭）。
func runWindow(url string) {
	w := webview.NewWithOptions(webview.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview.WindowOptions{
			Title:  "cs2hi — CS2 集锦工坊",
			Width:  1150,
			Height: 850,
			Center: true,
			IconId: 1, // exe 内嵌图标资源（app.ico 经 go-winres 嵌入，RT_ICON ID 1）
		},
	})
	if w == nil {
		fmt.Println("原生窗口初始化失败，请在浏览器中打开界面")
		openBrowser(url)
		select {} // 保持服务存活
	}
	w.SetSize(1150, 850, webview.HintNone)
	w.Navigate(url)
	w.Run()
}

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procSetStdHandle = kernel32.NewProc("SetStdHandle")

	// guiLogFile 持有日志文件引用，防止被 GC 关闭后标准句柄悬空。
	guiLogFile *os.File
)

const (
	stdOutputHandle = 0xFFFFFFF5 // (DWORD)-11
	stdErrorHandle  = 0xFFFFFFF4 // (DWORD)-12
)

// redirectOutput 把进程的标准输出/错误重定向到 cs2hi_gui.log。
//
// 本程序以 GUI 子系统（-H=windowsgui）构建，没有控制台：Go 运行时对
// panic / fatal error 的堆栈、依赖库的 log.Fatal、渲染管线的 fmt.Println
// 默认全部丢弃。渲染进程一旦异常终止，用户只看到窗口凭空消失，无从诊断
// （曾出现音频会话代码踩坏栈导致 fatal error: unknown caller pc 的闪退）。
// 日志写在 exe 同目录（便携工具目录可写），不可写时退回系统临时目录。
func redirectOutput() {
	dir := exeDir()
	path := filepath.Join(dir, "cs2hi_gui.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		path = filepath.Join(os.TempDir(), "cs2hi_gui.log")
		if f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err != nil {
			return
		}
	}
	// 运行时输出（fatal error / panic）走 GetStdHandle(STD_ERROR_HANDLE)，
	// 必须改进程标准句柄，仅替换 os.Stdout/os.Stderr 变量不够。
	h := uintptr(f.Fd())
	procSetStdHandle.Call(stdOutputHandle, h)
	procSetStdHandle.Call(stdErrorHandle, h)
	os.Stdout = f
	os.Stderr = f
	log.SetOutput(f)
	guiLogFile = f
	fmt.Printf("=== cs2hi GUI 启动 %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
}

// openBrowser 用系统默认浏览器打开 URL（失败静默）。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// 延迟打开，等服务就绪
	go func() {
		time.Sleep(300 * time.Millisecond)
		cmd.Start()
	}()
}
