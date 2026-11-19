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

type TileConfig struct {
	Mode           SliceMode
	TileWidth      int
	TileHeight     int
	OverlapX       int
	OverlapY       int
	GridRows       int
	GridCols       int
	EdgeMode       EdgeMode
	FilterBlank    bool
	BlankThreshold float64
	FilterVariance bool
	MinVariance    float64
	ROI            image.Rectangle
	OnlyInROI      bool
	ExcludedTiles  map[string]bool
	OutputFormat   string
	JpegQuality    int
	MaskDir        string
}

type TileRecord struct {
	SourceFile string `json:"source_file"`
	TileFile   string `json:"tile_file"`
	MaskFile   string `json:"mask_file,omitempty"`
	Row        int    `json:"row"`
	Col        int    `json:"col"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	OrigWidth  int    `json:"orig_width"`
	OrigHeight int    `json:"orig_height"`
}

type SliceManifest struct {
	CreatedAt   string       `json:"created_at"`
	SourceDir   string       `json:"source_dir"`
	OutputDir   string       `json:"output_dir"`
	TotalTiles  int          `json:"total_tiles"`
	TotalImages int          `json:"total_images"`
	Tiles       []TileRecord `json:"tiles"`
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
	CurrentFile    string
	IsRunning      bool
	ErrorMessage   string
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

	if cfg.OnlyInROI && !cfg.ROI.Empty() {
		var filtered []CropRect
		for _, c := range crops {
			if c.Bounds.Overlaps(cfg.ROI) {
				filtered = append(filtered, c)
			}
		}
		crops = filtered
	}

	return crops
}

func isBlankOrUniform(img image.Image, rect image.Rectangle, cfg TileConfig) bool {
	if !cfg.FilterBlank && !cfg.FilterVariance {
		return false
	}

	var blankCount int
	var totalLum float64
	var count int
	var values []float64

	// Sample pixels with stride to optimize performance
	stride := 4
	for y := rect.Min.Y; y < rect.Max.Y; y += stride {
		for x := rect.Min.X; x < rect.Max.X; x += stride {
			c := img.At(x, y)
			r, g, b, _ := c.RGBA()
			lum := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 257.0
			totalLum += lum
			count++
			if lum > 240.0 || lum < 15.0 {
				blankCount++
			}
			if cfg.FilterVariance {
				values = append(values, lum)
			}
		}
	}

	if count == 0 {
		return true
	}

	if cfg.FilterBlank {
		ratio := float64(blankCount) / float64(count)
		if ratio >= cfg.BlankThreshold {
			return true
		}
	}

	if cfg.FilterVariance && len(values) > 1 {
		meanLum := totalLum / float64(count)
		var sumSq float64
		for _, v := range values {
			diff := v - meanLum
			sumSq += diff * diff
		}
		variance := sumSq / float64(len(values))
		if math.Sqrt(variance) < cfg.MinVariance {
			return true
		}
	}

	return false
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

func findPairedMask(imagePath, maskDir string) string {
	if maskDir == "" {
		return ""
	}
	base := filepath.Base(imagePath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	candidates := []string{
		filepath.Join(maskDir, base),
		filepath.Join(maskDir, stem+"_mask"+ext),
		filepath.Join(maskDir, stem+"_mask.png"),
		filepath.Join(maskDir, stem+"_mask.tif"),
		filepath.Join(maskDir, stem+".png"),
		filepath.Join(maskDir, stem+".tif"),
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
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

	// Process images sequentially to avoid heap memory exhaustion on gigapixel inputs,
	// while parallelizing tile cropping and encoding across all CPU cores.
	numWorkers := runtime.NumCPU()
	if numWorkers > 8 {
		numWorkers = 8
	}
	if numWorkers < 2 {
		numWorkers = 2
	}

	for _, path := range files {
		select {
		case <-ctx.Done():
			progress.ErrorMessage = "批处理任务已被用户取消"
			return ctx.Err()
		default:
		}

		progress.CurrentFile = filepath.Base(path)

		imgFile, err := os.Open(path)
		if err != nil {
			atomic.AddInt32(&progress.ProcessedFiles, 1)
			continue
		}
		srcImg, _, err := image.Decode(imgFile)
		imgFile.Close()
		if err != nil {
			atomic.AddInt32(&progress.ProcessedFiles, 1)
			continue
		}

		bounds := srcImg.Bounds()
		imgW := bounds.Dx()
		imgH := bounds.Dy()

		crops := calculateCrops(imgW, imgH, cfg)
		maskPath := findPairedMask(path, cfg.MaskDir)
		var maskImg image.Image
		if maskPath != "" {
			if mf, err := os.Open(maskPath); err == nil {
				maskImg, _, _ = image.Decode(mf)
				mf.Close()
			}
		}

		baseName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		ext := ".png"
		if strings.ToLower(cfg.OutputFormat) == "jpg" {
			ext = ".jpg"
		}

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

					if cfg.ExcludedTiles != nil && cfg.ExcludedTiles[fmt.Sprintf("%d_%d", crop.Row, crop.Col)] {
						continue
					}

					if isBlankOrUniform(srcImg, crop.Bounds, cfg) {
						continue
					}

					padEdge := cfg.EdgeMode == EdgePad
					tileImg := cropSubImage(srcImg, crop.Bounds, cfg.TileWidth, cfg.TileHeight, padEdge)

					tileFileName := fmt.Sprintf("%s_r%d_c%d_x%d_y%d%s", baseName, crop.Row, crop.Col, crop.Bounds.Min.X, crop.Bounds.Min.Y, ext)
					tileDestPath := filepath.Join(outputDir, tileFileName)
					if err := saveImageFile(tileDestPath, tileImg, cfg.OutputFormat, cfg.JpegQuality); err != nil {
						continue
					}

					var maskTileName string
					if maskImg != nil {
						maskTile := cropSubImage(maskImg, crop.Bounds, cfg.TileWidth, cfg.TileHeight, padEdge)
						maskTileName = fmt.Sprintf("%s_mask_r%d_c%d_x%d_y%d%s", baseName, crop.Row, crop.Col, crop.Bounds.Min.X, crop.Bounds.Min.Y, ext)
						_ = saveImageFile(filepath.Join(outputDir, maskTileName), maskTile, cfg.OutputFormat, cfg.JpegQuality)
					}

					rec := TileRecord{
						SourceFile: filepath.Base(path),
						TileFile:   tileFileName,
						MaskFile:   maskTileName,
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

		srcImg = nil
		maskImg = nil
		atomic.AddInt32(&progress.ProcessedFiles, 1)
	}

	if ctx.Err() != nil {
		progress.ErrorMessage = "批处理任务已被用户取消"
		return ctx.Err()
	}

	manifest := SliceManifest{
		CreatedAt:   time.Now().Format("2006-01-02 15:04:05"),
		SourceDir:   inputDir,
		OutputDir:   outputDir,
		TotalTiles:  len(allRecords),
		TotalImages: len(files),
		Tiles:       allRecords,
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifestData, 0644)
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

	data, err := os.ReadFile(manifestPath)
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
			f, err := os.Open(tilePath)
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
		_ = saveImageFile(destPath, canvas, "png", 100)

		atomic.AddInt32(&progress.ProcessedFiles, 1)
	}

	return nil
}

// ExecuteCropSingleImage crops a specified ROI from a single image and saves it to outDir.
func ExecuteCropSingleImage(srcPath, outDir string, roi image.Rectangle, format string, quality int) (string, error) {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", err
	}
	f, err := os.Open(srcPath)
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

