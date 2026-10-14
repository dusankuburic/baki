package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// The avatar upload limits. The request body is capped before parsing (the handler's MaxBytesReader, the same
// AvatarMaxUploadBytes); what is STORED is the re-encoded image, capped by avatarMaxStoredBytes so a stored row
// stays under half a megabyte whatever the source file was.
const (
	// AvatarMaxUploadBytes caps the whole upload request; the handler installs it as the body reader.
	AvatarMaxUploadBytes = 5 << 20
	avatarMaxDimension   = 512
	avatarMaxStoredBytes = 512 << 10
	// avatarMaxDecodeSide and avatarMaxDecodePixels reject, on the cheap header parse, an image whose full
	// decode would blow up memory (a small compressed file can describe an enormous canvas): 16 M pixels is about
	// 64 MB of RGBA per request, where 50 M was 200 MB or more. The web client downscales to 512 px before it
	// uploads, so only a direct API caller with a source photo above 4096 px meets the cap, and a 12-megapixel
	// phone photo (4000×3000) still passes.
	avatarMaxDecodeSide   = 4096
	avatarMaxDecodePixels = 16_000_000
)

// ErrAvatarImage refuses an upload: not a PNG or JPEG image, undecodable, or a re-encode that cannot fit the
// stored-size cap. The wrapping message is safe to show the user; the sentinel maps to HTTP 400.
var ErrAvatarImage = errors.New("invalid avatar image")

// normalizeAvatarImage decodes data, downscales it to avatarMaxDimension on its longest side, and re-encodes it.
// The re-encode is the point, not an implementation detail: what is stored never carries the uploaded file's
// metadata (EXIF with its GPS coordinates), animation frames or payloads — only pixels this code produced. The
// accepted formats are exactly the registered decoders (PNG, JPEG); the upload's declared content type is never
// consulted.
func normalizeAvatarImage(data []byte) (mimeType string, out []byte, err error) {
	if cfg, _, cerr := image.DecodeConfig(bytes.NewReader(data)); cerr != nil {
		return "", nil, fmt.Errorf("%w: must be a PNG or JPEG image", ErrAvatarImage)
	} else if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > avatarMaxDecodeSide || cfg.Height > avatarMaxDecodeSide ||
		cfg.Width*cfg.Height > avatarMaxDecodePixels {
		return "", nil, fmt.Errorf("%w: dimensions %dx%d are too large", ErrAvatarImage, cfg.Width, cfg.Height)
	}

	src, format, derr := image.Decode(bytes.NewReader(data))
	if derr != nil {
		return "", nil, fmt.Errorf("%w: must be a PNG or JPEG image", ErrAvatarImage)
	}

	b := src.Bounds()
	if b.Dx() > avatarMaxDimension || b.Dy() > avatarMaxDimension {
		w, h := scaleDown(b.Dx(), b.Dy())
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.ApproxBiLinear.Scale(dst, dst.Rect, src, b, draw.Src, nil)
		src = dst
	}

	var buf bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&buf, src); err != nil {
			return "", nil, fmt.Errorf("%w: re-encoding failed", ErrAvatarImage)
		}
		mimeType = "image/png"
	case "jpeg":
		if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 85}); err != nil {
			return "", nil, fmt.Errorf("%w: re-encoding failed", ErrAvatarImage)
		}
		mimeType = "image/jpeg"
	default:
		// Unreachable with the registered decoders above; kept so a future decoder registration cannot make the
		// store accept a format its row comment does not promise.
		return "", nil, fmt.Errorf("%w: must be a PNG or JPEG image", ErrAvatarImage)
	}
	if buf.Len() > avatarMaxStoredBytes {
		return "", nil, fmt.Errorf("%w: the image does not fit %d KB once encoded", ErrAvatarImage, avatarMaxStoredBytes>>10)
	}
	return mimeType, buf.Bytes(), nil
}

// scaleDown returns w and h scaled to their longest side of avatarMaxDimension, never below 1.
func scaleDown(w, h int) (int, int) {
	basis := max(w, h)
	if basis <= avatarMaxDimension {
		return max(w, 1), max(h, 1)
	}
	return max(w*avatarMaxDimension/basis, 1), max(h*avatarMaxDimension/basis, 1)
}

// SaveAvatar validates and normalizes the image, stores it as the user's avatar, and points the profile's
// avatar URL at url — the serving route, an API concern the caller passes in (the service does not know routes).
// ErrAvatarImage for a refused image, ErrNotFound for an unknown user.
func (s *AuthService) SaveAvatar(ctx context.Context, userID string, data []byte, url string) error {
	mimeType, out, err := normalizeAvatarImage(data)
	if err != nil {
		return err
	}
	if err := s.backend.SaveUserAvatar(ctx, userID, mimeType, out); err != nil {
		return err
	}
	// The URL write is a second statement: if it fails the stored image is inert (nothing points at it) until the
	// user retries, which self-heals; a transaction across two store calls would widen the storage contract.
	user, err := s.backend.LoadUserByID(ctx, userID)
	if err != nil {
		return err
	}
	return s.backend.UpdateUserProfile(ctx, userID, user.DisplayName, url)
}

// DeleteAvatar removes the stored image. The avatar URL is cleared only when it pointed at url (the serving
// route): an external URL the user set by hand survives removing the upload.
func (s *AuthService) DeleteAvatar(ctx context.Context, userID, url string) error {
	if err := s.backend.DeleteUserAvatar(ctx, userID); err != nil {
		return err
	}
	user, err := s.backend.LoadUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.AvatarURL != url {
		return nil
	}
	return s.backend.UpdateUserProfile(ctx, userID, user.DisplayName, "")
}

// Avatar loads a user's stored image for the serving route; ErrNotFound when they never uploaded one.
func (s *AuthService) Avatar(ctx context.Context, userID string) (*storageif.UserAvatar, error) {
	return s.backend.LoadUserAvatar(ctx, userID)
}
