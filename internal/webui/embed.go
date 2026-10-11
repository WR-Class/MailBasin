package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var assets embed.FS

func Handler() http.Handler {
	root, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requested != "." && requested != "" {
			if info, statErr := fs.Stat(root, requested); statErr == nil && !info.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		index, readErr := fs.ReadFile(root, "index.html")
		if readErr != nil {
			http.Error(w, "前端资源尚未构建", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(".html"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}
