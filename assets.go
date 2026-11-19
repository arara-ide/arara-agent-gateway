package gateway

import (
	"embed"
	"io/fs"
)

//go:embed web
var webFS embed.FS

//go:embed docs/openapi.yaml
var OpenAPI []byte

func Web() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
