// Package render 实现自动化渲染流水线：
//
//	生成 HLAE 配置 → 启动 HLAE + CS2 自动录制 → 等待录制结束 → FFmpeg 合成成片。
//
// 工具链（HLAE、FFmpeg）为绿色便携版，缺失时自动下载到 <工具根目录>/tools/。
package render

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"cs2-highlights/internal/highlight"
	"cs2-highlights/internal/winutil"
)

// ErrCancelled 用户主动取消渲染时 Run 返回的错误（GUI 据此显示"已取消"而非"失败"）。
var ErrCancelled = errors.New("渲染已取消（已录制的片段保留在输出目录）")

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

// screenSize 返回主屏幕分辨率（宽, 高），失败时回退 1280x720。
func screenSize() (int, int) {
	w, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	h, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
	if w == 0 || h == 0 {
		return 1280, 720
	}
	return int(w), int(h)
}

const (
	hlaeReleaseAPI = "https://api.github.com/repos/advancedfx/advancedfx/releases/latest"
	ffmpegURL      = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	dlTimeout      = 30 * time.Minute
)

// Options 是一次渲染任务的参数。
type Options struct {
	DemoPath            string
	PlayerName          string
	OtherPlayerName     string // 预测状态软重置用的中转玩家（任意非目标玩家，切走再切回清零 TrueSpectate 累积）
	PlayerEntityID      int    // 目标玩家实体索引（spec_player 数字参数，不受名字空格/中文/特殊字符影响）
	OtherPlayerEntityID int    // 中转玩家实体索引
	PlayerID            uint64 // 目标玩家 SteamID64
	AccountID           uint32 // 目标玩家 SteamID32（CS2 spec_lock_to_accountid 锁定观战用）
	Clips               []highlight.Clip
	TickRate            float64
	OutDir              string
	Resolution          string // "宽x高"，如 "1280x720"
	FPS                 int
	CRF                 int
	Preset              string              // x264 编码预设：fast/medium/slow（默认 medium，体积/质量/时间平衡）
	HLAEPath            string              // 空 = 自动检测/下载
	CS2Path             string              // 空 = 自动检测
	Server              string              // 启动服务器选择: "pw"（完美世界国服）/ "worldwide"（国际服）/ ""（不指定，弹出选择框）
	View                string              // 录制视角: "pov"（第一人称）/ "third"（第三人称跟随）/ "default"（GOTV 导播视角）
	NoQuit              bool                // 录制结束后不自动退出游戏（用于调试）
	KeepRaw             bool                // 原始分片段视频保留在游戏目录（默认移动到输出目录 raw/）
	MaxPerPass          int                 // 每轮最多录制的片段数（0 = 不限制）。限制轮次可降低 TrueSpectate 死锁概率
	ReloadSplit         bool                // 实验性：每 MaxPerPass 段后 disconnect+重载 demo 代替重启游戏（游戏进程不退出）
	HideWindow          bool                // 游戏窗口移出屏幕后台渲染（GUI 默认开启，不打扰用户）
	Progress            func(ProgressEvent) // 进度回调（GUI 用；nil = 仅打印日志）
	Log                 func(string)        // 实时日志回调（GUI 任务日志用）。可能从多个 goroutine 调用，实现需并发安全；nil = 仅打印
	SkipConcat          bool                // 只输出分片段视频，不合成单条成片（节省时间，用户可后选合成）
}

// ProgressEvent 渲染进度事件。
type ProgressEvent struct {
	Stage string // "pass"（轮次）/ "record"（录制完成数）/ "encode"（编码）/ "concat"（合成）/ "warn"（警告）/ "done"（完成）
	Done  int    // 已完成的全局片段数
	Total int    // 全局片段总数
	Pass  int    // 当前轮次（1 起；0 = 未知）
	// ClipIndex 当前正在处理的全局片段编号（1 起；0 = 未知）。
	// 前端用它展示「全部中的第几个片段」，而不是容易误解的本轮序号。
	ClipIndex int
	Detail    string
	// Partial 当前录制中片段的进度（0~1，帧数/期望帧数）。
	// 前端进度条用 Done+Partial 平滑展示：避免 take 目录在录制开始时创建
	// 导致进度瞬间跳满、随后长时间停在 100% 的假象。
	Partial float64
	// Percent 总体进度（0~1，后端统一计算）。一轮渲染里录制与编码各占
	// 该轮的一半，保证录制结束进入编码时进度条不回退（早期按片段计数，
	// 编码阶段会从 100% 跳回 0%）。前端优先使用该值。
	Percent float64
}

// report 上报进度事件（Progress 为 nil 时静默）。
func report(opts *Options, ev ProgressEvent) {
	if opts != nil && opts.Progress != nil {
		opts.Progress(ev)
	}
}

// logf 输出一条渲染管线日志：同时写入 Options.Log（GUI 实时任务日志）
// 和 stdout（CLI / cs2hi_gui.log）。可能从进度、冻结监控等 goroutine 调用，
// Log 的实现必须并发安全。
func logf(opts *Options, format string, a ...any) {
	s := strings.TrimSuffix(fmt.Sprintf(format, a...), "\n")
	if opts != nil && opts.Log != nil {
		opts.Log(s)
	}
	fmt.Println(s)
}

// progressPercent 把「已完成片段当量 / 总片段数」转换成 0~1 的总体进度。
func progressPercent(done float64, total int) float64 {
	if total <= 0 {
		return 0
	}
	p := done / float64(total)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// Result 是渲染结果。
type Result struct {
	FinalVideo string   // 合集成片路径
	ClipVideos []string // 按分类归档的单回合视频路径
}

// ToolPaths 描述渲染工具链的路径。
type ToolPaths struct {
	HLAE    string // HLAE.exe
	HookDLL string // x64/AfxHookSource2.dll
	FFmpeg  string // ffmpeg.exe
	CS2     string // cs2.exe
}

// EnsureTools 检查工具链，缺失时自动下载到 <root>/tools/。
func EnsureTools(root string) (*ToolPaths, error) {
	toolsDir := filepath.Join(root, "tools")
	hlaeExe := filepath.Join(toolsDir, "hlae", "HLAE.exe")
	hookDll := filepath.Join(toolsDir, "hlae", "x64", "AfxHookSource2.dll")
	ffmpegExe := filepath.Join(toolsDir, "hlae", "ffmpeg", "ffmpeg.exe")

	if _, err := os.Stat(hlaeExe); err != nil {
		fmt.Println("缺少 HLAE，开始自动下载（约 9MB，仅首次需要）...")
		if err := downloadHLAE(toolsDir); err != nil {
			return nil, fmt.Errorf("下载 HLAE 失败: %w", err)
		}
	}
	if _, err := os.Stat(ffmpegExe); err != nil {
		fmt.Println("缺少 FFmpeg，开始自动下载（约 110MB，仅首次需要）...")
		if err := downloadFFmpeg(toolsDir); err != nil {
			return nil, fmt.Errorf("下载 FFmpeg 失败: %w", err)
		}
	}
	// HLAE 不同版本解析 ffmpeg.exe 的基准目录不同（HLAE 根 / x64 钩子目录），多放几个副本兜底
	for _, dst := range []string{
		filepath.Join(toolsDir, "hlae", "ffmpeg.exe"),
		filepath.Join(toolsDir, "hlae", "x64", "ffmpeg", "ffmpeg.exe"),
	} {
		if _, err := os.Stat(dst); err != nil {
			os.MkdirAll(filepath.Dir(dst), 0o755)
			copyFile(ffmpegExe, dst)
		}
	}
	cs2, err := FindCS2()
	if err != nil {
		return nil, err
	}
	return &ToolPaths{HLAE: hlaeExe, HookDLL: hookDll, FFmpeg: ffmpegExe, CS2: cs2}, nil
}

// steamInstallRoots 返回 Steam 主程序安装根目录（注册表 + 默认路径）。
// 游戏的个人设置（userdata\<账号>\730\local\cfg\）只存在于主安装目录下，
// libraryfolders.vdf 里的其他盘游戏库不含 userdata。
func steamInstallRoots() []string {
	var roots []string
	cmd := exec.Command("reg", "query", `HKLM\SOFTWARE\WOW6432Node\Valve\Steam`, "/v", "InstallPath")
	cmd.SysProcAttr = winutil.NoWindow()
	if out, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "InstallPath" {
				roots = append(roots, strings.Join(fields[2:], " "))
			}
		}
	}
	return append(roots, `C:\Program Files (x86)\Steam`)
}

