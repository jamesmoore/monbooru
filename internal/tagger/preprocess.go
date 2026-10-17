//go:build tagger

package tagger

import (
	"cmp"
	"fmt"
	"image"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
)

var inputTensorPools sync.Map

// The pool holds *[]float32, not []float32: pooling the slice itself boxes
// its header into the interface on every Put, which allocates.
func acquireTensor(size int) []float32 {
	if p, ok := inputTensorPools.Load(size); ok {
		return *p.(*sync.Pool).Get().(*[]float32)
	}
	n := size
	fresh := &sync.Pool{New: func() any { buf := make([]float32, 3*n*n); return &buf }}
	actual, _ := inputTensorPools.LoadOrStore(size, fresh)
	return *actual.(*sync.Pool).Get().(*[]float32)
}

func releaseTensor(size int, buf []float32) {
	if p, ok := inputTensorPools.Load(size); ok {
		p.(*sync.Pool).Put(&buf)
	}
}

var (
	imageNetMean = [3]float32{0.485, 0.456, 0.406}
	imageNetStd  = [3]float32{0.229, 0.224, 0.225}
)

// OpenAI CLIP values from joytag's preprocess, kept verbatim to match its
// reference output.
var (
	clipMean = [3]float32{0.48145466, 0.4578275, 0.40821073}
	clipStd  = [3]float32{0.26862954, 0.26130258, 0.27577711}
)

// camieDefaultFill is the padding colour of Camie's onnx_inference.py.
var camieDefaultFill = [3]uint8{124, 116, 104}

func padFill(profile Profile) [3]uint8 {
	switch {
	case profile.Pad == "white_square":
		return [3]uint8{0xff, 0xff, 0xff}
	case profile.FillColor != nil:
		return *profile.FillColor
	}
	return camieDefaultFill
}

func padAndResize(src image.Image, size int, profile Profile) *image.RGBA {
	fill := padFill(profile)
	scaled, offX, offY := resizeAspect(src, size)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i+0] = fill[0]
		dst.Pix[i+1] = fill[1]
		dst.Pix[i+2] = fill[2]
		dst.Pix[i+3] = 0xFF
	}
	// Over composites a (half-)transparent pixel onto the fill, as WD14's
	// reference does onto white; buildTensor ignores alpha.
	draw.Draw(dst, scaled.Bounds().Add(image.Pt(offX, offY)), scaled, image.Point{}, draw.Over)
	return dst
}

// Resizing before padding bounds each transient buffer by size^2, which keeps
// a parallel burst on huge sources inside small container memory caps.
func resizeAspect(src image.Image, size int) (scaled *image.RGBA, offX, offY int) {
	b := src.Bounds()
	w, h := b.Max.X-b.Min.X, b.Max.Y-b.Min.Y
	scaleW, scaleH := size, size
	if w >= h {
		scaleH = max(1, h*size/w)
	} else {
		scaleW = max(1, w*size/h)
	}
	scaled = image.NewRGBA(image.Rect(0, 0, scaleW, scaleH))
	// Not ApproxBiLinear: it reads four source pixels at any ratio, and
	// the aliasing it leaves gets tagged as pixel art.
	draw.BiLinear.Scale(scaled, scaled.Bounds(), src, b, draw.Src, nil)
	return scaled, (size - scaleW) / 2, (size - scaleH) / 2
}

var normalizePresets = map[string]struct {
	scale     float32
	mean, std [3]float32
}{
	"none":     {1, [3]float32{}, [3]float32{1, 1, 1}},
	"div255":   {255, [3]float32{}, [3]float32{1, 1, 1}},
	"imagenet": {255, imageNetMean, imageNetStd},
	"clip":     {255, clipMean, clipStd},
}

// normalizeTable indexes by tensor channel and byte value. Each entry
// is computed with the reference expression, so a preset's tensor is
// bit-identical to its model's own preprocessing.
func normalizeTable(profile Profile) *[3][256]float32 {
	scale, mean, std := float32(1), [3]float32{}, [3]float32{1, 1, 1}
	if p, ok := normalizePresets[profile.Normalize]; ok {
		scale, mean, std = p.scale, p.mean, p.std
	} else if profile.NormMean != nil && profile.NormStd != nil {
		scale, mean, std = cmp.Or(profile.NormScale, 1), *profile.NormMean, *profile.NormStd
	}
	var t [3][256]float32
	for c := range 3 {
		for v := range 256 {
			t[c][v] = (float32(v)/scale - mean[c]) / std[c]
		}
	}
	return &t
}

// buildTensor overwrites every element: pooled buffers are not zeroed.
func buildTensor(img *image.RGBA, tensor []float32, size int, profile Profile) (ort.Shape, error) {
	pix := img.Pix
	stride := img.Stride
	norm := normalizeTable(profile)
	r, b := 0, 2
	if profile.Channels == "bgr" {
		r, b = 2, 0
	}

	switch profile.Layout {
	case "nhwc":
		for y := 0; y < size; y++ {
			row := pix[y*stride:]
			for x := 0; x < size; x++ {
				src := x * 4
				dst := (y*size + x) * 3
				tensor[dst+0] = norm[0][row[src+r]]
				tensor[dst+1] = norm[1][row[src+1]]
				tensor[dst+2] = norm[2][row[src+b]]
			}
		}
		return ort.NewShape(1, int64(size), int64(size), 3), nil

	case "nchw":
		plane := size * size
		for y := 0; y < size; y++ {
			row := pix[y*stride:]
			for x := 0; x < size; x++ {
				src := x * 4
				off := y*size + x
				tensor[0*plane+off] = norm[0][row[src+r]]
				tensor[1*plane+off] = norm[1][row[src+1]]
				tensor[2*plane+off] = norm[2][row[src+b]]
			}
		}
		return ort.NewShape(1, 3, int64(size), int64(size)), nil
	}
	return nil, fmt.Errorf("buildTensor: unsupported layout %q", profile.Layout)
}
