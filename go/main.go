package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"go.bug.st/serial"
)

// v4.0.0 — kompletny refactor architektoniczny. Patrz docs/...
// max485-library.md sekcja "Changelog v4.0.0" + plan implementacji w sesji
// 2026-05-24. Wszystkie zmiany A1–A21 wdrożone w jednym tagu.
//
// Mapa modułów:
//   - main.go      — core protokół Modbus RTU + lifecycle ModbusDevice
//   - types.go     — struct ModbusDevice + ModbusException + rpio refcount + port mutex registry
//   - transceiver.go — Transceiver interface + 3 implementacje (ISL43485, MAX485, Auto)
//   - stats.go     — atomic counters + snapshot API
//   - lockfile.go  — UNIX /var/lock/LCK..* lockfile dla exclusive port access
//   - binding.go   — cgo + napi shim (oddzielnie: każdy handler async via napi_async_work)

// ---------- Function codes (F5.1 / A8) ----------
//
// Magic numbers zniknęły z hot path'u. Tabela expectedResponseLength()
// w jednym miejscu — łatwiej weryfikować, łatwiej dodać nowy FC.

const (
	FCReadCoils              byte = 0x01
	FCReadDiscreteInputs     byte = 0x02
	FCReadHoldingRegisters   byte = 0x03
	FCReadInputRegisters     byte = 0x04
	FCWriteSingleCoil        byte = 0x05
	FCWriteSingleRegister    byte = 0x06
	FCWriteMultipleCoils     byte = 0x0F
	FCWriteMultipleRegisters byte = 0x10
)

// expectedResponseLength zwraca oczekiwaną długość ODPOWIEDZI (bez exception)
// dla danego FC i count parametru.
//
// Format ogólny normalnej odpowiedzi (FC nie-exception):
//   read*  = [slaveID, FC, byteCount, ...data..., crc_lo, crc_hi]
//   write* = [slaveID, FC, addr_hi, addr_lo, qty_hi, qty_lo, crc_lo, crc_hi] = 8 bytes
//
// Exception responses zawsze = 5 bytes [slaveID, FC|0x80, exceptionCode, crc_lo, crc_hi]
// — sprawdzane w sendModbusRequest niezależnie od tej funkcji.
func expectedResponseLength(fc byte, count uint16) int {
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		// 1 bit per coil, byteCount = ceil(count/8), header 3 + data + CRC 2
		return 3 + int((count+7)/8) + 2
	case FCReadHoldingRegisters, FCReadInputRegisters:
		// 2 bytes per register
		return 3 + 2*int(count) + 2
	case FCWriteSingleCoil, FCWriteSingleRegister,
		FCWriteMultipleCoils, FCWriteMultipleRegisters:
		return 8
	default:
		// Unknown FC — najbezpieczniej fallback do minimum (exception=5).
		return 5
	}
}

// ---------- Timing constants ----------
//
// v4.0.0: zlikwidowane blind sleeps dzięki natywnemu API go.bug.st/serial
// (Drain, SetReadTimeout, ResetInputBuffer). Pozostałe stałe są spec-driven.

const (
	// M9 / requestTimeout: overall deadline na sendModbusRequest.
	// Typowa op @ 9600 baud = 24-32 ms. 300ms = ~10× margin dla edge cases.
	requestTimeout = 300 * time.Millisecond

	// SetReadTimeout per syscall — granularność deadline check.
	readSliceTimeout = 50 * time.Millisecond
)

// ifg liczy 3.5-character inter-frame gap dla danego baud rate (Modbus RTU spec).
// @ 9600/8N1: 3.5 * 11 bits / 9600 = ~4ms. @ 115200: ~0.33ms.
// Spec: między dwoma ramkami musi być ≥3.5 char silence; nasze IFG używamy:
//   1. Po Drain() przed enableRX() — daje slave'owi szansę zacząć nadawać.
//   2. Po ResetInputBuffer() przed Write() — bus settle, last byte poprzedniej
//      operacji wchodzi do bufora i jest jeszcze raz wyzerowany w razie czego.
func (d *ModbusDevice) ifg() time.Duration {
	if d.baudRate <= 0 {
		return 4 * time.Millisecond // safe fallback
	}
	// 3.5 char * 11 bits = 38.5 bit periods. period_us = 1e6 / baud.
	// total_us = 38_500_000 / baud.
	us := 38500000 / d.baudRate
	if us < 1 {
		us = 1
	}
	return time.Duration(us) * time.Microsecond
}

