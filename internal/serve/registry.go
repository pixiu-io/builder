package serve

import (
	"net/http"

	"github.com/google/go-containerregistry/pkg/registry"
)

func newRegistryHandler(blobDir string) http.Handler {
	return registry.New(registry.WithBlobHandler(registry.NewDiskBlobHandler(blobDir)))
}
