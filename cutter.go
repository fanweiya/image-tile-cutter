package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

type SliceMode string

const (
	ModeSlidingWindow SliceMode = "滑动窗口切分"
	ModeGrid          SliceMode = "等分网格切分"
	ModeManualROI     SliceMode = "手动选区裁剪"
)

type EdgeMode string

const (
	EdgeShiftBack EdgeMode = "回溯对齐 (保持尺寸)"
	EdgePad       EdgeMode = "填充补全 (固定尺寸)"
	EdgeDiscard   EdgeMode = "丢弃边缘 (忽略余量)"
	EdgeKeep      EdgeMode = "保留实际 (不规则尺寸)"
)

type Polygon []image.Point

func (p Polygon) Empty() bool {
	return len(p) < 3
}

func (p Polygon) Contains(pt image.Point) bool {
	if len(p) < 3 {
		return false
	}
	inside := false
	j := len(p) - 1
	for i := 0; i < len(p); i++ {
		xi, yi := p[i].X, p[i].Y
		xj, yj := p[j].X, p[j].Y
		intersect := ((yi > pt.Y) != (yj > pt.Y)) &&
			(pt.X < (xj-xi)*(pt.Y-yi)/(yj-yi)+xi)
		if intersect {
			inside = !inside
		}
		j = i
	}
	return inside
}

func (p Polygon) OverlapsRect(r image.Rectangle) bool {
	if len(p) < 3 {
		return true
	}
	for _, pt := range p {
		if pt.In(r) {
			return true
		}
	}
	corners := []image.Point{
		r.Min,
		{X: r.Max.X, Y: r.Min.Y},
		r.Max,
		{X: r.Min.X, Y: r.Max.Y},
		{X: (r.Min.X + r.Max.X) / 2, Y: (r.Min.Y + r.Max.Y) / 2},
	}
	for _, c := range corners {
		if p.Contains(c) {
			return true
		}
	}
	rectSegments := [][2]image.Point{
		{r.Min, {X: r.Max.X, Y: r.Min.Y}},
		{{X: r.Max.X, Y: r.Min.Y}, r.Max},
		{r.Max, {X: r.Min.X, Y: r.Max.Y}},
		{{X: r.Min.X, Y: r.Max.Y}, r.Min},
	}
	j := len(p) - 1
	for i := 0; i < len(p); i++ {
		p1, p2 := p[j], p[i]
		for _, seg := range rectSegments {
			if segmentsIntersect(p1, p2, seg[0], seg[1]) {
				return true
			}
		}
		j = i
	}
	return false
}

// SelectionStroke 一笔人工框选。Rect 为原图像素坐标，Keep 为 true 表示框内切片保留、false 表示框内切片排除。
// 同一个文件允许多笔，按录入顺序叠加，同一切片被多笔命中时以最后录入的一笔为准。
type SelectionStroke struct {
	Rect image.Rectangle `json:"rect"`
	Keep bool            `json:"keep"`
}

// ResolveStroke 按后录入优先的规则判定矩形 rect 的最终去留。hit 为 false 表示没有选区命中该矩形，交由其他规则决定。
func ResolveStroke(strokes []SelectionStroke, rect image.Rectangle) (keep bool, hit bool) {
	for i := len(strokes) - 1; i >= 0; i-- {
		if rect.Overlaps(strokes[i].Rect) {
			return strokes[i].Keep, true
		}
	}
	return false, false
}

// StrokesHaveKeep 判断笔迹中是否存在保留框。存在保留框时，未被任何笔迹命中的切片默认排除。
func StrokesHaveKeep(strokes []SelectionStroke) bool {
	for _, s := range strokes {
		if s.Keep {
			return true
		}
	}
	return false
}

// StrokeDecision 综合全部笔迹给出切片最终去留：被命中的切片按最后一笔判定；未被任何笔迹命中时，
// 只要存在保留框就一并排除（保留框即划定唯一工作范围），否则保留。
func StrokeDecision(strokes []SelectionStroke, rect image.Rectangle) bool {
	if keep, hit := ResolveStroke(strokes, rect); hit {
		return keep
	}
	return !StrokesHaveKeep(strokes)
}

