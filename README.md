# cs2hi —— CS2 Demo 分析与精彩集锦生成工具

解析 CS2 demo 文件，**一键生成指定玩家的精彩集锦视频**，并输出**玩家数据分析**。

## 功能

- **一键渲染成片**：`cs2hi render` 自动完成「启动游戏（HLAE 注入）→ 载入 demo → 按片段边界自动录屏 → 自动退出 → 编码合成」全流程
- **压缩包直接可用**：GUI/CLI 选择 demo 时支持 `.zip` / `.rar` / `.7z` / `.dem.gz` / `.tar.gz`，自动解压到压缩包旁的 `<名字>_demo/` 目录（已解压则复用）
- **玩家统计**：K/D/A、ADR、爆头率、KAST、首杀、补枪/被补、多杀回合（2k~5k）、残局胜/负（1vN）、道具使用（闪光/HE/烟/火）、逐武器明细、逐回合数据
- **集锦检测**：基于事件打分模型检测高光时刻（多杀、残局、ACE、盲狙、穿烟、ECO 击杀等），自动展开为带 tick 范围的剪辑片段
- **已渲染视频管理**：应用内预览支持左右按钮/方向键切换上一条/下一条（无需退出预览），批量删除支持全选/取消全选
- **绿色分发**：单文件 exe + 便携工具链，首次渲染自动下载 HLAE/FFmpeg（约 120MB 仅一次），`cs2hi package` 一键打包发给别人

## 环境要求

- Windows 10/11，已安装 Steam 且登录，已安装 CS2（Counter-Strike 2，国服/国际服均可）
- 渲染时游戏窗口会自动打开（CS2 demo 无法离线渲染，必须启动游戏引擎录制）

## 构建

```bash
# 需要 Go 1.21+（Windows）
# -H=windowsgui：GUI 子系统构建。双击运行不弹黑窗（关闭前端也不会牵连控制台），
# 从 cmd/PowerShell 运行 CLI 子命令时输出自动挂回当前控制台（见 cmd/cs2hi/console_windows.go）
go build -ldflags "-H=windowsgui" -o cs2hi.exe ./cmd/cs2hi
```

**应用图标与 exe 资源**（app.ico / manifest / 版本信息，嵌入后任务栏与资源管理器显示图标）：

```bash
# 1. 重新生成图标源文件（cmd/cs2hi/app.ico + 网页 favicon）：
go run ./tools/genicon
# 2. 重新生成资源对象文件（cmd/cs2hi/rsrc_windows_amd64.syso，go build 自动链接）：
cd cmd/cs2hi && go-winres simply --icon app.ico --manifest gui --arch amd64 \
  --no-suffix --out rsrc --product-version 1.0.0 --file-version 1.0.0 \
  --file-description "cs2hi - CS2 集锦工坊" --product-name cs2hi --original-filename cs2hi.exe \
  && mv rsrc rsrc_windows_amd64.syso
# go-winres 安装：go install github.com/tc-hib/go-winres@latest
```

## 用法

```bash
# 列出 demo 中的所有玩家
cs2hi players match.dem

# 全场 + 指定玩家统计（输出 player_stats.json / report.md）
cs2hi stats match.dem --player "某玩家"

# 只检测集锦片段（输出 highlight_clips.json）
cs2hi clips match.dem --player "某玩家" --max 12 --pre 8 --post 5 --min-score 40

# 统计 + 集锦 + 渲染脚本
cs2hi all match.dem --player "某玩家"

# ★ 一键全自动渲染成片（推荐）
cs2hi render match.dem --player "某玩家"

# 检查/自动下载渲染工具链（HLAE + FFmpeg）
cs2hi setup

# 打包成可分发的 zip（--light 精简包不含 FFmpeg，首次渲染自动下载）
cs2hi package
```

`--player` 支持玩家名（不区分大小写，支持部分匹配）或 SteamID64。

demo 参数支持 `.dem` 文件，也支持压缩包（`.zip` / `.rar` / `.7z` / `.dem.gz` / `.tar.gz`），工具会自动解压到压缩包旁的 `<名字>_demo/` 目录后分析。zip/gz/tar 用 Go 标准库解压，rar/7z 调用 Windows 自带的 `tar.exe`（Win10 1803+ 内置）。

> **提示**：中文玩家名在 PowerShell / CMD 下可正常使用；Git Bash 传中文参数会丢失编码，此时建议用 `--player <SteamID64>` 代替。

