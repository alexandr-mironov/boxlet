package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	"github.com/alexandr-mironov/boxlet/internal/guest"
	"github.com/alexandr-mironov/boxlet/internal/timesync"
)

// Синхронизация часов гостевой ОС с хостом (см. internal/timesync).
const (
	// timeSyncInterval — плановая синхронизация. Гость считает время тем же
	// аппаратным счётчиком, что и хост, так что между сеансами он почти не
	// уходит; период нужен, чтобы догонять подстройки часов хоста (timed).
	timeSyncInterval = 15 * time.Second
	// timeSyncTick — как часто проверять, не проснулся ли хост:
	// после сна синхронизация нужна сразу, а не через timeSyncInterval.
	timeSyncTick = 2 * time.Second
	// timeSyncTimeout — на один сеанс (8 обменов по vmnet — миллисекунды).
	timeSyncTimeout = 5 * time.Second
)

// timeSyncer периодически синхронизирует часы гостевой ОС с хостом через
// гостевой агент: при старте движка, каждые timeSyncInterval и сразу после
// пробуждения хоста (или перестановки его часов).
type timeSyncer struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	last   time.Time // время последнего успешного сеанса
	tried  time.Time // время последней попытки
	report timesync.Report
	err    error
}

// sessionFunc — один сеанс синхронизации (подменяется в тестах).
type sessionFunc func(ctx context.Context) (timesync.Report, error)

// agentSession возвращает сеанс синхронизации через агента бэкенда;
// nil — у бэкенда нет гостевого агента (нативный linux).
func agentSession(be backend.Backend, root string) sessionFunc {
	sh, ok := be.(backend.Sheller)
	if !ok {
		return nil
	}
	return func(ctx context.Context) (timesync.Report, error) {
		addr, err := sh.AgentAddr()
		if err != nil {
			return timesync.Report{}, err
		}
		token, err := guest.AgentToken(root)
		if err != nil {
			return timesync.Report{}, err
		}
		return backend.AgentTimeSync(ctx, addr, token)
	}
}

// start выполняет первую синхронизацию синхронно (до того, как наружу
// отдан сокет Docker) и запускает фоновый цикл. Повторный вызов
// перезапускает цикл.
func (t *timeSyncer) start(session sessionFunc) {
	if session == nil {
		return
	}
	t.stop()
	t.sync(session)
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
	go t.loop(ctx, session, time.NewTicker(timeSyncTick).C, hostClock())
}

func (t *timeSyncer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
}

// clockFunc — показания часов хоста: стенное время (без монотонной
// составляющей) и монотонное время от старта цикла.
type clockFunc func() (wall time.Time, mono time.Duration)

// hostClock — настоящие часы хоста. На macOS монотонные часы Go
// (mach_absolute_time) во сне стоят, стенные — идут.
func hostClock() clockFunc {
	start := time.Now()
	return func() (time.Time, time.Duration) {
		n := time.Now()
		return n.Round(0), n.Sub(start)
	}
}

// loop — фоновый цикл: на каждом тике решает, пора ли синхронизироваться.
// Расхождение прироста стенных и монотонных часов выдаёт сон хоста.
func (t *timeSyncer) loop(ctx context.Context, session sessionFunc, tick <-chan time.Time, clock clockFunc) {
	prevWall, prevMono := clock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		wall, mono := clock()
		jumped := timesync.HostClockJumped(wall.Sub(prevWall), mono-prevMono)
		prevWall, prevMono = wall, mono
		if jumped {
			log.Printf("boxletd: часы хоста скакнули (сон/перестановка) — синхронизирую часы гостя")
		}
		if t.due(time.Now(), jumped) {
			t.sync(session)
		}
	}
}

// due — пора ли синхронизироваться: после скачка часов хоста, после
// неудачного сеанса (агент ещё не поднялся, VM выходит из паузы) или
// по истечении timeSyncInterval. Старый агент без поддержки синхронизации
// не долбим на каждом тике — только по интервалу.
func (t *timeSyncer) due(now time.Time, jumped bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	retry := t.err != nil && !errors.Is(t.err, timesync.ErrUnsupported)
	return jumped || retry || t.tried.IsZero() || now.Sub(t.tried) >= timeSyncInterval
}

// sync — один сеанс. В лог пишутся перестановки часов и смена
// состояния ошибки, а не каждый плановый сеанс.
func (t *timeSyncer) sync(session sessionFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeSyncTimeout)
	rep, err := session(ctx)
	cancel()

	t.mu.Lock()
	prevErr := t.err
	t.report, t.err = rep, err
	t.tried = time.Now()
	if err == nil {
		t.last = time.Now()
	}
	t.mu.Unlock()

	switch {
	case err != nil && (prevErr == nil || prevErr.Error() != err.Error()):
		log.Printf("boxletd: синхронизация часов гостя: %v", err)
	case err == nil && prevErr != nil:
		log.Printf("boxletd: синхронизация часов гостя восстановлена: %s", rep)
	case err == nil && rep.Action == timesync.ActionStep:
		log.Printf("boxletd: часы гостя переставлены: %s", rep)
	}
}

// status — состояние для /api/status.
func (t *timeSyncer) status() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.IsZero() && t.err == nil {
		return nil
	}
	out := map[string]any{}
	if !t.last.IsZero() {
		out["last"] = t.last.Format(time.RFC3339)
		out["offset_ms"] = float64(t.report.Offset) / 1e6
		out["rtt_ms"] = float64(t.report.RTT) / 1e6
		out["action"] = t.report.Action
	}
	if t.err != nil {
		out["error"] = t.err.Error()
	}
	return out
}
