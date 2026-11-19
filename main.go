package main

import (
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/ui"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

//go:embed icon.png
var appIconPNG []byte

// Tab identifiers
const (
	TabBatchSlice = 0
	TabManualROI  = 1
	TabStitching  = 2
)

// CutterApp represents the GUI state of ImageTileCutter.
type CutterApp struct {
	win      *mygo.Window
	tray     *mygo.Tray
	quitting bool

	// Active navigation tab
	activeTab int

	// Directory paths
	inputDir     string
	outputDir    string
	maskDir      string
	manifestPath string

	// Image files state
	imageFiles     []string
	currentFileIdx int
	currentImage   image.Image
	previewBitmap  *ui.Bitmap

	// Batch slicing configuration
	sliceModeStr     string
	tileWidthStr     string
	tileHeightStr    string
	overlapXStr      string
	overlapYStr      string
	gridRowsStr      string
	gridColsStr      string
	edgeModeStr      string
	filterBlank      bool
	blankThreshold   float64
	filterVariance   bool
	minVariance      float64
	outputFormatStr  string
	jpegQuality      float64

	// Manual ROI cropping state
	roiActive   bool
	roiMinX     int
	roiMinY     int
	roiMaxX     int
	roiMaxY     int
	roiXStr     string
	roiYStr     string
	roiWStr     string
	roiHStr     string
	isDragging  bool
	dragStartX  float32
	dragStartY  float32

	// Viewport & zoom state
	zoomScale         float64
	zoomStr           string
	showGridOverlay   bool
	placeholderBitmap *ui.Bitmap

	// Background worker & progress state
	mu              sync.Mutex
	cancelFunc      context.CancelFunc
	progress        ProgressStats
	statusMessage   string
	errorMessage    string
}

func (app *CutterApp) buildTrayMenu() *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{
			Label: "显示主窗口",
			Click: func(*mygo.MenuItem, *mygo.Window) {
				app.win.Show()
				app.win.Focus()
			},
		},
		{
			Label: "隐藏主窗口",
			Click: func(*mygo.MenuItem, *mygo.Window) {
				app.win.Hide()
			},
		},
		mygo.Separator(),
		{
			Label: "选择原图目录...",
			Click: func(*mygo.MenuItem, *mygo.Window) {
				app.win.Show()
				app.win.Focus()
				app.selectInputDir()
			},
		},
		{
			Label: "选择输出目录...",
			Click: func(*mygo.MenuItem, *mygo.Window) {
				app.win.Show()
				app.win.Focus()
				app.selectOutputDir()
			},
		},
		mygo.Separator(),
		{
			Label: "检查更新...",
			Click: func(*mygo.MenuItem, *mygo.Window) {
				updater.CheckForUpdates()
			},
		},
		mygo.Separator(),
		{
			Label: "退出 ImageTileCutter",
			Click: func(*mygo.MenuItem, *mygo.Window) {
				app.quitting = true
				mygo.App.Quit()
			},
		},
	})
}

func (app *CutterApp) setupTray() {
	t, err := mygo.NewTray(mygo.TrayOptions{
		ID:      "image-tile-cutter-tray",
		Icon:    appIconPNG,
		ToolTip: "ImageTileCutter · 工业级大图切片裁剪工具",
		Menu:    app.buildTrayMenu(),
	})
	if err != nil {
		panic(err)
	}
	app.tray = t
}

func (app *CutterApp) selectInputDir() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:     "选择大图所在文件夹",
		Directory: true,
	})
	if err != nil {
		panic(err)
	}
	if len(paths) == 0 {
		return
	}
	app.loadDirectory(paths[0])
}

func (app *CutterApp) selectOutputDir() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:     "选择切片输出保存文件夹",
		Directory: true,
	})
	if err != nil {
		panic(err)
	}
	if len(paths) == 0 {
		return
	}
	app.outputDir = paths[0]
}

func (app *CutterApp) selectMaskDir() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:     "选择成对掩码图像文件夹 (可选)",
		Directory: true,
	})
	if err != nil {
		panic(err)
	}
	if len(paths) == 0 {
		return
	}
	app.maskDir = paths[0]
}

func (app *CutterApp) selectManifestFile() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title: "选择 manifest.json 元数据清单文件",
		Filters: []mygo.FileFilter{
			{Name: "Manifest JSON", Extensions: []string{"json"}},
		},
	})
	if err != nil {
		panic(err)
	}
	if len(paths) == 0 {
		return
	}
	app.manifestPath = paths[0]
}

func (app *CutterApp) loadDirectory(dirPath string) {
	app.inputDir = dirPath
	if app.outputDir == "" {
		app.outputDir = filepath.Join(dirPath, "tiles_output")
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		app.statusMessage = fmt.Sprintf("读取目录失败: %v", err)
		return
	}

	validExts := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true,
		".tif": true, ".tiff": true, ".bmp": true, ".webp": true,
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if validExts[ext] {
			files = append(files, filepath.Join(dirPath, entry.Name()))
		}
	}

	app.imageFiles = files
	app.currentFileIdx = 0
	if len(files) > 0 {
		app.loadImage(files[0])
		app.statusMessage = fmt.Sprintf("已加载目录，发现 %d 张待处理大图", len(files))
	} else {
		app.currentImage = nil
		app.previewBitmap = nil
		app.statusMessage = "当前目录下未发现支持的图片格式"
	}
}

