// cs2hi —— CS2 demo 分析与精彩集锦生成工具。
//
// 子命令：
//
//	players  列出 demo 中所有玩家
//	stats    输出比赛与玩家统计
//	clips    检测指定玩家的精彩集锦片段
//	all      统计 + 集锦 + 渲染脚本（推荐）
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cs2-highlights/internal/demo"
	"cs2-highlights/internal/gui"
	"cs2-highlights/internal/highlight"
	"cs2-highlights/internal/output"
	"cs2-highlights/internal/render"
	"cs2-highlights/internal/stats"
)

func main() {
	// GUI 子系统构建（-H=windowsgui）下，从命令行启动时把输出挂回父控制台；
	// 双击启动无父控制台，保持静默不弹黑窗。见 console_windows.go。
	attached := attachConsole()
	// 双击启动（无参数）默认打开图形界面；CLI 用户仍可用子命令
	if len(os.Args) < 2 {
		// GUI 模式：解绑控制台，关闭启动它的 cmd 窗口不会牵连前端窗口
		// （双击启动时 attachConsole 返回 false，无需解绑）
		if attached {
			freeConsole()
		}
		if err := gui.Run(0, true); err != nil {
			fatal("%v\n", err)
		}
		return
	}
	switch os.Args[1] {
	case "gui":
		// 同双击启动：解绑控制台，关闭启动它的 cmd 窗口不会牵连前端窗口
		if attached {
			freeConsole()
		}
		port := 0
		if len(os.Args) > 2 {
			port, _ = strconv.Atoi(os.Args[2])
		}
		if err := gui.Run(port, true); err != nil {
			fatal("%v\n", err)
		}
	case "players":
		cmdPlayers(os.Args[2:])
	case "stats":
		cmdStats(os.Args[2:])
	case "clips":
		cmdClips(os.Args[2:])
	case "all":
		cmdAll(os.Args[2:])
	case "render":
		cmdRender(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "package":
		cmdPackage(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`cs2hi —— CS2 demo 分析与精彩集锦生成工具

用法:
  cs2hi                                               打开图形界面（推荐，双击即可）
  cs2hi gui [端口]                                     打开图形界面（指定端口）
  cs2hi players <demo>                                列出 demo 中所有玩家
  cs2hi stats   <demo> [--player <名字|ID>]           输出比赛与玩家统计
  cs2hi clips   <demo> --player <名字|ID> [选项]      检测精彩集锦片段
  cs2hi all     <demo> --player <名字|ID> [选项]      统计 + 集锦 + 渲染脚本
  cs2hi render  <demo> --player <名字|ID> [选项]      全自动渲染成片（一键）
  cs2hi setup                                         检查/自动下载渲染工具链
  cs2hi package [--light]                             打包成可分发的 zip

<demo> 支持 .dem 或压缩包（.zip / .rar / .7z / .dem.gz / .tar.gz），压缩包自动解压。

clips/all/render 选项:
  --max N        最多输出片段数（默认 12）
  --pre S        高光前保留秒数（默认 8）
  --post S       高光后保留秒数（默认 5）
  --min-score F  片段最低得分（默认 40）
  -o DIR         输出目录（默认 <工具目录>\output\<demo名>_out）

render 专属选项:
  --res WxH      录制分辨率（默认 1920x1080）
  --fps N        录制帧率（默认 60）
  --crf N        视频质量，越小越清晰（默认 18）
  --preset S     x264 编码预设: fast/medium/slow（默认 medium）
  --view V       录制视角: pov（第一人称，默认）/ third（第三人称跟随）/ default（GOTV 导播）
  --server S     启动服务器: pw（国服，默认）/ worldwide（国际服）/ none（弹选择框）
  --hlae PATH    HLAE.exe 路径（默认自动检测/下载）
  --cs2 PATH     cs2.exe 路径（默认自动检测）
  --keep-raw     原始分片段视频保留在游戏目录
  --no-quit      录制完成后不自动退出游戏
  --max-per-pass N  每轮（一次游戏启动）最多录制片段数（默认 6，0 = 不限制）
`)
}

// absPath 把路径转为绝对路径（Windows 下 Abs 不会失败，出错时原样返回）。
// 渲染时 demo 路径会写进 cfg 由 CS2 加载，CS2 工作目录在游戏目录，
// 相对路径会找不到文件（playdemo 静默失败），因此必须在这里统一转换。
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// loadDemo 解析 demo 并打印进度。
func loadDemo(path string) *demo.Parsed {
	path = absPath(path)
	// 支持直接选择压缩包：自动解压出 .dem（zip/gz/tar 标准库，rar/7z 用系统 tar.exe）
	resolved, err := demo.ResolveDemoPath(path)
	if err != nil {
		fatal("%v\n", err)
	}
	if resolved != path {
		fmt.Printf("已解压 demo: %s\n", resolved)
	}
	path = resolved
	start := time.Now()
	fmt.Printf("正在解析 demo: %s ...\n", path)
	p, err := demo.Parse(path)
	if err != nil {
		fatal("%v\n", err)
	}
	fmt.Printf("解析完成，用时 %s（地图: %s，回合: %d，击杀: %d）\n",
		time.Since(start).Round(time.Millisecond), p.MapName, len(p.Rounds), len(p.Kills))
	return p
}

// findPlayer 按 SteamID64 或名字匹配玩家。
func findPlayer(p *demo.Parsed, query string) demo.PlayerInfo {
	q := strings.TrimSpace(query)
	if q == "" {
		fatal("缺少 --player 参数\n")
	}
	if id, ok := parseSteamID(q); ok {
		for _, pl := range p.Players {
			if pl.SteamID64 == id {
				return pl
			}
		}
		fatal("demo 中未找到 SteamID %d 的玩家\n", id)
	}
	var matches []demo.PlayerInfo
	for _, pl := range p.Players {
		if strings.EqualFold(pl.Name, q) {
			return pl
		}
		if strings.Contains(strings.ToLower(pl.Name), strings.ToLower(q)) {
			matches = append(matches, pl)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	if len(matches) > 1 {
		fmt.Fprintf(os.Stderr, "多个玩家匹配 \"%s\"，请使用完整名字或 SteamID:\n", q)
		for _, m := range matches {
			fmt.Fprintf(os.Stderr, "  %s (steamid: %d)\n", m.Name, m.SteamID64)
		}
		os.Exit(1)
	}
	fatal("未找到玩家 \"%s\"，可用 `cs2hi players <demo>` 查看全部玩家\n", q)
	return demo.PlayerInfo{}
}

func parseSteamID(s string) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// reorderArgs 把位置参数挪到末尾再交给 flag 包解析。
// Go 的 flag 包遇到第一个非 flag 参数就停止解析，
// 而本工具的用法是 "cs2hi all <demo> --player X"，因此需要先重排。
// 约定：所有 flag 都带值（无 bool 型 flag），支持 --flag=value 形式。
func reorderArgs(args []string) []string {
	var pos, out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		value, hasValue := "", false
		if eq := strings.Index(name, "="); eq >= 0 {
			value, hasValue = name[eq+1:], true
			name = name[:eq]
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value, hasValue = args[i+1], true
			i++
		}
		out = append(out, "--"+name)
		if hasValue {
			out = append(out, value)
		}
	}
	return append(out, pos...)
}

// outDir 计算输出目录：默认在工具根目录的 output/<demo名>_out/ 下。
func outDir(flagDir, demoPath string) string {
	if flagDir != "" {
		return flagDir
	}
	root := "."
	if exe, err := os.Executable(); err == nil {
		root = filepath.Dir(exe)
	}
	base := strings.TrimSuffix(filepath.Base(demoPath), filepath.Ext(demoPath))
	return filepath.Join(root, "output", base+"_out")
}

// exeDir 返回 cs2hi.exe 所在目录（工具的根目录）。
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(1)
}

// fmtSec 把秒数格式化为 mm:ss。
func fmtSec(sec float64) string {
	m := int(sec) / 60
	s := int(sec) % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// ---- players ----

func cmdPlayers(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "用法: cs2hi players <demo>")
		os.Exit(2)
	}
	p := loadDemo(args[0])
	ms := stats.ComputeAll(p)
	fmt.Printf("\n共 %d 名玩家:\n\n", len(ms.Players))
	printScoreboard(ms)
}

func printScoreboard(ms *stats.MatchStats) {
	fmt.Printf("%-24s %-14s %-4s %-4s %-4s %-6s %-6s %-4s\n",
		"玩家", "SteamID64", "阵营", "K", "D", "K/D", "ADR", "HS%")
	for _, pl := range ms.Players {
		fmt.Printf("%-24s %-14d %-4s %-4d %-4d %-6.2f %-6.1f %-4.0f%%\n",
			truncate(pl.Name, 24), pl.SteamID64, pl.Team, pl.Kills, pl.Deaths, pl.KDR, pl.ADR, pl.HeadshotRate*100)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- stats ----

func cmdStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	player := fs.String("player", "", "目标玩家（名字或 SteamID64），不填则只输出全场统计")
	out := fs.String("o", "", "输出目录（默认 <demo名>_out）")
	fs.Parse(reorderArgs(args))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: cs2hi stats <demo> [--player <名字>] [-o DIR]")
		os.Exit(2)
	}
	p := loadDemo(fs.Arg(0))
	ms := stats.ComputeAll(p)
	dir := outDir(*out, fs.Arg(0))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("创建输出目录失败: %v\n", err)
	}

	statsPath := filepath.Join(dir, "player_stats.json")
	if err := output.WriteStatsJSON(statsPath, ms); err != nil {
		fatal("写出统计失败: %v\n", err)
	}
	fmt.Printf("\n比赛统计已输出: %s\n", statsPath)
	printScoreboard(ms)

	if *player != "" {
		pl := findPlayer(p, *player)
		target := stats.FindPlayer(ms, pl.SteamID64)
		if target == nil {
			fatal("玩家 %s 不在本场比赛中\n", pl.Name)
		}
		m := &output.ClipManifest{PlayerName: target.Name, PlayerSteamID: target.SteamID64}
		reportPath := filepath.Join(dir, "report.md")
		if err := output.WriteReportMD(reportPath, ms, target, m); err != nil {
			fatal("写出报告失败: %v\n", err)
		}
		fmt.Printf("玩家报告已输出: %s\n", reportPath)
	}
}

