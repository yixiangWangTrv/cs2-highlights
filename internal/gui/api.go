package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"cs2-highlights/internal/demo"
	"cs2-highlights/internal/highlight"
	"cs2-highlights/internal/render"
	"cs2-highlights/internal/stats"
	"cs2-highlights/internal/winutil"
)

// ---- 请求/响应类型 ----

type parseReq struct {
	DemoPath string `json:"demoPath"`
}

// teamStat 玩家在某一阵营的数据（半场换边，每人都有 T/CT 两段）。
type teamStat struct {
	Kills  int     `json:"kills"`
	Deaths int     `json:"deaths"`
	KDR    float64 `json:"kdr"`
}

type playerResp struct {
	Name string `json:"name"`
	// steamId64 必须用字符串传输：uint64 超出 JS Number 安全整数范围（2^53），
	// 数字 JSON 会在前端精度丢失导致选不中玩家
	SteamID64 uint64   `json:"steamId64,string"`
	Team      string   `json:"team"` // 最终阵营（分组用）
	Kills     int      `json:"kills"`
	Deaths    int      `json:"deaths"`
	KDR       float64  `json:"kdr"`
	T         teamStat `json:"t"`  // T 方数据
	CT        teamStat `json:"ct"` // CT 方数据
}

type parseResp struct {
	MapName  string       `json:"mapName"`
	Rounds   int          `json:"rounds"`
	Players  []playerResp `json:"players"`
	DemoDir  string       `json:"demoDir"`  // demo 所在目录（供输出目录默认值）
	DemoPath string       `json:"demoPath"` // 实际解析的 .dem 路径（压缩包选择时为解压后的路径）
	Err      string       `json:"error,omitempty"`
}

type detectReq struct {
	DemoPath  string  `json:"demoPath"`
	SteamID64 uint64  `json:"steamId64,string"`
	MaxClips  int     `json:"maxClips"`
	MinScore  float64 `json:"minScore"`
}

type detectResp struct {
	PlayerName string             `json:"playerName"`
	Clips      []highlight.Clip   `json:"clips"`
	Stats      *stats.PlayerStats `json:"stats,omitempty"` // 目标玩家的完整解析统计（与片段检测同一次 demo 解析，不额外开销）
	Err        string             `json:"error,omitempty"`
}

type renderReq struct {
	DemoPath   string           `json:"demoPath"`
	SteamID64  uint64           `json:"steamId64,string"`
	Clips      []highlight.Clip `json:"clips"`
	OutDir     string           `json:"outDir"`
	Res        string           `json:"res"`
	FPS        int              `json:"fps"`
	CRF        int              `json:"crf"`
	Preset     string           `json:"preset"`
	View       string           `json:"view"`
	MaxPerPass int              `json:"maxPerPass"`
	SkipConcat bool             `json:"skipConcat"`
	HideWindow bool             `json:"hideWindow"` // 游戏窗口移出屏幕后台渲染
}

type renderResp struct {
	TaskID string `json:"taskId"`
	Err    string `json:"error,omitempty"`
}

// taskStatus 渲染任务状态（全局单任务）。
// doneVideo 渲染产出的视频（含元数据，供历史区展示）。
type doneVideo struct {
	Path        string  `json:"path"`
	Name        string  `json:"name"`
	RelDir      string  `json:"relDir"` // 相对输出目录（"" = 根目录成片，如 "clips/多杀"）
	SizeMB      int64   `json:"sizeMb"`
	DurationSec float64 `json:"durationSec"`
	ModTime     int64   `json:"modTime"` // unix 秒，排序用
}

type taskStatus struct {
	mu        sync.Mutex
	State     string      `json:"state"` // idle / running / done / error / cancelled
	Stage     string      `json:"stage"`
	Done      int         `json:"done"`
	Total     int         `json:"total"`
	Partial   float64     `json:"partial"`   // 当前录制中片段的帧进度（0~1），进度条平滑用
	Percent   float64     `json:"percent"`   // 总体进度（0~1，后端计算，阶段间单调不回退）
	Pass      int         `json:"pass"`      // 当前第几轮（1 起；0 = 未知）
	ClipIndex int         `json:"clipIndex"` // 当前处理的全局片段编号（1 起；0 = 未知）
	Detail    string      `json:"detail"`
	Final     string      `json:"finalVideo,omitempty"`
	Clips     []string    `json:"clipVideos,omitempty"`
	Videos    []doneVideo `json:"videos,omitempty"`  // 最近一次完成的视频列表（成片在前）
	History   []doneVideo `json:"history,omitempty"` // 本次会话累计渲染的视频（新完成的在前）
	Error     string      `json:"error,omitempty"`
	Logs      []string    `json:"logs,omitempty"` // 最近日志（渲染细节）
}

