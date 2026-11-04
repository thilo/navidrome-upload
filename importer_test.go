package main

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var mp3 = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 100)...)

func writeZip(t *testing.T, add func(*zip.Writer)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	add(zw)
	zw.Close()
	f.Close()
	return p
}

func file(zw *zip.Writer, name string, mode fs.FileMode, body []byte) {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(mode)
	w, _ := zw.CreateHeader(h)
	w.Write(body)
}

func TestUnzip(t *testing.T) {
	src := writeZip(t, func(zw *zip.Writer) {
		file(zw, "Album/01 ok.mp3", 0o644, mp3)
		file(zw, "../../escape.mp3", 0o644, mp3)
		file(zw, "/abs.mp3", 0o644, mp3)
		file(zw, `..\win.mp3`, 0o644, mp3)
		file(zw, "Album/link.mp3", fs.ModeSymlink|0o777, []byte("/etc/passwd"))
		file(zw, "Album/fake.mp3", 0o644, []byte("<script>alert(1)</script>"))
		file(zw, "Album/run.sh", 0o755, []byte("#!/bin/sh"))
		file(zw, "Album/cover.jpg", 0o644, []byte{0xFF, 0xD8, 0xFF, 0xE0})
		file(zw, "Album/inner.zip", 0o644, []byte("PK\x03\x04"))
		file(zw, "__MACOSX/Album/._01 ok.mp3", 0o644, mp3)
		file(zw, "Album/ctrl\x01.mp3", 0o644, mp3)
	})
	dst := filepath.Join(t.TempDir(), "out")
	j := &job{}
	budget := int64(1 << 20)
	if err := unzip(j, src, dst, &budget); err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.WalkDir(filepath.Dir(dst), func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dst, p)
			got = append(got, rel)
		}
		return nil
	})
	want := "Album/01 ok.mp3,Album/ctrl.mp3"
	if strings.Join(got, ",") != want {
		t.Errorf("written %v, want %s; skipped %v", got, want, j.Skipped)
	}
	if len(j.Skipped) != 8 {
		t.Errorf("skipped %d, want 8: %v", len(j.Skipped), j.Skipped)
	}
}

func TestUnzipBomb(t *testing.T) {
	src := writeZip(t, func(zw *zip.Writer) {
		file(zw, "big.flac", 0o644, append([]byte("fLaC"), bytes.Repeat([]byte{0}, 4<<20)...))
	})
	if fi, _ := os.Stat(src); fi.Size() > 64<<10 {
		t.Fatalf("test zip not compressed: %d bytes", fi.Size())
	}
	budget := int64(1 << 20)
	err := unzip(&job{}, src, filepath.Join(t.TempDir(), "out"), &budget)
	if err != errBudget {
		t.Fatalf("got %v, want errBudget", err)
	}
	if budget >= 0 {
		t.Errorf("budget %d not exhausted", budget)
	}
}

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"Artist/Album/01.mp3":             "Artist/Album/01.mp3",
		"a/./b.mp3":                       "",
		"../x.mp3":                        "",
		"/x.mp3":                          "",
		"null":                            "",
		"a//b.mp3":                        "",
		"a/\x00\x01..\x02/b":              "",
		strings.Repeat("é", 150) + ".mp3": strings.Repeat("é", 98) + ".mp3",
	} {
		got, _ := cleanPath(in)
		if got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}