// ---- clips ----

func cmdClips(args []string) {
	fs := flag.NewFlagSet("clips", flag.ExitOnError)
	player := fs.String("player", "", "目标玩家（名字或 SteamID64）")
	maxClips := fs.Int("max", 12, "最多输出的片段数")
	pre := fs.Float64("pre", 8, "高光时刻前保留秒数")
	post := fs.Float64("post", 5, "高光时刻后保留秒数")
	minScore := fs.Float64("min-score", 40, "片段最低得分")
	out := fs.String("o", "", "输出目录（默认 <demo名>_out）")
	fs.Parse(reorderArgs(args))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: cs2hi clips <demo> --player <名字> [--max 12] [--pre 8] [--post 5] [-o DIR]")
		os.Exit(2)
	}
	p := loadDemo(fs.Arg(0))
	pl := findPlayer(p, *player)

	cfg := highlight.Config{
		PreSeconds:  *pre,
		PostSeconds: *post,
		MaxClips:    *maxClips,
		MinScore:    *minScore,
	}
	det := highlight.Detect(p, pl.SteamID64, cfg)
	clips := det.Clips

	dir := outDir(*out, fs.Arg(0))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("创建输出目录失败: %v\n", err)
	}
	m := &output.ClipManifest{
		GeneratedAt:   time.Now().Format(time.RFC3339),
		DemoFile:      fs.Arg(0),
		MapName:       p.MapName,
		TickRate:      p.TickRate,
		PlayerName:    pl.Name,
		PlayerSteamID: pl.SteamID64,
		Config:        cfg,
		Clips:         clips,
		Rounds:        det.Rounds,
	}
	path := filepath.Join(dir, "highlight_clips.json")
	if err := output.WriteClipsJSON(path, m); err != nil {
		fatal("写出片段清单失败: %v\n", err)
	}
	fmt.Printf("\n%s 的精彩集锦共检测到 %d 个片段，已输出: %s\n", pl.Name, len(clips), path)
	printClipTable(clips)
}

