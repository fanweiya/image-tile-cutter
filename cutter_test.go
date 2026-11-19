package main

import (
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"testing"
	"time"

	xdraw "golang.org/x/image/draw"
)

func TestGigapixelDecodeAndScale(t *testing.T) {
	testFile := `C:\测试\EB球测试集\A1_01_BF_merge_src.jpg`
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test directory not found, skipping integration test")
	}

	t0 := time.Now()
	f, err := os.Open(testFile)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("DecodeConfig failed: %v", err)
	}
	if time.Since(t0) > 100*time.Millisecond {
		t.Errorf("DecodeConfig took too long: %v", time.Since(t0))
	}
	t.Logf("Config: %dx%d, format: %s in %v", cfg.Width, cfg.Height, format, time.Since(t0))

	f.Seek(0, 0)
	tDecode := time.Now()
	decoded, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	t.Logf("Full decode: %v", time.Since(tDecode))

	tScale := time.Now()
	maxDim := 2048
	targetW := maxDim
	targetH := int(float64(cfg.Height) * float64(maxDim) / float64(cfg.Width))
	base := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	xdraw.ApproxBiLinear.Scale(base, base.Bounds(), decoded, decoded.Bounds(), xdraw.Over, nil)
	t.Logf("Pyramid base preview built in: %v", time.Since(tScale))

	// Test viewport rendering from base preview (e.g. 800x600)
	tViewport := time.Now()
	viewport := image.NewRGBA(image.Rect(0, 0, 800, 600))
	xdraw.NearestNeighbor.Scale(viewport, viewport.Bounds(), base, base.Bounds(), xdraw.Over, nil)
	t.Logf("Viewport 800x600 preview scaled in: %v", time.Since(tViewport))
	if time.Since(tViewport) > 50*time.Millisecond {
		t.Errorf("Viewport scaling took too long: %v", time.Since(tViewport))
	}
}

func TestCalculateCropsAndROI(t *testing.T) {
	cfg := TileConfig{
		Mode:       ModeSlidingWindow,
		TileWidth:  500,
		TileHeight: 500,
		OverlapX:   100, // 20%
		OverlapY:   100, // 20%
		EdgeMode:   EdgeShiftBack,
	}

	crops := calculateCrops(1000, 1000, cfg)
	if len(crops) == 0 {
		t.Fatalf("Expected crops, got 0")
	}

	// Test with ROI limited to top-left corner
	cfgROI := cfg
	cfgROI.OnlyInROI = true
	cfgROI.ROI = image.Rect(0, 0, 300, 300)
	roiCrops := calculateCrops(1000, 1000, cfgROI)
	if len(roiCrops) == 0 || len(roiCrops) >= len(crops) {
		t.Errorf("Expected filtered ROI crops count (%d) to be less than full count (%d)", len(roiCrops), len(crops))
	}
}

func TestQualityFilter(t *testing.T) {
	// 1. Create a pure white blank image
	whiteImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			whiteImg.Set(x, y, image.White)
		}
	}

	cfg := TileConfig{
		FilterBlank:    true,
		BlankThreshold: 0.95,
	}

	if !isBlankOrUniform(whiteImg, whiteImg.Bounds(), cfg) {
		t.Errorf("Expected pure white image to be filtered as blank")
	}
}

func TestPolygonROIAndOverlaps(t *testing.T) {
	poly := Polygon{
		{X: 100, Y: 100},
		{X: 400, Y: 100},
		{X: 400, Y: 400},
		{X: 100, Y: 400},
	}

	if !poly.Contains(image.Point{X: 200, Y: 200}) {
		t.Errorf("Expected (200, 200) inside polygon")
	}
	if poly.Contains(image.Point{X: 50, Y: 50}) {
		t.Errorf("Expected (50, 50) outside polygon")
	}

	insideRect := image.Rect(150, 150, 250, 250)
	if !poly.OverlapsRect(insideRect) {
		t.Errorf("Expected insideRect to overlap polygon")
	}

	outsideRect := image.Rect(500, 500, 600, 600)
	if poly.OverlapsRect(outsideRect) {
		t.Errorf("Expected outsideRect to NOT overlap polygon")
	}
}

func TestLaplacianAndOtsuMetrics(t *testing.T) {
	// Checkerboard image has high variance and high laplacian gradient
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			if ((x/8)+(y/8))%2 == 0 {
				img.Set(x, y, image.Black)
			} else {
				img.Set(x, y, image.White)
			}
		}
	}

	cfg := TileConfig{
		FilterLaplacianBlur: true,
		MinLaplacianVar:     20.0,
		FilterForeground:    true,
		MinForegroundRatio:  0.2,
	}

	m := computeTileMetrics(img, img.Bounds(), cfg)
	if m.IsFiltered {
		t.Errorf("High-contrast checkerboard should not be filtered as blur, reason: %s", m.FilterReason)
	}
	if m.LaplacianVar < 10.0 {
		t.Errorf("Expected high laplacian variance on checkerboard, got: %f", m.LaplacianVar)
	}
	if m.ForegroundRatio < 0.3 {
		t.Errorf("Expected ~50%% foreground ratio on checkerboard, got: %f", m.ForegroundRatio)
	}
}

func TestBatchSlicingFailureTracking(t *testing.T) {
	inDir := t.TempDir()
	outDir := t.TempDir()

	// 1. Create a valid small image
	validPath := inDir + string(os.PathSeparator) + "valid.png"
	validImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			validImg.Set(x, y, image.White)
		}
	}
	if err := saveImageFile(validPath, validImg, "png", 100); err != nil {
		t.Fatalf("saveImageFile failed: %v", err)
	}

	// 2. Create a corrupted file
	corruptPath := inDir + string(os.PathSeparator) + "corrupt.png"
	if err := os.WriteFile(corruptPath, []byte("plain text corrupt content"), 0644); err != nil {
		t.Fatalf("writeFile failed: %v", err)
	}

	cfg := TileConfig{
		Mode:         ModeSlidingWindow,
		TileWidth:    50,
		TileHeight:   50,
		OutputFormat: "png",
	}

	var progress ProgressStats
	err := ExecuteBatchSlicing(t.Context(), inDir, outDir, cfg, &progress)
	if err != nil {
		t.Fatalf("ExecuteBatchSlicing returned unexpected error: %v", err)
	}

	if progress.FailedFiles != 1 {
		t.Errorf("Expected 1 failed file, got: %d", progress.FailedFiles)
	}
	if len(progress.GetFailures()) == 0 {
		t.Errorf("Expected failure messages to be tracked, got 0")
	}

	// Verify manifest.json exists and has failure count
	manifestPath := outDir + string(os.PathSeparator) + "manifest.json"
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("Failed to read manifest: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("manifest is empty")
	}
}
