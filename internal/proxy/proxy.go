// Package proxy поднимает на хосте публичный Docker API сокет
// (~/.boxlet/run/docker.sock) и пробрасывает соединения в dockerd
// внутри бэкенда на уровне L4.
//
// Прокси сознательно не разбирает HTTP-протокол: это сохраняет полную
// совместимость, включая hijack/upgrade-стримы (docker run/exec -it,
// attach, testcontainers).
package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/alexandr-mironov/boxlet/internal/backend"
)

// Proxy — слушатель публичного сокета.
type Proxy struct {
	backend backend.Backend
	socket  string

	mu     sync.Mutex
	ln     net.Listener
	cancel context.CancelFunc
	done   chan struct{}
}

// New создаёт прокси на указанном пути сокета.
func New(b backend.Backend, socket string) *Proxy {
	return &Proxy{backend: b, socket: socket}
}

// Start запускает прослушивание и приём соединений.
func (p *Proxy) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		return fmt.Errorf("прокси уже запущен")
	}
	if err := os.MkdirAll(filepath.Dir(p.socket), 0o755); err != nil {
		return err
	}
	// Старый сокет мог остаться после некорректного завершения.
	if _, err := os.Stat(p.socket); err == nil {
		if err := os.Remove(p.socket); err != nil {
			return fmt.Errorf("удалить старый сокет %s: %w", p.socket, err)
		}
	}
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		return fmt.Errorf("слушать %s: %w", p.socket, err)
	}
	// Доступ к сокету = root в VM: только владельцу, как и остальное
	// в ~/.boxlet; другие пользователи машины подключаться не должны.
	if err := os.Chmod(p.socket, 0o600); err != nil {
		ln.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	p.ln = ln
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.acceptLoop(ctx)
	return nil
}

// Stop прекращает приём соединений и удаляет сокет.
func (p *Proxy) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return nil
	}
	p.cancel()
	p.ln.Close()
	<-p.done
	p.ln = nil
	os.Remove(p.socket)
	return nil
}

func (p *Proxy) acceptLoop(ctx context.Context) {
	defer close(p.done)
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return // слушатель закрыт через Stop
		}
		go p.handle(ctx, conn)
	}
}

func (p *Proxy) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	guest, err := p.backend.GuestSocket(ctx)
	if err != nil {
		// Бэкенд не готов: docker CLI покажет «Cannot connect...»,
		// как и ведёт себя остановленный Docker Desktop.
		return
	}
	defer guest.Close()

	// Двунаправленное копирование; hijack-стримы (docker run/exec,
	// attach) живут в этом же потоке.
	//
	// Важно: EOF в одну сторону — это half-close, а не конец соединения.
	// Например, docker CLI после attach-запроса закрывает stdin
	// (CloseWrite), но ждёт вывод контейнера в обратную сторону.
	// Если убить обе стороны по первому EOF, вывод теряется.
	// Поэтому EOF пробрасываем через CloseWrite и ждём ОБА направления.
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
		<-done // ждём завершения второго направления
	case <-ctx.Done():
	}
}
