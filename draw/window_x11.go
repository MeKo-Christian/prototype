//go:build !glfw && !windows && !js

package draw

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <X11/Xatom.h>
#include <X11/keysym.h>
#include <stdlib.h>
#include <string.h>

// Create XImage wrapper (avoids Go pointer issues with XCreateImage macro)
XImage* createXImage(Display* display, Visual* visual, unsigned int depth,
                     int format, int offset, char* data,
                     unsigned int width, unsigned int height,
                     int bitmap_pad, int bytes_per_line) {
    return XCreateImage(display, visual, depth, format, offset, data,
                        width, height, bitmap_pad, bytes_per_line);
}

// Destroy XImage without freeing Go-managed data
void destroyXImage(XImage* image) {
    if (image) {
        image->data = NULL;
        XDestroyImage(image);
    }
}

// Create a blank (invisible) cursor
Cursor createBlankCursor(Display* display, Window window) {
    Pixmap pixmap;
    XColor color;
    Cursor cursor;
    char data[1] = {0};

    memset(&color, 0, sizeof(color));
    pixmap = XCreateBitmapFromData(display, window, data, 1, 1);
    cursor = XCreatePixmapCursor(display, pixmap, pixmap, &color, &color, 0, 0);
    XFreePixmap(display, pixmap);
    return cursor;
}

// Toggle fullscreen via EWMH _NET_WM_STATE_FULLSCREEN
void toggleFullscreen(Display* display, Window window, int fullscreen) {
    XEvent event;
    Atom wmState = XInternAtom(display, "_NET_WM_STATE", False);
    Atom wmFullscreen = XInternAtom(display, "_NET_WM_STATE_FULLSCREEN", False);

    memset(&event, 0, sizeof(event));
    event.type = ClientMessage;
    event.xclient.window = window;
    event.xclient.message_type = wmState;
    event.xclient.format = 32;
    event.xclient.data.l[0] = fullscreen ? 1 : 0;
    event.xclient.data.l[1] = wmFullscreen;
    event.xclient.data.l[2] = 0;

    XSendEvent(display, DefaultRootWindow(display), False,
               SubstructureRedirectMask | SubstructureNotifyMask, &event);
    XFlush(display);
}

// Set window icon via _NET_WM_ICON
void setWindowIcon(Display* display, Window window, unsigned long* data, int length) {
    Atom netWmIcon = XInternAtom(display, "_NET_WM_ICON", False);
    Atom cardinal = XInternAtom(display, "CARDINAL", False);
    XChangeProperty(display, window, netWmIcon, cardinal, 32,
                    PropModeReplace, (unsigned char*)data, length);
    XFlush(display);
}
*/
import "C"

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	agg "agg_go"
)

func init() {
	runtime.LockOSThread()
}

var fontCharW, fontCharH int

type cachedImage struct {
	aggImg *agg.Image
	width  int
	height int
}

type window struct {
	// X11
	display      *C.Display
	x11Window    C.Window
	gc           C.GC
	screen       C.int
	depth        C.int
	visual       *C.Visual
	wmDeleteAtom C.Atom
	ximg         *C.XImage
	imgData      []byte
	blankCursor  C.Cursor

	// Rendering
	aggCtx    *agg.Context
	aggImg    *agg.Image
	buffer    []uint8
	bufWidth  int
	bufHeight int

	// Image cache
	images map[string]*cachedImage

	// Font
	fontImg *image.NRGBA

	// Input state
	running   bool
	pressed   []Key
	typed     []rune
	keyDown   [keyCount]bool
	mouseDown [mouseButtonCount]bool
	clicks    []MouseClick
	mouseX    int
	mouseY    int
	wheelX    float64
	wheelY    float64

	// Window state
	width          int
	height         int
	originalWidth  int
	originalHeight int
	fullscreen     bool
	blurImages     bool
	iconPath       string
	showingCursor  bool
}

