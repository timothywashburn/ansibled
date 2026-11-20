package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
)

//go:embed static/*
var staticFS embed.FS

// assets are the embedded stylesheet, script and icon, served under
// /static/<name> with a content hash in the query string so they can be
// cached for a long time.
type assets struct {
	files map[string][]byte
	hash  map[string]string
}

func loadAssets() (*assets, error) {
	a := &assets{files: map[string][]byte{}, hash: map[string]string{}}
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		a.files[e.Name()] = b
		a.hash[e.Name()] = hex.EncodeToString(sum[:6])
	}
	return a, nil
}

// url is the path of an asset including its content hash.
func (a *assets) url(name string) string {
	return "/static/" + name + "?v=" + a.hash[name]
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, ok := s.assets.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("v") == s.assets.hash[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(b)
}