// dbg / dbgHex — per-instance debug logging (F3.3 / A13).
// Wcześniej globalny env var MAX485_DEBUG; teraz field na ModbusDevice
// — dwie instancje mogą mieć różne poziomy.
func (d *ModbusDevice) dbg(format string, args ...interface{}) {
	if d == nil || d.debugLevel < 1 {
		return
	}
	fmt.Fprintf(os.Stderr, "[max485] "+format+"\n", args...)
}

func (d *ModbusDevice) dbgHex(label string, data []byte) {
	if d == nil || d.debugLevel < 2 {
		return
	}
	fmt.Fprintf(os.Stderr, "[max485] %s: %s\n", label, hex.EncodeToString(data))
}

// ---------- ModbusError type (legacy, zachowany dla API stability) ----------

type ModbusError int

const (
	ModbusSuccess ModbusError = iota
	ModbusCRCError
	ModbusTimeoutError
	ModbusInvalidResponse
	ModbusSerialError
)

// ---------- NewModbusDevice / Close ----------

// NewModbusDeviceOptions enkapsuluje wszystkie parametry konstruktora —
// łatwiej rozszerzać bez breaking sygnatury.
//
// Transceiver budowany jest WEWNĄTRZ NewModbusDevice (po acquireRpio)
// na podstawie TransceiverType + pinów. Dzięki temu pin operacje (Pin().Output())
// wykonują się dopiero gdy mmap rpio jest aktywny.
type NewModbusDeviceOptions struct {
	PortName        string
	BaudRate        int
	TransceiverType string       // "isl43485" | "max485" | "auto"
	DePin           int          // dla isl43485, max485
	RePin           int          // tylko dla isl43485
	DebugLevel      int          // 0..2
	RetryConfig     *RetryConfig // nil = brak retry
	SkipPortLock    bool         // testy mogą pominąć lockfile (np. tmpfs)
}

func validateTransceiverType(t string) error {
	switch t {
	case "isl43485", "max485", "auto":
		return nil
	default:
		return fmt.Errorf("unknown transceiver type: %s (expected isl43485|max485|auto)", t)
	}
}

func transceiverNeedsRpio(t string) bool {
	return t == "isl43485" || t == "max485"
}

func buildTransceiver(t string, dePin, rePin int) (Transceiver, error) {
	switch t {
	case "isl43485":
		return NewGPIOTransceiverISL43485(dePin, rePin), nil
	case "max485":
		return NewGPIOTransceiverMAX485(dePin), nil
	case "auto":
		return NewAutoTransceiver(), nil
	default:
		return nil, fmt.Errorf("unknown transceiver type: %s", t)
	}
}

