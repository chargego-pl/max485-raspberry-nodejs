package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/stianeikeland/go-rpio/v4"
	"go.bug.st/serial"
)

// N5: debug logging behind env var MAX485_DEBUG=1 (lub =hex dla pełnego dump TX/RX).
// Domyślnie ciche — żeby produkcja nie wypluwała loga na stderr na każdą operację.
// Touchapp-control może włączyć przez `Environment=MAX485_DEBUG=1` w drop-inie.
var debugMode = os.Getenv("MAX485_DEBUG")

func dbg(format string, args ...interface{}) {
	if debugMode == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[max485] "+format+"\n", args...)
}

func dbgHex(label string, data []byte) {
	if debugMode != "hex" {
		return
	}
	fmt.Fprintf(os.Stderr, "[max485] %s: %s\n", label, hex.EncodeToString(data))
}

// Timing configuration.
//
// v4.0.0: migracja na go.bug.st/serial eliminuje większość workaroundów:
// - byteSendDelay → 0 (port.Write blokuje do FIFO copy; Drain() później czeka na drain)
// - postSendDelay → 0 (port.Drain() = real blocking wait na HW FIFO empty)
// - tcflushInput → port.ResetInputBuffer() (native API)
// - read deadline → port.SetReadTimeout(requestTimeout) (native)
const (
	// GPIO pin switching delays
	gpioSwitchDelay = 1 * time.Microsecond

	// Pre-send GPIO settling: enableTX → tx_start
	preSendDelay = 0 * time.Millisecond

	// Pre-receive GPIO settling: enableRX → first read
	// Slave Modbus standard: ≥3.5 char inter-frame gap (~3.6ms @ 9600).
	// W praktyce slave odpowiada po ~5ms; my chcemy być w RX gdy zaczyna nadawać.
	preReceiveDelay = 1 * time.Millisecond

	// M9: overall I/O deadline na CAŁY sendModbusRequest (od enableTX do return).
	// Typowa operacja @ 9600 baud = 24-32 ms. 300ms daje ~10× margin dla edge cases
	// (slow slave processing, dribbling response). Po deadline zwracamy timeout error.
	requestTimeout = 300 * time.Millisecond
)

// ModbusError represents possible Modbus errors
type ModbusError int

const (
	ModbusSuccess ModbusError = iota
	ModbusCRCError
	ModbusTimeoutError
	ModbusInvalidResponse
	ModbusSerialError
)