// ScaleStrokes 把选区按尺度 sc 换算到缩放后的工作图坐标，退化矩形补足 1 像素以保证相交判定有效。
func ScaleStrokes(strokes []SelectionStroke, sc float64) []SelectionStroke {
	if sc >= 0.999 && sc <= 1.001 {
		return strokes
	}
	out := make([]SelectionStroke, 0, len(strokes))
	for _, s := range strokes {
		rect := image.Rect(
			int(float64(s.Rect.Min.X)*sc),
			int(float64(s.Rect.Min.Y)*sc),
			int(float64(s.Rect.Max.X)*sc),
			int(float64(s.Rect.Max.Y)*sc),
		)
		if rect.Dx() < 1 {
			rect.Max.X = rect.Min.X + 1
		}
		if rect.Dy() < 1 {
			rect.Max.Y = rect.Min.Y + 1
		}
		out = append(out, SelectionStroke{Rect: rect, Keep: s.Keep})
	}
	return out
}

func segmentsIntersect(a, b, c, d image.Point) bool {
	ccw := func(p1, p2, p3 image.Point) bool {
		return (p3.Y-p1.Y)*(p2.X-p1.X) > (p2.Y-p1.Y)*(p3.X-p1.X)
	}
	return (ccw(a, c, d) != ccw(b, c, d)) && (ccw(a, b, c) != ccw(a, b, d))
}

type TileMetrics struct {
	MeanLum         float64
	Variance        float64
	LaplacianVar    float64
	ForegroundRatio float64
	IsFiltered      bool
	FilterReason    string
}

type TileConfig struct {
	Mode                SliceMode
	TileWidth           int
	TileHeight          int
	OverlapX            int
	OverlapY            int
	GridRows            int
	GridCols            int
	EdgeMode            EdgeMode
	FilterBlank         bool
	BlankThreshold      float64
	FilterVariance      bool
	MinVariance         float64
	FilterLaplacianBlur bool
	MinLaplacianVar     float64
	FilterForeground    bool
	MinForegroundRatio  float64
	ROI                 image.Rectangle
	OnlyInROI           bool
	UsePolygonROI       bool
	PolygonROI          Polygon
	StrokesByFile       map[string][]SelectionStroke
	OutputFormat        string
	JpegQuality         int
	MultiScales         []float64
}

