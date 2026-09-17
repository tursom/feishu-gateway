package gatewayassets

import (
	"embed"
	"io/fs"
)

//go:embed web/index.html web/app.js web/style.css
var assets embed.FS

func Web() fs.FS {
	web, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err)
	}
	return web
}