// NewModbusDevice creates a new Modbus device.
//
// v4.0.0: użycie go.bug.st/serial.v1 API — Open(name, *Mode) zamiast OpenPort(*Config).
// M3: defensive cleanup pattern — `success` flag + defer cleanup. Każdy step
// init może zawieść; jeśli zawiedzie po częściowej alokacji, defer cleanup
// zamyka co już zostało otwarte (port). rpio.Close NIE wywoływane bo process-global.
func NewModbusDevice(portName string, baudRate int, dePin, rePin int) (*ModbusDevice, error) {
	mode := &serial.Mode{
		BaudRate: baudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(portName, mode)
	if err != nil {
		return nil, fmt.Errorf("failed to open serial port: %v", err)
	}
	// M3: jeśli niżej coś się wykraczy, port musi być zamknięty
	success := false
	defer func() {
		if !success {
			_ = port.Close()
		}
	}()

	// SetReadTimeout — używamy short timeout (50ms), pętla read sprawdza overall deadline.
	// bug.st obsługuje natywnie (zamiast trym/serial 5s blocking workaround).
	if err := port.SetReadTimeout(50 * time.Millisecond); err != nil {
		return nil, fmt.Errorf("failed to set read timeout: %v", err)
	}

	// Initialize GPIO. rpio.Open jest process-global (vide M1 z review).
	if err := rpio.Open(); err != nil {
		return nil, fmt.Errorf("failed to initialize GPIO: %v", err)
	}

	de := rpio.Pin(dePin)
	re := rpio.Pin(rePin)
	de.Output()
	re.Output()
	// Set initial state to receive mode (idle, bus zwolniony)
	de.Low()
	re.Low()

	success = true // wszystko OK — defer cleanup NIE zamknie portu
	return &ModbusDevice{
		port:  port,
		dePin: de,
		rePin: re,
	}, nil
}

// Close closes the Modbus device
func (d *ModbusDevice) Close() {
	// M2: idempotent — drugie wywołanie no-op (zamiast double-free / panic).
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	// STABILITY: wymuś stan RX (driver disabled, receiver enabled) przed close.
	// Bez tego po crash/SIGKILL pin DE pozostawałby HIGH → ISL napędza bus
	// stale → bus zablokowany dla innych masterów. Kolejność jak w enableRX().
	d.rePin.Low()
	d.dePin.Low()
	if d.port != nil {
		d.port.Close()
		d.port = nil
	}
	// rpio.Close() celowo NIE wywoływane — proces-global mmap, drugi ModbusDevice
	// musiałby ponownie Open. Process exit oczyści zasoby.
}

// calculateCRC calculates CRC-16 for Modbus RTU
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

// enableTX enables RS485 transmit mode.
//
// ISL43485IBZ truth table (datasheet Renesas DS, Table 1):
//   /RE  DE | mode               RO        A/B driven
//   ────────┼──────────────────────────────────────────
//    0    0 | receive only       data      no (high-Z)
//    0    1 | transmit + echo    data      yes (driver active)
//    1    0 | SHUTDOWN (≥300 ns) high-Z    no
//    1    1 | transmit only      high-Z    yes
//
// STABILITY: kolejność tutaj wybrana tak by NIGDY nie wpaść w (/RE=1, DE=0).
// Z idle state (DE=0, /RE=0 = RX) → najpierw DE=1 (przejście przez "TX+echo",
// driver się aktywuje a receiver chwilę słyszy własne TX — n/a bo czytamy
// dopiero po enableRX). Następnie /RE=1 (TX only, czyste). Receiver echo
// jest harmless: tarm/serial bufor input jest czyszczony przez tcflushInput
// na początku sendModbusRequest (C3), a my nie czytamy w trakcie TX.
func (d *ModbusDevice) enableTX() {
	d.dePin.High() // (0,0)→(0,1): TX+RX echo. Driver ON. Bezpieczne, NIE shutdown.
	time.Sleep(gpioSwitchDelay)
	d.rePin.High() // (0,1)→(1,1): TX only. Receiver OFF (RO high-Z).
	time.Sleep(gpioSwitchDelay)
}

// enableRX enables RS485 receive mode (inversja enableTX).
// Z TX-only state (/RE=1, DE=1) → najpierw /RE=0 (przejście przez TX+RX echo,
// receiver się aktywuje a driver jeszcze nadaje — n/a bo TX już zakończona),
// potem DE=0 (RX only, bus zwolniony). Także nigdy nie wpadamy w shutdown.
func (d *ModbusDevice) enableRX() {
	d.rePin.Low() // (1,1)→(0,1): TX+RX echo. Receiver ON, driver jeszcze ON.
	time.Sleep(gpioSwitchDelay)
	d.dePin.Low() // (0,1)→(0,0): RX only. Driver OFF, bus zwolniony.
	time.Sleep(gpioSwitchDelay)
}

// sendModbusRequest sends a Modbus request and waits for response.
//
// v4.0.0: blokowy write + native Drain() zamiast byte-by-byte + sleep workarounds.
// C1 (thread-safe): mutex chroni cały cykl TX→RX.
// C3 (state recovery): port.ResetInputBuffer() przed TX czyści śmieci po previous op.
// M9: deadline-based read loop (port.SetReadTimeout 50ms granularity + overall 300ms).
func (d *ModbusDevice) sendModbusRequest(request []byte, expectedLength int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return nil, fmt.Errorf("modbus device is closed")
	}

	// N3: defensive copy — frame buffer w pełni kontrolujemy.
	bodyLen := len(request)
	frame := make([]byte, bodyLen+2)
	copy(frame, request)
	crc := calculateCRC(request)
	frame[bodyLen] = byte(crc & 0xFF)
	frame[bodyLen+1] = byte(crc >> 8)
	request = frame
	dbgHex("TX", request)

	// C3: drop stale bytes from previous (possibly partial) operation — native API z bug.st.
	if err := d.port.ResetInputBuffer(); err != nil {
		return nil, fmt.Errorf("failed to reset input buffer: %v", err)
	}

	// Enable TX, then write frame as single block (PL011 FIFO mieści całą ramkę).
	d.enableTX()
	time.Sleep(preSendDelay)
	n, err := d.port.Write(request)
	if err != nil {
		return nil, fmt.Errorf("write: %v", err)
	}
	if n != len(request) {
		return nil, fmt.Errorf("short write: %d of %d", n, len(request))
	}
	// Q2 SOLVED: port.Drain() — blokujący wait na fizyczne opróżnienie HW UART TX FIFO.
	// Eliminuje stary "blind" postSendDelay 3ms guess. Gwarantowane: po return,
	// ostatni bajt już wyszedł na drut → bezpiecznie zwolnić DE.
	if err := d.port.Drain(); err != nil {
		return nil, fmt.Errorf("drain: %v", err)
	}
	d.enableRX()
	time.Sleep(preReceiveDelay)

	// Read response.
	// C2: Modbus exception ramka jest KRÓTSZA (5 bajtów: slave, FC|0x80, exceptionCode, CRC, CRC)
	// niż normalna odpowiedź. Czytamy najpierw min(5, expectedLength) bajtów, peek FC,
	// jeśli MSB ustawiony — to exception, dalej czytamy do 5 bajtów total. Inaczej do expectedLength.
	exceptionLen := 5
	maxLen := expectedLength
	if exceptionLen > maxLen {
		maxLen = exceptionLen
	}
	response := make([]byte, maxLen)
	totalRead := 0
	deadline := time.Now().Add(requestTimeout)

	// Phase 1: czytaj do min(exceptionLen, expectedLength) — wystarczy do detekcji exception.
	// port.Read blokuje max 50ms (SetReadTimeout), więc pętla sprawdza overall deadline często.
	phaseTarget := exceptionLen
	if expectedLength < phaseTarget {
		phaseTarget = expectedLength
	}
	for totalRead < phaseTarget && time.Now().Before(deadline) {
		n, err := d.port.Read(response[totalRead:])
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		if n > 0 {
			totalRead += n
		}
	}

	// Detekcja exception: FC|0x80 (bit 7 set)
	isException := totalRead >= 2 && (response[1]&0x80) != 0
	finalLen := expectedLength
	if isException {
		finalLen = exceptionLen
	}

	// Phase 2: dokończ czytanie do finalLen z tym samym deadlinem
	for totalRead < finalLen && time.Now().Before(deadline) {
		n, err := d.port.Read(response[totalRead:])
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		if n > 0 {
			totalRead += n
		}
	}

	if totalRead < finalLen {
		dbg("RX timeout: got %d/%d bytes within %v", totalRead, finalLen, requestTimeout)
		return nil, fmt.Errorf("modbus timeout: got %d/%d bytes within %v", totalRead, finalLen, requestTimeout)
	}
	dbgHex("RX", response[:finalLen])

	// Verify slave ID
	if response[0] != request[0] {
		return nil, fmt.Errorf("invalid slave ID in response: got %d, expected %d", response[0], request[0])
	}

	// Verify CRC (przed function code check — gdyby CRC był zły, FC mismatch nieistotny)
	receivedCRC := binary.LittleEndian.Uint16(response[finalLen-2:])
	calculatedCRC := calculateCRC(response[:finalLen-2])
	if receivedCRC != calculatedCRC {
		return nil, fmt.Errorf("CRC error: received %04X, calculated %04X", receivedCRC, calculatedCRC)
	}

	// C2: jeśli exception, zwróć typed error z exceptionCode zamiast generic FC mismatch
	if isException {
		return nil, &ModbusException{
			SlaveID:       response[0],
			FunctionCode:  request[1],
			ExceptionCode: response[2],
		}
	}

	// Verify function code (normalna odpowiedź — FC musi się zgadzać)
	if response[1] != request[1] {
		return nil, fmt.Errorf("invalid function code in response: got %d, expected %d", response[1], request[1])
	}

	return response[:finalLen], nil
}