func (app *CutterApp) loadImage(path string) {
	f, err := os.Open(path)
	if err != nil {
		app.statusMessage = fmt.Sprintf("打开图片失败: %v", err)
		return
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		app.statusMessage = fmt.Sprintf("解码图片失败: %v", err)
		return
	}

	app.currentImage = img
	app.roiActive = false
	app.refreshPreview()
}

func (app *CutterApp) nextImage() {
	if len(app.imageFiles) == 0 {
		return
	}
	app.currentFileIdx = (app.currentFileIdx + 1) % len(app.imageFiles)
	app.loadImage(app.imageFiles[app.currentFileIdx])
}

func (app *CutterApp) prevImage() {
	if len(app.imageFiles) == 0 {
		return
	}
	app.currentFileIdx = (app.currentFileIdx - 1 + len(app.imageFiles)) % len(app.imageFiles)
	app.loadImage(app.imageFiles[app.currentFileIdx])
}

func (app *CutterApp) parseConfig() (TileConfig, error) {
	tw, _ := strconv.Atoi(app.tileWidthStr)
	if tw <= 0 {
		tw = 512
	}
	th, _ := strconv.Atoi(app.tileHeightStr)
	if th <= 0 {
		th = 512
	}
	ox, _ := strconv.Atoi(app.overlapXStr)
	oy, _ := strconv.Atoi(app.overlapYStr)
	rows, _ := strconv.Atoi(app.gridRowsStr)
	if rows <= 0 {
		rows = 4
	}
	cols, _ := strconv.Atoi(app.gridColsStr)
	if cols <= 0 {
		cols = 4
	}

	mode := ModeSlidingWindow
	if app.sliceModeStr == "等分网格切分" {
		mode = ModeGrid
	} else if app.activeTab == TabManualROI {
		mode = ModeManualROI
	}

	edge := EdgeShiftBack
	switch app.edgeModeStr {
	case "填充补全 (固定尺寸)":
		edge = EdgePad
	case "丢弃边缘 (忽略余量)":
		edge = EdgeDiscard
	case "保留实际 (不规则尺寸)":
		edge = EdgeKeep
	default:
		edge = EdgeShiftBack
	}

	var roi image.Rectangle
	if app.roiActive {
		roi = image.Rect(app.roiMinX, app.roiMinY, app.roiMaxX, app.roiMaxY)
	}

	outFmt := strings.ToLower(app.outputFormatStr)
	if outFmt != "jpg" && outFmt != "jpeg" {
		outFmt = "png"
	}

	return TileConfig{
		Mode:           mode,
		TileWidth:      tw,
		TileHeight:     th,
		OverlapX:       ox,
		OverlapY:       oy,
		GridRows:       rows,
		GridCols:       cols,
		EdgeMode:       edge,
		FilterBlank:    app.filterBlank,
		BlankThreshold: app.blankThreshold,
		FilterVariance: app.filterVariance,
		MinVariance:    app.minVariance,
		ROI:            roi,
		OutputFormat:   outFmt,
		JpegQuality:    int(app.jpegQuality),
		MaskDir:        app.maskDir,
	}, nil
}

