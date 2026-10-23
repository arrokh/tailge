// Package media implements Tailge's read-only local media browser.
package media

import (
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Server exposes selected directory trees as a read-only image and video
// browser. Each tree is opened as an os.Root so symlinks cannot escape it.
type Server struct {
	roots []mediaRoot
}

type mediaRoot struct {
	id       string
	name     string
	baseName string
	files    *os.Root
}

type entry struct {
	Name    string
	URL     string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

type pageData struct {
	Title       string
	Breadcrumbs []entry
	Entries     []entry
	MediaURL    string
	IsImage     bool
	IsVideo     bool
	ParentURL   string
}

var indexTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Tailge Media</title><style>
:root{color-scheme:dark light;font:16px system-ui,sans-serif}body{max-width:1000px;margin:2rem auto;padding:0 1rem}h1{font-size:1.7rem}a{color:#1769aa}.roots{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:1rem}.root{border:1px solid #8888;border-radius:10px;padding:1rem}.muted{opacity:.72}.path{overflow-wrap:anywhere}ul{padding-left:1.5rem}li{padding:.35rem 0}.preview{max-width:100%;max-height:78vh}video{width:100%;max-height:78vh;background:#111}table{border-collapse:collapse;width:100%}td{padding:.45rem .35rem;border-bottom:1px solid #8884}td:last-child{text-align:right;white-space:nowrap}
</style></head><body><header><h1>Tailge Media</h1><p class="muted">Read-only local media browser</p></header><main><section class="roots"><h2>Media folders</h2>{{range .Entries}}<article class="root"><h3><a href="{{.URL}}">{{.Name}}</a></h3><p class="muted">Browse images and videos</p></article>{{end}}</section></main></body></html>`))

var browseTemplate = template.Must(template.New("browse").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title><style>
:root{color-scheme:dark light;font:16px system-ui,sans-serif}body{max-width:1000px;margin:2rem auto;padding:0 1rem}h1{font-size:1.7rem}a{color:#1769aa}.muted{opacity:.72}.path{overflow-wrap:anywhere}ul{padding-left:1.5rem}li{padding:.35rem 0}.preview{max-width:100%;max-height:78vh}video{width:100%;max-height:78vh;background:#111}table{border-collapse:collapse;width:100%}td{padding:.45rem .35rem;border-bottom:1px solid #8884}td:last-child{text-align:right;white-space:nowrap}.toolbar{display:flex;gap:1rem;flex-wrap:wrap}
</style></head><body><header><p><a href="/">All media folders</a></p><h1>{{.Title}}</h1><p class="muted path">{{range $index, $crumb := .Breadcrumbs}}{{if $index}} / {{end}}<a href="{{$crumb.URL}}">{{$crumb.Name}}</a>{{end}}</p></header><main>
{{if .IsImage}}<p><a href="{{.MediaURL}}">Open image directly</a></p><img class="preview" src="{{.MediaURL}}" alt="{{.Title}}">
{{else if .IsVideo}}<video controls playsinline preload="metadata" src="{{.MediaURL}}">Your browser cannot play this video format.</video><p><a href="{{.MediaURL}}">Open video directly</a></p>
{{else}}<nav class="toolbar">{{if .ParentURL}}<a href="{{.ParentURL}}">Parent folder</a>{{end}}</nav><table><tbody>{{range .Entries}}<tr><td>{{if .IsDir}}📁{{else}}🎞️{{end}} <a href="{{.URL}}">{{.Name}}{{if .IsDir}}/{{end}}</a></td><td>{{if not .IsDir}}{{.Size}} bytes{{end}}</td></tr>{{else}}<tr><td>No supported images or videos in this folder.</td><td></td></tr>{{end}}</tbody></table>{{end}}
</main></body></html>`))

// New validates and opens one or more media directories. Duplicate directories
// are rejected to keep each browser entry unambiguous.
func New(paths []string) (*Server, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("provide at least one media directory")
	}
	server := &Server{roots: make([]mediaRoot, 0, len(paths))}
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			server.Close()
			return nil, fmt.Errorf("media directory path cannot be empty")
		}
		absolute, err := filepath.Abs(raw)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("resolve media directory %q: %w", raw, err)
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("resolve media directory %q: %w", raw, err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("inspect media directory %q: %w", raw, err)
		}
		if !info.IsDir() {
			server.Close()
			return nil, fmt.Errorf("media path %q is not a directory", raw)
		}
		if _, exists := seen[canonical]; exists {
			server.Close()
			return nil, fmt.Errorf("media directory %q was provided more than once", raw)
		}
		seen[canonical] = struct{}{}
		root, err := os.OpenRoot(canonical)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("open media directory %q: %w", raw, err)
		}
		baseName := filepath.Base(canonical)
		duplicate := false
		for index := range server.roots {
			if server.roots[index].baseName == baseName {
				duplicate = true
				server.roots[index].name = fmt.Sprintf("%s (root %d)", baseName, index+1)
			}
		}
		name := baseName
		id := strconv.Itoa(len(server.roots))
		if duplicate {
			name = fmt.Sprintf("%s (root %d)", baseName, len(server.roots)+1)
		}
		server.roots = append(server.roots, mediaRoot{id: id, name: name, baseName: baseName, files: root})
	}
	return server, nil
}

