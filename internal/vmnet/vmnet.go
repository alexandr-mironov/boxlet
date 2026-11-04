//go:build darwin

// Package vmnet — работа с NAT-сетью Virtualization.framework:
// адрес шлюза и поиск IP гостевой ОС по MAC в leases vmnet.
package vmnet

import (
	"os"
	"regexp"
	"strings"
)

// vmnetPlist — конфигурация сети Virtualization.framework (NAT).
const vmnetPlist = "/Library/Preferences/SystemConfiguration/com.apple.vmnet.plist"

// sharedNetRe извлекает адрес шлюза из plist (ключ Shared_Net_Address).
var sharedNetRe = regexp.MustCompile(`(?s)Shared_Net_Address</key>\s*<string>([0-9.]+)</string>`)

// GatewayIP возвращает адрес шлюза vmnet (обычно 192.168.64.1),
// читая com.apple.vmnet.plist; при любой ошибке — разумный дефолт.
func GatewayIP() string {
	const fallback = "192.168.64.1"
	data, err := os.ReadFile(vmnetPlist)
	if err != nil {
		return fallback
	}
	if m := sharedNetRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return fallback
}

// Поля записи в /var/db/dhcpd_leases. Порядок полей в блоке не
// гарантирован, а октеты MAC пишутся без ведущих нулей (4e:0:cc:…),
// поэтому разбираем блоки целиком и нормализуем каждый октет.
var (
	leaseBlockRe = regexp.MustCompile(`(?s)\{([^}]*)\}`)
	leaseIPRe    = regexp.MustCompile(`ip_address=([0-9.]+)`)
	leaseHWRe    = regexp.MustCompile(`hw_address=1,([0-9a-fA-F:]+)`)
)

// LookupLease находит IP гостевой ОС по её MAC в /var/db/dhcpd_leases.
// Пустая строка — lease пока не выдан.
func LookupLease(mac string) (string, error) {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return "", err
	}
	return parseLeases(data, mac), nil
}

// parseLeases разбирает содержимое dhcpd_leases. Вынесено для тестов.
func parseLeases(data []byte, mac string) string {
	want := normMAC(mac)
	for _, block := range leaseBlockRe.FindAllSubmatch(data, -1) {
		hw := leaseHWRe.FindSubmatch(block[1])
		ip := leaseIPRe.FindSubmatch(block[1])
		if hw == nil || ip == nil {
			continue
		}
		if normMAC(string(hw[1])) == want {
			return string(ip[1])
		}
	}
	return ""
}

// normMAC приводит MAC к 12 шестнадцатеричным цифрам без разделителей.
func normMAC(mac string) string {
	parts := strings.Split(mac, ":")
	var b strings.Builder
	for _, p := range parts {
		if len(p) == 1 {
			b.WriteByte('0')
		}
		b.WriteString(strings.ToLower(p))
	}
	return b.String()
}
