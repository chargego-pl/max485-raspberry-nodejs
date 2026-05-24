# max485-raspberry-nodejs

Node.js library for Modbus RTU communication over RS485 on Raspberry Pi.

**v4.0.0** — kompletny architectural refactor. Wszystkie operacje są
prawdziwie asynchroniczne (napi_async_work, off main thread), z natywnymi
JS Errors zamiast string sentinels, structured ModbusException, typed arrays,
opcjonalnym retry, observability counters i finalize_cb chroniącym przed
resource leak przy GC bez explicit `close()`.

## Installation

```bash
npm install max485-raspberry-nodejs
```

Wymaga Linux + Go 1.23+ + `make` + `node-gyp` toolchain (`gcc`, `g++`,
`libnode-dev`). Auto-build odpalany przez `npm install`.

## Quick start

```javascript
const ModbusRTU = require('max485-raspberry-nodejs');

async function main() {
    const device = await ModbusRTU.open({
        port:        '/dev/serial0',
        baudRate:    9600,
        transceiver: 'isl43485',  // 'isl43485' | 'max485' | 'auto'
        dePin:       17,           // BCM numbering
        rePin:       27,           // tylko dla isl43485
    });

    // Opcjonalne: włącz automatyczny retry dla transient errors (timeout/CRC).
    // ModbusException (illegal address etc.) NIE jest retry'owany.
    device.setRetryConfig({ maxRetries: 2, backoffMs: 50 });

    try {
        const coils = await device.readCoils(21, 0, 4);
        // -> [true, false, true, false]  (natywny Array<boolean>)

        await device.writeCoil(21, 0, true);
        await device.writeMultipleCoils(21, 0, [true, false, true, false]);

        const regs = await device.readHoldingRegisters(21, 0, 4);
        // -> [42, 100, 0, 65535]         (natywny Array<number>)

        await device.writeRegister(21, 0, 123);
        await device.writeMultipleRegisters(21, 0, [50, 100, 150, 200]);

    } catch (e) {
        if (e.code === 'MODBUS_EXCEPTION') {
            // Structured fields — bez parse'owania message string!
            console.error('Modbus exception:', {
                slaveID:       e.slaveID,        // np. 21
                functionCode:  e.functionCode,   // np. 0x05
                exceptionCode: e.exceptionCode,  // np. 0x02 = illegal data address
            });
        } else {
            console.error('Bus error:', e.message);
        }
    } finally {
        await device.close();
    }
}

main();
```

## Transceiver types

| Type | Description | Pins |
|------|-------------|------|
| `isl43485` | ISL43485IBZ (HAT Power Shield v2). Driver enable order chroni przed SHUTDOWN trap. | `dePin`, `rePin` |
| `max485` | Klasyczny MAX485 (DE i /RE złączone w 1 pin). | `dePin` only |
| `auto` | USB↔RS485 z auto-direction (CH340, FTDI w RS485 mode). | — |

## API

### `static async ModbusRTU.open(opts) → Promise<ModbusRTU>`

Async factory — **JEDYNY** supported sposób tworzenia instancji w v4.x.
`new ModbusRTU(...)` rzuca.

Opts: `{ port, baudRate, transceiver='isl43485', dePin=17, rePin=27 }`.

### Read ops (Promise → natywny Array)

- `readCoils(slaveID, startAddr, count) → Promise<boolean[]>`
- `readDiscreteInputs(slaveID, startAddr, count) → Promise<boolean[]>`
- `readHoldingRegisters(slaveID, startAddr, count) → Promise<number[]>`
- `readInputRegisters(slaveID, startAddr, count) → Promise<number[]>`

### Write ops (Promise<void>)

- `writeCoil(slaveID, coilAddr, value: boolean)`
- `writeRegister(slaveID, regAddr, value: number)`
- `writeMultipleCoils(slaveID, startAddr, values: boolean[])`
- `writeMultipleRegisters(slaveID, startAddr, values: number[])`

### Configuration

- `setDebug(level: 0|1|2)` — per-instance debug. `0`=off, `1`=basic events,
  `2`=basic + hex TX/RX dump. Wcześniej globalny env var `MAX485_DEBUG`;
  teraz consumer sam wybiera per device:
  ```js
  if (process.env.MAX485_DEBUG) device.setDebug(parseInt(process.env.MAX485_DEBUG, 10) || 1);
  ```
- `setRetryConfig({ maxRetries, backoffMs })` — włącz retry dla transient.
  `setRetryConfig(null)` lub `{ maxRetries: 0 }` wyłącza.

### Observability

- `stats() → object` (sync, czysty snapshot atomic counters)

```js
{
  opsTotal: 12345n,                            // BigInt
  opsByResult: {
    success: 12000n, timeout: 12n, crc_error: 3n,
    exception: 5n, io_error: 0n
  },
  opsBySlave: {
    '21': { ops: 5000n, successes: 4990n, timeouts: 5n, ..., sumLatencyMicro: 12340567n },
    '31': { ... }
  },
  lastTxUnixNano: 1748100000000000000n,
  lastRxUnixNano: 1748100000003200000n,
}
```

### Lifecycle

- `async close()` — zwalnia port, GPIO bus-idle, lockfile removed, rpio
  refcount decremented. Idempotent. Jeśli pominiesz, napi finalize_cb przy
  GC i tak posprząta (F1.1 / A4) — ale explicit close = deterministic timing.

## Error model

Wszystkie operacje bus mogą rzucić:

| Error.code | Pola | Kiedy |
|------------|------|-------|
| `MODBUS_EXCEPTION` | `slaveID`, `functionCode`, `exceptionCode` | Slave zwrócił FC \| 0x80 (permanent) |
| _undefined_ | `message` zawiera "modbus timeout", "CRC error", "write:", "drain:" etc. | Transient bus error (retry-able) |

`MODBUS_EXCEPTION` NIE jest retry'owany przez `setRetryConfig` (permanent
application error). Wszystko inne (timeout, CRC, IO) traktujemy jako
transient → retry kicks in jeśli skonfigurowane.

## v3.x → v4.0.0 migration

| v3.x | v4.0.0 |
|------|--------|
| `new ModbusRTU(port, baud, de, re)` | `await ModbusRTU.open({ port, baudRate, transceiver: 'isl43485', dePin, rePin })` |
| `device.close()` (sync) | `await device.close()` |
| `result.startsWith('Error:')` then throw | `try/catch` (natywny throw) |
| `error.message.includes('exception')` (parse) | `error.code === 'MODBUS_EXCEPTION'`, fields `slaveID`/`functionCode`/`exceptionCode` |
| `JSON.parse(await readCoils(...))` | `await readCoils(...)` (natywny array) |
| `await writeCoil(...) === 'success'` | `await writeCoil(...)` (Promise<void>) |
| `MAX485_DEBUG=1` env var | `device.setDebug(1)` |

## License

MIT
