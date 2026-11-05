package render

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cs2-highlights/internal/highlight"
)

// TestCfgVolumeValue 只应匹配独立的 volume 键，不能误伤名字里带 volume 的其它设置。
func TestCfgVolumeValue(t *testing.T) {
	data := []byte("\t\t\"snd_gamevolume\"\t\t\"1\"\n" +
		"\t\t\"dsp_volume\"\t\t\"0.800000\"\n" +
		"\t\t\"volume\"\t\t\"0.800000\"\n" +
		"\t\t\"snd_voipvolume\"\t\t\"1\"\n")
	val, ok := cfgVolumeValue(data)
	if !ok || val != "0.800000" {
		t.Fatalf("cfgVolumeValue = %q, %v；期望 %q, true", val, ok, "0.800000")
	}
}

func TestCfgVolumeValueMissing(t *testing.T) {
	data := []byte("\t\t\"snd_gamevolume\"\t\t\"1\"\n")
	if val, ok := cfgVolumeValue(data); ok {
		t.Fatalf("不应匹配 snd_gamevolume，却得到 %q", val)
	}
}

// TestSetCfgVolumeValue 替换应保留原有排版，且只改 volume 一行。
func TestSetCfgVolumeValue(t *testing.T) {
	data := []byte("{\n\t\"convars\"\n\t{\n" +
		"\t\t\"snd_gamevolume\"\t\t\"1\"\n" +
		"\t\t\"volume\"\t\t\"0\"\n" +
		"\t\t\"snd_voipvolume\"\t\t\"1\"\n" +
		"\t}\n}\n")
	want := "{\n\t\"convars\"\n\t{\n" +
		"\t\t\"snd_gamevolume\"\t\t\"1\"\n" +
		"\t\t\"volume\"\t\t\"1.000000\"\n" +
		"\t\t\"snd_voipvolume\"\t\t\"1\"\n" +
		"\t}\n}\n"
	orig := append([]byte(nil), data...)
	if got := string(setCfgVolumeValue(data, "1.000000")); got != want {
		t.Fatalf("setCfgVolumeValue =\n%s\n期望\n%s", got, want)
	}
	if !bytes.Equal(data, orig) {
		t.Fatal("setCfgVolumeValue 不应修改入参")
	}
}

// TestSetCfgVolumeValueInvalid 非法值（如空值、命令注入）不应写入配置。
func TestSetCfgVolumeValueInvalid(t *testing.T) {
	data := []byte("\t\t\"volume\"\t\t\"0\"\n")
	for _, bad := range []string{"", "$1", "abc", "-1", "1;quit"} {
		if got := string(setCfgVolumeValue(data, bad)); got != string(data) {
			t.Fatalf("非法值 %q 不应修改配置，得到 %q", bad, got)
		}
	}
}

// TestProgressPercent 总体进度必须夹在 0~1，且录制/编码半程累计不越界。
func TestProgressPercent(t *testing.T) {
	cases := []struct {
		done  float64
		total int
		want  float64
	}{
		{0, 12, 0},
		{3, 12, 0.25},
		{6, 12, 0.5},
		{9, 12, 0.75},
		{12, 12, 1},
		{20, 12, 1},
		{-1, 12, 0},
		{1, 0, 0},
	}
	for _, c := range cases {
		if got := progressPercent(c.done, c.total); got != c.want {
			t.Errorf("progressPercent(%v, %d) = %v，期望 %v", c.done, c.total, got, c.want)
		}
	}
}

