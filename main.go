// Windows 屏幕苍蝇 - 无安装、无联网、无持久化的小恶作剧。
// Build (Windows): go build -ldflags="-H=windowsgui -s -w" -o FlyOnScreen.exe main.go
// 运行后是透明、鼠标穿透的置顶苍蝇；Esc 或右上角按钮退出，3 分钟自动结束。
package main

import (
	"bytes"
	_ "embed"
	"image/png"
	"math"
	"math/rand"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Four realistic transparent fly sprites. They are embedded directly into the EXE;
// the recipient only needs the single executable file.
//
//go:embed assets/fly1.png
var pngFly1 []byte

//go:embed assets/fly2.png
var pngFly2 []byte

//go:embed assets/fly3.png
var pngFly3 []byte

//go:embed assets/fly4.png
var pngFly4 []byte

type sprite struct {
	w, h int
	pix  []byte
} // premultiplied BGRA
var flySprites [4]sprite

func loadSprites() bool {
	files := [][]byte{pngFly1, pngFly2, pngFly3, pngFly4}
	for i, data := range files {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return false
		}
		bounds := img.Bounds()
		w, h := bounds.Dx(), bounds.Dy()
		out := sprite{w: w, h: h, pix: make([]byte, w*h*4)}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				// Go Color.RGBA returns alpha-premultiplied 16-bit components.
				r, g, b, a := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
				ix := (y*w + x) * 4
				out.pix[ix] = byte(b >> 8)
				out.pix[ix+1] = byte(g >> 8)
				out.pix[ix+2] = byte(r >> 8)
				out.pix[ix+3] = byte(a >> 8)
			}
		}
		flySprites[i] = out
	}
	return true
}

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	gdi32               = syscall.NewLazyDLL("gdi32.dll")
	pCreateWindowEx     = user32.NewProc("CreateWindowExW")
	pDestroyWindow      = user32.NewProc("DestroyWindow")
	pShowWindow         = user32.NewProc("ShowWindow")
	pSetWindowPos       = user32.NewProc("SetWindowPos")
	pGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	pGetAsyncKeyState   = user32.NewProc("GetAsyncKeyState")
	pGetCursorPos       = user32.NewProc("GetCursorPos")
	pPeekMessage        = user32.NewProc("PeekMessageW")
	pTranslateMessage   = user32.NewProc("TranslateMessage")
	pDispatchMessage    = user32.NewProc("DispatchMessageW")
	pGetDC              = user32.NewProc("GetDC")
	pReleaseDC          = user32.NewProc("ReleaseDC")
	pUpdateLayered      = user32.NewProc("UpdateLayeredWindow")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
)

const (
	wsPopup         = 0x80000000
	wsVisible       = 0x10000000
	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExTopmost     = 0x00000008
	wsExToolwindow  = 0x00000080
	wsExNoActivate  = 0x08000000
	swpNoActivate   = 0x0010
	swpNoZorder     = 0x0004
	swpNoSize       = 0x0001
	ulwAlpha        = 0x00000002
	dibRGBColors    = 0
	acSrcAlpha      = 1
	pmRemove        = 1
	wmQuit          = 0x0012
	vkEscape        = 0x1B
	vkLeftButton    = 0x01
	vkM             = 0x4D
	flyWindowSize   = 96
	flyCount        = 13
	maxDuration     = 180 * time.Second
)

type point struct{ X, Y int32 }
type size struct{ CX, CY int32 }
type msg struct {
	Hwnd           uintptr
	Message        uint32
	_              uint32
	Wparam, Lparam uintptr
	Time           uint32
	Pt             point
	Private        uint32
}
type bitmapInfoHeader struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}
type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}
type blendFunction struct{ BlendOp, BlendFlags, SourceConstantAlpha, AlphaFormat byte }

type rgb struct {
	r, g, b int
	a       int
}

func RGBA(r, g, b, a int) rgb { return rgb{r, g, b, a} }

type canvas struct {
	w, h          int
	px            []byte // premultiplied BGRA
	angle         float64
	cs, sn, scale float64
}

