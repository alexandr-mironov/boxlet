package proxy

import "testing"

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
