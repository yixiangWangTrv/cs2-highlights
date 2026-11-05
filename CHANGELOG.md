# 更新记录

## 2026-11-01

### 修复日志

- **渲染完成后正常进游戏没有声音**：Windows 会按应用（exe 路径）持久化音频会话的静音状态——本机实测同一 exe 被静音后退出，再次启动时会话初始 `GetMute` 就是 `true`。渲染期间为避免 demo 声音外放会静音 cs2.exe 的音频会话，但解除时机只依赖控制台日志 `Host activate: Quitting`：实测该行与 `MainLoop returning` / `Source2Shutdown` 落在同一秒，从出现到音频会话销毁只有数十毫秒，100ms 轮询经常来不及，静音状态就被系统记住了。
  现在 cfg 在每轮最后一段录制结束后先 `demo_pause; wait 90`（host_framerate 60 → 约 1.5 秒）再 `quit`；`watchGameQuit` 以「日志里出现第 N 次 `Finishing recording`」为主信号，在游戏仍存活时解除静音，`Host activate: Quitting` 保留作兜底。实测解除静音后再次启动同一 exe，会话初始 `GetMute` 为 `false`，持久化状态被清除。
- `applySessionMute` 读取静音状态失败（会话正在销毁）时不再跳过写入，仍尝试 `SetMute`，争取最后一次把 `false` 写回持久化状态。
- 解除静音失败（音频会话已随游戏销毁）时在渲染日志中提示用户到 Windows 音量合成器手动取消 cs2.exe 静音，避免下次进游戏无声却无处查。
- **显示器外放有声音、耳机没声音**：Windows 的每应用静音状态是**按音频端点**持久化的，而旧实现静音/解除只操作「默认渲染端点」。cs2.exe 会同时在耳机、显示器 HDMI、SPDIF 等每个设备上建立会话，渲染时只静音了当时默认的那一个，解除也只改那一个；玩家之后换到另一个设备，就会撞上留存在该端点上的静音。现场实测（游戏运行中）：耳机端点 `cs2.exe mute=true`，显示器端点 `mute=false`。`setGameAudioMute` 现在遍历所有活跃渲染端点，静音与解除都覆盖全部设备。

### 验证

- 新增回归测试：`TestGenerateCfgsWaitsBeforeQuit`（quit 前必须有 demo_pause + wait，且只出现在最后一段）、`TestGenerateCfgsNoQuitSkipsWait`、`TestGenerateCfgsReloadLastPassWaits`（重载链最后一轮同样有等待窗口）、`TestWatchGameQuitOnRecordingDone`（录完 1/2 段不解除、2/2 段解除）、`TestWatchGameQuitOnQuitting`（Quitting 兜底）。
- `go test ./...` 全部通过；已重新构建 `cs2hi.exe`（`-H=windowsgui`）。
- 本机音频实验：同一路径 exe 静音退出 → 再次启动初始 `mute=true`；退出前解除 → 再次启动初始 `mute=false`（证实持久化机制与修复思路）。
- 现场核对（游戏运行中）：修复前耳机端点 `cs2.exe mute=true`、显示器/SPDIF 端点 `mute=false`；对所有端点解除后复查全部 `mute=false`。`CS2HI_AUDIO_MUTE_TEST=1 go test -run TestAudioSessionMute` 在新实现下通过（真实音频会话静音/解除）。

## 2026-10-31

### 修复日志

- **成片里人物/视角卡在原地**：CS2 2025-11 起默认启用 TrueView 预测回放（`cl_demo_predict`），它靠 demo 内的客户端命令流重建第一人称视角。渲染管线大量使用 `demo_gototick` 跳转，跳转后预测系统拿不到命令数据（控制台持续刷 `TrueView can't determine last executed command`，HUD 右上角显示「实况回放: 无数据」），第一人称镜头会永久停在跳转点——时间、击杀、HUD 都正常推进，但玩家不再移动和转向。现在渲染配置在 `playdemo` 前以及每次视角锁定命令里都执行 `cl_demo_predict 0`、`cl_spectator_predict 0` 关闭预测回放，并用 `cl_trueview_show_status 0` 隐藏状态文字；镜头改由服务端快照驱动的经典观战相机，正常跟随目标玩家。
- 新增回归测试 `TestGenerateCfgsDisablesTrueView`：`cs2hi_auto.cfg` / `cs2hi_record.cfg` / demo 重载 cfg 任何一处漏掉关闭命令都会测试失败。

