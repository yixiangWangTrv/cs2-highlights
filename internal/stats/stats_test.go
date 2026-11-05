package stats

import (
	"testing"

	"cs2-highlights/internal/demo"
)

// 构造一个 2v2 合成比赛：T 方 A(1)/B(2)，CT 方 X(3)/Y(4)。
func synthDemo() *demo.Parsed {
	p := &demo.Parsed{
		MapName:  "de_test",
		TickRate: 64,
		Players: []demo.PlayerInfo{
			{Name: "A", SteamID64: 1, Team: "T", TeamID: demo.TeamT},
			{Name: "B", SteamID64: 2, Team: "T", TeamID: demo.TeamT},
			{Name: "X", SteamID64: 3, Team: "CT", TeamID: demo.TeamCT},
			{Name: "Y", SteamID64: 4, Team: "CT", TeamID: demo.TeamCT},
		},
		Rounds: []demo.RoundRecord{
			{Number: 1, StartTick: 0, FreezeEndTick: 128, EndTick: 600, Winner: demo.TeamT, WinnerName: "T", AliveT: []uint64{1}},
			{Number: 2, StartTick: 1000, FreezeEndTick: 1128, EndTick: 1700, Winner: demo.TeamT, WinnerName: "T", AliveT: []uint64{1}, AliveCT: []uint64{3}},
			{Number: 3, StartTick: 1900, FreezeEndTick: 1950, EndTick: 2400, Winner: demo.TeamT, WinnerName: "T", AliveT: []uint64{1, 2}, AliveCT: []uint64{4}},
		},
	}
	mk := func(tick, round int, killer, victim uint64, hs bool, equipK, equipV int) demo.KillRecord {
		k := demo.KillRecord{
			Tick: tick, Round: round, Weapon: "AK-47", WeaponClass: "rifle",
			Killer: killer, KillerTeam: demo.TeamT, Victim: victim, VictimTeam: demo.TeamCT,
			IsHeadshot: hs, KillerEquipVal: equipK, VictimEquipVal: equipV,
		}
		if killer == 3 { // X 是 CT
			k.KillerTeam = demo.TeamCT
			k.VictimTeam = demo.TeamT
			k.Weapon = "M4A4"
		}
		return k
	}
	// 第1回合：A 连续击杀 X、Y（爆头 + 普通，8 秒内 → 2连杀；A 是本回合首杀）
	p.Kills = append(p.Kills,
		mk(500, 1, 1, 3, true, 2000, 2000),
		mk(550, 1, 1, 4, false, 2000, 2000),
		// 第2回合：X 击杀 B，4 秒内 A 反杀 X（补枪 + 被补）
		mk(1500, 2, 3, 2, false, 2000, 2000),
		mk(1600, 2, 1, 3, true, 2000, 2000),
		// 第3回合：A 首杀 X（爆头，ECO 击杀：900 vs 3500）
		mk(2000, 3, 1, 3, true, 900, 3500),
	)
	p.Hurts = []demo.HurtRecord{
		{Tick: 500, Round: 1, Attacker: 1, AttackerTeam: demo.TeamT, Victim: 3, VictimTeam: demo.TeamCT, HealthDamage: 100, Weapon: "AK-47"},
		{Tick: 550, Round: 1, Attacker: 1, AttackerTeam: demo.TeamT, Victim: 4, VictimTeam: demo.TeamCT, HealthDamage: 80, Weapon: "AK-47"},
		{Tick: 1500, Round: 2, Attacker: 3, AttackerTeam: demo.TeamCT, Victim: 2, VictimTeam: demo.TeamT, HealthDamage: 100, Weapon: "M4A4"},
		{Tick: 1600, Round: 2, Attacker: 1, AttackerTeam: demo.TeamT, Victim: 3, VictimTeam: demo.TeamCT, HealthDamage: 100, Weapon: "AK-47"},
		{Tick: 2000, Round: 3, Attacker: 1, AttackerTeam: demo.TeamT, Victim: 3, VictimTeam: demo.TeamCT, HealthDamage: 70, Weapon: "AK-47"},
	}
	return p
}

func TestComputeAll(t *testing.T) {
	ms := ComputeAll(synthDemo())
	if ms.TScore != 3 || ms.CTScore != 0 || ms.Winner != "T" {
		t.Fatalf("比分/胜方错误: T%d CT%d winner=%s", ms.TScore, ms.CTScore, ms.Winner)
	}

	a := FindPlayer(ms, 1)
	if a == nil {
		t.Fatal("未找到玩家 A")
	}
	if a.Kills != 4 || a.Deaths != 0 || a.Assists != 0 {
		t.Fatalf("A 的 K/D/A 错误: %d/%d/%d", a.Kills, a.Deaths, a.Assists)
	}
	if a.EntryKills != 2 {
		t.Fatalf("A 的首杀数应为 2（第1、3回合），实际 %d", a.EntryKills)
	}
	if a.TradeKills != 1 {
		t.Fatalf("A 的补枪数应为 1（第2回合反杀 X），实际 %d", a.TradeKills)
	}
	if a.MultiKills["2k"] != 1 {
		t.Fatalf("A 应有 1 个 2 杀回合，实际 %v", a.MultiKills)
	}
	if a.ClutchWins["1v1"] != 1 {
		t.Fatalf("A 应有 1 次 1v1 残局胜（第2回合），实际 %v", a.ClutchWins)
	}
	if a.DamageDealt != 350 {
		t.Fatalf("A 的总伤害应为 350，实际 %d", a.DamageDealt)
	}
	if a.ADR != 116.67 {
		t.Fatalf("A 的 ADR 应为 116.67（350/3），实际 %v", a.ADR)
	}
	if a.HeadshotRate != 0.75 {
		t.Fatalf("A 的爆头率应为 0.75（3/4），实际 %v", a.HeadshotRate)
	}
	if a.KAST != 1 {
		t.Fatalf("A 的 KAST 应为 1（100%%），实际 %v", a.KAST)
	}
	if w := a.Weapons["AK-47"]; w == nil || w.Kills != 4 || w.DamageDealt != 350 {
		t.Fatalf("A 的 AK-47 统计错误: %+v", a.Weapons["AK-47"])
	}

	b := FindPlayer(ms, 2)
	if b == nil || b.TradedDeaths != 1 || b.Deaths != 1 {
		t.Fatalf("B 应 1 死且被补 1 次（第2回合），实际 %+v", b)
	}

	x := FindPlayer(ms, 3)
	if x == nil {
		t.Fatal("未找到玩家 X")
	}
	if x.Kills != 1 || x.Deaths != 3 || x.EntryKills != 1 {
		t.Fatalf("X 的统计错误: K%d D%d entry%d", x.Kills, x.Deaths, x.EntryKills)
	}
	if x.ClutchLosses["1v1"] != 1 {
		t.Fatalf("X 应有 1 次 1v1 残局负（第2回合），实际 %v", x.ClutchLosses)
	}
	if x.KAST != 0.33 {
		t.Fatalf("X 的 KAST 应为 0.33，实际 %v", x.KAST)
	}
}
