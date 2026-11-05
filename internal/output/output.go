// Package output 负责把统计与集锦结果写成 JSON、Markdown 报告和渲染脚本。
package output

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cs2-highlights/internal/highlight"
	"cs2-highlights/internal/stats"
)

// ClipManifest 是集锦片段的输出清单。
type ClipManifest struct {
	GeneratedAt   string                     `json:"generated_at"`
	DemoFile      string                     `json:"demo_file"`
	MapName       string                     `json:"map"`
	TickRate      float64                    `json:"tick_rate"`
	PlayerName    string                     `json:"player_name"`
	PlayerSteamID uint64                     `json:"player_steam_id64"`
	Config        highlight.Config           `json:"config"`
	Clips         []highlight.Clip           `json:"clips"`
	Rounds        []highlight.RoundHighlight `json:"rounds"` // 逐回合集锦分析（除热身）
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// WriteClipsJSON 写出集锦清单。
func WriteClipsJSON(path string, m *ClipManifest) error {
	return writeJSON(path, m)
}

// WriteStatsJSON 写出统计结果。
func WriteStatsJSON(path string, s *stats.MatchStats) error {
	return writeJSON(path, s)
}

// WriteReportMD 写出 Markdown 报告。
func WriteReportMD(path string, s *stats.MatchStats, target *stats.PlayerStats, m *ClipManifest) error {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# CS2 Demo 分析报告：%s\n\n", s.MapName))
	b.WriteString(fmt.Sprintf("- Demo 文件：`%s`\n", filepath.Base(s.DemoFile)))
	b.WriteString(fmt.Sprintf("- 比分：**T %d : %d CT**（共 %d 回合）\n", s.TScore, s.CTScore, s.TotalRounds))
	if s.Winner != "" {
		b.WriteString(fmt.Sprintf("- 胜方：%s\n", s.Winner))
	}
	b.WriteString(fmt.Sprintf("- 生成时间：%s\n\n", time.Now().Format("2006-01-02 15:04:05")))

	b.WriteString("## 计分板\n\n")
	b.WriteString("| 玩家 | 阵营 | K | D | A | K/D | ADR | HS% | 多杀 | 残局 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, p := range s.Players {
		b.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %d | %.2f | %.1f | %.0f%% | %s | %s |\n",
			p.Name, p.Team, p.Kills, p.Deaths, p.Assists, p.KDR, p.ADR, p.HeadshotRate*100,
			formatCountMap(p.MultiKills), formatCountMap(p.ClutchWins)))
	}
	b.WriteString("\n")

	if target != nil {
		b.WriteString(fmt.Sprintf("## 目标玩家：%s 详细统计\n\n", target.Name))
		b.WriteString(fmt.Sprintf("- SteamID64：`%d`，阵营：%s，参与回合：%d\n", target.SteamID64, target.Team, target.RoundsPlayed))
		b.WriteString(fmt.Sprintf("- 击杀 %d / 死亡 %d / 助攻 %d，K/D **%.2f**，MVP %d，得分 %d\n",
			target.Kills, target.Deaths, target.Assists, target.KDR, target.MVPs, target.Score))
		b.WriteString(fmt.Sprintf("- ADR **%.1f**，爆头率 **%.0f%%**，总伤害 %d（含道具伤害 %d）\n",
			target.ADR, target.HeadshotRate*100, target.DamageDealt, target.UtilityDamage))
		b.WriteString(fmt.Sprintf("- 首杀 %d，补枪 %d，被补 %d，KAST **%.0f%%**\n",
			target.EntryKills, target.TradeKills, target.TradedDeaths, target.KAST*100))
		b.WriteString(fmt.Sprintf("- 多杀回合：%s\n", formatCountMap(target.MultiKills)))
		b.WriteString(fmt.Sprintf("- 残局胜：%s，残局负：%s\n",
			formatCountMap(target.ClutchWins), formatCountMap(target.ClutchLosses)))
		b.WriteString(fmt.Sprintf("- 道具：闪光 %d 颗（致盲敌人 %d 人次）、HE %d、烟 %d、火 %d\n",
			target.FlashesThrown, target.EnemiesFlashed, target.HEThrown, target.SmokeThrown, target.MolotovThrown))

		if len(target.Weapons) > 0 {
			b.WriteString("\n### 武器使用\n\n")
			b.WriteString("| 武器 | 击杀 | 爆头 | 伤害 | 开火 |\n|---|---|---|---|---|\n")
			weapons := make([]*stats.WeaponStat, 0, len(target.Weapons))
			names := map[*stats.WeaponStat]string{}
			for name, w := range target.Weapons {
				weapons = append(weapons, w)
				names[w] = name
			}
			sort.Slice(weapons, func(i, j int) bool { return weapons[i].Kills > weapons[j].Kills })
			for _, w := range weapons {
				b.WriteString(fmt.Sprintf("| %s | %d | %d | %d | %d |\n",
					names[w], w.Kills, w.Headshots, w.DamageDealt, w.ShotsFired))
			}
		}

		if len(target.Rounds) > 0 {
			b.WriteString("\n### 逐回合数据\n\n")
			b.WriteString("| 回合 | K | D | A | 伤害 | 胜负 |\n|---|---|---|---|---|---|\n")
			for _, r := range target.Rounds {
				outcome := "败"
				if r.Won {
					outcome = "胜"
				}
				b.WriteString(fmt.Sprintf("| %d | %d | %d | %d | %d | %s |\n",
					r.Round, r.Kills, r.Deaths, r.Assists, r.Damage, outcome))
			}
		}
		b.WriteString("\n")
	}

	// 逐回合集锦分析（每个正式回合都有结论）
	if m != nil && len(m.Rounds) > 0 {
		b.WriteString("## 逐回合集锦分析\n\n")
		b.WriteString("| 回合 | 击杀 | 本回合最佳时刻 | 得分 | 是否入选 |\n|---|---|---|---|---|\n")
		for _, r := range m.Rounds {
			title, score := "-", "-"
			mark := "无"
			if r.Score > 0 {
				title = r.Title
				score = fmt.Sprintf("%.0f", r.Score)
				mark = "否"
				if r.Qualified {
					mark = "✅ 是"
				}
			}
			b.WriteString(fmt.Sprintf("| %d | %d | %s | %s | %s |\n", r.Round, r.Kills, title, score, mark))
		}
		b.WriteString(fmt.Sprintf("\n> 入选标准：片段得分 ≥ %.0f（可用 --min-score 调整），每回合选取得分最高的时刻。\n\n", m.Config.MinScore))
	}

	if m != nil && len(m.Clips) > 0 {
		b.WriteString(fmt.Sprintf("## 精彩集锦片段：%s（共 %d 段）\n\n", m.PlayerName, len(m.Clips)))
		b.WriteString("| # | 回合 | 时间段 | 时长 | 得分 | 击杀 | 标题 |\n|---|---|---|---|---|---|---|\n")
		for _, c := range m.Clips {
			b.WriteString(fmt.Sprintf("| %d | %d | %s - %s | %.1fs | %.0f | %d | %s |\n",
				c.ID, c.Round, fmtSec(c.StartSec), fmtSec(c.EndSec), c.DurationSec, c.Score, c.Kills, c.Title))
		}
		b.WriteString("\n> 在游戏内播放 demo 时可用 `demo_gototick <tick>` 跳到对应片段；自动录制请见 `hlae_record.cfg`。\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// WriteRenderConfig 生成 HLAE 自动录制配置与 FFmpeg 合并清单。
func WriteRenderConfig(dir string, m *ClipManifest) error {
	if len(m.Clips) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("// 由 cs2hi 自动生成 —— HLAE (CS2) 集锦自动录制配置\n")
	b.WriteString("// 用法:\n")
	b.WriteString("//   1. 用 HLAE 启动 CS2（加载 AfxHookSource2）\n")
	b.WriteString("//   2. 游戏内播放本 demo，控制台执行: demo_pause\n")
	b.WriteString("//   3. 控制台执行: exec <本文件完整路径>\n")
	b.WriteString("//   4. 控制台执行: demo_resume —— 到达各片段边界时自动开始/停止录制\n")
	b.WriteString("// 视频默认输出到 <游戏目录>/hlae/videos/，最后用 ffmpeg_concat.txt 合并。\n\n")
	b.WriteString("mirv_streams remove all\n")
	b.WriteString("mirv_streams add normal cs2hi\n")
	b.WriteString("mirv_streams edit cs2hi codec libx264 crf 16\n")
	b.WriteString("mirv_cmd clear\n")
	start0 := m.Clips[0].StartTick - int(2*m.TickRate)
	if start0 < 0 {
		start0 = 0
	}
	b.WriteString(fmt.Sprintf("demo_gototick %d\n", start0))
	for _, c := range m.Clips {
		b.WriteString(fmt.Sprintf("mirv_cmd addAtTick %d \"mirv_streams record start\"\n", c.StartTick))
		b.WriteString(fmt.Sprintf("mirv_cmd addAtTick %d \"mirv_streams record end\"\n", c.EndTick))
	}
	cfg := filepath.Join(dir, "hlae_record.cfg")
	if err := os.WriteFile(cfg, []byte(b.String()), 0o644); err != nil {
		return err
	}

	// FFmpeg 合并清单（concat 格式不支持注释，说明放在 render_guide.md）
	var fb strings.Builder
	for i := range m.Clips {
		fb.WriteString(fmt.Sprintf("file 'clip%04d.mp4'\n", i+1))
	}
	return os.WriteFile(filepath.Join(dir, "ffmpeg_concat.txt"), []byte(fb.String()), 0o644)
}

// WriteRenderGuide 写出渲染操作指南。
func WriteRenderGuide(dir string, m *ClipManifest) error {
	var b strings.Builder
	b.WriteString("# 集锦渲染指南\n\n")
	b.WriteString("## 一键自动渲染（推荐）\n\n")
	b.WriteString("```\ncs2hi render <demo.dem> --player <玩家名>\n```\n\n")
	b.WriteString("自动完成：启动 CS2（HLAE 注入）→ 载入 demo → 按片段边界自动录屏 → 自动退出 → 编码 → 合成成片。\n\n")
	b.WriteString("- 国服版本默认用 `-perfectworld` 启动（跳过服务器选择框），可用 `--server worldwide` 切国际服\n")
	b.WriteString("- 录制分辨率默认跟随系统分辨率，可用 `--res 1280x720` 降低（分辨率越高临时占用越大）\n")
	b.WriteString("- 录制期间游戏窗口会自动打开，请勿操作键盘鼠标\n")
	b.WriteString("- 成片输出到本目录的 `<玩家名>_highlights.mp4`，分片段在 `raw/` 目录\n\n")
	b.WriteString("## 手动方式（备用）\n\n")
	b.WriteString("在游戏内播放 demo，控制台 `demo_gototick <tick>` 跳到各片段（tick 见 `highlight_clips.json`），用录屏软件录制。\n\n")
	b.WriteString("## 片段清单\n\n")
	for _, c := range m.Clips {
		b.WriteString(fmt.Sprintf("- #%d 第%d回合 [tick %d-%d] %s（%.0f 分）\n",
			c.ID, c.Round, c.StartTick, c.EndTick, c.Title, c.Score))
	}
	return os.WriteFile(filepath.Join(dir, "render_guide.md"), []byte(b.String()), 0o644)
}

// formatCountMap 把 {"2k":3,"3k":1} 格式化为 "2k×3, 3k×1"。
func formatCountMap(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func fmtSec(sec float64) string {
	m := int(sec) / 60
	s := int(sec) % 60
	return fmt.Sprintf("%d:%02d", m, s)
}
