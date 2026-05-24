package main

// F5.3 / A17 — CLI wydzielone z shared library do osobnego pakietu.
// Buildmode c-shared dla głównej libki nie inkluduje już tego kodu.
//
// Uwaga: ten pakiet używa typów z modbus core lib (../../). W Go workspace
// to wymaga że ten plik jest w innym pakiecie main niż ../../*.go ALE
// musi importować je jako library. To wymagałoby `modbus core` jako
// importowalny pakiet (nie main). Tu pójściemy prostszą drogą:
// CLI duplikuje minimalny core potrzebny do CLI use case (Open + 6 commands)
// żeby uniknąć restrukturyzacji całego modułu pod osobny pakiet "modbus".
//
// Alternatywa (do v5.0): wydzielić core do `internal/modbus` pakietu,
// shared lib i CLI oba używają jako importu. Tu zostawiamy CLI minimal.

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/stianeikeland/go-rpio/v4"
	"go.bug.st/serial"
)

func main() {
	port := flag.String("port", "/dev/ttyUSB0", "Serial port")
	baudRate := flag.Int("baud", 9600, "Baud rate")
	dePin := flag.Int("de", 17, "DE pin number")
	rePin := flag.Int("re", 27, "RE pin number")
	transceiverType := flag.String("transceiver", "isl43485", "isl43485 | max485 | auto")
	command := flag.String("cmd", "", "Command: read_coils | read_discrete | read_holdreg | read_inputreg | write_coil | write_register")
	slaveID := flag.Int("slave", 1, "Slave ID")
	startAddr := flag.Int("addr", 0, "Starting address")
	count := flag.Int("count", 1, "Count")
	value := flag.Int("value", 0, "Value to write")
	flag.Parse()

	if *command == "" {
		printUsage()
		os.Exit(2)
	}

	// Minimal direct serial + rpio (bez core library — patrz uwaga na górze).
	mode := &serial.Mode{BaudRate: *baudRate, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit}
	sp, err := serial.Open(*port, mode)
	if err != nil {
		log.Fatalf("open serial: %v", err)
	}
	defer sp.Close()
	_ = sp.SetReadTimeout(50 * time.Millisecond)

	if *transceiverType != "auto" {
		if err := rpio.Open(); err != nil {
			log.Fatalf("rpio.Open: %v", err)
		}
		defer rpio.Close()
	}

	cli := &cliRunner{
		port:            sp,
		transceiverType: *transceiverType,
		dePin:           *dePin,
		rePin:           *rePin,
		baudRate:        *baudRate,
	}
	cli.setupTransceiver()
	defer cli.idle()

	switch *command {
	case "read_coils":
		out, err := cli.readBits(0x01, byte(*slaveID), uint16(*startAddr), uint16(*count))
		check(err)
		for i, v := range out {
			fmt.Printf("Coil[%d] = %v\n", i, v)
		}
	case "read_discrete":
		out, err := cli.readBits(0x02, byte(*slaveID), uint16(*startAddr), uint16(*count))
		check(err)
		for i, v := range out {
			fmt.Printf("Input[%d] = %v\n", i, v)
		}
	case "read_holdreg":
		out, err := cli.readRegs(0x03, byte(*slaveID), uint16(*startAddr), uint16(*count))
		check(err)
		for i, v := range out {
			fmt.Printf("Reg[%d] = %d\n", i, v)
		}
	case "read_inputreg":
		out, err := cli.readRegs(0x04, byte(*slaveID), uint16(*startAddr), uint16(*count))
		check(err)
		for i, v := range out {
			fmt.Printf("Reg[%d] = %d\n", i, v)
		}
	case "write_coil":
		v := byte(0x00)
		if *value != 0 {
			v = 0xFF
		}
		req := []byte{byte(*slaveID), 0x05, byte(*startAddr >> 8), byte(*startAddr & 0xFF), v, 0x00}
		_, err := cli.exchange(req, 8)
		check(err)
		fmt.Println("OK")
	case "write_register":
		req := []byte{byte(*slaveID), 0x06, byte(*startAddr >> 8), byte(*startAddr & 0xFF),
			byte(*value >> 8), byte(*value & 0xFF)}
		_, err := cli.exchange(req, 8)
		check(err)
		fmt.Println("OK")
	default:
		printUsage()
		os.Exit(2)
	}
}

