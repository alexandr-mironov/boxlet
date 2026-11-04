package proxy

import (
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
)

// PortForwarder пробрасывает TCP-порты с localhost на порты гостевой ОС.
//
// Используется бэкендами, где IP гостя маршрутизируется с хоста напрямую
// (darwin-vz): dockerd публикует порты контейнеров на 0.0.0.0 гостя,
// а форвардер делает их доступными на хосте как 127.0.0.1:port.
// Слушатель создаётся только на 127.0.0.1, чтобы не светить порты
// контейнеров во внешние интерфейсы хоста.
type PortForwarder struct {
	// guestIP вызывается на каждое входящее соединение: IP гостя может
	// смениться между перезапусками VM, кэшировать его нельзя.
	guestIP func() (string, error)

	mu     sync.Mutex
	ln     map[int]net.Listener // хостовый порт → слушатель
	failed map[int]string       // хостовый порт → причина (чтобы не спамить лог)
	closed bool
	done   chan struct{}
}

// NewPortForwarder создаёт форвардер; guestIP — функция разрешения
// текущего адреса гостевой ОС (см. backend.Porter).
func NewPortForwarder(guestIP func() (string, error)) *PortForwarder {
	return &PortForwarder{
		guestIP: guestIP,
		ln:      map[int]net.Listener{},
		failed:  map[int]string{},
		done:    make(chan struct{}),
	}
}

// SetPorts приводит набор пробросов к заданному отображению
// «хостовый порт → гостевой порт»: лишние слушатели закрываются,
// недостающие открываются.
func (f *PortForwarder) SetPorts(ports map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	for hostPort, ln := range f.ln {
		if _, ok := ports[hostPort]; !ok {
			ln.Close()
			delete(f.ln, hostPort)
			log.Printf("boxletd: проброс 127.0.0.1:%d снят", hostPort)
		}
	}
	for hostPort, guestPort := range ports {
		if _, ok := f.ln[hostPort]; ok {
			continue
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(hostPort))
		if err != nil {
			// Логируем один раз на порт, пока причина не изменится.
			reason := err.Error()
			if hostPort < 1024 {
				reason = "порт <1024 требует root (или установите хелпер: boxlet priv install)"
			}
			if f.failed[hostPort] != reason {
				f.failed[hostPort] = reason
				log.Printf("boxletd: проброс 127.0.0.1:%d пропущен: %s", hostPort, reason)
			}
			continue
		}
		delete(f.failed, hostPort)
		f.ln[hostPort] = ln
		log.Printf("boxletd: проброс 127.0.0.1:%d → гость:%d", hostPort, guestPort)
		go f.acceptLoop(ln, hostPort, guestPort)
	}
}

// Close закрывает все слушатели.
func (f *PortForwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.done)
	for hostPort, ln := range f.ln {
		ln.Close()
		delete(f.ln, hostPort)
	}
}

func (f *PortForwarder) acceptLoop(ln net.Listener, hostPort, guestPort int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // слушатель закрыт через SetPorts/Close
		}
		go f.handle(conn, guestPort)
	}
}

func (f *PortForwarder) handle(client net.Conn, guestPort int) {
	defer client.Close()
	ip, err := f.guestIP()
	if err != nil {
		return
	}
	guest, err := net.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(guestPort)))
	if err != nil {
		return
	}
	defer guest.Close()

	// Как и в прокси сокета: half-close пробрасываем через CloseWrite,
	// оба направления дожидаются друг друга (см. proxy.go).
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(guest, client)
	go cp(client, guest)
	select {
	case <-done:
		<-done
	case <-f.done:
	}
}

// ParsePortSpecs разбирает список пробросов формата "host:guest"
// (конфиг forwarded_ports) в отображение «хостовый порт → гостевой порт».
func ParsePortSpecs(specs []string) (map[int]int, error) {
	ports := map[int]int{}
	for _, spec := range specs {
		host, guest, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf("проброс %q: ожидается формат host:guest", spec)
		}
		hp, err1 := strconv.Atoi(host)
		gp, err2 := strconv.Atoi(guest)
		if err1 != nil || err2 != nil || hp < 1 || hp > 65535 || gp < 1 || gp > 65535 {
			return nil, fmt.Errorf("проброс %q: некорректный номер порта", spec)
		}
		ports[hp] = gp
	}
	return ports, nil
}
