package artwork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	_ "golang.org/x/image/webp"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"time"

	"github.com/ayla6/avif"
	_ "github.com/ayla6/avif"
	"github.com/disintegration/imaging"
	"github.com/gen2brain/jpegxl"
	_ "github.com/gen2brain/jpegxl"
	"github.com/gen2brain/webp"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

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

	// Get lastUpdated and cacheKey from original artwork
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
	// Get artwork in original size, possibly from cache
	orig, _, err := a.a.Get(ctx, a.artID, 0, false)
	if err != nil {
		return nil, "", err
	}
	defer orig.Close()

	resized, origSize, err := resizeImage(orig, a.size, a.square)
	if resized == nil {
		log.Trace(ctx, "Image smaller than requested size", "artID", a.artID, "original", origSize, "resized", a.size, "square", a.square)
	} else {
		log.Trace(ctx, "Resizing artwork", "artID", a.artID, "original", origSize, "resized", a.size, "square", a.square)
	}
	if err != nil {
		log.Warn(ctx, "Could not resize image. Will return image as is", "artID", a.artID, "size", a.size, "square", a.square, err)
	}
	if err != nil || resized == nil {
		// if we couldn't resize the image, return the original
		orig, _, err = a.a.Get(ctx, a.artID, 0, false)
		return orig, "", err
	}
	return io.NopCloser(resized), fmt.Sprintf("%s@%d", a.artID, a.size), nil
}

func shouldEncodeLossless(format string, originalBytes int, bounds image.Rectangle, header []byte) bool {
	totalPixels := bounds.Dx() * bounds.Dy()
	if totalPixels == 0 {
		return false
	}
	bpp := float64(originalBytes*8) / float64(totalPixels)

	isNativeLossless := false
	if format == "png" {
		isNativeLossless = true
	} else if format == "jpeg" {
		return false
	} else if format == "webp" && len(header) >= 12 && string(header[0:4]) == "RIFF" && string(header[8:12]) == "WEBP" {
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

	if isNativeLossless && bpp < 8.0 {
		return true
	}
	if bpp < 3.0 {
		return true
	}

	return false
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func qualityBySize(n int, m int) int {
	if n < 300 {
		return conf.Server.CoverArtMinQuality
	}
	if n >= m {
		return conf.Server.CoverArtMaxQuality
	}

	top := float64(n - 300)
	bottom := float64(m - 300)

	ratio := top / bottom

	minQ := float64(conf.Server.CoverArtMinQuality)
	maxQ := float64(conf.Server.CoverArtMaxQuality)
	qualityFloat := minQ + (ratio * (maxQ - minQ))

	return int(qualityFloat)
}

func resizeImage(reader io.Reader, size int, square bool) (io.Reader, int, error) {
	br := bufio.NewReader(reader)

	header, _ := br.Peek(512)

	cr := &countingReader{r: br}
	original, format, err := image.Decode(cr)
	if err != nil {
		return nil, 0, err
	}

	originalBytes := cr.n
	imgSrcSameFormatAsServer := format == conf.Server.CoverArtFormat

	bounds := original.Bounds()
	originalSize := max(bounds.Max.X, bounds.Max.Y)

	if imgSrcSameFormatAsServer && originalSize <= size {
		return nil, originalSize, nil
	}

	var resized image.Image
	if originalSize <= size {
		resized = original
	} else {
		resized = imaging.Fit(original, size, size, imaging.Lanczos)
	}

	if square && bounds.Dx() != bounds.Dy() {
		bg := image.NewRGBA(image.Rect(0, 0, size, size))
		resized = imaging.OverlayCenter(bg, resized, 1)
	}

	encodeLossless := shouldEncodeLossless(format, originalBytes, bounds, header)

	buf := new(bytes.Buffer)

	if encodeLossless {
		switch conf.Server.CoverArtFormat {
		case "jxl":
			err = jpegxl.Encode(buf, resized, jpegxl.Options{Quality: 100})
		case "png":
			err = png.Encode(buf, resized)
		default:
			// if you wanna use shitty formats like png and jpeg pick png, it's gonna go with jpeg for lossy then. jpeg picks webp for lossless because ig some people would prefer how jpeg handles lossy images idk.
			// if you pick avif you also get webp for lossless because lossy avif is a joke
			err = webp.Encode(buf, resized, webp.Options{Quality: 100, Lossless: true})
		}
	} else {
		q := qualityBySize(size, min(size, originalSize))
		switch conf.Server.CoverArtFormat {
		case "webp":
			err = webp.Encode(buf, resized, webp.Options{Quality: q})
		case "jxl":
			err = jpegxl.Encode(buf, resized, jpegxl.Options{Quality: q})
		case "avif":
			err = avif.Encode(buf, resized, avif.Options{Quality: q, Advanced: map[string]string{
				"tune": "iq",
			}})
		default: // png and jpeg
			err = jpeg.Encode(buf, resized, &jpeg.Options{Quality: q})
		}
	}

	if imgSrcSameFormatAsServer && buf.Len() >= originalBytes {
		return nil, originalSize, nil
	}

	if err != nil {
		return buf, originalSize, err
	}

	return buf, originalSize, nil
}
