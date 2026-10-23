package media

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandlerBrowsesMultipleRootsAndRendersImagesAndVideo(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "Media & photos")
	second := filepath.Join(base, "Other media")
	if err := os.Mkdir(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(first, "Trips & events"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(first, "Trips & events", "photo <one>.png"): []byte("png-data"),
		filepath.Join(first, "Trips & events", "clip.mp4"):        []byte("0123456789"),
		filepath.Join(first, "Trips & events", "notes.txt"):       []byte("not served"),
		filepath.Join(first, ".secret.jpg"):                       []byte("hidden"),
		filepath.Join(second, "second.webp"):                      []byte("webp-data"),
	}
	for path, content := range files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server, err := New([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	handler := server.Handler()

	index := request(handler, http.MethodGet, "/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "Media &amp; photos") || !strings.Contains(index.Body.String(), "/browse/1/") {
		t.Fatalf("media roots missing from index: code=%d body=%s", index.Code, index.Body.String())
	}
	listing := request(handler, http.MethodGet, "/browse/0/")
	if listing.Code != http.StatusOK || !strings.Contains(listing.Body.String(), "Trips &amp; events/") || strings.Contains(listing.Body.String(), "secret.jpg") {
		t.Fatalf("root listing did not hide hidden files or show subdirectories: code=%d body=%s", listing.Code, listing.Body.String())
	}
	subdirectory := request(handler, http.MethodGet, "/browse/0/Trips%20%26%20events/")
	if subdirectory.Code != http.StatusOK || !strings.Contains(subdirectory.Body.String(), "photo &lt;one&gt;.png") || strings.Contains(subdirectory.Body.String(), "notes.txt") {
		t.Fatalf("media listing should escape filenames and omit unsupported files: code=%d body=%s", subdirectory.Code, subdirectory.Body.String())
	}
	imagePage := request(handler, http.MethodGet, "/browse/0/Trips%20%26%20events/photo%20%3Cone%3E.png")
	if imagePage.Code != http.StatusOK || !strings.Contains(imagePage.Body.String(), "<img") || !strings.Contains(imagePage.Body.String(), "/raw/0/Trips%20&amp;%20events/photo%20%3Cone%3E.png") {
		t.Fatalf("image did not render inline: code=%d body=%s", imagePage.Code, imagePage.Body.String())
	}
	videoPage := request(handler, http.MethodGet, "/browse/0/Trips%20%26%20events/clip.mp4")
	if videoPage.Code != http.StatusOK || !strings.Contains(videoPage.Body.String(), "<video controls") || !strings.Contains(videoPage.Body.String(), "preload=\"metadata\"") {
		t.Fatalf("video player missing: code=%d body=%s", videoPage.Code, videoPage.Body.String())
	}
}

func TestHandlerRejectsUntrustedHostsAndAllowsExplicitProxyHost(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "image.png"), []byte("pixels"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := New([]string{directory})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	request := httptest.NewRequest(http.MethodGet, "/raw/0/image.png", nil)
	request.Host = "attacker.example:8765"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest || strings.Contains(response.Body.String(), "pixels") {
		t.Fatalf("untrusted host response=%d body=%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/raw/0/image.png", nil)
	request.Host = "media.tailnet.example:8765"
	response = httptest.NewRecorder()
	server.Handler("media.tailnet.example").ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "pixels" {
		t.Fatalf("allowed proxy host response=%d body=%q", response.Code, response.Body.String())
	}
}

func TestHandlerStreamsMediaWithRangeAndSecurityHeaders(t *testing.T) {
	directory := t.TempDir()
	video := []byte("0123456789")
	if err := os.WriteFile(filepath.Join(directory, "clip.mp4"), video, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := New([]string{directory})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/raw/0/clip.mp4", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" || response.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("video range response=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
	}
	for name, want := range map[string]string{
		"Content-Type":                 "video/mp4",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
		"Content-Security-Policy":      "default-src 'self'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("header %s=%q, want %q", name, got, want)
		}
	}
}

func TestHandlerRejectsTraversalUnsupportedFilesAndWrites(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "media")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.jpg")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "notes.txt"), []byte("private text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "escape.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	server, err := New([]string{rootPath})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	handler := server.Handler()

	for _, target := range []string{
		"/browse/0/../outside.jpg",
		"/raw/0/../outside.jpg",
		"/raw/0/escape.jpg",
		"/raw/0/notes.txt",
		"/unknown/0/escape.jpg",
	} {
		response := request(handler, http.MethodGet, target)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", target, response.Code)
		}
	}
	response := request(handler, http.MethodPost, "/")
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("write method response=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

func TestHandlerDisambiguatesRootsWithTheSameDirectoryName(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first", "Media")
	second := filepath.Join(base, "second", "Media")
	for _, directory := range []string{first, second} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	server, err := New([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	index := request(server.Handler(), http.MethodGet, "/")
	for _, want := range []string{"Media (root 1)", "Media (root 2)"} {
		if !strings.Contains(index.Body.String(), want) {
			t.Errorf("index omitted disambiguated root %q: %s", want, index.Body.String())
		}
	}
}

func TestNewValidatesAndDeduplicatesDirectories(t *testing.T) {
	directory := t.TempDir()
	if _, err := New(nil); err == nil {
		t.Fatal("accepted no media directories")
	}
	if _, err := New([]string{filepath.Join(directory, "missing")}); err == nil {
		t.Fatal("accepted a missing directory")
	}
	valid, err := New([]string{directory})
	if err != nil {
		t.Fatalf("rejected valid directory: %v", err)
	}
	_ = valid.Close()
	if _, err := New([]string{directory, filepath.Join(directory, ".")}); err == nil {
		t.Fatal("accepted duplicate canonical directories")
	}
	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New([]string{file}); err == nil {
		t.Fatal("accepted a regular file as media directory")
	}
}

func request(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, nil)
	request.Host = "127.0.0.1"
	handler.ServeHTTP(response, request)
	return response
}