var task = &taskStatus{State: "idle"}

func (t *taskStatus) pushLog(s string) {
	t.mu.Lock()
	t.pushLogUnlocked(s)
	t.mu.Unlock()
}

// pushLogUnlocked 在已持有 t.mu 时追加日志。
func (t *taskStatus) pushLogUnlocked(s string) {
	t.Logs = append(t.Logs, s)
	if len(t.Logs) > 300 {
		t.Logs = t.Logs[len(t.Logs)-300:]
	}
}

// ---- handlers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func handleParse(w http.ResponseWriter, r *http.Request) {
	var req parseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, parseResp{Err: "请求解析失败: " + err.Error()})
		return
	}
	if req.DemoPath == "" {
		writeJSON(w, parseResp{Err: "请选择 demo 文件"})
		return
	}
	// 支持直接选择压缩包（zip/rar/7z/gz/tar.gz）：自动解压后取其中的 .dem
	demoPath, err := demo.ResolveDemoPath(req.DemoPath)
	if err != nil {
		writeJSON(w, parseResp{Err: "解压 demo 失败: " + err.Error()})
		return
	}
	if _, err := os.Stat(demoPath); err != nil {
		writeJSON(w, parseResp{Err: "demo 文件不存在: " + demoPath})
		return
	}
	p, err := demo.Parse(demoPath)
	if err != nil {
		writeJSON(w, parseResp{Err: "解析 demo 失败: " + err.Error()})
		return
	}
	resp := parseResp{MapName: p.MapName, Rounds: len(p.Rounds), DemoDir: filepath.Dir(demoPath), DemoPath: demoPath}
	killCount := map[uint64]int{}
	deathCount := map[uint64]int{}
	// 按击杀时的阵营分拆统计（半场换边，每人都有 T/CT 两段数据）
	tK, tD := map[uint64]int{}, map[uint64]int{}
	ctK, ctD := map[uint64]int{}, map[uint64]int{}
	for i := range p.Kills {
		k := &p.Kills[i]
		killCount[k.Killer]++
		deathCount[k.Victim]++
		if k.KillerTeam == demo.TeamT {
			tK[k.Killer]++
		} else if k.KillerTeam == demo.TeamCT {
			ctK[k.Killer]++
		}
		if k.VictimTeam == demo.TeamT {
			tD[k.Victim]++
		} else if k.VictimTeam == demo.TeamCT {
			ctD[k.Victim]++
		}
	}
	mkStat := func(ks, ds map[uint64]int, id uint64) teamStat {
		k, d := ks[id], ds[id]
		kdr := 0.0
		if d > 0 {
			kdr = float64(k) / float64(d)
		}
		return teamStat{Kills: k, Deaths: d, KDR: kdr}
	}
	for _, pl := range p.Players {
		k, d := killCount[pl.SteamID64], deathCount[pl.SteamID64]
		kdr := 0.0
		if d > 0 {
			kdr = float64(k) / float64(d)
		}
		resp.Players = append(resp.Players, playerResp{
			Name: pl.Name, SteamID64: pl.SteamID64, Team: pl.Team,
			Kills: k, Deaths: d, KDR: kdr,
			T: mkStat(tK, tD, pl.SteamID64), CT: mkStat(ctK, ctD, pl.SteamID64),
		})
	}
	sort.Slice(resp.Players, func(i, j int) bool { return resp.Players[i].Kills > resp.Players[j].Kills })
	writeJSON(w, resp)
}

