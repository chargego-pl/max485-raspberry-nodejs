const { NewModbusDevice, ReadCoils, ReadDiscreteInputs, ReadHoldingRegisters, ReadInputRegisters, WriteCoil, WriteRegister, WriteMultipleCoils, WriteMultipleRegisters, Close } = require('./build/Release/modbus');

// v4.0.0 BREAKING:
// - Błędy operacji bus są teraz natywnymi JS exceptions (napi_throw_error po
//   stronie Go) — `await fn()` ODRZUCA promise jak każda async funkcja.
//   Wcześniej (v3.x) Go zwracał string "Error: ..." a JS robił
//   `result.startsWith('Error:') && throw` — fragile, brak typed errors,
//   pomyłka konwencji = błąd cicho ignorowany.
// - Migracja do go.bug.st/serial.v1 (zamiast tarm/serial) — pozwala
//   na natywne Drain() (eliminacja blind postSendDelay) i SetReadTimeout()
//   (eliminacja 5s blocking workaround).
// - Consumer migration: wszystkie wywołania metod ModbusRTU muszą być w
//   try/catch (lub `.catch()`) — inaczej unhandled rejection zabije proces
//   przy każdym errorze busa (zerwany kabel, slave offline, CRC mismatch,
//   ModbusException itd.).
class ModbusRTU {
    constructor(port, baudRate, dePin, rePin) {
        // NewModbusDevice throws JS exception on failure (M6/M7) — konstruktor
        // propaguje. Brak tu `if (!this.device)` bo throw jest natywny.
        this.device = NewModbusDevice(port, baudRate, dePin, rePin);
    }

    async readCoils(slaveID, startAddr, count) {
        const result = await ReadCoils(this.device, slaveID, startAddr, count);
        return JSON.parse(result);
    }

    async readDiscreteInputs(slaveID, startAddr, count) {
        const result = await ReadDiscreteInputs(this.device, slaveID, startAddr, count);
        return JSON.parse(result);
    }

    async readHoldingRegisters(slaveID, startAddr, count) {
        const result = await ReadHoldingRegisters(this.device, slaveID, startAddr, count);
        return JSON.parse(result);
    }

    async readInputRegisters(slaveID, startAddr, count) {
        const result = await ReadInputRegisters(this.device, slaveID, startAddr, count);
        return JSON.parse(result);
    }

    async writeCoil(slaveID, coilAddr, value) {
        return await WriteCoil(this.device, slaveID, coilAddr, value);
    }

    async writeRegister(slaveID, regAddr, value) {
        return await WriteRegister(this.device, slaveID, regAddr, value);
    }

    async writeMultipleCoils(slaveID, startAddr, values) {
        return await WriteMultipleCoils(this.device, slaveID, startAddr, values);
    }

    async writeMultipleRegisters(slaveID, startAddr, values) {
        return await WriteMultipleRegisters(this.device, slaveID, startAddr, values);
    }

    close() {
        Close(this.device);
    }
}

module.exports = ModbusRTU;