// ---- all ----

func cmdAll(args []string) {
	fs := flag.NewFlagSet("all", flag.ExitOnError)
	player := fs.String("player", "", "目标玩家（名字或 SteamID64）")
	maxClips := fs.Int("max", 12, "最多输出的片段数")
	pre := fs.Float64("pre", 8, "高光时刻前保留秒数")
	post := fs.Float64("post", 5, "高光时刻后保留秒数")
	minScore := fs.Float64("min-score", 40, "片段最低得分")
	out := fs.String("o", "", "输出目录（默认 <demo名>_out）")
	fs.Parse(reorderArgs(args))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: cs2hi all <demo> --player <名字> [-o DIR]")
		os.Exit(2)
	}
	demoPath := absPath(fs.Arg(0))
	p := loadDemo(demoPath)
	demoPath = p.Path // 输入压缩包时解析的是解压后的 .dem，输出目录/清单都按它命名
	pl := findPlayer(p, *player)

	cfg := highlight.Config{
		PreSeconds:  *pre,
		PostSeconds: *post,
		MaxClips:    *maxClips,
		MinScore:    *minScore,
	}

	fmt.Println("\n[1/3] 聚合比赛统计 ...")
	ms := stats.ComputeAll(p)
	target := stats.FindPlayer(ms, pl.SteamID64)
	if target == nil {
		fatal("玩家 %s 不在本场比赛中\n", pl.Name)
	}

	fmt.Println("[2/3] 检测精彩集锦 ...")
	det := highlight.Detect(p, pl.SteamID64, cfg)
	clips := det.Clips

	fmt.Println("[3/3] 写出结果 ...")
	dir := outDir(*out, demoPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("创建输出目录失败: %v\n", err)
	}
	m := &output.ClipManifest{
		GeneratedAt:   time.Now().Format(time.RFC3339),
		DemoFile:      demoPath,
		MapName:       p.MapName,
		TickRate:      p.TickRate,
		PlayerName:    pl.Name,
		PlayerSteamID: pl.SteamID64,
		Config:        cfg,
		Clips:         clips,
		Rounds:        det.Rounds,
	}
	writeOrDie(filepath.Join(dir, "player_stats.json"), func(path string) error {
		return output.WriteStatsJSON(path, ms)
	})
	writeOrDie(filepath.Join(dir, "highlight_clips.json"), func(path string) error {
		return output.WriteClipsJSON(path, m)
	})
	writeOrDie(filepath.Join(dir, "report.md"), func(path string) error {
		return output.WriteReportMD(path, ms, target, m)
	})
	writeOrDie(filepath.Join(dir, "hlae_record.cfg"), func(path string) error {
		return output.WriteRenderConfig(dir, m)
	})
	writeOrDie(filepath.Join(dir, "render_guide.md"), func(path string) error {
		return output.WriteRenderGuide(dir, m)
	})

	fmt.Printf("\n完成！结果已输出到目录 %s:\n", dir)
	fmt.Printf("  - player_stats.json    全场与玩家统计数据\n")
	fmt.Printf("  - highlight_clips.json 集锦片段清单（tick 范围）\n")
	fmt.Printf("  - report.md            Markdown 分析报告\n")
	fmt.Printf("  - hlae_record.cfg      HLAE 自动录制配置\n")
	fmt.Printf("  - ffmpeg_concat.txt    FFmpeg 合并清单\n")
	fmt.Printf("  - render_guide.md      渲染操作指南\n\n")
	fmt.Printf("比赛: %s  T %d : %d CT，%s 数据: %d杀/%d死，ADR %.1f，共 %d 个高光片段\n",
		p.MapName, ms.TScore, ms.CTScore, target.Name, target.Kills, target.Deaths, target.ADR, len(clips))
	printClipTable(clips)
}

