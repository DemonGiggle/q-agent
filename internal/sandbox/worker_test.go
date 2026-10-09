package sandbox

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkerRefusesHostExecution(t *testing.T) {
	if err := Worker("tool", bytes.NewBufferString(`{"call":{"name":"shell","arguments":{"command":"true"}}}`), &bytes.Buffer{}); err == nil {
		t.Fatal("worker ran outside namespace")
	}
}
func TestExtractionRejectsTraversalAndSymlinkParents(t *testing.T) {
	for _, headers := range [][]tar.Header{{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}}, {{Name: "etc", Linkname: "/tmp", Typeflag: tar.TypeSymlink, Mode: 0777}, {Name: "etc/escape", Typeflag: tar.TypeReg, Mode: 0644}}} {
		dir := t.TempDir()
		archive := filepath.Join(dir, "image.tar")
		f, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(f)
		for _, h := range headers {
			if err = writer.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
		}
		writer.Close()
		f.Close()
		root := filepath.Join(dir, "root")
		os.Mkdir(root, 0700)
		if err = Extract(archive, root); err == nil {
			t.Fatal("unsafe archive extracted")
		}
	}
}
