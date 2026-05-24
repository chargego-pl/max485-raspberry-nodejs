package main

import (
	"sync"

	"github.com/stianeikeland/go-rpio/v4"
	"github.com/tarm/serial"
)

// ModbusDevice represents a Modbus RTU device.
//
// THREAD SAFETY: methods that touch the bus (Read*, Write*, sendModbusRequest)
// acquire `mu` for the duration of the operation. Multiple goroutines may call
// methods concurrently — they will be serialized on the bus, not interleaved.
type ModbusDevice struct {
	port   *serial.Port
	dePin  rpio.Pin
	rePin  rpio.Pin
	mu     sync.Mutex
	closed bool
}

// ModbusException to typed error reprezentujący odpowiedź slave z FC|0x80.
type ModbusException struct {
	SlaveID      byte
	FunctionCode byte // oryginalny FC (bez bitu 0x80)
	ExceptionCode byte // 0x01 illegal func, 0x02 illegal addr, 0x03 illegal val, 0x04 slave failure, ...
}

func (e *ModbusException) Error() string {
	desc := exceptionDescription(e.ExceptionCode)
	return "modbus exception: slave=" + byteHex(e.SlaveID) + " fc=" + byteHex(e.FunctionCode) +
		" code=" + byteHex(e.ExceptionCode) + " (" + desc + ")"
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