// NewModbusDevice tworzy nowy ModbusDevice z pełną walidacją i defensive
// cleanup w razie partial init failure.
//
// v4.0.0 sygnatura (A6 + opts pattern): wszystko przez NewModbusDeviceOptions.
// Sygnatura zachowuje też legacy form NewModbusDeviceLegacy dla wstecznego
// dostępu z binding'u.
func NewModbusDevice(opts NewModbusDeviceOptions) (*ModbusDevice, error) {
	if opts.PortName == "" {
		return nil, fmt.Errorf("port name required")
	}
	if opts.BaudRate <= 0 {
		return nil, fmt.Errorf("baud rate must be > 0, got %d", opts.BaudRate)
	}
	if err := validateTransceiverType(opts.TransceiverType); err != nil {
		return nil, err
	}

	// F1.3 / A16 — exclusive lock przed openem portu.
	var lockPath string
	if !opts.SkipPortLock {
		var err error
		lockPath, err = acquirePortLock(opts.PortName)
		if err != nil {
			return nil, err
		}
	}

	// F1.4 / A2 — rpio refcount tylko jeśli transceiver wymaga GPIO.
	needsRpio := transceiverNeedsRpio(opts.TransceiverType)
	if needsRpio {
		if err := acquireRpio(); err != nil {
			releasePortLock(lockPath)
			return nil, fmt.Errorf("failed to initialize GPIO: %v", err)
		}
	}

	mode := &serial.Mode{
		BaudRate: opts.BaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(opts.PortName, mode)
	if err != nil {
		if needsRpio {
			releaseRpio()
		}
		releasePortLock(lockPath)
		return nil, fmt.Errorf("failed to open serial port: %v", err)
	}

	// M3 / defensive cleanup
	success := false
	defer func() {
		if !success {
			_ = port.Close()
			if needsRpio {
				releaseRpio()
			}
			releasePortLock(lockPath)
		}
	}()

	if err := port.SetReadTimeout(readSliceTimeout); err != nil {
		return nil, fmt.Errorf("failed to set read timeout: %v", err)
	}

	// Transceiver tworzymy TUTAJ — po acquireRpio (Pin().Output() wymaga mmap'a).
	tx, err := buildTransceiver(opts.TransceiverType, opts.DePin, opts.RePin)
	if err != nil {
		return nil, err
	}

	d := &ModbusDevice{
		port:        port,
		transceiver: tx,
		baudRate:    opts.BaudRate,
		mu:          getPortMutex(opts.PortName),
		lockFile:    lockPath,
		debugLevel:  opts.DebugLevel,
		retryConfig: opts.RetryConfig,
		stats:       newAtomicStats(),
		needsRpio:   needsRpio,
	}

	success = true
	return d, nil
}

// Close zwalnia wszystkie zasoby. Idempotent (M2) — drugie wywołanie = no-op.
//
// Lifecycle release order (odwrotny do acquire):
//   1. transceiver.Close() — bus-idle (driver disabled, receiver enabled).
//   2. port.Close() — file descriptor zwolniony.
//   3. releaseRpio() — rpio refcount-- (mmap unmap'owany przy ostatnim).
//   4. releasePortLock() — lockfile removed.
func (d *ModbusDevice) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true

	if d.transceiver != nil {
		_ = d.transceiver.Close()
	}
	if d.port != nil {
		_ = d.port.Close()
		d.port = nil
	}
	if d.needsRpio {
		releaseRpio()
		d.needsRpio = false
	}
	releasePortLock(d.lockFile)
	d.lockFile = ""
}

// SetDebug — F3.3 / A13. Per-instance debug level setter.
func (d *ModbusDevice) SetDebug(level int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.debugLevel = level
}

// SetRetryConfig — F2.3 / A11. Włącz/wyłącz retry policy.
func (d *ModbusDevice) SetRetryConfig(cfg *RetryConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retryConfig = cfg
}

// Stats — F3.4 / A10. Snapshot counters.
func (d *ModbusDevice) Stats() Stats {
	return d.stats.snapshot()
}

// ---------- CRC ----------

