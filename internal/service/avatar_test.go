package service

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// pngBytes renders a w×h PNG of one colour, the shape every avatar upload path must accept.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: 0x60, G: 0x80, B: 0xa0, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

// jpegBytes is made the same way; jpeg.Encode needs the import for its side effect anyway (the production code
// registers it), and a re-encoded JPEG round-trip is worth exercising directly.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: 0x90, G: 0x60, B: 0x30, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestNormalizeAvatarImage(t *testing.T) {
	t.Run("small png passes through re-encoded", func(t *testing.T) {
		mime, out, err := normalizeAvatarImage(pngBytes(t, 64, 64))
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if mime != "image/png" || len(out) == 0 || len(out) > avatarMaxStoredBytes {
			t.Fatalf("got %q, %d bytes; want a stored image/png", mime, len(out))
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || cfg.Width != 64 || cfg.Height != 64 {
			t.Errorf("stored image is %dx%d (%v); want the original 64x64", cfg.Width, cfg.Height, err)
		}
	})
	t.Run("oversized image is downscaled to the dimension cap", func(t *testing.T) {
		// 1200×800 caps at 512 on its longest side: 512×341.
		mime, out, err := normalizeAvatarImage(pngBytes(t, 1200, 800))
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if mime != "image/png" {
			t.Fatalf("format changed: %q", mime)
		}
		cfg, _, cerr := image.DecodeConfig(bytes.NewReader(out))
		if cerr != nil || cfg.Width != avatarMaxDimension || cfg.Height == 0 {
			t.Errorf("stored image is %dx%d (%v); want the longest side at %d", cfg.Width, cfg.Height, cerr, avatarMaxDimension)
		}
		if cfg.Width > avatarMaxDimension || cfg.Height > avatarMaxDimension {
			t.Errorf("stored image is %dx%d; nothing over %d may be stored", cfg.Width, cfg.Height, avatarMaxDimension)
		}
	})
	t.Run("jpeg keeps its format", func(t *testing.T) {
		mime, _, err := normalizeAvatarImage(jpegBytes(t, 200, 200))
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if mime != "image/jpeg" {
			t.Errorf("got %q, want image/jpeg", mime)
		}
	})
	t.Run("not an image is refused", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"empty":                    {},
			"text":                     []byte("plainly not an image"),
			"png magic without a body": []byte("\x89PNG\r\n\x1a\n"),
		} {
			if _, _, err := normalizeAvatarImage(data); !errors.Is(err, ErrAvatarImage) {
				t.Errorf("%s: got %v, want an ErrAvatarImage", name, err)
			}
		}
	})
	t.Run("an enormous canvas is refused before decoding", func(t *testing.T) {
		// A tiny PNG whose IHDR declares 20000×20000: the header parse alone must reject it, without the
		// multi-gigabyte decode the full image would ask for.
		huge := pngBytes(t, 1, 1)
		huge[16], huge[17], huge[18], huge[19] = 0x00, 0x00, 0x4e, 0x20 // width 20000
		huge[20], huge[21], huge[22], huge[23] = 0x00, 0x00, 0x4e, 0x20 // height 20000
		if _, _, err := normalizeAvatarImage(huge); !errors.Is(err, ErrAvatarImage) {
			t.Errorf("got %v, want an ErrAvatarImage", err)
		}
	})

	t.Run("a canvas just over the side cap is refused", func(t *testing.T) {
		wide := pngBytes(t, 1, 1)
		wide[16], wide[17], wide[18], wide[19] = 0x00, 0x00, 0x10, 0x01 // width 4097
		if _, _, err := normalizeAvatarImage(wide); !errors.Is(err, ErrAvatarImage) {
			t.Errorf("got %v, want an ErrAvatarImage", err)
		}
	})
}

// The four quadrant colours of quadrantJPEG, far enough apart that a JPEG round trip cannot confuse them.
var quadrantColors = map[string]color.RGBA{
	"red":   {R: 0xff, A: 0xff},
	"green": {G: 0xff, A: 0xff},
	"blue":  {B: 0xff, A: 0xff},
	"white": {R: 0xff, G: 0xff, B: 0xff, A: 0xff},
}

