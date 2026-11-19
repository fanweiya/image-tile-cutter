package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/ui"
	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

//go:embed icon.png
var appIconPNG []byte

const appVersion = "0.1.4"

// Tab identifiers
const (
	TabBatchSlice = 0
	TabManualROI  = 1
	TabStitching  = 2
	TabGallery    = 3
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
	manifestPath string

	// Image files state
	imageFiles     []string
	currentFileIdx int
	currentImage   image.Image
	previewBitmap  *ui.Bitmap

	// Gigapixel image async loading and preview pyramid proxy
	origWidth      int
	origHeight     int
	basePreview    *image.RGBA
	isLoadingImage bool
	loadSeq        uint64

	// Batch slicing configuration
	sliceModeStr        string
	tileWidthStr        string
	tileHeightStr       string
	overlapXStr         string
	overlapYStr         string
	overlapUnitStr      string
	valStepSizeStr      string
	onlyInROI           bool
	manualExcludedTiles map[string]bool
	gridRowsStr         string
	gridColsStr         string
	edgeModeStr         string
	filterBlank         bool
	blankThreshold      float64
	filterVariance      bool
	minVariance         float64
	filterLaplacianBlur bool
	minLaplacianVar     float64
	filterForeground    bool
	minForegroundRatio  float64
	multiScale100       bool
	multiScale50        bool
	multiScale25        bool
	showHeatmap         bool
	heatmapMetric       string
	hoveredTileInfo     string
	showMiniMap         bool
	outputFormatStr     string
	jpegQuality         float64
	previewWidth        int
	previewHeight       int

	// Manual & Polygon ROI cropping state
	roiShapeMode  string // "矩形框选" 或 "任意多边形/套索"
	polygonPoints Polygon
	roiActive     bool
	roiMinX       int
	roiMinY       int
	roiMaxX       int
	roiMaxY       int
	roiXStr       string
	roiYStr       string
	roiWStr       string
	roiHStr       string
	isDragging    bool
	dragStartX    float32
	dragStartY    float32

	// Viewport & zoom state
	canvasScroll      ui.ScrollState
	sidebarScroll     ui.ScrollState
	zoomScale         float64
	zoomStr           string
	showGridOverlay   bool
	placeholderBitmap *ui.Bitmap
	miniMapBitmap     *ui.Bitmap

	// Tile gallery previewer state
	galleryFiles       []string
	galleryBitmaps     map[string]*ui.Bitmap
	selectedGalleryIdx int
	galleryLoadedDir   string

	// Background worker & progress state
	mu            sync.Mutex
	cancelFunc    context.CancelFunc
	progress      ProgressStats
	statusMessage string
	errorMessage  string
}

func (app *CutterApp) buildTrayMenu() *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{
			Label:    "ImageTileCutter v" + appVersion,
			Disabled: true,
		},
		mygo.Separator(),
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
				if app.tray != nil {
					app.tray.Destroy()
				}
				mygo.App.Exit(0)
			},
		},
	})
}

func (app *CutterApp) setupTray() {
	t, err := mygo.NewTray(mygo.TrayOptions{
		ID:      "image-tile-cutter-tray",
		Icon:    appIconPNG,
		ToolTip: "ImageTileCutter v" + appVersion + " · 大图切片与交互裁剪工具",
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

func (app *CutterApp) refreshGallery() {
	if app.outputDir == "" {
		return
	}
	var files []string
	validExts := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true}

	_ = filepath.Walk(app.outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if validExts[ext] {
			files = append(files, path)
		}
		if len(files) >= 500 {
			return filepath.SkipDir
		}
		return nil
	})

	app.galleryFiles = files
	app.galleryLoadedDir = app.outputDir
	if app.selectedGalleryIdx >= len(files) {
		app.selectedGalleryIdx = 0
	}
	if app.win != nil {
		app.win.Invalidate()
	}
}

func (app *CutterApp) deleteCurrentGalleryTile() {
	if len(app.galleryFiles) == 0 || app.selectedGalleryIdx >= len(app.galleryFiles) {
		return
	}
	targetPath := app.galleryFiles[app.selectedGalleryIdx]
	_ = os.Remove(targetPath)
	app.galleryFiles = append(app.galleryFiles[:app.selectedGalleryIdx], app.galleryFiles[app.selectedGalleryIdx+1:]...)
	if app.selectedGalleryIdx >= len(app.galleryFiles) && len(app.galleryFiles) > 0 {
		app.selectedGalleryIdx = len(app.galleryFiles) - 1
	}
	app.statusMessage = fmt.Sprintf("已删除切片废片: %s", filepath.Base(targetPath))
	if app.win != nil {
		app.win.Invalidate()
	}
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
	app.roiActive = false

	// Fast decode header in < 1ms to retrieve dimensions immediately
	f, err := os.Open(path)
	if err != nil {
		app.statusMessage = fmt.Sprintf("打开图片失败: %v", err)
		return
	}
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		app.statusMessage = fmt.Sprintf("读取图片头信息失败: %v", err)
		return
	}

	app.origWidth = cfg.Width
	app.origHeight = cfg.Height
	app.isLoadingImage = true
	app.statusMessage = fmt.Sprintf("正在后台异步载入高分辨率大图 (%d x %d 像素)...", cfg.Width, cfg.Height)

	// Adapt zoom scale immediately based on original dimensions
	if app.zoomStr == "自适应窗口" || app.zoomStr == "" {
		app.setZoom("自适应窗口")
	}

	if app.win != nil {
		app.win.Invalidate()
	}

	seq := atomic.AddUint64(&app.loadSeq, 1)

	go func(currentSeq uint64, imgPath string, origW, origH int) {
		imgFile, err := os.Open(imgPath)
		if err != nil {
			return
		}
		defer imgFile.Close()

		decoded, _, err := image.Decode(imgFile)
		if err != nil {
			return
		}

		if atomic.LoadUint64(&app.loadSeq) != currentSeq {
			return
		}

		// Build downsampled base preview proxy (max dimension 2048px)
		maxDim := 2048
		targetW := maxDim
		targetH := maxDim
		if origW >= origH {
			targetH = int(float64(origH) * float64(maxDim) / float64(origW))
			if targetH <= 0 {
				targetH = 1
			}
		} else {
			targetW = int(float64(origW) * float64(maxDim) / float64(origH))
			if targetW <= 0 {
				targetW = 1
			}
		}

		base := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
		xdraw.ApproxBiLinear.Scale(base, base.Bounds(), decoded, decoded.Bounds(), xdraw.Over, nil)

		if atomic.LoadUint64(&app.loadSeq) != currentSeq {
			return
		}

		if app.win != nil {
			app.win.Update(func() {
				if atomic.LoadUint64(&app.loadSeq) != currentSeq {
					return
				}
				app.currentImage = decoded
				app.basePreview = base
				app.isLoadingImage = false
				app.statusMessage = fmt.Sprintf("已成功载入图像: %s (%d x %d 像素)", filepath.Base(imgPath), origW, origH)

				if app.zoomStr == "自适应窗口" || app.zoomStr == "" {
					app.setZoom("自适应窗口")
				} else {
					app.refreshPreview()
				}
			})
		}
	}(seq, path, cfg.Width, cfg.Height)
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

func (app *CutterApp) getStepSize() int {
	step, err := strconv.Atoi(app.valStepSizeStr)
	if err != nil || step <= 0 {
		return 16
	}
	return step
}

func (app *CutterApp) adjustIntStr(str *string, delta int, minVal int, maxVal int) {
	v, _ := strconv.Atoi(*str)
	v += delta
	if v < minVal {
		v = minVal
	}
	if maxVal > minVal && v > maxVal {
		v = maxVal
	}
	*str = strconv.Itoa(v)
	app.refreshPreview()
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

	if app.overlapUnitStr == "百分比 (%)" {
		if ox < 0 {
			ox = 0
		}
		if ox > 95 {
			ox = 95
		}
		if oy < 0 {
			oy = 0
		}
		if oy > 95 {
			oy = 95
		}
		ox = int(math.Round(float64(tw) * float64(ox) / 100.0))
		oy = int(math.Round(float64(th) * float64(oy) / 100.0))
	}

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

	var multiScales []float64
	if app.multiScale100 {
		multiScales = append(multiScales, 1.0)
	}
	if app.multiScale50 {
		multiScales = append(multiScales, 0.5)
	}
	if app.multiScale25 {
		multiScales = append(multiScales, 0.25)
	}
	if len(multiScales) == 0 {
		multiScales = []float64{1.0}
	}

	usePoly := app.roiShapeMode == "任意多边形/套索" && !app.polygonPoints.Empty()

	return TileConfig{
		Mode:                mode,
		TileWidth:           tw,
		TileHeight:          th,
		OverlapX:            ox,
		OverlapY:            oy,
		GridRows:            rows,
		GridCols:            cols,
		EdgeMode:            edge,
		FilterBlank:         app.filterBlank,
		BlankThreshold:      app.blankThreshold,
		FilterVariance:      app.filterVariance,
		MinVariance:         app.minVariance,
		FilterLaplacianBlur: app.filterLaplacianBlur,
		MinLaplacianVar:     app.minLaplacianVar,
		FilterForeground:    app.filterForeground,
		MinForegroundRatio:  app.minForegroundRatio,
		ROI:                 roi,
		OnlyInROI:           app.onlyInROI,
		UsePolygonROI:       usePoly,
		PolygonROI:          app.polygonPoints,
		ExcludedTiles:       app.manualExcludedTiles,
		OutputFormat:        outFmt,
		JpegQuality:         int(app.jpegQuality),
		MultiScales:         multiScales,
	}, nil
}

type CutterConfigData struct {
	SliceModeStr        string  `json:"slice_mode_str,omitempty"`
	TileWidthStr        string  `json:"tile_width_str,omitempty"`
	TileHeightStr       string  `json:"tile_height_str,omitempty"`
	OverlapXStr         string  `json:"overlap_x_str,omitempty"`
	OverlapYStr         string  `json:"overlap_y_str,omitempty"`
	OverlapUnitStr      string  `json:"overlap_unit_str,omitempty"`
	ValStepSizeStr      string  `json:"val_step_size_str,omitempty"`
	GridRowsStr         string  `json:"grid_rows_str,omitempty"`
	GridColsStr         string  `json:"grid_cols_str,omitempty"`
	EdgeModeStr         string  `json:"edge_mode_str,omitempty"`
	FilterBlank         bool    `json:"filter_blank"`
	BlankThreshold      float64 `json:"blank_threshold"`
	FilterVariance      bool    `json:"filter_variance"`
	MinVariance         float64 `json:"min_variance"`
	FilterLaplacianBlur bool    `json:"filter_laplacian_blur"`
	MinLaplacianVar     float64 `json:"min_laplacian_var"`
	FilterForeground    bool    `json:"filter_foreground"`
	MinForegroundRatio  float64 `json:"min_foreground_ratio"`
	MultiScale100       bool    `json:"multi_scale_100"`
	MultiScale50        bool    `json:"multi_scale_50"`
	MultiScale25        bool    `json:"multi_scale_25"`
	OutputFormatStr     string  `json:"output_format_str,omitempty"`
	JpegQuality         float64 `json:"jpeg_quality"`
	ShowGridOverlay     bool    `json:"show_grid_overlay"`
	ShowMiniMap         bool    `json:"show_mini_map"`
	InputDir            string  `json:"input_dir,omitempty"`
	OutputDir           string  `json:"output_dir,omitempty"`
}

func getAppConfigPath() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		p := filepath.Join(dir, "config.json")
		return p
	}
	appData := os.Getenv("APPDATA")
	if appData != "" {
		d := filepath.Join(appData, "ImageTileCutter")
		_ = os.MkdirAll(d, 0755)
		return filepath.Join(d, "config.json")
	}
	return "config.json"
}

