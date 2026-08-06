//go:build windows

package logui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/hellowind777/hellogrok/internal/appinfo"
)

// StatusFunc returns short title + detail text for the status panel.
type StatusFunc func() (short, detail string)

// Keep callback alive for the whole process (required by Win32).
var windowProcCallback = syscall.NewCallback(wndProc)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pLoadCursorW      = user32.NewProc("LoadCursorW")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pGetClientRect    = user32.NewProc("GetClientRect")
	pMoveWindow       = user32.NewProc("MoveWindow")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pSetTimer         = user32.NewProc("SetTimer")
	pKillTimer        = user32.NewProc("KillTimer")
	pSetForeground    = user32.NewProc("SetForegroundWindow")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGetLastError     = kernel32.NewProc("GetLastError")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pGetWindowRect    = user32.NewProc("GetWindowRect")

	classOnce   sync.Once
	className   *uint16
	classRegErr error

	openMu      sync.Mutex
	openHWND    uintptr
	keepAlive   = map[uintptr]*winState{}
	keepAliveMu sync.Mutex
)

const (
	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsVScroll          = 0x00200000
	wsHScroll          = 0x00100000
	wsBorder           = 0x00800000
	wsClipSiblings     = 0x04000000
	esMultiline        = 0x0004
	esReadonly         = 0x0800
	esAutovscroll      = 0x0040
	esAutohscroll      = 0x0080
	esWantreturn       = 0x1000
	swShow             = 5
	swRestore          = 9
	wmDestroy          = 0x0002
	wmSize             = 0x0005
	wmSetFont          = 0x0030
	wmSetText          = 0x000C
	wmGetTextLength    = 0x000E
	wmSetSel           = 0x00B1
	wmReplaceSel       = 0x00C2
	emSetLimitText     = 0x00C5
	emScrollCaret      = 0x00B7
	wmTimer            = 0x0113
	wmClose            = 0x0010
	idcArrow           = 32512
	colorWindow        = 5
	defaultGUIFont     = 17
	timerID            = 1
	statusHeight       = 120
	errClassExists     = 1410
	smCXScreen         = 0
	smCYScreen         = 1
	// default monitor size: narrower than before
	defaultWinW                = 720
	defaultWinH                = 560
	logTailBytes         int64 = 96 * 1024
	maxLogIncrementBytes int64 = 1 << 20
	maxLogEditCharacters       = 7 << 20
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type point struct{ X, Y int32 }
type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type rect struct{ Left, Top, Right, Bottom int32 }

type winState struct {
	path       string
	status     StatusFunc
	editSt     uintptr
	editLg     uintptr
	offset     int64
	hwnd       uintptr
	lastStatus string // skip SetText when unchanged — preserves user selection/copy
}

func lastErr() uint32 {
	e, _, _ := pGetLastError.Call()
	return uint32(e) // #nosec G115 -- GetLastError is defined to return a DWORD.
}

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p := appinfo.LogPath()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "[logui] %s %s\n", time.Now().Format("15:04:05"), msg)
	_ = f.Close()
}

// Open shows status + live log. Closing the window does not stop the tray proxy.
func Open(path string, status StatusFunc) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	if status == nil {
		status = func() (string, string) { return "—", "" }
	}

	openMu.Lock()
	if openHWND != 0 {
		hwnd := openHWND
		openMu.Unlock()
		pShowWindow.Call(hwnd, swRestore)
		pSetForeground.Call(hwnd)
		logf("focus existing hwnd=%#x", hwnd)
		return nil
	}
	openMu.Unlock()

	type readyMsg struct {
		err error
	}
	ready := make(chan readyMsg, 1)

	go func() {
		runtime.LockOSThread()
		hwnd, st, err := buildWindow(path, status)
		if err != nil {
			logf("buildWindow failed: %v", err)
			ready <- readyMsg{err: err}
			return
		}
		logf("buildWindow ok hwnd=%#x", hwnd)
		ready <- readyMsg{err: nil}
		pump(hwnd, st) // blocks until window closed
		logf("pump ended hwnd=%#x", hwnd)
	}()

	select {
	case r := <-ready:
		return r.err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("创建状态与日志窗口超时，请查看 %%LOCALAPPDATA%%\\hellogrok\\hellogrok.log")
	}
}

