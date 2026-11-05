package proxy

import (
	"net"
	"strings"
	"testing"
)

func TestParsePortSpecs(t *testing.T) {
	m, err := ParsePortSpecs([]string{"80:80", "8080:80", "443:443"})
	if err != nil {
		t.Fatal(err)
	}
	if m[80] != 80 || m[8080] != 80 || m[443] != 443 {
		t.Errorf("неверный разбор: %v", m)
	}
}

func TestParsePortSpecsInvalid(t *testing.T) {
	for _, spec := range []string{"", "80", "x:80", "80:x", "0:80", "80:0", "70000:80", "80:80:80"} {
		if _, err := ParsePortSpecs([]string{spec}); err == nil {
			t.Errorf("спека %q должна давать ошибку", spec)
		}
	}
}

// Занятый порт: слушателя нет (Active), а в причине — настоящая ошибка
// bind, а не подменённое «требует root».
func TestSetPortsBusyPortKeepsRealError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	f := NewPortForwarder(func() (string, error) { return "127.0.0.1", nil })
	defer f.Close()
	f.SetPorts(map[int]int{port: port})
	if n := f.Active(); n != 0 {
		t.Fatalf("Active() = %d при занятом порте, ожидался 0", n)
	}
	if reason := f.failed[port]; !strings.Contains(reason, "address already in use") || strings.Contains(reason, "требует root") {
		t.Fatalf("причина: %q", reason)
	}
}
