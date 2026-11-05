// Package highlight 负责从事件日志中检测高光时刻并生成剪辑片段。
//
// 检测模型：先对每个事件打分生成"时刻"（Moment），按回合分组，
// 每回合内把时间窗口重叠的时刻合并成片段（Clip），逐回合给出集锦分析结果。
package highlight

import (
	"fmt"
	"sort"
	"strings"

	"cs2-highlights/internal/demo"
)

// Config 是集锦检测参数。
type Config struct {
	PreSeconds  float64 `json:"pre_seconds"`
	PostSeconds float64 `json:"post_seconds"`
	MaxClips    int     `json:"max_clips"`
	MinScore    float64 `json:"min_score"`
}

// DefaultConfig 返回默认参数。
func DefaultConfig() Config {
	return Config{PreSeconds: 8, PostSeconds: 5, MaxClips: 12, MinScore: 40}
}

const (
	streakWindowTicks = 512  // 连杀判定窗口：8 秒
	tradeWindowTicks  = 256  // 补枪判定窗口：4 秒
	ecoKillerMax      = 1000 // 击杀者装备价值低于该值视为 ECO
	ecoVictimMin      = 3000 // 受害者装备价值高于该值视为全起
	longRangeUnits    = 1000 // 远距离击杀阈值（游戏单位）
	clutchSpanSec     = 25   // 残局片段最多回看 25 秒
)

// Moment 是一次高光时刻。
type Moment struct {
	Tick         int      `json:"tick"`
	Round        int      `json:"round"`
	Score        float64  `json:"score"`
	Tags         []string `json:"tags"`
	Title        string   `json:"title"`
	Kind         string   `json:"kind"` // "frag"：击杀时刻；"round"：回合级事件
	SpanStartTick int     `json:"span_start_tick,omitempty"` // 片段起始点覆盖（0 = 默认 Tick 前后展开）
}

// Clip 是一个剪辑片段（一个回合的最佳片段，可能合并了多个时刻）。
type Clip struct {
	ID          int      `json:"id"`
	Title       string   `json:"title"`
	Score       float64  `json:"score"`
	Tags        []string `json:"tags"`
	Category    string   `json:"category"` // 残局 / 多杀 / 常规
	Round       int      `json:"round"`
	StartTick   int      `json:"start_tick"`
	EndTick     int      `json:"end_tick"`
	StartSec    float64  `json:"start_sec"`
	EndSec      float64  `json:"end_sec"`
	DurationSec float64  `json:"duration_sec"`
	Kills       int      `json:"kills"`
	Moments     []Moment `json:"moments"`
}