// RunWindow creates a new window and calls update 60 times per second.
func RunWindow(title string, width, height int, update UpdateFunction) error {
	if err := initSound(); err != nil {
		return err
	}
	defer closeSound()

	display := C.XOpenDisplay(nil)
	if display == nil {
		return errorString("cannot open X11 display")
	}

	screen := C.XDefaultScreen(display)
	depth := C.XDefaultDepth(display, screen)
	visual := C.XDefaultVisual(display, screen)

	rootWindow := C.XDefaultRootWindow(display)
	x11Window := C.XCreateSimpleWindow(
		display, rootWindow,
		0, 0, C.uint(width), C.uint(height),
		0,
		C.XBlackPixel(display, screen),
		C.XBlackPixel(display, screen),
	)

	cTitle := C.CString(title)
	C.XStoreName(display, x11Window, cTitle)
	C.free(unsafe.Pointer(cTitle))

	// Make non-resizable.
	var hints C.XSizeHints
	hints.flags = C.PMinSize | C.PMaxSize
	hints.min_width = C.int(width)
	hints.min_height = C.int(height)
	hints.max_width = C.int(width)
	hints.max_height = C.int(height)
	C.XSetWMNormalHints(display, x11Window, &hints)

	// Center on screen.
	screenWidth := int(C.XDisplayWidth(display, screen))
	screenHeight := int(C.XDisplayHeight(display, screen))
	C.XMoveWindow(display, x11Window,
		C.int((screenWidth-width)/2),
		C.int((screenHeight-height)/2))

	gc := C.XCreateGC(display, x11Window, 0, nil)

	cWmDelete := C.CString("WM_DELETE_WINDOW")
	wmDeleteAtom := C.XInternAtom(display, cWmDelete, C.False)
	C.free(unsafe.Pointer(cWmDelete))
	C.XSetWMProtocols(display, x11Window, &wmDeleteAtom, 1)

	eventMask := C.long(C.ExposureMask | C.KeyPressMask | C.KeyReleaseMask |
		C.ButtonPressMask | C.ButtonReleaseMask | C.PointerMotionMask |
		C.StructureNotifyMask)
	C.XSelectInput(display, x11Window, eventMask)

	C.XMapWindow(display, x11Window)
	C.XFlush(display)

	// Create RGBA rendering buffer backed by agg.Context.
	buf := make([]uint8, width*height*4)
	aggImg := agg.NewImage(buf, width, height, width*4)
	aggCtx := agg.NewContextForImage(aggImg)

	// Separate BGRA buffer for XPutImage.
	imgData := make([]byte, width*height*4)
	ximg := C.createXImage(display, visual, C.uint(depth),
		C.ZPixmap, 0,
		(*C.char)(unsafe.Pointer(&imgData[0])),
		C.uint(width), C.uint(height),
		32, C.int(width*4))

	blankCursor := C.createBlankCursor(display, x11Window)

	// Load bitmap font.
	fontStdImg, _, err := image.Decode(bytes.NewReader(bitmapFontWhitePng))
	if err != nil {
		C.XCloseDisplay(display)
		return err
	}
	var fontNRGBA *image.NRGBA
	if v, ok := fontStdImg.(*image.NRGBA); ok {
		fontNRGBA = v
	} else {
		bounds := fontStdImg.Bounds()
		fontNRGBA = image.NewNRGBA(bounds)
		for fy := bounds.Min.Y; fy < bounds.Max.Y; fy++ {
			for fx := bounds.Min.X; fx < bounds.Max.X; fx++ {
				fontNRGBA.Set(fx, fy, fontStdImg.At(fx, fy))
			}
		}
	}
	fontCharW = fontNRGBA.Bounds().Dx() / 16
	fontCharH = fontNRGBA.Bounds().Dy() / 16

	w := &window{
		display:        display,
		x11Window:      x11Window,
		gc:             gc,
		screen:         screen,
		depth:          depth,
		visual:         visual,
		wmDeleteAtom:   wmDeleteAtom,
		ximg:           ximg,
		imgData:        imgData,
		blankCursor:    blankCursor,
		aggCtx:         aggCtx,
		aggImg:         aggImg,
		buffer:         buf,
		bufWidth:       width,
		bufHeight:      height,
		images:         make(map[string]*cachedImage),
		fontImg:        fontNRGBA,
		running:        true,
		width:          width,
		height:         height,
		originalWidth:  width,
		originalHeight: height,
		showingCursor:  true,
	}

	defer func() {
		w.ShowCursor(true)
		w.cleanUp()
		C.XCloseDisplay(display)
	}()

	lastUpdateTime := time.Now().Add(-time.Hour)
	const updateInterval = 1.0 / 60.0

	for w.running {
		for C.XPending(display) > 0 {
			var event C.XEvent
			C.XNextEvent(display, &event)
			w.handleEvent(&event)
			if !w.running {
				break
			}
		}
		if !w.running {
			break
		}

		now := time.Now()
		if now.Sub(lastUpdateTime).Seconds() > updateInterval {
			// Clear buffer to black.
			for i := 0; i < len(w.buffer); i += 4 {
				w.buffer[i] = 0
				w.buffer[i+1] = 0
				w.buffer[i+2] = 0
				w.buffer[i+3] = 255
			}

			update(w)

			w.pressed = w.pressed[:0]
			w.typed = w.typed[:0]
			w.clicks = w.clicks[:0]
			w.wheelX = 0
			w.wheelY = 0

			w.blitToX11()
			lastUpdateTime = now
		} else {
			time.Sleep(time.Millisecond)
		}
	}

	return nil
}

