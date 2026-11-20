package main

// Image sends: the agent token posts a PNG or JPEG with a caption through the same gate, label, ceilings and ledger
// as text. Fixtures are generated pictures, nothing real.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

func fixtureImage(w, h int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	return m
}

func fixturePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, fixtureImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixtureJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, fixtureImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// hugePNG is a valid PNG header that claims w x h pixels (a decompression bomb's shape); no pixel data follows.
func hugePNG(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	chunk := append([]byte("IHDR"), ihdr...)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, 13)
	out = append(out, chunk...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(chunk))
}

func imageBody(b []byte, caption string) string {
	v, _ := json.Marshal(map[string]string{"image": base64.StdEncoding.EncodeToString(b), "caption": caption})
	return string(v)
}

func TestAgentImageInItsChat(t *testing.T) {
	st, fl, h := setupAgent(t, true, grp2)
	pic := fixturePNG(t, 64, 40)
	w := do(h, "POST", "/chats/"+grp2+"/send", atok, imageBody(pic, "*6. Watch page*\nthree shops"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "IMG1") {
		t.Fatalf("agent image send: %d %s", w.Code, w.Body)
	}
	if len(fl.images) != 1 || len(fl.texts) != 0 {
		t.Fatalf("one image, no text: images=%d texts=%d", len(fl.images), len(fl.texts))
	}
	got := fl.images[0]
	if got.chat != grp2 || got.caption != "SISO agent: *6. Watch page*\nthree shops" {
		t.Fatalf("image must go to the bound chat with the label on its caption: %q to %s", got.caption, got.chat)
	}
	if got.img.Mime != "image/png" || got.img.Width != 64 || got.img.Height != 40 || !bytes.Equal(got.img.Data, pic) {
		t.Errorf("image passed through: %s %dx%d %d bytes", got.img.Mime, got.img.Width, got.img.Height, len(got.img.Data))
	}
	// It is an agent send in the ledger, and /health counts it.
	if n := st.AgentSendsSince(time.Now().Add(-time.Hour)); n != 1 {
		t.Errorf("agent image in ledger: %d, want 1", n)
	}
	var hl struct{ AgentSendsDay int }
	json.Unmarshal(do(h, "GET", "/health", atok, "").Body.Bytes(), &hl)
	if hl.AgentSendsDay != 1 {
		t.Errorf("health agentSendsDay: %d", hl.AgentSendsDay)
	}
	// A JPEG goes too; an empty caption still carries the label.
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, imageBody(fixtureJPEG(t, 30, 20), "")); w.Code != 200 {
		t.Fatalf("agent jpeg: %d %s", w.Code, w.Body)
	}
	if fl.images[1].img.Mime != "image/jpeg" || fl.images[1].caption != "SISO agent:" {
		t.Errorf("jpeg: %s caption %q", fl.images[1].img.Mime, fl.images[1].caption)
	}
	// The API token's caption stays as written.
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+alice+"/send", tok, imageBody(pic, "from Shaan")); w.Code != 200 || fl.images[2].caption != "from Shaan" {
		t.Errorf("main image: %d %q", w.Code, fl.images[2].caption)
	}
}

func TestAgentImageRefusedOutsideItsChat(t *testing.T) {
	_, fl, h := setupAgent(t, true, grp2)
	pic := fixturePNG(t, 8, 8)
	for _, chat := range []string{grp, alice, "0@s.whatsapp.net"} {
		if w := do(h, "POST", "/chats/"+chat+"/send", atok, imageBody(pic, "x")); w.Code != 403 {
			t.Errorf("agent image to %s: %d, want 403", chat, w.Code)
		}
	}
	_, fl2, h2 := setupAgent(t, false, grp2)
	if w := do(h2, "POST", "/chats/"+grp2+"/send", atok, imageBody(pic, "x")); w.Code != 403 {
		t.Errorf("agent image with WA_SEND=0: %d", w.Code)
	}
	if len(fl.sent)+len(fl2.sent) != 0 {
		t.Errorf("sent %d images outside the rules", len(fl.sent)+len(fl2.sent))
	}
}

