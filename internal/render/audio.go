package render

import (
	"bytes"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// ---- 渲染期间游戏静音 ----
//
// 录制需要游戏内音量为正常值（volume 0 会把 HLAE 内录的 audio.wav 一起
// 静音），但后台渲染不应该把 demo 声音外放。这里只静音 cs2.exe 的
// Windows 音频会话（音量合成器里的单独应用静音）：引擎把混音写进
// audio.wav 发生在交给系统音频设备之前，因此录音不受影响。
//
// 静音/解除都遍历所有活跃渲染端点（见 setGameAudioMute）：cs2.exe 同时在
// 耳机、显示器 HDMI、SPDIF 等每个设备上建会话，而 Windows 的每应用静音
// 状态是按端点持久化的。只处理默认端点会留下"耳机端点上仍然是静音"的
// 残留，玩家换设备后就听不到声音。
//
// 注意：Windows 会按应用（exe 路径）持久化音频会话的静音状态——实测同一
// exe 被静音后退出，再次启动时会话初始 GetMute 就是 true，表现为「渲染完
// 成后正常进游戏，游戏没声音」。必须在游戏音频会话销毁前解除静音：
//   - 正常退出：watchGameQuit 以录制的最后一段结束（"Finishing recording"
//     计数）为主信号——cfg 在 quit 前插入了 demo_pause + wait 窗口；
//     "Host activate: Quitting" 只作兜底（实测与进程退出同秒，太晚）。
//   - 主动终止（取消/冻结/超时）：先 stopGameAudioMute 再 taskkill。

var (
	audioMuteStopMu sync.Mutex
	audioMuteStop   func()
)

// setGameAudioMute 对属于 pids 的音频会话设置静音/取消静音，返回变更数。
//
// 必须遍历【所有】活跃渲染端点，不能只看默认端点：cs2.exe 会同时在多个
// 端点上建会话（实测耳机 / 显示器 HDMI / SPDIF 各一个），而 Windows 的
// 每应用静音状态是【按端点】持久化的。只处理默认端点时，一旦渲染期间默认
// 设备与玩家之后使用的设备不同，残留静音就清不掉——表现为「显示器外放有
// 声音、插上耳机没声音」，而静音状态还挂在耳机端点上。
func setGameAudioMute(pids map[int]bool, mute bool) int {
	if len(pids) == 0 {
		return 0
	}
	// COM 接口有线程亲和性：锁定当前 OS 线程并在其上初始化（STA）。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		// S_OK / S_FALSE（已初始化）之外视为失败
		if oe, ok := err.(*ole.OleError); !ok || (oe.Code() != 0 && oe.Code() != 1) {
			return 0
		}
	}
	defer ole.CoUninitialize()

	var enum *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &enum); err != nil {
		return 0
	}
	defer enum.Release()

	var coll *wca.IMMDeviceCollection
	if err := enum.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &coll); err != nil {
		return 0
	}
	defer coll.Release()
	var devices uint32
	if err := coll.GetCount(&devices); err != nil {
		return 0
	}

	changed := 0
	for i := uint32(0); i < devices; i++ {
		var dev *wca.IMMDevice
		if err := coll.Item(i, &dev); err != nil {
			continue
		}
		changed += muteSessionsOnDevice(dev, pids, mute)
		dev.Release()
	}
	return changed
}

// muteSessionsOnDevice 处理单个渲染端点上的游戏会话，返回变更数。
func muteSessionsOnDevice(dev *wca.IMMDevice, pids map[int]bool, mute bool) int {
	var mgr *wca.IAudioSessionManager2
	if err := dev.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &mgr); err != nil {
		return 0
	}
	defer mgr.Release()
	var sessions *wca.IAudioSessionEnumerator
	if err := mgr.GetSessionEnumerator(&sessions); err != nil {
		return 0
	}
	defer sessions.Release()
	var count int
	if err := sessions.GetCount(&count); err != nil {
		return 0
	}

	changed := 0
	for i := 0; i < count; i++ {
		var ctrl *wca.IAudioSessionControl
		if err := sessions.GetSession(i, &ctrl); err != nil {
			continue
		}
		if pid, ok := audioSessionPID(ctrl); ok && pids[pid] {
			if applySessionMute(ctrl, mute) {
				changed++
			}
		}
		ctrl.Release()
	}
	return changed
}

// audioSessionPID 取音频会话所属进程 PID。
func audioSessionPID(ctrl *wca.IAudioSessionControl) (int, bool) {
	var ctrl2 *wca.IAudioSessionControl2
	if err := ctrl.PutQueryInterface(wca.IID_IAudioSessionControl2, &ctrl2); err != nil {
		return 0, false
	}
	defer ctrl2.Release()
	var pid uint32
	if err := ctrl2.GetProcessId(&pid); err != nil {
		return 0, false
	}
	return int(pid), true
}

