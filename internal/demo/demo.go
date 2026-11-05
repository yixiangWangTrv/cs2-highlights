// Package demo 负责解析 CS2 demo 文件，并输出结构化的原始事件日志。
package demo

import (
	"fmt"
	"os"
	"regexp"
	"sort"

	"github.com/golang/geo/r3"
	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	events "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// 团队编号常量，供统计与检测模块使用。
const (
	TeamT  = uint8(common.TeamTerrorists)
	TeamCT = uint8(common.TeamCounterTerrorists)
)

// PlayerInfo 是 demo 中一名玩家的基本信息（解析结束时快照）。
type PlayerInfo struct {
	Name          string
	SteamID64     uint64
	AccountID     uint32 // SteamID32（账号 ID），CS2 spec_lock_to_accountid 锁定观战用
	EntityID      int    // 玩家 controller 实体索引（spec_player 按索引锁定观战用，全 demo 稳定）
	Team          string
	TeamID        uint8
	IsBot         bool
	Score         int
	MVPs          int
	UtilityDamage int
}

// KillRecord 记录一次击杀。
type KillRecord struct {
	Tick           int
	Round          int
	TimeSec        float64
	Weapon         string
	WeaponClass    string
	Killer         uint64
	KillerName     string
	KillerTeam     uint8
	Victim         uint64
	VictimName     string
	VictimTeam     uint8
	Assister       uint64
	IsHeadshot     bool
	NoScope        bool
	ThroughSmoke   bool
	WallBang       bool
	AttackerBlind  bool
	AssistedFlash  bool
	Distance       float64
	KillerEquipVal int // 击杀者本回合冻结结束时的装备价值（用于 ECO 击杀判定）
	VictimEquipVal int
	KillerPos      [3]float64
	VictimPos      [3]float64
}

// HurtRecord 记录一次伤害。
type HurtRecord struct {
	Tick              int
	Round             int
	Attacker          uint64
	Victim            uint64
	AttackerTeam      uint8
	VictimTeam        uint8
	HealthDamage      int // 原始伤害（可能超出受害者剩余血量）
	HealthDamageTaken int // 实际扣血（按剩余血量封顶，0 表示事件未提供）
	ArmorDamage       int
	HitGroup          uint8
	Weapon            string
}

// FireRecord 记录一次开火。
type FireRecord struct {
	Tick    int
	Round   int
	Shooter uint64
	Weapon  string
}

// FlashRecord 记录一次闪光致盲。
type FlashRecord struct {
	Tick         int
	Round        int
	Attacker     uint64
	Victim       uint64
	AttackerTeam uint8
	VictimTeam   uint8
	DurationSec  float64
}

// ThrowRecord 记录一次投掷物出手。
type ThrowRecord struct {
	Tick        int
	Round       int
	Thrower     uint64
	GrenadeType int // common.EquipmentType 的 int 表示
}

// BombRecord 记录一次装包/拆包。
type BombRecord struct {
	Tick   int
	Round  int
	Player uint64
	Site   string
	Kind   string // "plant" | "defuse"
}

// RoundRecord 记录一回合的起止与结果。
type RoundRecord struct {
	Number        int
	StartTick     int
	FreezeEndTick int
	EndTick       int
	Winner        uint8
	WinnerName    string
	EndReason     string
	AliveT        []uint64 // 回合结束时 T 方存活玩家
	AliveCT       []uint64 // 回合结束时 CT 方存活玩家
}

// Parsed 是一个 demo 解析后的完整事件日志。
type Parsed struct {
	Path     string
	MapName  string
	TickRate float64
	Players  []PlayerInfo
	Rounds   []RoundRecord
	Kills    []KillRecord
	Hurts    []HurtRecord
	Fires    []FireRecord
	Flashes  []FlashRecord
	Throws   []ThrowRecord
	Bombs    []BombRecord
}

// TeamName 返回团队的中文名。
func TeamName(t uint8) string {
	switch common.Team(t) {
	case common.TeamTerrorists:
		return "T"
	case common.TeamCounterTerrorists:
		return "CT"
	case common.TeamSpectators:
		return "观战"
	}
	return "未分配"
}

// Parse 解析 demo 文件并返回事件日志。
func Parse(path string) (*Parsed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 demo 失败: %w", err)
	}
	defer f.Close()

	p := dem.NewParser(f)
	defer p.Close()

	out := &Parsed{Path: path}
	c := &collector{p: p, out: out}

	p.RegisterEventHandler(c.onRoundStart)
	p.RegisterEventHandler(c.onRoundFreezetimeEnd)
	p.RegisterEventHandler(c.onRoundEnd)
	p.RegisterEventHandler(c.onKill)
	p.RegisterEventHandler(c.onPlayerHurt)
	p.RegisterEventHandler(c.onWeaponFire)
	p.RegisterEventHandler(c.onPlayerFlashed)
	p.RegisterEventHandler(c.onGrenadeThrow)
	p.RegisterEventHandler(c.onBombPlanted)
	p.RegisterEventHandler(c.onBombDefused)

	if err := p.ParseToEnd(); err != nil {
		return nil, fmt.Errorf("解析 demo 失败: %w", err)
	}

	out.TickRate = p.TickRate()
	// v5 不再公开 demo 头信息，地图名直接从文件头字节中提取
	out.MapName = extractMapName(path)

	for _, pl := range p.GameState().Participants().All() {
		if pl == nil || pl.IsUnknown {
			continue
		}
		// 实体索引优先用解析过程中记录的首个有效值（最后快照可能已失效）
		eid := pl.EntityID
		if v, ok := c.eids[pl.SteamID64]; ok {
			eid = v
		}
		out.Players = append(out.Players, PlayerInfo{
			Name:          pl.Name,
			SteamID64:     pl.SteamID64,
			AccountID:     pl.SteamID32(),
			EntityID:      eid,
			Team:          TeamName(uint8(pl.Team)),
			TeamID:        uint8(pl.Team),
			IsBot:         pl.IsBot,
			Score:         pl.Score(),
			MVPs:          pl.MVPs(),
			UtilityDamage: pl.UtilityDamage(),
		})
	}
	sort.Slice(out.Players, func(i, j int) bool {
		if out.Players[i].TeamID != out.Players[j].TeamID {
			return out.Players[i].TeamID < out.Players[j].TeamID
		}
		return out.Players[i].Name < out.Players[j].Name
	})

	return out, nil
}

