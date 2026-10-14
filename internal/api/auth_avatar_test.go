package api

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// avatarPNG renders a small valid PNG for upload tests.
func avatarPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.RGBA{R: 0x40, G: 0x60, B: 0x90, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// avatarMultipart builds an avatar upload request body with the given part name and content.
func avatarMultipart(t *testing.T, field string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, "me.png")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func doAvatarUpload(t *testing.T, rt *Router, bearer, field string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := avatarMultipart(t, field, content)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/profile/avatar", body)
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// TestAvatarUploadAndServe is the full flow: upload sets the profile's avatar URL to the serving route, the route
// serves the stored bytes to any signed-in caller with the ETag handshake, and removing the upload clears it.
func TestAvatarUploadAndServe(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "avatar-uploader@example.com", "StrongPass123!")
	access, _ := resp["accessToken"].(string)
	bearer := "Bearer " + access
	userID, _ := resp["user"].(map[string]any)["id"].(string)
	route := "/api/users/" + userID + "/avatar"

	rr := doAvatarUpload(t, rt, bearer, "avatar", avatarPNG(t))
	checkStatus(t, rr, http.StatusOK)
	var profile struct {
		DisplayName string `json:"displayName"`
		AvatarURL   string `json:"avatarUrl"`
	}
	decodeJSON(t, rr, &profile)
	if profile.AvatarURL != route {
		t.Errorf("upload answered avatarUrl %q, want the serving route %q", profile.AvatarURL, route)
	}
	if u, err := rt.security.Backend.LoadUserByID(context.Background(), userID); err != nil || u.AvatarURL != route {
		t.Errorf("stored profile: %+v (%v); want the avatar URL at %q", u, err, route)
	}

	// Any signed-in caller can read it — here the same user through the plain bearer.
	rr = doRequestWithAuth(t, rt, http.MethodGet, route, bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content type %q, want image/png", ct)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" || rr.Header().Get("Cache-Control") == "" {
		t.Errorf("serving headers missing: ETag %q, Cache-Control %q", etag, rr.Header().Get("Cache-Control"))
	}

	// The ETag answers 304.
	req := httptest.NewRequest(http.MethodGet, route, nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("If-None-Match", etag)
	rr304 := httptest.NewRecorder()
	rt.ServeHTTP(rr304, req)
	checkStatus(t, rr304, http.StatusNotModified)

	// A user without an upload is a 404, not an error.
	other := login(t, rt, "avatar-less@example.com", "StrongPass123!")
	otherRoute := "/api/users/" + other["user"].(map[string]any)["id"].(string) + "/avatar"
	rr = doRequestWithAuth(t, rt, http.MethodGet, otherRoute, bearer, nil)
	checkStatus(t, rr, http.StatusNotFound)

	// Removal clears what the upload set.
	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/profile/avatar", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	if u, err := rt.security.Backend.LoadUserByID(context.Background(), userID); err != nil || u.AvatarURL != "" {
		t.Errorf("avatar URL after delete: %q (%v); want cleared", u.AvatarURL, err)
	}
	rr = doRequestWithAuth(t, rt, http.MethodGet, route, bearer, nil)
	checkStatus(t, rr, http.StatusNotFound)
}

// TestAvatarUploadRefusals: no session, not an image, wrong part name, and over the size cap.
func TestAvatarUploadRefusals(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "avatar-refusals@example.com", "StrongPass123!")
	bearer := "Bearer " + resp["accessToken"].(string)

	rr := doAvatarUpload(t, rt, "", "avatar", avatarPNG(t))
	checkStatus(t, rr, http.StatusUnauthorized)

	rr = doAvatarUpload(t, rt, bearer, "avatar", []byte("definitely not an image"))
	checkStatus(t, rr, http.StatusBadRequest)

	rr = doAvatarUpload(t, rt, bearer, "photo", avatarPNG(t))
	checkStatus(t, rr, http.StatusBadRequest)

	// A body over the cap is 413, not a 500 from a surprise read error.
	huge := bytes.Repeat([]byte{0x89}, 6<<20)
	rr = doAvatarUpload(t, rt, bearer, "avatar", huge)
	checkStatus(t, rr, http.StatusRequestEntityTooLarge)

	// Nothing above was stored.
	if a, err := rt.security.Backend.LoadUserAvatar(context.Background(), resp["user"].(map[string]any)["id"].(string)); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("refused uploads stored something: %+v (%v)", a, err)
	}
}

