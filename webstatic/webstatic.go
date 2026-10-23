// Package webstatic serves a built front end (SPA or plain static site) from an
// fs.FS. It owns exactly three things: cache headers, path safety, and the SPA
// fallback rule. Compression is deliberately out of scope; the ingress proxy
// terminates gzip/brotli so that the bytes served here stay range-seekable.
//
// Cache matrix (path is the cleaned request path):
//
//	index / *.html / *.htm            Cache-Control: no-store
//	<asset prefix>/**                 Cache-Control: public, max-age=<Immutable>, immutable
//	everything else                   Cache-Control: max-age=0, must-revalidate
//
// Files with a modification time get an ETag plus Last-Modified and are served
// through http.ServeContent, so conditional requests (304) and Range/If-Range
// work without extra code and http.Flusher keeps working on the raw writer.
//
// Path safety: the request path is rejected (404, never SPA fallback) if it
// contains a ".." element, a backslash or NUL, a drive-letter first element, or
// if the result is not an fs.ValidPath. That check happens before touching the
// fs.FS, so it holds for any implementation. Directories are never listed: a
// directory only serves its own <Index> file, otherwise it is a miss.
package webstatic

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

const defaultImmutable = 365 * 24 * time.Hour

// Options describes one site. Only FS is required.
type Options struct {
	FS            fs.FS         // site root; must not be nil
	Index         string        // default "index.html"
	SPA           bool          // serve Index for unmatched non-file paths
	Immutable     time.Duration // default 365*24h, used for AssetPrefixes
	AssetPrefixes []string      // default ["/assets/"]
	NotFound      http.Handler  // optional handler for misses that are not SPA routes
}

// Handler returns an http.Handler serving o.FS. It panics if o.FS is nil: that
// is a wiring bug, and failing at startup beats every request returning 500.
func Handler(o Options) http.Handler {
	if o.FS == nil {
		panic("webstatic: Options.FS is required")
	}
	if o.Index == "" {
		o.Index = "index.html"
	}
	if o.Immutable <= 0 {
		o.Immutable = defaultImmutable
	}
	prefixes := o.AssetPrefixes
	if len(prefixes) == 0 {
		prefixes = []string{"/assets/"}
	}
	assets := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		assets = append(assets, path.Clean(p)+"/")
	}
	return &site{
		fs:        o.FS,
		index:     o.Index,
		spa:       o.SPA,
		immutable: o.Immutable,
		assets:    assets,
		notFound:  o.NotFound,
	}
}

type site struct {
	fs        fs.FS
	index     string
	spa       bool
	immutable time.Duration
	assets    []string
	notFound  http.Handler
}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	cleaned, rel, ok := cleanPath(r.URL.Path)
	if !ok {
		s.serveNotFound(w, r)
		return
	}

	if rel != "" {
		if f, st, ok := s.open(rel); ok {
			s.serveFile(w, r, cleaned, f, st)
			return
		}
	}
	if rel == "" || rel == s.index {
		s.serveIndex(w, r)
		return
	}
	// SPA fallback only for paths that do not look like a file and are not
	// under an asset prefix: a missing foo.js must be a 404, otherwise a stale
	// front end gets index.html delivered under a javascript URL.
	if s.spa && !looksLikeFile(cleaned) && !s.isAsset(cleaned) {
		s.serveIndex(w, r)
		return
	}
	s.serveNotFound(w, r)
}

// cleanPath validates the request path and returns both the cleaned URL form
// ("/a/b") and the fs-relative name ("a/b", empty for "/").
func cleanPath(p string) (cleaned, rel string, ok bool) {
	if p == "" {
		p = "/"
	}
	if strings.ContainsAny(p, "\x00\\") {
		return "", "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", "", false
		}
	}
	cleaned = path.Clean("/" + p)
	if !strings.HasPrefix(cleaned, "/") {
		return "", "", false
	}
	if cleaned == "/" {
		return "/", "", true
	}
	rel = strings.TrimPrefix(cleaned, "/")
	if isDrivePath(rel) || !fs.ValidPath(rel) {
		return "", "", false
	}
	return cleaned, rel, true
}

