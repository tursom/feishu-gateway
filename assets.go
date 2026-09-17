package gatewayassets

import (
	"embed"
	"io/fs"
)

//go:embed web/index.html web/app.js web/style.css
var files embed.FS

func Web() fs.FS {
	sub, err := fs.Sub(files, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
