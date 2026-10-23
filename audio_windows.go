package main

// A tiny self-contained fly-wing audio synthesizer using WinMM waveOut.
// This is synthesized locally in memory; no external audio file, network,
// codec, runtime install, or Windows system-wide volume changes are needed.

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	audioSampleRate   = 22050
	audioChannels     = 1
	audioChunkSamples = 2205 // 100ms per audio buffer
	audioBufferCount  = 4
	waveMapper        = uintptr(0xffffffff)
	waveFormatPCM     = 1
	whdrDone          = 0x00000001
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	ExtraSize      uint16
}

type waveHeader struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

type soundBuffer struct {
	header   waveHeader
	samples  []int16
	pins     runtime.Pinner
	prepared bool
	queued   bool
}

type flySound struct {
	flying  atomic.Int32
	muted   atomic.Bool
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

var (
	winmm                  = syscall.NewLazyDLL("winmm.dll")
	waveOutOpen            = winmm.NewProc("waveOutOpen")
	waveOutPrepareHeader   = winmm.NewProc("waveOutPrepareHeader")
	waveOutWrite           = winmm.NewProc("waveOutWrite")
	waveOutUnprepareHeader = winmm.NewProc("waveOutUnprepareHeader")
	waveOutReset           = winmm.NewProc("waveOutReset")
	waveOutClose           = winmm.NewProc("waveOutClose")
	kernel32Audio          = syscall.NewLazyDLL("kernel32.dll")
	globalAlloc            = kernel32Audio.NewProc("GlobalAlloc")
	globalFree             = kernel32Audio.NewProc("GlobalFree")
)

func startFlySound(initialFlying int) *flySound {
	sound := &flySound{stop: make(chan struct{}), stopped: make(chan struct{})}
	sound.setFlying(initialFlying)
	go sound.run()
	return sound
}

func (a *flySound) close() {
	if a == nil {
		return
	}
	a.once.Do(func() { close(a.stop); <-a.stopped })
}

// Only the fly simulation writes the flying count. The audio worker reads
// it atomically. On computers without a sound device, the visual app keeps
// running; mute still works but there is nothing audible to suppress.
func (a *flySound) setFlying(n int) {
	if a != nil {
		a.flying.Store(int32(n))
	}
}
func (a *flySound) toggleMute() bool {
	if a == nil {
		return true
	}
	muted := !a.muted.Load()
	a.muted.Store(muted)
	return muted
}

type buzzSynth struct {
	phase, env, sec, smoothNoise float64
	seed                         uint32
}

func (s *buzzSynth) fill(pcm []int16, count int32, muted bool) {
	target := 0.0
	if !muted {
		if count > 0 {
			// Enough to hear a fly nearby, but intentionally not loud.
			target = math.Min(0.102, 0.027+0.013*math.Sqrt(float64(count)))
		} else {
			target = 0.0015 // barely audible while all flies are grounded
		}
	}
	if s.seed == 0 {
		s.seed = 0x5eeda15
	}
	dt := 1.0 / float64(audioSampleRate)
	for i := range pcm {
		// Soft attacks/releases, avoiding clicks when flies take off or land.
		s.env += (target - s.env) * 0.00065
		s.sec += dt
		// Housefly wing tone, with subtle wandering of pitch and flutter.
		hz := 178.0 + 16*math.Sin(2*math.Pi*0.39*s.sec) +
			8*math.Sin(2*math.Pi*1.17*s.sec)
		s.phase += 2 * math.Pi * hz * dt
		if s.phase > 2*math.Pi {
			s.phase -= 2 * math.Pi
		}
		s.seed = 1664525*s.seed + 1013904223
		noise := float64((s.seed>>16)&65535)/32767.5 - 1
		s.smoothNoise = 0.88*s.smoothNoise + 0.12*noise
		flutter := 0.91 + 0.09*math.Sin(2*math.Pi*22*s.sec)
		wave := (0.64*math.Sin(s.phase) +
			0.27*math.Sin(2*s.phase+0.18) +
			0.13*math.Sin(3*s.phase+0.42) +
			0.14*s.smoothNoise) * flutter
		v := s.env * wave
		if v > 0.9 {
			v = 0.9
		}
		if v < -0.9 {
			v = -0.9
		}
		pcm[i] = int16(v * 32767)
	}
}

func (a *flySound) run() {
	defer close(a.stopped)
	format := waveFormatEx{
		FormatTag: waveFormatPCM, Channels: audioChannels,
		SamplesPerSec: audioSampleRate, AvgBytesPerSec: audioSampleRate * 2,
		BlockAlign: 2, BitsPerSample: 16,
	}
	var device uintptr
	result, _, _ := waveOutOpen.Call(uintptr(unsafe.Pointer(&device)), waveMapper,
		uintptr(unsafe.Pointer(&format)), 0, 0, 0)
	if result != 0 || device == 0 {
		return
	}
	var buffers [audioBufferCount]soundBuffer
	defer func() {
		waveOutReset.Call(device)
		for i := range buffers {
			b := &buffers[i]
			if b.prepared {
				waveOutUnprepareHeader.Call(device, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(waveHeader{}))
			}
			// WinMM has returned all buffers after waveOutReset.
			b.pins.Unpin()
		}
		waveOutClose.Call(device)
	}()
	var synth buzzSynth
	for i := range buffers {
		b := &buffers[i]
		b.samples = make([]int16, audioChunkSamples)
		b.header = waveHeader{
			Data:         uintptr(unsafe.Pointer(&b.samples[0])),
			BufferLength: audioChunkSamples * 2,
		}
		// Pin both Go allocations for WinMM's asynchronous waveOutWrite.
		b.pins.Pin(&b.header)
		b.pins.Pin(&b.samples[0])
		r, _, _ := waveOutPrepareHeader.Call(device, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(waveHeader{}))
		if r != 0 {
			return
		}
		b.prepared = true
		synth.fill(b.samples, a.flying.Load(), a.muted.Load())
		r, _, _ = waveOutWrite.Call(device, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(waveHeader{}))
		if r != 0 {
			return
		}
		b.queued = true
	}
	ticker := time.NewTicker(12 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			for i := range buffers {
				b := &buffers[i]
				if b.queued && b.header.Flags&whdrDone != 0 {
					synth.fill(b.samples, a.flying.Load(), a.muted.Load())
					r, _, _ := waveOutWrite.Call(device, uintptr(unsafe.Pointer(&b.header)), unsafe.Sizeof(waveHeader{}))
					if r != 0 {
						return
					}
				}
			}
		}
	}
}
