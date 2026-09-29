//go:build tagger

package tagger

import (
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

func padAndResize(src image.Image, size int, profile Profile) *image.RGBA {
	switch profile.Pad {
	case "mean_color_aspect":
		return padMeanColorAspect(src, size, profile.FillColor)
	}
	return padWhiteSquare(src, size)
}

// The canvas starts at alpha 0, so one pass over alpha turns both the
// padding and any transparent source pixel white.
func padWhiteSquare(src image.Image, size int) *image.RGBA {
	scaled, offX, offY := resizeAspect(src, size)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, scaled.Bounds().Add(image.Pt(offX, offY)), scaled, image.Point{}, draw.Src)
	for i := 3; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] == 0 {
			dst.Pix[i-3] = 0xFF
			dst.Pix[i-2] = 0xFF
			dst.Pix[i-1] = 0xFF
			dst.Pix[i] = 0xFF
		}
	}
	return dst
}

func padMeanColorAspect(src image.Image, size int, fill [3]uint8) *image.RGBA {
	if fill == ([3]uint8{}) {
		fill = camieDefaultFill
	}
	scaled, offX, offY := resizeAspect(src, size)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i+0] = fill[0]
		dst.Pix[i+1] = fill[1]
		dst.Pix[i+2] = fill[2]
		dst.Pix[i+3] = 0xFF
	}
	draw.Draw(dst, scaled.Bounds().Add(image.Pt(offX, offY)), scaled, image.Point{}, draw.Src)
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

// buildTensor overwrites every element: pooled buffers are not zeroed.
func buildTensor(img *image.RGBA, tensor []float32, size int, profile Profile) (ort.Shape, error) {
	pix := img.Pix
	stride := img.Stride

	switch profile.Layout {
	case "nhwc":
		if profile.Normalize != "" && profile.Normalize != "none" {
			return nil, fmt.Errorf("buildTensor: NHWC + normalize=%q is not implemented", profile.Normalize)
		}
		bgr := profile.Channels == "bgr"
		for y := 0; y < size; y++ {
			row := pix[y*stride:]
			for x := 0; x < size; x++ {
				src := x * 4
				dst := (y*size + x) * 3
				r, g, b := row[src+0], row[src+1], row[src+2]
				if bgr {
					tensor[dst+0] = float32(b)
					tensor[dst+1] = float32(g)
					tensor[dst+2] = float32(r)
				} else {
					tensor[dst+0] = float32(r)
					tensor[dst+1] = float32(g)
					tensor[dst+2] = float32(b)
				}
			}
		}
		return ort.NewShape(1, int64(size), int64(size), 3), nil

	case "nchw":
		var mean, std [3]float32
		switch profile.Normalize {
		case "imagenet":
			mean, std = imageNetMean, imageNetStd
		case "clip":
			mean, std = clipMean, clipStd
		}
		bgr := profile.Channels == "bgr"
		plane := size * size
		for y := 0; y < size; y++ {
			row := pix[y*stride:]
			for x := 0; x < size; x++ {
				src := x * 4
				off := y*size + x
				r := float32(row[src+0])
				g := float32(row[src+1])
				b := float32(row[src+2])
				if bgr {
					r, b = b, r
				}
				switch profile.Normalize {
				case "imagenet", "clip":
					tensor[0*plane+off] = (r/255 - mean[0]) / std[0]
					tensor[1*plane+off] = (g/255 - mean[1]) / std[1]
					tensor[2*plane+off] = (b/255 - mean[2]) / std[2]
				case "div255":
					tensor[0*plane+off] = r / 255
					tensor[1*plane+off] = g / 255
					tensor[2*plane+off] = b / 255
				default:
					tensor[0*plane+off] = r
					tensor[1*plane+off] = g
					tensor[2*plane+off] = b
				}
			}
		}
		return ort.NewShape(1, 3, int64(size), int64(size)), nil
	}
	return nil, fmt.Errorf("buildTensor: unsupported layout %q", profile.Layout)
}
