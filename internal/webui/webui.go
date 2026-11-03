// Package webui отдаёт встроенный веб-интерфейс boxlet:
// статус VM, список контейнеров, управление start/stop.
// Это GUI MVP; desktop-приложение планируется в M4 поверх того же API.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler возвращает http.Handler со статикой веб-интерфейса.
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
