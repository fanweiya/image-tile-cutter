package main

import (
	"context"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
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
	if time.Since(tViewport) > 5*time.Millisecond {
		t.Errorf("Viewport scaling took too long: %v", time.Since(tViewport))
	}
}

func TestBatchSliceGigapixel(t *testing.T) {
	testFile := `C:\测试\EB球测试集\A2_52_BF_merge_src.jpg`
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("Test directory not found, skipping integration test")
	}

	tempOut := filepath.Join(".", ".test_output")
	_ = os.RemoveAll(tempOut)
	defer os.RemoveAll(tempOut)

	cfg := TileConfig{
		Mode:         ModeSlidingWindow,
		TileWidth:    512,
		TileHeight:   512,
		OverlapX:     64,
		OverlapY:     64,
		EdgeMode:     EdgeShiftBack,
		OutputFormat: "png",
	}

	progress := &ProgressStats{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := ExecuteBatchSlicing(ctx, `C:\测试\EB球测试集`, tempOut, cfg, progress)
	if err != nil {
		t.Fatalf("ExecuteBatchSlicing failed: %v", err)
	}
	t.Logf("Processed files: %d, Total tiles: %d", progress.ProcessedFiles, progress.TotalTiles)
	if progress.TotalTiles == 0 {
		t.Errorf("Expected tiles to be generated")
	}
}