func handleDetect(w http.ResponseWriter, r *http.Request) {
	var req detectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, detectResp{Err: "请求解析失败: " + err.Error()})
		return
	}
	maxClips := req.MaxClips
	if maxClips <= 0 {
		maxClips = 40
	}
	minScore := req.MinScore
	if minScore <= 0 {
		minScore = 20 // GUI 里放宽阈值，让用户自己筛选
	}
	demoPath, err := demo.ResolveDemoPath(req.DemoPath)
	if err != nil {
		writeJSON(w, detectResp{Err: "解压 demo 失败: " + err.Error()})
		return
	}
	p, err := demo.Parse(demoPath)
	if err != nil {
		writeJSON(w, detectResp{Err: "解析 demo 失败: " + err.Error()})
		return
	}
	name := ""
	for _, pl := range p.Players {
		if pl.SteamID64 == req.SteamID64 {
			name = pl.Name
			break
		}
	}
	if name == "" {
		writeJSON(w, detectResp{Err: "未找到该玩家"})
		return
	}
	det := highlight.Detect(p, req.SteamID64, highlight.Config{
		PreSeconds: 8, PostSeconds: 5, MaxClips: maxClips, MinScore: minScore,
	})
	// 顺带计算目标玩家的完整统计（复用本次解析结果，不再重新解析 demo）
	ms := stats.ComputeAll(p)
	writeJSON(w, detectResp{PlayerName: name, Clips: det.Clips, Stats: stats.FindPlayer(ms, req.SteamID64)})
}

func handleRender(w http.ResponseWriter, r *http.Request) {
	var req renderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, renderResp{Err: "请求解析失败: " + err.Error()})
		return
	}
	task.mu.Lock()
	if task.State == "running" {
		task.mu.Unlock()
		writeJSON(w, renderResp{Err: "已有渲染任务在进行中"})
		return
	}
	task.State = "running"
	task.Stage, task.Done, task.Total = "", 0, len(req.Clips)
	task.Pass, task.ClipIndex, task.Partial, task.Percent = 0, 0, 0, 0
	task.Detail, task.Final, task.Error, task.Logs = "准备中...", "", "", nil
	task.mu.Unlock()

	go runRenderTask(req)
	writeJSON(w, renderResp{TaskID: "main"})
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	task.mu.Lock()
	defer task.mu.Unlock()
	writeJSON(w, task)
}

// handleVideo 提供本地视频文件的流式访问（支持 Range，浏览器可播放拖动）。
func handleVideo(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || !strings.HasSuffix(strings.ToLower(path), ".mp4") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, path)
}

// handleOpenDir 在资源管理器中打开指定文件所在目录并选中该文件。
func handleOpenDir(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, map[string]string{"ok": "false"})
		return
	}
	exec.Command("explorer", "/select,", path).Start()
	writeJSON(w, map[string]string{"ok": "true"})
}

// browseResp 目录浏览结果。
type browseResp struct {
	Path string `json:"path"`
	// 不能用 omitempty：根目录时 Parent 必须显式返回 ""（前端以此识别"回到盘符列表"）
	Parent  string       `json:"parent"`
	Drives  []string     `json:"drives,omitempty"` // path 为空时返回盘符列表
	Entries []browseItem `json:"entries,omitempty"`
	Err     string       `json:"error,omitempty"`
}

type browseItem struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDir     bool   `json:"isDir"`
	IsDemo    bool   `json:"isDemo"`    // .dem 或支持的压缩包（可选择解析）
	IsArchive bool   `json:"isArchive"` // 压缩包（前端显示不同图标）
	SizeMB    int64  `json:"sizeMb"`
	ModTime   int64  `json:"modTime"` // 修改时间（Unix 秒），目录与文件都返回
}

// handleBrowse 浏览本地文件系统（浏览器无法直接获得本地路径，由服务端代理）。
// 请求: GET /api/browse?path=D:\demo
func handleBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		// 返回盘符列表
		var drives []string
		for c := 'A'; c <= 'Z'; c++ {
			d := string(c) + ":\\"
			if _, err := os.Stat(d); err == nil {
				drives = append(drives, d)
			}
		}
		writeJSON(w, browseResp{Drives: drives})
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeJSON(w, browseResp{Path: path, Err: "无法访问目录: " + err.Error()})
		return
	}
	resp := browseResp{Path: filepath.Clean(path)}
	parent := filepath.Dir(path)
	if parent != path {
		resp.Parent = parent
	} else {
		resp.Parent = "" // 根目录（如 C:\）：上级 = 盘符列表
	}
	var dirs, files []browseItem
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(path, e.Name())
		if e.IsDir() {
			dirs = append(dirs, browseItem{Name: e.Name(), Path: full, IsDir: true,
				ModTime: info.ModTime().Unix()})
			continue
		}
		isArchive := demo.IsArchivePath(e.Name())
		if !isArchive && !strings.EqualFold(filepath.Ext(e.Name()), ".dem") {
			continue // 只显示 .dem 与支持的压缩包，目录树保持清爽
		}
		files = append(files, browseItem{Name: e.Name(), Path: full, IsDemo: true,
			IsArchive: isArchive, SizeMB: info.Size() / (1 << 20),
			ModTime: info.ModTime().Unix()})
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	resp.Entries = append(dirs, files...)
	writeJSON(w, resp)
}

