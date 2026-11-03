// boxlet — CLI менеджера контейнеров Boxlet.
//
// Тонкий клиент: команды управления транслирует в демон boxletd
// через его HTTP API; init/doctor работают локально.
package main

import (
	"fmt"
	"os"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch cmd := os.Args[1]; cmd {
	case "init":
		err = cmdInit(os.Args[2:])
	case "start":
		err = cmdStart(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "shell":
		err = cmdShell(os.Args[2:])
	case "ps":
		err = cmdDockerPassthrough(append([]string{"ps"}, os.Args[2:]...))
	case "ui":
		err = cmdUI(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("boxlet %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "boxlet: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`boxlet — лёгкий менеджер контейнеров, совместимый с Docker

Использование:
  boxlet init      первичная настройка (конфиг, образ VM, сокет)
  boxlet start     запустить движок
  boxlet stop      остановить движок
  boxlet status    состояние движка и демона
  boxlet doctor    проверка окружения
  boxlet shell     shell внутри VM бэкенда
  boxlet ps        список контейнеров (через docker CLI)
  boxlet ui        открыть веб-интерфейс
  boxlet version   версия

Docker API доступен на ~/.boxlet/run/docker.sock
`)
}