func (w *window) cleanUp() {
	if w.ximg != nil {
		C.destroyXImage(w.ximg)
		w.ximg = nil
	}
	if w.blankCursor != 0 {
		C.XFreeCursor(w.display, w.blankCursor)
	}
	if w.gc != nil {
		C.XFreeGC(w.display, w.gc)
	}
	C.XDestroyWindow(w.display, w.x11Window)
	w.images = nil
}

func (w *window) Close() {
	w.running = false
}

func (w *window) SetIcon(path string) error {
	if w.iconPath == path {
		return nil
	}

	f, err := OpenFile(path)
	if err != nil {
		return err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}

	bounds := img.Bounds()
	imgW, imgH := bounds.Dx(), bounds.Dy()

	// _NET_WM_ICON format: [width, height, ARGB pixels...]
	data := make([]C.ulong, 2+imgW*imgH)
	data[0] = C.ulong(imgW)
	data[1] = C.ulong(imgH)
	for y := 0; y < imgH; y++ {
		for x := 0; x < imgW; x++ {
			r, g, b, a := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			pixel := (uint32(a>>8) << 24) | (uint32(r>>8) << 16) | (uint32(g>>8) << 8) | uint32(b>>8)
			data[2+y*imgW+x] = C.ulong(pixel)
		}
	}

	C.setWindowIcon(w.display, w.x11Window, &data[0], C.int(len(data)))
	w.iconPath = path
	return nil
}

func (w *window) Size() (int, int) {
	return w.width, w.height
}

func (w *window) SetFullscreen(f bool) {
	if f == w.fullscreen {
		return
	}
	w.fullscreen = f
	if f {
		C.toggleFullscreen(w.display, w.x11Window, 1)
	} else {
		C.toggleFullscreen(w.display, w.x11Window, 0)
	}
}

func (w *window) IsFullscreen() bool {
	return w.fullscreen
}

func (w *window) ShowCursor(show bool) {
	if w.showingCursor == show {
		return
	}
	if show {
		C.XUndefineCursor(w.display, w.x11Window)
	} else {
		C.XDefineCursor(w.display, w.x11Window, w.blankCursor)
	}
	w.showingCursor = show
}

// --- Input ---

func (w *window) WasKeyPressed(key Key) bool {
	for _, pressed := range w.pressed {
		if pressed == key {
			return true
		}
	}
	return false
}

func (w *window) IsKeyDown(key Key) bool {
	if key <= 0 || int(key) >= len(w.keyDown) {
		return false
	}
	return w.keyDown[key]
}

func (w *window) Characters() string {
	return string(w.typed)
}

func (w *window) IsMouseDown(button MouseButton) bool {
	if button < 0 || int(button) >= len(w.mouseDown) {
		return false
	}
	return w.mouseDown[button]
}

func (w *window) Clicks() []MouseClick {
	return w.clicks
}

func (w *window) MousePosition() (int, int) {
	return w.mouseX, w.mouseY
}

func (w *window) MouseWheelY() float64 {
	return w.wheelY
}

func (w *window) MouseWheelX() float64 {
	return w.wheelX
}

// --- Drawing primitives ---