type TileRecord struct {
	SourceFile string  `json:"source_file"`
	TileFile   string  `json:"tile_file"`
	Scale      float64 `json:"scale,omitempty"`
	Row        int     `json:"row"`
	Col        int     `json:"col"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	OrigWidth  int     `json:"orig_width"`
	OrigHeight int     `json:"orig_height"`
}

type SliceManifest struct {
	CreatedAt    string       `json:"created_at"`
	SourceDir    string       `json:"source_dir"`
	OutputDir    string       `json:"output_dir"`
	TotalTiles   int          `json:"total_tiles"`
	TotalImages  int          `json:"total_images"`
	FailedTiles  int          `json:"failed_tiles,omitempty"`
	FailedImages int          `json:"failed_images,omitempty"`
	Failures     []string     `json:"failures,omitempty"`
	Tiles        []TileRecord `json:"tiles"`
}

type CropRect struct {
	Row    int
	Col    int
	Bounds image.Rectangle
}

type ProgressStats struct {
	TotalFiles     int
	ProcessedFiles int32
	TotalTiles     int32
	FailedFiles    int32
	FailedTiles    int32
	CurrentFile    string
	IsRunning      bool
	ErrorMessage   string
	Failures       []string
	mu             sync.Mutex
}

func (p *ProgressStats) AddFailure(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Failures = append(p.Failures, msg)
}

func (p *ProgressStats) GetFailures() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	copied := make([]string, len(p.Failures))
	copy(copied, p.Failures)
	return copied
}

func calculateCrops(imgW, imgH int, cfg TileConfig) []CropRect {
	var crops []CropRect

	if cfg.Mode == ModeManualROI {
		roi := cfg.ROI
		if roi.Empty() {
			roi = image.Rect(0, 0, imgW, imgH)
		} else {
			roi = roi.Intersect(image.Rect(0, 0, imgW, imgH))
		}
		if !roi.Empty() {
			crops = append(crops, CropRect{
				Row:    0,
				Col:    0,
				Bounds: roi,
			})
		}
		return crops
	}

	if cfg.Mode == ModeGrid {
		rows := cfg.GridRows
		cols := cfg.GridCols
		if rows <= 0 {
			rows = 3
		}
		if cols <= 0 {
			cols = 3
		}
		cellW := imgW / cols
		cellH := imgH / rows
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				x1 := c * cellW
				y1 := r * cellH
				x2 := (c + 1) * cellW
				y2 := (r + 1) * cellH
				if c == cols-1 {
					x2 = imgW
				}
				if r == rows-1 {
					y2 = imgH
				}
				crops = append(crops, CropRect{
					Row:    r,
					Col:    c,
					Bounds: image.Rect(x1, y1, x2, y2),
				})
			}
		}
		return crops
	}

	// Sliding window
	tileW := cfg.TileWidth
	tileH := cfg.TileHeight
	if tileW <= 0 {
		tileW = 512
	}
	if tileH <= 0 {
		tileH = 512
	}
	overlapX := cfg.OverlapX
	overlapY := cfg.OverlapY
	if overlapX >= tileW {
		overlapX = tileW / 2
	}
	if overlapY >= tileH {
		overlapY = tileH / 2
	}

	stepX := tileW - overlapX
	stepY := tileH - overlapY
	if stepX <= 0 {
		stepX = 1
	}
	if stepY <= 0 {
		stepY = 1
	}

	var yCoords []int
	for y := 0; y < imgH; y += stepY {
		if y+tileH > imgH {
			if cfg.EdgeMode == EdgeShiftBack {
				shifted := imgH - tileH
				if shifted < 0 {
					shifted = 0
				}
				if len(yCoords) == 0 || yCoords[len(yCoords)-1] != shifted {
					yCoords = append(yCoords, shifted)
				}
			} else if cfg.EdgeMode == EdgeDiscard {
				// skip partial edge
			} else {
				yCoords = append(yCoords, y)
			}
			break
		}
		yCoords = append(yCoords, y)
	}

	var xCoords []int
	for x := 0; x < imgW; x += stepX {
		if x+tileW > imgW {
			if cfg.EdgeMode == EdgeShiftBack {
				shifted := imgW - tileW
				if shifted < 0 {
					shifted = 0
				}
				if len(xCoords) == 0 || xCoords[len(xCoords)-1] != shifted {
					xCoords = append(xCoords, shifted)
				}
			} else if cfg.EdgeMode == EdgeDiscard {
				// skip partial edge
			} else {
				xCoords = append(xCoords, x)
			}
			break
		}
		xCoords = append(xCoords, x)
	}

	for r, y := range yCoords {
		for c, x := range xCoords {
			x2 := x + tileW
			y2 := y + tileH
			if x2 > imgW {
				x2 = imgW
			}
			if y2 > imgH {
				y2 = imgH
			}
			crops = append(crops, CropRect{
				Row:    r,
				Col:    c,
				Bounds: image.Rect(x, y, x2, y2),
			})
		}
	}

	return filterCropsByROI(crops, cfg)
}

func filterCropsByROI(crops []CropRect, cfg TileConfig) []CropRect {
	if !cfg.OnlyInROI {
		return crops
	}

	var filtered []CropRect
	if cfg.UsePolygonROI && !cfg.PolygonROI.Empty() {
		for _, c := range crops {
			if cfg.PolygonROI.OverlapsRect(c.Bounds) {
				filtered = append(filtered, c)
			}
		}
		return filtered
	}

	if !cfg.ROI.Empty() {
		for _, c := range crops {
			if c.Bounds.Overlaps(cfg.ROI) {
				filtered = append(filtered, c)
			}
		}
		return filtered
	}

	return crops
}

type fastPixelReader struct {
	rgba  *image.RGBA
	nrgba *image.NRGBA
	gray  *image.Gray
	ycbcr *image.YCbCr
	img   image.Image
}

func newFastPixelReader(img image.Image) fastPixelReader {
	r := fastPixelReader{img: img}
	switch v := img.(type) {
	case *image.RGBA:
		r.rgba = v
	case *image.NRGBA:
		r.nrgba = v
	case *image.Gray:
		r.gray = v
	case *image.YCbCr:
		r.ycbcr = v
	}
	return r
}

func (r *fastPixelReader) lum(x, y int) float64 {
	if r.rgba != nil {
		i := y*r.rgba.Stride + x*4
		return (0.299*float64(r.rgba.Pix[i]) + 0.587*float64(r.rgba.Pix[i+1]) + 0.114*float64(r.rgba.Pix[i+2]))
	}
	if r.nrgba != nil {
		i := y*r.nrgba.Stride + x*4
		return (0.299*float64(r.nrgba.Pix[i]) + 0.587*float64(r.nrgba.Pix[i+1]) + 0.114*float64(r.nrgba.Pix[i+2]))
	}
	if r.gray != nil {
		i := y*r.gray.Stride + x
		return float64(r.gray.Pix[i])
	}
	if r.ycbcr != nil {
		i := r.ycbcr.YOffset(x, y)
		return float64(r.ycbcr.Y[i])
	}
	c := r.img.At(x, y)
	cr, cg, cb, _ := c.RGBA()
	return (0.299*float64(cr) + 0.587*float64(cg) + 0.114*float64(cb)) / 257.0
}

var metricBufferPool = sync.Pool{
	New: func() any {
		return make([]float64, 3*4096)
	},
}

func computeTileMetrics(img image.Image, rect image.Rectangle, cfg TileConfig) TileMetrics {
	var metrics TileMetrics
	w := rect.Dx()
	h := rect.Dy()
	if w <= 0 || h <= 0 {
		metrics.IsFiltered = true
		metrics.FilterReason = "空白或非法区域"
		return metrics
	}

	count := w * h
	if count <= 0 {
		metrics.IsFiltered = true
		metrics.FilterReason = "样本量为零"
		return metrics
	}

	buf := metricBufferPool.Get().([]float64)
	needed := 3 * w
	if len(buf) < needed {
		buf = make([]float64, needed*2)
	}
	r0 := buf[0:w]
	r1 := buf[w : 2*w]
	r2 := buf[2*w : 3*w]

	prevRow := r0
	currRow := r1
	nextRow := r2

	reader := newFastPixelReader(img)

	// Pre-fill row 0 into currRow
	for x := 0; x < w; x++ {
		currRow[x] = reader.lum(rect.Min.X+x, rect.Min.Y)
	}

	var hist [256]int
	var totalLum float64
	var totalSqLum float64
	var blankCount int

	var lapSum float64
	var lapSqSum float64
	var lapCount int

	for y := 0; y < h; y++ {
		imgY := rect.Min.Y + y
		if y < h-1 {
			nextY := imgY + 1
			for x := 0; x < w; x++ {
				nextRow[x] = reader.lum(rect.Min.X+x, nextY)
			}
		}

		for x := 0; x < w; x++ {
			lum := currRow[x]
			totalLum += lum
			totalSqLum += lum * lum
			if lum > 240.0 || lum < 15.0 {
				blankCount++
			}
			idx := int(lum)
			if idx < 0 {
				idx = 0
			} else if idx > 255 {
				idx = 255
			}
			hist[idx]++

			if y > 0 && y < h-1 && x > 0 && x < w-1 {
				lap := prevRow[x] + nextRow[x] + currRow[x-1] + currRow[x+1] - 4*currRow[x]
				lapSum += lap
				lapSqSum += lap * lap
				lapCount++
			}
		}

		prevRow, currRow, nextRow = currRow, nextRow, prevRow
	}

	metricBufferPool.Put(buf)

	meanLum := totalLum / float64(count)
	metrics.MeanLum = meanLum

	varLum := (totalSqLum - (totalLum*totalLum)/float64(count)) / float64(count)
	if varLum < 0 {
		varLum = 0
	}
	metrics.Variance = math.Sqrt(varLum)

	if lapCount > 1 {
		lapVar := (lapSqSum - (lapSum*lapSum)/float64(lapCount)) / float64(lapCount)
		if lapVar < 0 {
			lapVar = 0
		}
		metrics.LaplacianVar = lapVar
	}

	otsuThresh := computeOtsuThreshold(&hist, count)
	fgCount := 0
	if meanLum > 128 {
		for i := 0; i <= otsuThresh; i++ {
			fgCount += hist[i]
		}
	} else {
		for i := otsuThresh; i < 256; i++ {
			fgCount += hist[i]
		}
	}
	metrics.ForegroundRatio = float64(fgCount) / float64(count)

	if cfg.FilterBlank {
		ratio := float64(blankCount) / float64(count)
		if ratio >= cfg.BlankThreshold {
			metrics.IsFiltered = true
			metrics.FilterReason = fmt.Sprintf("单色背景过高 (%.0f%%)", ratio*100)
			return metrics
		}
	}

	if cfg.FilterVariance && metrics.Variance < cfg.MinVariance {
		metrics.IsFiltered = true
		metrics.FilterReason = fmt.Sprintf("平坦无纹理 (标准差 %.1f < %.1f)", metrics.Variance, cfg.MinVariance)
		return metrics
	}

	if cfg.FilterLaplacianBlur && metrics.LaplacianVar < cfg.MinLaplacianVar {
		metrics.IsFiltered = true
		metrics.FilterReason = fmt.Sprintf("对焦虚焦模糊 (清晰度 %.1f < %.1f)", metrics.LaplacianVar, cfg.MinLaplacianVar)
		return metrics
	}

	if cfg.FilterForeground && metrics.ForegroundRatio < cfg.MinForegroundRatio {
		metrics.IsFiltered = true
		metrics.FilterReason = fmt.Sprintf("组织占比不足 (%.1f%% < %.1f%%)", metrics.ForegroundRatio*100, cfg.MinForegroundRatio*100)
		return metrics
	}

	return metrics
}

func computeOtsuThreshold(hist *[256]int, total int) int {
	var sum float64
	for i := 0; i < 256; i++ {
		sum += float64(i * hist[i])
	}
	var sumB float64
	var wB int
	var maxVariance float64
	threshold := 128

	for i := 0; i < 256; i++ {
		wB += hist[i]
		if wB == 0 {
			continue
		}
		wF := total - wB
		if wF == 0 {
			break
		}
		sumB += float64(i * hist[i])
		mB := sumB / float64(wB)
		mF := (sum - sumB) / float64(wF)
		varianceBetween := float64(wB) * float64(wF) * (mB - mF) * (mB - mF)
		if varianceBetween > maxVariance {
			maxVariance = varianceBetween
			threshold = i
		}
	}
	return threshold
}

func isBlankOrUniform(img image.Image, rect image.Rectangle, cfg TileConfig) bool {
	return computeTileMetrics(img, rect, cfg).IsFiltered
}

type subImager interface {
	SubImage(r image.Rectangle) image.Image
}

func cropSubImage(src image.Image, rect image.Rectangle, targetW, targetH int, pad bool) image.Image {
	actualW := rect.Dx()
	actualH := rect.Dy()

	if !pad || (actualW == targetW && actualH == targetH) {
		if si, ok := src.(subImager); ok {
			return si.SubImage(rect)
		}
		dst := image.NewRGBA(image.Rect(0, 0, actualW, actualH))
		draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
		return dst
	}

	// Padding to target dimensions with black background
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(0, 0, actualW, actualH), src, rect.Min, draw.Over)
	return dst
}

func saveImageFile(path string, img image.Image, format string, quality int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if strings.ToLower(format) == "jpg" || strings.ToLower(format) == "jpeg" {
		q := quality
		if q <= 0 || q > 100 {
			q = 90
		}
		return jpeg.Encode(f, img, &jpeg.Options{Quality: q})
	}
	return png.Encode(f, img)
}

func downscaleImage(src image.Image, scale float64) image.Image {
	if scale >= 0.999 && scale <= 1.001 {
		return src
	}
	srcB := src.Bounds()
	dstW := int(float64(srcB.Dx()) * scale)
	dstH := int(float64(srcB.Dy()) * scale)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, srcB, xdraw.Over, nil)
	return dst
}

func ExecuteBatchSlicing(
	ctx context.Context,
	inputDir, outputDir string,
	cfg TileConfig,
	progress *ProgressStats,
) error {
	progress.IsRunning = true
	progress.ProcessedFiles = 0
	progress.TotalTiles = 0
	progress.ErrorMessage = ""
	defer func() { progress.IsRunning = false }()

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		progress.ErrorMessage = fmt.Sprintf("创建输出目录失败: %v", err)
		return err
	}

	var files []string
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		progress.ErrorMessage = fmt.Sprintf("读取输入目录失败: %v", err)
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".tif" || ext == ".tiff" || ext == ".bmp" || ext == ".webp" {
			files = append(files, filepath.Join(inputDir, entry.Name()))
		}
	}

	if len(files) == 0 {
		progress.ErrorMessage = "输入目录中未找到支持的图像文件"
		return fmt.Errorf("no supported image files found")
	}

	progress.TotalFiles = len(files)

	var mu sync.Mutex
	var allRecords []TileRecord

	numWorkers := runtime.NumCPU()
	if numWorkers > 8 {
		numWorkers = 8
	}
	if numWorkers < 2 {
		numWorkers = 2
	}

	scales := cfg.MultiScales
	if len(scales) == 0 {
		scales = []float64{1.0}
	}

	for _, path := range files {
		select {
		case <-ctx.Done():
			progress.ErrorMessage = "批处理任务已被用户取消"
			return ctx.Err()
		default:
		}

		progress.CurrentFile = filepath.Base(path)

		imgFile, err := os.Open(longPath(path))
		if err != nil {
			atomic.AddInt32(&progress.ProcessedFiles, 1)
			atomic.AddInt32(&progress.FailedFiles, 1)
			progress.AddFailure(fmt.Sprintf("文件打开失败: %s (%v)", filepath.Base(path), err))
			continue
		}
		srcImg, _, err := image.Decode(imgFile)
		imgFile.Close()
		if err != nil {
			atomic.AddInt32(&progress.ProcessedFiles, 1)
			atomic.AddInt32(&progress.FailedFiles, 1)
			progress.AddFailure(fmt.Sprintf("文件解码失败: %s (%v)", filepath.Base(path), err))
			continue
		}

		bounds := srcImg.Bounds()
		imgW := bounds.Dx()
		imgH := bounds.Dy()
		baseName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		ext := ".png"
		if strings.ToLower(cfg.OutputFormat) == "jpg" {
			ext = ".jpg"
		}

		// 框选笔迹按文件名归属，只对当前文件生效，坐标为原图像素
		fileStrokes := cfg.StrokesByFile[filepath.Base(path)]

		for _, sc := range scales {
			select {
			case <-ctx.Done():
				progress.ErrorMessage = "批处理任务已被用户取消"
				return ctx.Err()
			default:
			}

			var targetDir string
			var tilePrefix string
			if len(scales) > 1 {
				scaleDirName := fmt.Sprintf("scale_%.2fx", sc)
				targetDir = filepath.Join(outputDir, scaleDirName)
				tilePrefix = fmt.Sprintf("%s_scale%.2fx", baseName, sc)
			} else {
				targetDir = outputDir
				tilePrefix = baseName
			}
			_ = os.MkdirAll(targetDir, 0755)

			workImg := downscaleImage(srcImg, sc)
			workBounds := workImg.Bounds()
			workW := workBounds.Dx()
			workH := workBounds.Dy()

			scaleCfg := cfg
			if sc < 0.999 || sc > 1.001 {
				if !scaleCfg.ROI.Empty() {
					scaleCfg.ROI = image.Rect(
						int(float64(cfg.ROI.Min.X)*sc),
						int(float64(cfg.ROI.Min.Y)*sc),
						int(float64(cfg.ROI.Max.X)*sc),
						int(float64(cfg.ROI.Max.Y)*sc),
					)
				}
				if len(scaleCfg.PolygonROI) > 0 {
					scaledPoly := make(Polygon, len(cfg.PolygonROI))
					for pi, pt := range cfg.PolygonROI {
						scaledPoly[pi] = image.Point{
							X: int(float64(pt.X) * sc),
							Y: int(float64(pt.Y) * sc),
						}
					}
					scaleCfg.PolygonROI = scaledPoly
				}
			}

			strokes := ScaleStrokes(fileStrokes, sc)

			crops := calculateCrops(workW, workH, scaleCfg)
			cropChan := make(chan CropRect, len(crops))
			for _, crop := range crops {
				cropChan <- crop
			}
			close(cropChan)

			var tileWg sync.WaitGroup
			var localMu sync.Mutex
			var localRecords []TileRecord

			for w := 0; w < numWorkers; w++ {
				tileWg.Add(1)
				go func() {
					defer tileWg.Done()
					for crop := range cropChan {
						select {
						case <-ctx.Done():
							return
						default:
						}

						// 框选命中的切片按最后一笔的去留处理，未命中的交给质量过滤等既有规则
						if !StrokeDecision(strokes, crop.Bounds) {
							continue
						}

						if isBlankOrUniform(workImg, crop.Bounds, scaleCfg) {
							continue
						}

						padEdge := scaleCfg.EdgeMode == EdgePad
						tileImg := cropSubImage(workImg, crop.Bounds, scaleCfg.TileWidth, scaleCfg.TileHeight, padEdge)

						tileFileName := fmt.Sprintf("%s_r%d_c%d_x%d_y%d%s", tilePrefix, crop.Row, crop.Col, crop.Bounds.Min.X, crop.Bounds.Min.Y, ext)
						tileDestPath := filepath.Join(targetDir, tileFileName)
						if err := saveImageFile(tileDestPath, tileImg, scaleCfg.OutputFormat, scaleCfg.JpegQuality); err != nil {
							atomic.AddInt32(&progress.FailedTiles, 1)
							progress.AddFailure(fmt.Sprintf("切片写盘失败: %s (%v)", tileFileName, err))
							continue
						}

						relTilePath := tileFileName
						if len(scales) > 1 {
							relTilePath = filepath.Join(fmt.Sprintf("scale_%.2fx", sc), tileFileName)
						}

						rec := TileRecord{
							SourceFile: filepath.Base(path),
							TileFile:   relTilePath,
							Scale:      sc,
							Row:        crop.Row,
							Col:        crop.Col,
							X:          crop.Bounds.Min.X,
							Y:          crop.Bounds.Min.Y,
							Width:      crop.Bounds.Dx(),
							Height:     crop.Bounds.Dy(),
							OrigWidth:  imgW,
							OrigHeight: imgH,
						}
						localMu.Lock()
						localRecords = append(localRecords, rec)
						localMu.Unlock()
						atomic.AddInt32(&progress.TotalTiles, 1)
					}
				}()
			}
			tileWg.Wait()

			mu.Lock()
			allRecords = append(allRecords, localRecords...)
			mu.Unlock()
		}

		srcImg = nil
		atomic.AddInt32(&progress.ProcessedFiles, 1)
	}

	if ctx.Err() != nil {
		progress.ErrorMessage = "批处理任务已被用户取消"
		return ctx.Err()
	}

	manifest := SliceManifest{
		CreatedAt:    time.Now().Format("2006-01-02 15:04:05"),
		SourceDir:    inputDir,
		OutputDir:    outputDir,
		TotalTiles:   len(allRecords),
		TotalImages:  len(files),
		FailedTiles:  int(atomic.LoadInt32(&progress.FailedTiles)),
		FailedImages: int(atomic.LoadInt32(&progress.FailedFiles)),
		Failures:     progress.GetFailures(),
		Tiles:        allRecords,
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		progress.ErrorMessage = fmt.Sprintf("序列化清单失败: %v", err)
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifestData, 0644); err != nil {
		progress.ErrorMessage = fmt.Sprintf("写入清单失败: %v", err)
		return err
	}

	return nil
}

func ExecuteStitching(
	ctx context.Context,
	manifestPath, outputDir string,
	progress *ProgressStats,
) error {
	progress.IsRunning = true
	progress.ErrorMessage = ""
	defer func() { progress.IsRunning = false }()

	data, err := os.ReadFile(longPath(manifestPath))
	if err != nil {
		progress.ErrorMessage = fmt.Sprintf("读取清单文件失败: %v", err)
		return err
	}

	var manifest SliceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		progress.ErrorMessage = fmt.Sprintf("解析清单格式失败: %v", err)
		return err
	}

	if len(manifest.Tiles) == 0 {
		progress.ErrorMessage = "清单中没有切块记录"
		return fmt.Errorf("empty manifest")
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		progress.ErrorMessage = fmt.Sprintf("创建输出目录失败: %v", err)
		return err
	}

	// Group tiles by source file
	groups := make(map[string][]TileRecord)
	for _, t := range manifest.Tiles {
		groups[t.SourceFile] = append(groups[t.SourceFile], t)
	}

	tileBaseDir := filepath.Dir(manifestPath)
	progress.TotalFiles = len(groups)
	progress.ProcessedFiles = 0

	for sourceName, records := range groups {
		select {
		case <-ctx.Done():
			progress.ErrorMessage = "拼接任务已被取消"
			return ctx.Err()
		default:
		}

		progress.CurrentFile = sourceName
		if len(records) == 0 {
			continue
		}

		origW := records[0].OrigWidth
		origH := records[0].OrigHeight
		canvas := image.NewRGBA(image.Rect(0, 0, origW, origH))

		for _, rec := range records {
			tilePath := filepath.Join(tileBaseDir, rec.TileFile)
			f, err := os.Open(longPath(tilePath))
			if err != nil {
				continue
			}
			tileImg, _, err := image.Decode(f)
			f.Close()
			if err != nil {
				continue
			}

			destRect := image.Rect(rec.X, rec.Y, rec.X+rec.Width, rec.Y+rec.Height)
			draw.Draw(canvas, destRect, tileImg, image.Point{}, draw.Over)
		}

		outFileName := "stitched_" + sourceName
		if !strings.HasSuffix(strings.ToLower(outFileName), ".png") {
			outFileName = strings.TrimSuffix(outFileName, filepath.Ext(outFileName)) + ".png"
		}
		destPath := filepath.Join(outputDir, outFileName)
		if err := saveImageFile(destPath, canvas, "png", 100); err != nil {
			progress.ErrorMessage = fmt.Sprintf("保存拼接图像失败: %v", err)
			return err
		}

		atomic.AddInt32(&progress.ProcessedFiles, 1)
	}

	return nil
}

// ExecuteCropSingleImage crops a specified ROI from a single image and saves it to outDir.
func ExecuteCropSingleImage(srcPath, outDir string, roi image.Rectangle, format string, quality int) (string, error) {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", err
	}
	f, err := os.Open(longPath(srcPath))
	if err != nil {
		return "", err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return "", err
	}

	b := img.Bounds()
	if roi.Empty() {
		roi = b
	} else {
		roi = roi.Intersect(b)
	}
	if roi.Empty() {
		return "", fmt.Errorf("roi rectangle is empty or outside image bounds")
	}

	targetW := roi.Dx()
	targetH := roi.Dy()
	cropped := cropSubImage(img, roi, targetW, targetH, false)

	ext := "." + strings.ToLower(format)
	if ext == "." {
		ext = ".png"
	}
	base := filepath.Base(srcPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	outName := fmt.Sprintf("%s_crop_%d_%d_%dx%d%s", stem, roi.Min.X, roi.Min.Y, targetW, targetH, ext)
	outPath := filepath.Join(outDir, outName)

	if err := saveImageFile(outPath, cropped, format, quality); err != nil {
		return "", err
	}
	return outPath, nil
}
