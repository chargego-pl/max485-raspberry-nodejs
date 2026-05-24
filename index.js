'use strict';

const native = require('./build/Release/modbus');

// v4.0.0 — kompletny architectural refactor. Patrz docs/products/wiata-shed/
// hardware/max485-library.md sekcja "Changelog v4.0.0" dla pełnej listy
// breaking changes (A1–A21).
//
// BREAKING vs v3.x:
//   1. Konstruktor → factory async:    `new ModbusRTU(...)` ❌ → `await ModbusRTU.open({...})` ✓
//   2. Wszystkie metody bus zwracają natywne Promise (off-main-thread I/O).
//   3. Błędy = natywne JS Error throws (await odrzuca). ModbusException ma
//      structured properties: e.code === 'MODBUS_EXCEPTION', e.slaveID,
//      e.functionCode, e.exceptionCode.
//   4. Read* zwracają natywne arrays (boolean / number) zamiast JSON string.
//   5. Write* zwracają Promise<void> (undefined) zamiast "success" string.
//   6. Konstruktor wymaga transceiver type (isl43485 | max485 | auto) zamiast
//      hardcode'owanej obsługi tylko ISL43485.
//   7. close() jest async (`await device.close()`).
//   8. Debug per-instance: `device.setDebug(level)` zamiast env var
//      MAX485_DEBUG (consumer sam czyta env i wywołuje setter).
//   9. Opcjonalny retry: `device.setRetryConfig({ maxRetries: 2, backoffMs: 50 })`.
//  10. Telemetry: `await device.stats()` zwraca obiekt z counters per slave +
//      per result kind. BigInt fields (opsTotal, ops, etc.).

const VALID_TRANSCEIVERS = new Set(['isl43485', 'max485', 'auto']);

class ModbusRTU {
    /**
     * Async factory — JEDYNY supported sposób stworzenia instancji w v4.0.0.
     *
     * @param {object} opts
     * @param {string} opts.port            — np. '/dev/serial0'
     * @param {number} opts.baudRate        — np. 9600
     * @param {('isl43485'|'max485'|'auto')} [opts.transceiver='isl43485']
     * @param {number} [opts.dePin=17]      — używane dla isl43485, max485
     * @param {number} [opts.rePin=27]      — używane tylko dla isl43485
     * @returns {Promise<ModbusRTU>}
     */
    static async open(opts) {
        if (!opts || typeof opts !== 'object') {
            throw new TypeError('ModbusRTU.open(opts): opts object required');
        }
        const { port, baudRate } = opts;
        const transceiver = opts.transceiver || 'isl43485';
        const dePin = opts.dePin != null ? opts.dePin : 17;
        const rePin = opts.rePin != null ? opts.rePin : 27;

        if (typeof port !== 'string' || !port) throw new TypeError('opts.port: string required');
        if (typeof baudRate !== 'number' || baudRate <= 0) throw new TypeError('opts.baudRate: positive number required');
        if (!VALID_TRANSCEIVERS.has(transceiver)) {
            throw new TypeError(`opts.transceiver: must be one of ${[...VALID_TRANSCEIVERS].join('|')}`);
        }

        const handle = await native.NewModbusDevice(port, baudRate, transceiver, dePin, rePin);
        return new ModbusRTU(handle);
    }

    /**
     * @deprecated v4.0.0 — używaj `await ModbusRTU.open(opts)`. Konstruktor
     * pozostawiony tylko jako error-throw guard żeby consumer nie używał
     * sync semantyki z v3.x.
     */
    constructor(handle) {
        if (handle == null || typeof handle !== 'object') {
            throw new Error(
                'ModbusRTU: bezpośredni `new ModbusRTU(...)` nie jest wspierany w v4.0.0. ' +
                'Użyj `await ModbusRTU.open({ port, baudRate, transceiver, dePin, rePin })`.'
            );
        }
        this._handle = handle;
        this._closed = false;
    }

    _assertOpen() {
        if (this._closed) throw new Error('ModbusRTU: device already closed');
    }

    // ---------- read ops (zwracają natywne arrays) ----------

