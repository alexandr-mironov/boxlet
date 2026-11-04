//go:build darwin

package vmnet

import "testing"

const sampleLeases = `{
	ip_address=192.168.64.2
	hw_address=1,4e:0:cc:be:24:fe
	identifier=1,4e:0:cc:be:24:fe
	lease=0x6aeb3951
	name=localhost
}
{
	ip_address=192.168.64.3
	hw_address=1,b2:e4:c8:9b:5a:49
	lease=0x6aea364d
	name=other
}
`

func TestParseLeases(t *testing.T) {
	// Октеты MAC в leases без ведущих нулей, у нас — с нулями.
	if ip := parseLeases([]byte(sampleLeases), "4e:00:cc:be:24:fe"); ip != "192.168.64.2" {
		t.Errorf("ожидал 192.168.64.2, получил %q", ip)
	}
	if ip := parseLeases([]byte(sampleLeases), "B2:E4:C8:9B:5A:49"); ip != "192.168.64.3" {
		t.Errorf("ожидал 192.168.64.3 (верхний регистр), получил %q", ip)
	}
	if ip := parseLeases([]byte(sampleLeases), "aa:bb:cc:dd:ee:ff"); ip != "" {
		t.Errorf("неизвестный MAC должен дать пустую строку, получил %q", ip)
	}
}

func TestParseLeasesReversedFields(t *testing.T) {
	// Порядок полей в блоке не гарантирован.
	data := []byte("{\n\thw_address=1,4e:0:cc:be:24:fe\n\tip_address=192.168.64.9\n}\n")
	if ip := parseLeases(data, "4e:00:cc:be:24:fe"); ip != "192.168.64.9" {
		t.Errorf("ожидал 192.168.64.9, получил %q", ip)
	}
}