func (c *canvas) clear() { clear(c.px) }
func (c *canvas) setTransform(angle, scale float64) {
	c.angle = angle
	c.cs = math.Cos(angle)
	c.sn = math.Sin(angle)
	c.scale = scale
}
func (c *canvas) local(x, y float64) (float64, float64) {
	x *= c.scale
	y *= c.scale
	return float64(c.w)/2 + x*c.cs - y*c.sn, float64(c.h)/2 + x*c.sn + y*c.cs
}
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func (c *canvas) blend(x, y int, col rgb, cov float64) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	alpha := int(float64(col.a)*clampFloat(cov, 0, 1) + 0.5)
	if alpha == 0 {
		return
	}
	i := (y*c.w + x) * 4
	inv := 255 - alpha
	c.px[i] = byte((col.b*alpha + int(c.px[i])*inv + 127) / 255)
	c.px[i+1] = byte((col.g*alpha + int(c.px[i+1])*inv + 127) / 255)
	c.px[i+2] = byte((col.r*alpha + int(c.px[i+2])*inv + 127) / 255)
	c.px[i+3] = byte(alpha + (int(c.px[i+3])*inv+127)/255)
}
func (c *canvas) ellipse(cx, cy, rx, ry, tilt float64, col rgb) {
	sx, sy := c.local(cx, cy)
	r1, r2 := rx*c.scale, ry*c.scale
	ang := c.angle + tilt
	co, si := math.Cos(ang), math.Sin(ang)
	extx := math.Sqrt(r1*r1*co*co+r2*r2*si*si) + 2
	exty := math.Sqrt(r1*r1*si*si+r2*r2*co*co) + 2
	x0 := clampInt(int(math.Floor(sx-extx)), 0, c.w-1)
	x1 := clampInt(int(math.Ceil(sx+extx)), 0, c.w-1)
	y0 := clampInt(int(math.Floor(sy-exty)), 0, c.h-1)
	y1 := clampInt(int(math.Ceil(sy+exty)), 0, c.h-1)
	if r1 < 0.01 || r2 < 0.01 {
		return
	}
	softness := 0.8 / math.Min(r1, r2)
	for y := y0; y <= y1; y++ {
		py := float64(y) + 0.5 - sy
		for x := x0; x <= x1; x++ {
			px := float64(x) + 0.5 - sx
			xx := (px*co + py*si) / r1
			yy := (-px*si + py*co) / r2
			d := math.Sqrt(xx*xx + yy*yy)
			a := clampFloat((1+softness-d)/(2*softness), 0, 1)
			if a > 0 {
				c.blend(x, y, col, a)
			}
		}
	}
}
func (c *canvas) line(x1, y1, x2, y2, width float64, col rgb) {
	ax, ay := c.local(x1, y1)
	bx, by := c.local(x2, y2)
	radius := math.Max(0.35, width*c.scale/2)
	x0 := clampInt(int(math.Floor(math.Min(ax, bx)-radius-2)), 0, c.w-1)
	xh := clampInt(int(math.Ceil(math.Max(ax, bx)+radius+2)), 0, c.w-1)
	y0 := clampInt(int(math.Floor(math.Min(ay, by)-radius-2)), 0, c.h-1)
	yh := clampInt(int(math.Ceil(math.Max(ay, by)+radius+2)), 0, c.h-1)
	dx, dy := bx-ax, by-ay
	d2 := dx*dx + dy*dy
	for y := y0; y <= yh; y++ {
		for x := x0; x <= xh; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			t := 0.0
			if d2 > 0 {
				t = clampFloat(((px-ax)*dx+(py-ay)*dy)/d2, 0, 1)
			}
			dist := math.Hypot(px-ax-t*dx, py-ay-t*dy)
			a := clampFloat(radius+0.85-dist, 0, 1)
			if a > 0 {
				c.blend(x, y, col, a)
			}
		}
	}
}

// Rotate the real photograph and bilinearly resample it to a fly no more than
// about 40-50 physical screen pixels, as chosen from the screenshot.
func (c *canvas) photo(s *sprite, angle, diameter float64) {
	co, si := math.Cos(angle), math.Sin(angle)
	centerX, centerY := float64(c.w)/2, float64(c.h)/2
	radius := int(math.Ceil(diameter*.75)) + 2
	x0, x1 := clampInt(c.w/2-radius, 0, c.w-1), clampInt(c.w/2+radius, 0, c.w-1)
	y0, y1 := clampInt(c.h/2-radius, 0, c.h-1), clampInt(c.h/2+radius, 0, c.h-1)
	factor := float64(s.w) / diameter
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			px := float64(x) + .5 - centerX
			py := float64(y) + .5 - centerY
			// Source PNGs all have their heads facing to the right.
			sx := (px*co+py*si)*factor + float64(s.w)/2 - .5
			sy := (-px*si+py*co)*factor + float64(s.h)/2 - .5
			xi, yi := int(math.Floor(sx)), int(math.Floor(sy))
			if xi < 0 || yi < 0 || xi+1 >= s.w || yi+1 >= s.h {
				continue
			}
			fx, fy := sx-float64(xi), sy-float64(yi)
			a := (yi*s.w + xi) * 4
			b := a + 4
			d := a + s.w*4
			e := d + 4
			w0 := (1 - fx) * (1 - fy)
			w1 := fx * (1 - fy)
			w2 := (1 - fx) * fy
			w3 := fx * fy
			dst := (y*c.w + x) * 4
			for k := 0; k < 4; k++ {
				c.px[dst+k] = byte(float64(s.pix[a+k])*w0 + float64(s.pix[b+k])*w1 + float64(s.pix[d+k])*w2 + float64(s.pix[e+k])*w3 + .5)
			}
		}
	}
}