func calculateCRC(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 == 1 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// ---------- sendModbusRequest ----------
//
// Core bus operation. Chroniony bus mutex'em (per-port registry — F2.1).
// Wszystkie błędy I/O liczone do stats.
func (d *ModbusDevice) sendModbusRequest(request []byte, expectedLength int) ([]byte, error) {
	// F2.3 / A11 — retry wrap
	cfg := d.retryConfig
	maxAttempts := 1
	backoff := time.Duration(0)
	if cfg != nil && cfg.MaxRetries > 0 {
		maxAttempts = 1 + cfg.MaxRetries
		backoff = cfg.Backoff
	}

	slaveID := byte(0)
	if len(request) > 0 {
		slaveID = request[0]
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			d.dbg("retry %d/%d after error: %v (backoff %v)",
				attempt, maxAttempts-1, lastErr, backoff)
			time.Sleep(backoff)
		}
		start := time.Now()
		resp, err := d.sendOnce(request, expectedLength)
		elapsed := time.Since(start)
		d.stats.record(slaveID, err, elapsed)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// Retry tylko transient. ModbusException = permanent.
		if !isTransientError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (d *ModbusDevice) sendOnce(request []byte, expectedLength int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed || d.port == nil {
		return nil, fmt.Errorf("modbus device is closed")
	}

	// N3 — defensive copy + CRC append
	bodyLen := len(request)
	frame := make([]byte, bodyLen+2)
	copy(frame, request)
	crc := calculateCRC(request)
	frame[bodyLen] = byte(crc & 0xFF)
	frame[bodyLen+1] = byte(crc >> 8)
	d.dbgHex("TX", frame)

	expectedSlaveID := request[0]
	expectedFC := request[1]

	// F2.2 / A7: bus settle + reset stale bytes (tail-end poprzedniej operacji).
	// ifg() = 3.5 char @ baud — daje czas żeby ostatni byte wszedł do bufora.
	if err := d.port.ResetInputBuffer(); err != nil {
		return nil, fmt.Errorf("failed to reset input buffer: %v", err)
	}
	time.Sleep(d.ifg())
	// Second reset — wszystko co przybyło w trakcie settle.
	if err := d.port.ResetInputBuffer(); err != nil {
		return nil, fmt.Errorf("failed to reset input buffer (2nd pass): %v", err)
	}

	// Enable TX, write frame, drain to HW FIFO empty, switch to RX.
	if err := d.transceiver.EnableTX(); err != nil {
		return nil, fmt.Errorf("transceiver enable TX: %v", err)
	}
	d.stats.markTx()
	n, err := d.port.Write(frame)
	if err != nil {
		_ = d.transceiver.EnableRX()
		return nil, fmt.Errorf("write: %v", err)
	}
	if n != len(frame) {
		_ = d.transceiver.EnableRX()
		return nil, fmt.Errorf("short write: %d of %d", n, len(frame))
	}
	if err := d.port.Drain(); err != nil {
		_ = d.transceiver.EnableRX()
		return nil, fmt.Errorf("drain: %v", err)
	}
	if err := d.transceiver.EnableRX(); err != nil {
		return nil, fmt.Errorf("transceiver enable RX: %v", err)
	}
	// F1.2 + spec — slave używa IFG do detekcji frame boundary; dajemy chwilę.
	time.Sleep(d.ifg())

	// Read response z deadline'em i F2.2 slaveID skip-until-match.
	exceptionLen := 5
	maxLen := expectedLength
	if exceptionLen > maxLen {
		maxLen = exceptionLen
	}
	response := make([]byte, maxLen)
	totalRead := 0
	deadline := time.Now().Add(requestTimeout)

	// F2.2: skip-until-slaveID. Czytamy do bufora pomocniczego dopóki nie
	// trafimy na byte == expectedSlaveID. Stray bytes (od poprzedniego slave'a
	// który łamie spec i odpowiedział po deadline'ie) są dropped + logged.
	tmpBuf := make([]byte, 1)
	for totalRead == 0 && time.Now().Before(deadline) {
		nRead, err := d.port.Read(tmpBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		if nRead == 0 {
			continue
		}
		if tmpBuf[0] == expectedSlaveID {
			response[0] = tmpBuf[0]
			totalRead = 1
			d.stats.markRx()
			break
		}
		d.dbg("dropped stray byte 0x%02X (expected slave 0x%02X)", tmpBuf[0], expectedSlaveID)
	}
	if totalRead == 0 {
		return nil, fmt.Errorf("modbus timeout: no response from slave 0x%02X within %v",
			expectedSlaveID, requestTimeout)
	}

	// Phase 1: czytaj do min(exceptionLen, expectedLength) — wystarczy do detekcji exception.
	phaseTarget := exceptionLen
	if expectedLength < phaseTarget {
		phaseTarget = expectedLength
	}
	for totalRead < phaseTarget && time.Now().Before(deadline) {
		nRead, err := d.port.Read(response[totalRead:phaseTarget])
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		if nRead > 0 {
			totalRead += nRead
		}
	}

	// Exception detection: FC|0x80 (MSB set)
	isException := totalRead >= 2 && (response[1]&0x80) != 0
	finalLen := expectedLength
	if isException {
		finalLen = exceptionLen
	}

	// Phase 2: dokończ do finalLen
	for totalRead < finalLen && time.Now().Before(deadline) {
		nRead, err := d.port.Read(response[totalRead:finalLen])
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		if nRead > 0 {
			totalRead += nRead
		}
	}

	if totalRead < finalLen {
		d.dbg("RX timeout: got %d/%d bytes within %v", totalRead, finalLen, requestTimeout)
		return nil, fmt.Errorf("modbus timeout: got %d/%d bytes within %v",
			totalRead, finalLen, requestTimeout)
	}
	d.dbgHex("RX", response[:finalLen])

	// Verify CRC (przed FC check)
	receivedCRC := binary.LittleEndian.Uint16(response[finalLen-2:])
	calculatedCRC := calculateCRC(response[:finalLen-2])
	if receivedCRC != calculatedCRC {
		return nil, fmt.Errorf("CRC error: received %04X, calculated %04X", receivedCRC, calculatedCRC)
	}

	// C2 — typed ModbusException
	if isException {
		return nil, &ModbusException{
			SlaveID:       response[0],
			FunctionCode:  expectedFC,
			ExceptionCode: response[2],
		}
	}

	if response[1] != expectedFC {
		return nil, fmt.Errorf("invalid function code in response: got 0x%02X, expected 0x%02X",
			response[1], expectedFC)
	}

	return response[:finalLen], nil
}

// ---------- ReadCoils / ReadDiscreteInputs / ReadHoldingRegisters / ReadInputRegisters ----------

func (d *ModbusDevice) ReadCoils(slaveID byte, startAddr, count uint16) ([]bool, error) {
	request := []byte{
		slaveID, FCReadCoils,
		byte(startAddr >> 8), byte(startAddr & 0xFF),
		byte(count >> 8), byte(count & 0xFF),
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCReadCoils, count))
	if err != nil {
		return nil, err
	}
	return unpackBits(response, count), nil
}

func (d *ModbusDevice) ReadDiscreteInputs(slaveID byte, startAddr, count uint16) ([]bool, error) {
	request := []byte{
		slaveID, FCReadDiscreteInputs,
		byte(startAddr >> 8), byte(startAddr & 0xFF),
		byte(count >> 8), byte(count & 0xFF),
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCReadDiscreteInputs, count))
	if err != nil {
		return nil, err
	}
	return unpackBits(response, count), nil
}

func unpackBits(response []byte, count uint16) []bool {
	byteCount := response[2]
	result := make([]bool, count)
	for i := uint16(0); i < count; i++ {
		byteIndex := i / 8
		bitIndex := i % 8
		if byteIndex < uint16(byteCount) {
			result[i] = (response[3+byteIndex] & (1 << bitIndex)) != 0
		}
	}
	return result
}

func (d *ModbusDevice) ReadHoldingRegisters(slaveID byte, startAddr, count uint16) ([]uint16, error) {
	request := []byte{
		slaveID, FCReadHoldingRegisters,
		byte(startAddr >> 8), byte(startAddr & 0xFF),
		byte(count >> 8), byte(count & 0xFF),
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCReadHoldingRegisters, count))
	if err != nil {
		return nil, err
	}
	return unpackRegisters(response, count), nil
}

