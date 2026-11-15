// Package media normalizes prompt and tool images to configured dimensions and
// encoded size limits. It never changes non-image attachments.
package media

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	defaultMaxWidth       = 2000
	defaultMaxHeight      = 2000
	defaultMaxBase64Bytes = 5 << 20
)

// ImageConfig controls image processing. AutoResize is a pointer so an
// omitted JSON field keeps the default while an explicit false disables it.
type ImageConfig struct {
	AutoResize     *bool `json:"auto_resize,omitempty"`
	MaxWidth       int   `json:"max_width,omitempty"`
	MaxHeight      int   `json:"max_height,omitempty"`
	MaxBase64Bytes int   `json:"max_base64_bytes,omitempty"`
}

// Config is the media section of the configuration file.
type Config struct {
	Image ImageConfig `json:"image"`
}

// Default returns the built-in image limits.
func Default() Config {
	enabled := true
	return Config{Image: ImageConfig{
		AutoResize:     &enabled,
		MaxWidth:       defaultMaxWidth,
		MaxHeight:      defaultMaxHeight,
		MaxBase64Bytes: defaultMaxBase64Bytes,
	}}
}

// Normalize fills omitted fields and rejects non-positive limits.
func Normalize(input Config) Config {
	out := Default()
	if input.Image.AutoResize != nil {
		enabled := *input.Image.AutoResize
		out.Image.AutoResize = &enabled
	}
	if input.Image.MaxWidth > 0 {
		out.Image.MaxWidth = input.Image.MaxWidth
	}
	if input.Image.MaxHeight > 0 {
		out.Image.MaxHeight = input.Image.MaxHeight
	}
	if input.Image.MaxBase64Bytes > 0 {
		out.Image.MaxBase64Bytes = input.Image.MaxBase64Bytes
	}
	return out
}

// AutoResizeEnabled reports whether oversized images may be scaled down.
func (c Config) AutoResizeEnabled() bool {
	if c.Image.AutoResize == nil {
		return true
	}
	return *c.Image.AutoResize
}

// Process decodes one supported image, applies the configured limits, and
// returns bytes ready for base64 transport. The MIME may change when a JPEG
// encoding is the smallest result.
func Process(data []byte, mime string, config Config) ([]byte, string, error) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if !isImageMime(mime) {
		return data, mime, nil
	}
	config = Normalize(config)
	decoded, err := decodeImage(data, mime)
	if err != nil {
		return nil, mime, fmt.Errorf("decode image: %w", err)
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, mime, errors.New("image has no pixels")
	}
	if width <= config.Image.MaxWidth && height <= config.Image.MaxHeight &&
		base64.StdEncoding.EncodedLen(len(data)) <= config.Image.MaxBase64Bytes {
		return data, mime, nil
	}
	if !config.AutoResizeEnabled() {
		return nil, mime, fmt.Errorf(
			"image exceeds limits (%dx%d, %d encoded bytes; max %dx%d, %d encoded bytes)",
			width, height, base64.StdEncoding.EncodedLen(len(data)),
			config.Image.MaxWidth, config.Image.MaxHeight, config.Image.MaxBase64Bytes,
		)
	}

	ratio := 1.0
	if width > config.Image.MaxWidth {
		ratio = math.Min(ratio, float64(config.Image.MaxWidth)/float64(width))
	}
	if height > config.Image.MaxHeight {
		ratio = math.Min(ratio, float64(config.Image.MaxHeight)/float64(height))
	}
	hasAlpha := imageHasAlpha(decoded)
	for attempt := 0; attempt < 12; attempt++ {
		targetWidth := max(1, int(math.Round(float64(width)*ratio)))
		targetHeight := max(1, int(math.Round(float64(height)*ratio)))
		resized := resize(decoded, targetWidth, targetHeight)
		best, bestMime := smallestEncoding(resized, hasAlpha, config.Image.MaxBase64Bytes)
		if best != nil {
			return best, bestMime, nil
		}
		ratio *= 0.8
		if targetWidth == 1 && targetHeight == 1 {
			break
		}
	}
	return nil, mime, errors.New("image cannot be encoded within the configured base64 limit")
}

func isImageMime(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func decodeImage(data []byte, mime string) (image.Image, error) {
	switch mime {
	case "image/png":
		return png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		return jpeg.Decode(bytes.NewReader(data))
	case "image/gif":
		return gif.Decode(bytes.NewReader(data))
	case "image/webp":
		return webp.Decode(bytes.NewReader(data))
	default:
		return nil, fmt.Errorf("unsupported image type %q", mime)
	}
}

func resize(source image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), source, source.Bounds(), draw.Over, nil)
	return dst
}

// smallestEncoding tries lossless PNG and progressive JPEG qualities and keeps
// the smallest payload that fits the base64 limit.
func smallestEncoding(source image.Image, hasAlpha bool, maxBase64 int) ([]byte, string) {
	var best []byte
	bestMime := ""
	consider := func(data []byte, mime string) {
		if len(data) == 0 || base64.StdEncoding.EncodedLen(len(data)) > maxBase64 {
			return
		}
		if best == nil || len(data) < len(best) {
			best = data
			bestMime = mime
		}
	}

	var pngData bytes.Buffer
	if err := png.Encode(&pngData, source); err == nil {
		consider(pngData.Bytes(), "image/png")
	}
	if !hasAlpha {
		for _, quality := range []int{85, 75, 65, 55} {
			var jpegData bytes.Buffer
			if err := jpeg.Encode(&jpegData, source, &jpeg.Options{Quality: quality}); err == nil {
				consider(jpegData.Bytes(), "image/jpeg")
			}
		}
	}
	return best, bestMime
}

func imageHasAlpha(source image.Image) bool {
	bounds := source.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := source.At(x, y).RGBA()
			if alpha != 0xffff {
				return true
			}
		}
	}
	return false
}