func writeOrDie(path string, fn func(string) error) {
	if err := fn(path); err != nil {
		fatal("写出 %s 失败: %v\n", path, err)
	}
}

func printClipTable(clips []highlight.Clip) {
	if len(clips) == 0 {
		fmt.Println("  （无片段，可尝试降低 --min-score）")
		return
	}
	fmt.Printf("\n%-3s %-5s %-12s %-7s %-6s %-4s %s\n", "#", "回合", "时间段", "时长", "得分", "击杀", "标题")
	for _, c := range clips {
		fmt.Printf("%-3d %-5d %s-%s %-7s %-6.0f %-4d %s\n",
			c.ID, c.Round, fmtSec(c.StartSec), fmtSec(c.EndSec),
			fmt.Sprintf("%.1fs", c.DurationSec), c.Score, c.Kills, c.Title)
	}
}

// ---- render ----

func cmdRender(args []string) {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	player := fs.String("player", "", "目标玩家（名字或 SteamID64）")
	maxClips := fs.Int("max", 12, "最多渲染的片段数")
	pre := fs.Float64("pre", 8, "高光时刻前保留秒数")
	post := fs.Float64("post", 5, "高光时刻后保留秒数")
	minScore := fs.Float64("min-score", 40, "片段最低得分")
	res := fs.String("res", "1920x1080", "录制分辨率（宽x高）")
	fps := fs.Int("fps", 60, "录制帧率")
	crf := fs.Int("crf", 18, "视频质量，越小越清晰（默认 18：体积/质量甜点，16 体积 +56%）")
	preset := fs.String("preset", "medium", "x264 编码预设: fast/medium/slow（默认 medium，体积/质量/时间平衡）")
	view := fs.String("view", "pov", "录制视角: pov（第一人称）/ third（第三人称跟随）/ default（GOTV 导播）")
	hlaePath := fs.String("hlae", "", "HLAE.exe 路径（默认自动检测/下载）")
	cs2Path := fs.String("cs2", "", "cs2.exe 路径（默认自动检测）")
	server := fs.String("server", "pw", "启动服务器: pw（国服）/ worldwide（国际服）/ none（弹出选择框）")
	keepRaw := fs.Bool("keep-raw", false, "原始分片段视频保留在游戏目录")
	noQuit := fs.Bool("no-quit", false, "录制完成后不自动退出游戏")
	maxPerPass := fs.Int("max-per-pass", 6, "每轮（一次游戏启动）最多录制的片段数，自动分轮。降低 TrueSpectate 死锁概率，0 = 不限制")
	reloadSplit := fs.Bool("reload-split", false, "实验性：每轮结束不重启游戏，改为 disconnect+重载 demo 继续录制（游戏一次启动）")
	out := fs.String("o", "", "输出目录（默认 <工具目录>\\output\\<demo名>_out）")
	fs.Parse(reorderArgs(args))
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: cs2hi render <demo> --player <名字> [--view pov|third|default] [--res 1920x1080] [--fps 60]")
		os.Exit(2)
	}
	demoPath := absPath(fs.Arg(0)) // 必须绝对路径：cfg 里的 playdemo 由 CS2 加载，相对路径找不到 demo
	p := loadDemo(demoPath)
	demoPath = p.Path // 输入压缩包时 playdemo 必须指向解压出的 .dem
	pl := findPlayer(p, *player)
	// 中转玩家：预测状态软重置用（任意非目标玩家，锁定前切走再切回）
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

	cfg := highlight.Config{PreSeconds: *pre, PostSeconds: *post, MaxClips: *maxClips, MinScore: *minScore}
	det := highlight.Detect(p, pl.SteamID64, cfg)
	clips := det.Clips
	if len(clips) == 0 {
		fatal("未检测到高光片段（可尝试降低 --min-score）\n")
	}

	// 转绝对路径：FFmpeg concat 列表按列表文件所在目录解析相对路径，
	// 相对 -o 会导致路径拼接重复（输出目录重复出现两次）而合成失败。
	dir := absPath(outDir(*out, demoPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("创建输出目录失败: %v\n", err)
	}

	fmt.Println("[1/3] 检查渲染工具链（首次运行会自动下载 HLAE / FFmpeg）...")
	tp, err := render.EnsureTools(exeDir())
	if err != nil {
		fatal("%v\n", err)
	}
	if *hlaePath != "" {
		tp.HLAE = *hlaePath
	}
	if *cs2Path != "" {
		tp.CS2 = *cs2Path
	}
	if *server == "none" {
		*server = ""
	}
	fmt.Printf("工具链就绪: HLAE=%s\nCS2=%s\n", tp.HLAE, tp.CS2)

	fmt.Println("[2/3] 生成录制配置 ...")
	opts := render.Options{
		DemoPath:            demoPath,
		PlayerName:          pl.Name,
		OtherPlayerName:     otherName,
		PlayerEntityID:      pl.EntityID,
		OtherPlayerEntityID: otherEntityID,
		PlayerID:            pl.SteamID64,
		AccountID:           pl.AccountID,
		Clips:               clips,
		TickRate:            p.TickRate,
		OutDir:              dir,
		Resolution:          *res,
		FPS:                 *fps,
		CRF:                 *crf,
		Preset:              *preset,
		View:                *view,
		HLAEPath:            *hlaePath,
		CS2Path:             *cs2Path,
		Server:              *server,
		NoQuit:              *noQuit,
		KeepRaw:             *keepRaw,
		MaxPerPass:          *maxPerPass,
		ReloadSplit:         *reloadSplit,
	}

	fmt.Printf("[3/3] 启动渲染流水线：%d 个片段，预计需要几分钟 ...\n", len(clips))
	result, err := render.Run(tp, opts)
	if err != nil {
		fatal("%v\n", err)
	}
	fmt.Printf("\n渲染完成！\n  合集成片: %s\n  单回合片段（按分类）:\n", result.FinalVideo)
	for _, v := range result.ClipVideos {
		rel, _ := filepath.Rel(dir, v)
		fmt.Printf("    - %s\n", rel)
	}
}