// collector 在解析过程中收集事件。
type collector struct {
	p     dem.Parser
	out   *Parsed
	round int            // 当前回合号（1 起）
	eids  map[uint64]int // 首个有效玩家实体索引（最后快照可能已失效）
}

// active 判断当前是否处于计分阶段。
func (c *collector) active() bool {
	gs := c.p.GameState()
	return c.round > 0 && !gs.IsWarmupPeriod()
}

func (c *collector) onRoundStart(events.RoundStart) {
	gs := c.p.GameState()
	// 记录玩家实体索引（spec_player 按索引锁定观战）。取首个有效值：玩家死亡或
	// 断开后最后快照的 EntityID 可能失效，而实体索引在整场 demo 中稳定不变。
	if c.eids == nil {
		c.eids = map[uint64]int{}
	}
	for _, pl := range gs.Participants().All() {
		if pl == nil || pl.IsUnknown || pl.EntityID <= 0 {
			continue
		}
		if _, ok := c.eids[pl.SteamID64]; !ok {
			c.eids[pl.SteamID64] = pl.EntityID
		}
	}
	// 热身阶段不计数；TotalRoundsPlayed 兜底判断（部分 demo 可能不发 MatchStart）
	if !gs.IsMatchStarted() && gs.TotalRoundsPlayed() == 0 {
		return
	}
	c.round++
	c.out.Rounds = append(c.out.Rounds, RoundRecord{Number: c.round, StartTick: gs.IngameTick()})
}

