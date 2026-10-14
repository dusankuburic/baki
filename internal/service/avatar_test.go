package service

import (
	"bytes"
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
