package timesync

import (
	"fmt"
	"syscall"
	"time"
)

// Константы adjtimex(2) из <linux/timex.h> (в пакете syscall их нет).
const (
	adjMaxError         = 0x0004
	adjEstError         = 0x0008
	adjStatus           = 0x0010
	adjOffsetSingleshot = 0x8001 // ADJ_ADJTIME|ADJ_OFFSET: семантика adjtime(3)
	staUnsync           = 0x0040
)

// syncedErrorUS — maxerror/esterror (мкс), которые выставляются после
// синхронизации: порядок погрешности замера по vmnet.
const syncedErrorUS = 1000

// Apply применяет поправку offset к CLOCK_REALTIME гостевой ОС (нужен
// CAP_SYS_TIME — агент работает от root):
//   - ActionStep — settimeofday(now+offset): часы переставляются сразу;
//   - ActionSlew — adjtimex(ADJ_OFFSET_SINGLESHOT): ядро догоняет/
//     притормаживает часы со скоростью до 500 ppm (0,5 мс за секунду),
//     без скачков назад;
//   - ActionNone — часы не трогаются.
//
// Затем в ядре снимается флаг STA_UNSYNC — `adjtimex` в госте показывает
// часы синхронизированными (как после ntpd).
func Apply(offset time.Duration, action Action) error {
	switch action {
	case ActionStep:
		tv := syscall.NsecToTimeval(time.Now().Add(offset).UnixNano())
		if err := syscall.Settimeofday(&tv); err != nil {
			return fmt.Errorf("settimeofday: %w", err)
		}
	case ActionSlew:
		tx := syscall.Timex{Modes: adjOffsetSingleshot, Offset: offset.Microseconds()}
		if _, err := syscall.Adjtimex(&tx); err != nil {
			return fmt.Errorf("adjtimex(singleshot): %w", err)
		}
	}
	var tx syscall.Timex // Modes=0 — только чтение
	if _, err := syscall.Adjtimex(&tx); err != nil {
		return fmt.Errorf("adjtimex: %w", err)
	}
	tx = syscall.Timex{
		Modes:    adjStatus | adjMaxError | adjEstError,
		Status:   tx.Status &^ staUnsync,
		Maxerror: syncedErrorUS,
		Esterror: syncedErrorUS,
	}
	if _, err := syscall.Adjtimex(&tx); err != nil {
		return fmt.Errorf("adjtimex(status): %w", err)
	}
	return nil
}
