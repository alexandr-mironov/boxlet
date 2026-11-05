// Package certs выпускает TLS-сертификаты для канала хост → dockerd в VM.
//
// dockerd в гостевой ОС слушает TCP только с tlsverify: без клиентского
// сертификата boxlet к нему не подключиться ни другому локальному
// процессу, ни веб-странице через DNS rebinding на 127.0.0.1.
//
// Ключ CA не сохраняется: если какого-то файла не хватает,
// весь комплект выпускается заново.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ServerName — имя в серверном сертификате, которое проверяет клиент.
const ServerName = "boxlet"

const validity = 10 * 365 * 24 * time.Hour

// Bundle — комплект сертификатов в PEM.
type Bundle struct {
	CA         []byte
	ServerCert []byte
	ServerKey  []byte
	ClientCert []byte
	ClientKey  []byte
}

func (b *Bundle) files() map[string]*[]byte {
	return map[string]*[]byte{
		"ca.pem":         &b.CA,
		"server.pem":     &b.ServerCert,
		"server-key.pem": &b.ServerKey,
		"client.pem":     &b.ClientCert,
		"client-key.pem": &b.ClientKey,
	}
}

// Ensure читает комплект из dir или выпускает новый, если его нет.
func Ensure(dir string) (*Bundle, error) {
	b := &Bundle{}
	complete := true
	for name, dst := range b.files() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			complete = false
			break
		}
		if err != nil {
			return nil, err
		}
		*dst = data
	}
	if complete {
		return b, nil
	}

	b, err := generate()
	if err != nil {
		return nil, fmt.Errorf("выпустить TLS-сертификаты: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for name, data := range b.files() {
		if err := os.WriteFile(filepath.Join(dir, name), *data, 0o600); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// ClientConfig — TLS-конфигурация клиента dockerd: проверяет сервер
// по нашему CA и предъявляет клиентский сертификат.
func (b *Bundle) ClientConfig() (*tls.Config, error) {
	pair, err := tls.X509KeyPair(b.ClientCert, b.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("клиентский сертификат: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CA) {
		return nil, errors.New("не удалось разобрать ca.pem")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		ServerName:   ServerName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func generate() (*Bundle, error) {
	now := time.Now()
	// Часы гостевой ОС могут слегка отставать от хоста.
	notBefore := now.Add(-24 * time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "boxlet CA"},
		NotBefore:             notBefore,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	leaf := func(cn string, usage x509.ExtKeyUsage, dns []string, ips []net.IP) (certPEM, keyPEM []byte, err error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: serial(),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    notBefore,
			NotAfter:     now.Add(validity),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			DNSNames:     dns,
			IPAddresses:  ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		return pemBlock("CERTIFICATE", der), pemBlock("PRIVATE KEY", keyDER), nil
	}

	b := &Bundle{CA: pemBlock("CERTIFICATE", caDER)}
	b.ServerCert, b.ServerKey, err = leaf("boxlet dockerd", x509.ExtKeyUsageServerAuth,
		[]string{ServerName, "localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	b.ClientCert, b.ClientKey, err = leaf("boxlet client", x509.ExtKeyUsageClientAuth, nil, nil)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}
