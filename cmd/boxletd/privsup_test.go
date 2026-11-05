package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain: в роли boxlet-priv тестовый бинарь либо спит (держит
// «порты»), либо сразу выходит с ошибкой (порты заняты) — см. fakePriv.
func TestMain(m *testing.M) {
	switch os.Getenv("BOXLET_FAKE_PRIV") {
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "fail":
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// fakePriv — супервизор, запускающий тестовый бинарь вместо хелпера;
// starts считает попытки запуска.
func fakePriv(t *testing.T, mode string) (*privSupervisor, *atomic.Int32) {
	t.Helper()
	t.Setenv("BOXLET_FAKE_PRIV", mode)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	s := newPrivSupervisor(func() (string, error) {
		starts.Add(1)
		return self, nil
	}, filepath.Join(t.TempDir(), "boxlet-priv.log"))
	t.Cleanup(s.Stop)
	return s, &starts
}

func alive(s *privSupervisor) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Ровно то, что ломалось: на каждом тике с тем же списком портов
// демон запускал новую копию. Должен работать один процесс.
func TestReconcileKeepsSingleHelper(t *testing.T) {
	s, starts := fakePriv(t, "sleep")
	specs := []string{"443:443", "80:80"}
	for range 10 {
		s.Reconcile(specs)
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("запусков хелпера: %d, ожидался 1", n)
	}
	if !alive(s) {
		t.Fatal("хелпер не работает")
	}
}

func TestReconcileRestartsOnChangeAndStops(t *testing.T) {
	s, starts := fakePriv(t, "sleep")
	s.Reconcile([]string{"80:80"})
	first := s.cmd
	firstDone := s.done

	s.Reconcile([]string{"443:443", "80:80"})
	if n := starts.Load(); n != 2 {
		t.Fatalf("запусков хелпера: %d, ожидалось 2", n)
	}
	select {
	case <-firstDone:
	default:
		t.Fatalf("старый хелпер (pid %d) не остановлен при смене списка", first.Process.Pid)
	}

	// Пустой список (порты <1024 больше не нужны) — хелпера нет.
	s.Reconcile(nil)
	if alive(s) {
		t.Fatal("хелпер не остановлен при пустом списке")
	}
}

// Хелпер, который сразу падает, перезапускается с паузой, а не на
// каждом тике.
func TestReconcileBacksOffFailingHelper(t *testing.T) {
	s, starts := fakePriv(t, "fail")
	now := time.Now()
	s.now = func() time.Time { return now }
	specs := []string{"80:80"}

	s.Reconcile(specs)
	<-s.done // хелпер завершился

	for range 5 {
		s.Reconcile(specs) // тики до истечения паузы
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("запусков до истечения паузы: %d, ожидался 1", n)
	}

	now = now.Add(privBackoffMin)
	s.Reconcile(specs)
	if n := starts.Load(); n != 2 {
		t.Fatalf("после паузы запусков: %d, ожидалось 2", n)
	}
	<-s.done
	s.Reconcile(specs)
	if s.nextTry.Sub(now) != 2*privBackoffMin {
		t.Fatalf("пауза после второй неудачи: %s, ожидалось %s", s.nextTry.Sub(now), 2*privBackoffMin)
	}
}