func (d *ModbusDevice) ReadInputRegisters(slaveID byte, startAddr, count uint16) ([]uint16, error) {
	request := []byte{
		slaveID, FCReadInputRegisters,
		byte(startAddr >> 8), byte(startAddr & 0xFF),
		byte(count >> 8), byte(count & 0xFF),
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCReadInputRegisters, count))
	if err != nil {
		return nil, err
	}
	return unpackRegisters(response, count), nil
}

func unpackRegisters(response []byte, count uint16) []uint16 {
	byteCount := response[2]
	result := make([]uint16, count)
	for i := uint16(0); i < count; i++ {
		if 2*i+1 < uint16(byteCount) {
			result[i] = uint16(response[3+2*i])<<8 | uint16(response[4+2*i])
		}
	}
	return result
}

// ---------- Write operations ----------

func verifyWriteEcho(op string, request, response []byte) error {
	for i := 0; i < 6; i++ {
		if response[i] != request[i] {
			return fmt.Errorf("%s: response echo mismatch at byte %d: got 0x%02X, expected 0x%02X (full got % X expected % X)",
				op, i, response[i], request[i], response[:6], request[:6])
		}
	}
	return nil
}

// preserveModbusException — fmt.Errorf("...: %v", err) zgubiłoby typ
// *ModbusException (binding.go robi type assertion na *ModbusException żeby
// zbudować structured JS error). Helper zachowuje typ jeśli wrapped.
func preserveModbusException(prefix string, err error) error {
	if _, ok := err.(*ModbusException); ok {
		return err // PASS-THROUGH typed
	}
	return fmt.Errorf("%s: %v", prefix, err)
}