// ReadCoils reads coils from a Modbus slave
func (d *ModbusDevice) ReadCoils(slaveID byte, startAddr uint16, count uint16) ([]bool, error) {
	request := []byte{
		slaveID,
		0x01,
		byte(startAddr >> 8),
		byte(startAddr & 0xFF),
		byte(count >> 8),
		byte(count & 0xFF),
	}

	expectedLength := 5 + (count+7)/8
	response, err := d.sendModbusRequest(request, int(expectedLength))
	if err != nil {
		return nil, err
	}

	byteCount := response[2]
	result := make([]bool, count)
	for i := uint16(0); i < count; i++ {
		byteIndex := i / 8
		bitIndex := i % 8
		if byteIndex < uint16(byteCount) {
			result[i] = (response[3+byteIndex] & (1 << bitIndex)) != 0
		}
	}

	return result, nil
}

// ReadDiscreteInputs reads discrete inputs from a Modbus slave
func (d *ModbusDevice) ReadDiscreteInputs(slaveID byte, startAddr uint16, count uint16) ([]bool, error) {
	request := []byte{
		slaveID,
		0x02,
		byte(startAddr >> 8),
		byte(startAddr & 0xFF),
		byte(count >> 8),
		byte(count & 0xFF),
	}

	expectedLength := 5 + (count+7)/8
	response, err := d.sendModbusRequest(request, int(expectedLength))
	if err != nil {
		return nil, err
	}

	byteCount := response[2]
	result := make([]bool, count)
	for i := uint16(0); i < count; i++ {
		byteIndex := i / 8
		bitIndex := i % 8
		if byteIndex < uint16(byteCount) {
			result[i] = (response[3+byteIndex] & (1 << bitIndex)) != 0
		}
	}

	return result, nil
}

