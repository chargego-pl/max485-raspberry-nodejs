// v4.0.0 smoke test — używa nowego high-level API z index.js (factory pattern,
// natywne arrays, structured errors). Surowy native module nie jest już używany.

const ModbusRTU = require('./');

async function main() {
    const device = await ModbusRTU.open({
        port: '/dev/serial0',
        baudRate: 9600,
        transceiver: 'isl43485',
        dePin: 17,
        rePin: 27,
    });

    // Opcjonalne: włącz retry dla transient errors + debug
    device.setRetryConfig({ maxRetries: 2, backoffMs: 50 });
    if (process.env.MAX485_DEBUG) {
        device.setDebug(parseInt(process.env.MAX485_DEBUG, 10) || 1);
    }

    try {
        console.log('--- COILS ---');
        console.log('read coils 0..3:', await device.readCoils(21, 0, 4));

        console.log('\nwrite coil 0 = true');
        await device.writeCoil(21, 0, true);
        console.log('after write:', await device.readCoils(21, 0, 4));

        console.log('\nwrite multiple coils 0..3 = [true, false, true, false]');
        await device.writeMultipleCoils(21, 0, [true, false, true, false]);
        console.log('after multi write:', await device.readCoils(21, 0, 4));

        console.log('\n--- REGISTERS ---');
        console.log('read holding 0..3:', await device.readHoldingRegisters(21, 0, 4));

        console.log('\nwrite register 0 = 123');
        await device.writeRegister(21, 0, 123);
        console.log('after write:', await device.readHoldingRegisters(21, 0, 4));

        console.log('\nwrite multiple registers 0..3 = [50, 100, 150, 200]');
        await device.writeMultipleRegisters(21, 0, [50, 100, 150, 200]);
        console.log('after multi write:', await device.readHoldingRegisters(21, 0, 4));

        console.log('\n--- STATS ---');
        const s = device.stats();
        console.log('opsTotal:', s.opsTotal.toString());
        console.log('opsByResult:', Object.fromEntries(
            Object.entries(s.opsByResult).map(([k, v]) => [k, v.toString()])
        ));
        console.log('opsBySlave 21:', Object.fromEntries(
            Object.entries(s.opsBySlave['21'] || {}).map(([k, v]) => [k, v.toString()])
        ));
    } catch (e) {
        if (e.code === 'MODBUS_EXCEPTION') {
            console.error('ModbusException:', {
                slaveID: e.slaveID, functionCode: e.functionCode, exceptionCode: e.exceptionCode
            });
        } else {
            console.error('Error:', e.message);
        }
        process.exitCode = 1;
    } finally {
        await device.close();
        console.log('\nclosed');
    }
}

main().catch(e => { console.error(e); process.exit(1); });
