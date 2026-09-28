package gallery

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"sort"

	"github.com/monbooru/monbooru/internal/db"
)

// The standard pHash settings: a 32x32 greyscale input and an 8x8 DCT block.
const (
	phashSize  = 32
	phashBlock = 8
)

// The thumbnail, not the original: one input for every file type, and the
// pixels the grid shows.
func computePhashFromThumb(thumbPath string) (int64, error) {
	f, err := os.Open(thumbPath)
	if err != nil {
		return 0, fmt.Errorf("open thumb: %w", err)
	}
	defer func() { _ = f.Close() }()
	img, err := jpeg.Decode(f)
	if err != nil {
		return 0, fmt.Errorf("decode thumb: %w", err)
	}
	return int64(computePhash(img)), nil
}

// The smaller of the image's and its mirror's hash, so a flipped copy
// hashes the same.
func computePhash(img image.Image) uint64 {
	mat := greyResize32(img)
	h := dctHashMatrix(mat)
	var mirror [phashSize][phashSize]float64
	for r := 0; r < phashSize; r++ {
		for c := 0; c < phashSize; c++ {
			mirror[r][c] = mat[r][phashSize-1-c]
		}
	}
	if hm := dctHashMatrix(mirror); hm < h {
		return hm
	}
	return h
}

func greyResize32(img image.Image) [phashSize][phashSize]float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var out [phashSize][phashSize]float64
	if w == 0 || h == 0 {
		return out
	}
	for dy := 0; dy < phashSize; dy++ {
		y0 := dy * h / phashSize
		y1 := (dy + 1) * h / phashSize
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < phashSize; dx++ {
			x0 := dx * w / phashSize
			x1 := (dx + 1) * w / phashSize
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum float64
			var n int
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
					sum += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(bl>>8)
					n++
				}
			}
			out[dy][dx] = sum / float64(n)
		}
	}
	return out
}

func dctHashMatrix(in [phashSize][phashSize]float64) uint64 {
	var rowDCT [phashSize][phashBlock]float64
	for j := 0; j < phashSize; j++ {
		for k := 0; k < phashBlock; k++ {
			var s float64
			for n := 0; n < phashSize; n++ {
				s += in[j][n] * dctCosTable[n][k]
			}
			rowDCT[j][k] = s
		}
	}
	var block [phashBlock][phashBlock]float64
	for k := 0; k < phashBlock; k++ {
		for m := 0; m < phashBlock; m++ {
			var s float64
			for n := 0; n < phashSize; n++ {
				s += rowDCT[n][k] * dctCosTable[n][m]
			}
			block[m][k] = s
		}
	}
	coeffs := make([]float64, 0, phashBlock*phashBlock-1)
	for r := 0; r < phashBlock; r++ {
		for c := 0; c < phashBlock; c++ {
			if r == 0 && c == 0 {
				continue
			}
			coeffs = append(coeffs, block[r][c])
		}
	}
	sort.Float64s(coeffs)
	// 63 elements: median is the 32nd value, index 31.
	median := coeffs[len(coeffs)/2]
	var h uint64
	for r := 0; r < phashBlock; r++ {
		for c := 0; c < phashBlock; c++ {
			if r == 0 && c == 0 {
				continue
			}
			if block[r][c] >= median {
				h |= uint64(1) << uint(r*phashBlock+c)
			}
		}
	}
	return h
}

var dctCosTable [phashSize][phashBlock]float64

func init() {
	for n := 0; n < phashSize; n++ {
		for k := 0; k < phashBlock; k++ {
			dctCosTable[n][k] = math.Cos(float64(2*n+1) * float64(k) * math.Pi / float64(2*phashSize))
		}
	}
}

// A nil phash reports one cleared, so an index drops the old bytes' entry.
type PhashSink func(imageID int64, phash *int64)

func (s PhashSink) Stored(imageID int64, phash *int64) {
	if s != nil {
		s(imageID, phash)
	}
}

func RecomputeAndStorePhash(ctx context.Context, database *db.DB, imageID int64, thumbnailsPath string) (int64, error) {
	thumb := ThumbnailPath(thumbnailsPath, imageID)
	h, err := computePhashFromThumb(thumb)
	if err != nil {
		return 0, err
	}
	if _, err := database.Write.ExecContext(ctx, `UPDATE images SET phash = ? WHERE id = ?`, h, imageID); err != nil {
		return 0, fmt.Errorf("update phash: %w", err)
	}
	return h, nil
}