func (w *window) setPixel(x, y int, r, g, b, a uint8) {
	if x < 0 || y < 0 || x >= w.bufWidth || y >= w.bufHeight {
		return
	}
	i := (y*w.bufWidth + x) * 4
	if a == 255 {
		w.buffer[i] = r
		w.buffer[i+1] = g
		w.buffer[i+2] = b
		w.buffer[i+3] = 255
	} else if a > 0 {
		srcA := uint32(a)
		invA := 255 - srcA
		w.buffer[i] = uint8((uint32(r)*srcA + uint32(w.buffer[i])*invA) / 255)
		w.buffer[i+1] = uint8((uint32(g)*srcA + uint32(w.buffer[i+1])*invA) / 255)
		w.buffer[i+2] = uint8((uint32(b)*srcA + uint32(w.buffer[i+2])*invA) / 255)
		w.buffer[i+3] = uint8(srcA + uint32(w.buffer[i+3])*invA/255)
	}
}

func (w *window) DrawPoint(x, y int, color Color) {
	r, g, b, a := colorToUint8(color)
	w.setPixel(x, y, r, g, b, a)
}

func (w *window) DrawLine(fromX, fromY, toX, toY int, color Color) {
	if fromX == toX && fromY == toY {
		w.DrawPoint(fromX, fromY, color)
		return
	}

	r, g, b, a := colorToUint8(color)

	dx := toX - fromX
	dy := toY - fromY
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	sx := 1
	if fromX > toX {
		sx = -1
	}
	sy := 1
	if fromY > toY {
		sy = -1
	}
	err := dx - dy

	x, y := fromX, fromY
	for {
		w.setPixel(x, y, r, g, b, a)
		if x == toX && y == toY {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x += sx
		}
		if e2 < dx {
			err += dx
			y += sy
		}
	}
}

func (w *window) DrawRect(x, y, width, height int, color Color) {
	if width <= 0 || height <= 0 {
		return
	}
	if width == 1 && height == 1 {
		w.DrawPoint(x, y, color)
		return
	}
	if width == 1 {
		w.DrawLine(x, y, x, y+height-1, color)
		return
	}
	if height == 1 {
		w.DrawLine(x, y, x+width-1, y, color)
		return
	}
	w.DrawLine(x, y, x+width-1, y, color)
	w.DrawLine(x+width-1, y, x+width-1, y+height-1, color)
	w.DrawLine(x+width-1, y+height-1, x, y+height-1, color)
	w.DrawLine(x, y+height-1, x, y, color)
}

func (w *window) FillRect(x, y, width, height int, color Color) {
	if width <= 0 || height <= 0 {
		return
	}
	r, g, b, a := colorToUint8(color)
	for row := y; row < y+height; row++ {
		for col := x; col < x+width; col++ {
			w.setPixel(col, row, r, g, b, a)
		}
	}
}

func (w *window) DrawEllipse(x, y, width, height int, color Color) {
	outline := ellipseOutline(x, y, width, height)
	if len(outline) == 0 {
		return
	}
	r, g, b, a := colorToUint8(color)
	for _, p := range outline {
		w.setPixel(p.x, p.y, r, g, b, a)
	}
}

func (w *window) FillEllipse(x, y, width, height int, color Color) {
	area := ellipseArea(x, y, width, height)
	if len(area) == 0 {
		return
	}
	r, g, b, a := colorToUint8(color)
	for i := 0; i < len(area); i += 2 {
		for col := area[i].x; col <= area[i+1].x; col++ {
			w.setPixel(col, area[i].y, r, g, b, a)
		}
	}
}

// --- Images ---

func (w *window) getOrLoadImage(path string) (*cachedImage, error) {
	if img, ok := w.images[path]; ok {
		return img, nil
	}

	f, err := OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stdImg, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}

	aggImg, err := agg.NewImageFromStandardImage(stdImg)
	if err != nil {
		return nil, err
	}

	ci := &cachedImage{
		aggImg: aggImg,
		width:  stdImg.Bounds().Dx(),
		height: stdImg.Bounds().Dy(),
	}
	w.images[path] = ci
	return ci, nil
}

func (w *window) ImageSize(path string) (width, height int, err error) {
	img, err := w.getOrLoadImage(path)
	if err != nil {
		return 0, 0, err
	}
	return img.width, img.height, nil
}

