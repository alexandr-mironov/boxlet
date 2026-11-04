// boxlet-agent — агент внутри гостевой ОС: слушает TCP :2376 и выполняет
// команды с проксированием stdio. Используется для `boxlet shell`.
//
// Протокол (одно соединение = одна команда):
//   - клиент шлёт первую строку: argv через \x00, конец — \n;
//     пустая строка → интерактивный sh;
//   - дальше сырой двунаправленный поток: stdin команды ← соединение,
//     stdout+stderr команды → соединение; по завершении команды
//     соединение закрывается.
//
// Доступен только с хоста: на darwin-vz сеть vmnet NAT не публикует
// порт наружу, на qemu hostfwd привязан к 127.0.0.1.
package main

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

const addr = ":2376"

// tokenFile — токен кладётся в гостевую ОС через apkovl.
const tokenFile = "/etc/boxlet-agent.token"

// token — загружается при старте. Без токена агент не работает:
// это root-shell в гостевой ОС, а токен приходит отдельно от apkovl
// (fw_cfg/virtiofs) и может не доехать — тогда безопаснее отказать.
var token string

func main() {
	log.SetPrefix("boxlet-agent: ")
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		log.Fatalf("%s не найден — без токена агент не запускается: %v", tokenFile, err)
	}
	if token = strings.TrimSpace(string(data)); token == "" {
		log.Fatalf("%s пуст — без токена агент не запускается", tokenFile)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("слушаю %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	// Первая строка — токен аутентификации.
	auth, err := br.ReadString('\n')
	if err != nil {
		return
	}
	if subtle.ConstantTimeCompare(
		[]byte(strings.TrimRight(auth, "\r\n")), []byte(token)) != 1 {
		fmt.Fprint(conn, "boxlet-agent: неверный токен\n")
		return
	}
	// Вторая строка — argv через \x00; пустая → sh.
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var argv []string
	for _, p := range strings.Split(strings.TrimRight(string(line), "\r\n"), "\x00") {
		if p != "" {
			argv = append(argv, p)
		}
	}
	if len(argv) == 0 {
		argv = []string{"sh"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// В br могли остаться байты stdin, пришедшие в одном пакете со строкой argv.
	cmd.Stdin = io.MultiReader(br, conn)
	cmd.Stdout = conn
	cmd.Stderr = conn
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(conn, "\nboxlet-agent: %v\n", err)
	}
}
