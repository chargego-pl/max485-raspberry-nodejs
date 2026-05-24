package main

// F5.2 / A6 — Transceiver abstrahuje hardware-specific kontrolę direction
// RS485. Pozwala wymienić driver bez zmiany core'a sendModbusRequest.
//
// Trzy implementacje included:
//   - GPIOTransceiverISL43485: HAT Power Shield v2 (wiata). DE/RE jako
//     osobne piny, kolejność enableTX/enableRX dostosowana do ISL truth table
//     (uniknięcie SHUTDOWN trap przez (/RE=1, DE=0)).
//   - GPIOTransceiverMAX485: klasyczny single-pin DE=/RE wired together.
//   - AutoTransceiver: USB↔RS485 z auto-direction logic w chipie
//     (CH340, FTDI z RS485 mode). No-op enable/disable.
//
// Wszystkie metody Transceiver wywoływane są pod bus mutex'em — implementacje
// NIE muszą się synchronizować same.

import (
	"time"

	"github.com/stianeikeland/go-rpio/v4"
)

type Transceiver interface {
	// EnableTX przełącza driver w stan transmit. Po return safe do port.Write().
	EnableTX() error
	// EnableRX przełącza driver w stan receive. Po return safe do port.Read().
	EnableRX() error
	// Close ustawia bus-idle (RX, driver disabled) i zwalnia GPIO refs.
	Close() error
}

// ---------- ISL43485 (HAT v2) ----------

// gpioSwitchDelay — krótkie settle delay między DE i RE writes. ISL43485
// datasheet: 70ns enable propagation, więc 1µs jest bezpieczne z marginesem.
const gpioSwitchDelay = 1 * time.Microsecond

type GPIOTransceiverISL43485 struct {
	dePin rpio.Pin
	rePin rpio.Pin
}

// NewGPIOTransceiverISL43485 inicjalizuje DE/RE piny w bus-idle state
// (RX only). Zakłada że rpio.Open() został już zacquired (przez acquireRpio()).
func NewGPIOTransceiverISL43485(dePin, rePin int) *GPIOTransceiverISL43485 {
	de := rpio.Pin(dePin)
	re := rpio.Pin(rePin)
	de.Output()
	re.Output()
	// Bus-idle: (DE=0, /RE=0) = receive only. Bezpieczny startup.
	de.Low()
	re.Low()
	return &GPIOTransceiverISL43485{dePin: de, rePin: re}
}

// ISL43485IBZ truth table (datasheet Renesas, Table 1):
//
//	/RE  DE | mode               RO        A/B driven
//	────────┼──────────────────────────────────────────
//	 0    0 | receive only       data      no (high-Z)
//	 0    1 | transmit + echo    data      yes (driver active)
//	 1    0 | SHUTDOWN (≥300 ns) high-Z    no
//	 1    1 | transmit only      high-Z    yes
//
// STABILITY: kolejność tak, by NIGDY nie wpaść w (/RE=1, DE=0).
// Z idle (DE=0, /RE=0) → najpierw DE=1 (przejście przez "TX+echo"),
// potem /RE=1 (TX only). Receiver echo harmless bo input buffer jest
// resetowany przez sendModbusRequest przed TX, a nie czytamy w trakcie TX.
func (t *GPIOTransceiverISL43485) EnableTX() error {
	t.dePin.High()
	time.Sleep(gpioSwitchDelay)
	t.rePin.High()
	time.Sleep(gpioSwitchDelay)
	return nil
}

// Inwersja enableTX: najpierw /RE=0 (przez TX+RX echo), potem DE=0 (RX only).
// Także nigdy nie wpadamy w SHUTDOWN.
func (t *GPIOTransceiverISL43485) EnableRX() error {
	t.rePin.Low()
	time.Sleep(gpioSwitchDelay)
	t.dePin.Low()
	time.Sleep(gpioSwitchDelay)
	return nil
}

// Close ustawia bus-idle. Wcześniej (przed v3.0.3) brak tego = po SIGKILL
// DE pozostawał HIGH → ISL napędzał bus → blokada dla innych masterów.
func (t *GPIOTransceiverISL43485) Close() error {
	t.rePin.Low()
	t.dePin.Low()
	return nil
}

// ---------- MAX485 (classic single-pin) ----------

// MAX485 ma DE i /RE związane razem (1 pin sterujący). DE=1 → TX; DE=0 → RX.
// Brak SHUTDOWN trap — niemożliwy stan (RE=1, DE=0) bo to ten sam pin.
type GPIOTransceiverMAX485 struct {
	dePin rpio.Pin
}

func NewGPIOTransceiverMAX485(dePin int) *GPIOTransceiverMAX485 {
	de := rpio.Pin(dePin)
	de.Output()
	de.Low()
	return &GPIOTransceiverMAX485{dePin: de}
}

func (t *GPIOTransceiverMAX485) EnableTX() error {
	t.dePin.High()
	time.Sleep(gpioSwitchDelay)
	return nil
}

func (t *GPIOTransceiverMAX485) EnableRX() error {
	t.dePin.Low()
	time.Sleep(gpioSwitchDelay)
	return nil
}

func (t *GPIOTransceiverMAX485) Close() error {
	t.dePin.Low()
	return nil
}

// ---------- Auto (USB↔RS485 z hardware auto-direction) ----------

// CH340G w trybie RS485 lub FTDI FT232RL z auto-direction logic same
// przełączają DE na podstawie aktywności TX line. No GPIO.
type AutoTransceiver struct{}

func NewAutoTransceiver() *AutoTransceiver { return &AutoTransceiver{} }

func (t *AutoTransceiver) EnableTX() error { return nil }
func (t *AutoTransceiver) EnableRX() error { return nil }
func (t *AutoTransceiver) Close() error    { return nil }