func buildWindow(path string, status StatusFunc) (uintptr, *winState, error) {
	hInstance, _, _ := pGetModuleHandleW.Call(0)

	classOnce.Do(func() {
		var err error
		className, err = syscall.UTF16PtrFromString("hellogrok.MonitorWindow.v6")
		if err != nil {
			classRegErr = err
			return
		}
		cursor, _, _ := pLoadCursorW.Call(0, idcArrow)
		wc := wndClassEx{
			Size:       uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:    windowProcCallback,
			Instance:   hInstance,
			Cursor:     cursor,
			Background: uintptr(colorWindow + 1),
			ClassName:  className,
		}
		atom, _, callErr := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 && lastErr() != errClassExists {
			classRegErr = fmt.Errorf("RegisterClassExW: %v last=%d", callErr, lastErr())
			return
		}
		logf("RegisterClassEx atom=%#x", atom)
	})
	if classRegErr != nil {
		return 0, nil, classRegErr
	}

	st := &winState{path: path, status: status}
	title, _ := syscall.UTF16PtrFromString("hellogrok — 状态与日志（关闭不影响代理）")
	x, y, w, h := loadGeometry()
	hwnd, _, callErr := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return 0, nil, fmt.Errorf("CreateWindowExW: %v last=%d", callErr, lastErr())
	}
	st.hwnd = hwnd

	editClass, _ := syscall.UTF16PtrFromString("EDIT")
	mkEdit := func() (uintptr, error) {
		h, _, e := pCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(editClass)),
			0,
			wsChild|wsVisible|wsVScroll|wsHScroll|wsBorder|wsClipSiblings|
				esMultiline|esReadonly|esAutovscroll|esAutohscroll|esWantreturn,
			0, 0, 200, 80,
			hwnd, 0, hInstance, 0,
		)
		if h == 0 {
			return 0, fmt.Errorf("EDIT: %v last=%d", e, lastErr())
		}
		return h, nil
	}
	var err error
	if st.editSt, err = mkEdit(); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	if st.editLg, err = mkEdit(); err != nil {
		pDestroyWindow.Call(hwnd)
		return 0, nil, err
	}
	pSendMessageW.Call(st.editLg, emSetLimitText, 8<<20, 0)
	hfont, _, _ := pGetStockObject.Call(defaultGUIFont)
	pSendMessageW.Call(st.editSt, wmSetFont, hfont, 1)
	pSendMessageW.Call(st.editLg, wmSetFont, hfont, 1)

	refreshStatus(st)
	st.offset = reloadLog(st, logTailBytes)
	layout(st)

	keepAliveMu.Lock()
	keepAlive[hwnd] = st
	keepAliveMu.Unlock()
	openMu.Lock()
	openHWND = hwnd
	openMu.Unlock()

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	pSetForeground.Call(hwnd)
	pSetTimer.Call(hwnd, timerID, 400, 0)
	return hwnd, st, nil
}

func pump(hwnd uintptr, st *winState) {
	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // #nosec G115 -- GetMessageW returns a signed 32-bit BOOL through syscall.Call.
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	keepAliveMu.Lock()
	delete(keepAlive, hwnd)
	keepAliveMu.Unlock()
	openMu.Lock()
	if openHWND == hwnd {
		openHWND = 0
	}
	openMu.Unlock()
	_ = st
}