func (w *window) DrawImageFile(path string, x, y int) error {
	img, err := w.getOrLoadImage(path)
	if err != nil {
		return err
	}
	w.setImageFilter()
	return w.aggCtx.DrawImage(img.aggImg, float64(x), float64(y))
}

func (w *window) DrawImageFileRotated(path string, x, y, degrees int) error {
	return w.DrawImageFileTo(path, x, y, -1, -1, degrees)
}

func (w *window) DrawImageFileTo(path string, x, y, width, height, degrees int) error {
	img, err := w.getOrLoadImage(path)
	if err != nil {
		return err
	}

	if width == -1 && height == -1 {
		width, height = img.width, img.height
	}
	if width <= 0 || height <= 0 {
		return nil
	}

	w.setImageFilter()

	if degrees == 0 {
		return w.aggCtx.DrawImageScaled(img.aggImg, float64(x), float64(y), float64(width), float64(height))
	}

	cx := float64(x) + float64(width)/2
	cy := float64(y) + float64(height)/2

	w.aggCtx.PushTransform()
	w.aggCtx.Translate(cx, cy)
	w.aggCtx.RotateDegrees(float64(degrees))
	w.aggCtx.Translate(-cx, -cy)

	err = w.aggCtx.DrawImageScaled(img.aggImg, float64(x), float64(y), float64(width), float64(height))
	w.aggCtx.PopTransform()
	return err
}

func (w *window) DrawImageFilePart(
	path string,
	sourceX, sourceY, sourceWidth, sourceHeight int,
	destX, destY, destWidth, destHeight int,
	rotationCWDeg int,
) error {
	img, err := w.getOrLoadImage(path)
	if err != nil {
		return err
	}

	// Handle flipping via negative source dimensions.
	flipX := sourceWidth < 0
	flipY := sourceHeight < 0
	if flipX {
		sourceX += sourceWidth
		sourceWidth = -sourceWidth
	}
	if flipY {
		sourceY += sourceHeight
		sourceHeight = -sourceHeight
	}

	if destWidth <= 0 || destHeight <= 0 || sourceWidth <= 0 || sourceHeight <= 0 {
		return nil
	}

	w.setImageFilter()

	needsTransform := rotationCWDeg != 0 || flipX || flipY
	if needsTransform {
		cx := float64(destX) + float64(destWidth)/2
		cy := float64(destY) + float64(destHeight)/2

		w.aggCtx.PushTransform()
		w.aggCtx.Translate(cx, cy)
		if rotationCWDeg != 0 {
			w.aggCtx.RotateDegrees(float64(rotationCWDeg))
		}
		if flipX {
			w.aggCtx.Scale(-1, 1)
		}
		if flipY {
			w.aggCtx.Scale(1, -1)
		}
		w.aggCtx.Translate(-cx, -cy)
	}

	err = w.aggCtx.DrawImageRegion(img.aggImg,
		sourceX, sourceY, sourceWidth, sourceHeight,
		float64(destX), float64(destY), float64(destWidth), float64(destHeight))

	if needsTransform {
		w.aggCtx.PopTransform()
	}
	return err
}

func (w *window) setImageFilter() {
	if w.blurImages {
		w.aggCtx.SetImageFilter(agg.ImageFilterBilinear)
	} else {
		w.aggCtx.SetImageFilter(agg.ImageFilterNoFilter)
	}
}

func (w *window) BlurImages(blur bool) {
	w.blurImages = blur
}

// --- Text ---

func (w *window) GetTextSize(text string) (width, height int) {
	return w.GetScaledTextSize(text, 1.0)
}

func (w *window) GetScaledTextSize(text string, scale float32) (width, height int) {
	scale *= fontBaseScale
	lines := strings.Split(text, "\n")
	maxLineW := 0
	for _, line := range lines {
		lineW := utf8.RuneCountInString(line)
		if lineW > maxLineW {
			maxLineW = lineW
		}
	}

	charW := fontCharW - 2*fontGlyphMargin
	charH := fontCharH - 2*fontGlyphMargin
	width = int(float32(charW*maxLineW)*scale*fontKerningFactor + 0.5)
	height = int(float32(charH*len(lines))*scale + 0.5)
	return width, height
}

