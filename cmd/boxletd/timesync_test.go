package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/timesync"
)

func TestTimeSyncerDue(t *testing.T) {
	now := time.Now()
	var ts timeSyncer
	if !ts.due(now, false) {
		t.Fatal("до первой попытки синхронизация должна быть нужна")
	}
	ts.tried, ts.last = now, now
	if ts.due(now.Add(timeSyncTick), false) {
		t.Fatal("сразу после успешного сеанса синхронизация не нужна")
	}
	if !ts.due(now.Add(timeSyncTick), true) {
		t.Fatal("после сна хоста синхронизация нужна сразу")
	}
	if !ts.due(now.Add(timeSyncInterval), false) {
		t.Fatal("по истечении интервала синхронизация нужна")
	}
	ts.err = errors.New("connection refused")
	if !ts.due(now.Add(timeSyncTick), false) {
		t.Fatal("после ошибки — повтор на следующем тике")
	}
	ts.err = timesync.ErrUnsupported
	if ts.due(now.Add(timeSyncTick), false) {
		t.Fatal("старый агент не опрашиваем на каждом тике")
	}
}

// Повтор после ошибки идёт с backoff: следующий тик, затем удвоение
// до планового интервала — устойчивая ошибка не гоняет сеанс каждые 2 с.
func TestTimeSyncerRetryBackoff(t *testing.T) {
	now := time.Now()
	var ts timeSyncer
	ts.tried = now
	ts.err = errors.New("connection refused")
	for i, want := range []time.Duration{
		timeSyncTick,     // 1-я ошибка: повтор на следующем тике
		2 * timeSyncTick, // 2-я: через 4 с
		4 * timeSyncTick, // 3-я: через 8 с
		timeSyncInterval, // дальше — не чаще планового интервала
	} {
		ts.fails = i + 1
		if ts.due(now.Add(want-time.Nanosecond), false) {
			t.Fatalf("fails=%d: повтор раньше %v", ts.fails, want)
		}
		if !ts.due(now.Add(want), false) {
			t.Fatalf("fails=%d: повтор не наступил в %v", ts.fails, want)
		}
	}
}

// После успешного сеанса и следующей ошибки статус показывает замер
// успешного сеанса и ошибку последней попытки — не пустой отчёт.
func TestTimeSyncerStatusKeepsLastReport(t *testing.T) {
	var ts timeSyncer
	ok := func(context.Context) (timesync.Report, error) {
		return timesync.Report{Offset: 891 * time.Millisecond, RTT: 200 * time.Microsecond, Action: timesync.ActionStep}, nil
	}
	fail := func(context.Context) (timesync.Report, error) {
		return timesync.Report{}, errors.New("connection refused")
	}
	ts.sync(context.Background(), ok)
	ts.sync(context.Background(), fail)
	st := ts.status()
	if st == nil {
		t.Fatal("статус пуст")
	}
	if st["offset_ms"] != 891.0 || st["action"] != timesync.ActionStep {
		t.Fatalf("статус потерял отчёт успешного сеанса: %v", st)
	}
	if st["error"] != "connection refused" {
		t.Fatalf("ошибка последней попытки не попала в статус: %v", st)
	}
}

// stop во время первого сеанса: возвращённый контекст отменён, цикл
// не делает новых сеансов после остановки.
func TestTimeSyncerStopDuringFirstSync(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	session := func(ctx context.Context) (timesync.Report, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done() // сеанс висит, пока его не отменят
		return timesync.Report{}, ctx.Err()
	}
	var ts timeSyncer
	ctxch := make(chan context.Context, 1)
	go func() { ctxch <- ts.start(session) }()
	<-started
	ts.stop()
	ctx := <-ctxch
	if ctx.Err() == nil {
		t.Fatal("контекст запуска не отменён после stop")
	}
	time.Sleep(100 * time.Millisecond) // даём циклу шанс ошибочно сработать
	if got := calls.Load(); got != 1 {
		t.Fatalf("сеансов %d после stop, want 1", got)
	}
}

// Цикл: первый тик без скачка часов не синхронизирует (только что был
// сеанс), тик после «сна» хоста — синхронизирует сразу.
func TestTimeSyncerLoopWake(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{}, 10)
	session := func(context.Context) (timesync.Report, error) {
		calls.Add(1)
		done <- struct{}{}
		return timesync.Report{Action: timesync.ActionStep}, nil
	}

	var ts timeSyncer
	ts.sync(context.Background(), session) // как в start: первый сеанс синхронно
	<-done

	// Часы цикла: монотонные идут ровно по тику, а стенные на третьем
	// показании убегают на час (хост проспал).
	base := time.Now().Round(0)
	readings := []struct {
		wall time.Time
		mono time.Duration
	}{
		{base, 0},
		{base.Add(timeSyncTick), timeSyncTick},
		{base.Add(2*timeSyncTick + time.Hour), 2 * timeSyncTick},
	}
	var i int
	clock := func() (time.Time, time.Duration) {
		r := readings[i]
		i++
		return r.wall, r.mono
	}

	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ts.loop(ctx, session, tick, clock)

	tick <- time.Time{} // обычный тик
	tick <- time.Time{} // тик после сна
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("после сна хоста синхронизации не было")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("сеансов %d, want 2 (старт + после сна)", got)
	}
	st := ts.status()
	if st == nil || st["action"] != timesync.ActionStep {
		t.Fatalf("status() = %v", st)
	}
}