// TestAvatarServeRequiresAuth: the serving route answers 401 without a session — the bytes never leave
// unauthenticated, which is why the frontend fetches them with the bearer instead of an <img src>.
func TestAvatarServeRequiresAuth(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "avatar-auth@example.com", "StrongPass123!")
	bearer := "Bearer " + resp["accessToken"].(string)
	userID := resp["user"].(map[string]any)["id"].(string)
	checkStatus(t, doAvatarUpload(t, rt, bearer, "avatar", avatarPNG(t)), http.StatusOK)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/users/"+userID+"/avatar", "", nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

// TestProfileUpdateKeepsUploadInStep: saving the profile with the upload's own route (what the form sends back
// when only the name changed) keeps the image; an external URL or an empty one replaces it, so the old image is
// not left served and exported; pointing at the route with no upload is refused.
func TestProfileUpdateKeepsUploadInStep(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "avatar-profile@example.com", "StrongPass123!")
	access, _ := resp["accessToken"].(string)
	bearer := "Bearer " + access
	userID, _ := resp["user"].(map[string]any)["id"].(string)
	route := "/api/users/" + userID + "/avatar"
	hasImage := func() bool {
		_, err := rt.security.Backend.LoadUserAvatar(context.Background(), userID)
		return err == nil
	}
	put := func(name, avatarURL string) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, rt, http.MethodPut, "/api/auth/profile", bearer,
			map[string]string{"displayName": name, "avatarUrl": avatarURL})
	}

	checkStatus(t, put("Nobody", route), http.StatusBadRequest) // no upload yet
	checkStatus(t, doAvatarUpload(t, rt, bearer, "avatar", avatarPNG(t)), http.StatusOK)

	checkStatus(t, put("Renamed", route), http.StatusOK)
	if !hasImage() {
		t.Error("saving the name with the upload's own route dropped the image")
	}

	checkStatus(t, put("Renamed", "https://img.example.com/me.png"), http.StatusOK)
	if hasImage() {
		t.Error("an external avatar URL left the uploaded image stored")
	}

	checkStatus(t, doAvatarUpload(t, rt, bearer, "avatar", avatarPNG(t)), http.StatusOK)
	checkStatus(t, put("Renamed", ""), http.StatusOK)
	if hasImage() {
		t.Error("clearing the avatar URL left the uploaded image stored")
	}
}

// TestSettingsWriteRefusesInvalidProfile: the profile section's caps and timezone are checked on the server (the
// frontend's checks are a courtesy), and a refusal is a 400, not a 500.
func TestSettingsWriteRefusesInvalidProfile(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "profile-settings@example.com", "StrongPass123!")
	access, _ := resp["accessToken"].(string)
	bearer := "Bearer " + access

	post := func(profile map[string]string) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings", bearer, map[string]any{"profile": profile})
	}
	checkStatus(t, post(map[string]string{"bio": "hello", "jobTitle": "RPA developer", "timezone": "Europe/Belgrade"}), http.StatusOK)
	checkStatus(t, post(map[string]string{"timezone": "Mars/Olympus"}), http.StatusBadRequest)
	checkStatus(t, post(map[string]string{"bio": strings.Repeat("x", 501)}), http.StatusBadRequest)
	checkStatus(t, post(map[string]string{"jobTitle": strings.Repeat("x", 101)}), http.StatusBadRequest)
}