func TestAgentImageCountsTowardCap(t *testing.T) {
	st, fl, h := setupAgent(t, true, grp2)
	for i := 0; i < agentSendPerDay-1; i++ {
		st.db.Exec(`INSERT INTO sends (ts,chat,id,ok,agent) VALUES (?,?,?,1,1)`, time.Now().Add(-3*time.Hour).Unix(), grp2, "x")
	}
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, imageBody(fixturePNG(t, 8, 8), "twentieth")); w.Code != 200 {
		t.Fatalf("20th agent send as an image: %d %s", w.Code, w.Body)
	}
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"twenty-first"}`); w.Code != 429 {
		t.Errorf("text after the image hit the cap: %d, want 429", w.Code)
	}
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, imageBody(fixturePNG(t, 8, 8), "x")); w.Code != 429 {
		t.Errorf("image over the cap: %d, want 429", w.Code)
	}
	// The 4 s gap holds images too.
	_, _, h2 := setupAgent(t, true, grp2)
	do(h2, "POST", "/chats/"+grp2+"/send", atok, `{"text":"one"}`)
	if w := do(h2, "POST", "/chats/"+grp2+"/send", atok, imageBody(fixturePNG(t, 8, 8), "x")); w.Code != 429 {
		t.Errorf("image inside the gap: %d, want 429", w.Code)
	}
	if len(fl.images) != 1 {
		t.Errorf("images sent %d, want 1", len(fl.images))
	}
}

func TestImageTypeAndSizeRefused(t *testing.T) {
	_, fl, h := setupAgent(t, true, grp2)
	var gifBuf bytes.Buffer
	gif.Encode(&gifBuf, fixtureImage(8, 8), nil)
	big := append(fixturePNG(t, 8, 8), make([]byte, imageMaxBytes)...)
	pngHead := fixturePNG(t, 8, 8)[:40]
	for _, c := range []struct {
		name, body string
		code       int
		msg        string
	}{
		{"gif", imageBody(gifBuf.Bytes(), "x"), 415, "PNG or JPEG"},
		{"text file", imageBody([]byte("hello, not a picture"), "x"), 415, "PNG or JPEG"},
		{"svg", imageBody([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "x"), 415, "PNG or JPEG"},
		{"png magic, broken body", imageBody(append([]byte("\x89PNG\r\n\x1a\n"), "junk"...), "x"), 415, "PNG or JPEG"},
		{"truncated png", imageBody(pngHead, "x"), 415, "PNG or JPEG"},
		{"huge dimensions", imageBody(hugePNG(30000, 30000), "x"), 413, "pixels"},
		{"over 5 MB", imageBody(big, "x"), 413, "5 MB"},
		{"not base64", `{"image":"%%%not base64%%%","caption":"x"}`, 400, "base64"},
		{"text and image", `{"text":"hi","image":"` + base64.StdEncoding.EncodeToString(fixturePNG(t, 8, 8)) + `"}`, 400, "not both"},
		{"caption too long", imageBody(fixturePNG(t, 8, 8), strings.Repeat("ê", imageCaptionMax)), 400, "caption"},
	} {
		w := do(h, "POST", "/chats/"+grp2+"/send", atok, c.body)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d with %q", c.name, w.Code, strings.TrimSpace(w.Body.String()), c.code, c.msg)
		}
	}
	if len(fl.sent) != 0 {
		t.Errorf("a refused image was sent (%d)", len(fl.sent))
	}
}

func TestImageMessageCarriesUpload(t *testing.T) {
	img, err := checkImage(fixturePNG(t, 300, 120))
	if err != nil {
		t.Fatal(err)
	}
	up := whatsmeow.UploadResponse{URL: "https://mmg.example/x", DirectPath: "/v/x", MediaKey: []byte{1}, FileEncSHA256: []byte{2}, FileSHA256: []byte{3}, FileLength: 99}
	m := imageMessage(up, img, "SISO agent: hi").GetImageMessage()
	if m.GetURL() != up.URL || m.GetDirectPath() != up.DirectPath || m.GetFileLength() != 99 ||
		!bytes.Equal(m.GetMediaKey(), up.MediaKey) || !bytes.Equal(m.GetFileEncSHA256(), up.FileEncSHA256) || !bytes.Equal(m.GetFileSHA256(), up.FileSHA256) {
		t.Errorf("upload fields not copied: %v", m)
	}
	if m.GetCaption() != "SISO agent: hi" || m.GetMimetype() != "image/png" || m.GetWidth() != 300 || m.GetHeight() != 120 || m.GetMediaKeyTimestamp() == 0 {
		t.Errorf("caption, type or size: %v", m)
	}
	th, err := jpeg.DecodeConfig(bytes.NewReader(m.GetJPEGThumbnail()))
	if err != nil || th.Width > thumbMaxSide || th.Height > thumbMaxSide || th.Width < 2 {
		t.Errorf("thumbnail: %v %dx%d", err, th.Width, th.Height)
	}
	if t2, k, mime, _ := describe(imageMessage(up, img, "c")); t2 != "c" || k != "image" || mime != "image/png" {
		t.Errorf("a sent image reads back as an image with its caption: %q %q %q", t2, k, mime)
	}
}
