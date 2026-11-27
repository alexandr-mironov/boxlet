package main

import (
	"context"
	"net"
	"net/http"
	"time"
)

// dockerHTTP — общий HTTP-клиент к dockerd через прокси-сокет для
// служебных запросов демона (опрос портов раз в 2 с, сети).
//
// Клиент один на демон: раньше на каждый запрос создавался новый
// http.Transport, и его keep-alive соединение оставалось открытым
// навсегда — по соединению (unix-сокет на хосте + TCP к dockerd в госте)
// каждые 2 секунды, сотни за несколько минут.
func (d *daemon) dockerHTTP() *http.Client {
	d.dockerOnce.Do(func() {
		d.dockerCli = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", d.layout.DockerSock)
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		}}
	})
	return d.dockerCli
}