func (app *CutterApp) refreshPreview() {
	if app.currentImage == nil {
		app.previewBitmap = nil
		if app.win != nil {
			app.win.Invalidate()
		}
		return
	}

	origB := app.currentImage.Bounds()
	origW := origB.Dx()
	origH := origB.Dy()

	scale := app.zoomScale
	if scale <= 0.05 {
		scale = 1.0
	}

	dstW := int(float64(origW) * scale)
	dstH := int(float64(origH) * scale)
	if dstW <= 0 {
		dstW = 1
	}
	if dstH <= 0 {
		dstH = 1
	}

	previewImg := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	// Fast bilinear or nearest-neighbor scale
	for y := 0; y < dstH; y++ {
		sy := int(float64(y) / scale)
		if sy >= origH {
			sy = origH - 1
		}
		for x := 0; x < dstW; x++ {
			sx := int(float64(x) / scale)
			if sx >= origW {
				sx = origW - 1
			}
			previewImg.Set(x, y, app.currentImage.At(sx, sy))
		}
	}

	cfg, _ := app.parseConfig()

	// 1. Draw Grid Overlay in Batch mode
	if app.showGridOverlay && app.activeTab == TabBatchSlice {
		crops := calculateCrops(origW, origH, cfg)
		gridColor := color.RGBA{R: 0, G: 210, B: 255, A: 160} // Cyan overlay line

		for _, crop := range crops {
			x1 := int(float64(crop.Bounds.Min.X) * scale)
			y1 := int(float64(crop.Bounds.Min.Y) * scale)
			x2 := int(float64(crop.Bounds.Max.X) * scale)
			y2 := int(float64(crop.Bounds.Max.Y) * scale)

			if x2 >= dstW {
				x2 = dstW - 1
			}
			if y2 >= dstH {
				y2 = dstH - 1
			}

			// Draw bounding box edges
			for px := x1; px <= x2; px++ {
				if y1 >= 0 && y1 < dstH {
					previewImg.Set(px, y1, gridColor)
				}
				if y2 >= 0 && y2 < dstH {
					previewImg.Set(px, y2, gridColor)
				}
			}
			for py := y1; py <= y2; py++ {
				if x1 >= 0 && x1 < dstW {
					previewImg.Set(x1, py, gridColor)
				}
				if x2 >= 0 && x2 < dstW {
					previewImg.Set(x2, py, gridColor)
				}
			}
		}
	}

	// 2. Draw Manual ROI Overlay
	if app.roiActive {
		rx1 := int(float64(app.roiMinX) * scale)
		ry1 := int(float64(app.roiMinY) * scale)
		rx2 := int(float64(app.roiMaxX) * scale)
		ry2 := int(float64(app.roiMaxY) * scale)

		if rx1 > rx2 {
			rx1, rx2 = rx2, rx1
		}
		if ry1 > ry2 {
			ry1, ry2 = ry2, ry1
		}
		if rx2 >= dstW {
			rx2 = dstW - 1
		}
		if ry2 >= dstH {
			ry2 = dstH - 1
		}

		borderColor := color.RGBA{R: 255, G: 200, B: 0, A: 255} // Gold border
		fillColor := color.RGBA{R: 255, G: 215, B: 0, A: 45}    // Semi-transparent amber

		// Alpha blend fill
		for py := ry1; py <= ry2; py++ {
			for px := rx1; px <= rx2; px++ {
				origC := previewImg.RGBAAt(px, py)
				blended := color.RGBA{
					R: uint8((int(origC.R)*(255-45) + int(fillColor.R)*45) / 255),
					G: uint8((int(origC.G)*(255-45) + int(fillColor.G)*45) / 255),
					B: uint8((int(origC.B)*(255-45) + int(fillColor.B)*45) / 255),
					A: 255,
				}
				previewImg.SetRGBA(px, py, blended)
			}
		}

		// Double pixel border for visibility
		for w := 0; w < 2; w++ {
			for px := rx1; px <= rx2; px++ {
				if ry1+w < dstH {
					previewImg.Set(px, ry1+w, borderColor)
				}
				if ry2-w >= 0 {
					previewImg.Set(px, ry2-w, borderColor)
				}
			}
			for py := ry1; py <= ry2; py++ {
				if rx1+w < dstW {
					previewImg.Set(rx1+w, py, borderColor)
				}
				if rx2-w >= 0 {
					previewImg.Set(rx2-w, py, borderColor)
				}
			}
		}
	}

	app.previewBitmap = ui.NewBitmap(previewImg)
	if app.win != nil {
		app.win.Invalidate()
	}
}

func (app *CutterApp) setZoom(mode string) {
	app.zoomStr = mode
	if app.currentImage == nil || app.win == nil {
		return
	}
	origW := app.currentImage.Bounds().Dx()
	origH := app.currentImage.Bounds().Dy()

	switch mode {
	case "自适应窗口":
		winW, winH := app.win.Size()
		availW := float64(winW - 460) // Subtract control panel width
		availH := float64(winH - 140) // Subtract header & status bar
		if availW <= 100 {
			availW = 400
		}
		if availH <= 100 {
			availH = 300
		}
		scaleX := availW / float64(origW)
		scaleY := availH / float64(origH)
		scale := scaleX
		if scaleY < scale {
			scale = scaleY
		}
		if scale > 1.0 {
			scale = 1.0
		}
		app.zoomScale = scale
	case "100%":
		app.zoomScale = 1.0
	case "50%":
		app.zoomScale = 0.5
	case "25%":
		app.zoomScale = 0.25
	}
	app.refreshPreview()
}

func (app *CutterApp) updateROIStrings() {
	app.roiXStr = strconv.Itoa(app.roiMinX)
	app.roiYStr = strconv.Itoa(app.roiMinY)
	app.roiWStr = strconv.Itoa(app.roiMaxX - app.roiMinX)
	app.roiHStr = strconv.Itoa(app.roiMaxY - app.roiMinY)
}

