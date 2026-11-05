// Package stats 将 demo 事件日志聚合为玩家与比赛的统计数据。
package stats

import (
	"fmt"
	"math"
	"sort"

	"cs2-highlights/internal/demo"

	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

const tradeWindowTicks = 256 // 补枪判定窗口：4 秒（64 tick）

// WeaponStat 是单个武器的统计数据。
type WeaponStat struct {
	Kills       int `json:"kills"`
	Headshots   int `json:"headshots"`
	DamageDealt int `json:"damage_dealt"`
	ShotsFired  int `json:"shots_fired"`
}

// RoundLine 是玩家单回合的数据。
type RoundLine struct {
	Round   int  `json:"round"`
	Kills   int  `json:"kills"`
	Deaths  int  `json:"deaths"`
	Assists int  `json:"assists"`
	Damage  int  `json:"damage"`
	Won     bool `json:"won"`
}

// PlayerStats 是单名玩家的完整统计。
type PlayerStats struct {
	Name           string                 `json:"name"`
	AccountID      uint32                 `json:"account_id"`
	SteamID64      uint64                 `json:"steam_id64"`
	Team           string                 `json:"team"`
	IsBot          bool                   `json:"is_bot"`
	RoundsPlayed   int                    `json:"rounds_played"`
	Kills          int                    `json:"kills"`
	Deaths         int                    `json:"deaths"`
	Assists        int                    `json:"assists"`
	MVPs           int                    `json:"mvps"`
	Score          int                    `json:"score"`
	KDR            float64                `json:"kdr"`
	ADR            float64                `json:"adr"`
	HeadshotRate   float64                `json:"headshot_rate"`
	DamageDealt    int                    `json:"damage_dealt"`
	UtilityDamage  int                    `json:"utility_damage"`
	EntryKills     int                    `json:"entry_kills"`
	TradeKills     int                    `json:"trade_kills"`
	TradedDeaths   int                    `json:"traded_deaths"`
	MultiKills     map[string]int         `json:"multi_kills"`
	ClutchWins     map[string]int         `json:"clutch_wins"`
	ClutchLosses   map[string]int         `json:"clutch_losses"`
	FlashesThrown  int                    `json:"flashes_thrown"`
	EnemiesFlashed int                    `json:"enemies_flashed"`
	HEThrown       int                    `json:"he_thrown"`
	SmokeThrown    int                    `json:"smoke_thrown"`
	MolotovThrown  int                    `json:"molotov_thrown"`
	KAST           float64                `json:"kast"`
	Weapons        map[string]*WeaponStat `json:"weapons"`
	Rounds         []RoundLine            `json:"rounds"`
}

// MatchStats 是整场比赛的统计结果。
type MatchStats struct {
	DemoFile    string         `json:"demo_file"`
	MapName     string         `json:"map"`
	TickRate    float64        `json:"tick_rate"`
	TotalRounds int            `json:"total_rounds"`
	TScore      int            `json:"t_score"`
	CTScore     int            `json:"ct_score"`
	Winner      string         `json:"winner"`
	Players     []*PlayerStats `json:"players"`
}

// deathKey 唯一标识一次死亡（用于补枪与被补统计）。
type deathKey struct {
	who  uint64
	tick int
}

// FindPlayer 按 SteamID64 在比赛统计中查找玩家，未找到返回 nil。
func FindPlayer(ms *MatchStats, steamID uint64) *PlayerStats {
	for _, p := range ms.Players {
		if p.SteamID64 == steamID {
			return p
		}
	}
	return nil
}

// ComputeAll 聚合整场比赛的统计数据。
func ComputeAll(p *demo.Parsed) *MatchStats {
	ms := &MatchStats{
		DemoFile:    p.Path,
		MapName:     p.MapName,
		TickRate:    p.TickRate,
		TotalRounds: len(p.Rounds),
	}
	for i := range p.Rounds {
		switch p.Rounds[i].Winner {
		case demo.TeamT:
			ms.TScore++
		case demo.TeamCT:
			ms.CTScore++
		}
	}
	if ms.TScore > ms.CTScore {
		ms.Winner = "T"
	} else if ms.CTScore > ms.TScore {
		ms.Winner = "CT"
	}
	// 被补关系是全局的（队友的复仇也计入），先构建一次再分发给每个玩家
	traded := buildTradedMap(p)
	for _, info := range p.Players {
		if info.TeamID != demo.TeamT && info.TeamID != demo.TeamCT {
			continue
		}
		ms.Players = append(ms.Players, computeOne(p, info, traded))
	}
	sort.Slice(ms.Players, func(i, j int) bool {
		return ms.Players[i].Kills > ms.Players[j].Kills
	})
	return ms
}

// buildTradedMap 构建"被补掉的死亡"集合：某玩家死亡后 4 秒内，
// 凶手被该玩家的一名队友击杀，则这次死亡被视为被补（traded）。
func buildTradedMap(p *demo.Parsed) map[deathKey]bool {
	victimKills := map[uint64][]*demo.KillRecord{}
	for i := range p.Kills {
		k := &p.Kills[i]
		victimKills[k.Killer] = append(victimKills[k.Killer], k)
	}
	traded := map[deathKey]bool{}
	for i := range p.Kills {
		k := &p.Kills[i]
		for _, vk := range victimKills[k.Victim] {
			if vk.Tick < k.Tick && k.Tick-vk.Tick <= tradeWindowTicks && vk.VictimTeam == k.KillerTeam {
				traded[deathKey{vk.Victim, vk.Tick}] = true
			}
		}
	}
	return traded
}

func computeOne(p *demo.Parsed, info demo.PlayerInfo, traded map[deathKey]bool) *PlayerStats {
	id := info.SteamID64
	st := &PlayerStats{
		Name:          info.Name,
		SteamID64:     id,
		AccountID:     info.AccountID,
		Team:          info.Team,
		IsBot:         info.IsBot,
		MVPs:          info.MVPs,
		Score:         info.Score,
		UtilityDamage: info.UtilityDamage,
		MultiKills:    map[string]int{},
		ClutchWins:    map[string]int{},
		ClutchLosses:  map[string]int{},
		Weapons:       map[string]*WeaponStat{},
	}

	// 每个玩家的击杀列表（按 tick 递增，用于补枪判定）
	victimKills := map[uint64][]*demo.KillRecord{}
	for i := range p.Kills {
		k := &p.Kills[i]
		victimKills[k.Killer] = append(victimKills[k.Killer], k)
	}

	// 每回合首杀者
	type entryRec struct {
		tick   int
		killer uint64
	}
	entryKill := map[int]entryRec{}
	for i := range p.Kills {
		k := &p.Kills[i]
		if e, ok := entryKill[k.Round]; !ok || k.Tick < e.tick {
			entryKill[k.Round] = entryRec{k.Tick, k.Killer}
		}
	}

	// 每回合胜负
	winByRound := map[int]uint8{}
	for i := range p.Rounds {
		winByRound[p.Rounds[i].Number] = p.Rounds[i].Winner
	}

	type roundAgg struct {
		kills, deaths, assists, dmg int
		deathTicks                  []int
	}
	roundMap := map[int]*roundAgg{}
	ra := func(r int) *roundAgg {
		if roundMap[r] == nil {
			roundMap[r] = &roundAgg{}
		}
		return roundMap[r]
	}

	hsKills := 0

	for i := range p.Kills {
		k := &p.Kills[i]
		if k.Killer == id {
			ra(k.Round).kills++
			if k.IsHeadshot {
				hsKills++
			}
			// 补枪判定：受害者 4 秒内击杀过己方队友
			isTrade := false
			for _, vk := range victimKills[k.Victim] {
				if vk.Tick >= k.Tick-tradeWindowTicks && vk.Tick < k.Tick && vk.VictimTeam == k.KillerTeam {
					isTrade = true
				}
			}
			if isTrade {
				st.TradeKills++
			}
			w := st.wep(k.Weapon)
			w.Kills++
			if k.IsHeadshot {
				w.Headshots++
			}
		}
		if k.Victim == id {
			ra(k.Round).deaths++
			ra(k.Round).deathTicks = append(ra(k.Round).deathTicks, k.Tick)
		}
		if k.Assister == id {
			ra(k.Round).assists++
		}
	}

	for i := range p.Kills {
		k := &p.Kills[i]
		if k.Victim == id && traded[deathKey{id, k.Tick}] {
			st.TradedDeaths++
		}
	}

	// 伤害：只统计对敌方造成的伤害，按受害者剩余血量封顶（HLTV ADR 口径）
	for i := range p.Hurts {
		h := &p.Hurts[i]
		if h.Attacker == id && h.AttackerTeam != h.VictimTeam {
			dmg := h.HealthDamage
			if h.HealthDamageTaken > 0 {
				dmg = h.HealthDamageTaken
			}
			ra(h.Round).dmg += dmg
			st.DamageDealt += dmg
			st.wep(h.Weapon).DamageDealt += dmg
		}
	}

	// 开火统计
	for i := range p.Fires {
		f := &p.Fires[i]
		if f.Shooter == id {
			st.wep(f.Weapon).ShotsFired++
		}
	}

	// 首杀
	for _, e := range entryKill {
		if e.killer == id {
			st.EntryKills++
		}
	}

	// 多杀 / 回合明细
	for rn, agg := range roundMap {
		if agg.kills >= 2 && agg.kills <= 5 {
			st.MultiKills[fmt.Sprintf("%dk", agg.kills)]++
		}
		st.Kills += agg.kills
		st.Deaths += agg.deaths
		st.Assists += agg.assists
		st.Rounds = append(st.Rounds, RoundLine{
			Round:   rn,
			Kills:   agg.kills,
			Deaths:  agg.deaths,
			Assists: agg.assists,
			Damage:  agg.dmg,
			Won:     winByRound[rn] == info.TeamID,
		})
	}

	// 残局
	for i := range p.Rounds {
		r := &p.Rounds[i]
		winnerAlive, loserAlive := r.AliveCT, r.AliveT
		if r.Winner == demo.TeamT {
			winnerAlive, loserAlive = r.AliveT, r.AliveCT
		}
		if r.Winner != demo.TeamT && r.Winner != demo.TeamCT {
			continue
		}
		if len(winnerAlive) == 1 && len(loserAlive) >= 1 && winnerAlive[0] == id {
			st.ClutchWins[fmt.Sprintf("1v%d", len(loserAlive))]++
		}
		if len(loserAlive) == 1 && len(winnerAlive) >= 1 && loserAlive[0] == id {
			st.ClutchLosses[fmt.Sprintf("1v%d", len(winnerAlive))]++
		}
	}

	// 道具统计
	for i := range p.Throws {
		t := &p.Throws[i]
		if t.Thrower != id {
			continue
		}
		switch common.EquipmentType(t.GrenadeType) {
		case common.EqFlash:
			st.FlashesThrown++
		case common.EqHE:
			st.HEThrown++
		case common.EqSmoke:
			st.SmokeThrown++
		case common.EqMolotov, common.EqIncendiary:
			st.MolotovThrown++
		}
	}
	for i := range p.Flashes {
		f := &p.Flashes[i]
		if f.Attacker == id && f.AttackerTeam != f.VictimTeam {
			st.EnemiesFlashed++
		}
	}

	// KAST：回合内有击杀 / 助攻 / 存活 / 被补
	kastRounds := 0
	for _, agg := range roundMap {
		tradedThisRound := false
		for _, t := range agg.deathTicks {
			if traded[deathKey{id, t}] {
				tradedThisRound = true
				break
			}
		}
		if agg.kills > 0 || agg.assists > 0 || agg.deaths == 0 || tradedThisRound {
			kastRounds++
		}
	}

	// 汇总比率
	st.RoundsPlayed = len(roundMap)
	if st.Deaths > 0 {
		st.KDR = round2(float64(st.Kills) / float64(st.Deaths))
	} else {
		st.KDR = float64(st.Kills)
	}
	if st.RoundsPlayed > 0 {
		st.ADR = round2(float64(st.DamageDealt) / float64(st.RoundsPlayed))
		st.KAST = round2(float64(kastRounds) / float64(st.RoundsPlayed))
	}
	if st.Kills > 0 {
		st.HeadshotRate = round2(float64(hsKills) / float64(st.Kills))
	}
	sort.Slice(st.Rounds, func(i, j int) bool { return st.Rounds[i].Round < st.Rounds[j].Round })
	return st
}

func (st *PlayerStats) wep(name string) *WeaponStat {
	if name == "" {
		name = "未知"
	}
	if st.Weapons[name] == nil {
		st.Weapons[name] = &WeaponStat{}
	}
	return st.Weapons[name]
}

// round2 保留两位小数，避免 0.75 之类被四舍五入成 0.8。
func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
