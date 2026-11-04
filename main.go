// navidrome-upload takes resumable (tus) uploads of music files and zip
// archives, has beets tag and file them into a Navidrome library, and starts a
// Navidrome scan of the folders it wrote.
//
// There are no accounts of its own: the page runs on Navidrome's origin and
// sends the web UI's login token, which auth() checks with Navidrome.
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// Where Navidrome answers, and where this container mounts the music: the
// same path Navidrome mounts it at, since library paths come from Navidrome.
var (
	navidromeURL = envOr("NAVIDROME_URL", "http://navidrome:4533")
	musicRoot    = strings.TrimSuffix(envOr("MUSIC_DIR", "/music"), "/") + "/"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const (
	uploadsDir = "/staging/uploads"
	jobsDir    = "/staging/jobs"
	// Uploads waiting for import plus the zips being unpacked share this.
	stagingQuota = 2 << 30
	maxFileSize  = 1 << 30
	uploadTTL    = 24 * time.Hour
	authCacheTTL = 30 * time.Second
)

//go:embed static
var static embed.FS

type library struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type session struct {
	libs    []library
	expires time.Time
}

var (
	sessionsMu sync.Mutex
	sessions   = map[[32]byte]session{}
	errAuth    = errors.New("not logged in to Navidrome as an admin")
)

// auth checks the page's Navidrome token by asking Navidrome for its
// libraries. That endpoint is admin-only, so a 200 means the token is valid,
// not revoked, and an admin's. Every tus chunk is authenticated, so answers are
// cached briefly. The returned header carries a refreshed token when Navidrome
// issued one.
func auth(r *http.Request) ([]library, string, error) {
	token := r.Header.Get("X-ND-Authorization")
	if !strings.HasPrefix(token, "Bearer ") {
		return nil, "", errAuth
	}
	key := sha256.Sum256([]byte(token))
	sessionsMu.Lock()
	s, ok := sessions[key]
	sessionsMu.Unlock()
	if ok && time.Now().Before(s.expires) {
		return s.libs, "", nil
	}

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, navidromeURL+"/api/library", nil)
	req.Header.Set("X-ND-Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errAuth
	}
	var libs []library
	if err := json.NewDecoder(resp.Body).Decode(&libs); err != nil {
		return nil, "", err
	}
	sessionsMu.Lock()
	sessions[key] = session{libs, time.Now().Add(authCacheTTL)}
	sessionsMu.Unlock()
	return libs, resp.Header.Get("X-ND-Authorization"), nil
}

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := auth(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Extensions accepted at upload, each with the content sniff() must find.
// Audio only: cover art comes embedded in the files or from MusicBrainz.
var kinds = map[string]string{
	".mp3": "mp3", ".flac": "flac", ".m4a": "m4a", ".ogg": "ogg", ".opus": "ogg",
	".zip": "zip",
}

// createMu makes the quota check and the upload's creation one step.
var createMu sync.Mutex

func preCreate(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	up := hook.Upload
	if up.SizeIsDeferred {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("ERR_SIZE_REQUIRED", "upload size must be known", http.StatusBadRequest)
	}
	if kinds[strings.ToLower(filepath.Ext(up.MetaData["filename"]))] == "" {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("ERR_TYPE", "only mp3, flac, m4a, ogg, opus and zip files", http.StatusUnsupportedMediaType)
	}
	createMu.Lock()
	defer createMu.Unlock()
	if stagingUsed()+up.Size > stagingQuota {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError("ERR_QUOTA", "upload space is full; import or wait for pending uploads", http.StatusInsufficientStorage)
	}
	// Keep only what the importer reads.
	meta := tusd.MetaData{"filename": up.MetaData["filename"], "relativePath": up.MetaData["relativePath"]}
	return tusd.HTTPResponse{}, tusd.FileInfoChanges{MetaData: meta}, nil
}

// stagingUsed counts uploads at their declared size, so space is reserved
// before the bytes arrive, plus whatever import jobs have unpacked.
func stagingUsed() int64 {
	var n int64
	infos, _ := filepath.Glob(filepath.Join(uploadsDir, "*.info"))
	for _, p := range infos {
		if info, err := readInfo(p); err == nil {
			n += info.Size
		}
	}
	filepath.WalkDir(jobsDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func readInfo(path string) (tusd.FileInfo, error) {
	var info tusd.FileInfo
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &info)
	}
	return info, err
}

// sweep drops uploads nobody touched within uploadTTL and expired auth cache
// entries.
func sweep() {
	for range time.Tick(time.Hour) {
		infos, _ := filepath.Glob(filepath.Join(uploadsDir, "*.info"))
		for _, p := range infos {
			data := strings.TrimSuffix(p, ".info")
			fi, err := os.Stat(data)
			if err != nil || time.Since(fi.ModTime()) > uploadTTL {
				os.Remove(data)
				os.Remove(p)
				os.Remove(data + ".lock")
			}
		}
		sessionsMu.Lock()
		for k, s := range sessions {
			if time.Now().After(s.expires) {
				delete(sessions, k)
			}
		}
		sessionsMu.Unlock()
		jobsMu.Lock()
		for id, j := range jobs {
			if j.finished() && time.Since(j.Started) > time.Hour {
				delete(jobs, id)
			}
		}
		jobsMu.Unlock()
	}
}

// The page shares Navidrome's origin and so can read its login token; it gets
// a strict policy so nothing but its own files can run there.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	// A job dir left by a crash holds files nobody will import.
	os.RemoveAll(jobsDir)
	for _, d := range []string{uploadsDir, jobsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Error("staging", "err", err)
			os.Exit(1)
		}
	}

	store := filestore.New(uploadsDir)
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)
	filelocker.New(uploadsDir).UseIn(composer)
	tus, err := tusd.NewHandler(tusd.Config{
		BasePath:      "/upload/files/",
		StoreComposer: composer,
		MaxSize:       maxFileSize,
		// Serving uploaded bytes back on Navidrome's origin would let an
		// uploaded file run as a page there.
		DisableDownload:         true,
		DisableConcatenation:    true,
		RespectForwardedHeaders: true,
		PreUploadCreateCallback: preCreate,
	})
	if err != nil {
		log.Error("tus", "err", err)
		os.Exit(1)
	}

	staticFS, _ := fs.Sub(static, "static")
	mux := http.NewServeMux()
	mux.Handle("/upload/", http.StripPrefix("/upload/", http.FileServerFS(staticFS)))
	mux.Handle("/upload/files/", requireAuth(http.StripPrefix("/upload/files/", tus)))
	mux.HandleFunc("GET /upload/api/libraries", handleLibraries)
	mux.Handle("POST /upload/api/import", requireAuth(http.HandlerFunc(handleImport)))
	mux.Handle("GET /upload/api/import/{id}", requireAuth(http.HandlerFunc(handleJob)))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {})

	go sweep()
	go worker(log)
	srv := &http.Server{Addr: ":8080", Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func handleLibraries(w http.ResponseWriter, r *http.Request) {
	libs, fresh, err := auth(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if fresh != "" {
		w.Header().Set("X-ND-Authorization", fresh)
	}
	type pub struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	out := []pub{}
	for _, l := range libs {
		out = append(out, pub{l.ID, l.Name})
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
