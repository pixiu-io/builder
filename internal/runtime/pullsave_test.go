package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestNormalizePullArch(t *testing.T) {
	if got := normalizePullArch("aarch64"); got != "arm64" {
		t.Fatalf("got %q", got)
	}
	if got := normalizePullArch("x86_64"); got != "amd64" {
		t.Fatalf("got %q", got)
	}
}

func TestDockerTarRoundTrip(t *testing.T) {
	img, err := random.Image(32, 1)
	if err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "img.tar")
	ref, err := name.ParseReference("example.com/test:latest", name.WeakValidation)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarball.Write(ref, img, f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	got, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("ImageFromPath: %v", err)
	}
	if _, err := got.Digest(); err != nil {
		t.Fatal(err)
	}
}
