package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// The uploaded-avatar endpoints. The serving route and the multipart upload stay hand-written (a blob download
// and a multipart body, the two classes the typed adapter does not fit, docs/architecture/api-conventions.md);
// the removal is a typed Query endpoint. The registrations live here, not in auth.go's Mount, so the hand-written
// ratchet of that file does not grow.

// userAvatarRoute is the serving path of userID's uploaded avatar: the value users.avatar_url holds after an
// upload, and the only avatar path the frontend resolves through an authenticated fetch (an <img> tag cannot
// carry the Authorization header, so the frontend pulls the bytes and caches the object URL).
func userAvatarRoute(userID string) string { return "/api/users/" + userID + "/avatar" }

// mountAvatarProfileRoutes registers the caller's own avatar management inside /api/auth, beside PUT /profile:
// identity self-management, so it stays inside the group's token denial.
func (h *AuthHandler) mountAvatarProfileRoutes(rb *route.Builder) {
	rb.Post("/profile/avatar", h.handleAuthUploadAvatar, route.RateLimit(route.RateAvatar))
	rb.Delete("/profile/avatar", Query(h.deleteAvatar))
}

// mountAvatarServeRoute registers any signed-in caller's read of any user's stored avatar, for the places avatars
// appear cross-user (comment authors, the admin list). The bytes need the Authorization header, so no <img> can
// load them directly: the frontend fetches and caches (avatarCache); read-level for scoped tokens.
func (h *AuthHandler) mountAvatarServeRoute(rb *route.Builder) {
	rb.Get("/api/users/{id}/avatar", h.handleUserAvatar)
}

// profileEcho is the profile as the avatar endpoints answer it, the same shape PUT /api/auth/profile returns.
type profileEcho struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl"`
}

// deleteAvatar removes the caller's uploaded image; the profile's avatar URL is cleared only when it pointed at
// the serving route (an external URL set by hand survives).
func (h *AuthHandler) deleteAvatar(ctx context.Context, _ struct{}) (profileEcho, error) {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return profileEcho{}, apperr.Unauthenticated("", "unauthorized")
	}
	if h.backend == nil {
		return profileEcho{}, apperr.Unavailable("", "storage backend not available")
	}
	if err := h.authSvc.DeleteAvatar(ctx, claims.UserID, userAvatarRoute(claims.UserID)); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			return profileEcho{}, err
		}
		return profileEcho{}, apperr.Internal("", err.Error()).Wrap(err)
	}
	h.audit.Record(ctx, audit.Entry{Action: audit.ActionProfileUpdate, ResourceType: "user", ResourceID: claims.UserID,
		Meta: map[string]string{"field": "avatar", "change": "removed"}})
	return h.profileEcho(ctx, claims)
}

// @Summary      Upload an avatar image
// @Description  Re-encodes the uploaded PNG or JPEG (metadata stripped, a JPEG turned upright by its EXIF orientation, downscaled to at most 512 px, at most 512 KB stored), stores it as the caller's avatar and points the profile's avatar URL at the serving route. A multipart/form-data body with one "avatar" file part; the declared content type is never trusted — the bytes decide.
// @Tags         auth
// @Accept       multipart/form-data
// @Produce      json
// @Success      200 {object} profileEcho "The updated profile"
// @Failure      400 {object} map[string]string "Not a PNG or JPEG image, or not a multipart form with an avatar part"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      404 {object} map[string]string "Unknown account"
// @Failure      413 {object} map[string]string "Over the upload size limit"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/profile/avatar [post]
func (h *AuthHandler) handleAuthUploadAvatar(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, service.AvatarMaxUploadBytes)
	if err := r.ParseMultipartForm(64 << 10); err != nil {
		renderAvatarBodyError(w, err)
		return
	}
	file, _, err := r.FormFile("avatar")
	if err != nil {
		render.Error(w, fmt.Errorf("multipart form must carry one 'avatar' file part"), http.StatusBadRequest)
		return
	}
	defer file.Close()
	// The body reader is capped above, so this read cannot exceed the upload limit.
	data, err := io.ReadAll(file)
	if err != nil {
		renderAvatarBodyError(w, err)
		return
	}

	if err := h.authSvc.SaveAvatar(r.Context(), claims.UserID, data, userAvatarRoute(claims.UserID)); err != nil {
		switch {
		case errors.Is(err, service.ErrAvatarImage):
			render.Error(w, err, http.StatusBadRequest)
		case errors.Is(err, storageif.ErrNotFound):
			render.Error(w, err, http.StatusNotFound)
		default:
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionProfileUpdate, ResourceType: "user", ResourceID: claims.UserID,
		Meta: map[string]string{"field": "avatar", "change": "uploaded"}})
	echo, _ := h.profileEcho(r.Context(), claims)
	render.JSON(w, echo)
}

// profileEcho reads the caller's stored profile for an avatar endpoint's answer.
func (h *AuthHandler) profileEcho(ctx context.Context, claims *auth.Claims) (profileEcho, error) {
	echo := profileEcho{ID: claims.UserID, Email: claims.Email, Role: string(claims.Role)}
	user, err := h.authSvc.User(ctx, claims.UserID)
	if err != nil {
		return echo, nil // the claims' core answers; the stamps are best-effort, as /me treats them
	}
	echo.DisplayName, echo.AvatarURL = user.DisplayName, user.AvatarURL
	return echo, nil
}

// @Summary      Get a user's avatar image
// @Description  Serves the stored avatar of any signed-in user (avatars appear cross-user: comment authors, the admin list). 404 when the user never uploaded one; the ETag is the image's updated stamp, so a re-upload invalidates the cache.
// @Tags         users
// @Param        id path string true "User ID"
// @Produce      image/png
// @Produce      image/jpeg
// @Success      200 {object} string "The image bytes"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      404 {object} map[string]string "No uploaded avatar"
// @Router       /api/users/{id}/avatar [get]
func (h *AuthHandler) handleUserAvatar(w http.ResponseWriter, r *http.Request) {
	if auth.ClaimsFromContext(r.Context()) == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	avatar, err := h.authSvc.Avatar(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	etag := `"` + strconv.FormatInt(avatar.UpdatedAt.UnixNano(), 16) + `"`
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", avatar.MimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(avatar.Data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(avatar.Data) // the client is gone or the connection broke; nothing to clean up behind bytes
}

// renderAvatarBodyError answers a multipart parse/read failure: over the cap is 413, anything else a 400.
func renderAvatarBodyError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		render.Error(w, fmt.Errorf("avatar upload exceeds the size limit"), http.StatusRequestEntityTooLarge)
		return
	}
	render.Error(w, fmt.Errorf("avatar upload must be a multipart form"), http.StatusBadRequest)
}
