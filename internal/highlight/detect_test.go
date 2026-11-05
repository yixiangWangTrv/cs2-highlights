package highlight

import (
	"testing"

	"cs2-highlights/internal/demo"
)

// 复用 stats 包的合成比赛构造（通过公开字段构造同样的数据）。
func synthParsed() *demo.Parsed {
	return &demo.Parsed{
		MapName:  "de_test",
		TickRate: 64,
		Rounds: []demo.RoundRecord{
			{Number: 1, StartTick: 0, FreezeEndTick: 128, EndTick: 600, Winner: demo.TeamT, AliveT: []uint64{1}},
			{Number: 2, StartTick: 1000, FreezeEndTick: 1128, EndTick: 1700, Winner: demo.TeamT, AliveT: []uint64{1}, AliveCT: []uint64{3}},
			{Number: 3, StartTick: 1900, FreezeEndTick: 1950, EndTick: 2400, Winner: demo.TeamT, AliveT: []uint64{1, 2}, AliveCT: []uint64{4}},
		},
		Kills: []demo.KillRecord{
			// 第1回合：A 爆头首杀 + 8 秒内二连杀
			{Tick: 500, Round: 1, Weapon: "AK-47", WeaponClass: "rifle", Killer: 1, KillerTeam: demo.TeamT, Victim: 3, VictimTeam: demo.TeamCT, IsHeadshot: true, KillerEquipVal: 2000, VictimEquipVal: 2000},
			{Tick: 550, Round: 1, Weapon: "AK-47", WeaponClass: "rifle", Killer: 1, KillerTeam: demo.TeamT, Victim: 4, VictimTeam: demo.TeamCT, KillerEquipVal: 2000, VictimEquipVal: 2000},
			// 第2回合：X 杀 B，A 4 秒内补枪反杀
			{Tick: 1500, Round: 2, Weapon: "M4A4", WeaponClass: "rifle", Killer: 3, KillerTeam: demo.TeamCT, Victim: 2, VictimTeam: demo.TeamT, KillerEquipVal: 2000, VictimEquipVal: 2000},
			{Tick: 1600, Round: 2, Weapon: "AK-47", WeaponClass: "rifle", Killer: 1, KillerTeam: demo.TeamT, Victim: 3, VictimTeam: demo.TeamCT, IsHeadshot: true, KillerEquipVal: 2000, VictimEquipVal: 2000},
			// 第3回合：A 首杀 + 爆头 + ECO 击杀
			{Tick: 2000, Round: 3, Weapon: "AK-47", WeaponClass: "rifle", Killer: 1, KillerTeam: demo.TeamT, Victim: 3, VictimTeam: demo.TeamCT, IsHeadshot: true, KillerEquipVal: 900, VictimEquipVal: 3500},
		},
	}
}

func TestDetect(t *testing.T) {
	p := synthParsed()
	cfg := DefaultConfig()
	det := Detect(p, 1, cfg)
	clips := det.Clips

	if len(clips) != 3 {
		t.Fatalf("应检测到 3 个片段，实际 %d: %+v", len(clips), clips)
	}
	// 按得分排序：第2回合（补枪55+残局110=165）> 第1回合（65+75=140）> 第3回合（100）
	if clips[0].Score != 165 || clips[1].Score != 140 || clips[2].Score != 100 {
		t.Fatalf("片段得分排序错误: %v %v %v", clips[0].Score, clips[1].Score, clips[2].Score)
	}
	// 第2回合片段应包含残局 + 补枪标签，且击杀数为 1
	c0 := clips[0]
	if !hasTag(c0.Tags, "残局") || !hasTag(c0.Tags, "补枪") || c0.Kills != 1 {
		t.Fatalf("第2回合片段标签/击杀数错误: %v kills=%d", c0.Tags, c0.Kills)
	}
	if c0.StartTick != 1128 || c0.EndTick != 1836 {
		t.Fatalf("残局片段应覆盖整个回合 [1128,1836]（回合结束后 3 秒收尾，不越入下一回合），实际 [%d,%d]", c0.StartTick, c0.EndTick)
	}
	// 第1回合片段：两个时刻合并，2 连杀 + 首杀
	c1 := clips[1]
	// 后摇不再被回合结束截断（回合末击杀需完整收录），延伸到下一回合前
	if c1.StartTick != 128 || c1.EndTick != 870 || c1.Kills != 2 || len(c1.Moments) != 2 {
		t.Fatalf("第1回合片段错误: [%d,%d] kills=%d moments=%d", c1.StartTick, c1.EndTick, c1.Kills, len(c1.Moments))
	}
	if !hasTag(c1.Tags, "首杀") || !hasTag(c1.Tags, "2连杀") {
		t.Fatalf("第1回合片段标签错误: %v", c1.Tags)
	}
	// 第3回合片段：ECO 击杀
	if !hasTag(clips[2].Tags, "ECO击杀") {
		t.Fatalf("第3回合片段应含 ECO击杀 标签: %v", clips[2].Tags)
	}
	// ID 与时间字段
	if clips[0].ID != 1 || clips[2].ID != 3 {
		t.Fatalf("片段 ID 错误: %d %d %d", clips[0].ID, clips[1].ID, clips[2].ID)
	}
	if clips[2].StartSec != 1950.0/64 || clips[2].EndSec != 2320.0/64 {
		t.Fatalf("第3回合片段时间错误: %v-%v", clips[2].StartSec, clips[2].EndSec)
	}
}

func TestDetectNoKills(t *testing.T) {
	p := synthParsed()
	det := Detect(p, 2, DefaultConfig())
	if clips := det.Clips; clips != nil {
		t.Fatalf("无击杀玩家应无片段，实际 %+v", clips)
	}
}

func TestDetectMinScore(t *testing.T) {
	p := synthParsed()
	cfg := DefaultConfig()
	cfg.MinScore = 150
	det := Detect(p, 1, cfg)
	clips := det.Clips
	if len(clips) != 1 || clips[0].Score != 165 {
		t.Fatalf("最低分 150 时应只剩 1 个片段，实际 %+v", clips)
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