func (app *CutterApp) applyROIFromStrings() {
	if app.currentImage == nil {
		return
	}
	origW := app.currentImage.Bounds().Dx()
	origH := app.currentImage.Bounds().Dy()

	x, _ := strconv.Atoi(app.roiXStr)
	y, _ := strconv.Atoi(app.roiYStr)
	w, _ := strconv.Atoi(app.roiWStr)
	h, _ := strconv.Atoi(app.roiHStr)

	if w <= 0 || h <= 0 {
		return
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x+w > origW {
		w = origW - x
	}
	if y+h > origH {
		h = origH - y
	}

	app.roiMinX = x
	app.roiMinY = y
	app.roiMaxX = x + w
	app.roiMaxY = y + h
	app.roiActive = true
	app.refreshPreview()
}

func (app *CutterApp) estimateTiles() {
	if app.currentImage == nil {
		app.statusMessage = "请先选择原图目录并加载图片"
		return
	}
	cfg, err := app.parseConfig()
	if err != nil {
		app.statusMessage = fmt.Sprintf("参数错误: %v", err)
		return
	}

	crops := calculateCrops(app.currentImage.Bounds().Dx(), app.currentImage.Bounds().Dy(), cfg)
	totalPerImage := len(crops)
	totalEstimated := totalPerImage * len(app.imageFiles)
	app.statusMessage = fmt.Sprintf("预估切图: 当前单图 %d 片 | 整批 %d 张原图预计产出约 %d 个切片",
		totalPerImage, len(app.imageFiles), totalEstimated)
}

func (app *CutterApp) startBatchSlicing() {
	if app.inputDir == "" {
		app.statusMessage = "请先选择包含原图的文件夹"
		return
	}
	if app.outputDir == "" {
		app.outputDir = filepath.Join(app.inputDir, "tiles_output")
	}

	cfg, err := app.parseConfig()
	if err != nil {
		app.statusMessage = fmt.Sprintf("参数错误: %v", err)
		return
	}

	app.mu.Lock()
	if app.progress.IsRunning {
		app.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.cancelFunc = cancel
	app.progress = ProgressStats{
		TotalFiles:     len(app.imageFiles),
		ProcessedFiles: 0,
		TotalTiles:     0,
		IsRunning:      true,
	}
	app.mu.Unlock()

	go func() {
		err := ExecuteBatchSlicing(ctx, app.inputDir, app.outputDir, cfg, &app.progress)
		app.mu.Lock()
		app.progress.IsRunning = false
		app.cancelFunc = nil
		if err != nil {
			if ctx.Err() != nil {
				app.statusMessage = "切图任务已由用户主动终止"
			} else {
				app.statusMessage = fmt.Sprintf("切图过程异常: %v", err)
			}
		} else {
			app.statusMessage = fmt.Sprintf("切图全部完成！共处理 %d 张图像，生成 %d 个切片，清单已写入 manifest.json",
				app.progress.ProcessedFiles, app.progress.TotalTiles)
		}
		app.mu.Unlock()
		if app.win != nil {
			app.win.Invalidate()
		}
	}()
}

func (app *CutterApp) startStitching() {
	if app.manifestPath == "" {
		app.statusMessage = "请先选择切片清单文件 manifest.json"
		return
	}
	if app.outputDir == "" {
		app.outputDir = filepath.Join(filepath.Dir(app.manifestPath), "stitched_output")
	}

	app.mu.Lock()
	if app.progress.IsRunning {
		app.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.cancelFunc = cancel
	app.progress = ProgressStats{
		IsRunning: true,
	}
	app.mu.Unlock()

	go func() {
		err := ExecuteStitching(ctx, app.manifestPath, app.outputDir, &app.progress)
		app.mu.Lock()
		app.progress.IsRunning = false
		app.cancelFunc = nil
		if err != nil {
			if ctx.Err() != nil {
				app.statusMessage = "拼接任务已由用户主动终止"
			} else {
				app.statusMessage = fmt.Sprintf("拼接过程异常: %v", err)
			}
		} else {
			app.statusMessage = fmt.Sprintf("大图反向拼接还原完成！已输出至: %s", app.outputDir)
		}
		app.mu.Unlock()
		if app.win != nil {
			app.win.Invalidate()
		}
	}()
}

func (app *CutterApp) stopTask() {
	app.mu.Lock()
	if app.cancelFunc != nil {
		app.cancelFunc()
	}
	app.mu.Unlock()
}

func (app *CutterApp) cropCurrentROI() {
	if len(app.imageFiles) == 0 || app.currentImage == nil {
		app.statusMessage = "当前无打开的图像文件"
		return
	}
	if !app.roiActive {
		app.statusMessage = "请先在视图中拖拽框选裁切选区"
		return
	}
	if app.outputDir == "" {
		app.outputDir = filepath.Join(filepath.Dir(app.imageFiles[app.currentFileIdx]), "roi_crops")
	}

	roi := image.Rect(app.roiMinX, app.roiMinY, app.roiMaxX, app.roiMaxY)
	outFmt := strings.ToLower(app.outputFormatStr)
	outPath, err := ExecuteCropSingleImage(app.imageFiles[app.currentFileIdx], app.outputDir, roi, outFmt, int(app.jpegQuality))
	if err != nil {
		app.statusMessage = fmt.Sprintf("裁剪保存失败: %v", err)
		return
	}
	app.statusMessage = fmt.Sprintf("当前选区已裁剪保存: %s", filepath.Base(outPath))
}

func (app *CutterApp) openFolderInExplorer(path string) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(path, 0755)
	_ = exec.Command("explorer.exe", path).Start()
}

func drawTile(dst *image.RGBA, x0, y0, w, h int, fill, stroke color.RGBA) {
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			if x == x0 || x == x0+w-1 || y == y0 || y == y0+h-1 {
				dst.Set(x, y, stroke)
			} else {
				dst.Set(x, y, fill)
			}
		}
	}
}

