package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Static assets are served under a name that contains a digest of their
// contents, e.g. /static/app.4f2a1c9d.css.
//
// The previous arrangement served them under their plain names with an hour of
// cache, which means every visitor asks again every hour and is told nothing
// changed — a round trip each, for files that have not moved between deploys.
// A name that changes when the bytes change can be cached permanently instead,
// because a new version arrives under a new name rather than by expiry. The
// trade is that templates cannot hardcode the path, which is what assetPath is
// for.
//
// Plain names keep working. Anything already referencing /static/app.css — a
// bookmark, a cached page from before a deploy — still gets the file, just
// without the long cache.

// asset is one embedded file under both of its names.
type asset struct {
	// hashed is the digest-bearing path, e.g. "app.4f2a1c9d.css".
	hashed string
	body   []byte
	ctype  string
	etag   string
}

type assets struct {
	// byName maps both the plain and the hashed name to the file.
	byName map[string]*asset
	// paths maps a plain name to the URL templates should emit.
	paths map[string]string
}

func loadAssets(fsys fs.FS) (*assets, error) {
	a := &assets{byName: map[string]*asset{}, paths: map[string]string{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:4])

		ext := path.Ext(p)
		hashed := strings.TrimSuffix(p, ext) + "." + digest + ext
		f := &asset{
			hashed: hashed,
			body:   body,
			ctype:  contentType(ext),
			etag:   `"` + hex.EncodeToString(sum[:16]) + `"`,
		}

		a.byName[p] = f
		a.byName[hashed] = f
		a.paths[p] = "/static/" + hashed
		return nil
	})
	return a, err
}

// contentType names the types actually shipped. http.DetectContentType reads
// an SVG as text/xml and a stylesheet as text/plain, either of which a browser
// refuses to use for what it was asked for.
func contentType(ext string) string {
	switch ext {
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".png":
		return "image/png"
	case ".woff2":
		return "font/woff2"
	}
	return "application/octet-stream"
}

// assetPath is the template helper. An unknown name is passed through under its
// plain path rather than rendering empty, so a typo is a missing file in the
// browser's console instead of a page with no styling and no explanation.
func (a *assets) path(name string) string {
	if p, ok := a.paths[name]; ok {
		return p
	}
	return "/static/" + name
}

func (a *assets) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		f, ok := a.byName[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", f.ctype)
		// Serving the plain name is the case this matters for: it cannot be
		// cached forever, so the next best thing is that revalidating it costs a
		// 304 with no body. ServeContent does the If-None-Match comparison.
		h.Set("ETag", f.etag)
		// Immutable only under the hashed name. The plain name addresses
		// whatever the current build happens to be, so a year of caching would
		// pin a visitor to the version they first saw.
		if name == f.hashed {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "public, max-age=3600")
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.body))
	})
}