func (w *window) DrawText(text string, x, y int, color Color) {
	w.DrawScaledText(text, x, y, 1, color)
}

func (w *window) DrawScaledText(text string, x, y int, scale float32, color Color) {
	if len(text) == 0 || scale <= 0 {
		return
	}

	scale *= fontBaseScale

	charW := fontCharW - 2*fontGlyphMargin
	charH := fontCharH - 2*fontGlyphMargin

	glyphDestW := float32(charW) * scale * fontKerningFactor
	glyphDestH := float32(charH) * scale

	cr, cg, cb, ca := colorToUint8(color)

	destX := float32(x)
	destY := float32(y)

	for _, r := range text {
		if r == '\n' {
			destX = float32(x)
			destY += glyphDestH
			continue
		}

		index := runeToFont(r)
		srcX := int(index%16)*fontCharW + fontGlyphMargin
		srcY := int(index/16)*fontCharH + fontGlyphMargin

		w.drawGlyph(srcX, srcY, charW, charH,
			destX, destY, glyphDestW, glyphDestH,
			cr, cg, cb, ca)

		destX += glyphDestW
	}
}

// drawGlyph renders a single font glyph with color tinting and scaling.
// The font image is white-on-transparent, so we use the alpha channel as
// the glyph shape and multiply by the desired color.
func (w *window) drawGlyph(srcX, srcY, srcW, srcH int,
	destX, destY, destW, destH float32,
	cr, cg, cb, ca uint8) {

	if destW <= 0 || destH <= 0 {
		return
	}

	dx0 := int(destX)
	dy0 := int(destY)
	dx1 := int(destX + destW)
	dy1 := int(destY + destH)

	for dy := dy0; dy < dy1; dy++ {
		sy := srcY + int(float32(dy-dy0)/destH*float32(srcH))
		if sy < srcY || sy >= srcY+srcH {
			continue
		}

		for dx := dx0; dx < dx1; dx++ {
			sx := srcX + int(float32(dx-dx0)/destW*float32(srcW))
			if sx < srcX || sx >= srcX+srcW {
				continue
			}

			fi := w.fontImg.PixOffset(sx, sy)
			fa := w.fontImg.Pix[fi+3]
			if fa == 0 {
				continue
			}

			a := uint8(uint32(fa) * uint32(ca) / 255)
			if a == 0 {
				continue
			}

			w.setPixel(dx, dy, cr, cg, cb, a)
		}
	}
}

// --- Sound ---

func (w *window) PlaySoundFile(path string) error {
	return playSoundFile(path)
}

// --- X11 event handling ---

func (w *window) handleEvent(event *C.XEvent) {
	eventType := (*C.XAnyEvent)(unsafe.Pointer(event))._type

	switch eventType {
	case C.KeyPress:
		w.handleKeyPress(event)
	case C.KeyRelease:
		w.handleKeyRelease(event)
	case C.ButtonPress:
		w.handleButtonPress(event)
	case C.ButtonRelease:
		w.handleButtonRelease(event)
	case C.MotionNotify:
		w.handleMotionNotify(event)
	case C.ClientMessage:
		w.handleClientMessage(event)
	case C.ConfigureNotify:
		w.handleConfigureNotify(event)
	}
}

func (w *window) handleKeyPress(event *C.XEvent) {
	keyEvent := (*C.XKeyEvent)(unsafe.Pointer(event))

	keySym := C.XLookupKeysym(keyEvent, 0)
	key := x11KeyToPrototype(keySym)
	if key != 0 {
		w.pressed = append(w.pressed, key)
		if int(key) < len(w.keyDown) {
			w.keyDown[key] = true
		}
	}

	// Character input.
	var buf [32]C.char
	var dummy C.KeySym
	n := C.XLookupString(keyEvent, &buf[0], 32, &dummy, nil)
	if n > 0 {
		s := C.GoStringN(&buf[0], n)
		for _, r := range s {
			if r >= 32 {
				w.typed = append(w.typed, r)
			}
		}
	}
}