type mode int

const (
	walking mode = iota
	rubbing
	flying
)

type fly struct {
	x, y                           float64
	angle, phase, scale, speed     float64
	state                          mode
	stateUntil                     float64
	turnBias                       float64
	hwnd, memDC, bitmap, oldBitmap uintptr
	dib                            []byte
	canvas                         canvas
	homeW, homeH                   int
	variant                        int
}

func (f *fly) switchState(t float64, rng *rand.Rand) {
	p := rng.Float64()
	switch f.state {
	case walking:
		if p < 0.32 {
			f.state = rubbing
			f.stateUntil = t + 1.2 + rng.Float64()*2.2
		} else if p < 0.65 {
			f.state = flying
			f.stateUntil = t + 1.4 + rng.Float64()*3.2
			f.angle += rng.Float64()*1.4 - 0.7
		} else {
			f.stateUntil = t + 2 + rng.Float64()*3.5
		}
	case rubbing:
		if p < 0.25 {
			f.state = flying
			f.stateUntil = t + 1.1 + rng.Float64()*2.8
		} else {
			f.state = walking
			f.stateUntil = t + 2 + rng.Float64()*3
		}
	case flying:
		if p < 0.53 {
			f.state = walking
			f.stateUntil = t + 1 + rng.Float64()*3
		} else {
			f.state = rubbing
			f.stateUntil = t + 1 + rng.Float64()*2
		}
	}
	f.turnBias = rng.Float64()*2 - 1
}
func (f *fly) update(t, dt float64, bounds [4]float64, rng *rand.Rand) {
	if t >= f.stateUntil {
		f.switchState(t, rng)
	}
	f.phase += dt
	switch f.state {
	case walking:
		f.angle += dt * (0.18*math.Sin(t*.95+f.turnBias*8) + 0.13*f.turnBias)
		v := f.speed * 0.35
		f.x += math.Cos(f.angle) * v * dt
		f.y += math.Sin(f.angle) * v * dt
	case rubbing:
		f.angle += math.Sin(t*1.1+f.turnBias) * dt * 0.06
	case flying:
		f.angle += dt * (0.35*math.Sin(t*1.5+f.turnBias*10) + 0.23*f.turnBias)
		v := f.speed * 3.2
		f.x += math.Cos(f.angle) * v * dt
		f.y += math.Sin(f.angle)*v*dt + math.Sin(t*16+f.phase)*dt*12
	}
	xmin, ymin, xmax, ymax := bounds[0], bounds[1], bounds[2], bounds[3]
	margin := 22.0
	if f.x < xmin+margin {
		f.x = xmin + margin
		f.angle = math.Pi - f.angle
	}
	if f.x > xmax-margin {
		f.x = xmax - margin
		f.angle = math.Pi - f.angle
	}
	if f.y < ymin+margin {
		f.y = ymin + margin
		f.angle = -f.angle
	}
	if f.y > ymax-margin {
		f.y = ymax - margin
		f.angle = -f.angle
	}
}
func (f *fly) draw(t float64) {
	c := &f.canvas
	c.clear()
	diameter := f.scale // chosen physical fly size: 40-50 pixels
	if f.state == flying {
		diameter += 1.2 * math.Sin(t*25)
	}
	c.photo(&flySprites[f.variant], f.angle, diameter)
	c.setTransform(f.angle, 1)
	// A translucent flutter aura makes the wing beats visibly different from
	// crawling while retaining the photograph's own wing shape.
	if f.state == flying {
		flutter := math.Sin(t*55 + f.phase*3)
		for _, side := range []float64{-1, 1} {
			yy := side * (diameter*.18 + flutter*2.5)
			c.ellipse(-diameter*.12, yy, diameter*.18, diameter*.035, side*.50, RGBA(137, 154, 173, 60))
		}
	}
	// The front pair of legs is animated separately on top of the photo.
	// When grooming, the two forelegs repeatedly meet and separate.
	if f.state == rubbing || f.state == walking {
		for _, side := range []float64{-1, 1} {
			var startY, kneeY, tipY float64
			startY = side * diameter * .058
			if f.state == rubbing {
				pulse := math.Sin(t*17 + side*.8)
				kneeY = side * (diameter*.102 + 1.3*pulse)
				tipY = side * (diameter*.018 + 2.4*math.Sin(t*19+side*1.2))
			} else {
				pulse := math.Sin(t*12 + side*math.Pi)
				kneeY = side * diameter * .15
				tipY = side*diameter*.19 + 2.5*pulse
			}
			fromX := diameter * .155
			kneeX := diameter * .28
			tipX := diameter*.37 + 1.4*math.Sin(t*17+side)
			c.line(fromX, startY, kneeX, kneeY, 1.28, RGBA(30, 30, 29, 219))
			c.line(kneeX, kneeY, tipX, tipY, 0.95, RGBA(27, 28, 27, 226))
		}
	}
}
func newFly(x, y, angle, scale, speed float64, state mode, until float64, screenDC uintptr) (*fly, bool) {
	class := syscall.StringToUTF16Ptr("STATIC")
	hwnd, _, _ := pCreateWindowEx.Call(wsExLayered|wsExTransparent|wsExTopmost|wsExToolwindow|wsExNoActivate,
		uintptr(unsafe.Pointer(class)), 0, wsPopup,
		uintptr(int32(x-flyWindowSize/2)), uintptr(int32(y-flyWindowSize/2)), flyWindowSize, flyWindowSize,
		0, 0, 0, 0)
	if hwnd == 0 {
		return nil, false
	}
	memDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	bmi := bitmapInfo{Header: bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: flyWindowSize, Height: -flyWindowSize, Planes: 1, BitCount: 32, Compression: 0}}
	var bits unsafe.Pointer
	bitmap, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bmi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if memDC == 0 || bitmap == 0 || bits == nil {
		pDestroyWindow.Call(hwnd)
		if memDC != 0 {
			pDeleteDC.Call(memDC)
		}
		return nil, false
	}
	old, _, _ := pSelectObject.Call(memDC, bitmap)
	dib := unsafe.Slice((*byte)(bits), flyWindowSize*flyWindowSize*4)
	f := &fly{x: x, y: y, angle: angle, scale: scale, speed: speed, state: state, stateUntil: until, hwnd: hwnd, memDC: memDC, bitmap: bitmap, oldBitmap: old, dib: dib,
		canvas: canvas{w: flyWindowSize, h: flyWindowSize, px: dib}}
	pShowWindow.Call(hwnd, 8) // SW_SHOWNA: show without stealing focus
	return f, true
}
func (f *fly) present(screenDC uintptr) {
	dest := point{int32(f.x) - flyWindowSize/2, int32(f.y) - flyWindowSize/2}
	start := point{0, 0}
	sz := size{flyWindowSize, flyWindowSize}
	blend := blendFunction{BlendOp: 0, BlendFlags: 0, SourceConstantAlpha: 255, AlphaFormat: acSrcAlpha}
	pUpdateLayered.Call(f.hwnd, screenDC, uintptr(unsafe.Pointer(&dest)), uintptr(unsafe.Pointer(&sz)), f.memDC, uintptr(unsafe.Pointer(&start)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)
}
func (f *fly) dispose() {
	pSelectObject.Call(f.memDC, f.oldBitmap)
	pDeleteObject.Call(f.bitmap)
	pDeleteDC.Call(f.memDC)
	pDestroyWindow.Call(f.hwnd)
}
func getScreenMetric(index int) int32 {
	r, _, _ := pGetSystemMetrics.Call(uintptr(index))
	return int32(r)
}
func pressed(key uintptr) bool { r, _, _ := pGetAsyncKeyState.Call(key); return r&0x8000 != 0 }
func utf16(s string) uintptr   { return uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(s))) }
func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !loadSprites() {
		return
	}
	// GetSystemMetrics and the window coordinates refer to real pixels.
	if p := user32.NewProc("SetProcessDPIAware"); p.Find() == nil {
		p.Call()
	}
	x, y, w, h := getScreenMetric(76), getScreenMetric(77), getScreenMetric(78), getScreenMetric(79)
	if w < 300 || h < 200 {
		x, y, w, h = 0, 0, getScreenMetric(0), getScreenMetric(1)
	}
	if w < 300 || h < 200 {
		return
	}
	screenDC, _, _ := pGetDC.Call(0)
	if screenDC == 0 {
		return
	}
	defer pReleaseDC.Call(0, screenDC)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var flies []*fly
	for i := 0; i < flyCount; i++ {
		xx := float64(x) + 45 + rng.Float64()*float64(w-90)
		yy := float64(y) + 45 + rng.Float64()*float64(h-90)
		state := walking
		if i%4 == 0 {
			state = flying
		} else if i%4 == 1 {
			state = rubbing
		}
		f, ok := newFly(xx, yy, rng.Float64()*math.Pi*2, 40+rng.Float64()*10, 28+rng.Float64()*28, state, 1.5+rng.Float64()*3.5, screenDC)
		if ok {
			f.variant = i % 4
			flies = append(flies, f)
		}
	}
	defer func() {
		for _, f := range flies {
			f.dispose()
		}
	}()
	if len(flies) == 0 {
		return
	}
	// Make the buzz start with the initial airborne flies, not an all-grounded
	// first buffer. The Windows sound device may still need a brief warm-up.
	initialFlying := 0
	for _, f := range flies {
		if f.state == flying {
			initialFlying++
		}
	}
	sound := startFlySound(initialFlying)
	defer sound.close()
	// Discreet exit control: 20x20 px = 400 square pixels, about one tenth
	// of the previous 130x31 px (=4030 square pixels) button area.
	// Escape remains a full-size keyboard exit, independent of this tiny button.
	btnW, btnH := int32(20), int32(20)
	bx, by := x+w-btnW-12, y+12
	btn, _, _ := pCreateWindowEx.Call(wsExTopmost|wsExToolwindow,
		utf16("BUTTON"), utf16("×"), wsPopup|wsVisible,
		uintptr(bx), uintptr(by), uintptr(btnW), uintptr(btnH), 0, 0, 0, 0)
	if btn != 0 {
		defer pDestroyWindow.Call(btn)
	}
	// Same compact footprint as the close button. Click ♪ or press M to mute.
	muteX, muteY := bx-btnW-7, by
	muteBtn, _, _ := pCreateWindowEx.Call(wsExTopmost|wsExToolwindow,
		utf16("BUTTON"), utf16("♪"), wsPopup|wsVisible,
		uintptr(muteX), uintptr(muteY), uintptr(btnW), uintptr(btnH), 0, 0, 0, 0)
	if muteBtn != 0 {
		defer pDestroyWindow.Call(muteBtn)
	}
	setWindowText := user32.NewProc("SetWindowTextW")
	changeMute := func() {
		muted := sound.toggleMute()
		if muteBtn != 0 {
			label := "♪"
			if muted {
				label = "M"
			}
			setWindowText.Call(muteBtn, utf16(label))
		}
	}
	bounds := [4]float64{float64(x), float64(y), float64(x + w), float64(y + h)}
	started := time.Now()
	last := started
	prevMouseDown := false
	prevMDown := false
	var message msg
	for time.Since(started) < maxDuration {
		for {
			has, _, _ := pPeekMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, pmRemove)
			if has == 0 {
				break
			}
			if message.Message == wmQuit {
				return
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
		}
		if pressed(vkEscape) {
			return
		}
		mDown := pressed(vkM)
		if mDown && !prevMDown {
			changeMute()
		}
		prevMDown = mDown
		mouseDown := pressed(vkLeftButton)
		if mouseDown && !prevMouseDown {
			var cursor point
			pGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
			if btn != 0 && cursor.X >= bx && cursor.X < bx+btnW && cursor.Y >= by && cursor.Y < by+btnH {
				return
			}
			if muteBtn != 0 && cursor.X >= muteX && cursor.X < muteX+btnW && cursor.Y >= muteY && cursor.Y < muteY+btnH {
				changeMute()
			}
		}
		prevMouseDown = mouseDown
		now := time.Now()
		dt := now.Sub(last).Seconds()
		if dt > 0.08 {
			dt = 0.08
		}
		last = now
		t := now.Sub(started).Seconds()
		flyingNow := 0
		for _, f := range flies {
			f.update(t, dt, bounds, rng)
			if f.state == flying {
				flyingNow++
			}
			f.draw(t)
			f.present(screenDC)
		}
		sound.setFlying(flyingNow)
		time.Sleep(33 * time.Millisecond)
	}
}
