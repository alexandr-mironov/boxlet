package main

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// privBackoffMin/Max — пауза перед повторным запуском хелпера,
	// который завершился или не запустился: растёт вдвое с каждой
	// неудачей подряд. Без паузы демон порождал бы его на каждом тике.
	privBackoffMin = 2 * time.Second
	privBackoffMax = 5 * time.Minute
	// privHealthyRun — сколько хелпер должен проработать, чтобы его
	// выход не считался неудачей (сбрасывает backoff).
	privHealthyRun = 30 * time.Second
	// privStopTimeout — сколько ждём выхода хелпера после SIGTERM.
	privStopTimeout = 5 * time.Second
)

// privSupervisor держит не больше одного процесса `boxlet-priv forward`
// с актуальным списком портов: перезапускает его при смене списка,
// дожидается (reap) каждого запущенного процесса и после неудачных
// запусков выдерживает растущую паузу.
type privSupervisor struct {
	path    func() (string, error) // путь к установленному хелперу
	logPath string
	now     func() time.Time

	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{} // закрывается, когда cmd завершился
	started  time.Time
	key      string // список портов запущенного (или последнего) хелпера
	failures int    // неудач подряд для текущего key
	nextTry  time.Time
}

func newPrivSupervisor(path func() (string, error), logPath string) *privSupervisor {
	return &privSupervisor{path: path, logPath: logPath, now: time.Now}
}

// Reconcile приводит хелпер к списку specs ("host:guest", отсортирован):
// пустой список — хелпера нет, тот же список — ничего не делаем,
// другой — перезапуск. Вызывается на каждом тике проброса портов.
func (s *privSupervisor) Reconcile(specs []string) {
	key := strings.Join(specs, ",")
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil {
		select {
		case <-s.done:
			s.reapedLocked()
		default:
			if key == s.key {
				return // актуальный хелпер работает
			}
			s.stopLocked()
		}
	}
	if key != s.key {
		// Новый список стоит попробовать сразу, без паузы прошлых неудач.
		s.key, s.failures, s.nextTry = key, 0, time.Time{}
	}
	if len(specs) == 0 || s.now().Before(s.nextTry) {
		return
	}

	path, err := s.path()
	if err != nil {
		s.failedLocked("путь к boxlet-priv: %v", err)
		return
	}
	cmd := exec.Command(path, append([]string{"forward"}, specs...)...)
	var logFile *os.File
	if f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		logFile = f
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		s.failedLocked("запуск boxlet-priv: %v", err)
		return
	}
	done := make(chan struct{})
	s.cmd, s.done, s.started = cmd, done, s.now()
	go func() {
		err := cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
		log.Printf("boxletd: boxlet-priv (pid %d) завершился: %v", cmd.Process.Pid, err)
		close(done)
	}()
	log.Printf("boxletd: boxlet-priv (pid %d): порты %s", cmd.Process.Pid, key)
}

// Stop останавливает хелпер (при остановке движка или демона).
func (s *privSupervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		s.stopLocked()
	}
	s.key, s.failures, s.nextTry = "", 0, time.Time{}
}

// stopLocked: SIGTERM, ожидание выхода, при необходимости SIGKILL.
// Хелпер — дочерний процесс демона, поэтому после него не остаётся
// ни зомби, ни сироты.
func (s *privSupervisor) stopLocked() {
	s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(privStopTimeout):
		log.Printf("boxletd: boxlet-priv (pid %d) не вышел по SIGTERM, SIGKILL", s.cmd.Process.Pid)
		s.cmd.Process.Kill()
		<-s.done
	}
	s.cmd, s.done = nil, nil
}

// reapedLocked учитывает самостоятельный выход хелпера: быстрый выход
// (занятые порты, нет прав, повреждённый бинарь) — неудача с паузой
// перед следующей попыткой, долгая работа — нормальный перезапуск.
func (s *privSupervisor) reapedLocked() {
	ranFor := s.now().Sub(s.started)
	s.cmd, s.done = nil, nil
	if ranFor >= privHealthyRun {
		s.failures, s.nextTry = 0, time.Time{}
		return
	}
	s.failedLocked("boxlet-priv завершился через %s", ranFor.Round(time.Millisecond))
}

func (s *privSupervisor) failedLocked(format string, args ...any) {
	s.failures++
	backoff := privBackoffMin << (s.failures - 1)
	if backoff > privBackoffMax || backoff <= 0 {
		backoff = privBackoffMax
	}
	s.nextTry = s.now().Add(backoff)
	log.Printf("boxletd: "+format+"; повтор через %s", append(args, backoff)...)
}