// quadrantJPEG renders a w×h JPEG whose quadrants are red (top left), green (top right), blue (bottom left) and white
// (bottom right), so every flip and quarter turn puts a different colour in the top left corner.
func quadrantJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			name := map[[2]bool]string{{false, false}: "red", {true, false}: "green", {false, true}: "blue", {true, true}: "white"}[[2]bool{x >= w/2, y >= h/2}]
			img.Set(x, y, quadrantColors[name])
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// exifSegment builds an APP1 "Exif" segment by hand, as a camera writes it: a TIFF header in the given byte order and
// IFD0 with a Make entry and then the Orientation entry (a SHORT, left-justified in its 4-byte value field).
func exifSegment(order binary.ByteOrder, orientation uint16) []byte {
	tiff := make([]byte, 8+2+2*12+4)
	if order == binary.LittleEndian {
		copy(tiff, "II*\x00")
	} else {
		copy(tiff, "MM\x00*")
	}
	order.PutUint32(tiff[4:], 8) // IFD0 right after the header
	order.PutUint16(tiff[8:], 2) // two entries
	order.PutUint16(tiff[10:], 0x010f)
	order.PutUint16(tiff[12:], 2) // ASCII
	order.PutUint32(tiff[14:], 3)
	copy(tiff[18:], "Go\x00")
	order.PutUint16(tiff[22:], 0x0112)
	order.PutUint16(tiff[24:], 3) // SHORT
	order.PutUint32(tiff[26:], 1)
	order.PutUint16(tiff[30:], orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	return append(seg, payload...)
}

// withSegments inserts segments right after a JPEG's SOI marker, where cameras put their APPn segments.
func withSegments(jpg []byte, segs ...[]byte) []byte {
	out := append([]byte{}, jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

// quadrantsOf names the colour at the centre of each quadrant of a stored image: top left, top right, bottom left,
// bottom right.
func quadrantsOf(t *testing.T, stored []byte) (w, h int, quadrants [4]string) {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("decode stored image: %v", err)
	}
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	for i, p := range []image.Point{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}} {
		r, g, bl, _ := img.At(b.Min.X+p.X, b.Min.Y+p.Y).RGBA()
		best, bestDist := "", uint64(1<<63)
		for name, c := range quadrantColors {
			d := func(a uint32, b uint8) uint64 { x := int64(a>>8) - int64(b); return uint64(x * x) }
			if dist := d(r, c.R) + d(g, c.G) + d(bl, c.B); dist < bestDist {
				best, bestDist = name, dist
			}
		}
		quadrants[i] = best
	}
	return w, h, quadrants
}

// TestNormalizeAvatarImage_AppliesTheEXIFOrientation is the regression test for the re-encode ignoring a JPEG's EXIF
// Orientation: the re-encode drops the tag (it must: no metadata is stored), so the pixels have to be turned upright
// first, or a phone photo uploaded straight to the API is stored on its side. The expected corners are worked out
// by hand from the EXIF definitions (2 mirrors, 3 turns 180°, 6 turns 90° clockwise, 8 counter-clockwise, 5 and 7
// mirror across a diagonal), not from the code under test.
func TestNormalizeAvatarImage_AppliesTheEXIFOrientation(t *testing.T) {
	const w, h = 64, 32
	src := quadrantJPEG(t, w, h)
	want := map[uint16][4]string{
		1: {"red", "green", "blue", "white"},
		2: {"green", "red", "white", "blue"},
		3: {"white", "blue", "green", "red"},
		4: {"blue", "white", "red", "green"},
		5: {"red", "blue", "green", "white"},
		6: {"blue", "red", "white", "green"},
		7: {"white", "green", "blue", "red"},
		8: {"green", "white", "red", "blue"},
	}
	for o := uint16(1); o <= 8; o++ {
		for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
			mime, out, err := normalizeAvatarImage(withSegments(src, exifSegment(order, o)))
			if err != nil {
				t.Fatalf("orientation %d (%v): %v", o, order, err)
			}
			gotW, gotH, got := quadrantsOf(t, out)
			wantW, wantH := w, h
			if o >= 5 {
				wantW, wantH = h, w
			}
			if mime != "image/jpeg" || gotW != wantW || gotH != wantH || got != want[o] {
				t.Errorf("orientation %d (%v): %s %dx%d %v, want image/jpeg %dx%d %v", o, order, mime, gotW, gotH, got, wantW, wantH, want[o])
			}
			if bytes.Contains(out, []byte("Exif\x00\x00")) {
				t.Errorf("orientation %d (%v): the stored image still carries an Exif segment", o, order)
			}
		}
	}

	t.Run("after a JFIF APP0 segment", func(t *testing.T) {
		app0 := []byte{0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
		_, out, err := normalizeAvatarImage(withSegments(src, app0, exifSegment(binary.BigEndian, 6)))
		if err != nil {
			t.Fatal(err)
		}
		if gotW, gotH, got := quadrantsOf(t, out); gotW != h || gotH != w || got != want[6] {
			t.Errorf("%dx%d %v, want %dx%d %v", gotW, gotH, got, h, w, want[6])
		}
	})

	t.Run("a turned image is downscaled on its new longest side", func(t *testing.T) {
		_, out, err := normalizeAvatarImage(withSegments(quadrantJPEG(t, 1200, 600), exifSegment(binary.LittleEndian, 8)))
		if err != nil {
			t.Fatal(err)
		}
		if gotW, gotH, got := quadrantsOf(t, out); gotW != 256 || gotH != avatarMaxDimension || got != want[8] {
			t.Errorf("%dx%d %v, want 256x%d %v", gotW, gotH, got, avatarMaxDimension, want[8])
		}
	})
}

// An Exif segment that cannot be read leaves the image as it is: never an error, never a panic.
func TestNormalizeAvatarImage_UnreadableEXIFKeepsTheImage(t *testing.T) {
	const w, h = 64, 32
	src := quadrantJPEG(t, w, h)
	upright := [4]string{"red", "green", "blue", "white"}
	valid := exifSegment(binary.BigEndian, 6)
	tamper := func(f func(seg []byte)) []byte {
		seg := append([]byte{}, valid...)
		f(seg)
		return seg
	}
	// Offsets inside the segment: the TIFF header starts after the marker, the length and "Exif\0\0"; the
	// Orientation entry is IFD0's second (tag, type, count, value at +22, +24, +26, +30).
	const tiff = 4 + 6
	for name, seg := range map[string][]byte{
		"no orientation entry":  tamper(func(s []byte) { binary.BigEndian.PutUint16(s[tiff+22:], 0x0110) }),
		"orientation 0":         tamper(func(s []byte) { binary.BigEndian.PutUint16(s[tiff+30:], 0) }),
		"orientation 9":         tamper(func(s []byte) { binary.BigEndian.PutUint16(s[tiff+30:], 9) }),
		"orientation as a LONG": tamper(func(s []byte) { binary.BigEndian.PutUint16(s[tiff+24:], 4) }),
		"two orientations":      tamper(func(s []byte) { binary.BigEndian.PutUint32(s[tiff+26:], 2) }),
		"unknown byte order":    tamper(func(s []byte) { copy(s[tiff:], "XX\x00*") }),
		"IFD offset past the end": tamper(func(s []byte) {
			binary.BigEndian.PutUint32(s[tiff+4:], 0xfffffff0)
		}),
		"IFD offset into the header": tamper(func(s []byte) { binary.BigEndian.PutUint32(s[tiff+4:], 2) }),
		"entries run past the end": tamper(func(s []byte) {
			binary.BigEndian.PutUint16(s[tiff+8:], 0xffff)
			binary.BigEndian.PutUint16(s[tiff+22:], 0x0110)
		}),
		"not Exif": tamper(func(s []byte) { copy(s[4:], "Ex1f") }),
	} {
		mime, out, err := normalizeAvatarImage(withSegments(src, seg))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if gotW, gotH, got := quadrantsOf(t, out); mime != "image/jpeg" || gotW != w || gotH != h || got != upright {
			t.Errorf("%s: %s %dx%d %v, want the image as it is", name, mime, gotW, gotH, got)
		}
	}
}

// jpegOrientation reads only what it has bounds-checked: every truncation of a header that carries the tag answers 1
// without a panic until the whole segment is there, and arbitrary bytes answer 1 to 8 (FuzzJPEGOrientation).
func TestJPEGOrientation_TruncatedHeader(t *testing.T) {
	seg := exifSegment(binary.LittleEndian, 6)
	full := withSegments(quadrantJPEG(t, 8, 8), seg)
	for n := 0; n < 2+len(seg); n++ {
		if o := jpegOrientation(full[:n]); o != 1 {
			t.Errorf("first %d bytes: orientation %d, want 1 (the segment is incomplete)", n, o)
		}
	}
	if o := jpegOrientation(full[:2+len(seg)]); o != 6 {
		t.Errorf("SOI and the whole segment: orientation %d, want 6", o)
	}
}

// Stray bytes between segments (a non-0xFF byte, a stuffed FF 00) are skipped up to the next marker, as libjpeg and
// image/jpeg do: the reader gave up on the first one, so the image decoded but was stored on its side.
func TestJPEGOrientation_SkipsStrayBytesBetweenSegments(t *testing.T) {
	src := quadrantJPEG(t, 64, 32)
	app0 := []byte{0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
	stray := [][]byte{{0x00}, {0x7f, 0x00, 0x13}, {0xff, 0x00}, {0x00, 0xff, 0x00}}
	for _, junk := range stray {
		data := withSegments(src, junk, app0, junk, exifSegment(binary.BigEndian, 6))
		if o := jpegOrientation(data); o != 6 {
			t.Errorf("stray % x before the segments: orientation %d, want 6", junk, o)
		}
		_, out, err := normalizeAvatarImage(data)
		if err != nil {
			t.Fatalf("stray % x: %v", junk, err)
		}
		if gotW, gotH, got := quadrantsOf(t, out); gotW != 32 || gotH != 64 || got != [4]string{"blue", "red", "white", "green"} {
			t.Errorf("stray % x: stored %dx%d %v, want the image turned 90° clockwise", junk, gotW, gotH, got)
		}
	}
	// Bytes that never reach a marker still answer 1.
	if o := jpegOrientation(append([]byte{0xff, 0xd8}, bytes.Repeat([]byte{0x7f}, 64)...)); o != 1 {
		t.Errorf("no marker after the stray bytes: orientation %d, want 1", o)
	}
}

func FuzzJPEGOrientation(f *testing.F) {
	f.Add(withSegments([]byte{0xff, 0xd8, 0xff, 0xd9}, exifSegment(binary.BigEndian, 6)))
	f.Add(withSegments([]byte{0xff, 0xd8, 0xff, 0xd9}, exifSegment(binary.LittleEndian, 3)))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xff, 0xd0, 0xff, 0xe1, 0x00, 0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		if o := jpegOrientation(data); o < 1 || o > 8 {
			t.Fatalf("orientation %d, want 1 to 8", o)
		}
	})
}

