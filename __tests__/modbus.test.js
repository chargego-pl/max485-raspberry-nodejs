// v4.0.0 — stary test używał ffi-napi (legacy implementation z prebuilt .so).
// Teraz mamy node-gyp + napi addon + factory pattern. Test mockujemy native
// module — sprawdzamy że index.js (high-level wrapper) prawidłowo wywołuje
// natywne funkcje.

jest.mock('../build/Release/modbus', () => ({
    NewModbusDevice: jest.fn(),
    ReadCoils: jest.fn(),
    ReadDiscreteInputs: jest.fn(),
    ReadHoldingRegisters: jest.fn(),
    ReadInputRegisters: jest.fn(),
    WriteCoil: jest.fn(),
    WriteRegister: jest.fn(),
    WriteMultipleCoils: jest.fn(),
    WriteMultipleRegisters: jest.fn(),
    Close: jest.fn(),
    SetDebug: jest.fn(),
    SetRetryConfig: jest.fn(),
    Stats: jest.fn(),
}), { virtual: true });

const native = require('../build/Release/modbus');
const ModbusRTU = require('../index.js');

const fakeHandle = { __external: true };

describe('ModbusRTU v4.0.0 wrapper', () => {
    beforeEach(() => {
        jest.clearAllMocks();
        native.NewModbusDevice.mockResolvedValue(fakeHandle);
        native.Close.mockResolvedValue(undefined);
    });

    test('open() wywołuje native z poprawnymi argumentami i zwraca instance', async () => {
        const device = await ModbusRTU.open({
            port: '/dev/serial0', baudRate: 9600,
            transceiver: 'isl43485', dePin: 17, rePin: 27,
        });
        expect(native.NewModbusDevice).toHaveBeenCalledWith('/dev/serial0', 9600, 'isl43485', 17, 27);
        expect(device).toBeInstanceOf(ModbusRTU);
    });

    test('open() defaults: transceiver=isl43485, dePin=17, rePin=27', async () => {
        await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        expect(native.NewModbusDevice).toHaveBeenCalledWith('/dev/serial0', 9600, 'isl43485', 17, 27);
    });

    test('open() rzuca przy złym transceiver type', async () => {
        await expect(ModbusRTU.open({
            port: '/dev/serial0', baudRate: 9600, transceiver: 'bogus'
        })).rejects.toThrow(/transceiver/);
    });

    test('bezpośredni `new ModbusRTU(null)` rzuca z hint o factory', () => {
        expect(() => new ModbusRTU(null)).toThrow(/ModbusRTU\.open/);
    });

    test('readCoils() forwarduje args do native', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        native.ReadCoils.mockResolvedValue([true, false, true]);
        const out = await device.readCoils(21, 0, 3);
        expect(native.ReadCoils).toHaveBeenCalledWith(fakeHandle, 21, 0, 3);
        expect(out).toEqual([true, false, true]);
    });

    test('writeCoil() forwarduje i normalizuje boolean', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        native.WriteCoil.mockResolvedValue(undefined);
        await device.writeCoil(21, 0, 1); // truthy → true
        expect(native.WriteCoil).toHaveBeenCalledWith(fakeHandle, 21, 0, true);
    });

    test('writeMultipleCoils() normalizuje array booleanów', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        native.WriteMultipleCoils.mockResolvedValue(undefined);
        await device.writeMultipleCoils(21, 0, [1, 0, 'yes', '']);
        expect(native.WriteMultipleCoils).toHaveBeenCalledWith(fakeHandle, 21, 0, [true, false, true, false]);
    });

    test('setDebug() forwarduje level', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        device.setDebug(2);
        expect(native.SetDebug).toHaveBeenCalledWith(fakeHandle, 2);
    });

    test('setRetryConfig(null) wyłącza retry', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        device.setRetryConfig(null);
        expect(native.SetRetryConfig).toHaveBeenCalledWith(fakeHandle, 0, 0);
    });

    test('setRetryConfig({maxRetries, backoffMs}) przekazuje obie wartości', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        device.setRetryConfig({ maxRetries: 3, backoffMs: 100 });
        expect(native.SetRetryConfig).toHaveBeenCalledWith(fakeHandle, 3, 100);
    });

    test('close() jest idempotent + setuje _closed', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        await device.close();
        await device.close(); // no-op
        expect(native.Close).toHaveBeenCalledTimes(1);
    });

    test('po close() metody rzucają', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        await device.close();
        expect(() => device.readCoils(21, 0, 1)).toThrow(/closed/);
    });

    test('stats() zwraca raw native object', async () => {
        const device = await ModbusRTU.open({ port: '/dev/serial0', baudRate: 9600 });
        native.Stats.mockReturnValue({ opsTotal: 42n, opsByResult: {} });
        const s = device.stats();
        expect(s.opsTotal).toBe(42n);
    });
});