func check(err error) {
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

func printUsage() {
	fmt.Println("modbus-cli — diagnostyczny CLI dla max485-raspberry-nodejs.")
	fmt.Println("Użycie: modbus-cli -cmd <COMMAND> [-port /dev/serial0] [-baud 9600] [-de 17] [-re 27]")
	fmt.Println("        [-transceiver isl43485|max485|auto] [-slave 1] [-addr 0] [-count 1] [-value 0]")
	fmt.Println()
	fmt.Println("COMMAND: read_coils | read_discrete | read_holdreg | read_inputreg | write_coil | write_register")
}

// ---------- minimal protocol implementation (no retry, no stats, no lockfile) ----------
//
// Świadomie minimalna: CLI jest do diagnostyki one-shot, nie produkcji.
// Production code idzie przez Node addon → shared lib (pełen feature set).

type cliRunner struct {
	port            serial.Port
	transceiverType string
	dePin, rePin    int
	baudRate        int
}

func (c *cliRunner) setupTransceiver() {
	switch c.transceiverType {
	case "isl43485":
		de := rpio.Pin(c.dePin)
		re := rpio.Pin(c.rePin)
		de.Output()
		re.Output()
		de.Low()
		re.Low()
	case "max485":
		de := rpio.Pin(c.dePin)
		de.Output()
		de.Low()
	}
}

func (c *cliRunner) idle() {
	c.setupTransceiver() // ustaw RX/idle ponownie przed exit
}

func (c *cliRunner) enableTX() {
	switch c.transceiverType {
	case "isl43485":
		rpio.Pin(c.dePin).High()
		time.Sleep(time.Microsecond)
		rpio.Pin(c.rePin).High()
		time.Sleep(time.Microsecond)
	case "max485":
		rpio.Pin(c.dePin).High()
		time.Sleep(time.Microsecond)
	}
}

func (c *cliRunner) enableRX() {
	switch c.transceiverType {
	case "isl43485":
		rpio.Pin(c.rePin).Low()
		time.Sleep(time.Microsecond)
		rpio.Pin(c.dePin).Low()
		time.Sleep(time.Microsecond)
	case "max485":
		rpio.Pin(c.dePin).Low()
		time.Sleep(time.Microsecond)
	}
}

func crc16(data []byte) uint16 {
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

func (c *cliRunner) exchange(req []byte, expectedLen int) ([]byte, error) {
	frame := append([]byte{}, req...)
	cr := crc16(req)
	frame = append(frame, byte(cr&0xFF), byte(cr>>8))

	_ = c.port.ResetInputBuffer()
	c.enableTX()
	if _, err := c.port.Write(frame); err != nil {
		c.enableRX()
		return nil, err
	}
	_ = c.port.Drain()
	c.enableRX()

	ifgUs := 38500000 / c.baudRate
	time.Sleep(time.Duration(ifgUs) * time.Microsecond)

	buf := make([]byte, expectedLen)
	total := 0
	deadline := time.Now().Add(300 * time.Millisecond)
	for total < expectedLen && time.Now().Before(deadline) {
		n, err := c.port.Read(buf[total:])
		if err != nil {
			return nil, err
		}
		if n > 0 {
			total += n
		}
	}
	if total < expectedLen {
		return nil, fmt.Errorf("timeout: %d/%d", total, expectedLen)
	}
	got := binary16(buf[expectedLen-2:])
	calc := crc16(buf[:expectedLen-2])
	if got != calc {
		return nil, fmt.Errorf("CRC error: got %04X calc %04X", got, calc)
	}
	return buf, nil
}

func binary16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func (c *cliRunner) readBits(fc, slave byte, addr, n uint16) ([]bool, error) {
	req := []byte{slave, fc, byte(addr >> 8), byte(addr & 0xFF), byte(n >> 8), byte(n & 0xFF)}
	expected := 3 + int((n+7)/8) + 2
	resp, err := c.exchange(req, expected)
	if err != nil {
		return nil, err
	}
	bc := resp[2]
	out := make([]bool, n)
	for i := uint16(0); i < n; i++ {
		bi := i / 8
		if bi < uint16(bc) {
			out[i] = (resp[3+bi] & (1 << (i % 8))) != 0
		}
	}
	return out, nil
}

func (c *cliRunner) readRegs(fc, slave byte, addr, n uint16) ([]uint16, error) {
	req := []byte{slave, fc, byte(addr >> 8), byte(addr & 0xFF), byte(n >> 8), byte(n & 0xFF)}
	expected := 3 + 2*int(n) + 2
	resp, err := c.exchange(req, expected)
	if err != nil {
		return nil, err
	}
	out := make([]uint16, n)
	for i := uint16(0); i < n; i++ {
		out[i] = uint16(resp[3+2*i])<<8 | uint16(resp[4+2*i])
	}
	return out, nil
}