func generatePlaceholderBitmap() *ui.Bitmap {
	w, h := 260, 160
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))

	bg := color.RGBA{R: 246, G: 248, B: 252, A: 255}
	border := color.RGBA{R: 216, G: 226, B: 238, A: 255}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x == 0 || x == w-1 || y == 0 || y == h-1 {
				rgba.Set(x, y, border)
			} else {
				rgba.Set(x, y, bg)
			}
		}
	}

	cx, cy := w/2, h/2 - 4
	tileW, tileH := 42, 42
	gap := 10

	// 4 tiles representing image tile slicing
	drawTile(rgba, cx-tileW-gap/2, cy-tileH-gap/2, tileW, tileH,
		color.RGBA{R: 215, G: 238, B: 255, A: 255}, color.RGBA{R: 50, G: 145, B: 240, A: 255})
	drawTile(rgba, cx+gap/2, cy-tileH-gap/2, tileW, tileH,
		color.RGBA{R: 230, G: 226, B: 255, A: 255}, color.RGBA{R: 115, G: 95, B: 230, A: 255})
	drawTile(rgba, cx-tileW-gap/2, cy+gap/2, tileW, tileH,
		color.RGBA{R: 255, G: 244, B: 215, A: 255}, color.RGBA{R: 235, G: 160, B: 25, A: 255})
	drawTile(rgba, cx+gap/2, cy+gap/2, tileW, tileH,
		color.RGBA{R: 218, G: 250, B: 232, A: 255}, color.RGBA{R: 40, G: 185, B: 110, A: 255})

	// Dashed crosshair lines representing cutting axes
	dashCol := color.RGBA{R: 255, G: 110, B: 110, A: 220}
	for y := cy - tileH - gap - 4; y <= cy + tileH + gap + 4; y++ {
		if (y/4)%2 == 0 && y >= 0 && y < h {
			rgba.Set(cx, y, dashCol)
		}
	}
	for x := cx - tileW - gap - 4; x <= cx + tileW + gap + 4; x++ {
		if (x/4)%2 == 0 && x >= 0 && x < w {
			rgba.Set(x, cy, dashCol)
		}
	}

	return ui.NewBitmap(rgba)
}