// RoundHighlight 是单个回合（除热身外）的集锦分析结果。
type RoundHighlight struct {
	Round     int      `json:"round"`
	Kills     int      `json:"kills"` // 本回合目标玩家击杀数
	FirstKill int      `json:"first_kill_tick,omitempty"`
	LastKill  int      `json:"last_kill_tick,omitempty"`
	RoundEnd  int      `json:"round_end_tick,omitempty"`
	Score     float64  `json:"score"` // 本回合最佳片段得分（0 = 无高光时刻）
	Title     string   `json:"title,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Qualified bool     `json:"qualified"` // 是否达到入选阈值
}

// Result 是检测结果：片段列表 + 逐回合分析。
type Result struct {
	Clips  []Clip
	Rounds []RoundHighlight
}

type roundCtx struct {
	rec   *demo.RoundRecord
	kills []*demo.KillRecord
}

// Detect 检测指定玩家的高光片段，并逐回合（除热身）给出集锦分析。
func Detect(p *demo.Parsed, playerID uint64, cfg Config) *Result {
	tickRate := p.TickRate
	if tickRate <= 0 {
		tickRate = 64
	}
	pre := int(cfg.PreSeconds * tickRate)
	post := int(cfg.PostSeconds * tickRate)

	roundInfo := map[int]*roundCtx{}
	for i := range p.Rounds {
		roundInfo[p.Rounds[i].Number] = &roundCtx{rec: &p.Rounds[i]}
	}
	for i := range p.Kills {
		k := &p.Kills[i]
		if rc, ok := roundInfo[k.Round]; ok {
			rc.kills = append(rc.kills, k)
		}
	}

	// 每个玩家的击杀列表（按 tick 递增）
	victimKills := map[uint64][]*demo.KillRecord{}
	for i := range p.Kills {
		k := &p.Kills[i]
		victimKills[k.Killer] = append(victimKills[k.Killer], k)
	}
	ownKills := victimKills[playerID]

	// 每回合目标玩家的击杀数、第一个与最后一个击杀 tick
	roundKillCount := map[int]int{}
	roundFirstKill := map[int]int{}
	roundLastKill := map[int]int{}
	for i := range p.Kills {
		k := &p.Kills[i]
		if k.Killer == playerID {
			roundKillCount[k.Round]++
			if roundFirstKill[k.Round] == 0 || k.Tick < roundFirstKill[k.Round] {
				roundFirstKill[k.Round] = k.Tick
			}
			if k.Tick > roundLastKill[k.Round] {
				roundLastKill[k.Round] = k.Tick
			}
		}
	}

	var moments []Moment

	// ---- 击杀级时刻 ----
	for i := range p.Kills {
		k := &p.Kills[i]
		if k.Killer != playerID {
			continue
		}
		m := Moment{Tick: k.Tick, Round: k.Round, Kind: "frag"}
		score := 25.0
		var tags []string
		tags = append(tags, k.Weapon)

		if k.IsHeadshot {
			score += 15
			tags = append(tags, "爆头")
		}
		if k.NoScope {
			score += 25
			tags = append(tags, "盲狙")
		}
		if k.ThroughSmoke {
			score += 15
			tags = append(tags, "穿烟")
		}
		if k.WallBang {
			score += 5
			tags = append(tags, "穿墙")
		}
		if k.AttackerBlind {
			score += 15
			tags = append(tags, "被闪白击杀")
		}
		if k.AssistedFlash {
			score += 10
			tags = append(tags, "闪光助攻")
		}
		if k.Distance > longRangeUnits && (k.WeaponClass == "rifle" || k.WeaponClass == "heavy") {
			score += 10
			tags = append(tags, "远距离")
		}
		switch k.WeaponClass {
		case "equipment":
			lw := strings.ToLower(k.Weapon)
			if strings.Contains(lw, "knife") {
				score += 60
				tags = append(tags, "刀杀")
			} else if strings.Contains(lw, "zeus") {
				score += 45
				tags = append(tags, "电击枪")
			}
		case "grenade":
			score += 15
			tags = append(tags, "道具击杀")
		}

		// 连杀：同一回合 8 秒窗口内的击杀数
		streak := 1
		for _, prev := range ownKills {
			if prev.Tick >= k.Tick {
				break
			}
			if prev.Round == k.Round && k.Tick-prev.Tick <= streakWindowTicks {
				streak++
			}
		}
		streak = min(streak, 5)
		if streak >= 2 {
			score += float64(25 * streak)
			tags = append(tags, fmt.Sprintf("%d连杀", streak))
		}

		// 首杀（本回合第一个击杀）
		if rc := roundInfo[k.Round]; rc != nil && len(rc.kills) > 0 && rc.kills[0].Tick == k.Tick {
			score += 25
			tags = append(tags, "首杀")
		}

		// 补枪：受害者 4 秒内击杀过己方队友
		for _, vk := range victimKills[k.Victim] {
			if vk.Tick < k.Tick && k.Tick-vk.Tick <= tradeWindowTicks && vk.VictimTeam == k.KillerTeam {
				score += 15
				tags = append(tags, "补枪")
				break
			}
		}

		// ECO 击杀：手枪/半起打全起
		if k.KillerEquipVal <= ecoKillerMax && k.VictimEquipVal >= ecoVictimMin {
			score += 35
			tags = append(tags, "ECO击杀")
		}

		m.Score = score
		m.Tags = dedupe(tags)
		m.Title = fmt.Sprintf("第%d回合 %s", k.Round, strings.Join(m.Tags, "·"))
		moments = append(moments, m)
	}

	// ---- 回合级时刻：残局、多杀回合 ----
	for i := range p.Rounds {
		r := &p.Rounds[i]
		winnerAlive, loserAlive := r.AliveCT, r.AliveT
		if r.Winner == demo.TeamT {
			winnerAlive, loserAlive = r.AliveT, r.AliveCT
		}
		if r.Winner == demo.TeamT || r.Winner == demo.TeamCT {
			if len(winnerAlive) == 1 && len(loserAlive) >= 1 && winnerAlive[0] == playerID {
				n := len(loserAlive)
				moments = append(moments, Moment{
					Tick:  r.EndTick,
					Round: r.Number,
					Kind:  "round",
					Score: 60 + 50*float64(n),
					Tags:  []string{fmt.Sprintf("1v%d", n), "残局"},
					Title: fmt.Sprintf("第%d回合 1v%d 残局", r.Number, n),
				})
			}
		}
		if n := roundKillCount[r.Number]; n >= 3 {
			// 三杀及以上：单回合多杀是高光（击杀分散时连杀窗口检测不到，这里兜底）。
			// 片段覆盖从本回合第一个击杀到最后一个击杀，确保每个击杀都入镜。
			tag := "三杀"
			if n >= 4 {
				tag = "四杀"
			}
			if n >= 5 {
				tag = "五杀"
			}
			moments = append(moments, Moment{
				Tick:          roundLastKill[r.Number],
				SpanStartTick: roundFirstKill[r.Number],
				Round:         r.Number,
				Kind:          "frag",
				Score:         40 + 40*float64(n),
				Tags:          []string{tag, "多杀回合"},
				Title:         fmt.Sprintf("第%d回合 %s", r.Number, tag),
			})
		}
	}

	// ---- 装包 / 拆弹（只有伴随击杀或残局时才有集锦价值）----
	for i := range p.Bombs {
		b := &p.Bombs[i]
		if b.Player != playerID {
			continue
		}
		hasKills := roundKillCount[b.Round] >= 1
		if b.Kind == "plant" && hasKills {
			moments = append(moments, Moment{
				Tick: b.Tick, Round: b.Round, Kind: "frag", Score: 40,
				Tags:  []string{fmt.Sprintf("%s点下包", b.Site)},
				Title: fmt.Sprintf("第%d回合 %s点下包", b.Round, b.Site),
			})
		} else if b.Kind == "defuse" && (hasKills || isClutchWin(p.Rounds, b.Round, playerID)) {
			moments = append(moments, Moment{
				Tick: b.Tick, Round: b.Round, Kind: "frag", Score: 60,
				Tags:  []string{"拆弹"},
				Title: fmt.Sprintf("第%d回合 拆弹", b.Round),
			})
		}
	}

	// ---- 按回合分组，回合内合并重叠时刻为片段 ----
	byRound := map[int][]Moment{}
	for _, m := range moments {
		byRound[m.Round] = append(byRound[m.Round], m)
	}

	// 每回合的下一回合开始 tick（片段后摇不截断到回合结束，
	// 回合结束后的 walk 阶段可以录，但不能跨入下一回合冻结时间）
	nextRoundStart := map[int]int{}
	for i := range p.Rounds {
		if i+1 < len(p.Rounds) {
			nextRoundStart[p.Rounds[i].Number] = p.Rounds[i+1].StartTick
		}
	}

	var clips []Clip
	var rounds []RoundHighlight
	for i := range p.Rounds {
		r := &p.Rounds[i]
		rh := RoundHighlight{
			Round:     r.Number,
			Kills:     roundKillCount[r.Number],
			FirstKill: roundFirstKill[r.Number],
			LastKill:  roundLastKill[r.Number],
			RoundEnd:  r.EndTick,
		}
		moms := byRound[r.Number]
		if len(moms) == 0 {
			rounds = append(rounds, rh)
			continue
		}
		sort.Slice(moms, func(a, b int) bool { return moms[a].Tick < moms[b].Tick })
		clusters := clusterMoments(moms, r, nextRoundStart[r.Number], pre, post, tickRate)
		// 每回合只选取得分最高的片段（一个回合 = 一个视频）
		best := clusters[0]
		for _, c := range clusters[1:] {
			if c.Score > best.Score {
				best = c
			}
		}
		best.Category = categorize(best.Tags)
		rh.Score = best.Score
		rh.Title = best.Title
		rh.Tags = best.Tags
		rh.Qualified = best.Score >= cfg.MinScore
		rounds = append(rounds, rh)
		clips = append(clips, best)
	}

	// ---- 按得分排序取 Top N ----
	sort.SliceStable(clips, func(i, j int) bool { return clips[i].Score > clips[j].Score })
	var out []Clip
	for _, c := range clips {
		if c.Score < cfg.MinScore {
			continue
		}
		out = append(out, c)
		if len(out) >= cfg.MaxClips {
			break
		}
	}

	// ---- 补充计算字段 ----
	for i := range out {
		c := &out[i]
		c.ID = i + 1
		c.StartSec = float64(c.StartTick) / tickRate
		c.EndSec = float64(c.EndTick) / tickRate
		c.DurationSec = c.EndSec - c.StartSec
		for _, k := range p.Kills {
			if k.Killer == playerID && k.Tick >= c.StartTick && k.Tick <= c.EndTick {
				c.Kills++
			}
		}
	}
	return &Result{Clips: out, Rounds: rounds}
}

// isClutchWin 判断该回合是否为目标玩家的残局胜。
func isClutchWin(rounds []demo.RoundRecord, round int, playerID uint64) bool {
	for i := range rounds {
		r := &rounds[i]
		if r.Number != round {
			continue
		}
		winnerAlive := r.AliveCT
		if r.Winner == demo.TeamT {
			winnerAlive = r.AliveT
		}
		if (r.Winner == demo.TeamT || r.Winner == demo.TeamCT) &&
			len(winnerAlive) == 1 && winnerAlive[0] == playerID {
			return true
		}
	}
	return false
}

// clusterMoments 把同一回合内时间窗口重叠的时刻合并成片段。
// nextStart 是下一回合的开始 tick（后摇上限，0 = 未知则不限制）。
func clusterMoments(moms []Moment, r *demo.RoundRecord, nextStart, pre, post int, tickRate float64) []Clip {
	roundStart := r.FreezeEndTick
	if roundStart <= 0 {
		roundStart = r.StartTick
	}
	roundEnd := r.EndTick

	// 后摇上限：击杀/残局的后摇不因回合结束而截断（最后一杀常与回合结束同一 tick），
	// 但不超过下一回合开始前 1 秒，避免录到冻结时间。
	endCap := func(end int) int {
		if nextStart > 0 && end > nextStart-int(tickRate) {
			end = nextStart - int(tickRate)
		}
		return end
	}

	span := func(m Moment) (int, int) {
		if m.Kind == "round" {
			// 残局：最多回看 clutchSpanSec 秒，结束后最多 3 秒收尾
			start := roundEnd - int(clutchSpanSec*tickRate)
			if start < roundStart {
				start = roundStart
			}
			end := roundEnd + int(3*tickRate)
			return start, endCap(end)
		}
		// 多杀回合：从第一个击杀前 pre 秒覆盖到最后一个击杀后 post 秒
		if m.SpanStartTick > 0 {
			start, end := m.SpanStartTick-pre, m.Tick+post
			if start < roundStart {
				start = roundStart
			}
			if end <= start {
				end = start + int(5*tickRate)
			}
			return start, endCap(end)
		}
		start, end := m.Tick-pre, m.Tick+post
		if start < roundStart {
			start = roundStart
		}
		if end <= start {
			end = start + int(5*tickRate)
		}
		return start, endCap(end)
	}

	var clips []Clip
	for _, m := range moms {
		start, end := span(m)
		merged := false
		for i := range clips {
			c := &clips[i]
			if start <= c.EndTick && c.StartTick <= end {
				if end > c.EndTick {
					c.EndTick = end
				}
				c.Score += m.Score
				c.Tags = dedupe(append(c.Tags, m.Tags...))
				c.Moments = append(c.Moments, m)
				if len(c.Moments) > 1 {
					c.Title = c.Moments[0].Title + fmt.Sprintf(" 等%d个高光", len(c.Moments))
				}
				merged = true
				break
			}
		}
		if !merged {
			clips = append(clips, Clip{
				StartTick: start,
				EndTick:   end,
				Round:     m.Round,
				Score:     m.Score,
				Tags:      m.Tags,
				Title:     m.Title,
				Moments:   []Moment{m},
			})
		}
	}
	return clips
}

// categorize 按标签把片段归类：残局（1vN）> 多杀（3连杀及以上/四杀/五杀）> 常规。
func categorize(tags []string) string {
	has := func(kw string) bool {
		for _, t := range tags {
			if strings.Contains(t, kw) {
				return true
			}
		}
		return false
	}
	if has("残局") {
		return "残局"
	}
	if has("3连杀") || has("4连杀") || has("5连杀") || has("三杀") || has("四杀") || has("五杀") || has("多杀回合") {
		return "多杀"
	}
	return "常规"
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