func (app *CutterApp) saveConfig() {
	data := CutterConfigData{
		SliceModeStr:        app.sliceModeStr,
		TileWidthStr:        app.tileWidthStr,
		TileHeightStr:       app.tileHeightStr,
		OverlapXStr:         app.overlapXStr,
		OverlapYStr:         app.overlapYStr,
		OverlapUnitStr:      app.overlapUnitStr,
		ValStepSizeStr:      app.valStepSizeStr,
		GridRowsStr:         app.gridRowsStr,
		GridColsStr:         app.gridColsStr,
		EdgeModeStr:         app.edgeModeStr,
		FilterBlank:         app.filterBlank,
		BlankThreshold:      app.blankThreshold,
		FilterVariance:      app.filterVariance,
		MinVariance:         app.minVariance,
		FilterLaplacianBlur: app.filterLaplacianBlur,
		MinLaplacianVar:     app.minLaplacianVar,
		FilterForeground:    app.filterForeground,
		MinForegroundRatio:  app.minForegroundRatio,
		MultiScale100:       app.multiScale100,
		MultiScale50:        app.multiScale50,
		MultiScale25:        app.multiScale25,
		OutputFormatStr:     app.outputFormatStr,
		JpegQuality:         app.jpegQuality,
		ShowGridOverlay:     app.showGridOverlay,
		ShowMiniMap:         app.showMiniMap,
		InputDir:            app.inputDir,
		OutputDir:           app.outputDir,
	}
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err == nil {
		_ = os.WriteFile(getAppConfigPath(), bytes, 0644)
	}
}

func (app *CutterApp) loadConfig() {
	bytes, err := os.ReadFile(getAppConfigPath())
	if err != nil {
		return
	}
	var data CutterConfigData
	if err := json.Unmarshal(bytes, &data); err != nil {
		return
	}
	if data.SliceModeStr != "" {
		app.sliceModeStr = data.SliceModeStr
	}
	if data.TileWidthStr != "" {
		app.tileWidthStr = data.TileWidthStr
	}
	if data.TileHeightStr != "" {
		app.tileHeightStr = data.TileHeightStr
	}
	if data.OverlapXStr != "" {
		app.overlapXStr = data.OverlapXStr
	}
	if data.OverlapYStr != "" {
		app.overlapYStr = data.OverlapYStr
	}
	if data.OverlapUnitStr != "" {
		app.overlapUnitStr = data.OverlapUnitStr
	}
	if data.ValStepSizeStr != "" {
		app.valStepSizeStr = data.ValStepSizeStr
	}
	if data.GridRowsStr != "" {
		app.gridRowsStr = data.GridRowsStr
	}
	if data.GridColsStr != "" {
		app.gridColsStr = data.GridColsStr
	}
	if data.EdgeModeStr != "" {
		app.edgeModeStr = data.EdgeModeStr
	}
	app.filterBlank = data.FilterBlank
	if data.BlankThreshold > 0 {
		app.blankThreshold = data.BlankThreshold
	}
	app.filterVariance = data.FilterVariance
	if data.MinVariance > 0 {
		app.minVariance = data.MinVariance
	}
	app.filterLaplacianBlur = data.FilterLaplacianBlur
	if data.MinLaplacianVar > 0 {
		app.minLaplacianVar = data.MinLaplacianVar
	}
	app.filterForeground = data.FilterForeground
	if data.MinForegroundRatio > 0 {
		app.minForegroundRatio = data.MinForegroundRatio
	}
	app.multiScale100 = data.MultiScale100
	app.multiScale50 = data.MultiScale50
	app.multiScale25 = data.MultiScale25
	if data.OutputFormatStr != "" {
		app.outputFormatStr = data.OutputFormatStr
	}
	if data.JpegQuality > 0 {
		app.jpegQuality = data.JpegQuality
	}
	app.showGridOverlay = data.ShowGridOverlay
	app.showMiniMap = data.ShowMiniMap
	if data.InputDir != "" {
		if fi, err := os.Stat(data.InputDir); err == nil && fi.IsDir() {
			app.loadDirectory(data.InputDir)
		}
	}
	if data.OutputDir != "" {
		app.outputDir = data.OutputDir
	}
}

func interpolateHeatmapColor(t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	if t <= 0.33 {
		u := t / 0.33
		return color.RGBA{
			R: uint8(30 + u*(-30)),
			G: uint8(90 + u*(120)),
			B: uint8(240 + u*(-10)),
			A: 255,
		}
	} else if t <= 0.66 {
		u := (t - 0.33) / 0.33
		return color.RGBA{
			R: uint8(0 + u*(120)),
			G: uint8(210 + u*(10)),
			B: uint8(230 + u*(-180)),
			A: 255,
		}
	} else {
		u := (t - 0.66) / 0.34
		if u > 1.0 {
			u = 1.0
		}
		return color.RGBA{
			R: uint8(120 + u*(125)),
			G: uint8(220 + u*(-180)),
			B: uint8(50 + u*(-10)),
			A: 255,
		}
	}
}