func (c *collector) onRoundFreezetimeEnd(events.RoundFreezetimeEnd) {
	if c.round > 0 && len(c.out.Rounds) > 0 {
		c.out.Rounds[len(c.out.Rounds)-1].FreezeEndTick = c.p.GameState().IngameTick()
	}
}

func (c *collector) onRoundEnd(e events.RoundEnd) {
	gs := c.p.GameState()
	if c.round <= 0 || len(c.out.Rounds) == 0 {
		return
	}
	r := &c.out.Rounds[len(c.out.Rounds)-1]
	r.EndTick = gs.IngameTick()
	r.Winner = uint8(e.Winner)
	r.WinnerName = TeamName(uint8(e.Winner))
	r.EndReason = roundEndReasonName(e.Reason)
	for _, pl := range gs.Participants().All() {
		if pl == nil || !pl.IsAlive() {
			continue
		}
		switch pl.Team {
		case common.TeamTerrorists:
			r.AliveT = append(r.AliveT, pl.SteamID64)
		case common.TeamCounterTerrorists:
			r.AliveCT = append(r.AliveCT, pl.SteamID64)
		}
	}
}

func (c *collector) onKill(e events.Kill) {
	if !c.active() {
		return
	}
	if e.Killer == nil || e.Victim == nil {
		return // 世界伤害等无法归属的击杀
	}
	k := KillRecord{
		Tick:           c.p.GameState().IngameTick(),
		Round:          c.round,
		Killer:         e.Killer.SteamID64,
		KillerName:     e.Killer.Name,
		KillerTeam:     uint8(e.Killer.Team),
		Victim:         e.Victim.SteamID64,
		VictimName:     e.Victim.Name,
		VictimTeam:     uint8(e.Victim.Team),
		IsHeadshot:     e.IsHeadshot,
		NoScope:        e.NoScope,
		ThroughSmoke:   e.ThroughSmoke,
		WallBang:       e.IsWallBang(),
		AttackerBlind:  e.AttackerBlind,
		AssistedFlash:  e.AssistedFlash,
		Distance:       float64(e.Distance),
		KillerEquipVal: e.Killer.EquipmentValueFreezeTimeEnd(),
		VictimEquipVal: e.Victim.EquipmentValueFreezeTimeEnd(),
		KillerPos:      vec3(e.Killer.Position()),
		VictimPos:      vec3(e.Victim.Position()),
	}
	if e.Assister != nil {
		k.Assister = e.Assister.SteamID64
	}
	if e.Weapon != nil {
		k.Weapon = e.Weapon.String()
		k.WeaponClass = weaponClass(e.Weapon.Class())
	}
	c.out.Kills = append(c.out.Kills, k)
}

func (c *collector) onPlayerHurt(e events.PlayerHurt) {
	if !c.active() {
		return
	}
	if e.Attacker == nil || e.Player == nil {
		return
	}
	h := HurtRecord{
		Tick:              c.p.GameState().IngameTick(),
		Round:             c.round,
		Attacker:          e.Attacker.SteamID64,
		AttackerTeam:      uint8(e.Attacker.Team),
		Victim:            e.Player.SteamID64,
		VictimTeam:        uint8(e.Player.Team),
		HealthDamage:      e.HealthDamage,
		HealthDamageTaken: e.HealthDamageTaken,
		ArmorDamage:       e.ArmorDamage,
		HitGroup:          uint8(e.HitGroup),
	}
	if e.Weapon != nil {
		h.Weapon = e.Weapon.String()
	}
	c.out.Hurts = append(c.out.Hurts, h)
}

func (c *collector) onWeaponFire(e events.WeaponFire) {
	if !c.active() || e.Shooter == nil {
		return
	}
	rec := FireRecord{Tick: c.p.GameState().IngameTick(), Round: c.round, Shooter: e.Shooter.SteamID64}
	if e.Weapon != nil {
		rec.Weapon = e.Weapon.String()
	}
	c.out.Fires = append(c.out.Fires, rec)
}

