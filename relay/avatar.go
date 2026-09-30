// Avatars: one small picture per mailbox, uploaded by the owner and readable by anyone.
//
// Friends and group members show it next to the owner's messages. It lives on the post office
// (not on the owner's own machine) so it shows even when that machine is offline or not
// reachable from outside -- the post office is the one place every member already talks to.
//
//	POST   /box/avatar?box=   owner-signed; body = the image, Content-Type = its type
//	DELETE /box/avatar?box=   owner-signed; back to "no avatar"
//	GET    /avatar?box=       public; 404 when none. ETag = sha256, so clients can revalidate
//
// Only small raster images and sanitized SVG are accepted: the picture is shown to other people,
// so it must not be able to run script or pull in outside resources.
package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MaxAvatarBytes bounds one avatar (a 256px PNG or a hand-sized SVG fits comfortably).
const MaxAvatarBytes = 256 << 10

var avatarTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/svg+xml": true}

// svgUnsafe matches script, event handlers, embedded HTML and outside references.
var svgUnsafe = regexp.MustCompile(`(?i)<\s*script|<\s*foreignObject|<\s*iframe|<\s*embed|<\s*object|\son[a-z]+\s*=|javascript:|(?:xlink:)?href\s*=\s*["']\s*(?:https?:|//|data:text)|@import|url\(\s*["']?\s*(?:https?:|//)`)

type avatarMeta struct {
	Type    string    `json:"type"`
	SHA256  string    `json:"sha256"`
	Size    int       `json:"size"`
	Updated time.Time `json:"updated"`
}

func (s *Server) avatarDir() string { return filepath.Join(s.dataDir, "avatar") }
func (s *Server) avatarPaths(box string) (string, string) {
	return filepath.Join(s.avatarDir(), box+".img"), filepath.Join(s.avatarDir(), box+".json")
}

// mountAvatar registers the avatar routes; called from mountCore.
func (s *Server) mountAvatar(must func(error)) {
	must(s.HandleFunc("POST /box/avatar", s.boxAvatarPut))
	must(s.HandleFunc("DELETE /box/avatar", s.boxAvatarDelete))
	must(s.HandleFunc("GET /avatar", s.avatarGet))
}

func (s *Server) boxAvatarPut(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if err := s.authBox(r, "POST", "/box/avatar", box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if !avatarTypes[ct] {
		WriteError(w, 415, "avatar must be png, jpeg, webp or svg")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxAvatarBytes+1))
	if err != nil {
		WriteError(w, 400, "failed to read avatar")
		return
	}
	if len(raw) == 0 || len(raw) > MaxAvatarBytes {
		WriteError(w, 413, "avatar is empty or too large")
		return
	}
	if ct == "image/svg+xml" {
		if !bytes.Contains(bytes.ToLower(raw), []byte("<svg")) || svgUnsafe.Match(raw) {
			WriteError(w, 400, "svg must not contain script or outside references")
			return
		}
	} else if got := http.DetectContentType(raw); got != ct {
		WriteError(w, 400, "image content does not match its type")
		return
	}
	sum := sha256.Sum256(raw)
	meta := avatarMeta{Type: ct, SHA256: hex.EncodeToString(sum[:]), Size: len(raw), Updated: time.Now().UTC()}
	img, mp := s.avatarPaths(box)
	if err := os.MkdirAll(s.avatarDir(), 0o755); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	if err := writeFileAtomic(img, raw); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	mb, _ := json.Marshal(meta)
	if err := writeFileAtomic(mp, mb); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "sha256": meta.SHA256})
}

func (s *Server) boxAvatarDelete(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if err := s.authBox(r, "DELETE", "/box/avatar", box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	img, mp := s.avatarPaths(box)
	_ = os.Remove(mp)
	_ = os.Remove(img)
	WriteJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) avatarGet(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	img, mp := s.avatarPaths(box)
	var meta avatarMeta
	mb, err := os.ReadFile(mp)
	if err != nil || json.Unmarshal(mb, &meta) != nil || !avatarTypes[meta.Type] {
		w.Header().Set("Cache-Control", "max-age=300")
		WriteError(w, 404, "no avatar")
		return
	}
	etag := `"` + meta.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "max-age=300")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	raw, err := os.ReadFile(img)
	if err != nil {
		WriteError(w, 404, "no avatar")
		return
	}
	w.Header().Set("Content-Type", meta.Type)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	_, _ = w.Write(raw)
}