func drawLine(dst *image.RGBA, x0, y0, x1, y1 int, col color.RGBA, thickness int) {
	dx := int(math.Abs(float64(x1 - x0)))
	dy := int(math.Abs(float64(y1 - y0)))
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	errVal := dx - dy

	b := dst.Bounds()
	for {
		for ty := -thickness / 2; ty <= thickness/2; ty++ {
			for tx := -thickness / 2; tx <= thickness/2; tx++ {
				px, py := x0+tx, y0+ty
				if px >= b.Min.X && px < b.Max.X && py >= b.Min.Y && py < b.Max.Y {
					dst.SetRGBA(px, py, col)
				}
			}
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * errVal
		if e2 > -dy {
			errVal -= dy
			x0 += sx
		}
		if e2 < dx {
			errVal += dx
			y0 += sy
		}
	}
}

func (app *CutterApp) refreshPreview() {
	if app.basePreview == nil {
		app.previewBitmap = nil
		if app.win != nil {
			app.win.Invalidate()
		}
		return
	}

	origW := app.origWidth
	origH := app.origHeight
	if origW <= 0 || origH <= 0 {
		return
	}

	scale := app.zoomScale
	if scale <= 0.0001 {
		scale = 0.05
	}

	dstW := int(float64(origW) * scale)
	dstH := int(float64(origH) * scale)
	if dstW <= 0 {
		dstW = 1
	}
	if dstH <= 0 {
		dstH = 1
	}

	const maxViewportDim = 4096
	if dstW > maxViewportDim {
		dstW = maxViewportDim
	}
	if dstH > maxViewportDim {
		dstH = maxViewportDim
	}

	app.previewWidth = dstW
	app.previewHeight = dstH

	previewImg := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	// Fast scaling from basePreview proxy or high-resolution full decoded image
	if app.currentImage != nil && (scale > 0.3 || dstW > 2048 || dstH > 2048) {
		xdraw.NearestNeighbor.Scale(previewImg, previewImg.Bounds(), app.currentImage, app.currentImage.Bounds(), xdraw.Over, nil)
	} else {
		xdraw.NearestNeighbor.Scale(previewImg, previewImg.Bounds(), app.basePreview, app.basePreview.Bounds(), xdraw.Over, nil)
	}

	cfg, _ := app.parseConfig()

	// 1. Draw Grid Overlay in Batch mode
	if app.showGridOverlay && app.activeTab == TabBatchSlice {
		allCropsCfg := cfg
		allCropsCfg.OnlyInROI = false
		crops := calculateCrops(origW, origH, allCropsCfg)

		cyanBorder := color.RGBA{R: 0, G: 210, B: 255, A: 200}
		redBorder := color.RGBA{R: 240, G: 45, B: 45, A: 240}
		redFill := color.RGBA{R: 240, G: 45, B: 45, A: 40}

		renderScaleX := float64(dstW) / float64(origW)
		renderScaleY := float64(dstH) / float64(origH)

		baseW := app.basePreview.Bounds().Dx()
		baseH := app.basePreview.Bounds().Dy()
		baseScaleX := float64(baseW) / float64(origW)
		baseScaleY := float64(baseH) / float64(origH)

		for _, crop := range crops {
			tileKey := fmt.Sprintf("%d_%d", crop.Row, crop.Col)
			isManuallyExcluded := app.manualExcludedTiles[tileKey]

			var isOutsideROI bool
			if cfg.OnlyInROI {
				if cfg.UsePolygonROI && !cfg.PolygonROI.Empty() {
					isOutsideROI = !cfg.PolygonROI.OverlapsRect(crop.Bounds)
				} else if !cfg.ROI.Empty() {
					isOutsideROI = !crop.Bounds.Overlaps(cfg.ROI)
				}
			}

			var metrics TileMetrics
			isQualityFiltered := false
			if baseW > 0 && baseH > 0 {
				baseRect := image.Rect(
					int(float64(crop.Bounds.Min.X)*baseScaleX),
					int(float64(crop.Bounds.Min.Y)*baseScaleY),
					int(float64(crop.Bounds.Max.X)*baseScaleX),
					int(float64(crop.Bounds.Max.Y)*baseScaleY),
				)
				metrics = computeTileMetrics(app.basePreview, baseRect, cfg)
				isQualityFiltered = metrics.IsFiltered
			}

			isDiscarded := isManuallyExcluded || isOutsideROI || isQualityFiltered

			x1 := int(float64(crop.Bounds.Min.X) * renderScaleX)
			y1 := int(float64(crop.Bounds.Min.Y) * renderScaleY)
			x2 := int(float64(crop.Bounds.Max.X) * renderScaleX)
			y2 := int(float64(crop.Bounds.Max.Y) * renderScaleY)

			if x2 >= dstW {
				x2 = dstW - 1
			}
			if y2 >= dstH {
				y2 = dstH - 1
			}

			if isDiscarded {
				// Translucent red fill
				for py := y1; py <= y2; py++ {
					for px := x1; px <= x2; px++ {
						origC := previewImg.RGBAAt(px, py)
						blended := color.RGBA{
							R: uint8((int(origC.R)*(255-40) + int(redFill.R)*40) / 255),
							G: uint8((int(origC.G)*(255-40) + int(redFill.G)*40) / 255),
							B: uint8((int(origC.B)*(255-40) + int(redFill.B)*40) / 255),
							A: 255,
						}
						previewImg.SetRGBA(px, py, blended)
					}
				}

				// Red border
				for px := x1; px <= x2; px++ {
					if y1 >= 0 && y1 < dstH && px >= 0 && px < dstW {
						previewImg.Set(px, y1, redBorder)
					}
					if y2 >= 0 && y2 < dstH && px >= 0 && px < dstW {
						previewImg.Set(px, y2, redBorder)
					}
				}
				for py := y1; py <= y2; py++ {
					if x1 >= 0 && x1 < dstW && py >= 0 && py < dstH {
						previewImg.Set(x1, py, redBorder)
					}
					if x2 >= 0 && x2 < dstW && py >= 0 && py < dstH {
						previewImg.Set(x2, py, redBorder)
					}
				}

				// Diagonal red X
				w := x2 - x1
				h := y2 - y1
				if w > 4 && h > 4 {
					for i := 0; i <= w; i++ {
						px := x1 + i
						py1 := y1 + int(float64(i)*float64(h)/float64(w))
						py2 := y2 - int(float64(i)*float64(h)/float64(w))
						if py1 >= 0 && py1 < dstH && px >= 0 && px < dstW {
							previewImg.Set(px, py1, redBorder)
						}
						if py2 >= 0 && py2 < dstH && px >= 0 && px < dstW {
							previewImg.Set(px, py2, redBorder)
						}
					}
				}
			} else {
				// Heatmap color overlay when enabled
				if app.showHeatmap {
					var valNorm float64
					switch app.heatmapMetric {
					case "清晰度 (Laplacian)":
						valNorm = metrics.LaplacianVar / 80.0
					case "组织前景比 (Otsu)":
						valNorm = metrics.ForegroundRatio
					default: // "纹理方差"
						valNorm = metrics.Variance / 40.0
					}
					heatColor := interpolateHeatmapColor(valNorm)
					for py := y1; py <= y2; py++ {
						for px := x1; px <= x2; px++ {
							origC := previewImg.RGBAAt(px, py)
							blended := color.RGBA{
								R: uint8((int(origC.R)*(255-65) + int(heatColor.R)*65) / 255),
								G: uint8((int(origC.G)*(255-65) + int(heatColor.G)*65) / 255),
								B: uint8((int(origC.B)*(255-65) + int(heatColor.B)*65) / 255),
								A: 255,
							}
							previewImg.SetRGBA(px, py, blended)
						}
					}
				}

				// Cyan border for valid kept tiles
				for px := x1; px <= x2; px++ {
					if y1 >= 0 && y1 < dstH && px >= 0 && px < dstW {
						previewImg.Set(px, y1, cyanBorder)
					}
					if y2 >= 0 && y2 < dstH && px >= 0 && px < dstW {
						previewImg.Set(px, y2, cyanBorder)
					}
				}
				for py := y1; py <= y2; py++ {
					if x1 >= 0 && x1 < dstW && py >= 0 && py < dstH {
						previewImg.Set(x1, py, cyanBorder)
					}
					if x2 >= 0 && x2 < dstW && py >= 0 && py < dstH {
						previewImg.Set(x2, py, cyanBorder)
					}
				}
			}
		}
	}

	// 2. Draw Manual Rectangle ROI Overlay
	if app.roiActive && (app.activeTab == TabManualROI || (app.activeTab == TabBatchSlice && app.onlyInROI)) && app.roiShapeMode != "任意多边形/套索" {
		renderScaleX := float64(dstW) / float64(origW)
		renderScaleY := float64(dstH) / float64(origH)
		rx1 := int(float64(app.roiMinX) * renderScaleX)
		ry1 := int(float64(app.roiMinY) * renderScaleY)
		rx2 := int(float64(app.roiMaxX) * renderScaleX)
		ry2 := int(float64(app.roiMaxY) * renderScaleY)

		if rx1 > rx2 {
			rx1, rx2 = rx2, rx1
		}
		if ry1 > ry2 {
			ry1, ry2 = ry2, ry1
		}
		if rx1 < 0 {
			rx1 = 0
		}
		if ry1 < 0 {
			ry1 = 0
		}
		if rx2 >= dstW {
			rx2 = dstW - 1
		}
		if ry2 >= dstH {
			ry2 = dstH - 1
		}

		borderColor := color.RGBA{R: 255, G: 190, B: 0, A: 255}
		fillColor := color.RGBA{R: 255, G: 190, B: 0, A: 45}

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

		for w := 0; w < 2; w++ {
			for px := rx1; px <= rx2; px++ {
				if ry1+w >= 0 && ry1+w < dstH && px >= 0 && px < dstW {
					previewImg.Set(px, ry1+w, borderColor)
				}
				if ry2-w >= 0 && ry2-w < dstH && px >= 0 && px < dstW {
					previewImg.Set(px, ry2-w, borderColor)
				}
			}
			for py := ry1; py <= ry2; py++ {
				if rx1+w >= 0 && rx1+w < dstW && py >= 0 && py < dstH {
					previewImg.Set(rx1+w, py, borderColor)
				}
				if rx2-w >= 0 && rx2-w < dstW && py >= 0 && py < dstH {
					previewImg.Set(rx2-w, py, borderColor)
				}
			}
		}
	}

	// 3. Draw Polygon ROI Overlay
	if (app.activeTab == TabManualROI || (app.activeTab == TabBatchSlice && app.onlyInROI)) &&
		app.roiShapeMode == "任意多边形/套索" && len(app.polygonPoints) > 0 {
		renderScaleX := float64(dstW) / float64(origW)
		renderScaleY := float64(dstH) / float64(origH)
		polyColor := color.RGBA{R: 255, G: 200, B: 0, A: 255}
		vertexColor := color.RGBA{R: 0, G: 220, B: 255, A: 255}

		for i := 0; i < len(app.polygonPoints); i++ {
			p1 := app.polygonPoints[i]
			var p2 image.Point
			if i+1 < len(app.polygonPoints) {
				p2 = app.polygonPoints[i+1]
			} else if len(app.polygonPoints) >= 3 {
				p2 = app.polygonPoints[0]
			} else {
				break
			}
			x1 := int(float64(p1.X) * renderScaleX)
			y1 := int(float64(p1.Y) * renderScaleY)
			x2 := int(float64(p2.X) * renderScaleX)
			y2 := int(float64(p2.Y) * renderScaleY)
			drawLine(previewImg, x1, y1, x2, y2, polyColor, 2)
		}

		for _, pt := range app.polygonPoints {
			vx := int(float64(pt.X) * renderScaleX)
			vy := int(float64(pt.Y) * renderScaleY)
			for dy := -3; dy <= 3; dy++ {
				for dx := -3; dx <= 3; dx++ {
					px, py := vx + dx, vy + dy
					if px >= 0 && px < dstW && py >= 0 && py < dstH {
						previewImg.Set(px, py, vertexColor)
					}
				}
			}
		}
	}

	app.previewBitmap = ui.NewBitmap(previewImg)
	app.refreshMiniMap()
	if app.win != nil {
		app.win.Invalidate()
	}
}

// refreshMiniMap builds an isolated floating mini-map HUD showing full thumbnail and current viewport rectangle.
func (app *CutterApp) refreshMiniMap() {
	if !app.showMiniMap || app.basePreview == nil || app.origWidth <= 0 || app.origHeight <= 0 {
		app.miniMapBitmap = nil
		return
	}

	miniW := 170
	miniH := int(float64(miniW) * float64(app.origHeight) / float64(app.origWidth))
	if miniH > 130 {
		miniH = 130
	}
	if miniH < 50 {
		miniH = 50
	}

	miniImg := image.NewRGBA(image.Rect(0, 0, miniW, miniH))
	xdraw.ApproxBiLinear.Scale(miniImg, miniImg.Bounds(), app.basePreview, app.basePreview.Bounds(), xdraw.Over, nil)

	// Draw subtle border around thumbnail
	borderColor := color.RGBA{R: 90, G: 130, B: 180, A: 240}
	for x := 0; x < miniW; x++ {
		miniImg.Set(x, 0, borderColor)
		miniImg.Set(x, miniH-1, borderColor)
	}
	for y := 0; y < miniH; y++ {
		miniImg.Set(0, y, borderColor)
		miniImg.Set(miniW-1, y, borderColor)
	}

	// Calculate visible viewport rectangle on the minimap
	winW, winH := 1280, 900
	if app.win != nil {
		winW, winH = app.win.Size()
	}
	availW := float64(winW - 400)
	availH := float64(winH - 200)
	if availW <= 100 {
		availW = 600
	}
	if availH <= 100 {
		availH = 500
	}

	dispW := float64(app.previewWidth)
	dispH := float64(app.previewHeight)
	if dispW <= 0 {
		dispW = float64(miniW)
	}
	if dispH <= 0 {
		dispH = float64(miniH)
	}

	var vx, vy, vw, vh int
	if dispW <= availW && dispH <= availH {
		// Entire image fits in viewport
		vx, vy = 0, 0
		vw, vh = miniW, miniH
	} else {
		scaleMiniX := float64(miniW) / dispW
		scaleMiniY := float64(miniH) / dispH

		vx = int(float64(app.canvasScroll.X) * scaleMiniX)
		vy = int(float64(app.canvasScroll.Y) * scaleMiniY)
		vw = int(availW * scaleMiniX)
		vh = int(availH * scaleMiniY)

		if vw > miniW {
			vw = miniW
		}
		if vh > miniH {
			vh = miniH
		}
		if vw < 12 {
			vw = 12
		}
		if vh < 10 {
			vh = 10
		}
		if vx < 0 {
			vx = 0
		}
		if vy < 0 {
			vy = 0
		}
		if vx+vw > miniW {
			vx = miniW - vw
		}
		if vy+vh > miniH {
			vy = miniH - vh
		}
		if vx < 0 {
			vx = 0
		}
		if vy < 0 {
			vy = 0
		}
	}

	// Draw yellow viewport indicator rectangle on minimap
	boxColor := color.RGBA{R: 255, G: 215, B: 0, A: 255}
	fillColor := color.RGBA{R: 255, G: 215, B: 0, A: 35}

	for py := vy; py < vy+vh; py++ {
		for px := vx; px < vx+vw; px++ {
			if px >= 0 && px < miniW && py >= 0 && py < miniH {
				if px == vx || px == vx+vw-1 || py == vy || py == vy+vh-1 {
					miniImg.Set(px, py, boxColor)
				} else {
					origC := miniImg.RGBAAt(px, py)
					blended := color.RGBA{
						R: uint8((int(origC.R)*220 + int(fillColor.R)*35) / 255),
						G: uint8((int(origC.G)*220 + int(fillColor.G)*35) / 255),
						B: uint8((int(origC.B)*220 + int(fillColor.B)*35) / 255),
						A: 255,
					}
					miniImg.SetRGBA(px, py, blended)
				}
			}
		}
	}

	app.miniMapBitmap = ui.NewBitmap(miniImg)
}

func (app *CutterApp) setZoom(mode string) {
	app.zoomStr = mode
	if app.origWidth <= 0 || app.origHeight <= 0 {
		return
	}
	origW := app.origWidth
	origH := app.origHeight

	switch mode {
	case "自适应窗口":
		winW, winH := 1280, 900
		if app.win != nil {
			winW, winH = app.win.Size()
		}
		availW := float64(winW - 460) // Subtract control panel width
		availH := float64(winH - 200) // Subtract header tabs, zoom bar, and status bar
		if availW <= 100 {
			availW = 600
		}
		if availH <= 100 {
			availH = 500
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
	case "5%":
		app.zoomScale = 0.05
	case "10%":
		app.zoomScale = 0.10
	case "20%":
		app.zoomScale = 0.20
	case "35%":
		app.zoomScale = 0.35
	case "50%":
		app.zoomScale = 0.50
	case "75%":
		app.zoomScale = 0.75
	case "100%":
		app.zoomScale = 1.0
	case "150%":
		app.zoomScale = 1.50
	case "200%":
		app.zoomScale = 2.0
	default:
		if strings.HasSuffix(mode, "%") {
			pct, err := strconv.ParseFloat(strings.TrimSuffix(mode, "%"), 64)
			if err == nil && pct > 0 {
				app.zoomScale = pct / 100.0
			}
		}
	}
	app.refreshPreview()
}

func (app *CutterApp) zoomIn() {
	newScale := app.zoomScale * 1.25
	if newScale > 4.0 {
		newScale = 4.0
	}
	app.zoomScale = newScale
	app.zoomStr = fmt.Sprintf("%.0f%%", newScale*100)
	app.refreshPreview()
}

func (app *CutterApp) zoomOut() {
	newScale := app.zoomScale / 1.25
	if newScale < 0.01 {
		newScale = 0.01
	}
	app.zoomScale = newScale
	app.zoomStr = fmt.Sprintf("%.0f%%", newScale*100)
	app.refreshPreview()
}

func (app *CutterApp) updateROIStrings() {
	app.roiXStr = strconv.Itoa(app.roiMinX)
	app.roiYStr = strconv.Itoa(app.roiMinY)
	app.roiWStr = strconv.Itoa(app.roiMaxX - app.roiMinX)
	app.roiHStr = strconv.Itoa(app.roiMaxY - app.roiMinY)
}

func (app *CutterApp) applyROIFromStrings() {
	if app.origWidth <= 0 || app.origHeight <= 0 {
		return
	}
	origW := app.origWidth
	origH := app.origHeight

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
	if app.origWidth <= 0 || app.origHeight <= 0 {
		app.statusMessage = "请先选择原图目录并加载图片"
		return
	}
	cfg, err := app.parseConfig()
	if err != nil {
		app.statusMessage = fmt.Sprintf("参数错误: %v", err)
		return
	}

	crops := calculateCrops(app.origWidth, app.origHeight, cfg)
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
	app.saveConfig()

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
			failedInfo := ""
			if app.progress.FailedFiles > 0 || app.progress.FailedTiles > 0 {
				failedInfo = fmt.Sprintf("（异常跳过图片: %d 张，失败切片: %d 个）", app.progress.FailedFiles, app.progress.FailedTiles)
			}
			app.statusMessage = fmt.Sprintf("切图全部完成！共处理 %d 张图像，生成 %d 个切片%s，清单已写入 manifest.json",
				app.progress.ProcessedFiles, app.progress.TotalTiles, failedInfo)
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

func (app *CutterApp) renderGalleryView(c *ui.Context) {
	ui.Column(c).Grow(1).Fill().Children(func() {
		ui.Row(c).Padding(8, 16).Gap(12).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "🔄 重新扫描切片").Clicked() {
				app.refreshGallery()
			}
			ui.Text(c, fmt.Sprintf("输出目录共发现 %d 个切片图片", len(app.galleryFiles))).TextColor(ui.RGB(100, 115, 135)).Bold()

			if len(app.galleryFiles) > 0 {
				if ui.Button(c, "🗑 删除当前切片废片").Clicked() {
					app.deleteCurrentGalleryTile()
				}
				if app.selectedGalleryIdx >= 0 && app.selectedGalleryIdx < len(app.galleryFiles) {
					curFile := filepath.Base(app.galleryFiles[app.selectedGalleryIdx])
					ui.Text(c, fmt.Sprintf("当前切片: %s (%d / %d)", curFile, app.selectedGalleryIdx+1, len(app.galleryFiles))).TextColor(ui.RGB(50, 130, 220))
				}
			}
		})

		ui.Divider(c)

		if len(app.galleryFiles) == 0 {
			ui.Column(c).Grow(1).Fill().Center().Gap(10).Children(func() {
				ui.Text(c, "暂无可抽检的切片").FontSize(16).Bold().TextColor(ui.RGB(140, 150, 165))
				ui.Text(c, "请先在【批量智能切图】标签页完成切图任务，或确认输出目录是否正确").TextColor(ui.RGB(160, 170, 185))
				if ui.Button(c, "立即扫描输出目录").Clicked() {
					app.refreshGallery()
				}
			})
		} else {
			ui.Row(c).Grow(1).Fill().Children(func() {
				ui.Scroll(c).Width(320).FillHeight().Padding(10, 10).Children(func() {
					for idx, p := range app.galleryFiles {
						base := filepath.Base(p)
						btnLabel := base
						if idx == app.selectedGalleryIdx {
							btnLabel = "▶ " + base
						}
						btn := ui.Button(c, btnLabel)
						if btn.Clicked() {
							app.selectedGalleryIdx = idx
						}
					}
				})

				ui.Divider(c)

				ui.Column(c).Grow(1).Fill().Center().Padding(16, 16).Children(func() {
					if app.selectedGalleryIdx >= 0 && app.selectedGalleryIdx < len(app.galleryFiles) {
						selPath := app.galleryFiles[app.selectedGalleryIdx]
						f, err := os.Open(selPath)
						if err == nil {
							tileImg, _, err := image.Decode(f)
							f.Close()
							if err == nil {
								tb := ui.NewBitmap(tileImg)
								ui.Image(c, tb)
								info := fmt.Sprintf("文件路径: %s | 分辨率: %d x %d 像素", selPath, tileImg.Bounds().Dx(), tileImg.Bounds().Dy())
								ui.Text(c, info).FontSize(12).TextColor(ui.RGB(120, 135, 150)).Padding(8, 0)
							}
						}
					}
				})
			})
		}
	})
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

		// 2. Tab Selector Bar (Clean single row, strictly isolated from view tools)
		ui.Row(c).Wrap().Padding(8, 16).Gap(10).AlignItems(ui.Center).Children(func() {
			batchBtnText := "批量智能切图"
			if app.activeTab == TabBatchSlice {
				batchBtnText = "● 批量智能切图"
			}
			if ui.Button(c, batchBtnText).Clicked() {
				app.activeTab = TabBatchSlice
				app.refreshPreview()
			}

			roiBtnText := "交互框选裁剪"
			if app.activeTab == TabManualROI {
				roiBtnText = "● 交互框选裁剪"
			}
			if ui.Button(c, roiBtnText).Clicked() {
				app.activeTab = TabManualROI
				app.refreshPreview()
			}

			stitchBtnText := "反向还原拼接"
			if app.activeTab == TabStitching {
				stitchBtnText = "● 反向还原拼接"
			}
			if ui.Button(c, stitchBtnText).Clicked() {
				app.activeTab = TabStitching
				app.refreshPreview()
			}

			galleryBtnText := "切片画廊抽检"
			if app.activeTab == TabGallery {
				galleryBtnText = "● 切片画廊抽检"
			}
			if ui.Button(c, galleryBtnText).Clicked() {
				app.activeTab = TabGallery
				app.refreshGallery()
			}
		})

		ui.Divider(c)

		// 3. Main Work Area: Left Workspace Viewport + Vertical Divider + Right Parameter Sidebar
		if app.activeTab == TabGallery {
			app.renderGalleryView(c)
		} else {
			ui.Row(c).Grow(1).Fill().Children(func() {
				// Left: Interactive Preview Canvas Area
				ui.Column(c).Grow(1).Fill().Children(func() {
					// Viewport Toolbar: Pager, Zoom & Overlay Controls (Single responsive row)
					ui.Row(c).Wrap().Padding(6, 12).Gap(8).AlignItems(ui.Center).Children(func() {
						// 1. File Pager
						if len(app.imageFiles) > 0 {
							if ui.Button(c, "◀").Clicked() {
								app.prevImage()
							}
							fname := filepath.Base(app.imageFiles[app.currentFileIdx])
							if len(fname) > 22 {
								fname = fname[:9] + "..." + fname[len(fname)-9:]
							}
							infoStr := fmt.Sprintf("%d/%d: %s", app.currentFileIdx+1, len(app.imageFiles), fname)
							ui.Text(c, infoStr).TextColor(ui.RGB(65, 80, 100)).Bold()
							if ui.Button(c, "▶").Clicked() {
								app.nextImage()
							}
						} else {
							ui.Text(c, "未加载大图文件").TextColor(ui.RGB(140, 150, 165))
						}

						ui.Text(c, "┆").TextColor(ui.RGB(200, 210, 225))

						// 2. Zoom Controls
						if ui.Button(c, "－").Clicked() {
							app.zoomOut()
						}
						if ui.Button(c, "＋").Clicked() {
							app.zoomIn()
						}
						if ui.Button(c, "适应窗口").Clicked() {
							app.setZoom("自适应窗口")
						}
						if ui.Button(c, "1:1").Clicked() {
							app.setZoom("100%")
						}
						zoomLevels := []string{"自适应窗口", "5%", "10%", "20%", "35%", "50%", "75%", "100%", "150%", "200%"}
						zoomSel := ui.Select(c, &app.zoomStr, zoomLevels).Width(95)
						if zoomSel.Changed() {
							app.setZoom(app.zoomStr)
						}

						// 3. Overlays (Batch slicing mode)
						if app.activeTab == TabBatchSlice {
							ui.Text(c, "┆").TextColor(ui.RGB(200, 210, 225))

							gridLabel := "切分网格: 开启"
							if !app.showGridOverlay {
								gridLabel = "切分网格: 关闭"
							}
							if ui.Button(c, gridLabel).Clicked() {
								app.showGridOverlay = !app.showGridOverlay
								app.refreshPreview()
							}

							mapLabel := "鸟瞰小地图: 开启"
							if !app.showMiniMap {
								mapLabel = "鸟瞰小地图: 关闭"
							}
							if ui.Button(c, mapLabel).Clicked() {
								app.showMiniMap = !app.showMiniMap
								app.refreshMiniMap()
								if app.win != nil {
									app.win.Invalidate()
								}
							}

							heatLabel := "热力图: 关闭"
							if app.showHeatmap {
								heatLabel = "热力图: 开启"
							}
							if ui.Button(c, heatLabel).Clicked() {
								app.showHeatmap = !app.showHeatmap
								app.refreshPreview()
							}
							if app.showHeatmap {
								heatSel := ui.Select(c, &app.heatmapMetric, []string{"纹理方差", "清晰度", "组织比"}).Width(95)
								if heatSel.Changed() {
									app.refreshPreview()
								}
							}
						}
					})

					ui.Divider(c)

					// Image Rendering & Drag Interaction Area OR Centered Placeholder / Loading View
					if app.isLoadingImage {
						ui.Column(c).Grow(1).Fill().Center().Gap(14).Children(func() {
							ui.Text(c, "正在后台解码高分辨率巨幅图像...").FontSize(18).Bold().TextColor(ui.RGB(230, 140, 0)).TextAlign(ui.Center)
							dimStr := fmt.Sprintf("原始分辨率: %d x %d 像素 · 正在加速构建金字塔缩略缓存", app.origWidth, app.origHeight)
							ui.Text(c, dimStr).FontSize(13).TextColor(ui.RGB(130, 140, 155)).TextAlign(ui.Center)
						})
					} else if app.previewBitmap != nil {
						ui.Box(c).Grow(1).Fill().Children(func() {
							ui.ScrollBoth(c).TrackScroll(&app.canvasScroll).Grow(1).Fill().HandleInput(func(ev ui.InputEvent) bool {
								if ev.Kind == ui.InputScroll {
									if ev.DY < 0 {
										app.zoomIn()
										return true
									} else if ev.DY > 0 {
										app.zoomOut()
										return true
									}
								}
								if ev.Kind == ui.InputZoom {
									if ev.Scale > 1.0 {
										app.zoomIn()
										return true
									} else if ev.Scale < 1.0 {
										app.zoomOut()
										return true
									}
								}
								if ev.Kind == ui.InputKeyDown {
									switch ev.Key {
									case ui.KeyEqual:
										app.zoomIn()
										return true
									case ui.KeyMinus:
										app.zoomOut()
										return true
									case ui.Key0:
										app.setZoom("自适应窗口")
										return true
									case ui.Key1:
										app.setZoom("100%")
										return true
									case ui.KeyW, ui.KeyUp:
										app.canvasScroll.Y -= 64
										if app.canvasScroll.Y < 0 {
											app.canvasScroll.Y = 0
										}
										return true
									case ui.KeyS, ui.KeyDown:
										app.canvasScroll.Y += 64
										return true
									case ui.KeyA, ui.KeyLeft:
										app.canvasScroll.X -= 64
										if app.canvasScroll.X < 0 {
											app.canvasScroll.X = 0
										}
										return true
									case ui.KeyD, ui.KeyRight:
										app.canvasScroll.X += 64
										return true
									}
								}
								return false
							}).Children(func() {
								imgElem := ui.Image(c, app.previewBitmap).HandleInput(func(ev ui.InputEvent) bool {
									if ev.Kind == ui.InputScroll {
										if ev.DY < 0 {
											app.zoomIn()
											return true
										} else if ev.DY > 0 {
											app.zoomOut()
											return true
										}
									}
									if ev.Kind == ui.InputZoom {
										if ev.Scale > 1.0 {
											app.zoomIn()
											return true
										} else if ev.Scale < 1.0 {
											app.zoomOut()
											return true
										}
									}
									return false
								})

								px, py, over := imgElem.PointerPosition()
								if over && app.previewWidth > 0 && app.previewHeight > 0 && app.origWidth > 0 && app.origHeight > 0 {
								origX := int(float64(px) * float64(app.origWidth) / float64(app.previewWidth))
								origY := int(float64(py) * float64(app.origHeight) / float64(app.previewHeight))

								cfg, _ := app.parseConfig()
								cfg.OnlyInROI = false
								crops := calculateCrops(app.origWidth, app.origHeight, cfg)
								foundTile := false
								for _, crop := range crops {
									if origX >= crop.Bounds.Min.X && origX < crop.Bounds.Max.X &&
										origY >= crop.Bounds.Min.Y && origY < crop.Bounds.Max.Y {
										foundTile = true
										tileKey := fmt.Sprintf("%d_%d", crop.Row, crop.Col)
										isExcluded := app.manualExcludedTiles[tileKey]
										statusText := "保留切片"
										if isExcluded {
											statusText = "手动排除 (红色打X)"
										} else {
											baseW := app.basePreview.Bounds().Dx()
											baseH := app.basePreview.Bounds().Dy()
											if baseW > 0 && baseH > 0 {
												baseScaleX := float64(baseW) / float64(app.origWidth)
												baseScaleY := float64(baseH) / float64(app.origHeight)
												baseRect := image.Rect(
													int(float64(crop.Bounds.Min.X)*baseScaleX),
													int(float64(crop.Bounds.Min.Y)*baseScaleY),
													int(float64(crop.Bounds.Max.X)*baseScaleX),
													int(float64(crop.Bounds.Max.Y)*baseScaleY),
												)
												m := computeTileMetrics(app.basePreview, baseRect, cfg)
												if m.IsFiltered {
													statusText = "质量剔除: " + m.FilterReason
												}
												app.hoveredTileInfo = fmt.Sprintf("切片 [第 %d 行, 第 %d 列] | 像素范围:[%d,%d - %d,%d] | 均值亮度:%.0f | 标准差:%.1f | 清晰度:%.1f | 组织比:%.0f%% | 判定: %s",
													crop.Row+1, crop.Col+1, crop.Bounds.Min.X, crop.Bounds.Min.Y, crop.Bounds.Max.X, crop.Bounds.Max.Y,
													m.MeanLum, m.Variance, m.LaplacianVar, m.ForegroundRatio*100, statusText)
											}
										}
										break
									}
								}
								if !foundTile {
									app.hoveredTileInfo = fmt.Sprintf("大图像素坐标: X=%d, Y=%d", origX, origY)
								}
							}

							// 1. Polygon point placement when polygon ROI is selected
							if (app.activeTab == TabManualROI || (app.activeTab == TabBatchSlice && app.onlyInROI)) && app.roiShapeMode == "任意多边形/套索" {
								if imgElem.Clicked() && over {
									if app.previewWidth > 0 && app.previewHeight > 0 && app.origWidth > 0 && app.origHeight > 0 {
										origX := int(float64(px) * float64(app.origWidth) / float64(app.previewWidth))
										origY := int(float64(py) * float64(app.origHeight) / float64(app.previewHeight))
										app.polygonPoints = append(app.polygonPoints, image.Point{X: origX, Y: origY})
										app.statusMessage = fmt.Sprintf("已采集多边形顶点 #%d: (%d, %d)", len(app.polygonPoints), origX, origY)
										app.refreshPreview()
									}
								}
							}

							// 2. Interactive tile manual exclusion/recovery in batch slicing mode
							if app.activeTab == TabBatchSlice && !app.onlyInROI {
								if imgElem.Clicked() {
									if app.previewWidth > 0 && app.previewHeight > 0 && app.origWidth > 0 && app.origHeight > 0 {
										origX := int(float64(px) * float64(app.origWidth) / float64(app.previewWidth))
										origY := int(float64(py) * float64(app.origHeight) / float64(app.previewHeight))

										cfg, _ := app.parseConfig()
										cfg.OnlyInROI = false
										crops := calculateCrops(app.origWidth, app.origHeight, cfg)
										for _, crop := range crops {
											if origX >= crop.Bounds.Min.X && origX < crop.Bounds.Max.X &&
												origY >= crop.Bounds.Min.Y && origY < crop.Bounds.Max.Y {
												tileKey := fmt.Sprintf("%d_%d", crop.Row, crop.Col)
												if app.manualExcludedTiles[tileKey] {
													delete(app.manualExcludedTiles, tileKey)
													app.statusMessage = fmt.Sprintf("已恢复切片 [第 %d 行, 第 %d 列]", crop.Row+1, crop.Col+1)
												} else {
													app.manualExcludedTiles[tileKey] = true
													app.statusMessage = fmt.Sprintf("已手动排除切片 [第 %d 行, 第 %d 列] (红色打X)", crop.Row+1, crop.Col+1)
												}
												app.refreshPreview()
												break
											}
										}
									}
								}
							}

							// 3. Interactive rectangular ROI drag selection
							if (app.activeTab == TabManualROI || (app.activeTab == TabBatchSlice && app.onlyInROI)) && app.roiShapeMode != "任意多边形/套索" {
								isPressed := imgElem.Pressed()
								if isPressed {
									if !app.isDragging && over {
										app.isDragging = true
										app.dragStartX = px
										app.dragStartY = py
									} else if app.isDragging {
										renderScaleX := float64(app.previewWidth) / float64(app.origWidth)
										renderScaleY := float64(app.previewHeight) / float64(app.origHeight)
										if renderScaleX <= 0 {
											renderScaleX = 1.0
										}
										if renderScaleY <= 0 {
											renderScaleY = 1.0
										}
										x1 := int(float64(app.dragStartX) / renderScaleX)
										y1 := int(float64(app.dragStartY) / renderScaleY)
										x2 := int(float64(px) / renderScaleX)
										y2 := int(float64(py) / renderScaleY)

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

						// 2. Isolated Floating Mini-Map HUD at Bottom-Left (never scales with image zoom)
						if app.showMiniMap && app.miniMapBitmap != nil {
							ui.Box(c).Absolute().Left(16).Bottom(16).Children(func() {
								ui.Column(c).Padding(6, 6).Background(ui.RGBA(18, 22, 28, 230)).Border(1, ui.RGBA(70, 95, 125, 230)).Radius(8).Children(func() {
									ui.Row(c).AlignItems(ui.Center).Gap(6).Children(func() {
										ui.Text(c, "🗺 鸟瞰小地图").FontSize(11).Bold().TextColor(ui.RGB(140, 185, 235)).Grow(1)
										if ui.Button(c, "✕").Padding(1, 6).FontSize(10).Clicked() {
											app.showMiniMap = false
											app.refreshMiniMap()
											if app.win != nil {
												app.win.Invalidate()
											}
										}
									})
									miniImgElem := ui.Image(c, app.miniMapBitmap).Cursor(ui.CursorPointer)
									if px, py, over := miniImgElem.PointerPosition(); over && miniImgElem.Clicked() {
										if app.previewWidth > 0 && app.previewHeight > 0 {
											w, h := 1280, 900
											if app.win != nil {
												w, h = app.win.Size()
											}
											availW := float64(w - 400)
											availH := float64(h - 200)
											miniW, miniH := app.miniMapBitmap.Size()
											if miniW > 0 && miniH > 0 {
												ratioX := float64(px) / float64(miniW)
												ratioY := float64(py) / float64(miniH)
												app.canvasScroll.X = float32(ratioX*float64(app.previewWidth) - availW/2)
												app.canvasScroll.Y = float32(ratioY*float64(app.previewHeight) - availH/2)
												if app.canvasScroll.X < 0 {
													app.canvasScroll.X = 0
												}
												if app.canvasScroll.Y < 0 {
													app.canvasScroll.Y = 0
												}
												app.refreshMiniMap()
												if app.win != nil {
													app.win.Invalidate()
												}
											}
										}
									}
								})
							})
						}
					})
				} else {
					// Elegant Centered Placeholder View
					ui.Column(c).Grow(1).Fill().Center().Gap(14).Children(func() {
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

			// Right: Configuration Control Sidebar with vertical scrollbar
			ui.Scroll(c).Width(380).FillHeight().TrackScroll(&app.sidebarScroll).Padding(14, 18).Children(func() {
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
	}

		ui.Divider(c)

		// 4. Bottom Execution Bar & Dedicated Status Bar
		ui.Column(c).Padding(8, 16).Gap(4).Children(func() {
			ui.Row(c).Wrap().Gap(10).AlignItems(ui.Center).Children(func() {
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

					ui.Text(c, "┆").TextColor(ui.RGB(200, 210, 225))

					if app.hoveredTileInfo != "" {
						ui.Text(c, app.hoveredTileInfo).FontSize(11).TextColor(ui.RGB(0, 130, 210)).Bold().SingleLine()
					} else if app.origWidth > 0 && app.origHeight > 0 {
						dimInfo := fmt.Sprintf("原图尺寸: %d × %d 像素 (显示: %.1f%%) · 滚轮缩放 · WASD平移 · 点击切片排除/恢复", app.origWidth, app.origHeight, app.zoomScale*100)
						ui.Text(c, dimInfo).FontSize(11).TextColor(ui.RGB(110, 125, 140)).SingleLine()
					}
				}
			})

			if app.statusMessage != "" {
				ui.Text(c, app.statusMessage).FontSize(11).TextColor(ui.RGB(120, 140, 165))
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

		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "微调步长:").Width(70)
			ui.Select(c, &app.valStepSizeStr, []string{"1", "8", "16", "32", "64", "128"}).Width(75)
			ui.Text(c, "px (点击+/-增减)").TextColor(ui.RGB(140, 150, 165))
		})

		if app.sliceModeStr == "滑动窗口切分" {
			stepDelta := app.getStepSize()

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "切片宽度:").Width(70)
				if ui.Button(c, "－").Clicked() {
					app.adjustIntStr(&app.tileWidthStr, -stepDelta, 32, 16384)
				}
				if ui.TextInput(c, &app.tileWidthStr).Width(60).Changed() {
					app.refreshPreview()
				}
				if ui.Button(c, "＋").Clicked() {
					app.adjustIntStr(&app.tileWidthStr, stepDelta, 32, 16384)
				}
				ui.Text(c, "px").TextColor(ui.RGB(140, 150, 165))
			})

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "切片高度:").Width(70)
				if ui.Button(c, "－").Clicked() {
					app.adjustIntStr(&app.tileHeightStr, -stepDelta, 32, 16384)
				}
				if ui.TextInput(c, &app.tileHeightStr).Width(60).Changed() {
					app.refreshPreview()
				}
				if ui.Button(c, "＋").Clicked() {
					app.adjustIntStr(&app.tileHeightStr, stepDelta, 32, 16384)
				}
				ui.Text(c, "px").TextColor(ui.RGB(140, 150, 165))
			})

			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "步长单位:").Width(70)
				unitSel := ui.Select(c, &app.overlapUnitStr, []string{"像素 (px)", "百分比 (%)"}).Width(120)
				if unitSel.Changed() {
					if app.overlapUnitStr == "百分比 (%)" {
						ox, _ := strconv.Atoi(app.overlapXStr)
						if ox > 90 {
							app.overlapXStr = "20"
						}
						oy, _ := strconv.Atoi(app.overlapYStr)
						if oy > 90 {
							app.overlapYStr = "20"
						}
					}
					app.refreshPreview()
				}
			})

			isPct := app.overlapUnitStr == "百分比 (%)"
			overlapStep := stepDelta
			unitLabel := "px"
			maxOverlap := 8192
			if isPct {
				overlapStep = 5
				unitLabel = "%"
				maxOverlap = 95
			}

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "横向重叠:").Width(70)
				if ui.Button(c, "－").Clicked() {
					app.adjustIntStr(&app.overlapXStr, -overlapStep, 0, maxOverlap)
				}
				if ui.TextInput(c, &app.overlapXStr).Width(60).Changed() {
					app.refreshPreview()
				}
				if ui.Button(c, "＋").Clicked() {
					app.adjustIntStr(&app.overlapXStr, overlapStep, 0, maxOverlap)
				}
				ui.Text(c, unitLabel).TextColor(ui.RGB(140, 150, 165))
			})

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "纵向重叠:").Width(70)
				if ui.Button(c, "－").Clicked() {
					app.adjustIntStr(&app.overlapYStr, -overlapStep, 0, maxOverlap)
				}
				if ui.TextInput(c, &app.overlapYStr).Width(60).Changed() {
					app.refreshPreview()
				}
				if ui.Button(c, "＋").Clicked() {
					app.adjustIntStr(&app.overlapYStr, overlapStep, 0, maxOverlap)
				}
				ui.Text(c, unitLabel).TextColor(ui.RGB(140, 150, 165))
			})
		} else {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "等分行数:").Width(70)
				if ui.Button(c, "－").Clicked() {
					app.adjustIntStr(&app.gridRowsStr, -1, 1, 100)
				}
				if ui.TextInput(c, &app.gridRowsStr).Width(60).Changed() {
					app.refreshPreview()
				}
				if ui.Button(c, "＋").Clicked() {
					app.adjustIntStr(&app.gridRowsStr, 1, 1, 100)
				}
				ui.Text(c, "行").TextColor(ui.RGB(140, 150, 165))
			})

			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "等分列数:").Width(70)
				if ui.Button(c, "－").Clicked() {
					app.adjustIntStr(&app.gridColsStr, -1, 1, 100)
				}
				if ui.TextInput(c, &app.gridColsStr).Width(60).Changed() {
					app.refreshPreview()
				}
				if ui.Button(c, "＋").Clicked() {
					app.adjustIntStr(&app.gridColsStr, 1, 1, 100)
				}
				ui.Text(c, "列").TextColor(ui.RGB(140, 150, 165))
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

		ui.Text(c, "多尺度金字塔切片 (可选)").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			if ui.Checkbox(c, &app.multiScale100, "100% 原始细节").Changed() {
				app.refreshPreview()
			}
			if ui.Checkbox(c, &app.multiScale50, "50% 中景").Changed() {
				app.refreshPreview()
			}
			if ui.Checkbox(c, &app.multiScale25, "25% 全景宏观").Changed() {
				app.refreshPreview()
			}
		})
		ui.Text(c, "勾选多尺度时，切片将在输出目录按分级子文件夹(scale_1.0x / scale_0.5x 等)分别保存").FontSize(11).TextColor(ui.RGB(140, 150, 165))

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

		if ui.Checkbox(c, &app.filterVariance, "过滤低纹理平坦切片 (灰度标准差)").Changed() {
			app.refreshPreview()
		}
		if app.filterVariance {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("最小标准差: %.1f", app.minVariance)).Width(105)
				if ui.Slider(c, &app.minVariance, 1.0, 50.0).Width(150).Changed() {
					app.refreshPreview()
				}
			})
		}

		if ui.Checkbox(c, &app.filterLaplacianBlur, "过滤对焦虚焦模糊切片 (拉普拉斯梯度)").Changed() {
			app.refreshPreview()
		}
		if app.filterLaplacianBlur {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("清晰度阈值: %.0f", app.minLaplacianVar)).Width(100)
				if ui.Slider(c, &app.minLaplacianVar, 5.0, 150.0).Width(150).Changed() {
					app.refreshPreview()
				}
			})
		}

		if ui.Checkbox(c, &app.filterForeground, "过滤载玻片空白背景 (大津生物组织比)").Changed() {
			app.refreshPreview()
		}
		if app.filterForeground {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("组织占比: %.0f%%", app.minForegroundRatio*100)).Width(100)
				if ui.Slider(c, &app.minForegroundRatio, 0.05, 0.90).Width(150).Changed() {
					app.refreshPreview()
				}
			})
		}

		ui.Divider(c)

		ui.Text(c, "自选区域与切片控制").FontSize(14).Bold().TextColor(ui.RGB(50, 65, 85))

		if ui.Checkbox(c, &app.onlyInROI, "仅在选区范围内切分 (ROI区域限定)").Changed() {
			app.refreshPreview()
		}

		if app.onlyInROI {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "选区形态:").Width(70)
				roiModeSel := ui.Select(c, &app.roiShapeMode, []string{"矩形框选", "任意多边形/套索"}).Width(140)
				if roiModeSel.Changed() {
					app.refreshPreview()
				}
			})

			if app.roiShapeMode == "任意多边形/套索" {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, fmt.Sprintf("多边形顶点: %d 个", len(app.polygonPoints))).TextColor(ui.RGB(210, 140, 20)).Bold()
					if ui.Button(c, "清空顶点").Clicked() {
						app.polygonPoints = nil
						app.refreshPreview()
					}
				})
				ui.Text(c, "提示: 在左侧大图上点击鼠标左键标定顶点(>=3点自动闭合)").FontSize(11).TextColor(ui.RGB(140, 150, 165))
			}
		}

		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if len(app.manualExcludedTiles) > 0 {
				ui.Text(c, fmt.Sprintf("手动排除: %d 片", len(app.manualExcludedTiles))).TextColor(ui.RGB(230, 70, 70)).Bold()
				if ui.Button(c, "恢复全部切片").Clicked() {
					app.manualExcludedTiles = make(map[string]bool)
					app.refreshPreview()
				}
			} else {
				ui.Text(c, "当前未手动排除切片").TextColor(ui.RGB(140, 150, 165))
			}
		})

		if app.roiActive && app.roiShapeMode != "任意多边形/套索" {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("选区: [%d,%d] - [%d,%d]", app.roiMinX, app.roiMinY, app.roiMaxX, app.roiMaxY)).TextColor(ui.RGB(210, 140, 20))
				if ui.Button(c, "清除选区框").Clicked() {
					app.roiActive = false
					app.updateROIStrings()
					app.refreshPreview()
				}
			})
		}

		ui.Text(c, "操作提示: 点击预览图切片可直接排除/恢复(红框红X为剔除)；勾选ROI后可在图中按住鼠标拖拽选框或点击绘制多边形。").FontSize(11).TextColor(ui.RGB(140, 150, 165))

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

		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "选区形态:").Width(70)
			roiSel := ui.Select(c, &app.roiShapeMode, []string{"矩形框选", "任意多边形/套索"}).Width(150)
			if roiSel.Changed() {
				app.refreshPreview()
			}
		})

		if app.roiShapeMode == "任意多边形/套索" {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("多边形顶点: %d 个", len(app.polygonPoints))).TextColor(ui.RGB(210, 140, 20)).Bold()
				if ui.Button(c, "清空多边形顶点").Clicked() {
					app.polygonPoints = nil
					app.refreshPreview()
				}
			})
			ui.Text(c, "提示: 在左侧大图上点击鼠标左键可依次标定多边形顶点，达到3点及以上将自动闭合形成裁切多边形。").FontSize(11).TextColor(ui.RGB(140, 150, 165))
		} else {
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
		activeTab:           TabBatchSlice,
		sliceModeStr:        "滑动窗口切分",
		tileWidthStr:        "512",
		tileHeightStr:       "512",
		overlapXStr:         "64",
		overlapYStr:         "64",
		overlapUnitStr:      "像素 (px)",
		valStepSizeStr:      "16",
		manualExcludedTiles: make(map[string]bool),
		gridRowsStr:         "4",
		gridColsStr:         "4",
		edgeModeStr:         "回溯对齐 (保持尺寸)",
		filterBlank:         true,
		blankThreshold:      0.95,
		filterVariance:      false,
		minVariance:         10.0,
		filterLaplacianBlur: false,
		minLaplacianVar:     35.0,
		filterForeground:    false,
		minForegroundRatio:  0.20,
		multiScale100:       true,
		multiScale50:        false,
		multiScale25:        false,
		showHeatmap:         false,
		heatmapMetric:       "纹理方差",
		roiShapeMode:        "矩形框选",
		showMiniMap:         true,
		outputFormatStr:     "PNG",
		jpegQuality:         90,
		zoomScale:           1.0,
		zoomStr:             "自适应窗口",
		showGridOverlay:     true,
		statusMessage:       "就绪：请选择原图文件夹或直接拖拽图片文件夹到窗口",
	}
	app.placeholderBitmap = generatePlaceholderBitmap()
	app.loadConfig()

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
		app.saveConfig()
	})
	mygo.App.WhenReady(func() {
		app.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "ImageTileCutter · 大图切片与交互裁剪工具",
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

		app.win.OnResize(func() {
			if app.zoomStr == "自适应窗口" && app.origWidth > 0 && app.origHeight > 0 {
				app.setZoom("自适应窗口")
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