// ReadHoldingRegisters reads holding registers from a Modbus slave
func (d *ModbusDevice) ReadHoldingRegisters(slaveID byte, startAddr uint16, count uint16) ([]uint16, error) {
	request := []byte{
		slaveID,
		0x03,
		byte(startAddr >> 8),
		byte(startAddr & 0xFF),
		byte(count >> 8),
		byte(count & 0xFF),
	}

	expectedLength := 5 + 2*count
	response, err := d.sendModbusRequest(request, int(expectedLength))
	if err != nil {
		return nil, err
	}

	byteCount := response[2]
	result := make([]uint16, count)
	for i := uint16(0); i < count; i++ {
		if 2*i+1 < uint16(byteCount) {
			result[i] = uint16(response[3+2*i])<<8 | uint16(response[4+2*i])
		}
	}

	return result, nil
}

// ReadInputRegisters reads input registers from a Modbus slave
func (d *ModbusDevice) ReadInputRegisters(slaveID byte, startAddr uint16, count uint16) ([]uint16, error) {
	request := []byte{
		slaveID,
		0x04,
		byte(startAddr >> 8),
		byte(startAddr & 0xFF),
		byte(count >> 8),
		byte(count & 0xFF),
	}

	expectedLength := 5 + 2*count
	response, err := d.sendModbusRequest(request, int(expectedLength))
	if err != nil {
		return nil, err
	}

	byteCount := response[2]
	result := make([]uint16, count)
	for i := uint16(0); i < count; i++ {
		if 2*i+1 < uint16(byteCount) {
			result[i] = uint16(response[3+2*i])<<8 | uint16(response[4+2*i])
		}
	}

	return result, nil
}