    readCoils(slaveID, startAddr, count) {
        this._assertOpen();
        return native.ReadCoils(this._handle, slaveID, startAddr, count);
    }

    readDiscreteInputs(slaveID, startAddr, count) {
        this._assertOpen();
        return native.ReadDiscreteInputs(this._handle, slaveID, startAddr, count);
    }

    readHoldingRegisters(slaveID, startAddr, count) {
        this._assertOpen();
        return native.ReadHoldingRegisters(this._handle, slaveID, startAddr, count);
    }

    readInputRegisters(slaveID, startAddr, count) {
        this._assertOpen();
        return native.ReadInputRegisters(this._handle, slaveID, startAddr, count);
    }

    // ---------- write ops (Promise<void>) ----------

    writeCoil(slaveID, coilAddr, value) {
        this._assertOpen();
        return native.WriteCoil(this._handle, slaveID, coilAddr, Boolean(value));
    }

    writeRegister(slaveID, regAddr, value) {
        this._assertOpen();
        return native.WriteRegister(this._handle, slaveID, regAddr, value);
    }

    writeMultipleCoils(slaveID, startAddr, values) {
        this._assertOpen();
        return native.WriteMultipleCoils(this._handle, slaveID, startAddr, values.map(Boolean));
    }

    writeMultipleRegisters(slaveID, startAddr, values) {
        this._assertOpen();
        return native.WriteMultipleRegisters(this._handle, slaveID, startAddr, values);
    }

    // ---------- config ----------

    /**
     * Per-instance debug. Wcześniej env var MAX485_DEBUG (globalny). Teraz
     * consumer sam decyduje per device:
     *   if (process.env.MAX485_DEBUG) device.setDebug(parseInt(process.env.MAX485_DEBUG, 10) || 1);
     *
     * @param {number} level — 0=off, 1=basic events, 2=basic + hex TX/RX
     */
    setDebug(level) {
        this._assertOpen();
        native.SetDebug(this._handle, level | 0);
    }

    /**
     * Opcjonalny retry/backoff dla transient errors (timeout, CRC error).
     * ModbusException (illegal address, slave failure etc.) NIE jest retry'owany
     * (permanent error). Domyślnie wyłączone.
     *
     * @param {object} [cfg]
     * @param {number} cfg.maxRetries   — 0 lub null/undefined = disable
     * @param {number} cfg.backoffMs    — pauza między próbami w ms
     */
    setRetryConfig(cfg) {
        this._assertOpen();
        if (!cfg) {
            native.SetRetryConfig(this._handle, 0, 0);
            return;
        }
        const { maxRetries = 0, backoffMs = 0 } = cfg;
        native.SetRetryConfig(this._handle, maxRetries | 0, backoffMs | 0);
    }

    // ---------- observability ----------

    /**
     * Snapshot counters. Zwraca obiekt:
     * {
     *   opsTotal: BigInt,
     *   opsByResult: { success: BigInt, timeout: BigInt, crc_error: BigInt, exception: BigInt, io_error: BigInt },
     *   opsBySlave: { '21': { ops, successes, timeouts, crcErrors, exceptions, ioErrors, sumLatencyMicro } (all BigInt), ... },
     *   lastTxUnixNano: BigInt,
     *   lastRxUnixNano: BigInt
     * }
     *
     * BigInt — bo counters mogą przekroczyć Number.MAX_SAFE_INTEGER (2^53)
     * w długo działających instancjach. Consumer może zawsze `Number(v)`
     * jeśli wie że safely fits.
     */
    stats() {
        this._assertOpen();
        return native.Stats(this._handle);
    }

    // ---------- lifecycle ----------

    /**
     * Async close — port zwolniony, GPIO bus-idle, lockfile removed, rpio
     * refcount decremented.
     *
     * Idempotent (drugie wywołanie = no-op). Jeśli nie wywołane, finalize_cb
     * po stronie napi posprząta przy GC instancji (F1.1 / A4) — ale dobrym
     * tonem jest explicit close (deterministic timing).
     */
    async close() {
        if (this._closed) return;
        this._closed = true;
        await native.Close(this._handle);
    }
}

module.exports = ModbusRTU;
