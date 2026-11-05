package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	"github.com/alexandr-mironov/boxlet/internal/certs"
)

// tlsBackend — бэкенд, чей GuestSocket дозванивается до TLS-сервера,
// как qemu-бэкенд до dockerd с tlsverify.
type tlsBackend struct {
	backend.Backend
	addr string
	cfg  *tls.Config
}

func (b *tlsBackend) GuestSocket(ctx context.Context) (net.Conn, error) {
	return (&tls.Dialer{Config: b.cfg}).DialContext(ctx, "tcp", b.addr)
}

// Half-close должен проходить через прокси и поверх TLS: клиент шлёт
// данные и закрывает запись, сервер дочитывает до EOF и только потом
// отвечает (так ведёт себя attach после закрытия stdin).
func TestHalfCloseOverTLS(t *testing.T) {
	bundle, err := certs.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(bundle.ServerCert, bundle.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(bundle.CA)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		in, _ := io.ReadAll(c) // до close_notify от прокси
		io.WriteString(c, "echo:"+string(in))
	}()

	clientCfg, err := bundle.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "docker.sock")
	p := New(&tlsBackend{addr: ln.Addr().String(), cfg: clientCfg}, sock)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("права на сокет %v, ожидалось 0600", fi.Mode().Perm())
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "hello")
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(got) != "echo:hello" {
		t.Fatalf("ответ %q, ожидалось %q", got, "echo:hello")
	}
}
