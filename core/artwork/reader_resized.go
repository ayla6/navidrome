package artwork

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/ayla6/avif"
	_ "github.com/ayla6/avif"
	"github.com/gen2brain/jpegxl"
	_ "github.com/gen2brain/jpegxl"
	"github.com/gen2brain/webp"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type resizedArtworkReader struct {
	artID      model.ArtworkID
	cacheKey   string
	lastUpdate time.Time
	size       int
	square     bool
	a          *artwork
}

func resizedFromOriginal(ctx context.Context, a *artwork, artID model.ArtworkID, size int, square bool) (*resizedArtworkReader, error) {
	r := &resizedArtworkReader{a: a}
	r.artID = artID
	r.size = size
	r.square = square

	original, err := a.getArtworkReader(ctx, artID, 0, false)
	if err != nil {
		return nil, err
	}
	r.cacheKey = original.Key()
	r.lastUpdate = original.LastUpdated()
	return r, nil
}

func (a *resizedArtworkReader) Key() string {
	baseKey := fmt.Sprintf("%s.%d", a.cacheKey, a.size)
	if a.square {
		return baseKey + ".square"
	}
	return fmt.Sprintf("%s.%s", baseKey, conf.Server.CoverArtFormat)
}

func (a *resizedArtworkReader) LastUpdated() time.Time {
	return a.lastUpdate
}

func (a *resizedArtworkReader) Reader(ctx context.Context) (io.ReadCloser, string, error) {
	orig, _, err := a.a.Get(ctx, a.artID, 0, false)
	if err != nil {
		return nil, "", err
	}
	defer orig.Close()

	resized, origSize, err := a.resizeImage(ctx, orig)
	if resized == nil {
		log.Trace(ctx, "Image smaller than requested size", "artID", a.artID, "original", origSize, "resized", a.size, "square", a.square)
	} else {
		log.Trace(ctx, "Resizing artwork", "artID", a.artID, "original", origSize, "resized", a.size, "square", a.square)
	}
	if err != nil {
		log.Warn(ctx, "Could not resize image. Will return image as is", "artID", a.artID, "size", a.size, "square", a.square, err)
	}
	if err != nil || resized == nil {
		orig, _, err = a.a.Get(ctx, a.artID, 0, false)
		return orig, "", err
	}
	if rc, ok := resized.(io.ReadCloser); ok {
		return rc, fmt.Sprintf("%s@%d", a.artID, a.size), nil
	}
	return io.NopCloser(resized), fmt.Sprintf("%s@%d", a.artID, a.size), nil
}

func (a *resizedArtworkReader) resizeImage(ctx context.Context, reader io.Reader) (io.Reader, int, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("reading image data: %w", err)
	}

	if !a.square {
		if isAnimatedGIF(data) {
			if a.a.ffmpeg.IsAvailable() {
				r, err := a.a.ffmpeg.ConvertAnimatedImage(ctx, bytes.NewReader(data), a.size, conf.Server.CoverArtQuality)
				if err == nil {
					return r, 0, nil
				}
				log.Warn(ctx, "Could not convert animated GIF, falling back to static", err)
			}
		} else if isAnimatedWebP(data) || isAnimatedPNG(data) {
			return bytes.NewReader(data), 0, nil
		}
	}

	return resizeStaticImage(data, a.size, a.square)
}

func shouldEncodeLossless(format string, originalBytes int, bounds image.Rectangle, header []byte) bool {
	totalPixels := bounds.Dx() * bounds.Dy()
	if totalPixels == 0 {
		return false
	}
	bpp := float64(originalBytes*8) / float64(totalPixels)

	isNativeLossless := false
	switch format {
	case "png":
		isNativeLossless = true
	case "jpeg":
		return false
	case "webp":
		if len(header) >= 12 && string(header[0:4]) == "RIFF" && string(header[8:12]) == "WEBP" {
			offset := 12
			for offset+8 <= len(header) {
				chunkID := string(header[offset : offset+4])
				if chunkID == "VP8L" {
					isNativeLossless = true
					break
				}
				if chunkID == "VP8 " {
					break
				}
				chunkLen := binary.LittleEndian.Uint32(header[offset+4 : offset+8])
				advance := 8 + int(chunkLen)
				if chunkLen%2 != 0 {
					advance++
				}
				offset += advance
			}
		}
	}

	if isNativeLossless && bpp < 8.0 {
		return true
	}
	return bpp < 3.0
}

func qualityBySize(n int, m int) int {
	if n < 300 {
		return conf.Server.CoverArtMinQuality
	}
	if n >= m {
		return conf.Server.CoverArtMaxQuality
	}
	ratio := float64(n-300) / float64(m-300)
	minQ := float64(conf.Server.CoverArtMinQuality)
	maxQ := float64(conf.Server.CoverArtMaxQuality)
	return int(minQ + ratio*(maxQ-minQ))
}

func resizeStaticImage(data []byte, size int, square bool) (io.Reader, int, error) {
	header := data
	if len(header) > 512 {
		header = header[:512]
	}

	original, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	originalBytes := len(data)

	bounds := original.Bounds()
	originalSize := max(bounds.Max.X, bounds.Max.Y)

	// Clamp to original — upscaling wastes resources and adds no information
	if size > originalSize {
		size = originalSize
	}

	if originalSize <= size && !square {
		return nil, originalSize, nil
	}

	srcW, srcH := bounds.Dx(), bounds.Dy()
	scale := float64(size) / float64(max(srcW, srcH))
	dstW := int(float64(srcW) * scale)
	dstH := int(float64(srcH) * scale)

	var dst *image.NRGBA
	var dstRect image.Rectangle
	if square {
		dst = image.NewNRGBA(image.Rect(0, 0, size, size))
		offsetX := (size - dstW) / 2
		offsetY := (size - dstH) / 2
		dstRect = image.Rect(offsetX, offsetY, offsetX+dstW, offsetY+dstH)
	} else {
		dst = image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
		dstRect = dst.Bounds()
	}
	xdraw.BiLinear.Scale(dst, dstRect, original, bounds, draw.Src, nil)

	encodeLossless := shouldEncodeLossless(format, originalBytes, bounds, header)

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	if encodeLossless {
		switch conf.Server.CoverArtFormat {
		case "jxl":
			err = jpegxl.Encode(buf, dst, jpegxl.Options{Quality: 100})
		case "png":
			err = png.Encode(buf, dst)
		default:
			err = webp.Encode(buf, dst, webp.Options{Quality: 100, Lossless: true})
		}
	} else {
		q := qualityBySize(size, originalSize)
		switch conf.Server.CoverArtFormat {
		case "webp":
			err = webp.Encode(buf, dst, webp.Options{Quality: q})
		case "jxl":
			err = jpegxl.Encode(buf, dst, jpegxl.Options{Quality: q})
		case "avif":
			err = avif.Encode(buf, dst, avif.Options{Quality: q, Advanced: map[string]string{
				"tune": "iq",
			}})
		default: // png and jpeg both fall back to jpeg for lossy
			err = jpeg.Encode(buf, dst, &jpeg.Options{Quality: q})
		}
	}

	// If we re-encoded to the same format and somehow made it bigger, just serve the original
	if format == conf.Server.CoverArtFormat && buf.Len() >= originalBytes {
		bufPool.Put(buf)
		return nil, originalSize, nil
	}

	if err != nil {
		bufPool.Put(buf)
		return nil, originalSize, err
	}

	encoded := make([]byte, buf.Len())
	copy(encoded, buf.Bytes())
	bufPool.Put(buf)
	return bytes.NewReader(encoded), originalSize, nil
}