func (d *ModbusDevice) WriteCoil(slaveID byte, coilAddr uint16, value bool) error {
	request := []byte{
		slaveID, FCWriteSingleCoil,
		byte(coilAddr >> 8), byte(coilAddr & 0xFF),
		0x00, 0x00,
	}
	if value {
		request[4] = 0xFF
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCWriteSingleCoil, 0))
	if err != nil {
		return preserveModbusException("failed to write coil", err)
	}
	return verifyWriteEcho("write_coil", request, response)
}

func (d *ModbusDevice) WriteRegister(slaveID byte, regAddr, value uint16) error {
	request := []byte{
		slaveID, FCWriteSingleRegister,
		byte(regAddr >> 8), byte(regAddr & 0xFF),
		byte(value >> 8), byte(value & 0xFF),
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCWriteSingleRegister, 0))
	if err != nil {
		return preserveModbusException("failed to write register", err)
	}
	return verifyWriteEcho("write_register", request, response)
}

func (d *ModbusDevice) WriteMultipleCoils(slaveID byte, startAddr uint16, values []bool) error {
	byteCount := (len(values) + 7) / 8
	request := make([]byte, 7+byteCount)
	request[0] = slaveID
	request[1] = FCWriteMultipleCoils
	request[2] = byte(startAddr >> 8)
	request[3] = byte(startAddr & 0xFF)
	request[4] = byte(len(values) >> 8)
	request[5] = byte(len(values) & 0xFF)
	request[6] = byte(byteCount)
	for i, value := range values {
		if value {
			request[7+i/8] |= 1 << (i % 8)
		}
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCWriteMultipleCoils, uint16(len(values))))
	if err != nil {
		return preserveModbusException("failed to write multiple coils", err)
	}
	return verifyWriteEcho("write_multiple_coils", request, response)
}

func (d *ModbusDevice) WriteMultipleRegisters(slaveID byte, startAddr uint16, values []uint16) error {
	request := make([]byte, 7+2*len(values))
	request[0] = slaveID
	request[1] = FCWriteMultipleRegisters
	request[2] = byte(startAddr >> 8)
	request[3] = byte(startAddr & 0xFF)
	request[4] = byte(len(values) >> 8)
	request[5] = byte(len(values) & 0xFF)
	request[6] = byte(2 * len(values))
	for i, value := range values {
		request[7+2*i] = byte(value >> 8)
		request[8+2*i] = byte(value & 0xFF)
	}
	response, err := d.sendModbusRequest(request, expectedResponseLength(FCWriteMultipleRegisters, uint16(len(values))))
	if err != nil {
		return preserveModbusException("failed to write multiple registers", err)
	}
	return verifyWriteEcho("write_multiple_registers", request, response)
}

// ---------- main() — pusty stub bo shared lib (F5.3 / A17) ----------
//
// Buildmode c-shared wymaga package main z func main(). CLI został
// przeniesiony do cmd/modbus-cli/main.go jako osobny pakiet.
func main() {}