// cs2SearchRoots 返回查找 cs2.exe 的候选根目录：Steam 安装目录 + libraryfolders.vdf 中的全部游戏库。
func cs2SearchRoots() []string {
	libs := map[string]bool{}
	pathRe := regexp.MustCompile(`"path"\s+"((?:[^"\\]|\\.)*)"`)
	for _, root := range steamInstallRoots() {
		libs[root] = true
		vdf, err := os.ReadFile(filepath.Join(root, "steamapps", "libraryfolders.vdf"))
		if err != nil {
			continue
		}
		for _, m := range pathRe.FindAllSubmatch(vdf, -1) {
			p := strings.ReplaceAll(string(m[1]), `\\`, `\`)
			if p != "" {
				libs[p] = true
			}
		}
	}
	roots := make([]string, 0, len(libs))
	for lib := range libs {
		roots = append(roots, lib)
	}
	return roots
}

// FindCS2 自动探测 CS2 的 cs2.exe：Steam 安装目录 + libraryfolders.vdf 中的游戏库。
func FindCS2() (string, error) {
	for _, root := range cs2SearchRoots() {
		exe := filepath.Join(root, "steamapps", "common", "Counter-Strike Global Offensive", "game", "bin", "win64", "cs2.exe")
		if _, err := os.Stat(exe); err == nil {
			return exe, nil
		}
	}
	return "", fmt.Errorf("未找到 CS2 安装（cs2.exe）。请确认 Steam 已安装 CS2，或用 --cs2 参数指定路径")
}

// ---- 游戏视频设置备份/还原 ----
//
// 渲染用 -w -h 指定分辨率启动游戏，游戏退出时会把该分辨率写回
// cs2_video.txt（userdata\<账号>\730\local\cfg\），用户之后真实进游戏时
// 默认分辨率被篡改。渲染开始前备份全部账号的设置文件（原样字节拷贝），
// 渲染结束统一还原。备份文件留盘：若上次渲染中途崩溃（进程被杀）没来得及
// 还原，下次渲染开始时检测到遗留备份会先自动还原，实现自愈。

// videoBakSuffix 视频设置备份文件后缀。
const videoBakSuffix = ".cs2hi.bak"

// videoSettingBackup 一份视频设置文件的备份信息。
type videoSettingBackup struct {
	path    string
	existed bool // 备份时文件是否存在；false = 渲染期间新建的文件，还原时删除
}

// backupVideoSettings 备份所有 Steam 账号的 CS2 视频设置文件。
func backupVideoSettings() []videoSettingBackup {
	var backups []videoSettingBackup
	for _, cfgDir := range userCfgDirs() {
		for _, name := range []string{"cs2_video.txt", "video.txt"} {
			p := filepath.Join(cfgDir, name)
			bak := p + videoBakSuffix
			if data, err := os.ReadFile(bak); err == nil {
				// 上次渲染崩溃遗留的备份：当前文件很可能已被游戏写成渲染分辨率 → 先还原
				os.WriteFile(p, data, 0o644)
				os.Remove(bak)
			}
			if data, err := os.ReadFile(p); err == nil {
				if os.WriteFile(bak, data, 0o644) == nil {
					backups = append(backups, videoSettingBackup{path: p, existed: true})
				}
			} else {
				backups = append(backups, videoSettingBackup{path: p, existed: false})
			}
		}
	}
	return backups
}

// patchVideoSettings 渲染期间把游戏视频设置强制为「有边框窗口 + 录制分辨率」。
// 用户设置常见 nowindowborder 1（无边框窗口铺满屏幕，视觉等同全屏）：
// 无边框窗口会持续拉回屏幕，无视 HideWindow 的移出屏幕操作，用户看到游戏"全屏"打开；
// 窗口位置拉锯还可能是录制中引擎崩溃的诱因。写入发生在备份之后，
// 渲染结束由 restoreVideoSettings 统一还原（含崩溃遗留备份的自愈路径）。
func patchVideoSettings(backups []videoSettingBackup, w, h int) {
	re := []struct {
		re   *regexp.Regexp
		repl string
	}{
		{regexp.MustCompile(`("setting\.fullscreen"\s+")\d+(")`), `$1` + "0" + `$2`},
		{regexp.MustCompile(`("setting\.coop_fullscreen"\s+")\d+(")`), `$1` + "0" + `$2`},
		{regexp.MustCompile(`("setting\.nowindowborder"\s+")\d+(")`), `$1` + "0" + `$2`},
		{regexp.MustCompile(`("setting\.defaultres"\s+")\d+(")`), `$1` + strconv.Itoa(w) + `$2`},
		{regexp.MustCompile(`("setting\.defaultresheight"\s+")\d+(")`), `$1` + strconv.Itoa(h) + `$2`},
	}
	for _, b := range backups {
		if !b.existed {
			continue // 渲染前不存在（全新账号），无法 patch
		}
		data, err := os.ReadFile(b.path)
		if err != nil {
			continue
		}
		orig := string(data)
		for _, r := range re {
			orig = r.re.ReplaceAllString(orig, r.repl)
		}
		if orig != string(data) {
			os.WriteFile(b.path, []byte(orig), 0o644)
		}
	}
}

// restoreVideoSettings 渲染结束后还原备份的视频设置，返回实际还原/删除的文件数。
func restoreVideoSettings(backups []videoSettingBackup) int {
	n := 0
	for _, b := range backups {
		bak := b.path + videoBakSuffix
		if !b.existed {
			// 渲染前不存在（全新账号首次启动游戏会生成）：删除以还原原始状态
			if _, err := os.Stat(b.path); err == nil {
				os.Remove(b.path)
				n++
			}
			continue
		}
		data, err := os.ReadFile(bak)
		if err != nil {
			continue
		}
		cur, errCur := os.ReadFile(b.path)
		if errCur != nil || !bytes.Equal(cur, data) {
			os.WriteFile(b.path, data, 0o644)
			n++
		}
		os.Remove(bak)
	}
	return n
}

// userCfgDirs 返回所有 Steam 账号的 CS2 本地配置目录（多账号全覆盖）。
func userCfgDirs() []string {
	var dirs []string
	for _, root := range steamInstallRoots() {
		entries, err := os.ReadDir(filepath.Join(root, "userdata"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dirs = append(dirs, filepath.Join(root, "userdata", e.Name(), "730", "local", "cfg"))
		}
	}
	return dirs
}

// ---- 游戏音量备份/还原 ----
//
// 当前管线不再修改游戏音量：录制需要真实音量（volume 0 会把 audio.wav
// 一起静音），而录制期间扬声器输出本身由引擎/HLAE 静音。这里的备份/还原
// 保留作兜底：若配置再次引入音量改动，volume 作为持久化 cvar 会被游戏
// 退出写回 userdata\<账号>\730\local\cfg\cs2_machine_convars.vcfg，需要
// 还原；旧版本遗留的 .cs2hi.vol.bak 也会在渲染启动时先自愈。

// volumeBakSuffix 音量备份文件后缀。
const volumeBakSuffix = ".cs2hi.vol.bak"

// volumeLineRe 匹配 machine convars 中的 volume 行。key 用完整引号定界，
// 不会误伤 snd_gamevolume / dsp_volume 等名字里带 volume 的其它项。
var volumeLineRe = regexp.MustCompile(`("volume"\s+")([0-9.]+)(")`)

// volumeValueRe 合法音量值（CS2 的 volume 为 0~1 的小数，也接受整数写法）。
var volumeValueRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// volumeSettingBackup 一份音量设置备份。
type volumeSettingBackup struct {
	path string // cs2_machine_convars.vcfg 路径
	val  string // 渲染前的音量值
}

// cfgVolumeValue 从 vcfg 内容中提取 volume 值。
func cfgVolumeValue(data []byte) (string, bool) {
	m := volumeLineRe.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	return string(m[2]), true
}

// setCfgVolumeValue 把 vcfg 内容中的 volume 值改为 val；未找到或 val 非法时原样返回。
func setCfgVolumeValue(data []byte, val string) []byte {
	if !volumeValueRe.MatchString(val) {
		return data
	}
	m := volumeLineRe.FindSubmatchIndex(data)
	if m == nil {
		return data
	}
	// 只替换值本身，保留原有的引号与空白排版
	out := make([]byte, 0, len(data)+len(val))
	out = append(out, data[:m[4]]...)
	out = append(out, val...)
	out = append(out, data[m[5]:]...)
	return out
}

// backupVolumeSettings 备份所有 Steam 账号的音量设置。
func backupVolumeSettings() []volumeSettingBackup {
	var backups []volumeSettingBackup
	for _, cfgDir := range userCfgDirs() {
		p := filepath.Join(cfgDir, "cs2_machine_convars.vcfg")
		bak := p + volumeBakSuffix
		if orig, err := os.ReadFile(bak); err == nil {
			// 上次渲染中断遗留的备份：当前文件很可能已被游戏写成 volume 0 → 先还原
			if data, err := os.ReadFile(p); err == nil {
				if out := setCfgVolumeValue(data, strings.TrimSpace(string(orig))); !bytes.Equal(out, data) {
					os.WriteFile(p, out, 0o644)
				}
			}
			os.Remove(bak)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		val, ok := cfgVolumeValue(data)
		if !ok {
			continue
		}
		if os.WriteFile(bak, []byte(val), 0o644) == nil {
			backups = append(backups, volumeSettingBackup{path: p, val: val})
		}
	}
	return backups
}

// restoreVolumeSettings 渲染结束后还原音量设置，返回实际还原的文件数。
func restoreVolumeSettings(backups []volumeSettingBackup) int {
	n := 0
	for _, b := range backups {
		if data, err := os.ReadFile(b.path); err == nil {
			if out := setCfgVolumeValue(data, b.val); !bytes.Equal(out, data) {
				os.WriteFile(b.path, out, 0o644)
				n++
			}
		}
		os.Remove(b.path + volumeBakSuffix)
	}
	return n
}

// downloadHLAE 通过 GitHub API 下载最新 HLAE 便携版并解压到 tools/hlae/。
func downloadHLAE(toolsDir string) error {
	var rel struct {
		Assets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := fetchJSON(hlaeReleaseAPI, &rel); err != nil {
		return err
	}
	var assetID int64
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, "hlae_") && strings.HasSuffix(a.Name, ".zip") {
			assetID = a.ID
			break
		}
	}
	if assetID == 0 {
		return fmt.Errorf("未在 GitHub 发布页找到 HLAE 下载包")
	}
	zipPath := filepath.Join(toolsDir, "hlae.zip")
	url := fmt.Sprintf("https://api.github.com/repos/advancedfx/advancedfx/releases/assets/%d", assetID)
	if err := downloadFile(zipPath, url, "application/octet-stream"); err != nil {
		return err
	}
	defer os.Remove(zipPath)
	return unzip(zipPath, filepath.Join(toolsDir, "hlae"))
}

// downloadFFmpeg 下载 FFmpeg essentials 并放入 HLAE 的 ffmpeg 目录。
func downloadFFmpeg(toolsDir string) error {
	zipPath := filepath.Join(toolsDir, "ffmpeg.zip")
	if err := downloadFile(zipPath, ffmpegURL, ""); err != nil {
		return err
	}
	defer os.Remove(zipPath)
	tmp := filepath.Join(toolsDir, "ffmpeg-tmp")
	defer os.RemoveAll(tmp)
	if err := unzip(zipPath, tmp); err != nil {
		return err
	}
	var ffmpegExe string
	filepath.Walk(tmp, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.EqualFold(info.Name(), "ffmpeg.exe") {
			ffmpegExe = path
		}
		return nil
	})
	if ffmpegExe == "" {
		return fmt.Errorf("FFmpeg 压缩包中未找到 ffmpeg.exe")
	}
	dst := filepath.Join(toolsDir, "hlae", "ffmpeg", "ffmpeg.exe")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	src, err := os.Open(ffmpegExe)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// quitAfterRenderSuffix 是每轮最后一段录制结束后的收尾命令。
//
// 不能在 record end 后直接 quit：Windows 会按应用（exe 路径）持久化音频会话
// 的静音状态（实测同一 exe 再次启动时 GetMute 返回上次退出时的值），而引擎从
// 控制台日志出现 "Host activate: Quitting" 到进程退出只有几十毫秒——实测
// Quitting / MainLoop returning / Source2Shutdown 落在同一秒，外部进程来不及
// 在会话销毁前解除静音，结果就是「渲染完成后正常进游戏，游戏没声音」。
//
// 所以在 quit 前先暂停 demo（等待期不外放声音）再等待约 1.5 秒。watchGameQuit
// 一旦看到录制结束日志就能在游戏仍存活时解除静音，Windows 随后把 false 写回
// 持久化状态，下次进游戏不受影响。
//
// wait 的单位是帧，按 fps 折算成约 1.5 秒（120fps 下固定 90 帧只有 0.75 秒，
// 余量太薄）。
func quitAfterRenderSuffix(fps int) string {
	frames := int(1.5 * float64(fps))
	if frames < 30 {
		frames = 30
	}
	return fmt.Sprintf("; demo_pause; wait %d; quit", frames)
}

// GenerateCfgs 生成两阶段 HLAE 配置并写入游戏的 csgo\cfg\ 目录（CS2 的 exec
// 不支持绝对路径，只能 exec cfg 目录下的相对文件名），同时存一份副本到输出目录。
//
//	auto.cfg（启动时 exec）：playdemo 载入 demo，demo tick 5 时跳转到第一个片段前 2 秒
//	record.cfg（跳转后 exec）：按片段边界用 mirv_cmd addAtTick 自动起停录制，录完退出游戏
//
// 关键经验（在真实国服 CS2 + HLAE 2.192 上验证）：
//   - mirv_cmd 的 tick 在 demo 播放中 = demo tick（从 0 起），队列跨 demo 加载存活
//   - playdemo 必须放在 cfg 内（命令行 +playdemo 会打乱 mirv_cmd 的时序）
//   - 游戏窗口无焦点时引擎会休眠降速，必须 engine_no_focus_sleep 0
//   - CS2 版 mirv_streams 用 settings add ffmpeg 命名预设设置编码，需 record screen enabled 1
func GenerateCfgs(cs2Path, outDir, demoPath string, opts Options) (gameCfgDir string, err error) {
	tickRate := opts.TickRate
	if tickRate <= 0 {
		tickRate = 64
	}
	// cs2.exe 在 game\bin\win64\，cfg 目录在 game\csgo\cfg\
	gameCfgDir = filepath.Join(filepath.Dir(cs2Path), "..", "..", "csgo", "cfg")

	// 片段按时间顺序排列（mirv_cmd 在 seek 跳过的区间内会立即触发所有待执行命令，
	// 因此必须按时间递增链式录制，跳转点之间不能残留其他片段的录制命令）
	chrono := make([]highlight.Clip, len(opts.Clips))
	copy(chrono, opts.Clips)
	sort.Slice(chrono, func(i, j int) bool { return chrono[i].StartTick < chrono[j].StartTick })

	// seek0 必须取时间顺序第一段（否则时间在 seek 点之后的过期命令会被立即触发产生空录制）。
	// seek 提前 6 秒：观战锁定有约 2 秒延迟，需要给镜头留出切换时间。
	seek0 := 0
	if len(chrono) > 0 {
		seek0 = chrono[0].StartTick - int(7*tickRate)
		if seek0 < 0 {
			seek0 = 0
		}
	}

	// ReloadSplit：每 MaxPerPass 段后 disconnect 回主菜单并重新 playdemo，
	// 用 demo 重载代替游戏重启清零累积状态（实验性，游戏进程不退出）。
	// autoN.cfg 在重载后 exec：mirv_cmd clear 后重建剩余片段的全部命令
	// （playdemo 重新解析整个 demo，full packet 立即可用，tick 5 的 seek 链正常触发）。
	type reloadCfg struct {
		name string
		from int // 重载后从第 from 个片段开始
	}
	var reloadCfgs []reloadCfg
	reloadEvery := 0
	if opts.ReloadSplit && opts.MaxPerPass > 0 {
		reloadEvery = opts.MaxPerPass
	}

	var b strings.Builder
	b.WriteString("// 由 cs2hi 自动生成 —— 按片段边界自动录屏（时间顺序链式跳转）\n")
	b.WriteString("// 屏幕录制输出 TGA 原始帧序列 + audio.wav（HLAE 内置 ffmpeg 管道在本机不可用），由 cs2hi 事后编码\n")
	// TrueView 预测回放在 demo_gototick 跳转后会丢失命令数据，第一人称镜头卡死在
	// 跳转点（时间/击杀正常推进但视角不跟随玩家）。渲染全程关闭预测回放。
	b.WriteString("cl_demo_predict 0\n")
	b.WriteString("cl_spectator_predict 0\n")
	b.WriteString("cl_trueview_show_status 0\n")
	// 不能用 volume 0 静音：引擎录制的是最终混音，音量 0 会让 audio.wav 也是静音。
	// 渲染期间的外放静音由 audio.go 的 Windows 音频会话静音负责（只静音播放）。
	b.WriteString("host_framerate " + fmt.Sprintf("%d", opts.FPS) + " // 限速实时渲染：观战系统同步需要墙钟时间，超速渲染会死锁 TrueSpectate\n")
	b.WriteString("mirv_streams record fps " + fmt.Sprintf("%d", opts.FPS) + "\n")
	b.WriteString("mirv_streams record screen enabled 1 // CS2 必须显式启用画面录制，否则只有音频\n")
	b.WriteString("mirv_cmd clear\n")
	// 兜底 seek：若 auto.cfg 的 seek 在菜单期提前触发失败，demo 从头播放时在此再次跳转
	b.WriteString(fmt.Sprintf("mirv_cmd addAtTick 5 \"demo_gototick %d; demo_resume\"\n", seek0))
	// record.cfg 只生成第一轮的片段命令（后续轮次的命令由各自的 autoN.cfg 在重载时
	// 一次性入队——mirv_cmd clear 在 mirv_cmd 命令链中执行无效，靠"不重复入队"避免残留）。
	to0 := len(chrono)
	suffix := ""
	if !opts.NoQuit {
		suffix = quitAfterRenderSuffix(opts.FPS)
	}
	if reloadEvery > 0 && reloadEvery < len(chrono) {
		to0 = reloadEvery
		name := fmt.Sprintf("cs2hi_auto%d.cfg", 1+len(reloadCfgs))
		reloadCfgs = append(reloadCfgs, reloadCfg{name: name, from: reloadEvery})
		// 重载链不显式 disconnect（mirv_cmd 命令链会在 disconnect 时被中止，
		// 后续 exec 永不执行）。exec autoN 里的 playdemo 自己会断开当前 demo
		// 并重新加载。wait 300 在 demo 播放中给录制收尾留缓冲。
		suffix = fmt.Sprintf("; wait 300; exec %s", name)
	}
	b.WriteString(genClipCmds(chrono, 0, to0, opts, tickRate, suffix))
	recordContent := b.String()

	// 重载阶段的 autoN.cfg：本轮片段命令 + tick 5 的 seek 链（带渲染参数重设）
	// + playdemo。每轮 demo 会话的命令只入队一次，无重复无残留。
	reloadContents := map[string]string{}
	for i, rc := range reloadCfgs {
		to := rc.from + reloadEvery
		if to > len(chrono) {
			to = len(chrono)
		}
		suffix := ""
		if !opts.NoQuit {
			suffix = quitAfterRenderSuffix(opts.FPS)
		}
		if i+1 < len(reloadCfgs) {
			suffix = fmt.Sprintf("; wait 300; exec %s", reloadCfgs[i+1].name)
		}
		seek := chrono[rc.from].StartTick - int(7*tickRate)
		if seek < 0 {
			seek = 0
		}
		// demo 加载期间 host_framerate 1（几乎不渲染，引擎全力加载，实测加载期
		// 60fps 渲染会诱发死锁）；加载完成后的 tick 5 seek 链里恢复 host_framerate 60。
		reloadContents[rc.name] = fmt.Sprintf("// 由 cs2hi 自动生成 —— demo 重载阶段\n"+
			// TrueView 预测回放在 seek 后拿不到命令数据，关闭以免第一人称镜头卡死
			"cl_demo_predict 0\n"+
			"cl_spectator_predict 0\n"+
			"cl_trueview_show_status 0\n"+
			"%s"+
			"mirv_cmd addAtTick 5 \"demo_gototick %d; demo_resume; host_framerate %d; mirv_streams record fps %d; mirv_streams record screen enabled 1\"\n"+
			"host_framerate 1\n"+
			"playdemo \"%s\"\n",
			genClipCmds(chrono, rc.from, to, opts, tickRate, suffix), seek, opts.FPS, opts.FPS,
			strings.ReplaceAll(opts.DemoPath, "/", `\`))
	}

	// playdemo 必须放在 cfg 内执行（路径用反斜杠 + 引号）。
	// 注意：不能加 -accountid 参数——实测它会破坏观战系统（相机卡在地图底下），
	// 视角锁定由 record.cfg 里的 spec_player + spec_mode 完成（锁定跨 seek 保持）。
	// demo_ui_mode 0 在 playdemo 前设置，隐藏 demo 播放控制条。
	// demo 路径必须转绝对路径：CS2 的工作目录在游戏目录，相对路径会 playdemo 失败。
	demoAbs := opts.DemoPath
	if a, err := filepath.Abs(demoAbs); err == nil {
		demoAbs = a
	}
	demoBackslash := strings.ReplaceAll(demoAbs, "/", `\`)
	autoContent := fmt.Sprintf("// 由 cs2hi 自动生成 —— 阶段1：载入 demo 并在 demo tick 5 时跳转\n"+
		// 不用 -nosound（音频系统未初始化时音频录制会在 soundsystem.dll 崩溃），
		// 也不能用 volume 0（会把录制的 audio.wav 一起静音）。
		// 关闭 TrueView 预测回放：seek 后预测拿不到命令数据，第一人称镜头会卡在跳转点。
		"cl_demo_predict 0\n"+
		"cl_spectator_predict 0\n"+
		"cl_trueview_show_status 0\n"+
		"demo_ui_mode 0 // 隐藏 demo 播放控制条\n"+
		"mirv_cmd clear\n"+
		"mirv_cmd addAtTick 5 \"demo_gototick %d; demo_resume; exec cs2hi_record.cfg\"\n"+
		"engine_no_focus_sleep 0 // 窗口无焦点时保持全速，否则后台录制极慢\n"+
		"playdemo \"%s\"\n", seek0, demoBackslash)

	// 写入游戏 cfg 目录（exec 相对名从该目录加载）
	if err := os.MkdirAll(gameCfgDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(gameCfgDir, "cs2hi_auto.cfg"), []byte(autoContent), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(gameCfgDir, "cs2hi_record.cfg"), []byte(recordContent), 0o644); err != nil {
		return "", err
	}
	for name, content := range reloadContents {
		if err := os.WriteFile(filepath.Join(gameCfgDir, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	// 副本存到输出目录供查看
	os.WriteFile(filepath.Join(outDir, "cs2hi_auto.cfg"), []byte(autoContent), 0o644)
	os.WriteFile(filepath.Join(outDir, "cs2hi_record.cfg"), []byte(recordContent), 0o644)
	for name, content := range reloadContents {
		os.WriteFile(filepath.Join(outDir, name), []byte(content), 0o644)
	}
	return gameCfgDir, nil
}

// genClipCmds 生成 clips[from:to] 的 mirv_cmd 录制命令链（时间顺序）。
// lastSuffix 是最后一个片段 endCmd 的追加后缀（quit 或 demo 重载链）。
//
// 时序设计（实测验证）：seek 提前 7 秒；观战锁定在 seek 后 2 秒单独调度
// （紧跟 seek 的观战命令会被 Demo Skipping 机制吞掉，必须错开）；每个片段在
// seek 后 4 秒再锁定一次（TrueSpectate 的初始激活/重建会抢先首次锁定，双保险）；
// 开录在 seek 后 6 秒，与最后一次锁定间隔至少 2 秒——观战同步期间开录会死锁 TrueSpectate。
func genClipCmds(clips []highlight.Clip, from, to int, opts Options, tickRate float64, lastSuffix string) string {
	viewCmds := viewSetupCommands(opts)
	var b strings.Builder
	for i := from; i < to; i++ {
		c := clips[i]
		seekI := c.StartTick - int(7*tickRate)
		if seekI < 0 {
			seekI = 0
		}
		lockTick := seekI + int(2*tickRate)
		recordStart := c.StartTick - int(1*tickRate)
		if recordStart < 0 {
			recordStart = 0
		}
		if len(viewCmds) > 0 {
			lockCmds := strings.Join(viewCmds, "; ")
			b.WriteString(fmt.Sprintf("mirv_cmd addAtTick %d \"%s\"\n", lockTick, lockCmds))
			b.WriteString(fmt.Sprintf("mirv_cmd addAtTick %d \"%s\"\n", lockTick+int(2*tickRate), lockCmds))
		}
		b.WriteString(fmt.Sprintf("mirv_cmd addAtTick %d \"mirv_streams record start\"\n", recordStart))
		// 片段间隙切 spec_mode 0（自由视角）：让 TrueView 真正关闭休息，给预测系统
		// 释放累积状态的机会（spec_player 切换无效——TrueView 1 秒内自动重建）。
		// 下一片段 seek 后 2 秒的锁定命令会重新 spec_mode 1 激活。
		endCmd := "mirv_streams record end"
		if opts.View == "pov" || opts.View == "third" {
			endCmd += "; spec_mode 0"
		}
		if i < to-1 {
			seekNext := clips[i+1].StartTick - int(7*tickRate)
			if seekNext < 0 {
				seekNext = 0
			}
			endCmd += fmt.Sprintf("; demo_gototick %d; demo_resume", seekNext)
		} else {
			endCmd += lastSuffix
		}
		b.WriteString(fmt.Sprintf("mirv_cmd addAtTick %d \"%s\"\n", c.EndTick, endCmd))
	}
	return b.String()
}

// viewSetupCommands 生成视角跟随与画面配置的指令序列。
//
// 在国服 CS2 + HLAE 2.192 上实测验证（2026-10-20）：
//   - playdemo 的 -accountid 参数会破坏观战系统（相机卡在地图底下），必须去掉
//   - spec_lock_to_accountid / spec_lock_to_current_player 无效
//   - 有效配方：spec_player <目标>（切目标）+ spec_mode 1（第一人称/2=第三人称）
//     目标一旦锁定会跨 seek 持续保持，无需重复锁定
//   - cl_drawhud 1 保留观战 HUD（小地图、当前视角玩家名横幅）
//   - demo_ui_mode 0 隐藏 demo 播放控制条（锁定后设置，避免干扰相机初始化）
//
// TrueView 必须关闭（2026-10-31 实测）：CS2 2025-11 起默认启用 TrueView 预测回放，
// 它靠 demo 里的客户端命令流重建玩家视角。我们的管线大量使用 demo_gototick 跳转，
// 跳转后预测系统拿不到命令数据（控制台刷 "TrueView can't determine last executed
// command"、HUD 右上角显示「实况回放: 无数据」），第一人称镜头会永久卡在跳转点：
// 时间、击杀、HUD 都正常推进，但视角/人物不再跟随目标玩家移动和转向。
// cl_demo_predict 0 关闭 demo 回放的 TrueView；cl_spectator_predict 0 关闭
// spec_player 观战时的 TrueView（两个开关独立）；cl_trueview_show_status 0
// 隐藏右上角的「实况回放」状态文字。关闭后退回服务端快照驱动的经典观战相机。
func viewSetupCommands(opts Options) []string {
	if opts.View == "default" || opts.View == "" {
		// 导播视角也隐藏控制条，并兜底关闭 TrueView 状态文字
		return []string{"cl_drawhud 1", "cl_demo_predict 0", "cl_spectator_predict 0",
			"cl_trueview_show_status 0", "demo_ui_mode 0"}
	}
	mode := "1"
	if opts.View == "third" {
		mode = "2"
	}
	cmds := []string{"cl_drawhud 1", "cl_demo_predict 0", "cl_spectator_predict 0",
		"cl_trueview_show_status 0"}
	// 实测有效顺序：先 spec_player 切目标，再 spec_mode 切第一/第三人称
	// 预测状态软重置（实验）：TrueSpectate 死锁是预测状态随 seek 次数累积
	// 触发（实测连续 9 次 seek 后必挂，短轮次不挂）。每次锁定前把观战目标
	// 切到另一玩家再切回，强制预测系统重建，清零累积状态。
	target := specPlayerArg(opts.PlayerName, opts.PlayerEntityID)
	other := specPlayerArg(opts.OtherPlayerName, opts.OtherPlayerEntityID)
	if other != "" && target != "" {
		cmds = append(cmds, "spec_player "+other)
	}
	if target != "" {
		cmds = append(cmds, "spec_player "+target)
	}
	cmds = append(cmds, "spec_mode "+mode, "demo_ui_mode 0")
	return cmds
}

// specPlayerArg 生成 spec_player 的参数，优先使用玩家实体索引（纯数字）。
//
// 玩家名不可靠：CS2 demo 里玩家名普遍带前导空格（本机实测 " Hinson1985"、
// " b哥严父"），中文名也很常见；且名字要嵌进 mirv_cmd 的引号字符串里，
// 引号/分号还会截断命令链。旧版直接跳过含空格的玩家名，导致 cfg 里根本
// 没有 spec_player——镜头停在 GOTV 默认目标，录出来的不是所选玩家。
//
// 实体索引来自 demo 解析（玩家 controller 实体，整场稳定），与 HLAE 官方
// mirv_script_spec_lock 的锁定方式一致。索引缺失时回退到名字（去掉首尾空白，
// 带引号/分号/换行等无法安全表达的名字直接放弃，避免生成坏命令）。
func specPlayerArg(name string, entityID int) string {
	if entityID > 0 {
		return strconv.Itoa(entityID)
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\";\r\n") {
		return ""
	}
	return name
}

// Run 启动自动化渲染并等待完成，返回成片路径。
// CS2 的 TrueSpectate 预测扫描会随机死锁（每次 seek 后有小概率冻结），
// 因此按轮次执行：冻结后自动杀掉游戏、保留已录片段、重启继续录剩余的，
// 直到全部片段录制完成（断点续录）。
func Run(tp *ToolPaths, opts Options) (*Result, error) {
	cancelRequested.Store(false) // 新任务重置取消标记
	// 当前管线不改游戏音量（录制需要真实音量），备份/还原仅作兜底：
	// 旧版本遗留的音量备份会在这里先自愈；万一将来配置再次改动音量，
	// 游戏退出写回的持久化值也会在渲染结束（含失败/取消路径）统一还原。
	volBackups := backupVolumeSettings()
	defer func() {
		if n := restoreVolumeSettings(volBackups); n > 0 {
			logf(&opts, "已还原游戏音量设置（渲染前的状态）")
		}
	}()
	// 渲染用 -w -h 指定分辨率启动游戏，会改写游戏保存的分辨率设置（cs2_video.txt）。
	// 先备份全部账号的视频设置，渲染结束（含失败/取消路径）统一还原。
	backups := backupVideoSettings()
	defer func() {
		if n := restoreVideoSettings(backups); n > 0 {
			logf(&opts, "已还原游戏分辨率设置（渲染前的状态）")
		}
	}()
	if !opts.NoQuit && !steamRunning() {
		logf(&opts, "警告: 未检测到 Steam 进程，CS2 启动可能失败。建议先启动 Steam。")
	}
	w, h := screenSize()
	if opts.Resolution != "" {
		if n, _ := fmt.Sscanf(opts.Resolution, "%dx%d", &w, &h); n != 2 {
			return nil, fmt.Errorf("分辨率格式错误: %s（应为 宽x高，如 1280x720）", opts.Resolution)
		}
	}
	// 强制有边框窗口模式 + 录制分辨率（渲染结束自动还原），保证后台隐藏可靠
	patchVideoSettings(backups, w, h)
	serverArg := "-perfectworld "
	switch opts.Server {
	case "worldwide":
		serverArg = "-worldwide "
	case "":
		serverArg = ""
	}
	estBytes := peakEstimateBytes(opts.Clips, opts.TickRate, opts.FPS, w, h)
	if free := freeBytes(filepath.Dir(tp.CS2)); free > 0 && estBytes > float64(free) {
		return nil, fmt.Errorf("预计峰值占用 %.1f GB，但游戏盘仅剩 %.1f GB。可用 --res 1280x720 降低分辨率",
			float64(estBytes)/1e9, float64(free)/1e9)
	}
	logf(&opts, "预计峰值临时占用 %.1f GB（%dx%d@%dfps，录制完成后统一编码）", float64(estBytes)/1e9, w, h, opts.FPS)

	// 录制顺序 = 时间顺序，与 GenerateCfgs 内部的时间排序一致
	chrono := make([]highlight.Clip, len(opts.Clips))
	copy(chrono, opts.Clips)
	sort.Slice(chrono, func(i, j int) bool { return chrono[i].StartTick < chrono[j].StartTick })

	rawDir := filepath.Join(opts.OutDir, "raw")
	os.MkdirAll(rawDir, 0o755)

	// 断点续录主循环。
	// 每轮最多录制 opts.MaxPerPass 个片段（默认 6）：TrueSpectate 预测扫描的
	// 死锁概率随单轮 seek 次数累积（实测连续 9 次 seek 后在第 9 个片段录制中
	// 必死锁，而短轮次到同一 demo 位置不死锁），主动分轮让每轮状态新鲜，
	// 把"意外死锁中断"变成"计划内分轮"。
	var allClips []string // 各轮录制的片段（按录制顺序）
	remaining := chrono
	emptyPasses := 0
	passNo := 0
	for len(remaining) > 0 {
		// 取消后不再启动新一轮：Cancel 只杀当前游戏，若这里不拦截，
		// 断点续录循环会把"被取消的轮次"当成失败轮次自动重试（实测取消后
		// 反而立刻开始录第二轮）。
		if cancelRequested.Load() {
			return nil, ErrCancelled
		}
		passNo++
		batch := remaining
		if opts.MaxPerPass > 0 && len(batch) > opts.MaxPerPass && !opts.ReloadSplit {
			batch = batch[:opts.MaxPerPass]
		}
		logf(&opts, "[渲染] 已录 %d/%d 个片段，第 %d 轮启动（本轮 %d 个，剩余 %d 个）...",
			len(allClips), len(chrono), passNo, len(batch), len(remaining))
		report(&opts, ProgressEvent{Stage: "pass", Pass: passNo, Done: len(allClips), Total: len(chrono),
			ClipIndex: len(allClips) + 1,
			Percent:   progressPercent(float64(len(allClips)), len(chrono)),
			Detail: fmt.Sprintf("第 %d 轮启动（本轮 %d 个片段，全局第 %d–%d 个）",
				passNo, len(batch), len(allClips)+1, len(allClips)+len(batch))})
		passOpts := opts
		passOpts.Clips = batch
		if opts.Progress != nil {
			// runPass 内部只看到本轮 batch，这里把进度 Total 修正为全局片段数
			orig := opts.Progress
			passOpts.Progress = func(ev ProgressEvent) {
				ev.Total = len(chrono)
				orig(ev)
			}
		}
		clips, good, err := runPass(tp, passOpts, rawDir, len(allClips), len(chrono), passNo, w, h, serverArg, volBackups)
		if err != nil {
			return nil, err
		}
		// 本轮已被取消：已录制的片段已编码保留，但不再续录、不再合成
		if cancelRequested.Load() {
			return nil, ErrCancelled
		}
		if good == 0 {
			emptyPasses++
			if emptyPasses >= 3 {
				return nil, fmt.Errorf("连续 %d 轮未录到任何片段，渲染中止（已完成 %d/%d）", emptyPasses, len(allClips), len(chrono))
			}
			logf(&opts, "本轮未录到片段，稍后重试 ...")
			time.Sleep(10 * time.Second)
			continue
		}
		emptyPasses = 0
		allClips = append(allClips, clips...)
		remaining = remaining[good:] // 每轮按连续成功前缀推进（截断/失败的片段留给下一轮重录）
		logf(&opts, "第 %d 轮完成：本轮录到 %d 个片段（全局 %d/%d）", passNo, good, len(allClips), len(chrono))
		report(&opts, ProgressEvent{Stage: "record", Pass: passNo, Done: len(allClips), Total: len(chrono),
			ClipIndex: len(allClips),
			Percent:   progressPercent(float64(len(allClips)), len(chrono)),
			Detail:    fmt.Sprintf("已录 %d/%d 个片段", len(allClips), len(chrono))})
	}

	// 按分类归档为单独的视频文件：clips/<分类>/<第N回合>_<标签>.mp4
	clipsDir := filepath.Join(opts.OutDir, "clips")
	var categorized []string
	for i, v := range allClips {
		if i >= len(chrono) {
			break
		}
		c := chrono[i]
		cat := c.Category
		if cat == "" {
			cat = "常规"
		}
		catDir := filepath.Join(clipsDir, cat)
		os.MkdirAll(catDir, 0o755)
		dst := filepath.Join(catDir, fmt.Sprintf("第%d回合_%s.mp4", c.Round, shortClipLabel(c)))
		if dst != v {
			if err := os.Rename(v, dst); err != nil {
				dst = v
			}
		}
		categorized = append(categorized, dst)
	}

	writeVideoIndex(opts.OutDir, chrono, categorized)

	// SkipConcat：只输出分片段视频（用户已选择片段，未必需要合一条成片，省时间）
	if opts.SkipConcat {
		report(&opts, ProgressEvent{Stage: "done", Pass: passNo, Done: len(chrono), Total: len(chrono),
			ClipIndex: len(chrono), Partial: 1, Percent: 1,
			Detail: fmt.Sprintf("完成 %d 个片段（跳过合成）", len(categorized))})
		return &Result{FinalVideo: "", ClipVideos: categorized}, nil
	}

	logf(&opts, "正在合成 %d 个片段（合集）...", len(categorized))
	report(&opts, ProgressEvent{Stage: "concat", Pass: passNo, Done: len(chrono), Total: len(chrono),
		ClipIndex: len(chrono), Partial: 1, Percent: 1,
		Detail: fmt.Sprintf("合成 %d 个片段为成片", len(categorized))})
	concatList := filepath.Join(opts.OutDir, "concat_list.txt")
	var cb strings.Builder
	for _, v := range categorized {
		// 列表里必须是绝对路径：concat 格式的相对路径按列表文件所在目录解析，
		// 相对路径会导致目录重复拼接（output\..._out2\output\..._out2\clips\...）。
		if a, err := filepath.Abs(v); err == nil {
			v = a
		}
		cb.WriteString(fmt.Sprintf("file '%s'\n", strings.ReplaceAll(v, `\`, "/")))
	}
	if err := os.WriteFile(concatList, []byte(cb.String()), 0o644); err != nil {
		return nil, err
	}
	final := filepath.Join(opts.OutDir, safeFileName(opts.PlayerName)+"_highlights.mp4")
	// 视频流直接 copy（各片段编码参数一致）；音频重编码，避免逐段 AAC
	// 编码器延迟在拼接处累积出细微错位/爆音。
	ff := exec.Command(tp.FFmpeg, "-y", "-f", "concat", "-safe", "0", "-i", concatList,
		"-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-shortest", final)
	ff.SysProcAttr = winutil.NoWindow()
	if out, err := ff.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("FFmpeg 合成失败: %w\n%s", err, out)
	}
	report(&opts, ProgressEvent{Stage: "done", Pass: passNo, Done: len(chrono), Total: len(chrono),
		ClipIndex: len(chrono), Partial: 1, Percent: 1, Detail: "渲染完成"})
	return &Result{FinalVideo: final, ClipVideos: categorized}, nil
}

// runPass 执行一轮渲染：生成配置 → 启动游戏录制 → 编码本轮 take。
// 返回本轮录制的片段（录制顺序）与「连续成功前缀数」good：
// 片段中间出现截断/编码失败时只算前缀（后续片段交给下一轮从空洞处重录，
// 防止主循环按 len(clips) 推进把空洞误记为已完成）。
// startIdx 是本轮第一个片段的全局编号（跨轮连续编号）。
func runPass(tp *ToolPaths, opts Options, rawDir string, startIdx, totalClips, passNo, w, h int, serverArg string, volBackups []volumeSettingBackup) ([]string, int, error) {
	// 取消后不要再启动新游戏（重试等待期间用户可能已点取消）
	if cancelRequested.Load() {
		return nil, 0, ErrCancelled
	}
	gameCfgDir, err := GenerateCfgs(tp.CS2, opts.OutDir, opts.DemoPath, opts)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if matches, e := filepath.Glob(filepath.Join(gameCfgDir, "cs2hi_auto*.cfg")); e == nil {
			for _, m := range matches {
				os.Remove(m)
			}
		}
		os.Remove(filepath.Join(gameCfgDir, "cs2hi_record.cfg"))
	}()

	// CS2 的 exec 不支持绝对路径，cfg 已写入 game\csgo\cfg\，这里用相对名。
	// -condebug 让游戏把控制台输出写到 game\csgo\console.log，用于失败诊断。
	// 不能用 -nosound：音频系统未初始化时 HLAE 音频录制会在 soundsystem.dll
	// 触发 Access Violation 崩溃（实测必现）。也不能用 volume 0 静音——引擎
	// 录制的是最终混音，音量 0 会让 audio.wav 变成静音；实测录制期间扬声器
	// 输出本身已是静音（引擎把声音送进录制管道），无需额外静音。
	cmdLine := fmt.Sprintf("-steam -insecure %s+sv_lan 1 -console -condebug -windowed -w %d -h %d +exec cs2hi_auto.cfg",
		serverArg, w, h)

	takeDir := filepath.Join(filepath.Dir(tp.CS2), "untitled_rec")
	os.MkdirAll(takeDir, 0o755)
	// 清理上一轮残留的 take 目录：HLAE 的 take 编号是全局计数（不随游戏重启
	// 归零），残留目录会让本轮编号从 takeNNNN 继续，与「takeNNNN 从 0 起
	// 对应本轮片段序号」的对齐假设冲突，导致录好的片段因编号错位被跳过
	// （实测：连续三轮完整录制全部白录）。
	if entries, err := os.ReadDir(takeDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "take") {
				os.RemoveAll(filepath.Join(takeDir, e.Name()))
			}
		}
	}
	start := time.Now()

	// 编码策略：录制期间【零编码】。此前边录边编码的 ffmpeg 与游戏渲染
	// 争抢 CPU/磁盘 IO，实测 1080p 单轮 12 段时在第 9 段录制中必死锁
	// （TrueSpectate 冻结），720p 编码负担减半死锁推迟、分轮模式编码与
	// 游戏运行错开故不死锁——竞争即死锁诱因。改为游戏退出后统一编码，
	// 磁盘峰值 = 本轮 TGA 总量（1080p60 约 356MB/s，12 段约 110GB）。
	encoded := []string{}

	logf(&opts, "第 %d 轮：正在启动 CS2 渲染（游戏会在后台隐藏运行，请勿操作键盘鼠标）...", passNo)
	cmd := exec.Command(tp.HLAE, "-customLoader", "-noGui", "-autoStart",
		"-hookDllPath", tp.HookDLL, "-programPath", tp.CS2, "-cmdLine", cmdLine)
	cmd.SysProcAttr = winutil.NoWindow()
	cmd.Dir = filepath.Dir(tp.HLAE)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Dir(tp.FFmpeg)+";"+os.Getenv("PATH"),
		"FFMPEG_PATH="+filepath.Dir(tp.FFmpeg))
	beforePIDs := gamePIDs("cs2.exe")
	currentGameMu.Lock()
	currentGameBefore = beforePIDs
	currentGameMu.Unlock()
	// 清空上一轮 console.log：退出监听以日志出现 "Host activate: Quitting" 为准，
	// 不能读到旧内容误判。路径必须在删除前解析（gameConsoleLog 依赖文件是否存在）。
	consoleLog := gameConsoleLog(tp.CS2)
	os.Remove(consoleLog)
	launcherLog, _ := os.Create(filepath.Join(opts.OutDir, "hlae_launcher.log"))
	cmd.Stdout = launcherLog
	cmd.Stderr = launcherLog
	if err := cmd.Start(); err != nil {
		launcherLog.Close()
		return nil, 0, fmt.Errorf("启动 HLAE 失败: %w", err)
	}
	// 渲染期间静音游戏音频会话：只静音扬声器播放，HLAE 内录的 audio.wav 不受影响。
	// Windows 会按应用持久化静音状态，必须赶在游戏音频会话销毁前解除：正常
	// 退出由 watchGameQuit 用「最后一段录完」信号解除（cfg 在 quit 前留了
	// demo_pause + wait 窗口）；取消/冻结/超时等主动终止路径由 killNewGame
	// 在 taskkill 前解除。
	startGameAudioMute(beforePIDs, func(format string, args ...any) { logf(&opts, format, args...) })
	defer stopGameAudioMute()
	stopQuitWatch := make(chan struct{})
	defer close(stopQuitWatch)
	// 录制完成的可靠信号：本轮最后一段录完时日志出现第 len(opts.Clips) 次
	// "Finishing recording"，cfg 随后 demo_pause + wait 约 1.5 秒才 quit。
	// 在这个窗口内解除静音，Windows 才会把 false 持久化，下次进游戏不静音
	// （只靠 Quitting 日志太晚：引擎退出与日志行几乎同秒）。
	expectedFinishes := 0
	if !opts.NoQuit {
		expectedFinishes = len(opts.Clips)
	}
	go watchGameQuit(consoleLog, expectedFinishes, stopQuitWatch, stopGameAudioMute)
	if opts.HideWindow {
		// 后台渲染：游戏窗口透明化（完全不可见 + 点击穿透 + 不抢焦点）。
		// 高频持续维护（50ms）——游戏可能在加载/seek 时重置窗口样式，
		// 需要快速恢复透明；首次执行立即进行，缩短窗口出现到透明化的
		// 可见窗口期（闪烁的来源）。
		// 注意必须用 newPIDs（渲染新启动的游戏实例），传 beforePIDs 只会匹配
		// 渲染前就已存在的进程，新游戏窗口永远不会被隐藏。
		stopHide := make(chan struct{})
		defer close(stopHide)
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			var gamePids map[int]bool // 游戏进程出现后缓存，避免每 tick 都跑 tasklist
			for {
				if len(gamePids) == 0 {
					gamePids = newPIDs(beforePIDs)
				}
				if len(gamePids) > 0 {
					hideGameWindows(gamePids)
				}
				select {
				case <-stopHide:
					return
				case <-ticker.C:
				}
			}
		}()
		// 光标防护：游戏启动时会把光标 warp 到窗口中心（引擎固有行为），
		// 检测到跳变后自动恢复，用户操作无感
		stopCursor := make(chan struct{})
		defer close(stopCursor)
		go guardCursor(stopCursor)
	}
	// 录制进度监控：take 目录在片段录制开始时创建（此时帧数从 0 增长）。
	// 进度语义 = 已完成片段数 + 当前录制中片段的帧进度（Partial），
	// 每 1 秒上报一次：进度条随帧数平滑前进，而非录制开始即跳满。
	stopProg := make(chan struct{})
	var stopProgOnce sync.Once
	stopProgress := func() { stopProgOnce.Do(func() { close(stopProg) }) }
	defer stopProgress()
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		seen := 0
		for {
			select {
			case <-stopProg:
				return
			case <-ticker.C:
				takes, _ := findTakeDirs(takeDir, start)
				if len(takes) == 0 {
					continue
				}
				if len(takes) == seen {
					// 正在录的片段帧数在增长：上报当前帧进度（1 秒节流）
					cur := takes[len(takes)-1]
					ci := takeNum(cur)
					if ci >= 0 && ci < len(opts.Clips) {
						want := opts.Clips[ci].DurationSec * float64(opts.FPS)
						if want > 0 {
							partial := float64(countTGA(cur)) / want
							if partial > 1 {
								partial = 1
							}
							report(&opts, ProgressEvent{Stage: "record", Pass: passNo, ClipIndex: startIdx + seen,
								Done: startIdx + seen - 1, Total: totalClips, Partial: partial,
								Percent: progressPercent(float64(startIdx)+(float64(seen-1)+partial)/2, totalClips),
								Detail: fmt.Sprintf("第 %d 轮 · 全局第 %d/%d 个片段 · 录制中 %d%%",
									passNo, startIdx+seen, totalClips, int(partial*100))})
						}
					}
					continue
				}
				seen = len(takes)
				if seen-1 < len(opts.Clips) {
					c := opts.Clips[seen-1]
					cat := c.Category
					if cat == "" {
						cat = "常规"
					}
					logf(&opts, "开始录制全局第 %d/%d 个片段（第 %d 回合 · %s · %.1fs）",
						startIdx+seen, totalClips, c.Round, cat, c.DurationSec)
				}
				report(&opts, ProgressEvent{Stage: "record", Pass: passNo, ClipIndex: startIdx + seen,
					Done: startIdx + seen - 1, Total: totalClips,
					Percent: progressPercent(float64(startIdx)+float64(seen-1)/2, totalClips),
					Detail: fmt.Sprintf("第 %d 轮 · 开始录制全局第 %d/%d 个片段",
						passNo, startIdx+seen, totalClips)})
			}
		}
	}()
	// 冻结监控：TrueSpectate 死锁时游戏进程 CPU 时间停止增长，检测到后主动
	// 终止游戏，让本轮正常收尾（已录片段保留），避免 cmd.Wait() 永久阻塞。
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go watchFreeze(beforePIDs, stopWatch, &opts, func() { killNewGame(beforePIDs) })

	// 等 HLAE 退出（游戏退出后 HLAE 随之退出）；30 分钟超时兜底防永久阻塞
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		launcherLog.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("HLAE 退出异常: %w（检查 Steam 是否已登录）", err)
		}
	case <-time.After(30 * time.Minute):
		launcherLog.Close()
		logf(&opts, "  ⚠ HLAE 等待超时（30 分钟），强制终止游戏 ...")
		killNewGame(beforePIDs)
		<-waitCh
	}
	waitGameExit(beforePIDs, 2*time.Minute)
	// 兜底：清理因死锁等原因未退出的游戏实例
	killNewGame(beforePIDs)
	// 录制阶段结束，立刻停掉录制进度监控：编码阶段会逐个删除 take 目录，
	// 监控协程看到 take 数量减少会误判为"重新开始录制"，用旧的 Detail/0%
	// 覆盖编码进度（前端表现为进度回退、日志里冒出第二次"开始录制"）。
	stopProgress()
	// 游戏一退出就还原音量（兜底逻辑）：本轮后期的编码/合成可能持续数分钟，
	// 拖到最后还原会让用户期间启动 CS2 时仍是旧值（且运行中的游戏退出时
	// 会再写回）。当前管线不改音量，这里通常是无操作。
	if n := restoreVolumeSettings(volBackups); n > 0 {
		logf(&opts, "已还原游戏音量设置（渲染前的状态）")
	}

	// 存档本轮游戏控制台日志（-condebug 的 console.log 每轮启动都会被覆盖，
	// 死锁/崩溃现场必须保留下来才能诊断退出原因）
	if data, err := os.ReadFile(gameConsoleLog(tp.CS2)); err == nil && len(data) > 0 {
		os.WriteFile(filepath.Join(opts.OutDir, fmt.Sprintf("console_pass_%s.log", time.Now().Format("150405"))), data, 0o644)
		// 当场打印尾部，退出原因第一时间可见
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		tail := 3
		if len(lines) < tail {
			tail = len(lines)
		}
		logf(&opts, "  [console 尾部] %s", strings.Join(lines[len(lines)-tail:], " | "))
	}

	// 游戏退出后统一编码所有 take。
	// 截断检测：死锁时正在录制的 take 帧序列戛然而止（无 Finishing recording），
	// 若帧数不足预期时长（×fps）的 85% 判定截断 → 丢弃，交给下一轮重录，
	// 避免残缺片段混入成片（实测死锁轮次曾把 15.1s/37.5s 的截断片段当完整保留）。
	// 注意：take 对齐必须用目录名编号（takeNNNN 从 0 起对应本轮片段序号）。
	// good = 从 batch 开头连续成功的片段数；中间出现截断/编码失败即 break，
	// 后续片段留给下一轮从空洞处重录（主循环按 good 推进 remaining，
	// 若按 len(clips) 推进会把空洞误记为已完成）。
	takes, _ := findTakeDirs(takeDir, start)
	good := 0
	for _, t := range takes {
		ci := takeNum(t)
		if ci < 0 || ci >= len(opts.Clips) {
			continue
		}
		if ci != good {
			break // 空洞：前面的片段已丢失，前缀到此为止
		}
		want := opts.Clips[ci].DurationSec * float64(opts.FPS)
		if got := countTGA(t); got > 0 && want > 0 && float64(got) < want*0.85 {
			logf(&opts, "  ⚠ 片段 %d（%s）帧数不足（%d/%d，%.0f%%），判定截断，下一轮重录 ...",
				startIdx+ci+1, filepath.Base(t), got, int(want), float64(got)/want*100)
			report(&opts, ProgressEvent{Stage: "warn", Pass: passNo, ClipIndex: startIdx + ci + 1,
				Detail: fmt.Sprintf("片段 %d 录制中断（%d/%d 帧），自动重录", startIdx+ci+1, got, int(want))})
			os.RemoveAll(t)
			break
		}
		idx := startIdx + good + 1
		mp4 := filepath.Join(rawDir, fmt.Sprintf("clip%02d.mp4", idx))
		logf(&opts, "  编码片段 %d/%d: %s ...", idx, totalClips, filepath.Base(t))
		// 进度单位：每个片段两等份（录制完成 1 份、编码完成 1 份）。
		// 编码开始时只计已编码完成的 good 个片段，当前片段要等编码结束后
		// 由下一段/本轮结束事件计入——否则最后一个片段刚开始编码进度条
		// 就到了 100%（旧公式多算了 good+1，用户会看到"没渲染完就满格"）。
		report(&opts, ProgressEvent{Stage: "encode", Pass: passNo, ClipIndex: idx, Done: idx, Total: totalClips,
			Percent: progressPercent(float64(startIdx)+(float64(len(opts.Clips))+float64(good))/2, totalClips),
			Detail:  fmt.Sprintf("第 %d 轮 · 全局第 %d/%d 个片段 · 编码中", passNo, idx, totalClips)})
		encStart := time.Now()
		if err := encodeTGA(tp.FFmpeg, t, mp4, opts.FPS, opts.CRF, opts.Preset); err != nil {
			logf(&opts, "    编码失败: %v", err)
			os.RemoveAll(t)
			break
		}
		logf(&opts, "    片段 %d 编码完成（用时 %.1fs）", idx, time.Since(encStart).Seconds())
		encoded = append(encoded, mp4)
		os.RemoveAll(t)
		good++
	}

	raw, err := findNewVideos(filepath.Dir(tp.CS2), start)
	if err != nil || len(raw) == 0 {
		if len(encoded) == 0 {
			// 本轮没录到任何片段：诊断信息上报进度（GUI 用户可见）并交由主循环重试
			diag := diagnoseFailure(opts.OutDir, tp.CS2, start, beforePIDs)
			logf(&opts, "%v", diag)
			report(&opts, ProgressEvent{Stage: "warn", Pass: passNo, Detail: diag.Error()})
			return nil, 0, nil
		}
		raw = encoded
	}
	if len(encoded) == 0 {
		// 游戏目录兜底路径（HLAE 直录 mp4）：无法逐段核对，按数量乐观对齐
		if good == 0 {
			good = len(raw)
			if good > len(opts.Clips) {
				good = len(opts.Clips)
			}
		}
	}
	return raw, good, nil
}

// writeVideoIndex 写出两部分对照的集锦视频索引：
// 第一部分：每个集锦视频对应了哪些精彩操作；第二部分：每个精彩操作对应哪个集锦视频。
func writeVideoIndex(outDir string, clips []highlight.Clip, videos []string) {
	var b strings.Builder
	b.WriteString("# 集锦视频索引\n\n")
	b.WriteString("## 第一部分：集锦视频 → 精彩操作\n\n")
	for i, v := range videos {
		if i >= len(clips) {
			break
		}
		c := clips[i]
		rel, _ := filepath.Rel(outDir, v)
		b.WriteString(fmt.Sprintf("### `%s`\n\n", filepath.ToSlash(rel)))
		b.WriteString(fmt.Sprintf("- 分类：%s，第 %d 回合，得分 %.0f，时长 %.1f 秒\n", c.Category, c.Round, c.Score, c.DurationSec))
		for _, m := range c.Moments {
			b.WriteString(fmt.Sprintf("  - %s（%.0f 分）\n", m.Title, m.Score))
		}
		b.WriteString("\n")
	}
	b.WriteString("## 第二部分：精彩操作 → 集锦视频\n\n")
	b.WriteString("| 精彩操作 | 所在视频 |\n|---|---|\n")
	for i, v := range videos {
		if i >= len(clips) {
			break
		}
		c := clips[i]
		rel, _ := filepath.Rel(outDir, v)
		for _, m := range c.Moments {
			b.WriteString(fmt.Sprintf("| %s（%.0f 分） | `%s` |\n", m.Title, m.Score, filepath.ToSlash(rel)))
		}
	}
	os.WriteFile(filepath.Join(outDir, "video_index.md"), []byte(b.String()), 0o644)
}

// shortClipLabel 生成片段的短标签（用于文件名）。
// 优先级：五杀 > 四杀 > 三杀 > 1vN残局 > N连杀 > 单杀修饰词，保证名称与击杀数一致。
func shortClipLabel(c highlight.Clip) string {
	has := func(kw string) string {
		for _, t := range c.Tags {
			if strings.Contains(t, kw) {
				return safeFileName(t)
			}
		}
		return ""
	}
	for _, kw := range []string{"五杀", "四杀", "三杀", "残局", "4连杀", "3连杀", "2连杀"} {
		if l := has(kw); l != "" {
			return l
		}
	}
	for _, t := range c.Tags {
		switch t {
		case "爆头", "盲狙", "穿烟", "首杀", "补枪", "刀杀", "电击枪", "拆弹", "ECO击杀", "穿墙", "被闪白击杀", "闪光助攻", "远距离", "道具击杀":
			return safeFileName(t)
		}
	}
	return "击杀"
}

func steamRunning() bool {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq steam.exe", "/NH")
	cmd.SysProcAttr = winutil.NoWindow()
	out, err := cmd.Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "steam.exe")
}

// gameConsoleLog 返回 CS2 的 -condebug 控制台日志路径。
// 日志写在 game\csgo\console.log（cs2.exe 在 game\bin\win64\）。
func gameConsoleLog(cs2Path string) string {
	p := filepath.Join(filepath.Dir(cs2Path), "..", "..", "csgo", "console.log")
	if _, err := os.Stat(p); err != nil {
		p = filepath.Join(filepath.Dir(cs2Path), "console.log")
	}
	return p
}

// diagnoseFailure 录制失败时收集诊断信息：控制台日志线索 + 清理本次启动的游戏实例。
func diagnoseFailure(outDir, cs2Path string, start time.Time, before map[int]bool) error {
	var hints []string
	consoleLog := gameConsoleLog(cs2Path)
	if data, err := os.ReadFile(consoleLog); err == nil {
		lines := strings.Split(string(data), "\n")
		var interesting []string
		for _, l := range lines {
			low := strings.ToLower(l)
			if strings.Contains(low, "unknown command") || strings.Contains(low, "mirv") ||
				strings.Contains(low, "error") || strings.Contains(low, "playdemo") ||
				(strings.Contains(low, "steam") && strings.Contains(low, "running")) {
				interesting = append(interesting, l)
			}
		}
		if len(interesting) > 0 {
			joined := strings.Join(interesting, "\n  ")
			if len(joined) > 2000 {
				joined = joined[len(joined)-2000:]
			}
			hints = append(hints, fmt.Sprintf("游戏控制台线索:\n  %s", joined))
		}
	}
	launcherLog := filepath.Join(outDir, "hlae_launcher.log")
	if data, err := os.ReadFile(launcherLog); err == nil && len(data) > 0 {
		hints = append(hints, fmt.Sprintf("HLAE 启动器输出:\n  %s", strings.TrimSpace(string(data))))
	}

	// 清理本次渲染启动的 cs2.exe 实例（PID 快照对比，与系统语言无关）
	killNewGame(before)

	msg := "未找到录制输出，渲染失败。请确认 Steam 已登录且 CS2 可正常启动"
	if len(hints) > 0 {
		msg += "。诊断信息:\n" + strings.Join(hints, "\n")
	}
	return fmt.Errorf("%s", msg)
}

// gamePIDs 返回指定进程名的 PID 集合（tasklist CSV 输出解析，与系统语言无关）。
func gamePIDs(exeName string) map[int]bool {
	pids := map[int]bool{}
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq "+exeName, "/FO", "CSV", "/NH")
	cmd.SysProcAttr = winutil.NoWindow()
	out, err := cmd.Output()
	if err != nil {
		return pids
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) >= 2 && strings.EqualFold(strings.Trim(fields[0], "\""), exeName) {
			if pid, err := strconv.Atoi(strings.Trim(fields[1], "\"")); err == nil {
				pids[pid] = true
			}
		}
	}
	return pids
}

// newPIDs 返回在 before 之后新出现的进程 PID 集合。
func newPIDs(before map[int]bool) map[int]bool {
	diff := map[int]bool{}
	for pid := range gamePIDs("cs2.exe") {
		if !before[pid] {
			diff[pid] = true
		}
	}
	return diff
}

// waitGameExit 等待本次渲染启动的游戏进程退出。
func waitGameExit(before map[int]bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(newPIDs(before)) == 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// 死锁检测参数：每 freezeSampleEvery 秒采样一次进程 CPU 时间，
// 冻结判定窗口 freezeWindow（窗口内 CPU 增长低于 freezeMinCPU 即判死锁）。
// 正常 60fps 实时渲染下 25 秒墙钟至少消耗数秒 CPU（多核并行）；
// 死锁时仅剩的杂散线程累计远低于 3 秒。启动初期（初始化/加载 demo）
// 跳过判定，避免误杀。
const (
	freezeSampleEvery = 5 * time.Second
	freezeWindow      = 25 * time.Second
	freezeMinCPU      = 3 * uint64(time.Second/100) // CPU 时间单位 100ns，3 秒 = 3e7
	freezeGrace       = 90 * time.Second            // 启动后宽限期
)

// watchFreeze 监控本次启动的游戏进程：CPU 时间停止增长即判定 TrueSpectate
// 死锁，终止游戏让本轮正常收尾（已录片段保留，由主循环断点续录）。
func watchFreeze(before map[int]bool, stop <-chan struct{}, opts *Options, onFrozen func()) {
	ticker := time.NewTicker(freezeSampleEvery)
	defer ticker.Stop()
	launched := time.Now()
	type sample struct {
		at  time.Time
		cpu uint64
	}
	var samples []sample
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if time.Since(launched) < freezeGrace {
				continue // 游戏初始化/加载 demo 期间不判定
			}
			pids := newPIDs(before)
			if len(pids) == 0 {
				return // 游戏已正常退出
			}
			var total uint64
			for pid := range pids {
				if c, err := procCPUTime(pid); err == nil {
					total += c
				}
			}
			now := time.Now()
			samples = append(samples, sample{now, total})
			// 只保留冻结窗口内的样本
			cut := now.Add(-freezeWindow)
			for len(samples) > 0 && samples[0].at.Before(cut) {
				samples = samples[1:]
			}
			if len(samples) >= 4 && now.Sub(samples[0].at) >= freezeWindow-5*time.Second {
				growth := samples[len(samples)-1].cpu - samples[0].cpu
				if growth < freezeMinCPU {
					logf(opts, "  ⚠ 检测到游戏冻结（进程 CPU 时间停止增长），终止本轮渲染 ...")
					for pid := range pids {
						logf(opts, "    [诊断] PID %d：内存 %d MB | 句柄 %d | GDI %d",
							pid, procMemMB(pid), procHandleCount(pid), procGDIObjects(pid))
					}
					onFrozen()
					return
				}
			}
		}
	}
}

// findNewVideos 在目录树下查找 start 之后生成的大小可观的 mp4 文件。
func findNewVideos(root string, after time.Time) ([]string, error) {
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".mp4") &&
			info.ModTime().After(after) && info.Size() > 1<<20 {
			found = append(found, path)
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool {
		fi, _ := os.Stat(found[i])
		fj, _ := os.Stat(found[j])
		return fi.ModTime().Before(fj.ModTime())
	})
	return found, err
}

// findTakeDirs 查找 start 之后生成的 TGA 帧序列目录（HLAE 的 takeXXXX）。
func findTakeDirs(untitledRec string, after time.Time) ([]string, error) {
	entries, err := os.ReadDir(untitledRec)
	if err != nil {
		return nil, err
	}
	var takes []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "take") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().After(after) {
			continue
		}
		// 必须包含 TGA 帧文件
		files, _ := os.ReadDir(filepath.Join(untitledRec, e.Name()))
		hasTGA := false
		for _, f := range files {
			if strings.EqualFold(filepath.Ext(f.Name()), ".tga") {
				hasTGA = true
				break
			}
		}
		if hasTGA {
			takes = append(takes, filepath.Join(untitledRec, e.Name()))
		}
	}
	sort.Strings(takes) // take0000 < take0001 ...，即录制顺序
	return takes, nil
}

// takeNum 从 take 目录名解析编号（take0008 → 8），失败返回 -1。
// 每轮游戏重启后编号从 take0000 重新开始，与本轮片段列表一一对应。
func takeNum(dir string) int {
	base := filepath.Base(dir)
	if strings.HasPrefix(base, "take") && len(base) > 4 {
		if n, err := strconv.Atoi(base[4:]); err == nil {
			return n
		}
	}
	return -1
}

// countTGA 统计 take 目录内的 TGA 帧文件数（用于截断检测）。
func countTGA(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.EqualFold(filepath.Ext(e.Name()), ".tga") {
			n++
		}
	}
	return n
}

// encodeTGA 把单个 TGA 帧序列目录编码为 mp4（含 HLAE 同步写入的 audio.wav）。
// 实测（i9-14900KF，1080p60 38.5s 片段）：fast 10s/105MB、medium 12s/103MB、
// slow 21s/103MB——medium 体积接近 slow 而时间接近 fast，为默认甜点；
// CRF 是体积主控：16→18 体积 -36%（SSIM 0.987→0.982 视觉无差别），18→20 再 -30%。
//
// 音频说明：HLAE 在 take 目录里同时写 audio.wav（引擎内录，与画面同一时间轴）。
// 早期版本用 volume 0 静音，导致 wav 也是静音（实测 -91dB）；现改为保留玩家
// 音量，渲染期间的外放静音由 Windows 音频会话静音负责（audio.go，只静音
// cs2.exe 播放，不影响引擎内部写出的 wav）。
// 音频参数固定 aac/44.1k/立体声：所有片段一致才能用 concat 无损拼接。
// apad + shortest：音频比画面短时补静音，成片时长始终以画面为准。
func encodeTGA(ffmpeg, takeDir, mp4 string, fps, crf int, preset string) error {
	if preset == "" {
		preset = "medium"
	}
	args := []string{"-y", "-framerate", strconv.Itoa(fps), "-i", filepath.Join(takeDir, "%05d.tga")}
	if wav := filepath.Join(takeDir, "audio.wav"); fileExists(wav) {
		args = append(args, "-i", wav)
	} else {
		// 兜底：没录到音频时补一条同参数的静音轨，保证各片段流布局一致
		args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=44100")
	}
	args = append(args,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-preset", preset, "-crf", strconv.Itoa(crf),
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ar", "44100", "-ac", "2",
		"-af", "apad", "-shortest",
		mp4)
	cmd := exec.Command(ffmpeg, args...)
	cmd.SysProcAttr = winutil.NoWindow()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("编码 %s 失败: %w\n%s", filepath.Base(takeDir), err, out)
	}
	return nil
}

// fileExists 判断文件是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// peakEstimateBytes 估算 TGA 原始帧的峰值临时占用（字节）。
// 录制期间零编码（编码与游戏渲染争抢 CPU/IO 会诱发 TrueSpectate 死锁），
// 因此峰值 = 全部片段 TGA 总量 × 1.05 余量（1080p60 约 356MB/s）。
func peakEstimateBytes(clips []highlight.Clip, tickRate float64, fps, w, h int) float64 {
	totalSec := 0.0
	for _, c := range clips {
		dur := c.DurationSec
		if dur <= 0 && tickRate > 0 {
			dur = float64(c.EndTick-c.StartTick) / tickRate
		}
		totalSec += dur
	}
	return totalSec * 1.05 * float64(fps) * float64(w*h*3+32)
}

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
	procOpenProcess           = kernel32.NewProc("OpenProcess")
	procGetProcessTimes       = kernel32.NewProc("GetProcessTimes")
	procGetProcessMemoryInfo  = kernel32.NewProc("GetProcessMemoryInfo")
	psapi                     = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo2 = psapi.NewProc("GetProcessMemoryInfo")
	procGetProcessHandleCount = kernel32.NewProc("GetProcessHandleCount")
	procGetGuiResources       = user32.NewProc("GetGuiResources")
	procEnumWindows           = user32.NewProc("EnumWindows")
	procGetWindowThreadPID    = user32.NewProc("GetWindowThreadProcessId")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procIsWindowVisible       = user32.NewProc("IsWindowVisible")
	procGetWindowLongPtrW     = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
	procGetWindowRect         = user32.NewProc("GetWindowRect")
	procSetLayeredAttributes  = user32.NewProc("SetLayeredWindowAttributes")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procSetCursorPos          = user32.NewProc("SetCursorPos")
	procShowWindow            = user32.NewProc("ShowWindow")
)

// processMemoryCounters 对应 Windows PROCESS_MEMORY_COUNTERS 结构。
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uint64
	WorkingSetSize             uint64
	QuotaPeakPagedPoolUsage    uint64
	QuotaPagedPoolUsage        uint64
	QuotaPeakNonPagedPoolUsage uint64
	QuotaNonPagedPoolUsage     uint64
	PagefileUsage              uint64
	PeakPagefileUsage          uint64
}

// procMemMB 返回指定进程的提交内存（MB），失败返回 0。
func procMemMB(pid int) uint64 {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return 0
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var pmc processMemoryCounters
	pmc.Cb = uint32(unsafe.Sizeof(pmc))
	r, _, _ := procGetProcessMemoryInfo2.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.Cb))
	if r == 0 {
		return 0
	}
	return pmc.WorkingSetSize / (1 << 20)
}

// procHandleCount 返回指定进程的句柄数（失败返回 0）。
// 用于检测句柄泄漏：HLAE/CS2 每段录制可能泄漏句柄，
// 接近 Windows 每进程上限（约 10000）时引擎会冻结。
func procHandleCount(pid int) uint32 {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return 0
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var n uint32
	r, _, _ := procGetProcessHandleCount.Call(h, uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return 0
	}
	return n
}

// procGDIObjects 返回指定进程的 GDI 对象数（失败返回 0）。
// GetGuiResources 需要 PROCESS_QUERY_INFORMATION 权限。
func procGDIObjects(pid int) uint32 {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo|0x0400, 0, uintptr(pid))
	if h == 0 {
		return 0
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	r, _, _ := procGetGuiResources.Call(h, 0) // GR_GDIOBJECTS = 0
	return uint32(r)
}

const processQueryLimitedInfo = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION

type filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

// procCPUTime 返回指定进程的累计 CPU 时间（kernel + user，单位 100ns）。
// 进程 CPU 时间是死锁检测的关键信号：TrueSpectate 死锁时引擎渲染循环挂起，
// 进程 CPU 时间停止增长；而 demo 正常播放（含暂停/片段间隙）时帧循环持续消耗 CPU。
func procCPUTime(pid int) (uint64, error) {
	h, _, err := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return 0, err
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var creation, exit, kernel, user filetime
	r, _, err := procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return 0, err
	}
	ft := func(f filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return ft(kernel) + ft(user), nil
}

// killNewGame 终止本次渲染启动的游戏实例（PID 快照对比，与系统语言无关）。
// 先解除音频静音再终止：Windows 会持久化应用静音状态，被强杀时也必须在
// 音频会话销毁前还原，否则用户下次启动 CS2 会没声音。
func killNewGame(before map[int]bool) {
	stopGameAudioMute()
	for pid := range newPIDs(before) {
		cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
		cmd.SysProcAttr = winutil.NoWindow()
		cmd.Run()
	}
}

// currentGameBefore 记录当前渲染轮次启动前的游戏 PID 快照（GUI 取消用）。
var (
	currentGameMu     sync.Mutex
	currentGameBefore map[int]bool
)

// cancelRequested 取消标记：置位后 Run 不再启动新一轮录制。
var cancelRequested atomic.Bool

// Cancel 取消当前渲染（GUI 取消按钮）：置取消标记并终止本次渲染启动的
// 游戏实例。当前轮已录制的片段仍会在收尾时编码保留，但不会续录下一轮。
func Cancel() {
	cancelRequested.Store(true)
	currentGameMu.Lock()
	before := currentGameBefore
	currentGameMu.Unlock()
	if before != nil {
		killNewGame(before)
	}
}

// hideGameWindows 让游戏窗口对用户完全隐形且无感，但不干扰渲染
// （HLAE 的 mirv_streams 抓取的是游戏渲染管线内部帧，与窗口显示状态无关）：
//   - WS_EX_LAYERED + SetLayeredWindowAttributes(alpha=0)：窗口完全透明。
//     早期用 alpha=1 是担心 0 会让引擎跳过渲染——实测（720p60 录制中切换）
//     alpha=0 下进程 CPU 时间与 TGA 帧数增长完全正常，甚至略高于 alpha=1，
//     而 alpha=1 在深色背景上会透出 1/255 的画面"影子"。每 tick 强制 alpha=0，
//     防止游戏重置分层属性后残影回来
//   - WS_EX_TRANSPARENT：鼠标点击穿透到下层窗口，用户操作桌面无感
//   - WS_EX_TOOLWINDOW：任务栏 / Alt-Tab 不显示
//   - WS_EX_NOACTIVATE：不激活、不抢焦点
//
// 窗口位置完全不移动：窗口位置稳定是防「屏幕闪烁」与「游戏 warp 光标」
// 的关键——移出屏幕会被游戏拉回，拉锯造成闪烁与光标重置；留在原地
// 透明后游戏对位置满意，SDL 无理由重置。窗口矩形同步到共享变量，
// 供 guardCursor 做 warp 检测恢复（游戏启动时仍会主动 warp 一次光标到
// 窗口中心，属引擎固有行为，无法阻止，只能事后恢复）。

// EnumWindows 回调必须全局复用：syscall.NewCallback 的回调槽位上限是
// 2000（Go runtime cb_max），若每次调用都新建闭包（50ms 一次），约 100 秒
// 就会耗尽，触发 runtime throw("too many callback functions") 直接终止
// 整个进程——GUI 表现为窗口无任何征兆自动消失（无崩溃转储、无事件日志）。
// 这里用普通函数（funcval 固定）只注册一次，pids 通过全局变量传入。
var (
	hidePIDsMu    sync.Mutex
	hidePIDs      map[int]bool
	enumWindowsC  sync.Once
	enumWindowsCB uintptr
)

func hideGameWindows(pids map[int]bool) {
	hidePIDsMu.Lock()
	hidePIDs = pids
	hidePIDsMu.Unlock()
	enumWindowsC.Do(func() { enumWindowsCB = syscall.NewCallback(hideWindowProc) })
	procEnumWindows.Call(enumWindowsCB, 0)
}

// hideWindowProc EnumWindows 回调：对目标进程的可见窗口做透明化处理。
func hideWindowProc(hwnd, _ uintptr) uintptr {
	hidePIDsMu.Lock()
	pids := hidePIDs
	hidePIDsMu.Unlock()
	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if !pids[int(pid)] {
		return 1 // 继续枚举
	}
	gwlExStyle := ^uintptr(19) // GWL_EXSTYLE = -20（补码形式，避免负常量溢出 uintptr）
	const (
		wsExToolWindow  = 0x00000080
		wsExTransparent = 0x00000020
		wsExNoActivate  = 0x08000000
		wsExLayered     = 0x00080000
		lwaAlpha        = 0x00000002
		swpNoMove       = 0x0002
		swpNoSize       = 0x0001
		swpNoZOrder     = 0x0004
		swpNoActivate   = 0x0010
		swpFrameChanged = 0x0020
	)
	wantEx := uintptr(wsExToolWindow | wsExTransparent | wsExNoActivate | wsExLayered)
	if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
		// 同步窗口矩形（光标防护用，窗口位置不变但尺寸可能随分辨率调整）
		var rect struct{ left, top, right, bottom int32 }
		if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); r != 0 {
			cursorGuardMu.Lock()
			cursorGuardRect = rect
			cursorGuardHas = true
			cursorGuardMu.Unlock()
		}
		ex, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
		if ex&wantEx != wantEx {
			// 样式被游戏重置过（或首次设置）：先隐藏（杜绝设置完成前的
			// 可见窗口期——黑框闪现的来源），补全样式透明化后再无激活显示。
			// 隐藏窗口只有几毫秒（几次系统调用），引擎渲染循环无感知。
			procShowWindow.Call(hwnd, 0) // SW_HIDE
			procSetWindowLongPtrW.Call(hwnd, gwlExStyle, ex|wantEx)
			procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
				swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
			procSetLayeredAttributes.Call(hwnd, 0, 0, lwaAlpha)
			procShowWindow.Call(hwnd, 8) // SW_SHOWNA：显示但不激活
		}
		// 每 tick 强制完全透明：alpha=1 在深色背景下会透出画面影子；
		// 实测 alpha=0 不影响引擎渲染（CPU/帧数正常）。
		procSetLayeredAttributes.Call(hwnd, 0, 0, lwaAlpha)
	}
	return 1 // 继续枚举
}

// ---- 光标防护：CS2 启动/加载时会把光标 warp 到窗口中心（引擎固有行为，
// 实测强制拉鼠标到屏幕中间一次），事后检测并恢复，让用户操作无感。 ----
var (
	cursorGuardMu   sync.Mutex
	cursorGuardRect struct{ left, top, right, bottom int32 }
	cursorGuardHas  bool
)

// guardCursor 采样光标位置：检测「瞬间跳变到游戏窗口中心附近」的 warp
// 特征（正常鼠标甩动在采样间隔内达不到该距离、且不会恰好落在透明窗口的
// 中心区域），命中则把光标恢复到跳变前位置。
func guardCursor(stop <-chan struct{}) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastX, lastY int32
	haveLast := false
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			var pt struct{ x, y int32 }
			if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r == 0 {
				continue
			}
			cursorGuardMu.Lock()
			has := cursorGuardHas
			rect := cursorGuardRect
			cursorGuardMu.Unlock()
			if haveLast && has {
				dx, dy := pt.x-lastX, pt.y-lastY
				cx := (rect.left + rect.right) / 2
				cy := (rect.top + rect.bottom) / 2
				// warp 特征：100ms 内位移 400px+ 且目标落在窗口中心 ±140px 区域
				if dx*dx+dy*dy > 400*400 &&
					pt.x >= cx-140 && pt.x <= cx+140 && pt.y >= cy-140 && pt.y <= cy+140 {
					procSetCursorPos.Call(uintptr(lastX), uintptr(lastY))
					pt.x, pt.y = lastX, lastY
				}
			}
			lastX, lastY = pt.x, pt.y
			haveLast = true
		}
	}
}

// freeBytes 返回指定路径所在磁盘的可用字节数，失败返回 0。
func freeBytes(path string) uint64 {
	dir, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var free, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0
	}
	return free
}

func safeFileName(name string) string {
	repl := strings.NewReplacer(`\`, "_", "/", "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_")
	return repl.Replace(strings.TrimSpace(name))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fetchJSON(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func downloadFile(dst, url, accept string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	client := &http.Client{Timeout: dlTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// unzip 解压 zip（带路径穿越防护）。
func unzip(zipPath, dstDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue
		}
		dst := filepath.Join(dstDir, name)
		if f.FileInfo().IsDir() {
			os.MkdirAll(dst, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
