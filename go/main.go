package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/stianeikeland/go-rpio/v4"
	"github.com/tarm/serial"
)

// Timing configuration
const (
	// GPIO pin switching delays
	gpioSwitchDelay = 1 * time.Microsecond

	// Serial communication delays
	preSendDelay     = 0 * time.Millisecond
	byteSendDelay    = 1 * time.Millisecond
	postSendDelay    = 3 * time.Millisecond
	preReceiveDelay  = 1 * time.Millisecond
	receiveReadDelay = 1 * time.Millisecond
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

// NewModbusDevice creates a new Modbus device
func NewModbusDevice(portName string, baudRate int, dePin, rePin int) (*ModbusDevice, error) {
	// Configure serial port
	config := &serial.Config{
		Name:        portName,
		Baud:        baudRate,
		ReadTimeout: time.Second * 5,
		Size:        8,
		Parity:      serial.ParityNone,
		StopBits:    serial.Stop1,
	}

	port, err := serial.OpenPort(config)
	if err != nil {
		return nil, fmt.Errorf("failed to open serial port: %v", err)
	}

	// Set additional port parameters
	if err := port.Flush(); err != nil {
		port.Close()
		return nil, fmt.Errorf("failed to flush port: %v", err)
	}

	// Initialize GPIO
	if err := rpio.Open(); err != nil {
		port.Close()
		return nil, fmt.Errorf("failed to initialize GPIO: %v", err)
	}

	de := rpio.Pin(dePin)
	re := rpio.Pin(rePin)

	de.Output()
	re.Output()

	// Set initial state to receive mode
	de.Low()
	re.Low()

	return &ModbusDevice{
		port:   port,
		dePin:  de,
		rePin:  re,
	}, nil
}

// Close closes the Modbus device
func (d *ModbusDevice) Close() {
	if d.port != nil {
		d.port.Close()
	}
	rpio.Close()
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

// enableTX enables RS485 transmit mode
func (d *ModbusDevice) enableTX() {
	// For ISL43485IBZ:
	// DE must be HIGH to enable transmission
	// RE must be HIGH to disable reception
	d.rePin.High()
	time.Sleep(gpioSwitchDelay)
	d.dePin.High()
	time.Sleep(gpioSwitchDelay)
}

// enableRX enables RS485 receive mode
func (d *ModbusDevice) enableRX() {
	// For ISL43485IBZ:
	// DE must be LOW to disable transmission
	// RE must be LOW to enable reception
	d.dePin.Low()
	time.Sleep(gpioSwitchDelay)
	d.rePin.Low()
	time.Sleep(gpioSwitchDelay)
}

// sendModbusRequest sends a Modbus request and waits for response
func (d *ModbusDevice) sendModbusRequest(request []byte, expectedLength int) ([]byte, error) {
	// Wyczyść zalegające bajty w RX/TX bufferze przed nowym cyklem.
	// Bez tego ostatni bajt response z poprzedniego cyklu (który nadszedł
	// po timeout=5s) miesza się z nową response → "invalid slave ID:
	// got X, expected Y" lub "invalid response length".
	// tarm/serial.Flush() wywołuje tcflush(TCIOFLUSH) — oba kierunki.
	if err := d.port.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush port before send: %v", err)
	}

	// Add CRC to request
	crc := calculateCRC(request)
	request = append(request, byte(crc&0xFF), byte(crc>>8))

	// Send request
	d.enableTX()
	time.Sleep(preSendDelay)

	// Send data byte by byte
	for i, b := range request {
		n, err := d.port.Write([]byte{b})
		if err != nil {
			return nil, fmt.Errorf("failed to write byte %d: %v", i, err)
		}
		if n != 1 {
			return nil, fmt.Errorf("failed to write byte %d: wrote %d bytes", i, n)
		}
		time.Sleep(byteSendDelay)
	}

	// Wait for transmission to complete
	time.Sleep(postSendDelay)

	// Wait for response
	d.enableRX()
	
	// Add a small delay to ensure the device has time to respond
	time.Sleep(preReceiveDelay)
	
	// Read response with timeout
	response := make([]byte, expectedLength)
	totalRead := 0
	
	// Try to read all expected bytes
	for totalRead < expectedLength {
		n, err := d.port.Read(response[totalRead:])
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		if n == 0 {
			break
		}
		totalRead += n
		time.Sleep(receiveReadDelay)
	}
	
	if totalRead < expectedLength {
		return nil, fmt.Errorf("invalid response length: got %d, expected %d", totalRead, expectedLength)
	}

	// Verify slave ID
	if response[0] != request[0] {
		return nil, fmt.Errorf("invalid slave ID in response: got %d, expected %d", response[0], request[0])
	}

	// Verify function code
	if response[1] != request[1] {
		return nil, fmt.Errorf("invalid function code in response: got %d, expected %d", response[1], request[1])
	}

	// Verify CRC
	receivedCRC := binary.LittleEndian.Uint16(response[totalRead-2:])
	calculatedCRC := calculateCRC(response[:totalRead-2])
	if receivedCRC != calculatedCRC {
		return nil, fmt.Errorf("CRC error: received %04X, calculated %04X", receivedCRC, calculatedCRC)
	}

	return response, nil
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

	// Verify response matches request
	if response[0] != request[0] || response[1] != request[1] || 
	   response[2] != request[2] || response[3] != request[3] ||
	   response[4] != request[4] || response[5] != request[5] {
		return fmt.Errorf("response does not match request: got % X, expected % X", response, request)
	}

	return nil
}

// WriteRegister writes a single holding register to a Modbus slave
func (d *ModbusDevice) WriteRegister(slaveID byte, regAddr uint16, value uint16) error {
	request := []byte{
		slaveID,
		0x06,
		byte(regAddr >> 8),
		byte(regAddr & 0xFF),
		byte(value >> 8),
		byte(value & 0xFF),
	}

	_, err := d.sendModbusRequest(request, 8)
	return err
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

	_, err := d.sendModbusRequest(request, 8)
	return err
}

// WriteMultipleRegisters writes multiple holding registers to a Modbus slave
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

	_, err := d.sendModbusRequest(request, 8)
	return err
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