输出目录默认为 `<工具目录>\output\<demo名>_out\`：

| 文件 | 内容 |
|---|---|
| `player_stats.json` | 全场与每名玩家的完整统计 |
| `highlight_clips.json` | 集锦片段清单（tick/时间范围、得分、标签） |
| `report.md` | Markdown 分析报告（计分板 + 玩家明细 + 片段表） |
| `<玩家名>_highlights.mp4` | 渲染成片（render 后） |
| `raw/` | 分片段视频（render 后） |
| `cs2hi_auto.cfg` / `cs2hi_record.cfg` | 渲染配置副本 |

## 集锦打分规则

| 事件 | 得分 |
|---|---|
| 普通击杀 | 25 |
| 爆头 | +15 |
| 盲狙 | +25 |
| 穿烟击杀 | +15 |
| 穿墙 | +5 |
| 被闪白击杀 | +15 |
| 闪光助攻 | +10 |
| 远距离（>1000 单位，步枪/重武器） | +10 |
| 连杀（8 秒窗口内第 2~5 杀） | +25/杀 |
| 首杀 | +25 |
| 补枪（4 秒内为队友复仇） | +15 |
| ECO 击杀（≤$1000 打 ≥$3000） | +35 |
| 刀杀 / 电击枪 | +60 / +45 |
| 道具击杀 | +15 |
| 残局胜（1vN） | 60 + 50×N |
| 四杀 / 五杀回合 | 200 / 240 |
| 下包 / 拆弹 | 40 / 60 |

片段取时刻前 8 秒、后 5 秒（可调），重叠片段自动合并，按总分排序取 Top N（默认 12，最低 40 分）。

## 统计口径

- **ADR**：对敌方造成的生命值伤害 ÷ 参与回合数（近似 HLTV 口径）
- **KAST**：击杀/助攻/存活/被补 的回合占比
- **残局**：回合结束时己方唯一存活者（胜=ClutchWin，负=ClutchLoss）
- **多杀**：单回合内击杀数（2k~5k）
- **ECO 击杀**：击杀者冻结结束时装备价值 ≤$1000 且受害者 ≥$3000
- 热身阶段事件不计入统计

## 渲染流水线实现要点

- 启动：`HLAE.exe -customLoader -noGui -autoStart` 注入 `AfxHookSource2.dll` 并拉起 cs2.exe
- 国服安装默认 `-perfectworld` 跳过服务器选择框（`--server worldwide|pw|none` 可切换）
- `mirv_cmd addAtTick` 按 demo tick 调度：demo 播放 0.08 秒后自动 seek 到第一段前 2 秒，之后链式跳转逐段录制（时间顺序）
- 屏幕录制输出 TGA 帧序列 + 同目录 `audio.wav`（HLAE 内置 ffmpeg 管道在部分机器不可用），游戏退出后统一编码为 mp4（含音轨），编码完成即删除原始帧
- **音频**：HLAE 把游戏混音写进 take 目录的 `audio.wav`，cs2hi 编码时混入成片（aac 192k/44.1k 立体声）。**不能**用 `volume 0` 静音——引擎录的是最终混音，音量 0 会让 wav 也变成静音
- 渲染期间通过 Windows 音频会话 API **只静音 cs2.exe 的扬声器输出**（不影响引擎内录的 wav）。静音/解除都遍历**所有活跃渲染端点**——cs2.exe 在每个播放设备（耳机 / 显示器 HDMI / SPDIF…）上各有一个会话，而 Windows 的每应用静音状态按端点持久化，只看默认端点会留下「显示器有声音、耳机没声音」的残留。Windows 按应用持久化静音状态（实测同一 exe 再次启动时音频会话初始就是静音），必须在音频会话销毁前解除：cfg 在每轮最后一段录完后先 `demo_pause; wait 90`（约 1.5 秒）再 `quit`，外部以「第 N 次 Finishing recording」为主信号在游戏仍存活时解除；`Host activate: Quitting` 只作兜底（该日志行与进程退出常落在同一秒，单独依赖会漏）。取消/冻结/超时等强杀路径也会先解除；解除失败会在日志里提示手动取消静音
- 编码进度按「录制占该轮一半、编码占另一半」计算，进度条单调不回退（旧版编码阶段会从 100% 跳回 0%）
- **GUI 运行日志**：`-H=windowsgui` 构建没有控制台，渲染管线输出与 panic/fatal 堆栈默认全部丢失（曾出现音频会话 `GetMute` 越界写踩坏栈、进程静默退出的问题：界面凭空消失，游戏因失去隐藏进程而留在前台）。现在 GUI 启动时把 stdout/stderr 重定向到 exe 同目录的 `cs2hi_gui.log`，闪退可直接查现场；渲染 goroutine 内的普通 panic 会 recover 成任务错误显示在界面上
- **实时日志**：渲染弹窗的日志区展示管线实时输出（轮次启动、开始录制哪个回合、编码耗时、合成、警告、游戏控制台尾部），不再只有固定的三条启动信息
- **后台隐藏**：渲染期间把游戏窗口设为分层窗口 alpha=0（完全透明）+ 点击穿透 + 不进任务栏/Alt-Tab，窗口位置保持不动（移动窗口会引发闪烁与光标 warp）。早期用 alpha=1 担心"0 会让引擎跳过渲染"，实测 720p60 录制中切到 alpha=0，进程 CPU 时间与 TGA 帧数增长完全正常，深色背景上的画面残影彻底消失
- **进度口径**：每个片段按"录制完成 1 份 + 编码完成 1 份"计算；编码开始时只计已完成的片段，最后一段编码期间进度条不会再提前到 100%
- 进度界面结构化展示：当前轮次（第 N 轮）、全部片段中的全局序号（第 x/y 个）、当前片段帧进度百分比；重录/重试轮次不会让进度条回退
- 视角控制：`--view pov`（第一人称，默认）/ `--view third`（第三人称跟随）/ `--view default`（GOTV 导播视角），通过 `spec_player`+`spec_mode` 锁定目标玩家（锁定跨片段持续保持）。锁定参数优先用 demo 解析出的**玩家实体索引**（纯数字，与 HLAE 官方 `mirv_script_spec_lock` 相同做法）：CS2 demo 的玩家名普遍带前导空格（如 `" Hinson1985"`），按名字匹配既无法安全嵌入 cfg 又会因空格被跳过，导致镜头停在 GOTV 默认目标；每个片段在 seek 后 2 秒和 4 秒各锁定一次，防止 TrueSpectate 重建抢先
- **必须关闭 TrueView 预测回放**：CS2 2025-11 起 demo 默认启用 TrueView（`cl_demo_predict`），靠 demo 内的客户端命令流重建第一人称视角；管线用 `demo_gototick` 跳转后命令数据丢失（控制台刷 `TrueView can't determine last executed command`，HUD 显示「实况回放: 无数据」），镜头会永久卡在跳转点——时间/击杀/HUD 正常推进，但人物不再移动转向。渲染配置在 `playdemo` 前及每次视角锁定命令里执行 `cl_demo_predict 0`、`cl_spectator_predict 0`，并用 `cl_trueview_show_status 0` 隐藏状态文字；镜头改由服务端快照驱动，正常跟随目标玩家
- 多杀回合（三杀/四杀/五杀）片段从本回合第一个击杀完整覆盖到最后一个击杀，后摇不因回合结束截断
- **断点续录**：CS2 的 TrueSpectate 预测扫描死锁（单轮连续 seek 次数累积触发），管线计划分轮（`--max-per-pass 6`，每轮最多 6 个片段）+ 冻结检测（CPU 时间监控 + HLAE 看门狗）+ 自动重启续录，直到全部完成；每轮游戏控制台日志存档到输出目录（`console_pass_*.log`）便于诊断
- **取消渲染**：GUI 取消会置取消标记并终止当前游戏进程，已录制片段照常编码保留，但不再自动开始下一轮（早期版本只杀进程，续录循环会把它当失败轮次自动重试）
- 录制完成自动 `quit` 退出游戏（quit 前暂停 demo 并等待约 1.5 秒，给外部解除游戏静音留窗口）；`host_framerate` 限速实时渲染（超速渲染会让观战同步跟不上）；`engine_no_focus_sleep 0` 保证窗口无焦点时全速渲染
- 渲染用 `-w -h` 临时设置分辨率，游戏退出会把该分辨率写回 `userdata\<账号>\730\local\cfg\cs2_video.txt`。渲染开始前备份全部账号的视频设置，结束统一还原（中途崩溃则下次渲染启动时自愈），不会篡改玩家真实游戏的默认分辨率
- 不能用 `-nosound`：音频系统未初始化时 HLAE 音频录制会在 soundsystem.dll 崩溃。当前管线不改游戏音量（录制需要真实音量），音量备份/还原逻辑保留作兜底：旧版本遗留的音量改动会自动还原，不会留下"录完集锦后游戏没声音"
- 启动前自动预估峰值磁盘占用并校验空间

