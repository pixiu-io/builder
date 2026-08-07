package serve

import (
	"io"
	"log"
	"net/http"

	"github.com/google/go-containerregistry/pkg/registry"
)

func newRegistryHandler(blobDir string) http.Handler {
	// registry 包默认把每个请求打到标准日志（stderr），serve 加载/导入镜像时会刷屏。
	// 用静默 logger 抑制；错误仍通过响应码返回客户端，不影响可观测性。
	silent := log.New(io.Discard, "", 0)
	return registry.New(
		registry.WithBlobHandler(registry.NewDiskBlobHandler(blobDir)),
		registry.Logger(silent),
	)
}
