package guest

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
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
	return n.ApkovlURLFor("10.0.2.2")
}

// ApkovlURLFor — URL для kernel cmdline через указанный адрес хоста.
// Используется бэкендом darwin-vz: гость достаёт оверлей через шлюз vmnet.
func (n *NetbootServer) ApkovlURLFor(host string) string {
	return fmt.Sprintf("http://%s:%d/boxlet.apkovl.tar.gz", host, n.port)
}

// vmnetPlist — конфигурация сети Virtualization.framework (NAT).
const vmnetPlist = "/Library/Preferences/SystemConfiguration/com.apple.vmnet.plist"

// sharedNetRe извлекает адрес шлюза из plist (ключ Shared_Net_Address).
var sharedNetRe = regexp.MustCompile(`(?s)Shared_Net_Address</key>\s*<string>([0-9.]+)</string>`)

// GatewayIP возвращает адрес шлюза vmnet (обычно 192.168.64.1),
// читая com.apple.vmnet.plist; при любой ошибке — разумный дефолт.
func GatewayIP() string {
	const fallback = "192.168.64.1"
	data, err := os.ReadFile(vmnetPlist)
	if err != nil {
		return fallback
	}
	if m := sharedNetRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return fallback
}

// Close останавливает сервер.
func (n *NetbootServer) Close() error { return n.srv.Close() }