// isDrivePath reports whether rel starts with a Windows volume name such as
// "C:/Windows", which would be absolute on that platform.
func isDrivePath(rel string) bool {
	if len(rel) < 2 || rel[1] != ':' {
		return false
	}
	c := rel[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// looksLikeFile reports whether the last element carries an extension. Dotfiles
// (".env") are not treated as files; a trailing dot is not an extension either.
func looksLikeFile(p string) bool {
	base := path.Base(p)
	i := strings.LastIndexByte(base, '.')
	return i > 0 && i < len(base)-1
}

func (s *site) isAsset(p string) bool {
	for _, prefix := range s.assets {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (s *site) cacheControl(p string) string {
	if isHTML(p, s.index) {
		// Login pages and anything rendered per-deploy must not be cached.
		return "no-store"
	}
	if s.isAsset(p) {
		return "public, max-age=" + strconv.FormatInt(int64(s.immutable/time.Second), 10) + ", immutable"
	}
	return "max-age=0, must-revalidate"
}

func isHTML(p, index string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return true
	}
	return p == "/" || path.Base(p) == index
}

// open resolves name to a regular file. A directory is not served: it is only
// tried as a directory index, so fs.FS contents can never be listed.
func (s *site) open(name string) (fs.File, fs.FileInfo, bool) {
	if f, st, ok := s.openFile(name); ok {
		return f, st, true
	}
	if f, st, ok := s.openFile(name + "/" + s.index); ok {
		return f, st, true
	}
	return nil, nil, false
}

func (s *site) openFile(name string) (fs.File, fs.FileInfo, bool) {
	if !fs.ValidPath(name) {
		return nil, nil, false
	}
	f, err := s.fs.Open(name)
	if err != nil {
		return nil, nil, false
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, false
	}
	return f, st, true
}

func (s *site) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, st, ok := s.openFile(s.index)
	if !ok {
		s.serveNotFound(w, r)
		return
	}
	s.serveFile(w, r, "/"+s.index, f, st)
}

func (s *site) serveFile(w http.ResponseWriter, r *http.Request, urlPath string, f fs.File, st fs.FileInfo) {
	defer f.Close()

	// fs.File only guarantees Read; embed.FS and os.DirFS also implement
	// io.Seeker, which lets ServeContent stream large files and ranges.
	var body io.ReadSeeker
	if rs, ok := f.(io.ReadSeeker); ok {
		body = rs
	} else {
		data, err := io.ReadAll(f)
		if err != nil {
			s.serveNotFound(w, r)
			return
		}
		body = bytes.NewReader(data)
	}

	h := w.Header()
	h.Set("Cache-Control", s.cacheControl(urlPath))
	setContentType(h, urlPath)
	if mod := st.ModTime(); !mod.IsZero() {
		// ServeContent validates If-None-Match/If-Modified-Since against these.
		h.Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("%x-%x", mod.UnixNano(), st.Size())))
	}
	name := path.Base(urlPath)
	if name == "" || name == "." || name == "/" {
		name = st.Name()
	}
	http.ServeContent(w, r, name, st.ModTime(), body)
}

func (s *site) serveNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.notFound != nil {
		s.notFound.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// setContentType pins the types that matter for a front end. Without this,
// mime.TypeByExtension consults the Windows registry, where .js is not
// guaranteed to be JavaScript.
func setContentType(h http.Header, p string) {
	if h.Get("Content-Type") != "" {
		return
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		h.Set("Content-Type", "text/html; charset=utf-8")
	case ".js", ".mjs":
		h.Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		h.Set("Content-Type", "text/css; charset=utf-8")
	case ".json", ".map":
		h.Set("Content-Type", "application/json")
	case ".webmanifest":
		h.Set("Content-Type", "application/manifest+json")
	case ".svg":
		h.Set("Content-Type", "image/svg+xml")
	case ".wasm":
		h.Set("Content-Type", "application/wasm")
	case ".txt":
		h.Set("Content-Type", "text/plain; charset=utf-8")
	case ".xml":
		h.Set("Content-Type", "application/xml; charset=utf-8")
	}
}
