// Package dnsd — минимальный DNS-сервер зоны boxlet.local.
//
// Отвечает на A-запросы вида <что-угодно>.boxlet.local адресом гостевой ОС,
// чтобы к опубликованным портам контейнеров можно было обращаться по
// человекочитаемому имени (например web.boxlet.local:8080). macOS
// направляет запросы этой зоны сюда через /etc/resolver/boxlet
// (ставится: boxlet dns install).
//
// Реализация намеренно минимальна (только stdlib): серверу не нужны
// рекурсия, кэш и полный набор типов записей — он обслуживает ровно
// одну зону и один тип ответа.
package dnsd

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strings"
)

// Port — UDP/TCP-порт сервера на 127.0.0.1. Нестандартный, чтобы не
// требовать root и не конфликтовать с другими резолверами; порт
// прописывается в /etc/resolver/boxlet.
const Port = 15353

// Zone — обслуживаемая зона (с поддоменами любой глубины).
const Zone = "boxlet.local"

// ttl ответов маленький: IP гостя может смениться при перезапуске VM.
const ttl = 5

// Server отвечает адресом, который возвращает resolve на каждый запрос
// (IP гостя кэшировать нельзя — см. proxy.PortForwarder).
type Server struct {
	resolve func() (string, error)
}

func New(resolve func() (string, error)) *Server {
	return &Server{resolve: resolve}
}

// Query шлёт A-запрос к серверу на 127.0.0.1:Port — для диагностики
// (boxlet dns status), минуя системный резолвер.
func Query(ctx context.Context, name string) (string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", fmt.Sprintf("127.0.0.1:%d", Port))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	q := []byte{0xBA, 0xBE, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", fmt.Errorf("некорректное имя: %s", name)
		}
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	q = append(q, 0, 0x00, 0x01, 0x00, 0x01) // root, A, IN

	if _, err := conn.Write(q); err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	resp := buf[:n]
	if len(resp) < 16 {
		return "", fmt.Errorf("короткий ответ (%d байт)", n)
	}
	switch rcode := resp[3] & 0x0F; rcode {
	case rcodeOK:
	case rcodeNX:
		return "", fmt.Errorf("NXDOMAIN")
	default:
		return "", fmt.Errorf("rcode=%d", rcode)
	}
	if binary.BigEndian.Uint16(resp[6:8]) == 0 {
		return "", fmt.Errorf("пустой ответ")
	}
	ip := resp[len(resp)-4:]
	return net.IP(ip).String(), nil
}

// Run слушает UDP и TCP на 127.0.0.1:Port до отмены ctx.
func (s *Server) Run(ctx context.Context) error {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: Port}
	udp, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("dns udp: %w", err)
	}
	tcp, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", Port))
	if err != nil {
		udp.Close()
		return fmt.Errorf("dns tcp: %w", err)
	}
	log.Printf("dnsd: %s → гостевая ОС, слушаю 127.0.0.1:%d", Zone, Port)

	go func() {
		<-ctx.Done()
		udp.Close()
		tcp.Close()
	}()
	go s.serveTCP(tcp)

	buf := make([]byte, 4096)
	for {
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			return nil // сокет закрыт по ctx.Done
		}
		if resp := s.answer(buf[:n]); resp != nil {
			udp.WriteToUDP(resp, from)
		}
	}
}

// serveTCP обслуживает TCP-запросы (протокол DNS: 2-байтовый префикс длины).
// macOS резолвер обычно ходит по UDP, TCP — для полноты.
func (s *Server) serveTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			var lbuf [2]byte
			if _, err := readFull(conn, lbuf[:]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(lbuf[:]))
			if n <= 0 || n > 65535 {
				return
			}
			msg := make([]byte, n)
			if _, err := readFull(conn, msg); err != nil {
				return
			}
			resp := s.answer(msg)
			if resp == nil {
				return
			}
			binary.BigEndian.PutUint16(lbuf[:], uint16(len(resp)))
			conn.Write(lbuf[:])
			conn.Write(resp)
		}()
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Типы записей и коды ответов, которые реально нужны серверу.
const (
	qtypeA    = 1
	qtypeAAAA = 28
	rcodeOK   = 0
	rcodeNX   = 3 // NXDOMAIN
	rcodeFail = 2 // SERVFAIL
)

// answer разбирает DNS-запрос и строит ответ. Возвращает nil, если
// сообщение не похоже на корректный запрос (молча игнорируем мусор).
func (s *Server) answer(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	id := binary.BigEndian.Uint16(query[0:2])
	flags := binary.BigEndian.Uint16(query[2:4])
	if flags&0x8000 != 0 {
		return nil // это ответ, а не запрос
	}
	qd := int(binary.BigEndian.Uint16(query[4:6]))
	if qd != 1 {
		return nil
	}

	name, end, ok := parseName(query, 12)
	if !ok || end+4 > len(query) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(query[end : end+2])

	lower := strings.ToLower(name)
	inZone := lower == Zone || strings.HasSuffix(lower, "."+Zone)

	rcode := uint16(rcodeOK)
	var ip net.IP
	switch {
	case !inZone:
		rcode = rcodeNX
	case qtype == qtypeA:
		addr, err := s.resolve()
		if err != nil || addr == "" {
			rcode = rcodeFail
		} else if ip = net.ParseIP(addr).To4(); ip == nil {
			rcode = rcodeFail
		}
	default:
		// AAAA и прочие типы для нашей зоны: честный пустой ответ
		// (NOERROR без записей), иначе резолвер будет ждать таймаут.
	}

	// Ответ: ID запроса, флаги QR|RD, RCODE; секция вопроса копируется.
	out := make([]byte, 0, end+4+16)
	out = binary.BigEndian.AppendUint16(out, id)
	out = binary.BigEndian.AppendUint16(out, 0x8000|(flags&0x0100)|rcode)
	answers := uint16(0)
	if ip != nil {
		answers = 1
	}
	out = binary.BigEndian.AppendUint16(out, 1)       // QDCOUNT
	out = binary.BigEndian.AppendUint16(out, answers) // ANCOUNT
	out = append(out, 0, 0, 0, 0)                     // NSCOUNT, ARCOUNT
	out = append(out, query[12:end+4]...)             // вопрос как есть
	if ip != nil {
		out = append(out, 0xC0, 0x0C) // имя — указатель на вопрос
		out = binary.BigEndian.AppendUint16(out, qtypeA)
		out = binary.BigEndian.AppendUint16(out, 1) // class IN
		out = binary.BigEndian.AppendUint32(out, ttl)
		out = binary.BigEndian.AppendUint16(out, 4)
		out = append(out, ip...)
	}
	return out
}

// parseName читает доменное имя из сообщения (метки через точку).
// Сжатие указателями в секции вопроса не встречается, но отступаем
// аккуратно: метка длиной >63 или выход за границы — ошибка.
func parseName(msg []byte, off int) (string, int, bool) {
	var parts []string
	for {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		if l == 0 {
			return strings.Join(parts, "."), off + 1, true
		}
		if l > 63 || off+1+l > len(msg) {
			return "", 0, false
		}
		parts = append(parts, string(msg[off+1:off+1+l]))
		off += 1 + l
	}
}