func (w *window) handleKeyRelease(event *C.XEvent) {
	keyEvent := (*C.XKeyEvent)(unsafe.Pointer(event))

	keySym := C.XLookupKeysym(keyEvent, 0)
	key := x11KeyToPrototype(keySym)
	if key != 0 && int(key) < len(w.keyDown) {
		w.keyDown[key] = false
	}
}

func (w *window) handleButtonPress(event *C.XEvent) {
	buttonEvent := (*C.XButtonEvent)(unsafe.Pointer(event))

	switch buttonEvent.button {
	case C.Button1:
		w.mouseDown[LeftButton] = true
		w.clicks = append(w.clicks, MouseClick{
			X: int(buttonEvent.x), Y: int(buttonEvent.y), Button: LeftButton,
		})
	case C.Button2:
		w.mouseDown[MiddleButton] = true
		w.clicks = append(w.clicks, MouseClick{
			X: int(buttonEvent.x), Y: int(buttonEvent.y), Button: MiddleButton,
		})
	case C.Button3:
		w.mouseDown[RightButton] = true
		w.clicks = append(w.clicks, MouseClick{
			X: int(buttonEvent.x), Y: int(buttonEvent.y), Button: RightButton,
		})
	case 4: // Scroll up
		w.wheelY += 1
	case 5: // Scroll down
		w.wheelY -= 1
	case 6: // Scroll left
		w.wheelX -= 1
	case 7: // Scroll right
		w.wheelX += 1
	}
}

func (w *window) handleButtonRelease(event *C.XEvent) {
	buttonEvent := (*C.XButtonEvent)(unsafe.Pointer(event))

	switch buttonEvent.button {
	case C.Button1:
		w.mouseDown[LeftButton] = false
	case C.Button2:
		w.mouseDown[MiddleButton] = false
	case C.Button3:
		w.mouseDown[RightButton] = false
	}
}

func (w *window) handleMotionNotify(event *C.XEvent) {
	motionEvent := (*C.XMotionEvent)(unsafe.Pointer(event))
	w.mouseX = int(motionEvent.x)
	w.mouseY = int(motionEvent.y)
}

func (w *window) handleClientMessage(event *C.XEvent) {
	clientEvent := (*C.XClientMessageEvent)(unsafe.Pointer(event))
	dataPtr := (*C.long)(unsafe.Pointer(&clientEvent.data[0]))
	if C.Atom(*dataPtr) == w.wmDeleteAtom {
		w.running = false
	}
}

func (w *window) handleConfigureNotify(event *C.XEvent) {
	configEvent := (*C.XConfigureEvent)(unsafe.Pointer(event))
	newWidth := int(configEvent.width)
	newHeight := int(configEvent.height)

	if newWidth != w.width || newHeight != w.height {
		w.width = newWidth
		w.height = newHeight
		w.recreateBuffers(newWidth, newHeight)
	}
}

func (w *window) recreateBuffers(width, height int) {
	w.bufWidth = width
	w.bufHeight = height
	w.buffer = make([]uint8, width*height*4)
	w.aggImg = agg.NewImage(w.buffer, width, height, width*4)
	w.aggCtx = agg.NewContextForImage(w.aggImg)

	if w.ximg != nil {
		C.destroyXImage(w.ximg)
	}
	w.imgData = make([]byte, width*height*4)
	w.ximg = C.createXImage(w.display, w.visual, C.uint(w.depth),
		C.ZPixmap, 0,
		(*C.char)(unsafe.Pointer(&w.imgData[0])),
		C.uint(width), C.uint(height),
		32, C.int(width*4))
}

// --- Buffer blit ---

func (w *window) blitToX11() {
	n := w.bufWidth * w.bufHeight * 4
	for i := 0; i < n; i += 4 {
		w.imgData[i+0] = w.buffer[i+2] // B
		w.imgData[i+1] = w.buffer[i+1] // G
		w.imgData[i+2] = w.buffer[i+0] // R
		w.imgData[i+3] = w.buffer[i+3] // A
	}
	C.XPutImage(w.display, w.x11Window, w.gc, w.ximg,
		0, 0, 0, 0, C.uint(w.bufWidth), C.uint(w.bufHeight))
	C.XFlush(w.display)
}

// --- X11 key mapping ---