func (app *CutterApp) view(c *ui.Context) {
	ui.Column(c).Fill().Children(func() {
		// 1. Top Global Navigation & Directory Selection Header
		ui.Row(c).Wrap().Padding(10, 16).Gap(10).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "选择原图目录").Clicked() {
				app.selectInputDir()
			}
			inDisplay := app.inputDir
			if inDisplay == "" {
				inDisplay = "未指定原图目录 (支持直接拖拽图片文件夹到窗口)"
			}
			ui.Text(c, inDisplay).TextColor(ui.RGB(140, 150, 165))

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				if ui.Button(c, "选择输出目录").Clicked() {
					app.selectOutputDir()
				}
				outDisplay := app.outputDir
				if outDisplay == "" {
					outDisplay = "默认 tiles_output"
				}
				ui.Text(c, outDisplay).TextColor(ui.RGB(140, 150, 165))
			})

			if app.outputDir != "" {
				if ui.Button(c, "打开输出文件夹").Clicked() {
					app.openFolderInExplorer(app.outputDir)
				}
			}
		})

		ui.Divider(c)

		// 2. Tab Selector Bar
		ui.Row(c).Padding(8, 16).Gap(10).AlignItems(ui.Center).Children(func() {
			batchBtnText := "批量智能切图"
			if app.activeTab == TabBatchSlice {
				batchBtnText = "【 批量智能切图 】"
			}
			if ui.Button(c, batchBtnText).Clicked() {
				app.activeTab = TabBatchSlice
				app.refreshPreview()
			}

			roiBtnText := "交互框选裁剪"
			if app.activeTab == TabManualROI {
				roiBtnText = "【 交互框选裁剪 】"
			}
			if ui.Button(c, roiBtnText).Clicked() {
				app.activeTab = TabManualROI
				app.refreshPreview()
			}

			stitchBtnText := "反向还原拼接"
			if app.activeTab == TabStitching {
				stitchBtnText = "【 反向还原拼接 】"
			}
			if ui.Button(c, stitchBtnText).Clicked() {
				app.activeTab = TabStitching
				app.refreshPreview()
			}

			// Image switcher controls when files are loaded
			if len(app.imageFiles) > 0 {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					if ui.Button(c, "◀ 上一张").Clicked() {
						app.prevImage()
					}
					infoStr := fmt.Sprintf("%d / %d: %s", app.currentFileIdx+1, len(app.imageFiles), filepath.Base(app.imageFiles[app.currentFileIdx]))
					ui.Text(c, infoStr).TextColor(ui.RGB(100, 110, 130)).Bold()
					if ui.Button(c, "下一张 ▶").Clicked() {
						app.nextImage()
					}
				})
			}
		})

		ui.Divider(c)

		// 3. Main Work Area: Left Workspace Viewport + Vertical Divider + Right Parameter Sidebar
		ui.Row(c).Grow(1).Fill().Children(func() {
			// Left: Interactive Preview Canvas Area
			ui.Column(c).Grow(1).Fill().Children(func() {
				// Canvas Toolbar: Zoom & View Options
				ui.Row(c).Padding(6, 14).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "缩放:").TextColor(ui.RGB(140, 150, 165))
					zoomSel := ui.Select(c, &app.zoomStr, []string{"自适应窗口", "100%", "50%", "25%"}).Width(100)
					if zoomSel.Changed() {
						app.setZoom(app.zoomStr)
					}

					if app.activeTab == TabBatchSlice {
						gridLabel := "显示切分网格: 开启"
						if !app.showGridOverlay {
							gridLabel = "显示切分网格: 关闭"
						}
						if ui.Button(c, gridLabel).Clicked() {
							app.showGridOverlay = !app.showGridOverlay
							app.refreshPreview()
						}
					}

					if app.currentImage != nil {
						origW := app.currentImage.Bounds().Dx()
						origH := app.currentImage.Bounds().Dy()
						dimInfo := fmt.Sprintf("原始分辨率: %d x %d 像素 (显示比例: %.1f%%)", origW, origH, app.zoomScale*100)
						ui.Text(c, dimInfo).TextColor(ui.RGB(120, 130, 145))
					}
				})

				ui.Divider(c)

				// Image Rendering & Drag Interaction Area OR Centered Placeholder View
				if app.previewBitmap != nil {
					ui.ScrollBoth(c).Grow(1).Fill().Children(func() {
						imgElem := ui.Image(c, app.previewBitmap)

						// Handle interactive mouse drag in Manual ROI mode
						if app.activeTab == TabManualROI {
							px, py, over := imgElem.PointerPosition()
							isPressed := imgElem.Pressed()

							if isPressed {
								if !app.isDragging && over {
									app.isDragging = true
									app.dragStartX = px
									app.dragStartY = py
								} else if app.isDragging {
									scale := app.zoomScale
									if scale <= 0 {
										scale = 1.0
									}
									x1 := int(float64(app.dragStartX) / scale)
									y1 := int(float64(app.dragStartY) / scale)
									x2 := int(float64(px) / scale)
									y2 := int(float64(py) / scale)

									if x1 > x2 {
										x1, x2 = x2, x1
									}
									if y1 > y2 {
										y1, y2 = y2, y1
									}

									app.roiMinX = x1
									app.roiMinY = y1
									app.roiMaxX = x2
									app.roiMaxY = y2
									app.roiActive = true
									app.updateROIStrings()
									app.refreshPreview()
								}
							} else if app.isDragging {
								app.isDragging = false
								app.updateROIStrings()
								app.refreshPreview()
							}
						}
					})
				} else {
					// Elegant Centered Placeholder View
					ui.Column(c).Grow(1).Center().Gap(14).Children(func() {
						if app.placeholderBitmap != nil {
							ui.Image(c, app.placeholderBitmap)
						}
						ui.Text(c, "尚未加载图像").FontSize(18).Bold().TextAlign(ui.Center)
						ui.Text(c, "点击下方按钮选择文件夹，或直接拖拽图片文件夹到此窗口").FontSize(13).TextColor(ui.RGB(130, 140, 155)).TextAlign(ui.Center)
						if ui.Button(c, "选择原图目录...").Clicked() {
							app.selectInputDir()
						}
						ui.Text(c, "支持格式: TIFF · PNG · JPG · WEBP · BMP").FontSize(11).TextColor(ui.RGB(160, 170, 185)).TextAlign(ui.Center)
					})
				}
			})

			// Vertical Divider separating left viewport and right sidebar
			ui.Divider(c)

			// Right: Configuration Control Sidebar
			ui.Scroll(c).Width(380).Padding(14, 18).Children(func() {
				switch app.activeTab {
				case TabBatchSlice:
					app.renderBatchConfigView(c)
				case TabManualROI:
					app.renderManualROIConfigView(c)
				case TabStitching:
					app.renderStitchingConfigView(c)
				}
			})
		})

		ui.Divider(c)

		// 4. Bottom Execution Bar & Status Bar
		ui.Column(c).Padding(10, 16).Gap(6).Children(func() {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				if app.progress.IsRunning {
					if ui.Button(c, "终止切图任务").Clicked() {
						app.stopTask()
					}
					statusStr := fmt.Sprintf("正在切分处理中: %d / %d 张原图 | 已产生 %d 个切片...",
						app.progress.ProcessedFiles, app.progress.TotalFiles, app.progress.TotalTiles)
					ui.Text(c, statusStr).TextColor(ui.RGB(230, 140, 0)).Bold()
				} else {
					if app.activeTab == TabBatchSlice {
						if ui.Button(c, "预估切图数量").Clicked() {
							app.estimateTiles()
						}
						if ui.Button(c, "开始批量切图").Clicked() {
							app.startBatchSlicing()
						}
					} else if app.activeTab == TabManualROI {
						if ui.Button(c, "裁剪保存当前单图选区").Clicked() {
							app.cropCurrentROI()
						}
						if ui.Button(c, "以此选区批量切所有图片").Clicked() {
							app.startBatchSlicing()
						}
					} else if app.activeTab == TabStitching {
						if ui.Button(c, "开始大图反向还原拼接").Clicked() {
							app.startStitching()
						}
					}
				}
			})

			if app.statusMessage != "" {
				ui.Text(c, app.statusMessage).TextColor(ui.RGB(120, 140, 165))
			}
		})
	})
}

