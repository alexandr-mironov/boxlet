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
	ts.sync(session) // как в start: первый сеанс синхронно
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