// handleVideos 扫描当前 demo 的输出目录，返回已存在的视频列表（含历史渲染）。
// GET /api/videos?demoPath=...&outDir=...
func handleVideos(w http.ResponseWriter, r *http.Request) {
	demoPath := r.URL.Query().Get("demoPath")
	outDir := r.URL.Query().Get("outDir")
	if demoPath == "" {
		writeJSON(w, map[string]any{"videos": []doneVideo{}})
		return
	}
	base := strings.TrimSuffix(filepath.Base(demoPath), filepath.Ext(demoPath))
	dir := filepath.Join(outDir, base+"_highlights")
	if outDir == "" {
		dir = filepath.Join(filepath.Dir(demoPath), base+"_highlights")
	}
	var paths []string
	// 根目录 mp4（成片）
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(strings.ToLower(e.Name()), ".mp4") {
				paths = append(paths, filepath.Join(dir, e.Name()))
			}
		}
	}
	// clips 子目录递归（分片段）
	_ = filepath.Walk(filepath.Join(dir, "clips"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(strings.ToLower(p), ".mp4") {
			paths = append(paths, p)
		}
		return nil
	})
	writeJSON(w, map[string]any{"videos": videoMetaList(paths, dir), "dir": dir})
}

// videoMetaList 统计一组视频文件的大小、时长、修改时间与相对目录。
func videoMetaList(paths []string, root string) []doneVideo {
	ffprobe := findFFprobe()
	videos := make([]doneVideo, 0, len(paths))
	for _, p := range paths {
		v := doneVideo{Path: p, Name: filepath.Base(p)}
		if info, err := os.Stat(p); err == nil {
			v.SizeMB = info.Size() / (1 << 20)
			v.ModTime = info.ModTime().Unix()
		}
		if root != "" {
			if rel, err := filepath.Rel(root, filepath.Dir(p)); err == nil {
				v.RelDir = filepath.ToSlash(rel)
			}
		}
		if ffprobe != "" {
			cmd := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration",
				"-of", "default=noprint_wrappers=1:nokey=1", p)
			cmd.SysProcAttr = winutil.NoWindow()
			if out, err := cmd.Output(); err == nil {
				fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &v.DurationSec)
			}
		}
		videos = append(videos, v)
	}
	return videos
}

// findFFprobe 在工具链目录查找 ffprobe.exe（与 ffmpeg 同目录或 bin 子目录）。
func findFFprobe() string {
	for _, cand := range []string{
		filepath.Join(exeDir(), "tools", "hlae", "ffmpeg", "ffprobe.exe"),
		filepath.Join(exeDir(), "tools", "hlae", "ffmpeg", "bin", "ffprobe.exe"),
	} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// handleThumb 提供视频首帧缩略图（懒生成 + 缓存到视频同目录的 .thumbs/）。
// GET /api/thumb?path=xxx.mp4 —— 取 1 秒处帧避免开头黑帧。
func handleThumb(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || !strings.HasSuffix(strings.ToLower(path), ".mp4") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	cacheDir := filepath.Join(filepath.Dir(path), ".thumbs")
	thumb := filepath.Join(cacheDir, filepath.Base(path)+".jpg")
	if _, err := os.Stat(thumb); err != nil {
		ffmpeg := ""
		for _, cand := range []string{
			filepath.Join(exeDir(), "tools", "hlae", "ffmpeg", "ffmpeg.exe"),
			filepath.Join(exeDir(), "tools", "hlae", "ffmpeg", "bin", "ffmpeg.exe"),
		} {
			if _, e := os.Stat(cand); e == nil {
				ffmpeg = cand
				break
			}
		}
		if ffmpeg == "" {
			http.Error(w, "ffmpeg not found", http.StatusInternalServerError)
			return
		}
		os.MkdirAll(cacheDir, 0o755)
		cmd := exec.Command(ffmpeg, "-y", "-ss", "1", "-i", path, "-frames:v", "1", "-q:v", "4", thumb)
		cmd.SysProcAttr = winutil.NoWindow()
		if out, err := cmd.CombinedOutput(); err != nil {
			http.Error(w, fmt.Sprintf("缩略图生成失败: %v\n%s", err, out), http.StatusInternalServerError)
			return
		}
	}
	http.ServeFile(w, r, thumb)
}

// handleCancel 取消渲染：置取消标记并终止游戏进程。
// 管线收尾后 Run 返回 ErrCancelled，不再启动下一轮续录。
func handleCancel(w http.ResponseWriter, r *http.Request) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.State != "running" {
		writeJSON(w, map[string]string{"ok": "false"})
		return
	}
	task.pushLogUnlocked("用户取消渲染，正在终止游戏 ...")
	render.Cancel()
	writeJSON(w, map[string]string{"ok": "true"})
}

