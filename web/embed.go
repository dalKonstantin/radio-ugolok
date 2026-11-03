// Package web вшивает фронтенд в бинарник.
package web

import "embed"

//go:embed index.html app.css js fonts
var FS embed.FS
