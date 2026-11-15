package media

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func samplePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func configWith(width, height, maxBase64 int) Config {
	enabled := true
	return Config{Image: ImageConfig{
		AutoResize:     &enabled,
		MaxWidth:       width,
		MaxHeight:      height,
		MaxBase64Bytes: maxBase64,
	}}
}

func TestProcessResizesOversizedImage(t *testing.T) {
	data := samplePNG(t, 300, 200)
	out, mime, err := Process(data, "image/png", configWith(100, 100, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" && mime != "image/jpeg" {
		t.Fatalf("unexpected mime: %s", mime)
	}
	decoded, err := decodeImage(out, mime)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() > 100 || decoded.Bounds().Dy() > 100 {
		t.Fatalf("image not resized: %v", decoded.Bounds())
	}
	if base64.StdEncoding.EncodedLen(len(out)) > 1<<20 {
		t.Fatalf("encoded image over limit: %d", base64.StdEncoding.EncodedLen(len(out)))
	}
}

func TestProcessKeepsSmallImage(t *testing.T) {
	data := samplePNG(t, 40, 30)
	out, mime, err := Process(data, "image/png", Default())
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || !bytes.Equal(out, data) {
		t.Fatal("small image should pass through unchanged")
	}
}

func TestProcessRejectsWithoutAutoResize(t *testing.T) {
	data := samplePNG(t, 300, 200)
	config := configWith(100, 100, 1<<20)
	disabled := false
	config.Image.AutoResize = &disabled
	if _, _, err := Process(data, "image/png", config); err == nil {
		t.Fatal("oversized image should be rejected when auto resize is off")
	}
}

func TestProcessRejectsImpossibleBase64Limit(t *testing.T) {
	data := samplePNG(t, 300, 200)
	if _, _, err := Process(data, "image/png", configWith(20, 20, 32)); err == nil {
		t.Fatal("image should fail when no encoding fits")
	}
}