func (app *CutterApp) renderBatchConfigView(c *ui.Context) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Text(c, "切分模式与尺寸规格").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))

		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "切图模式:").Width(70)
			modeSel := ui.Select(c, &app.sliceModeStr, []string{"滑动窗口切分", "等分网格切分"}).Width(160)
			if modeSel.Changed() {
				app.refreshPreview()
			}
		})

		if app.sliceModeStr == "滑动窗口切分" {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "切片尺寸:").Width(70)
				ui.Text(c, "宽")
				if ui.TextInput(c, &app.tileWidthStr).Width(60).Changed() {
					app.refreshPreview()
				}
				ui.Text(c, "高")
				if ui.TextInput(c, &app.tileHeightStr).Width(60).Changed() {
					app.refreshPreview()
				}
				ui.Text(c, "px").TextColor(ui.RGB(150, 160, 175))
			})

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "步长重叠:").Width(70)
				ui.Text(c, "横")
				if ui.TextInput(c, &app.overlapXStr).Width(60).Changed() {
					app.refreshPreview()
				}
				ui.Text(c, "纵")
				if ui.TextInput(c, &app.overlapYStr).Width(60).Changed() {
					app.refreshPreview()
				}
				ui.Text(c, "px").TextColor(ui.RGB(150, 160, 175))
			})
		} else {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "网格等分:").Width(70)
				ui.Text(c, "行")
				if ui.TextInput(c, &app.gridRowsStr).Width(60).Changed() {
					app.refreshPreview()
				}
				ui.Text(c, "列")
				if ui.TextInput(c, &app.gridColsStr).Width(60).Changed() {
					app.refreshPreview()
				}
			})
		}

		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "边缘处理:").Width(70)
			edgeSel := ui.Select(c, &app.edgeModeStr, []string{
				"回溯对齐 (保持尺寸)",
				"填充补全 (固定尺寸)",
				"丢弃边缘 (忽略余量)",
				"保留实际 (不规则尺寸)",
			}).Width(190)
			if edgeSel.Changed() {
				app.refreshPreview()
			}
		})

		ui.Divider(c)

		ui.Text(c, "智能内容质量过滤").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))

		if ui.Checkbox(c, &app.filterBlank, "过滤空白 / 纯单色背景切片").Changed() {
			app.refreshPreview()
		}
		if app.filterBlank {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("单色占比: %.0f%%", app.blankThreshold*100)).Width(95)
				if ui.Slider(c, &app.blankThreshold, 0.5, 1.0).Width(150).Changed() {
					app.refreshPreview()
				}
			})
		}

		if ui.Checkbox(c, &app.filterVariance, "过滤低方差无纹理平坦切片").Changed() {
			app.refreshPreview()
		}
		if app.filterVariance {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("最小方差: %.1f", app.minVariance)).Width(95)
				if ui.Slider(c, &app.minVariance, 1.0, 50.0).Width(150).Changed() {
					app.refreshPreview()
				}
			})
		}

		ui.Divider(c)

		ui.Text(c, "成对掩码同步切图 (可选)").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))
		if ui.Button(c, "选择配准掩码目录").Clicked() {
			app.selectMaskDir()
		}
		maskDisplay := app.maskDir
		if maskDisplay == "" {
			maskDisplay = "未指定掩码目录 (如需同步切分Label/Mask则指定)"
		}
		ui.Text(c, maskDisplay).TextColor(ui.RGB(140, 150, 165))

		ui.Divider(c)

		ui.Text(c, "导出格式设置").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "图片格式:").Width(70)
			ui.Select(c, &app.outputFormatStr, []string{"PNG", "JPG"}).Width(80)
			if app.outputFormatStr == "JPG" {
				ui.Text(c, fmt.Sprintf("质量: %.0f", app.jpegQuality))
				ui.Slider(c, &app.jpegQuality, 50, 100).Width(90)
			}
		})
	})
}

