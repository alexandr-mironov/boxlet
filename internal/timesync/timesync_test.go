package timesync

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		offset time.Duration
		want   Action
	}{
		{0, ActionNone},
		{50 * time.Microsecond, ActionNone},
		{-99 * time.Microsecond, ActionNone},
		{MinAdjust, ActionSlew},
		{-3 * time.Millisecond, ActionSlew},
		{49 * time.Millisecond, ActionSlew},
		{StepThreshold, ActionStep},
		{-1050 * time.Millisecond, ActionStep}, // отставание после загрузки
		{-2 * time.Hour, ActionStep},           // после сна хоста
		{time.Second, ActionStep},              // гость спешит
	}
	for _, c := range cases {
		if got := Decide(c.offset); got != c.want {
			t.Errorf("Decide(%v) = %s, want %s", c.offset, got, c.want)
		}
	}
}

func TestBest(t *testing.T) {
	if _, err := Best(nil); err == nil {
		t.Fatal("Best(nil): ожидалась ошибка")
	}
	got, err := Best([]Sample{
		{Offset: 5 * time.Millisecond, RTT: 3 * time.Millisecond},
		{Offset: 1 * time.Millisecond, RTT: 200 * time.Microsecond},
		{Offset: 9 * time.Millisecond, RTT: 10 * time.Millisecond},
	})
	if err != nil || got.RTT != 200*time.Microsecond || got.Offset != time.Millisecond {
		t.Fatalf("Best = %+v, %v; want минимальный RTT", got, err)
	}
}

func TestHostClockJumped(t *testing.T) {
	if HostClockJumped(2*time.Second, 2*time.Second) {
		t.Error("обычный тик принят за скачок")
	}
	if HostClockJumped(2*time.Second+300*time.Millisecond, 2*time.Second) {
		t.Error("подстройка timed (доли секунды) принята за скачок")
	}
	if !HostClockJumped(time.Hour, 2*time.Second) {
		t.Error("сон хоста не распознан")
	}
	if !HostClockJumped(-5*time.Second, 2*time.Second) {
		t.Error("перестановка часов хоста назад не распознана")
	}
}

// fakeClock — часы с заданным смещением от «настоящих» и фиксированным
// шагом на каждый вызов (имитирует RTT).
type fakeClock struct {
	t    time.Time
	step time.Duration
}

func (c *fakeClock) now() time.Time {
	c.t = c.t.Add(c.step)
	return c.t
}

// runPair гоняет Guest и Host по net.Pipe: часы гостя отстают от хоста
// на lag, каждое чтение часов гостя сдвигает их на guestStep.
func runPair(t *testing.T, lag, guestStep time.Duration, adjust Adjuster) (Report, Report, error) {
	t.Helper()
	base := time.Unix(1_795_592_079, 0)
	guestClock := &fakeClock{t: base.Add(-lag), step: guestStep}
	// Хост отвечает «в середине» интервала гостя: его время = время гостя
	// на момент ответа + lag. Часы гостя сдвигаются при чтении на guestStep,
	// так что середина интервала — текущее время гостя + guestStep/2.
	hostNow := func() time.Time { return guestClock.t.Add(lag + guestStep/2) }

	g, h := net.Pipe()
	defer g.Close()
	defer h.Close()
	type res struct {
		rep Report
		err error
	}
	gch := make(chan res, 1)
	go func() {
		rep, err := Guest(g, guestClock.now, adjust)
		g.Close()
		gch <- res{rep, err}
	}()
	hrep, herr := Host(h, hostNow)
	gr := <-gch
	if gr.err != nil {
		t.Fatalf("Guest: %v", gr.err)
	}
	return gr.rep, hrep, herr
}

func TestSyncStepAfterBoot(t *testing.T) {
	var gotOffset time.Duration
	var gotAction Action
	adjust := func(off time.Duration, a Action) error { gotOffset, gotAction = off, a; return nil }

	lag := 1046 * time.Millisecond // замер 2026-11-25: гость отстаёт на ~1,05 с
	grep, hrep, err := runPair(t, lag, 300*time.Microsecond, adjust)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if gotAction != ActionStep || gotOffset != lag {
		t.Fatalf("adjust(%v, %s), want (%v, step)", gotOffset, gotAction, lag)
	}
	if hrep != grep {
		t.Fatalf("отчёт хоста %+v != отчёт гостя %+v", hrep, grep)
	}
	if hrep.RTT != 300*time.Microsecond || hrep.Action != ActionStep {
		t.Fatalf("отчёт %+v", hrep)
	}
}

func TestSyncSlewAndNone(t *testing.T) {
	var gotAction Action
	adjust := func(_ time.Duration, a Action) error { gotAction = a; return nil }

	if _, _, err := runPair(t, -3*time.Millisecond, 200*time.Microsecond, adjust); err != nil {
		t.Fatal(err)
	}
	if gotAction != ActionSlew {
		t.Fatalf("гость спешит на 3 мс: действие %s, want slew", gotAction)
	}
	rep, _, err := runPair(t, 20*time.Microsecond, 200*time.Microsecond, adjust)
	if err != nil {
		t.Fatal(err)
	}
	// adjust вызывается и для none — чтобы снять STA_UNSYNC.
	if gotAction != ActionNone || rep.Action != ActionNone {
		t.Fatalf("расхождение 20 мкс: действие %s, want none", gotAction)
	}
}

func TestSyncLargeRTTLeavesClockAlone(t *testing.T) {
	called := false
	adjust := func(time.Duration, Action) error { called = true; return nil }
	rep, _, err := runPair(t, time.Second, 2*MaxRTT, adjust)
	if err == nil || called {
		t.Fatalf("RTT %v: err=%v, adjust вызван=%v; want ошибку без поправки", 2*MaxRTT, err, called)
	}
	if rep.Action != ActionNone {
		t.Fatalf("отчёт гостя: %+v", rep)
	}
}

func TestSyncAdjustError(t *testing.T) {
	adjust := func(time.Duration, Action) error { return errors.New("EPERM") }
	_, _, err := runPair(t, time.Second, 100*time.Microsecond, adjust)
	if err == nil || !strings.Contains(err.Error(), "EPERM") {
		t.Fatalf("ошибка подстройки не дошла до хоста: %v", err)
	}
}

// Старый агент не знает @timesync и отвечает текстом ошибки exec.
func TestHostOldAgent(t *testing.T) {
	g, h := net.Pipe()
	go func() {
		io.WriteString(g, "\nboxlet-agent: exec: \"@timesync\": executable file not found in $PATH\n")
		g.Close()
	}()
	_, err := Host(h, time.Now)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Host со старым агентом: %v, want ErrUnsupported", err)
	}
}

// Агент с неверным токеном отвечает текстом — Host возвращает само
// сообщение агента, а не ErrUnsupported (совет «обновите boxlet»
// ввёл бы в заблуждение).
func TestHostAgentAuthError(t *testing.T) {
	g, h := net.Pipe()
	go func() {
		io.WriteString(g, "boxlet-agent: неверный токен\n")
		g.Close()
	}()
	_, err := Host(h, time.Now)
	if err == nil || errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "неверный токен") {
		t.Fatalf("Host при неверном токене: %v, want текст ошибки агента", err)
	}
}
