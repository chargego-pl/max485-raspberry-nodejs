package main

import (
	"sync"
	"time"

	"github.com/stianeikeland/go-rpio/v4"
	"go.bug.st/serial"
)

// ModbusDevice represents a Modbus RTU device.
//
// THREAD SAFETY: metody dotykające bus'a (Read*, Write*, sendModbusRequest)
// acquire `mu` for the duration of the operation. Multiple goroutines may call
// methods concurrently — they will be serialized on the bus, not interleaved.
//
// MUTEX SCOPE (F2.1 / A3): `mu` jest pobierany z pakietowego registry per port
// path (getPortMutex). Dwie instancje na tym samym `/dev/serial0` współdzielą
// jeden mutex → nigdy nie kolidują na busie nawet jeśli ktoś omyłkowo otworzył
// drugą instancję (i tak F1.3 lockfile by ją zablokowało, ale to belt-and-braces).
//
// PORT FIELD CONVENTION (A5): pole `port` jest dotykane WYŁĄCZNIE w
// sendModbusRequest i Close, OBA pod mutexem. Po Close → port = nil i flag
// `closed = true`. Inne metody NIE czytają port bezpośrednio.
//
// v4.0.0 (A6): zamiast pojedynczych pinów DE/RE używamy interface'u
// Transceiver — pozwala wymienić driver RS485 (GPIO+ISL43485, GPIO+MAX485,
// USB↔RS485 auto-direction) bez modyfikacji core'a.
type ModbusDevice struct {
	port        serial.Port
	transceiver Transceiver
	baudRate    int          // F1.2 / A12 — do liczenia IFG (3.5 char @ baud)
	mu          *sync.Mutex  // F2.1 / A3 — per-port mutex z registry
	closed      bool
	lockFile    string       // F1.3 / A16 — ścieżka lockfile, pusta jeśli unsupported
	needsRpio   bool         // F1.4 / A2 — czy ten device zacquired rpio (do release)

	// F3.3 / A13 — per-instance debug (zamiast globalnego env var).
	// 0=off, 1=basic events, 2=basic + hex dump TX/RX.
	debugLevel int

	// F2.3 / A11 — opcjonalny retry. nil = brak retry (default).
	retryConfig *RetryConfig

	// F3.4 / A10 — observability counters. Atomic + per-slave mapa pod own lockiem.
	stats *atomicStats
}

// ModbusException to typed error reprezentujący odpowiedź slave z FC|0x80.
//
// F3.1 / A21: w binding.go ten typ jest rozpoznawany i konwertowany na
// natywny JS Error z structured properties (code='MODBUS_EXCEPTION', slaveID,
// functionCode, exceptionCode) — consumer może rozróżnić "illegal address"
// od "device busy" bez parse'owania message string'a.
type ModbusException struct {
	SlaveID       byte
	FunctionCode  byte // oryginalny FC (bez bitu 0x80)
	ExceptionCode byte // 0x01..0x0B
}

func (e *ModbusException) Error() string {
	desc := exceptionDescription(e.ExceptionCode)
	return "modbus exception: slave=" + byteHex(e.SlaveID) + " fc=" + byteHex(e.FunctionCode) +
		" code=" + byteHex(e.ExceptionCode) + " (" + desc + ")"
}

// RetryConfig — F2.3 / A11. Opcjonalna polityka retry dla błędów transient
// (timeout, CRC). Exception responses (modbus exception code) NIE są retry'owane
// bo to "permanent" błędy aplikacyjne (illegal address etc.).
type RetryConfig struct {
	MaxRetries int
	Backoff    time.Duration
}

// Default predicate dla retry — true jeśli błąd jest transient.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	// ModbusException NIE retry'ujemy (permanent application error).
	if _, ok := err.(*ModbusException); ok {
		return false
	}
	// Wszystko inne (timeout, CRC, IO) traktujemy jako transient.
	return true
}

func exceptionDescription(code byte) string {
	switch code {
	case 0x01:
		return "illegal function"
	case 0x02:
		return "illegal data address"
	case 0x03:
		return "illegal data value"
	case 0x04:
		return "slave device failure"
	case 0x05:
		return "acknowledge"
	case 0x06:
		return "slave device busy"
	case 0x08:
		return "memory parity error"
	case 0x0A:
		return "gateway path unavailable"
	case 0x0B:
		return "gateway target device failed to respond"
	default:
		return "unknown"
	}
}

func byteHex(b byte) string {
	const hex = "0123456789abcdef"
	return "0x" + string([]byte{hex[b>>4], hex[b&0x0F]})
}

// ---------- F1.4 / A2: rpio reference counting ----------
//
// rpio.Open() to process-global mmap /dev/gpiomem. Wcześniej Close NIE wywoływał
// rpio.Close (komentarz "process-global, leave open"), co znaczyło że nawet po
// zwolnieniu wszystkich ModbusDevice mmap pozostawał — łagodny leak (~4KB)
// ale nieprawidłowy. Teraz: reference-counted Open/Close. Pierwsze Open
// rzeczywiście mmap'uje, ostatnie Close unmap'uje.

var (
	rpioRefCount int
	rpioMu       sync.Mutex
)

func acquireRpio() error {
	rpioMu.Lock()
	defer rpioMu.Unlock()
	if rpioRefCount == 0 {
		if err := rpio.Open(); err != nil {
			return err
		}
	}
	rpioRefCount++
	return nil
}

func releaseRpio() {
	rpioMu.Lock()
	defer rpioMu.Unlock()
	if rpioRefCount > 0 {
		rpioRefCount--
		if rpioRefCount == 0 {
			_ = rpio.Close()
		}
	}
}

// ---------- F2.1 / A3: per-bus mutex registry ----------

var (
	portMutexes   = map[string]*sync.Mutex{}
	portMutexesMu sync.Mutex
)

func getPortMutex(portName string) *sync.Mutex {
	portMutexesMu.Lock()
	defer portMutexesMu.Unlock()
	if m, ok := portMutexes[portName]; ok {
		return m
	}
	m := &sync.Mutex{}
	portMutexes[portName] = m
	return m
}