func x11KeyToPrototype(keySym C.KeySym) Key {
	switch {
	case keySym >= 'a' && keySym <= 'z':
		return KeyA + Key(keySym-'a')
	case keySym >= 'A' && keySym <= 'Z':
		return KeyA + Key(keySym-'A')
	case keySym >= '0' && keySym <= '9':
		return Key0 + Key(keySym-'0')
	}

	switch keySym {
	case C.XK_F1:
		return KeyF1
	case C.XK_F2:
		return KeyF2
	case C.XK_F3:
		return KeyF3
	case C.XK_F4:
		return KeyF4
	case C.XK_F5:
		return KeyF5
	case C.XK_F6:
		return KeyF6
	case C.XK_F7:
		return KeyF7
	case C.XK_F8:
		return KeyF8
	case C.XK_F9:
		return KeyF9
	case C.XK_F10:
		return KeyF10
	case C.XK_F11:
		return KeyF11
	case C.XK_F12:
		return KeyF12
	case C.XK_F13:
		return KeyF13
	case C.XK_F14:
		return KeyF14
	case C.XK_F15:
		return KeyF15
	case C.XK_F16:
		return KeyF16
	case C.XK_F17:
		return KeyF17
	case C.XK_F18:
		return KeyF18
	case C.XK_F19:
		return KeyF19
	case C.XK_F20:
		return KeyF20
	case C.XK_F21:
		return KeyF21
	case C.XK_F22:
		return KeyF22
	case C.XK_F23:
		return KeyF23
	case C.XK_F24:
		return KeyF24
	case C.XK_KP_0, C.XK_KP_Insert:
		return KeyNum0
	case C.XK_KP_1, C.XK_KP_End:
		return KeyNum1
	case C.XK_KP_2, C.XK_KP_Down:
		return KeyNum2
	case C.XK_KP_3, C.XK_KP_Page_Down:
		return KeyNum3
	case C.XK_KP_4, C.XK_KP_Left:
		return KeyNum4
	case C.XK_KP_5, C.XK_KP_Begin:
		return KeyNum5
	case C.XK_KP_6, C.XK_KP_Right:
		return KeyNum6
	case C.XK_KP_7, C.XK_KP_Home:
		return KeyNum7
	case C.XK_KP_8, C.XK_KP_Up:
		return KeyNum8
	case C.XK_KP_9, C.XK_KP_Page_Up:
		return KeyNum9
	case C.XK_Return:
		return KeyEnter
	case C.XK_KP_Enter:
		return KeyNumEnter
	case C.XK_Control_L:
		return KeyLeftControl
	case C.XK_Control_R:
		return KeyRightControl
	case C.XK_Shift_L:
		return KeyLeftShift
	case C.XK_Shift_R:
		return KeyRightShift
	case C.XK_Alt_L:
		return KeyLeftAlt
	case C.XK_Alt_R:
		return KeyRightAlt
	case C.XK_Left:
		return KeyLeft
	case C.XK_Right:
		return KeyRight
	case C.XK_Up:
		return KeyUp
	case C.XK_Down:
		return KeyDown
	case C.XK_Escape:
		return KeyEscape
	case C.XK_space:
		return KeySpace
	case C.XK_BackSpace:
		return KeyBackspace
	case C.XK_Tab:
		return KeyTab
	case C.XK_Home:
		return KeyHome
	case C.XK_End:
		return KeyEnd
	case C.XK_Page_Down:
		return KeyPageDown
	case C.XK_Page_Up:
		return KeyPageUp
	case C.XK_Delete:
		return KeyDelete
	case C.XK_Insert:
		return KeyInsert
	case C.XK_KP_Add:
		return KeyNumAdd
	case C.XK_KP_Subtract:
		return KeyNumSubtract
	case C.XK_KP_Multiply:
		return KeyNumMultiply
	case C.XK_KP_Divide:
		return KeyNumDivide
	case C.XK_Caps_Lock:
		return KeyCapslock
	case C.XK_Print:
		return KeyPrint
	case C.XK_Pause:
		return KeyPause
	}

	return 0
}

// --- Helpers ---

func colorToUint8(c Color) (r, g, b, a uint8) {
	return uint8(c.R * 255), uint8(c.G * 255), uint8(c.B * 255), uint8(c.A * 255)
}
