// v4.0.0 mocha integration test — wymaga fizycznego urządzenia Modbus
// na busie (np. wiata RPi z Pico firmware'm).
//
// Uruchomienie: `mocha test/test.js` (mocha musi być zainstalowane).

const ModbusRTU = require('../index.js');
const assert = require('assert');

describe('ModbusDevice (v4.0.0 integration)', function () {
    this.timeout(10000);

    let device;

    before(async () => {
        device = await ModbusRTU.open({
            port: '/dev/serial0',
            baudRate: 9600,
            transceiver: 'isl43485',
            dePin: 17,
            rePin: 27,
        });
    });

    after(async () => {
        if (device) {
            await device.close();
        }
    });

    it('powinno odczytać cewki', async () => {
        const coils = await device.readCoils(21, 0, 10);
        assert(Array.isArray(coils), 'readCoils powinno zwrócić tablicę');
        assert.strictEqual(coils.length, 10);
        coils.forEach(coil => {
            assert.strictEqual(typeof coil, 'boolean');
        });
    });

    it('powinno odczytać rejestry', async () => {
        const registers = await device.readHoldingRegisters(21, 0, 5);
        assert(Array.isArray(registers));
        assert.strictEqual(registers.length, 5);
        registers.forEach(register => {
            assert.strictEqual(typeof register, 'number');
            assert(register >= 0 && register <= 65535);
        });
    });

    it('powinno zapisać i odczytać cewkę', async () => {
        const testValue = true;
        await device.writeCoil(21, 0, testValue);
        const [coil] = await device.readCoils(21, 0, 1);
        assert.strictEqual(coil, testValue);
    });

    it('powinno zapisać i odczytać rejestr', async () => {
        const testValue = 12345;
        await device.writeRegister(21, 0, testValue);
        const [register] = await device.readHoldingRegisters(21, 0, 1);
        assert.strictEqual(register, testValue);
    });

    it('stats() raportuje wzrost opsTotal po operacjach', async () => {
        const before = device.stats().opsTotal;
        await device.readCoils(21, 0, 1);
        const after = device.stats().opsTotal;
        assert(after > before, `expected opsTotal > ${before}, got ${after}`);
    });
});
