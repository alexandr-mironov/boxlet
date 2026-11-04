package dnsd

import (
	"encoding/binary"
	"net"
	"testing"
)

// buildQuery собирает минимальный DNS-запрос A для name.
func buildQuery(t *testing.T, id uint16, name string, qtype uint16) []byte {
	t.Helper()
	out := make([]byte, 0, 64)
	out = binary.BigEndian.AppendUint16(out, id)
	out = binary.BigEndian.AppendUint16(out, 0x0100) // RD
	out = binary.BigEndian.AppendUint16(out, 1)      // QDCOUNT
	out = append(out, 0, 0, 0, 0, 0, 0)
	for _, label := range splitDots(name) {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	out = binary.BigEndian.AppendUint16(out, qtype)
	out = binary.BigEndian.AppendUint16(out, 1) // IN
	return out
}

func splitDots(s string) []string {
	var parts []string
	cur := ""
	for _, c := range s {
		if c == '.' {
			parts = append(parts, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}

func TestAnswerAInZone(t *testing.T) {
	s := New(func() (string, error) { return "192.168.64.2", nil })
	resp := s.answer(buildQuery(t, 0x1234, "web."+Zone, qtypeA))
	if resp == nil {
		t.Fatal("nil ответ")
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 {
		t.Fatal("ID не совпал")
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	if flags&0x8000 == 0 || flags&0x000F != rcodeOK {
		t.Fatalf("флаги: %#x", flags)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 1 {
		t.Fatalf("ANCOUNT = %d, хочу 1", an)
	}
	// Ответ — последние 16 байт: ptr(2) type(2) class(2) ttl(4) rdlen(2) ip(4)
	tail := resp[len(resp)-4:]
	if !net.IP(tail).Equal(net.ParseIP("192.168.64.2")) {
		t.Fatalf("IP в ответе: %v", tail)
	}
}

func TestAnswerZoneRootItself(t *testing.T) {
	s := New(func() (string, error) { return "10.0.0.5", nil })
	if resp := s.answer(buildQuery(t, 1, Zone, qtypeA)); resp == nil || binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatal("корень зоны должен резолвиться")
	}
}

func TestAnswerDeepSubdomain(t *testing.T) {
	s := New(func() (string, error) { return "10.0.0.5", nil })
	if resp := s.answer(buildQuery(t, 1, "a.b.c."+Zone, qtypeA)); resp == nil || binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatal("глубокий поддомен должен резолвиться")
	}
}

func TestAnswerCaseInsensitive(t *testing.T) {
	s := New(func() (string, error) { return "10.0.0.5", nil })
	if resp := s.answer(buildQuery(t, 1, "WEB.BOXLET.LOCAL", qtypeA)); resp == nil || binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatal("регистр не должен иметь значения")
	}
}

func TestAnswerOutOfZoneNXDOMAIN(t *testing.T) {
	s := New(func() (string, error) { return "10.0.0.5", nil })
	resp := s.answer(buildQuery(t, 7, "example.com", qtypeA))
	if resp == nil {
		t.Fatal("nil ответ")
	}
	if flags := binary.BigEndian.Uint16(resp[2:4]); flags&0x000F != rcodeNX {
		t.Fatalf("хочу NXDOMAIN, флаги %#x", flags)
	}
	// Суффикс без точки-разделителя — не наша зона.
	resp = s.answer(buildQuery(t, 8, "notboxlet.local", qtypeA))
	if flags := binary.BigEndian.Uint16(resp[2:4]); flags&0x000F != rcodeNX {
		t.Fatal("notboxlet.local не должен попадать в зону")
	}
}

func TestAnswerAAAAEmpty(t *testing.T) {
	s := New(func() (string, error) { return "10.0.0.5", nil })
	resp := s.answer(buildQuery(t, 9, "web."+Zone, qtypeAAAA))
	if resp == nil {
		t.Fatal("nil ответ")
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	if flags&0x000F != rcodeOK || binary.BigEndian.Uint16(resp[6:8]) != 0 {
		t.Fatal("AAAA: хочу NOERROR без записей")
	}
}

func TestAnswerResolveFailureSERVFAIL(t *testing.T) {
	s := New(func() (string, error) { return "", errTest })
	resp := s.answer(buildQuery(t, 10, "web."+Zone, qtypeA))
	if flags := binary.BigEndian.Uint16(resp[2:4]); flags&0x000F != rcodeFail {
		t.Fatal("при недоступном госте хочу SERVFAIL")
	}
}

func TestAnswerGarbage(t *testing.T) {
	s := New(func() (string, error) { return "10.0.0.5", nil })
	if s.answer(nil) != nil || s.answer([]byte{1, 2, 3}) != nil {
		t.Fatal("мусор должен игнорироваться")
	}
	// Ответ, а не запрос (бит QR) — игнорируем.
	q := buildQuery(t, 1, "web."+Zone, qtypeA)
	q[2] |= 0x80
	if s.answer(q) != nil {
		t.Fatal("DNS-ответ на входе должен игнорироваться")
	}
}

var errTest = errForTest()

type testErr struct{}

func (testErr) Error() string { return "test" }

func errForTest() error { return testErr{} }

func TestParseName(t *testing.T) {
	msg := buildQuery(t, 1, "web."+Zone, qtypeA)
	name, end, ok := parseName(msg, 12)
	if !ok || name != "web."+Zone {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
	if binary.BigEndian.Uint16(msg[end:end+2]) != qtypeA {
		t.Fatal("end указывает не на QTYPE")
	}
	// Обрезанное сообщение.
	if _, _, ok := parseName(msg[:14], 12); ok {
		t.Fatal("обрезанное имя не должно парситься")
	}
}
