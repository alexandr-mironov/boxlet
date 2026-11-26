// Package timesync — синхронизация часов гостевой ОС с часами хоста
// через гостевой агент boxlet-agent.
//
// Зачем: в гостевой ОС нет NTP, а время в ней расходится с хостом рывками —
// при загрузке (RTC отдаёт время с точностью до секунды) и после сна хоста
// (VM стоит на паузе). Контейнеры (Postgres и т.п.) видят now() раньше, чем
// метки времени, которые пишут процессы на хосте, — тесты с условиями вида
// now() BETWEEN created_at AND ... падают. Синхронизация от хоста не зависит
// от интернета и держит гостя в пределах долей миллисекунды от хоста.
//
// Протокол (поверх соединения агента после токена и строки argv = Command):
//
//	гость → хост: 'T'                 запрос времени
//	хост → гость: 8 байт big-endian   time.Now().UnixNano() хоста
//	... (Rounds раз)
//	гость → хост: 'R' + JSON Report + '\n', затем закрытие соединения.
//
// Ведёт обмен гость: он измеряет RTT своими монотонными часами и берёт
// замер с минимальным RTT (как SNTP), смещение = время_хоста − середина
// интервала запроса.
package timesync

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Command — argv[0], по которому агент вместо запуска процесса
// выполняет синхронизацию часов. '@' не встречается в именах команд.
const Command = "@timesync"

// Rounds — число замеров за один сеанс; берётся замер с минимальным RTT.
const Rounds = 8

// StepThreshold — при расхождении не меньше этого часы гостя
// переставляются сразу (шаг), меньше — плавно подкручиваются (slew),
// чтобы время в госте не прыгало назад на мелких поправках.
const StepThreshold = 50 * time.Millisecond

// MinAdjust — поправки меньше этого не применяются: это уровень шума
// измерения (половина RTT в vmnet — десятки–сотни микросекунд).
const MinAdjust = 100 * time.Microsecond

// MaxRTT — замер с RTT больше этого не годится для синхронизации
// (VM только что вышла из паузы, хост перегружен): ошибка ± RTT/2.
const MaxRTT = 50 * time.Millisecond

const (
	msgTime   = 'T'
	msgReport = 'R'
)

// Action — что сделано с часами гостя.
type Action string

const (
	ActionNone Action = "none" // расхождение в пределах шума
	ActionSlew Action = "slew" // плавная подстройка (adjtimex)
	ActionStep Action = "step" // перестановка часов (settimeofday)
)

// Report — итог сеанса синхронизации, гость отдаёт его хосту.
type Report struct {
	Offset time.Duration `json:"offset_ns"` // время хоста − время гостя до поправки
	RTT    time.Duration `json:"rtt_ns"`    // RTT выбранного замера
	Action Action        `json:"action"`
	Error  string        `json:"error,omitempty"`
}

func (r Report) String() string {
	s := fmt.Sprintf("смещение %+.3f мс (RTT %.3f мс), %s",
		float64(r.Offset)/1e6, float64(r.RTT)/1e6, r.Action)
	if r.Error != "" {
		s += ": " + r.Error
	}
	return s
}

// Sample — один замер.
type Sample struct {
	Offset time.Duration
	RTT    time.Duration
}

// Best выбирает замер с минимальным RTT: у него наименьшая
// неопределённость смещения (± RTT/2).
func Best(samples []Sample) (Sample, error) {
	if len(samples) == 0 {
		return Sample{}, errors.New("нет замеров")
	}
	best := samples[0]
	for _, s := range samples[1:] {
		if s.RTT < best.RTT {
			best = s
		}
	}
	return best, nil
}

// Decide выбирает действие для смещения offset (хост − гость).
func Decide(offset time.Duration) Action {
	a := offset.Abs()
	switch {
	case a < MinAdjust:
		return ActionNone
	case a < StepThreshold:
		return ActionSlew
	default:
		return ActionStep
	}
}

// Adjuster применяет поправку к часам гостя (см. Apply). Вызывается
// и с ActionNone — тогда часы не трогаются.
type Adjuster func(offset time.Duration, action Action) error