// TestGenerateCfgsKeepsGameVolume 回归测试：cfg 里不能出现 volume 0。
// 引擎录制的是最终混音，volume 0 会让 audio.wav 变成静音（实测 -91dB）。
func TestGenerateCfgsKeepsGameVolume(t *testing.T) {
	root := t.TempDir()
	cs2 := filepath.Join(root, "game", "bin", "win64", "cs2.exe")
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		DemoPath:   filepath.Join(root, "demo.dem"),
		PlayerName: "test",
		FPS:        60,
		TickRate:   64,
		OutDir:     outDir,
		Clips:      []highlight.Clip{{StartTick: 1000, EndTick: 2000, DurationSec: 15}},
	}
	cfgDir, err := GenerateCfgs(cs2, outDir, opts.DemoPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cs2hi_auto.cfg", "cs2hi_record.cfg"} {
		data, err := os.ReadFile(filepath.Join(cfgDir, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		if strings.Contains(string(data), "volume 0") {
			t.Errorf("%s 仍包含 volume 0：会把录制的 audio.wav 静音", name)
		}
	}
}

// TestSpecPlayerArg 锁定参数优先用实体索引，名字回退时去掉首尾空白；
// 无法安全嵌入 cfg 命令链的名字（引号/分号/换行）必须放弃。
func TestSpecPlayerArg(t *testing.T) {
	cases := []struct {
		name     string
		entityID int
		want     string
	}{
		{" Hinson1985", 10, "10"}, // CS2 demo 玩家名带前导空格：索引不受影响
		{" Hinson1985", 0, "Hinson1985"},
		{"b哥严父", 0, "b哥严父"},
		{"  ", 0, ""},
		{``, 0, ""},
		{`a"b`, 0, ""},
		{"a;b", 0, ""},
		{"a\r\nb", 0, ""},
	}
	for _, c := range cases {
		if got := specPlayerArg(c.name, c.entityID); got != c.want {
			t.Errorf("specPlayerArg(%q, %d) = %q，期望 %q", c.name, c.entityID, got, c.want)
		}
	}
}

// TestGenerateCfgsLocksPlayerWithSpacedName 回归测试：CS2 demo 的玩家名普遍
// 带前导空格（实测 " Hinson1985"），旧版为避免命令注入跳过含空格的玩家名，
// 导致 cfg 里没有 spec_player，录出来的视角是 GOTV 默认目标而不是所选玩家。
func TestGenerateCfgsLocksPlayerWithSpacedName(t *testing.T) {
	root := t.TempDir()
	cs2 := filepath.Join(root, "game", "bin", "win64", "cs2.exe")
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		DemoPath:            filepath.Join(root, "demo.dem"),
		PlayerName:          " Hinson1985",
		OtherPlayerName:     " b哥严父",
		PlayerEntityID:      10,
		OtherPlayerEntityID: 12,
		View:                "pov",
		FPS:                 60,
		TickRate:            64,
		OutDir:              outDir,
		Clips:               []highlight.Clip{{StartTick: 32801, EndTick: 34593, DurationSec: 28}},
	}
	cfgDir, err := GenerateCfgs(cs2, outDir, opts.DemoPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "cs2hi_record.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	rec := string(data)
	// 软重置先切中转玩家，再切回目标：两条命令都必须在
	if !strings.Contains(rec, "spec_player 12; spec_player 10; spec_mode 1") {
		t.Fatalf("record.cfg 缺少按实体索引的视角锁定：\n%s", rec)
	}
	// 每个片段两次锁定（seek 后 2 秒 + 4 秒），防止 TrueSpectate 重建抢先
	if n := strings.Count(rec, "spec_player 12; spec_player 10"); n != 2 {
		t.Fatalf("应有 2 次锁定命令，实际 %d 次：\n%s", n, rec)
	}
}

// TestGenerateCfgsDisablesTrueView 回归测试：渲染配置必须关闭 TrueView 预测回放。
// CS2 2025-11 起默认启用 TrueView，demo_gototick 跳转后预测拿不到客户端命令
// （控制台："TrueView can't determine last executed command"；HUD：「实况回放:
// 无数据」），第一人称镜头会卡在跳转点——时间和击杀正常，但人物/视角不再跟随。
// auto.cfg / record.cfg / 观战命令链都必须显式关闭，任何一处遗漏都会让镜头卡死。
func TestGenerateCfgsDisablesTrueView(t *testing.T) {
	root := t.TempDir()
	cs2 := filepath.Join(root, "game", "bin", "win64", "cs2.exe")
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		DemoPath:            filepath.Join(root, "demo.dem"),
		PlayerName:          " Hinson1985",
		OtherPlayerName:     " b哥严父",
		PlayerEntityID:      10,
		OtherPlayerEntityID: 12,
		View:                "pov",
		FPS:                 60,
		TickRate:            64,
		OutDir:              outDir,
		ReloadSplit:         true,
		MaxPerPass:          1,
		Clips: []highlight.Clip{
			{StartTick: 32801, EndTick: 34593, DurationSec: 28},
			{StartTick: 42801, EndTick: 44000, DurationSec: 18.7},
		},
	}
	cfgDir, err := GenerateCfgs(cs2, outDir, opts.DemoPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cs2hi_auto.cfg", "cs2hi_record.cfg", "cs2hi_auto1.cfg"} {
		data, err := os.ReadFile(filepath.Join(cfgDir, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		content := string(data)
		for _, cvar := range []string{"cl_demo_predict 0", "cl_spectator_predict 0", "cl_trueview_show_status 0"} {
			if !strings.Contains(content, cvar) {
				t.Errorf("%s 缺少 %q：TrueView 会导致第一人称镜头卡在跳转点\n%s", name, cvar, content)
			}
		}
	}
}

// TestHideGameWindowsCallbackReused 回归测试：EnumWindows 回调必须只注册一次。
// 早期实现每次 hideGameWindows 都新建闭包，50ms 一次会在约 100 秒后耗尽
// Go 运行时的 2000 个 Windows 回调槽位，进程被 throw 直接终止（GUI 无故消失）。
func TestHideGameWindowsCallbackReused(t *testing.T) {
	hideGameWindows(map[int]bool{})
	first := enumWindowsCB
	for i := 0; i < 100; i++ {
		hideGameWindows(map[int]bool{})
	}
	if enumWindowsCB != first {
		t.Fatal("EnumWindows 回调被重复注册：会耗尽 2000 个回调槽位导致进程崩溃")
	}
}

// newCfgTestOpts 构造 GenerateCfgs 测试用的最小 Options。
func newCfgTestOpts(t *testing.T, clips ...highlight.Clip) (string, string, Options) {
	t.Helper()
	root := t.TempDir()
	cs2 := filepath.Join(root, "game", "bin", "win64", "cs2.exe")
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cs2, outDir, Options{
		DemoPath:   filepath.Join(root, "demo.dem"),
		PlayerName: "test",
		FPS:        60,
		TickRate:   64,
		OutDir:     outDir,
		Clips:      clips,
	}
}

// TestGenerateCfgsWaitsBeforeQuit 回归测试：不能录完最后一段立刻 quit。
//
// Windows 按应用持久化音频会话的静音状态；引擎从 "Host activate: Quitting"
// 到进程退出只有几十毫秒，外部进程来不及解除静音，玩家下次正常进游戏就会
// 没声音。cfg 必须在 quit 前插入 demo_pause + wait，给解除静音留出窗口。
func TestGenerateCfgsWaitsBeforeQuit(t *testing.T) {
	cs2, outDir, opts := newCfgTestOpts(t,
		highlight.Clip{StartTick: 1000, EndTick: 2000, DurationSec: 15},
		highlight.Clip{StartTick: 5000, EndTick: 6000, DurationSec: 15},
	)
	cfgDir, err := GenerateCfgs(cs2, outDir, opts.DemoPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "cs2hi_record.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	rec := string(data)
	if n := strings.Count(rec, quitAfterRenderSuffix(opts.FPS)); n != 1 {
		t.Fatalf("quit 收尾应只出现 1 次（最后一段），实际 %d 次：\n%s", n, rec)
	}
	if !strings.Contains(rec, "demo_pause") || !strings.Contains(rec, "wait 90; quit") {
		t.Fatalf("最后一段后缺少 demo_pause + wait 窗口：\n%s", rec)
	}
}

// TestGenerateCfgsNoQuitSkipsWait 调试模式（NoQuit）不退出游戏，也不该插入等待。
func TestGenerateCfgsNoQuitSkipsWait(t *testing.T) {
	cs2, outDir, opts := newCfgTestOpts(t,
		highlight.Clip{StartTick: 1000, EndTick: 2000, DurationSec: 15},
	)
	opts.NoQuit = true
	cfgDir, err := GenerateCfgs(cs2, outDir, opts.DemoPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "cs2hi_record.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := string(data); strings.Contains(rec, "quit") || strings.Contains(rec, "demo_pause") {
		t.Fatalf("NoQuit 模式不应包含 quit/demo_pause：\n%s", rec)
	}
}

// TestGenerateCfgsReloadLastPassWaits 重载链的最后一轮同样需要退出前的等待窗口。
func TestGenerateCfgsReloadLastPassWaits(t *testing.T) {
	cs2, outDir, opts := newCfgTestOpts(t,
		highlight.Clip{StartTick: 1000, EndTick: 2000, DurationSec: 15},
		highlight.Clip{StartTick: 5000, EndTick: 6000, DurationSec: 15},
	)
	opts.ReloadSplit = true
	opts.MaxPerPass = 1
	cfgDir, err := GenerateCfgs(cs2, outDir, opts.DemoPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "cs2hi_auto1.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if last := string(data); !strings.Contains(last, quitAfterRenderSuffix(opts.FPS)) {
		t.Fatalf("重载链最后一轮缺少退出前等待窗口：\n%s", last)
	}
}

// appendLog 向测试日志追加一行。
func appendLog(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestWatchGameQuitOnRecordingDone 回归测试：最后一段录完（"Finishing recording"
// 出现 expected 次）时必须解除静音——此时 cfg 的 wait 窗口还没结束，游戏仍存活。
func TestWatchGameQuitOnRecordingDone(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go watchGameQuit(logPath, 2, stop, func() { done <- struct{}{} })

	// 只录完 1 段：不能解除静音（后面还有片段，解除会外放）
	appendLog(t, logPath, `10/31 20:37:03 Finishing recording ... Finishing movie: take0000 [840 frames]`)
	select {
	case <-done:
		t.Fatal("只录完 1/2 段就解除了静音")
	case <-time.After(500 * time.Millisecond):
	}

	appendLog(t, logPath, `10/31 20:37:26 Finishing recording ... Finishing movie: take0001 [840 frames]`)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("最后一段录完后没有解除静音：下次进游戏会没有声音")
	}
}

// TestWatchGameQuitOnQuitting 兜底信号：引擎开始退出时也要解除静音。
func TestWatchGameQuitOnQuitting(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go watchGameQuit(logPath, 0, stop, func() { done <- struct{}{} })

	appendLog(t, logPath, `10/31 20:37:47 [HostStateManager] Host activate: Quitting`)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Quitting 出现后没有解除静音")
	}
}
