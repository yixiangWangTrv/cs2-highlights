// dumpdemo —— 临时诊断工具：dump 指定回合的完整事件时间线（含玩家连接/断开、slot 号）。
// 用途：分析第 17 回合 27:45 附近 TrueSpectate 死锁的触发事件。
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"

	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

type ev struct {
	tick int
	msg  string
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: dumpdemo <demo.dem> <回合号> [起始tick] [结束tick]")
		os.Exit(2)
	}
	round, _ := strconv.Atoi(os.Args[2])
	startTick, endTick := 0, int(^uint(0)>>1)
	if len(os.Args) > 3 {
		startTick, _ = strconv.Atoi(os.Args[3])
	}
	if len(os.Args) > 4 {
		endTick, _ = strconv.Atoi(os.Args[4])
	}

	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	p := dem.NewParser(f)
	defer p.Close()

	var events_ []ev
	roundNo := 0
	inRound := func(t int) bool {
		return roundNo == round && t >= startTick && t <= endTick
	}
	slotName := map[int]string{}

	p.RegisterEventHandler(func(e events.RoundStart) {
		gs := p.GameState()
		if !gs.IsMatchStarted() && gs.TotalRoundsPlayed() == 0 {
			return
		}
		roundNo++
	})
	p.RegisterEventHandler(func(e events.RoundFreezetimeEnd) {
		if inRound(p.GameState().IngameTick()) {
			events_ = append(events_, ev{p.GameState().IngameTick(), "== 冻结结束"})
		}
	})
	p.RegisterEventHandler(func(e events.RoundEnd) {
		if roundNo == round {
			events_ = append(events_, ev{p.GameState().IngameTick(), fmt.Sprintf("== 回合结束 winner=%s reason=%v", teamName(e.Winner), e.Reason)})
		}
	})
	p.RegisterEventHandler(func(e events.Kill) {
		t := p.GameState().IngameTick()
		if !inRound(t) || e.Killer == nil || e.Victim == nil {
			return
		}
		wn := "?"
		if e.Weapon != nil {
			wn = e.Weapon.String()
		}
		hs := ""
		if e.IsHeadshot {
			hs = " 爆头"
		}
		events_ = append(events_, ev{t, fmt.Sprintf("击杀: %s(uid%d) -[%s%s]-> %s(uid%d)",
			e.Killer.Name, e.Killer.UserID, wn, hs, e.Victim.Name, e.Victim.UserID)})
	})
	p.RegisterEventHandler(func(e events.PlayerHurt) {
		t := p.GameState().IngameTick()
		if !inRound(t) || e.Attacker == nil || e.Player == nil {
			return
		}
		wn := "?"
		if e.Weapon != nil {
			wn = e.Weapon.String()
		}
		events_ = append(events_, ev{t, fmt.Sprintf("伤害: %s(uid%d) -> %s(uid%d) %s -%dHP hitgroup=%d",
			e.Attacker.Name, e.Attacker.UserID, e.Player.Name, e.Player.UserID, wn, e.HealthDamageTaken, e.HitGroup)})
	})
	p.RegisterEventHandler(func(e events.WeaponFire) {
		t := p.GameState().IngameTick()
		if inRound(t) && e.Shooter != nil {
			wn := "?"
			if e.Weapon != nil {
				wn = e.Weapon.String()
			}
			events_ = append(events_, ev{t, fmt.Sprintf("开火: %s(uid%d) %s", e.Shooter.Name, e.Shooter.UserID, wn)})
		}
	})
	p.RegisterEventHandler(func(e events.GrenadeProjectileThrow) {
		t := p.GameState().IngameTick()
		if inRound(t) && e.Projectile != nil && e.Projectile.Owner != nil && e.Projectile.WeaponInstance != nil {
			events_ = append(events_, ev{t, fmt.Sprintf("投掷: %s(uid%d) %v", e.Projectile.Owner.Name, e.Projectile.Owner.UserID, e.Projectile.WeaponInstance.Type)})
		}
	})
	p.RegisterEventHandler(func(e events.PlayerFlashed) {
		t := p.GameState().IngameTick()
		if inRound(t) && e.Player != nil {
			events_ = append(events_, ev{t, fmt.Sprintf("致盲: %s(uid%d) %.1fs", e.Player.Name, e.Player.UserID, e.FlashDuration().Seconds())})
		}
	})
	p.RegisterEventHandler(func(e events.BombPlanted) {
		t := p.GameState().IngameTick()
		if inRound(t) {
			events_ = append(events_, ev{t, fmt.Sprintf("下包: site=%v", e.Site)})
		}
	})
	p.RegisterEventHandler(func(e events.BombDefused) {
		t := p.GameState().IngameTick()
		if inRound(t) {
			events_ = append(events_, ev{t, "拆弹"})
		}
	})
	// 玩家连接/断开（死锁信号 slot 3 命令流断裂可能与重连/替补有关）
	p.RegisterEventHandler(func(e events.PlayerDisconnected) {
		t := p.GameState().IngameTick()
		events_ = append(events_, ev{t, fmt.Sprintf("** 玩家断开: uid%d", e.Player.UserID)})
	})
	p.RegisterEventHandler(func(e events.PlayerConnect) {
		t := p.GameState().IngameTick()
		events_ = append(events_, ev{t, fmt.Sprintf("** 玩家连接: uid%d name=%s", e.Player.UserID, e.Player.Name)})
	})
	p.RegisterEventHandler(func(e events.DataTablesParsed) {
		for _, pl := range p.GameState().Participants().All() {
			if pl != nil && !pl.IsUnknown {
				slotName[int(pl.UserID)] = pl.Name
			}
		}
	})

	if err := p.ParseToEnd(); err != nil {
		panic(err)
	}

	sort.Slice(events_, func(i, j int) bool { return events_[i].tick < events_[j].tick })
	fmt.Printf("== 第 %d 回合事件时间线（tick %d-%d，tickrate %.1f）==\n", round, startTick, endTick, p.TickRate())
	for _, e := range events_ {
		fmt.Printf("%8d (%6.1fs)  %s\n", e.tick, float64(e.tick)/p.TickRate(), e.msg)
	}
	fmt.Println("\n== 玩家 slot 对照 ==")
	slots := make([]int, 0, len(slotName))
	for s := range slotName {
		slots = append(slots, s)
	}
	sort.Ints(slots)
	for _, s := range slots {
		fmt.Printf("  uid %d: %s\n", s, slotName[s])
	}
}

func teamName(t common.Team) string {
	switch t {
	case common.TeamTerrorists:
		return "T"
	case common.TeamCounterTerrorists:
		return "CT"
	case common.TeamSpectators:
		return "观战"
	}
	return fmt.Sprintf("team%d", t)
}