// verifyWriteEcho sprawdza echo response dla write operations (FC 05, 06, 0F, 10).
// Slave Modbus odsyła pierwsze 6 bajtów request'a 1:1 jako potwierdzenie.
// N4: poprzednio tylko WriteCoil to robił — WriteRegister/WriteMultiple* milczały o
// niespójności (slave mógł zapisać inną wartość, my byśmy nie wiedzieli).
func verifyWriteEcho(op string, request, response []byte) error {
	for i := 0; i < 6; i++ {
		if response[i] != request[i] {
			return fmt.Errorf("%s: response echo mismatch at byte %d: got 0x%02X, expected 0x%02X (full got % X expected % X)",
				op, i, response[i], request[i], response[:6], request[:6])
		}
	}
	return nil
}

// WriteCoil writes a single coil to a Modbus slave
func (d *ModbusDevice) WriteCoil(slaveID byte, coilAddr uint16, value bool) error {
	request := []byte{
		slaveID,
		0x05,
		byte(coilAddr >> 8),
		byte(coilAddr & 0xFF),
		0x00,
		0x00,
	}
	if value {
		request[4] = 0xFF
	}

	response, err := d.sendModbusRequest(request, 8)
	if err != nil {
		return fmt.Errorf("failed to write coil: %v", err)
	}
	return verifyWriteEcho("write_coil", request, response)
}

// WriteRegister writes a single holding register to a Modbus slave.
// N4: weryfikuje echo (poprzednio brak — slave mógł zapisać inną wartość bez ostrzeżenia).
func (d *ModbusDevice) WriteRegister(slaveID byte, regAddr uint16, value uint16) error {
	request := []byte{
		slaveID,
		0x06,
		byte(regAddr >> 8),
		byte(regAddr & 0xFF),
		byte(value >> 8),
		byte(value & 0xFF),
	}

	response, err := d.sendModbusRequest(request, 8)
	if err != nil {
		return fmt.Errorf("failed to write register: %v", err)
	}
	return verifyWriteEcho("write_register", request, response)
}

// WriteMultipleCoils writes multiple coils to a Modbus slave
func (d *ModbusDevice) WriteMultipleCoils(slaveID byte, startAddr uint16, values []bool) error {
	byteCount := (len(values) + 7) / 8
	request := make([]byte, 7+byteCount)
	request[0] = slaveID
	request[1] = 0x0F
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

	// N4: response = [slave, FC, addr_hi, addr_lo, qty_hi, qty_lo, CRC, CRC] = 8 bytes.
	// Pierwsze 6 bajtów = echo żądania (bez byteCount + data + CRC).
	response, err := d.sendModbusRequest(request, 8)
	if err != nil {
		return fmt.Errorf("failed to write multiple coils: %v", err)
	}
	return verifyWriteEcho("write_multiple_coils", request, response)
}

// WriteMultipleRegisters writes multiple holding registers to a Modbus slave.
// N4: weryfikuje echo response (poprzednio brak — slave mógł napisać inną liczbę
// rejestrów / na innym adresie a my byśmy tego nie wiedzieli).
func (d *ModbusDevice) WriteMultipleRegisters(slaveID byte, startAddr uint16, values []uint16) error {
	request := make([]byte, 7+2*len(values))
	request[0] = slaveID
	request[1] = 0x10
	request[2] = byte(startAddr >> 8)
	request[3] = byte(startAddr & 0xFF)
	request[4] = byte(len(values) >> 8)
	request[5] = byte(len(values) & 0xFF)
	request[6] = byte(2 * len(values))

	for i, value := range values {
		request[7+2*i] = byte(value >> 8)
		request[8+2*i] = byte(value & 0xFF)
	}

	response, err := d.sendModbusRequest(request, 8)
	if err != nil {
		return fmt.Errorf("failed to write multiple registers: %v", err)
	}
	return verifyWriteEcho("write_multiple_registers", request, response)
}

