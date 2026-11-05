package guest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
)

// NetbootServer — временный HTTP-сервер, отдающий гостевой ОС
// apkovl-оверлей при загрузке: qemu-гостю через slirp-адрес хоста
// 10.0.2.2, darwin-vz-гостю через шлюз vmnet. Modloop отдельно не нужен:
// он подключён к VM read-only диском.
//
// Секретов в apkovl нет (URL виден в kernel cmdline, а её печатает
// серийная консоль — см. WriteSecrets). Случайный путь и короткая жизнь
// сервера — дополнительная мера: не раздавать оверлей с бинарём агента
// посторонним дольше, чем нужно для загрузки.
type NetbootServer struct {
	srv   *http.Server
	port  int
	token string
}

// ServeNetboot поднимает сервер на случайном порту адреса listenHost.
// qemu передаёт "127.0.0.1": slirp ходит на 10.0.2.2 через loopback хоста.
// darwin-vz передаёт "" (все интерфейсы): мост vmnet появляется только
// вместе с VM, заранее привязаться к адресу шлюза нельзя.
func ServeNetboot(listenHost string, apkovl []byte) (*NetbootServer, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(buf)

	mux := http.NewServeMux()
	mux.HandleFunc("/"+token+"/boxlet.apkovl.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(apkovl)
	})

	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, "0"))
	if err != nil {
		return nil, fmt.Errorf("netboot-сервер: %w", err)
	}
	nb := &NetbootServer{
		srv:   &http.Server{Handler: mux},
		port:  ln.Addr().(*net.TCPAddr).Port,
		token: token,
	}
	go nb.srv.Serve(ln)
	return nb, nil
}

// Port возвращает порт сервера.
func (n *NetbootServer) Port() int { return n.port }

// ApkovlURL — URL для kernel cmdline (10.0.2.2 — хост в slirp).
func (n *NetbootServer) ApkovlURL() string {
	return n.ApkovlURLFor("10.0.2.2")
}

// ApkovlURLFor — URL для kernel cmdline через указанный адрес хоста.
// Используется бэкендом darwin-vz: гость достаёт оверлей через шлюз vmnet.
func (n *NetbootServer) ApkovlURLFor(host string) string {
	return fmt.Sprintf("http://%s:%d/%s/boxlet.apkovl.tar.gz", host, n.port, n.token)
}

// Close останавливает сервер.
func (n *NetbootServer) Close() error { return n.srv.Close() }