## 输出组织

```
output\<demo名>_out\
├── clips\
│   ├── 残局\第N回合_1v2残局.mp4     ← 残局 1vN 单独归类
│   ├── 多杀\第N回合_三杀.mp4        ← 三杀/四杀/五杀
│   └── 常规\第N回合_爆头.mp4        ← 单杀精彩镜头
├── <玩家名>_highlights.mp4          ← 合集成片
├── video_index.md                   ← 两部分对照：视频→精彩操作 / 精彩操作→视频
├── report.md                        ← 分析报告（含逐回合集锦分析表）
└── highlight_clips.json             ← 片段清单（tick/时间范围/得分/标签/分类）
```

指令参考：[HLAE 文档](https://doc.hlae.site/)、[mirv_streams Wiki](https://github.com/advancedfx/advancedfx/wiki/Source2:mirv_streams)、[CS2 官方启动参数](https://developer.valvesoftware.com/wiki/Command_line_options_(Source_2))。

## 技术架构

- 解析：demoinfocs-golang v4（原生支持 CS2，解析一个 demo 仅需数秒）
- 管线：`internal/demo`（事件日志）→ `internal/stats`（统计聚合）/ `internal/highlight`（高光检测）→ `internal/output`（JSON/MD/渲染脚本）
- 事件日志与统计、检测解耦，便于扩展新的分析维度（热图、道具轨迹等）

## 已知限制

- 残局判定基于回合结束时的存活快照，回合中途加入的玩家（如替补）统计以实际参与回合为准
- 装备价值依赖 demoinfocs 的追踪，个别 CS2 版本下 ECO 判定可能不触发
- HLAE 录制需要游戏客户端与 HLAE 环境，工具本身不执行游戏渲染