func main() {
	// Parse command line arguments
	port := flag.String("port", "/dev/ttyUSB0", "Serial port")
	baudRate := flag.Int("baud", 9600, "Baud rate")
	dePin := flag.Int("de", 17, "DE pin number")
	rePin := flag.Int("re", 27, "RE pin number")
	command := flag.String("cmd", "", "Command to execute")
	slaveID := flag.Int("slave", 1, "Slave ID")
	startAddr := flag.Int("addr", 0, "Starting address")
	count := flag.Int("count", 1, "Count")
	value := flag.Int("value", 0, "Value to write")
	flag.Parse()

	// Create Modbus device
	device, err := NewModbusDevice(*port, *baudRate, *dePin, *rePin)
	if err != nil {
		log.Fatalf("Failed to create Modbus device: %v", err)
	}
	defer device.Close()

	// Execute command
	switch *command {
	case "read_coils":
		values, err := device.ReadCoils(byte(*slaveID), uint16(*startAddr), uint16(*count))
		if err != nil {
			log.Fatalf("Failed to read coils: %v", err)
		}
		for i, v := range values {
			fmt.Printf("Coil[%d] = %v\n", i, v)
		}

	case "read_discrete":
		values, err := device.ReadDiscreteInputs(byte(*slaveID), uint16(*startAddr), uint16(*count))
		if err != nil {
			log.Fatalf("Failed to read discrete inputs: %v", err)
		}
		for i, v := range values {
			fmt.Printf("Input[%d] = %v\n", i, v)
		}

	case "read_holdreg":
		values, err := device.ReadHoldingRegisters(byte(*slaveID), uint16(*startAddr), uint16(*count))
		if err != nil {
			log.Fatalf("Failed to read holding registers: %v", err)
		}
		for i, v := range values {
			fmt.Printf("Reg[%d] = %d\n", i, v)
		}

	case "read_inputreg":
		values, err := device.ReadInputRegisters(byte(*slaveID), uint16(*startAddr), uint16(*count))
		if err != nil {
			log.Fatalf("Failed to read input registers: %v", err)
		}
		for i, v := range values {
			fmt.Printf("Reg[%d] = %d\n", i, v)
		}

	case "write_coil":
		err := device.WriteCoil(byte(*slaveID), uint16(*startAddr), *value != 0)
		if err != nil {
			log.Fatalf("Failed to write coil: %v", err)
		}

	case "write_register":
		err := device.WriteRegister(byte(*slaveID), uint16(*startAddr), uint16(*value))
		if err != nil {
			log.Fatalf("Failed to write register: %v", err)
		}

	default:
		fmt.Println("Usage:")
		fmt.Println("  read_coils   - Read coils")
		fmt.Println("  read_discrete - Read discrete inputs")
		fmt.Println("  read_holdreg  - Read holding registers")
		fmt.Println("  read_inputreg - Read input registers")
		fmt.Println("  write_coil    - Write single coil")
		fmt.Println("  write_register - Write single register")
		fmt.Println("\nRequired flags:")
		fmt.Println("  -port <port>     - Serial port (default: /dev/ttyUSB0)")
		fmt.Println("  -baud <rate>     - Baud rate (default: 9600)")
		fmt.Println("  -de <pin>        - DE pin number (default: 17)")
		fmt.Println("  -re <pin>        - RE pin number (default: 27)")
		fmt.Println("  -slave <id>      - Slave ID (default: 1)")
		fmt.Println("  -addr <addr>     - Starting address (default: 0)")
		fmt.Println("  -count <count>   - Count (default: 1)")
		fmt.Println("  -value <value>   - Value to write (default: 0)")
	}
}