### 功能与界面

- **文件浏览器显示修改时间**：选择 demo 或输出目录时，目录与 demo/压缩包都会展示本地时间 `YYYY-MM-DD HH:mm`（文件同时保留大小），可据此快速判断新旧；文件名过长时省略显示，时间/大小始终靠右可见。
- **切换目录后滚动位置重置**：原来在目录 A 滚到底再进入目录 B，列表会停在底部看不到开头；现在每次打开新目录/盘符列表都回到顶部。

### 验证

- 真实 demo 渲染验证（1280x720@60，1 个 27.8s 四杀片段）：镜头全程跟随目标玩家移动/转向，玩家模型、敌人轮廓、枪口火焰正常；控制台 TrueView 报错从单轮 3265 条降为 0 条。
- 文件浏览 API 验证：目录与文件均返回 `modTime`；`app.js` 中时间格式化与滚动重置逻辑已随 exe 内嵌更新。
- 新增回归测试 `TestHandleBrowseReturnsModTime`；`go test ./...` 全部通过；已重新构建 `cs2hi.exe`（`-H=windowsgui`）。

## 2026-10-30

### 修复日志

- **渲染时界面闪退、游戏留在前台**：修复音频会话 `GetMute` 把 4 字节 BOOL 写入 Go 的 1 字节 bool、踩坏栈上 defer 函数指针导致进程 fatal error 的问题（本次闪退根因）。
- **进度条提前到 100%**：进度改为按「录制完成 1 份 + 编码完成 1 份」计算，最后一段编码期间不再提前满格。
- **编码阶段进度回退**：编码逐个删除 take 目录会被录制进度监控误判为「重新开始录制」，现游戏退出后立即停止该监控。
- **渲染日志只有三条固定文案**：轮次启动、开始录制、编码耗时、控制台尾部、警告等管线日志实时接入弹窗。
- **深色背景能看到游戏画面影子**：隐藏方式由分层窗口 alpha=1 改为 alpha=0，实测不影响引擎渲染与 HLAE 抓帧。
- **渲染视角不在目标玩家身上**：观战锁定改用 demo 解析出的玩家实体索引（纯数字）执行 `spec_player`——demo 玩家名普遍带前导空格/中文，按名字匹配会被跳过，镜头停在 GOTV 默认目标；同时去掉会破坏观战系统的 `-accountid`，每个片段在 seek 后 2 秒、4 秒各锁定一次，防止 TrueSpectate 重建抢占。
- **GUI 静默退出无迹可查**：stdout/stderr 重定向到 exe 同目录的 `cs2hi_gui.log`；渲染 goroutine 增加 panic 兜底，异常转为界面错误。
- 警告事件不再清零进度；同一条警告不再重复写入日志。

### 版本新功能

- **压缩包直接解析**：支持 `.zip` / `.rar` / `.7z` / `.dem.gz` / `.tar.gz` 等，自动解压到压缩包旁的 `<名字>_demo/` 并取其中最大的 `.dem`；已解压且较新时直接复用，zip/gz/tar 用 Go 标准库，rar/7z 调用系统 `tar.exe`，另设 8GB 解压上限防压缩炸弹。
- **渲染实时日志**：进度弹窗日志区带时间戳，完整展示渲染操作过程（保留最近 300 条）。
- **视频预览左右切换**：预览内新增 ‹ › 按钮，支持键盘 ←/→，首尾循环；标题显示「文件名 · 3/10」，无需退出预览再点下一个。
- **批量删除全选**：批量删除操作条新增「全选 / 取消全选」，自动同步已选计数与卡片勾选状态。

### 验证

- 真实 CS2 渲染验证（720p60，多片段 / 45.9s 长片段）：进程全程存活、游戏窗口 alpha=0 完全不可见、录制编码正常、视频设置自动还原。
- 前端通过 WebView2 远程调试自动化验证：预览切换/循环/方向键、批量全选与取消，无 JS 异常。
- 已重新构建 `cs2hi.exe`（`-H=windowsgui`）。