// runRenderTask 在后台 goroutine 执行渲染并更新任务状态。
func runRenderTask(req renderReq) {
	setStatus := func(ev render.ProgressEvent) {
		task.mu.Lock()
		task.Stage, task.Done, task.Detail, task.Partial = ev.Stage, ev.Done, ev.Detail, ev.Partial
		// warn 事件不带进度（Percent 为零值）：保留上一次进度，避免后端进度
		// 被清零（前端虽有只增不减的兜底，但状态接口本身应保持单调）。
		if ev.Stage != "warn" {
			task.Percent = ev.Percent
		}
		// warn 等事件可能不带轮次/总数：只在有值时更新，避免界面信息被清零
		if ev.Total > 0 {
			task.Total = ev.Total
		}
		if ev.Pass > 0 {
			task.Pass = ev.Pass
		}
		if ev.ClipIndex > 0 {
			task.ClipIndex = ev.ClipIndex
		}
		task.mu.Unlock()
	}
	setResult := func(state, errMsg, final string, clips []string) {
		task.mu.Lock()
		task.State = state
		if errMsg != "" {
			task.Error = errMsg
		}
		task.Final, task.Clips = final, clips
		task.mu.Unlock()
	}
	// taskLog 追加带时间戳的任务日志（渲染管线的实时日志走这里；
	// stdout 由渲染包的 logf 自己写，避免重复输出）。
	taskLog := func(s string) {
		task.pushLog(time.Now().Format("15:04:05") + "  " + s)
	}
	logf := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		taskLog(s)
		fmt.Println(s)
	}
	// 兜底：渲染 goroutine 里的普通 panic 不能带走整个 GUI 进程（否则窗口
	// 凭空消失、游戏停在可见状态）。转成任务错误展示，并把堆栈写进 GUI 日志。
	// 注意：内存踩踏等 fatal error 无法 recover，只能在源头修复（见 audio.go）。
	defer func() {
		if r := recover(); r != nil {
			logf("渲染任务异常终止: %v\n%s", r, debug.Stack())
			setResult("error", fmt.Sprintf("渲染任务异常: %v（详情见 cs2hi_gui.log）", r), "", nil)
		}
	}()

	if len(req.Clips) == 0 {
		setResult("error", "未选择任何片段", "", nil)
		return
	}
	// 输入可能是压缩包：解压出实际 .dem（playdemo 与输出目录命名都用它）
	demoPath, err := demo.ResolveDemoPath(req.DemoPath)
	if err != nil {
		setResult("error", "解压 demo 失败: "+err.Error(), "", nil)
		return
	}
	// 查找目标玩家名（渲染锁定观战视角需要）
	p, err := demo.Parse(demoPath)
	if err != nil {
		setResult("error", "解析 demo 失败: "+err.Error(), "", nil)
		return
	}
	pl := demo.PlayerInfo{}
	for _, pp := range p.Players {
		if pp.SteamID64 == req.SteamID64 {
			pl = pp
			break
		}
	}
	if pl.Name == "" {
		setResult("error", "未找到该玩家", "", nil)
		return
	}
	otherName, otherEntityID := "", 0
	for _, o := range p.Players {
		// SteamID64=0 是 CSTV/平台观察者等非玩家条目，spec_player 切过去可能
		// 落到无效目标；中转必须是真实玩家实体。
		if o.SteamID64 == 0 || o.SteamID64 == pl.SteamID64 || o.EntityID <= 0 {
			continue
		}
		otherName, otherEntityID = o.Name, o.EntityID
		break
	}

	// 输出目录：统一在所选目录下自动创建 <demo名>_highlights 子目录，
	// 不直接在用户选中的目录里铺文件；留空 = demo 所在目录
	base := strings.TrimSuffix(filepath.Base(demoPath), filepath.Ext(demoPath))
	outDir := filepath.Join(req.OutDir, base+"_highlights")
	if req.OutDir == "" {
		outDir = filepath.Join(filepath.Dir(demoPath), base+"_highlights")
	}
	fps := req.FPS
	if fps == 0 {
		fps = 60
	}
	crf := req.CRF
	if crf == 0 {
		crf = 18
	}
	preset := req.Preset
	if preset == "" {
		preset = "medium"
	}
	view := req.View
	if view == "" {
		view = "pov"
	}
	res := req.Res
	if res == "" {
		res = "1920x1080"
	}
	maxPerPass := req.MaxPerPass
	if maxPerPass == 0 {
		maxPerPass = 6
	}

	logf("[1/3] 检查渲染工具链 ...")
	tp, err := render.EnsureTools(exeDir())
	if err != nil {
		setResult("error", err.Error(), "", nil)
		return
	}
	logf("[2/3] 生成录制配置（%d 个片段）...", len(req.Clips))
	logf("输出目录: %s", outDir)
	opts := render.Options{
		DemoPath:            demoPath,
		PlayerName:          pl.Name,
		OtherPlayerName:     otherName,
		PlayerEntityID:      pl.EntityID,
		OtherPlayerEntityID: otherEntityID,
		PlayerID:            pl.SteamID64,
		Clips:               req.Clips,
		TickRate:            p.TickRate,
		OutDir:              outDir,
		Resolution:          res,
		FPS:                 fps,
		CRF:                 crf,
		Preset:              preset,
		View:                view,
		Server:              "pw",
		MaxPerPass:          maxPerPass,
		SkipConcat:          req.SkipConcat,
		// GUI 渲染默认后台进行：游戏窗口移出屏幕，不打扰用户
		HideWindow: true,
		Progress: func(ev render.ProgressEvent) {
			setStatus(ev)
			// 警告文本已由渲染包通过 Log 写入任务日志（见 logf），这里不重复记录
		},
		// 渲染管线的实时日志（轮次启动/开始录制/编码/合成/警告）写入任务日志，
		// 前端每 600ms 轮询展示；渲染包同时把同样的内容打到 cs2hi_gui.log。
		Log: taskLog,
	}
	logf("[3/3] 启动渲染流水线 ...")
	result, err := render.Run(tp, opts)
	if err != nil {
		if errors.Is(err, render.ErrCancelled) {
			logf("已取消渲染（已录制的片段保留在输出目录 raw/）")
			setResult("cancelled", "", "", nil)
			return
		}
		setResult("error", err.Error(), "", nil)
		return
	}
	final := result.FinalVideo
	msg := "渲染完成"
	if final == "" {
		msg = "片段渲染完成（已跳过合成）"
	}
	// 收集视频元数据（大小 + 时长），供历史区展示；成片置顶；新完成的追加到会话历史
	videos := collectVideoMeta(tp.FFmpeg, final, result.ClipVideos)
	task.mu.Lock()
	task.Videos = videos
	task.History = append(videos, task.History...)
	task.mu.Unlock()
	setResult("done", "", final, result.ClipVideos)
	logf("%s", msg)
}

// collectVideoMeta 统计视频文件大小与时长（成片在前，分片段随后）。
func collectVideoMeta(ffmpegPath, final string, clips []string) []doneVideo {
	paths := []string{}
	if final != "" {
		paths = append(paths, final)
	}
	paths = append(paths, clips...)
	return videoMetaList(paths, "")
}

// handleDelete 删除视频文件（历史区的删除按钮）。
// 仅允许删除 .mp4 文件（本地个人工具，用户通过自己界面操作）。
func handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"ok": "false", "err": "请求解析失败"})
		return
	}
	if req.Path == "" || !strings.HasSuffix(strings.ToLower(req.Path), ".mp4") {
		writeJSON(w, map[string]string{"ok": "false", "err": "仅支持删除 mp4 文件"})
		return
	}
	// 重试：播放器/缩略图进程可能刚释放文件句柄（Windows 共享冲突），
	// 立即删除偶发失败；短重试可消除这类瞬时占用。
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = os.Remove(req.Path); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		writeJSON(w, map[string]string{"ok": "false", "err": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// exeDir 返回可执行文件所在目录（工具链根目录）。
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}
