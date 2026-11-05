package render

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// writeToneWAV 生成一段可播放的 PCM 单声道 WAV（测试音频会话用）。
func writeToneWAV(path string, seconds int) error {
	const rate = 44100
	n := rate * seconds
	buf := make([]byte, 44+2*n)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+2*n))
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)     // fmt 块大小
	binary.LittleEndian.PutUint16(buf[20:], 1)      // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1)      // 单声道
	binary.LittleEndian.PutUint32(buf[24:], rate)   // 采样率
	binary.LittleEndian.PutUint32(buf[28:], rate*2) // 字节率
	binary.LittleEndian.PutUint16(buf[32:], 2)      // 块对齐
	binary.LittleEndian.PutUint16(buf[34:], 16)     // 位深
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(2*n))
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/rate))
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(v))
	}
	return os.WriteFile(path, buf, 0o644)
}

// TestAudioSessionMute 验证静音逻辑不会踩坏栈（回归测试，默认跳过）。
//
// 背景：go-wca v0.3.0 的 ISimpleAudioVolume.GetMute(*bool) 会往 1 字节 bool
// 后面多写 3 字节 BOOL。实测踩中 defer 记录里的函数指针，applySessionMute
// 返回时跳到低 16 位被清零的地址，进程 fatal error 直接终止（GUI 闪退）。
// 本测试让测试进程自己播放声音产生音频会话，再驱动 setGameAudioMute；
// 若实现退回有缺陷的 GetMute，测试进程会当场崩溃（测试失败）。
//
// 需要真实音频设备且会短暂发声，因此仅在 CS2HI_AUDIO_MUTE_TEST=1 时运行。
func TestAudioSessionMute(t *testing.T) {
	if os.Getenv("CS2HI_AUDIO_MUTE_TEST") == "" {
		t.Skip("set CS2HI_AUDIO_MUTE_TEST=1 to run (plays a short tone)")
	}
	wav := filepath.Join(t.TempDir(), "tone.wav")
	if err := writeToneWAV(wav, 5); err != nil {
		t.Fatal(err)
	}
	winmm := syscall.NewLazyDLL("winmm.dll")
	play := winmm.NewProc("PlaySoundW")
	p, _ := syscall.UTF16PtrFromString(wav)
	const (
		sndAsync     = 0x0001
		sndNoDefault = 0x0002
		sndFileName  = 0x00020000
	)
	if r, _, err := play.Call(uintptr(unsafe.Pointer(p)), 0, sndAsync|sndNoDefault|sndFileName); r == 0 {
		t.Fatalf("PlaySound 失败: %v", err)
	}
	defer play.Call(0, 0, 0) // 停止播放

	pids := map[int]bool{os.Getpid(): true}
	// 等音频会话出现；出现后立即静音（尽量缩短发声时间）
	muted := false
	for i := 0; i < 40 && !muted; i++ {
		if setGameAudioMute(pids, true) > 0 {
			muted = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !muted {
		t.Skip("当前环境没有可用的音频会话（无音频设备？）")
	}
	// 已静音状态下反复读取（bug 正是发生在 GetMute）——能返回即说明栈未损坏
	for i := 0; i < 20; i++ {
		setGameAudioMute(pids, true)
	}
	if n := setGameAudioMute(pids, false); n > 0 {
		t.Log("已解除静音")
	}
}