func (c *collector) onPlayerFlashed(e events.PlayerFlashed) {
	if !c.active() || e.Player == nil {
		return
	}
	rec := FlashRecord{
		Tick:        c.p.GameState().IngameTick(),
		Round:       c.round,
		Victim:      e.Player.SteamID64,
		VictimTeam:  uint8(e.Player.Team),
		DurationSec: e.FlashDuration().Seconds(),
	}
	if e.Attacker != nil {
		rec.Attacker = e.Attacker.SteamID64
		rec.AttackerTeam = uint8(e.Attacker.Team)
	}
	c.out.Flashes = append(c.out.Flashes, rec)
}

func (c *collector) onGrenadeThrow(e events.GrenadeProjectileThrow) {
	if !c.active() {
		return
	}
	proj := e.Projectile
	if proj == nil || proj.Owner == nil || proj.WeaponInstance == nil {
		return
	}
	c.out.Throws = append(c.out.Throws, ThrowRecord{
		Tick:        c.p.GameState().IngameTick(),
		Round:       c.round,
		Thrower:     proj.Owner.SteamID64,
		GrenadeType: int(proj.WeaponInstance.Type),
	})
}

func (c *collector) onBombPlanted(e events.BombPlanted) {
	if !c.active() || e.Player == nil {
		return
	}
	c.out.Bombs = append(c.out.Bombs, BombRecord{
		Tick:   c.p.GameState().IngameTick(),
		Round:  c.round,
		Player: e.Player.SteamID64,
		Site:   string(e.Site),
		Kind:   "plant",
	})
}

func (c *collector) onBombDefused(e events.BombDefused) {
	if !c.active() || e.Player == nil {
		return
	}
	c.out.Bombs = append(c.out.Bombs, BombRecord{
		Tick:   c.p.GameState().IngameTick(),
		Round:  c.round,
		Player: e.Player.SteamID64,
		Site:   string(e.Site),
		Kind:   "defuse",
	})
}

func vec3(v r3.Vector) [3]float64 {
	return [3]float64{v.X, v.Y, v.Z}
}

func weaponClass(c common.EquipmentClass) string {
	switch c {
	case common.EqClassPistols:
		return "pistol"
	case common.EqClassSMG:
		return "smg"
	case common.EqClassHeavy:
		return "heavy"
	case common.EqClassRifle:
		return "rifle"
	case common.EqClassEquipment:
		return "equipment"
	case common.EqClassGrenade:
		return "grenade"
	}
	return "unknown"
}

var mapNameRe = regexp.MustCompile(`\b(?:de|cs|ar|csgo)_[a-z0-9_]+\b`)

// extractMapName 从 demo 文件头中提取地图名（CS2 头里是明文）。
func extractMapName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 2048)
	n, _ := f.Read(buf)
	if m := mapNameRe.Find(buf[:n]); m != nil {
		name := string(m)
		// 去掉被误吞的 protobuf 长度字节：真实地图名最多一个末尾数字（如 de_dust2）
		for len(name) > 2 && name[len(name)-1] >= '0' && name[len(name)-1] <= '9' &&
			name[len(name)-2] >= '0' && name[len(name)-2] <= '9' {
			name = name[:len(name)-1]
		}
		return name
	}
	return ""
}

func roundEndReasonName(r events.RoundEndReason) string {
	switch r {
	case events.RoundEndReasonTargetBombed:
		return "C4 爆炸"
	case events.RoundEndReasonBombDefused:
		return "拆弹成功"
	case events.RoundEndReasonCTWin:
		return "CT 获胜"
	case events.RoundEndReasonTerroristsWin:
		return "T 获胜"
	case events.RoundEndReasonDraw:
		return "平局"
	case events.RoundEndReasonHostagesRescued:
		return "人质获救"
	case events.RoundEndReasonTargetSaved:
		return "目标被保护"
	case events.RoundEndReasonTerroristsSurrender:
		return "T 投降"
	case events.RoundEndReasonCTSurrender:
		return "CT 投降"
	}
	return fmt.Sprintf("reason-%d", r)
}