func (app *CutterApp) renderManualROIConfigView(c *ui.Context) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Text(c, "交互式框选选区").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))
		ui.Text(c, "在左侧图像预览区域按住鼠标左键拖拽，即可绘制目标裁切框。").TextColor(ui.RGB(140, 150, 165))

		if app.roiActive {
			ui.Divider(c)
			ui.Text(c, "当前选区坐标与范围:").Bold()

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "起点 X:")
				if ui.TextInput(c, &app.roiXStr).Width(60).Changed() {
					app.applyROIFromStrings()
				}
				ui.Text(c, "起点 Y:")
				if ui.TextInput(c, &app.roiYStr).Width(60).Changed() {
					app.applyROIFromStrings()
				}
			})

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "宽度 W:")
				if ui.TextInput(c, &app.roiWStr).Width(60).Changed() {
					app.applyROIFromStrings()
				}
				ui.Text(c, "高度 H:")
				if ui.TextInput(c, &app.roiHStr).Width(60).Changed() {
					app.applyROIFromStrings()
				}
			})

			if ui.Button(c, "清除当前框选").Clicked() {
				app.roiActive = false
				app.updateROIStrings()
				app.refreshPreview()
			}
		} else {
			ui.Text(c, "当前尚未选定区域，请在左侧预览图拖拽选框。").TextColor(ui.RGB(130, 140, 155))
		}

		ui.Divider(c)

		ui.Text(c, "导出格式设置").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "保存格式:").Width(70)
			ui.Select(c, &app.outputFormatStr, []string{"PNG", "JPG"}).Width(80)
			if app.outputFormatStr == "JPG" {
				ui.Text(c, fmt.Sprintf("质量: %.0f", app.jpegQuality))
				ui.Slider(c, &app.jpegQuality, 50, 100).Width(90)
			}
		})
	})
}

func (app *CutterApp) renderStitchingConfigView(c *ui.Context) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Text(c, "离散切片无损反向拼接").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))
		ui.Text(c, "根据切图时生成的 manifest.json 元数据清单，将小切片无缝重构恢复为原始巨幅全景图。").TextColor(ui.RGB(140, 150, 165))

		ui.Divider(c)

		if ui.Button(c, "选择清单 manifest.json").Clicked() {
			app.selectManifestFile()
		}
		manifestDisplay := app.manifestPath
		if manifestDisplay == "" {
			manifestDisplay = "未选择清单文件"
		}
		ui.Text(c, manifestDisplay).TextColor(ui.RGB(140, 150, 165))

		ui.Divider(c)

		if ui.Button(c, "指定拼接输出目录").Clicked() {
			app.selectOutputDir()
		}
		outDisplay := app.outputDir
		if outDisplay == "" {
			outDisplay = "默认原切片目录/stitched_output"
		}
		ui.Text(c, outDisplay).TextColor(ui.RGB(140, 150, 165))
	})
}


func main() {
	app := &CutterApp{
		activeTab:       TabBatchSlice,
		sliceModeStr:    "滑动窗口切分",
		tileWidthStr:    "512",
		tileHeightStr:   "512",
		overlapXStr:     "64",
		overlapYStr:     "64",
		gridRowsStr:     "4",
		gridColsStr:     "4",
		edgeModeStr:     "回溯对齐 (保持尺寸)",
		filterBlank:     true,
		blankThreshold:  0.95,
		filterVariance:  false,
		minVariance:     10.0,
		outputFormatStr: "PNG",
		jpegQuality:     90,
		zoomScale:       0.5,
		zoomStr:         "50%",
		showGridOverlay: true,
		statusMessage:   "就绪：请选择原图文件夹或直接拖拽图片文件夹到窗口",
	}
	app.placeholderBitmap = generatePlaceholderBitmap()

	if len(os.Args) > 1 {
		p := os.Args[1]
		info, err := os.Stat(p)
		if err == nil {
			if info.IsDir() {
				app.loadDirectory(p)
			} else {
				app.loadDirectory(filepath.Dir(p))
			}
		}
	}

	useMirroredUpdates()
	mygo.Use(updater.New(updater.Options{
		Icon:     appIconPNG,
		Language: "zh-Hans",
	}))

	mygo.App.SetName("ImageTileCutter")
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		app.quitting = true
	})
	mygo.App.WhenReady(func() {
		app.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "ImageTileCutter · 工业级大图切片与交互裁剪工具",
			Width:     1280,
			Height:    900,
			MinWidth:  800,
			MinHeight: 600,
			Content:   ui.View(app.view),
		})
		_ = app.win.SetIcon(appIconPNG)

		app.setupTray()

		app.win.OnClose(func(e *mygo.CloseEvent) {
			if !app.quitting {
				e.PreventDefault()
				app.win.Hide()
			}
		})

		app.win.OnFileDrop(func(e *mygo.FileDropEvent) {
			if len(e.Paths) > 0 {
				p := e.Paths[0]
				info, err := os.Stat(p)
				if err == nil {
					if info.IsDir() {
						app.loadDirectory(p)
					} else {
						app.loadDirectory(filepath.Dir(p))
					}
				}
			}
		})
	})

	mygo.App.OnWindowAllClosed(func() {})

	if err := mygo.App.Run(); err != nil {
		panic(err)
	}
}