// ---- setup ----

func cmdSetup(args []string) {
	fmt.Println("检查渲染工具链 ...")
	tp, err := render.EnsureTools(exeDir())
	if err != nil {
		fatal("%v\n", err)
	}
	fmt.Printf("工具链就绪:\n  HLAE:   %s\n  Hook:   %s\n  FFmpeg: %s\n  CS2:    %s\n",
		tp.HLAE, tp.HookDLL, tp.FFmpeg, tp.CS2)
}

// ---- package ----

func cmdPackage(args []string) {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	light := fs.Bool("light", false, "精简包（不含 FFmpeg，首次渲染时自动下载）")
	fs.Parse(reorderArgs(args))

	root := exeDir()
	name := fmt.Sprintf("cs2hi-%s.zip", time.Now().Format("20060102"))
	dst := filepath.Join(root, name)
	f, err := os.Create(dst)
	if err != nil {
		fatal("创建压缩包失败: %v\n", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	addFile := func(src, rel string) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	// cs2hi.exe 自身
	if err := addFile(filepath.Join(root, "cs2hi.exe"), "cs2hi.exe"); err != nil {
		fatal("打包 cs2hi.exe 失败: %v\n", err)
	}
	// HLAE 工具目录
	skipFFmpeg := func(rel string) bool {
		return *light && strings.HasPrefix(rel, "tools/hlae/ffmpeg/")
	}
	hlaeRoot := filepath.Join(root, "tools", "hlae")
	err = filepath.Walk(hlaeRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if skipFFmpeg(rel) {
			return nil
		}
		return addFile(path, rel)
	})
	if err != nil {
		fatal("打包工具目录失败: %v\n", err)
	}
	// 使用说明
	guide := "【cs2hi 使用说明】\n\n" +
		"1. 解压本压缩包到任意目录（路径不含中文更稳妥）\n" +
		"2. 环境要求：Windows 10/11，已安装 Steam 且已安装 CS2（Counter-Strike 2）\n" +
		"3. 使用前先启动 Steam 并登录\n" +
		"4. 常用命令（在本目录打开 PowerShell 或 CMD）：\n" +
		"     cs2hi players <demo>                    列出 demo 中的所有玩家\n" +
		"     cs2hi all <demo> --player <玩家名>      统计分析 + 集锦片段清单\n" +
		"     cs2hi render <demo> --player <玩家名>   全自动渲染集锦视频（推荐）\n" +
		"5. render 首次运行会自动下载 HLAE / FFmpeg（约 120MB，需联网，仅一次）\n" +
		"6. render 过程中游戏窗口会自动打开并开始录制，请勿操作键盘鼠标，\n" +
		"   录制完成后游戏自动退出，成片输出到 output\\<demo名>_out\\ 目录\n" +
		"7. 中文玩家名请在 PowerShell 中使用；CMD 或乱码时用 SteamID 代替玩家名\n"
	w, err := zw.Create("使用说明.txt")
	if err != nil {
		fatal("写入说明失败: %v\n", err)
	}
	if _, err := w.Write([]byte(guide)); err != nil {
		fatal("写入说明失败: %v\n", err)
	}
	if err := zw.Close(); err != nil {
		fatal("完成压缩包失败: %v\n", err)
	}
	fmt.Printf("打包完成: %s\n", dst)
}
