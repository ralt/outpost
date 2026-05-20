package bringback

import (
	"archive/tar"
	"bytes"
	"testing"
)

func TestCountTarFiles(t *testing.T) {
	cases := []struct {
		name  string
		build func(w *tar.Writer)
		want  int
	}{
		{"empty", func(w *tar.Writer) {}, 0},
		{"one file", func(w *tar.Writer) {
			writeFile(t, w, "a.txt", "hi")
		}, 1},
		{"three files", func(w *tar.Writer) {
			writeFile(t, w, "a.txt", "x")
			writeFile(t, w, "sub/b.txt", "y")
			writeFile(t, w, "sub/c.txt", "z")
		}, 3},
		{"files plus dir", func(w *tar.Writer) {
			writeDir(t, w, "sub/")
			writeFile(t, w, "sub/a.txt", "x")
		}, 1},
		{"symlink counts", func(w *tar.Writer) {
			writeFile(t, w, "a.txt", "x")
			writeSymlink(t, w, "b.lnk", "a.txt")
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			c.build(tw)
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := countTarFiles(buf.Bytes())
			if err != nil {
				t.Fatalf("countTarFiles: %v", err)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func writeFile(t *testing.T, w *tar.Writer, name, body string) {
	t.Helper()
	h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := w.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

func writeDir(t *testing.T, w *tar.Writer, name string) {
	t.Helper()
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
}

func writeSymlink(t *testing.T, w *tar.Writer, name, target string) {
	t.Helper()
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: target}); err != nil {
		t.Fatal(err)
	}
}
