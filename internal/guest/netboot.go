package guest

import (
	"fmt"
	"net"
	"net/http"
)

// NetbootServer — временный HTTP-сервер, отдающий гостевой ОС
// apkovl-оверлей при загрузке. Доступен из VM через slirp-адрес
// хоста 10.0.2.2. Modloop отдельно не нужен: initramfs берёт его
// с подключённого alpine-virt ISO.
type NetbootServer struct {
	srv  *http.Server
	port int
}

// ServeNetboot поднимает сервер на случайном порту.
// Слушает на всех интерфейсах: slirp пробрасывает гостевые запросы
// на хост. Порт случайный и сервер живёт только на время загрузки VM.
func ServeNetboot(apkovl []byte) (*NetbootServer, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/boxlet.apkovl.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(apkovl)
	})

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("netboot-сервер: %w", err)
	}
	nb := &NetbootServer{
		srv:  &http.Server{Handler: mux},
		port: ln.Addr().(*net.TCPAddr).Port,
	}
	go nb.srv.Serve(ln)
	return nb, nil
}

// Port возвращает порт сервера.
func (n *NetbootServer) Port() int { return n.port }

// ApkovlURL — URL для kernel cmdline (10.0.2.2 — хост в slirp).
func (n *NetbootServer) ApkovlURL() string {
	return fmt.Sprintf("http://10.0.2.2:%d/boxlet.apkovl.tar.gz", n.port)
}

// Close останавливает сервер.
func (n *NetbootServer) Close() error { return n.srv.Close() }
