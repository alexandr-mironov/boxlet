package certs

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// serverConfig — TLS-конфигурация, эквивалентная dockerd с tlsverify.
func serverConfig(t *testing.T, b *Bundle) *tls.Config {
	t.Helper()
	pair, err := tls.X509KeyPair(b.ServerCert, b.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b.CA)
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
}

func TestEnsureReusesBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	first, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.ClientCert, second.ClientCert) {
		t.Fatal("повторный Ensure выпустил новый комплект")
	}
	fi, err := os.Stat(filepath.Join(dir, "client-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("права на ключ %v, ожидалось 0600", fi.Mode().Perm())
	}

	// Неполный комплект перевыпускается целиком.
	os.Remove(filepath.Join(dir, "ca.pem"))
	third, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.ClientCert, third.ClientCert) {
		t.Fatal("неполный комплект не перевыпущен")
	}
}

func TestMutualTLS(t *testing.T) {
	b, err := Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig(t, b))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.WriteString(c, "ok")
			}()
		}
	}()

	clientCfg, err := b.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("клиент с сертификатом: %v", err)
	}
	got, _ := io.ReadAll(conn)
	conn.Close()
	if string(got) != "ok" {
		t.Fatalf("ответ %q", got)
	}

	// Без клиентского сертификата сервер должен отказать.
	noCert := clientCfg.Clone()
	noCert.Certificates = nil
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, noCert)
	defer c.Close()
	c.Handshake()
	if _, err := io.ReadAll(c); err == nil {
		t.Fatal("сервер принял клиента без сертификата")
	}
}