func TestAuthServiceSaveAndDeleteAvatar(t *testing.T) {
	store := testutil.NewFakeBackend()
	svc := NewAuthService(store, nil)
	const (
		id  = "svc-avatar-user"
		url = "/api/users/svc-avatar-user/avatar"
	)
	store.Users = map[string]*interfaces.User{
		id: {ID: id, Email: "svc-avatar@example.com", DisplayName: "Ada"},
	}

	if _, err := svc.Avatar(t.Context(), id); !errors.Is(err, interfaces.ErrNotFound) {
		t.Fatalf("Avatar(before): %v, want ErrNotFound", err)
	}

	if err := svc.SaveAvatar(t.Context(), id, pngBytes(t, 48, 48), url); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}
	got, err := svc.Avatar(t.Context(), id)
	if err != nil {
		t.Fatalf("Avatar: %v", err)
	}
	if got.MimeType != "image/png" || len(got.Data) == 0 {
		t.Fatalf("stored avatar: %q, %d bytes", got.MimeType, len(got.Data))
	}
	// The upload points the profile at the serving route and keeps the display name.
	u := store.Users[id]
	if u.AvatarURL != url || u.DisplayName != "Ada" {
		t.Errorf("profile after upload: avatar URL %q, name %q", u.AvatarURL, u.DisplayName)
	}

	// Removing the upload clears the URL it set.
	if err := svc.DeleteAvatar(t.Context(), id, url); err != nil {
		t.Fatalf("DeleteAvatar: %v", err)
	}
	if _, err := svc.Avatar(t.Context(), id); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("Avatar(after delete): %v, want ErrNotFound", err)
	}
	if u = store.Users[id]; u.AvatarURL != "" {
		t.Errorf("avatar URL after delete: %q, want cleared", u.AvatarURL)
	}

	// An external URL the user set by hand survives removing the upload.
	store.Users[id].AvatarURL = "https://example.com/me.png"
	if err := svc.DeleteAvatar(t.Context(), id, url); err != nil {
		t.Fatalf("DeleteAvatar(external): %v", err)
	}
	if u = store.Users[id]; u.AvatarURL != "https://example.com/me.png" {
		t.Errorf("external URL after delete: %q, want it kept", u.AvatarURL)
	}

	// A refused image changes nothing.
	if err := svc.SaveAvatar(t.Context(), id, []byte("nope"), url); !errors.Is(err, ErrAvatarImage) {
		t.Errorf("SaveAvatar(not an image): %v, want ErrAvatarImage", err)
	}
	if _, err := svc.Avatar(t.Context(), id); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("a refused image must not store anything: %v", err)
	}

	// An unknown user is ErrNotFound, from the store's foreign key.
	if err := svc.SaveAvatar(t.Context(), "svc-avatar-nobody", pngBytes(t, 8, 8), "/x"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("SaveAvatar(unknown user): %v, want ErrNotFound", err)
	}
}

