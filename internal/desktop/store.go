// Package desktop proxies the guest Driver and serves transient screenshots.
package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/png" // Register the only supported screenshot format.
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// MaxImageBytes bounds a single encoded screenshot.
	MaxImageBytes       = 16 << 20
	maxStoreBytes       = 128 << 20
	maxImagePixels      = 32 << 20
	screenshotRetention = 5 * time.Minute
	pngFormat           = "png"
	// ScreenshotPath is the HTTP route shared by both transports.
	ScreenshotPath = "/screenshots/"
)

// Screenshot describes a published PNG without carrying its bytes.
type Screenshot struct {
	// URL is built only from the configured public base URL.
	URL string
	// Width is the encoded image width in pixels.
	Width int
	// Height is the encoded image height in pixels.
	Height int
	// Scale maps image pixels to the Driver's coordinate frame.
	Scale float64
}

type storedImage struct {
	path    string
	sandbox string
	expires time.Time
	bytes   int64
}

// Store keeps bounded PNG files and an ephemeral in-memory index.
type Store struct {
	mu      sync.Mutex
	dir     string
	lock    *os.File
	baseURL string
	images  map[string]storedImage
	bytes   int64
	closed  bool
	now     func() time.Time
}

// NewStore exclusively owns and clears a dedicated scratch child of dir.
// Unrelated contents of dir are preserved; another live store is refused.
func NewStore(dir, baseURL string) (*Store, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" {
		return nil, errors.New(
			"screenshots.base_url must be an absolute HTTP or HTTPS URL without credentials, query, or fragment",
		)
	}
	if dir == "" {
		return nil, errors.New("screenshots.dir is required")
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create screenshot directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".agentcompute-screenshots.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open screenshot lock: %w", err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("screenshot directory is already in use: %w", err)
	}
	scratch := filepath.Join(dir, ".agentcompute-screenshots")
	if err = os.RemoveAll(scratch); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("clear screenshot scratch: %w", err)
	}
	if err = os.Mkdir(scratch, 0o700); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("create screenshot scratch: %w", err)
	}
	return &Store{
		dir:     scratch,
		lock:    lock,
		baseURL: strings.TrimRight(baseURL, "/"),
		images:  make(map[string]storedImage),
		now:     time.Now,
	}, nil
}

// Publish stores one PNG until five minutes or sandbox expiry, whichever is sooner.
// Oversized images and exhausted capacity fail without evicting live screenshots.
func (s *Store) Publish(sandbox string, sandboxExpiry time.Time, source io.Reader) (Screenshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Screenshot{}, errors.New("screenshot store is closed")
	}
	now := s.now()
	s.expireLocked(now)
	expires := now.Add(screenshotRetention)
	if sandboxExpiry.Before(expires) {
		expires = sandboxExpiry
	}
	if !expires.After(now) {
		return Screenshot{}, errors.New("sandbox expired before screenshot publication")
	}
	file, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return Screenshot{}, fmt.Errorf("create screenshot: %w", err)
	}
	pending := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(pending) }()
	n, err := io.Copy(file, io.LimitReader(source, MaxImageBytes+1))
	if err != nil {
		return Screenshot{}, fmt.Errorf("copy screenshot: %w", err)
	}
	if n > MaxImageBytes {
		return Screenshot{}, errors.New("screenshot exceeds 16 MiB")
	}
	if n > maxStoreBytes-s.bytes {
		return Screenshot{}, errors.New("screenshot store exceeds 128 MiB")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return Screenshot{}, fmt.Errorf("rewind screenshot: %w", err)
	}
	config, format, err := image.DecodeConfig(file)
	if err != nil || format != pngFormat {
		return Screenshot{}, errors.New("screenshot is not a valid PNG header")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > maxImagePixels/int64(config.Height) {
		return Screenshot{}, errors.New("screenshot exceeds pixel bounds")
	}
	if err = file.Close(); err != nil {
		return Screenshot{}, fmt.Errorf("close screenshot: %w", err)
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return Screenshot{}, fmt.Errorf("generate screenshot identifier: %w", err)
	}
	id := hex.EncodeToString(token[:])
	path := filepath.Join(s.dir, id)
	if err = os.Rename(pending, path); err != nil {
		return Screenshot{}, fmt.Errorf("publish screenshot: %w", err)
	}
	s.images[id] = storedImage{path: path, sandbox: sandbox, expires: expires, bytes: n}
	s.bytes += n
	return Screenshot{URL: s.baseURL + ScreenshotPath + id, Width: config.Width, Height: config.Height, Scale: 1}, nil
}

// PurgeSandbox removes every screenshot owned by the named sandbox.
func (s *Store) PurgeSandbox(sandbox string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.images {
		if entry.sandbox == sandbox {
			s.removeLocked(id, entry)
		}
	}
}

// Sweep removes expired screenshots and releases their capacity.
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
}

func (s *Store) expireLocked(now time.Time) {
	for id, entry := range s.images {
		if !entry.expires.After(now) {
			s.removeLocked(id, entry)
		}
	}
}

func (s *Store) removeLocked(id string, entry storedImage) {
	_ = os.Remove(entry.path)
	delete(s.images, id)
	s.bytes -= entry.bytes
}

// ServeHTTP serves GET and HEAD only; screenshot identifiers are bearer URLs.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, ScreenshotPath)
	if !strings.HasPrefix(r.URL.Path, ScreenshotPath) || len(id) != 32 {
		http.NotFound(w, r)
		return
	}
	if _, err := hex.DecodeString(id); err != nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.expireLocked(s.now())
	entry, exists := s.images[id]
	var file *os.File
	if exists && !s.closed {
		file, _ = os.Open(entry.path)
	}
	s.mu.Unlock()
	if file == nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "image/png")
	http.ServeContent(w, r, "screenshot.png", time.Time{}, file)
}

// Close drops the ephemeral index and removes this store's scratch directory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	clear(s.images)
	s.bytes = 0
	return errors.Join(os.RemoveAll(s.dir), s.lock.Close())
}