// fix Open goroutine to call pump after ready
// (Open already structured to call pump after ready <- nil)

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	st := stateFrom(hwnd)
	switch msg {
	case wmSize:
		if st != nil {
			layout(st)
		}
		return 0
	case wmTimer:
		if st != nil && wParam == timerID {
			refreshStatus(st)
			appendNew(st)
		}
		return 0
	case wmClose:
		// remember geometry before destroy
		if st != nil {
			saveGeometry(hwnd)
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		pKillTimer.Call(hwnd, timerID)
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

func stateFrom(hwnd uintptr) *winState {
	keepAliveMu.Lock()
	defer keepAliveMu.Unlock()
	return keepAlive[hwnd]
}

func layout(st *winState) {
	var rc rect
	ok, _, _ := pGetClientRect.Call(st.hwnd, uintptr(unsafe.Pointer(&rc)))
	width, height := rc.Right-rc.Left, rc.Bottom-rc.Top
	if ok == 0 || width <= 0 || height <= 0 {
		return
	}
	w := uintptr(width)  // #nosec G115 -- positive Win32 client dimensions are bounded by int32.
	h := uintptr(height) // #nosec G115 -- positive Win32 client dimensions are bounded by int32.
	sh := uintptr(statusHeight)
	if h < sh+80 {
		sh = h / 3
	}
	if st.editSt != 0 {
		pMoveWindow.Call(st.editSt, 0, 0, w, sh, 1)
	}
	if st.editLg != 0 {
		pMoveWindow.Call(st.editLg, 0, sh, w, h-sh, 1)
	}
}

func refreshStatus(st *winState) {
	short, detail := "—", ""
	if st.status != nil {
		short, detail = st.status()
	}
	text := "【状态】 " + short + "\r\n" +
		"----------------------------------------\r\n" +
		detail + "\r\n" +
		"----------------------------------------\r\n" +
		"下方为实时日志。关闭本窗口不会停止代理。"
	// WM_SETTEXT clears selection; only update when content actually changes.
	if text == st.lastStatus {
		return
	}
	st.lastStatus = text
	setEditText(st.editSt, text)
}

func reloadLog(st *winState, max int64) int64 {
	data, size, err := readTailFile(st.path, max)
	if err != nil {
		setEditText(st.editLg, "无法读取日志:\r\n"+st.path+"\r\n"+err.Error())
		return 0
	}
	setEditText(st.editLg, "【日志】 "+st.path+"\r\n"+
		"----------------------------------------\r\n"+
		toCRLF(string(data)))
	scrollToEnd(st.editLg)
	return size
}

func appendNew(st *winState) {
	fi, err := os.Stat(st.path)
	if err != nil {
		return
	}
	size := fi.Size()
	if size < st.offset {
		st.offset = reloadLog(st, logTailBytes)
		return
	}
	if size == st.offset {
		return
	}
	delta := size - st.offset
	currentLength, _, _ := pSendMessageW.Call(st.editLg, wmGetTextLength, 0, 0)
	if delta > maxLogIncrementBytes || currentLength >= maxLogEditCharacters ||
		int64(currentLength)+delta > maxLogEditCharacters {
		st.offset = reloadLog(st, logTailBytes)
		return
	}
	f, err := os.Open(st.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, 0); err != nil {
		return
	}
	buf, err := io.ReadAll(io.LimitReader(f, delta))
	if err != nil || len(buf) == 0 {
		return
	}
	st.offset += int64(len(buf))
	appendEdit(st.editLg, toCRLF(string(buf)))
	scrollToEnd(st.editLg)
}

func setEditText(edit uintptr, text string) {
	if edit == 0 {
		return
	}
	p, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(edit, wmSetText, 0, uintptr(unsafe.Pointer(p)))
}

func appendEdit(edit uintptr, text string) {
	if edit == 0 {
		return
	}
	lenR, _, _ := pSendMessageW.Call(edit, wmGetTextLength, 0, 0)
	pSendMessageW.Call(edit, wmSetSel, lenR, lenR)
	p, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(edit, wmReplaceSel, 0, uintptr(unsafe.Pointer(p)))
}

func scrollToEnd(edit uintptr) {
	if edit == 0 {
		return
	}
	lenR, _, _ := pSendMessageW.Call(edit, wmGetTextLength, 0, 0)
	pSendMessageW.Call(edit, wmSetSel, lenR, lenR)
	pSendMessageW.Call(edit, emScrollCaret, 0, 0)
}

func toCRLF(s string) string {
	out := make([]byte, 0, len(s)+64)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			if i == 0 || s[i-1] != '\r' {
				out = append(out, '\r', '\n')
			} else {
				out = append(out, '\n')
			}
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// geometryJSON is persisted under %LOCALAPPDATA%\hellogrok\window.json
type geometryJSON struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func geometryPath() string {
	return filepath.Join(appinfo.DataDir(), "window.json")
}

func defaultGeometry() (x, y, w, h int) {
	w, h = defaultWinW, defaultWinH
	sw, _, _ := pGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := pGetSystemMetrics.Call(smCYScreen)
	if sw == 0 {
		sw = 1920
	}
	if sh == 0 {
		sh = 1080
	}
	// horizontal center, slightly above vertical center
	x = (int(sw) - w) / 2
	y = (int(sh)-h)/2 - int(sh)/20
	if y < 20 {
		y = 20
	}
	if x < 0 {
		x = 0
	}
	return
}

func loadGeometry() (x, y, w, h int) {
	x, y, w, h = defaultGeometry()
	b, err := os.ReadFile(geometryPath())
	if err != nil {
		return
	}
	var g geometryJSON
	if json.Unmarshal(b, &g) != nil {
		return
	}
	if g.W >= 400 && g.H >= 300 {
		w, h = g.W, g.H
	}
	// keep on-screen roughly
	sw, _, _ := pGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := pGetSystemMetrics.Call(smCYScreen)
	if g.X > -100 && g.Y > -100 && int(sw) > 0 && g.X < int(sw)-50 && g.Y < int(sh)-50 {
		x, y = g.X, g.Y
	}
	return
}

func saveGeometry(hwnd uintptr) {
	var rc rect
	r, _, _ := pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	if r == 0 {
		return
	}
	g := geometryJSON{
		X: int(rc.Left),
		Y: int(rc.Top),
		W: int(rc.Right - rc.Left),
		H: int(rc.Bottom - rc.Top),
	}
	if g.W < 200 || g.H < 150 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(geometryPath()), 0o700)
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(geometryPath(), b, 0o600)
	logf("saved geometry %+v", g)
}
