package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxZipEntries = 5000
	maxLogLines   = 300
)

type job struct {
	ID      string    `json:"id"`
	State   string    `json:"state"` // queued, running, done, failed
	Error   string    `json:"error,omitempty"`
	Log     []string  `json:"log"`
	Skipped []string  `json:"skipped"`
	Targets []string  `json:"targets"` // folders written, relative to the library
	Scan    string    `json:"scan,omitempty"`
	Started time.Time `json:"-"`

	lib     library
	uploads []string
	token   string     // the importing admin's, for the duplicate check
	scan    url.Values // their Subsonic login, for startScan
}

func (j *job) finished() bool { return j.State == "done" || j.State == "failed" }

func (j *job) skip(name, why string) {
	jobsMu.Lock()
	j.Skipped = append(j.Skipped, name+": "+why)
	jobsMu.Unlock()
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*job{}
	queue  = make(chan *job, 16)
	// tusd's filestore names uploads with 32 hex characters.
	uploadID = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func handleImport(w http.ResponseWriter, r *http.Request) {
	libs, _, err := auth(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		Library int      `json:"library"`
		Uploads []string `json:"uploads"`
		// The web UI's Subsonic token: startScan accepts no other login.
		Subsonic struct{ U, T, S string } `json:"subsonic"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || len(req.Uploads) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	j := &job{ID: randomID(), State: "queued", Log: []string{}, Skipped: []string{}, Targets: []string{}, Started: time.Now()}
	for _, l := range libs {
		if l.ID == req.Library {
			j.lib = l
		}
	}
	// Navidrome reports the path it reads; this container mounts the same
	// volume at the same place.
	if j.lib.Path == "" || !strings.HasPrefix(filepath.Clean(j.lib.Path)+"/", musicRoot) {
		http.Error(w, "unknown library", http.StatusBadRequest)
		return
	}
	for _, id := range req.Uploads {
		if !uploadID.MatchString(id) {
			http.Error(w, "bad upload id", http.StatusBadRequest)
			return
		}
	}
	j.uploads = req.Uploads
	j.token = r.Header.Get("X-ND-Authorization")
	j.scan = url.Values{"u": {req.Subsonic.U}, "t": {req.Subsonic.T}, "s": {req.Subsonic.S}}
	select {
	case queue <- j:
	default:
		http.Error(w, "too many imports queued", http.StatusServiceUnavailable)
		return
	}
	jobsMu.Lock()
	jobs[j.ID] = j
	jobsMu.Unlock()
	writeJSON(w, map[string]string{"id": j.ID})
}

func handleJob(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, ok := jobs[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, j)
}

// worker runs one import at a time: beets is the heavy part and the library
// lives on one network share.
func worker(log *slog.Logger) {
	for j := range queue {
		setState(j, "running", "")
		err := runJob(j)
		os.RemoveAll(filepath.Join(jobsDir, j.ID))
		if len(j.Targets) > 0 {
			scan := startScan(j)
			jobsMu.Lock()
			j.Scan = scan
			jobsMu.Unlock()
		}
		j.token, j.scan = "", nil
		if err != nil {
			log.Error("import", "job", j.ID, "err", err)
			setState(j, "failed", err.Error())
		} else {
			log.Info("import", "job", j.ID, "folders", len(j.Targets), "skipped", len(j.Skipped))
			setState(j, "done", "")
		}
	}
}

func setState(j *job, state, msg string) {
	jobsMu.Lock()
	j.State, j.Error = state, msg
	jobsMu.Unlock()
}

func runJob(j *job) error {
	dir := filepath.Join(jobsDir, j.ID)
	in := filepath.Join(dir, "in")
	if err := os.MkdirAll(in, 0o700); err != nil {
		return err
	}
	budget := int64(stagingQuota) - stagingUsed()
	for _, id := range j.uploads {
		data := filepath.Join(uploadsDir, id)
		err := stage(j, data, in, &budget)
		os.Remove(data)
		os.Remove(data + ".info")
		os.Remove(data + ".lock")
		if err != nil {
			return err
		}
	}

	// beets tags and files the tracks into out/, still on local disk, with a
	// fresh database that holds only this import. Only tracks that pass the
	// duplicate check then move on into the library.
	out := filepath.Join(dir, "out")
	db := filepath.Join(dir, "library.db")
	beet := func(args ...string) *exec.Cmd {
		cmd := exec.Command("beet", append([]string{"-c", "/etc/beets/config.yaml", "-l", db, "-d", out}, args...)...)
		cmd.Env = append(os.Environ(), "BEETSDIR="+dir, "HOME="+dir)
		return cmd
	}
	if err := logRun(j, beet("import", in)); err != nil {
		return fmt.Errorf("beets: %w", err)
	}
	listing, err := beet("ls", "-f", "$mb_trackid\x1f$albumartist\x1f$artist\x1f$album\x1f$title\x1f$path").Output()
	if err != nil {
		return fmt.Errorf("beets ls: %w", err)
	}

	// beets names a second copy "<name>.1.mp3"; shorter paths first keeps
	// the unsuffixed one.
	lines := strings.Split(strings.TrimSpace(string(listing)), "\n")
	sort.SliceStable(lines, func(a, b int) bool { return len(lines[a]) < len(lines[b]) })
	seen := map[string]bool{}
	albums := map[string]string{} // out/ album folder -> library folder
	for _, line := range lines {
		f := strings.Split(line, "\x1f")
		if len(f) != 6 {
			continue
		}
		t := track{mbid: f[0], albumArtist: f[1], artist: f[2], album: f[3], title: f[4]}
		rel, _ := filepath.Rel(out, f[5])
		key := strings.ToLower(t.artist + "\x1f" + t.album + "\x1f" + t.title)
		switch dup, err := inLibrary(j.token, t); {
		case err != nil:
			return fmt.Errorf("duplicate check: %w", err)
		case seen[key] || (t.mbid != "" && seen[t.mbid]):
			j.skip(rel, "duplicate within this upload")
			continue
		case dup:
			j.skip(rel, "already in the library")
			continue
		}
		seen[key], seen[t.mbid] = true, true
		dst := filepath.Join(j.lib.Path, rel)
		if err := moveFile(f[5], dst); errors.Is(err, os.ErrExist) {
			j.skip(rel, "a file with that name is already in the library")
			continue
		} else if err != nil {
			return err
		}
		if _, ok := albums[filepath.Dir(f[5])]; !ok {
			albums[filepath.Dir(f[5])] = filepath.Dir(dst)
			jobsMu.Lock()
			j.Targets = append(j.Targets, filepath.Dir(rel))
			jobsMu.Unlock()
		}
	}
	// fetchart's cover for a MusicBrainz match follows its album, unless the
	// library folder has one already.
	for src, dst := range albums {
		moveFile(filepath.Join(src, "cover.jpg"), filepath.Join(dst, "cover.jpg"))
	}

	// beets moves what it imports; anything still here was not imported.
	filepath.WalkDir(in, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(in, p)
			j.skip(rel, "not imported by beets (unreadable tags?)")
		}
		return nil
	})
	return nil
}

// startScan has Navidrome scan just the folders this import wrote. Without
// it they still appear after the scheduled scan.
func startScan(j *job) string {
	q := url.Values{"v": {"1.16.1"}, "c": {"navidrome-upload"}, "f": {"json"}}
	for k, v := range j.scan {
		q[k] = v
	}
	for _, t := range j.Targets {
		q.Add("target", fmt.Sprintf("%d:%s", j.lib.ID, t))
	}
	resp, err := http.Get(navidromeURL + "/rest/startScan?" + q.Encode())
	if err != nil {
		return "not started: " + err.Error()
	}
	defer resp.Body.Close()
	var body struct {
		R struct {
			Status string
			Error  struct{ Message string }
		} `json:"subsonic-response"`
	}
	if json.NewDecoder(resp.Body).Decode(&body); body.R.Status != "ok" {
		return "not started: " + body.R.Error.Message + "; the scheduled scan picks the folders up"
	}
	return "started"
}

type track struct{ mbid, albumArtist, artist, album, title string }

// inLibrary asks Navidrome, which indexes the whole music folder however the
// files got there, whether t is already in any library: the same MusicBrainz
// recording, or the same title and album by the same artist.
func inLibrary(token string, t track) (bool, error) {
	same := func(a, b string) bool { return a != "" && strings.EqualFold(a, b) }
	queries := []string{t.title + " " + t.album}
	if t.mbid != "" {
		// Navidrome's title filter also matches recording IDs exactly.
		queries = append(queries, t.mbid)
	}
	for _, q := range queries {
		if strings.TrimSpace(q) == "" {
			continue
		}
		// ponytail: one page of 500 matches; a title+album shared by more
		// songs than that could hide a duplicate.
		u := navidromeURL + "/api/song?" + url.Values{"title": {q}, "missing": {"false"}, "_start": {"0"}, "_end": {"500"}}.Encode()
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("X-ND-Authorization", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, err
		}
		var songs []struct {
			Title, Album, Artist, AlbumArtist string
			MbzRecordingID                    string `json:"mbzRecordingID"`
		}
		err = json.NewDecoder(resp.Body).Decode(&songs)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			return false, fmt.Errorf("navidrome answered %s", resp.Status)
		}
		for _, s := range songs {
			if same(t.mbid, s.MbzRecordingID) ||
				(same(t.title, s.Title) && same(t.album, s.Album) &&
					(same(t.artist, s.Artist) || same(t.albumArtist, s.AlbumArtist) || same(t.artist, s.AlbumArtist))) {
				return true, nil
			}
		}
	}
	return false, nil
}

// moveFile moves src to dst across filesystems, creating dst's folder, and
// fails with os.ErrExist rather than replace a file.
func moveFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func logRun(j *job, cmd *exec.Cmd) error {
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		// Each job's fresh database logs beets' schema migrations.
		if strings.HasPrefix(sc.Text(), "Created database backup") {
			continue
		}
		jobsMu.Lock()
		if len(j.Log) < maxLogLines {
			j.Log = append(j.Log, sc.Text())
		}
		jobsMu.Unlock()
	}
	return cmd.Wait()
}

// stage puts one finished upload into the job's input folder: a zip is
// unpacked, anything else is moved. Only a full staging budget fails the job;
// a bad file is skipped and reported.
func stage(j *job, data, in string, budget *int64) error {
	info, err := readInfo(data + ".info")
	if err != nil {
		j.skip("upload "+filepath.Base(data), "not found")
		return nil
	}
	name := info.MetaData["filename"]
	fi, err := os.Stat(data)
	if err != nil || fi.Size() != info.Size {
		j.skip(name, "upload not finished")
		return nil
	}
	kind := kinds[strings.ToLower(filepath.Ext(name))]
	f, err := os.Open(data)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	if sniff(head[:n]) != kind {
		j.skip(name, "content is not "+kind)
		return nil
	}

	// relativePath is set when a folder was dropped.
	rel, ok := cleanPath(info.MetaData["relativePath"])
	if !ok {
		rel, ok = cleanPath(name)
	}
	if !ok {
		j.skip(name, "unusable file name")
		return nil
	}
	if kind == "zip" {
		dst := filepath.Join(in, strings.TrimSuffix(rel, filepath.Ext(rel)))
		return unzip(j, data, dst, budget)
	}
	dst := filepath.Join(in, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		j.skip(name, "a file with that name is already in this import")
		return nil
	}
	// Same disk, so a rename; the upload already counts against the quota.
	return os.Rename(data, dst)
}

var errBudget = errors.New("unpacked archive exceeds the free upload space")

// unzip writes only regular files with an accepted extension and matching
// content, under dst. Entry names and declared sizes are not trusted: names go
// through cleanPath, and every written byte is counted against the staging
// budget, which stops zip bombs.
func unzip(j *job, src, dst string, budget *int64) error {
	zr, err := zip.OpenReader(src)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		j.skip(filepath.Base(dst)+".zip", "not a readable zip")
		return nil
	}
	defer zr.Close()
	if len(zr.File) > maxZipEntries {
		j.skip(filepath.Base(dst)+".zip", fmt.Sprintf("more than %d entries", maxZipEntries))
		return nil
	}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		base := path.Base(name)
		if f.FileInfo().IsDir() || strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, "._") || base == ".DS_Store" {
			continue
		}
		kind := kinds[strings.ToLower(path.Ext(base))]
		rel, ok := cleanPath(name)
		switch {
		case !f.Mode().IsRegular():
			j.skip(name, "not a regular file")
			continue
		case !ok:
			j.skip(name, "unsafe path")
			continue
		case kind == "" || kind == "zip":
			j.skip(name, "not an audio file")
			continue
		}
		err := extract(f, filepath.Join(dst, rel), kind, budget)
		if errors.Is(err, errBudget) {
			return err
		}
		if err != nil {
			j.skip(name, err.Error())
		}
	}
	return nil
}

func extract(f *zip.File, dst, kind string, budget *int64) (err error) {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(rc, head)
	if sniff(head[:n]) != kind {
		return errors.New("content is not " + kind)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(dst)
		}
	}()
	limit, tooBig := *budget, errBudget
	if limit > maxFileSize {
		limit, tooBig = maxFileSize, errors.New("larger than 1 GiB")
	}
	written, err := io.Copy(out, io.LimitReader(io.MultiReader(bytes.NewReader(head[:n]), rc), limit+1))
	*budget -= written
	if err != nil {
		return err // includes zip.ErrChecksum
	}
	if written > limit {
		return tooBig
	}
	return nil
}

func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("ID3")), len(b) > 1 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "mp3"
	case bytes.HasPrefix(b, []byte("fLaC")):
		return "flac"
	case bytes.HasPrefix(b, []byte("OggS")):
		return "ogg"
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return "m4a"
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return "zip"
	}
	return ""
}

// cleanPath turns a client-supplied slash path into a relative local one:
// control characters go, each part is capped at 200 bytes keeping its
// extension, and any empty, "." or ".." part rejects the whole path.
func cleanPath(p string) (string, bool) {
	if p == "" || p == "null" || strings.HasPrefix(p, "/") {
		return "", false
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		part = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, part))
		if ext := filepath.Ext(part); len(part) > 200 && len(ext) <= 10 {
			stem := strings.TrimSuffix(part, ext)
			for len(stem)+len(ext) > 200 {
				_, size := utf8.DecodeLastRuneInString(stem)
				stem = stem[:len(stem)-size]
			}
			part = stem + ext
		}
		for len(part) > 200 {
			_, size := utf8.DecodeLastRuneInString(part)
			part = part[:len(part)-size]
		}
		if part == "" || part == "." || part == ".." {
			return "", false
		}
		parts[i] = part
	}
	return filepath.Join(parts...), true
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