// Guest — сторона гостевой ОС (агент): ведёт обмен с хостом по rw,
// вычисляет смещение, применяет поправку через adjust и отправляет
// отчёт хосту. now — часы гостя (time.Now; в тестах — подмена,
// для RTT используется монотонная составляющая).
func Guest(rw io.ReadWriter, now func() time.Time, adjust Adjuster) (Report, error) {
	var samples []Sample
	var buf [8]byte
	for range Rounds {
		t0 := now()
		if _, err := rw.Write([]byte{msgTime}); err != nil {
			return Report{}, err
		}
		if _, err := io.ReadFull(rw, buf[:]); err != nil {
			return Report{}, fmt.Errorf("ответ хоста: %w", err)
		}
		t1 := now()
		host := time.Unix(0, int64(binary.BigEndian.Uint64(buf[:])))
		rtt := t1.Sub(t0)
		// Время хоста соответствует середине интервала запроса.
		// host без монотонной составляющей — Sub считает по стенным часам.
		samples = append(samples, Sample{Offset: host.Sub(t0.Add(rtt / 2)), RTT: rtt})
	}
	best, _ := Best(samples)
	rep := Report{Offset: best.Offset, RTT: best.RTT, Action: ActionNone}
	switch {
	case best.RTT > MaxRTT:
		rep.Error = fmt.Sprintf("RTT %v больше %v — замер ненадёжен, часы не тронуты", best.RTT, MaxRTT)
	default:
		// adjust вызывается и для ActionNone: часы не трогает,
		// но отмечает их в ядре синхронизированными.
		rep.Action = Decide(best.Offset)
		if err := adjust(best.Offset, rep.Action); err != nil {
			rep.Error = err.Error()
		}
	}
	data, _ := json.Marshal(rep)
	if _, err := rw.Write(append(append([]byte{msgReport}, data...), '\n')); err != nil {
		return rep, err
	}
	return rep, nil
}

// ErrUnsupported — агент в госте не знает команду синхронизации
// (старый boxlet-agent): он попытался запустить "@timesync" как процесс.
var ErrUnsupported = errors.New("гостевой агент не поддерживает синхронизацию часов (обновите boxlet и перезапустите VM)")

// Host — сторона хоста: отвечает на запросы времени гостя своими часами
// now (time.Now) и возвращает отчёт гостя.
func Host(rw io.ReadWriter, now func() time.Time) (Report, error) {
	br := bufio.NewReader(rw)
	var buf [8]byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return Report{}, fmt.Errorf("обмен с гостем: %w", err)
		}
		switch b {
		case msgTime:
			binary.BigEndian.PutUint64(buf[:], uint64(now().UnixNano()))
			if _, err := rw.Write(buf[:]); err != nil {
				return Report{}, err
			}
		case msgReport:
			line, err := br.ReadBytes('\n')
			if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
				return Report{}, fmt.Errorf("отчёт гостя: %w", err)
			}
			var rep Report
			if err := json.Unmarshal(line, &rep); err != nil {
				return Report{}, fmt.Errorf("отчёт гостя: %w", err)
			}
			if rep.Error != "" {
				return rep, errors.New(rep.Error)
			}
			return rep, nil
		default:
			// Агент ответил текстом: старый агент пытался запустить
			// "@timesync" как процесс ("\nboxlet-agent: exec: ..."),
			// при неверном токене — "boxlet-agent: неверный токен".
			// Дочитываем строку, чтобы не советовать обновить boxlet
			// там, где причина в другом.
			line, _ := br.ReadBytes('\n')
			msg := strings.TrimSpace(string(b) + string(line))
			if i := strings.Index(msg, "boxlet-agent:"); i >= 0 {
				msg = strings.TrimSpace(msg[i+len("boxlet-agent:"):])
				if !strings.Contains(msg, "exec") {
					return Report{}, fmt.Errorf("гостевой агент: %s", msg)
				}
			}
			return Report{}, ErrUnsupported
		}
	}
}

// HostClockJumped сообщает, что между двумя тиками хоста стенные часы
// ушли от монотонных больше чем на порог: хост спал (на macOS монотонные
// часы Go во сне стоят) или его часы переставили. В обоих случаях
// часы гостя надо синхронизировать немедленно.
func HostClockJumped(wallElapsed, monoElapsed time.Duration) bool {
	return (wallElapsed - monoElapsed).Abs() > time.Second
}