// Close releases the directory handles held by the server.
func (s *Server) Close() error {
	var first error
	if s == nil {
		return nil
	}
	for _, root := range s.roots {
		if err := root.files.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Handler returns the HTTP handler for the media browser.
func (s *Server) Handler(allowedHosts ...string) http.Handler {
	allowed := map[string]struct{}{
		"localhost": {},
		"127.0.0.1": {},
		"::1":       {},
	}
	for _, host := range allowedHosts {
		if normalized := normalizeHost(host); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	return securityHeaders(restrictHost(allowed, http.HandlerFunc(s.serveHTTP)))
}

func restrictHost(allowed map[string]struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(r.Host)
		if _, ok := allowed[host]; !ok {
			http.Error(w, "request host is not allowed", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeHost(value string) string {
	host := value
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" {
		s.serveIndex(w, r)
		return
	}
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segments) < 2 || (segments[0] != "browse" && segments[0] != "raw") {
		http.NotFound(w, r)
		return
	}
	root := s.rootByID(segments[1])
	if root == nil {
		http.NotFound(w, r)
		return
	}
	relative, ok := relativePath(segments[2:])
	if !ok {
		http.NotFound(w, r)
		return
	}
	if segments[0] == "raw" {
		s.serveRaw(w, r, root, relative)
		return
	}
	s.serveBrowse(w, r, root, relative)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	entries := make([]entry, 0, len(s.roots))
	for _, root := range s.roots {
		entries = append(entries, entry{Name: root.name, URL: browseURL(&root, ".")})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexTemplate.Execute(w, pageData{Title: "Tailge Media", Entries: entries})
}

func (s *Server) serveBrowse(w http.ResponseWriter, r *http.Request, root *mediaRoot, relative string) {
	file, err := root.files.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		mediaType, supported := supportedMediaType(info.Name())
		if !supported {
			http.NotFound(w, r)
			return
		}
		parent := parentRelative(relative)
		data := pageData{
			Title:       info.Name(),
			Breadcrumbs: breadcrumbs(root, parent), MediaURL: rawURL(root, relative),
			IsImage: strings.HasPrefix(mediaType, "image/"), IsVideo: strings.HasPrefix(mediaType, "video/"),
			ParentURL: browseURL(root, parent),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = browseTemplate.Execute(w, data)
		return
	}
	children, err := file.ReadDir(-1)
	if err != nil {
		http.Error(w, "cannot read media directory", http.StatusInternalServerError)
		return
	}
	entries := make([]entry, 0, len(children))
	for _, child := range children {
		name := child.Name()
		if strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
			continue
		}
		childRelative := name
		if relative != "." {
			childRelative = relative + "/" + name
		}
		childFile, openErr := root.files.OpenFile(childRelative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			continue
		}
		childInfo, statErr := childFile.Stat()
		_ = childFile.Close()
		if statErr != nil {
			continue
		}
		if childInfo.IsDir() {
			entries = append(entries, entry{Name: name, URL: browseURL(root, childRelative), IsDir: true, ModTime: childInfo.ModTime()})
			continue
		}
		if !childInfo.Mode().IsRegular() {
			continue
		}
		if _, supported := supportedMediaType(name); !supported {
			continue
		}
		entries = append(entries, entry{Name: name, URL: browseURL(root, childRelative), Size: childInfo.Size(), ModTime: childInfo.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	data := pageData{
		Title:       root.name,
		Breadcrumbs: breadcrumbs(root, relative), Entries: entries,
	}
	if relative != "." {
		data.ParentURL = browseURL(root, parentRelative(relative))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = browseTemplate.Execute(w, data)
}

func (s *Server) serveRaw(w http.ResponseWriter, r *http.Request, root *mediaRoot, relative string) {
	file, err := root.files.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	mediaType, supported := supportedMediaType(info.Name())
	if !supported {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) rootByID(id string) *mediaRoot {
	index, err := strconv.Atoi(id)
	if err != nil || index < 0 || index >= len(s.roots) || s.roots[index].id != id {
		return nil
	}
	return &s.roots[index]
}

func relativePath(segments []string) (string, bool) {
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		return ".", true
	}
	if segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\") {
			return "", false
		}
	}
	relative := strings.Join(segments, "/")
	return relative, fs.ValidPath(relative)
}

func supportedMediaType(name string) (string, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".avif":
		return "image/avif", true
	case ".bmp":
		return "image/bmp", true
	case ".mp4", ".m4v":
		return "video/mp4", true
	case ".webm":
		return "video/webm", true
	case ".ogv":
		return "video/ogg", true
	case ".mov":
		return "video/quicktime", true
	default:
		return "", false
	}
}

func browseURL(root *mediaRoot, relative string) string {
	return "/browse/" + root.id + "/" + escapedRelative(relative)
}

func rawURL(root *mediaRoot, relative string) string {
	return "/raw/" + root.id + "/" + escapedRelative(relative)
}

func escapedRelative(relative string) string {
	if relative == "." || relative == "" {
		return ""
	}
	segments := strings.Split(relative, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return strings.Join(segments, "/")
}

func parentRelative(relative string) string {
	if relative == "." || relative == "" {
		return "."
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
	if parent == "" {
		return "."
	}
	return parent
}

func breadcrumbs(root *mediaRoot, relative string) []entry {
	result := []entry{{Name: root.name, URL: browseURL(root, ".")}}
	if relative == "." || relative == "" {
		return result
	}
	current := ""
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." {
			continue
		}
		if current == "" {
			current = segment
		} else {
			current += "/" + segment
		}
		result = append(result, entry{Name: segment, URL: browseURL(root, current)})
	}
	return result
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
