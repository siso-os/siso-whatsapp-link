package main

// Outgoing images: the checks an image passes before it is uploaded (PNG or JPEG by its own bytes, at most 5 MB and
// 20 megapixels), and the WhatsApp message built from the upload (caption, size, a small JPEG thumbnail).
// Shaan, 19 Nov 2026: "send the different paragraphs with the screenshots" to the agent group.

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const (
	imageMaxBytes   = 5 << 20
	imageMaxPixels  = 20_000_000 // a PNG is decoded for its thumbnail: this bounds that memory (~80 MB)
	imageCaptionMax = 1024       // characters, WhatsApp's own caption limit
	thumbMaxSide    = 96
)

// OutImage is an image that passed checkImage: its bytes, its real type and its size.
type OutImage struct {
	Data          []byte
	Mime          string
	Width, Height int
	pixels        image.Image // decoded once by checkImage, reused for the thumbnail
}

var (
	errImageType  = errors.New("only PNG or JPEG images can be sent")
	errImageLarge = fmt.Errorf("image too large (max %d MB)", imageMaxBytes>>20)
	errImagePix   = fmt.Errorf("image has too many pixels (max %d million)", imageMaxPixels/1_000_000)
)

// checkImage trusts the bytes, not a name or a header: a PNG or JPEG signature, a header of the same type within the
// pixel limit, and a body that decodes whole (a truncated or corrupt file is refused, not sent).
func checkImage(b []byte) (OutImage, error) {
	if len(b) > imageMaxBytes {
		return OutImage{}, errImageLarge
	}
	var mime string
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		mime = "image/jpeg"
	default:
		return OutImage{}, errImageType
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || "image/"+format != mime || cfg.Width < 1 || cfg.Height < 1 {
		return OutImage{}, errImageType
	}
	if cfg.Width*cfg.Height > imageMaxPixels {
		return OutImage{}, errImagePix
	}
	pix, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return OutImage{}, errImageType
	}
	return OutImage{Data: b, Mime: mime, Width: cfg.Width, Height: cfg.Height, pixels: pix}, nil
}

// thumbnail is the small preview WhatsApp shows before the picture downloads (nil if it cannot be made; it is optional).
func thumbnail(img OutImage) []byte {
	src := img.pixels
	if src == nil {
		return nil
	}
	b := src.Bounds()
	w, h := thumbMaxSide, thumbMaxSide
	if b.Dx() >= b.Dy() {
		h = max(1, b.Dy()*thumbMaxSide/b.Dx())
	} else {
		w = max(1, b.Dx()*thumbMaxSide/b.Dy())
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/h))
		}
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, dst, &jpeg.Options{Quality: 60}) != nil {
		return nil
	}
	return out.Bytes()
}

// imageMessage copies the upload's fields into an ImageMessage, as whatsmeow's Upload documents (upload.go).
func imageMessage(up whatsmeow.UploadResponse, img OutImage, caption string) *waE2E.Message {
	m := &waE2E.ImageMessage{
		Mimetype:          proto.String(img.Mime),
		URL:               proto.String(up.URL),
		DirectPath:        proto.String(up.DirectPath),
		MediaKey:          up.MediaKey,
		FileEncSHA256:     up.FileEncSHA256,
		FileSHA256:        up.FileSHA256,
		FileLength:        proto.Uint64(up.FileLength),
		Width:             proto.Uint32(uint32(img.Width)),
		Height:            proto.Uint32(uint32(img.Height)),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		JPEGThumbnail:     thumbnail(img),
	}
	if caption != "" {
		m.Caption = proto.String(caption)
	}
	return &waE2E.Message{ImageMessage: m}
}