// sessionGetMute 读取会话的静音状态。
//
// 不能用 wca.ISimpleAudioVolume.GetMute：go-wca v0.3.0 把它的参数声明为
// *bool，但 Windows 的 BOOL 是 4 字节，COM 方法会往 1 字节的 Go bool 后面
// 多写 3 个字节，踩坏相邻栈变量。实测踩中的是 defer 记录里的函数指针
// （栈布局：cur 与 deferwrap1 相差 2 字节），函数返回时跳到被清零低 16 位的
// 地址，触发 fatal error: unknown caller pc —— 进程直接终止。GUI 表现为窗口
// 无征兆消失（windowsgui 无 stderr、无 WER 事件），游戏没人再隐藏/编码，
// 成片残留 raw take。这里按 BOOL（int32）自己调 vtable，杜绝越界写。
func sessionGetMute(vol *wca.ISimpleAudioVolume) (bool, error) {
	var b int32
	hr, _, _ := syscall.Syscall(vol.VTable().GetMute, 2,
		uintptr(unsafe.Pointer(vol)),
		uintptr(unsafe.Pointer(&b)),
		0)
	if hr != 0 {
		return false, ole.NewError(hr)
	}
	return b != 0, nil
}

// applySessionMute 设置单个会话的静音状态；返回是否实际发生了变更。
func applySessionMute(ctrl *wca.IAudioSessionControl, mute bool) bool {
	var vol *wca.ISimpleAudioVolume
	if err := ctrl.PutQueryInterface(wca.IID_ISimpleAudioVolume, &vol); err != nil {
		return false
	}
	defer vol.Release()
	// 读状态失败（会话正在销毁）时仍然写目标状态：游戏临退出时 GetMute 可能
	// 已经失败，但 SetMute 还有最后一次机会把 false 写回 Windows 的持久化
	// 状态。反之方向也安全（已静音时重复 SetMute(true) 无副作用）。
	if cur, err := sessionGetMute(vol); err == nil && cur == mute {
		return false
	}
	return vol.SetMute(mute, nil) == nil
}

// startGameAudioMute 在渲染游戏运行期间持续静音其音频会话（新出现的会话
// 也会在一个周期内被静音），并注册全局停止函数供主动终止路径调用。
//
// logf 可为 nil；解除静音失败（音频会话已随游戏销毁）时用它提示用户手动
// 取消静音——这种情况下 Windows 会把静音状态记下来，下次进游戏没有声音。
func startGameAudioMute(before map[int]bool, logf func(format string, args ...any)) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(stopCh) })
		<-done
	}
	audioMuteStopMu.Lock()
	audioMuteStop = stop
	audioMuteStopMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var pids map[int]bool
		everMuted := false
		for {
			if len(pids) == 0 {
				pids = newPIDs(before)
			}
			if len(pids) > 0 {
				if setGameAudioMute(pids, true) > 0 {
					everMuted = true
				}
			}
			select {
			case <-stopCh:
				if len(pids) > 0 {
					n := setGameAudioMute(pids, false)
					if everMuted && n == 0 && logf != nil {
						logf("  ⚠ 解除游戏静音失败：音频会话已随游戏销毁。下次进入游戏若没有声音，" +
							"请在 Windows 音量合成器（右键任务栏音量图标）里取消 cs2.exe 的静音")
					}
				}
				return
			case <-ticker.C:
			}
		}
	}()
}

// stopGameAudioMute 停止静音循环并解除静音（幂等）。必须在游戏进程退出前调用。
func stopGameAudioMute() {
	audioMuteStopMu.Lock()
	stop := audioMuteStop
	audioMuteStop = nil
	audioMuteStopMu.Unlock()
	if stop != nil {
		stop()
	}
}

// watchGameQuit 轮询游戏控制台日志，在游戏退出前解除静音（调用 onQuit）。
//
// 两个触发条件：
//   - 本轮录制完成：日志里出现 expected 次 "Finishing recording"。cfg 在最后
//     一段后先 demo_pause + wait（见 quitAfterRenderSuffix），游戏此时仍存活
//     约 1.5 秒，解除静音可以可靠写回 Windows 的每应用静音状态；
//   - 引擎开始退出："Host activate: Quitting"（兜底）。实测该行与
//     MainLoop returning / Source2Shutdown 落在同一秒，距音频会话销毁可能只有
//     几十毫秒，不能作为唯一信号。
//
// expected <= 0 时只监听 Quitting（调试模式 NoQuit 下没有录制结束后的等待窗口）。
func watchGameQuit(logPath string, expected int, stop <-chan struct{}, onQuit func()) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	quitMark := []byte("Host activate: Quitting")
	finishMark := []byte("Finishing recording")
	var lastSize int64 = -1
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// 日志只在有新增内容时重读（录制期间可达数 MB，避免每 100ms
			// 全量读取造成无谓开销）
			fi, err := os.Stat(logPath)
			if err != nil || fi.Size() == lastSize {
				continue
			}
			lastSize = fi.Size()
			data, err := os.ReadFile(logPath)
			if err != nil {
				continue
			}
			if bytes.Contains(data, quitMark) ||
				(expected > 0 && bytes.Count(data, finishMark) >= expected) {
				onQuit()
				return
			}
		}
	}
}