func TestAuthServiceUpdateProfileKeepsUploadInStep(t *testing.T) {
	store := testutil.NewFakeBackend()
	svc := NewAuthService(store, nil)
	const (
		id  = "svc-profile-user"
		url = "/api/users/svc-profile-user/avatar"
	)
	store.Users = map[string]*interfaces.User{id: {ID: id, Email: "svc-profile@example.com"}}

	// The serving route with no upload behind it would be a 404 in every avatar slot.
	if err := svc.UpdateProfile(t.Context(), id, "Ada", url, url); !errors.Is(err, ErrAvatarImage) {
		t.Fatalf("UpdateProfile(route without upload): %v, want ErrAvatarImage", err)
	}
	if err := svc.SaveAvatar(t.Context(), id, pngBytes(t, 32, 32), url); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}

	// Renaming while keeping the route keeps the image.
	if err := svc.UpdateProfile(t.Context(), id, "Ada L.", url, url); err != nil {
		t.Fatalf("UpdateProfile(keep route): %v", err)
	}
	if _, err := svc.Avatar(t.Context(), id); err != nil {
		t.Errorf("the upload was dropped by a rename: %v", err)
	}
	if u := store.Users[id]; u.DisplayName != "Ada L." || u.AvatarURL != url {
		t.Errorf("profile: %q, %q", u.DisplayName, u.AvatarURL)
	}

	// An external URL replaces the upload; the stored image goes with it.
	if err := svc.UpdateProfile(t.Context(), id, "Ada L.", "https://example.com/me.png", url); err != nil {
		t.Fatalf("UpdateProfile(external): %v", err)
	}
	if _, err := svc.Avatar(t.Context(), id); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("an external URL left the upload stored: %v", err)
	}

	// So does clearing the URL.
	if err := svc.SaveAvatar(t.Context(), id, pngBytes(t, 32, 32), url); err != nil {
		t.Fatalf("SaveAvatar: %v", err)
	}
	if err := svc.UpdateProfile(t.Context(), id, "Ada L.", "", url); err != nil {
		t.Fatalf("UpdateProfile(clear): %v", err)
	}
	if _, err := svc.Avatar(t.Context(), id); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("clearing the URL left the upload stored: %v", err)
	}
}
