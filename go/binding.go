//go:build cgo
// +build cgo

package main

// v4.0.0 binding — kompletny rewrite. Główne różnice vs v3.x:
//
//   F1.1 / A4: napi_finalize callback dla external handle — GC bez .close()
//              już nie leak'uje portu / GPIO / lockfile.
//   F3.1 / A21: ModbusException konwertowany na natywny JS Error z structured
//              properties (code='MODBUS_EXCEPTION', slaveID, functionCode,
//              exceptionCode) — consumer rozróżnia bez parse'owania message.
//   F3.2 / A19: Write* funkcje zwracają undefined (Promise<void>) zamiast
//              "success" string.
//   F4.x / A1: Wszystkie bus operations (Read*, Write*, NewModbusDevice, Close)
//              przepuszczone przez napi_async_work — execute callback leci
//              na uv worker thread, Node event loop nie blokowany.
//   F5.5 / A18: ReadCoils/ReadDiscreteInputs/ReadHoldingRegisters/ReadInputRegisters
//              zwracają napi arrays (boolean / number) zamiast JSON string.
//
// Architektura async ops:
//   - JS wywołuje *JS funkcję → walidacja args.
//   - Tworzymy napi_promise + napi_async_work, w danych work'a wskaźnik na
//     Go-side asyncOp (cgo.Handle).
//   - napi_queue_async_work — Node odpala execute_cb na thread'zie z uv pool'a.
//   - execute_cb wywołuje goExecuteAsyncOp(handle) — Go robi I/O.
//   - complete_cb wywołuje goCompleteAsyncOp(env, handle, deferred) —
//     buduje result napi value lub error i resolve/reject deferred.
//
// Synchroniczne pozostają: SetDebug, SetRetryConfig, Stats (czyste set/get,
// bez bus I/O).

/*
#include <node_api.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>

// ---- Function declarations (Go //export side) ----
napi_value NewModbusDeviceJS(napi_env env, napi_callback_info info);
napi_value ReadCoilsJS(napi_env env, napi_callback_info info);
napi_value ReadDiscreteInputsJS(napi_env env, napi_callback_info info);
napi_value ReadHoldingRegistersJS(napi_env env, napi_callback_info info);
napi_value ReadInputRegistersJS(napi_env env, napi_callback_info info);
napi_value WriteCoilJS(napi_env env, napi_callback_info info);
napi_value WriteRegisterJS(napi_env env, napi_callback_info info);
napi_value WriteMultipleCoilsJS(napi_env env, napi_callback_info info);
napi_value WriteMultipleRegistersJS(napi_env env, napi_callback_info info);
napi_value CloseJS(napi_env env, napi_callback_info info);
napi_value SetDebugJS(napi_env env, napi_callback_info info);
napi_value SetRetryConfigJS(napi_env env, napi_callback_info info);
napi_value StatsJS(napi_env env, napi_callback_info info);

// napi_finalize callback dla external device handle (F1.1 / A4)
void deviceFinalize(napi_env env, void* data, void* hint);

// async_work callbacks
void asyncExecute(napi_env env, void* data);
void asyncComplete(napi_env env, napi_status status, void* data);

// ---- Helper to register function in exports ----
static void create_function(napi_env env, napi_value exports, const char* name, napi_callback cb) {
    napi_value fn;
    napi_create_function(env, NULL, 0, cb, NULL, &fn);
    napi_set_named_property(env, exports, name, fn);
}

// ---- Helper: create JS Error z structured properties (F3.1 / A21) ----
//
// Buduje napi Error object z:
//   - error.code     = "MODBUS_EXCEPTION"
//   - error.message  = human-readable
//   - error.slaveID, error.functionCode, error.exceptionCode (number)
// I throws.
static void throw_modbus_exception(napi_env env, uint8_t slaveID, uint8_t fc, uint8_t excCode, const char* desc) {
    char msg[256];
    snprintf(msg, sizeof(msg), "modbus exception: slave=0x%02X fc=0x%02X code=0x%02X (%s)",
             slaveID, fc, excCode, desc);

    napi_value code_str, msg_str, err;
    napi_create_string_utf8(env, "MODBUS_EXCEPTION", NAPI_AUTO_LENGTH, &code_str);
    napi_create_string_utf8(env, msg, NAPI_AUTO_LENGTH, &msg_str);
    napi_create_error(env, code_str, msg_str, &err);

    napi_value slave_v, fc_v, exc_v;
    napi_create_uint32(env, slaveID, &slave_v);
    napi_create_uint32(env, fc, &fc_v);
    napi_create_uint32(env, excCode, &exc_v);
    napi_set_named_property(env, err, "slaveID", slave_v);
    napi_set_named_property(env, err, "functionCode", fc_v);
    napi_set_named_property(env, err, "exceptionCode", exc_v);

    napi_throw(env, err);
}

// ---- Helper: reject deferred z generic Error ----
static void reject_with_string(napi_env env, napi_deferred deferred, const char* msg) {
    napi_value err_str, err;
    napi_create_string_utf8(env, msg, NAPI_AUTO_LENGTH, &err_str);
    napi_create_error(env, NULL, err_str, &err);
    napi_reject_deferred(env, deferred, err);
}

// ---- Helper: reject deferred z typed ModbusException ----
static void reject_with_modbus_exception(napi_env env, napi_deferred deferred,
                                          uint8_t slaveID, uint8_t fc, uint8_t excCode, const char* desc) {
    char msg[256];
    snprintf(msg, sizeof(msg), "modbus exception: slave=0x%02X fc=0x%02X code=0x%02X (%s)",
             slaveID, fc, excCode, desc);

    napi_value code_str, msg_str, err;
    napi_create_string_utf8(env, "MODBUS_EXCEPTION", NAPI_AUTO_LENGTH, &code_str);
    napi_create_string_utf8(env, msg, NAPI_AUTO_LENGTH, &msg_str);
    napi_create_error(env, code_str, msg_str, &err);

    napi_value slave_v, fc_v, exc_v;
    napi_create_uint32(env, slaveID, &slave_v);
    napi_create_uint32(env, fc, &fc_v);
    napi_create_uint32(env, excCode, &exc_v);
    napi_set_named_property(env, err, "slaveID", slave_v);
    napi_set_named_property(env, err, "functionCode", fc_v);
    napi_set_named_property(env, err, "exceptionCode", exc_v);

    napi_reject_deferred(env, deferred, err);
}

// ---- Helpers: build napi typed arrays from C arrays ----
static napi_value build_bool_array(napi_env env, const uint8_t* values, size_t len) {
    napi_value arr;
    napi_create_array_with_length(env, len, &arr);
    for (size_t i = 0; i < len; i++) {
        napi_value v;
        napi_get_boolean(env, values[i] ? true : false, &v);
        napi_set_element(env, arr, i, v);
    }
    return arr;
}

static napi_value build_uint16_array(napi_env env, const uint16_t* values, size_t len) {
    napi_value arr;
    napi_create_array_with_length(env, len, &arr);
    for (size_t i = 0; i < len; i++) {
        napi_value v;
        napi_create_uint32(env, values[i], &v);
        napi_set_element(env, arr, i, v);
    }
    return arr;
}

// ---- AUTO_LENGTH wrapper (NAPI_AUTO_LENGTH macro nie zawsze dostępne z cgo) ----
// Ten helper jest BEZPIECZNY w preamble bo NIE referuje //export funkcji.
static napi_status create_string_auto(napi_env env, const char* s, napi_value* result) {
    return napi_create_string_utf8(env, s, NAPI_AUTO_LENGTH, result);
}

// make_async_work + make_external_device są zdefiniowane w binding_helpers.c
// (osobnym pliku) bo wymagają referencji do exported Go callbacks (asyncExecute,
// asyncComplete, deviceFinalize). cgo zakazuje referowania //export z preamble
// tego samego pliku Go.
extern napi_status make_async_work(napi_env env, napi_value name, void* data, napi_async_work* result);
extern napi_status make_external_device(napi_env env, void* data, napi_value* result);
*/
import "C"
import (
	"fmt"
	"runtime/cgo"
	"time"
	"unsafe"
)

// ---------- arg validation helpers (M4/M5) ----------

func undef(env C.napi_env) C.napi_value {
	var u C.napi_value
	C.napi_get_undefined(env, &u)
	return u
}

func napiThrow(env C.napi_env, msg string) C.napi_value {
	cs := C.CString(msg)
	defer C.free(unsafe.Pointer(cs))
	C.napi_throw_error(env, nil, cs)
	return undef(env)
}

func getArgs(env C.napi_env, info C.napi_callback_info, expected int) ([8]C.napi_value, bool) {
	var args [8]C.napi_value
	argc := C.size_t(expected)
	if C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil) != C.napi_ok {
		napiThrow(env, "napi_get_cb_info failed")
		return args, false
	}
	if int(argc) < expected {
		napiThrow(env, fmt.Sprintf("expected %d args, got %d", expected, int(argc)))
		return args, false
	}
	return args, true
}

func getUint8Arg(env C.napi_env, v C.napi_value, name string) (C.uint8_t, bool) {
	var t C.napi_valuetype
	if C.napi_typeof(env, v, &t) != C.napi_ok || t != C.napi_number {
		napiThrow(env, fmt.Sprintf("arg %s: expected number (typeof=%d)", name, int(t)))
		return 0, false
	}
	var u C.uint32_t
	if C.napi_get_value_uint32(env, v, &u) != C.napi_ok {
		napiThrow(env, fmt.Sprintf("arg %s: napi_get_value_uint32 failed", name))
		return 0, false
	}
	return C.uint8_t(u), true
}

func getUint16Arg(env C.napi_env, v C.napi_value, name string) (C.uint16_t, bool) {
	var t C.napi_valuetype
	if C.napi_typeof(env, v, &t) != C.napi_ok || t != C.napi_number {
		napiThrow(env, fmt.Sprintf("arg %s: expected number (typeof=%d)", name, int(t)))
		return 0, false
	}
	var u C.uint32_t
	if C.napi_get_value_uint32(env, v, &u) != C.napi_ok {
		napiThrow(env, fmt.Sprintf("arg %s: napi_get_value_uint32 failed", name))
		return 0, false
	}
	return C.uint16_t(u), true
}

func getInt32Arg(env C.napi_env, v C.napi_value, name string) (C.int32_t, bool) {
	var t C.napi_valuetype
	if C.napi_typeof(env, v, &t) != C.napi_ok || t != C.napi_number {
		napiThrow(env, fmt.Sprintf("arg %s: expected number (typeof=%d)", name, int(t)))
		return 0, false
	}
	var i C.int32_t
	if C.napi_get_value_int32(env, v, &i) != C.napi_ok {
		napiThrow(env, fmt.Sprintf("arg %s: napi_get_value_int32 failed", name))
		return 0, false
	}
	return i, true
}

func getStringArg(env C.napi_env, v C.napi_value, name string) (string, bool) {
	var t C.napi_valuetype
	if C.napi_typeof(env, v, &t) != C.napi_ok || t != C.napi_string {
		napiThrow(env, fmt.Sprintf("arg %s: expected string (typeof=%d)", name, int(t)))
		return "", false
	}
	var n C.size_t
	if C.napi_get_value_string_utf8(env, v, nil, 0, &n) != C.napi_ok {
		napiThrow(env, fmt.Sprintf("arg %s: napi_get_value_string_utf8 sizing failed", name))
		return "", false
	}
	buf := make([]C.char, n+1)
	if C.napi_get_value_string_utf8(env, v, &buf[0], n+1, nil) != C.napi_ok {
		napiThrow(env, fmt.Sprintf("arg %s: napi_get_value_string_utf8 read failed", name))
		return "", false
	}
	return C.GoString(&buf[0]), true
}

func getBoolArg(env C.napi_env, v C.napi_value, name string) (bool, bool) {
	var t C.napi_valuetype
	if C.napi_typeof(env, v, &t) != C.napi_ok || t != C.napi_boolean {
		napiThrow(env, fmt.Sprintf("arg %s: expected boolean (typeof=%d)", name, int(t)))
		return false, false
	}
	var b C.bool
	if C.napi_get_value_bool(env, v, &b) != C.napi_ok {
		napiThrow(env, fmt.Sprintf("arg %s: napi_get_value_bool failed", name))
		return false, false
	}
	return bool(b), true
}

func getDeviceArg(env C.napi_env, v C.napi_value, name string) (*ModbusDevice, bool) {
	var t C.napi_valuetype
	if C.napi_typeof(env, v, &t) != C.napi_ok || t != C.napi_external {
		napiThrow(env, fmt.Sprintf("arg %s: expected external device handle (typeof=%d)", name, int(t)))
		return nil, false
	}
	var handlePtr unsafe.Pointer
	if C.napi_get_value_external(env, v, &handlePtr) != C.napi_ok || handlePtr == nil {
		napiThrow(env, fmt.Sprintf("arg %s: invalid device handle", name))
		return nil, false
	}
	h := *(*cgo.Handle)(handlePtr)
	d, ok := h.Value().(*ModbusDevice)
	if !ok || d == nil {
		napiThrow(env, fmt.Sprintf("arg %s: device handle does not point to ModbusDevice", name))
		return nil, false
	}
	return d, true
}

// ---------- async op infrastructure (F4 / A1) ----------

// opType identyfikuje operację. Każda *JS funkcja tworzy asyncOp z odpowiednim opType,
// asyncExecute dispatch'uje na właściwą metodę ModbusDevice.
const (
	opNewDevice = iota + 1
	opClose
	opReadCoils
	opReadDiscreteInputs
	opReadHoldingRegisters
	opReadInputRegisters
	opWriteCoil
	opWriteRegister
	opWriteMultipleCoils
	opWriteMultipleRegisters
)

// asyncOp trzyma wszystko potrzebne dla pojedynczej async operacji.
// Kontekst budowany w *JS funkcji, przekazywany przez cgo.Handle do C, używany
// w asyncExecute (worker thread) i asyncComplete (main thread).
type asyncOp struct {
	opType   int
	device   *ModbusDevice
	slaveID  byte
	addr     uint16
	count    uint16
	boolVal  bool
	uintVal  uint16
	boolVals []bool
	uintVals []uint16

	// NewDevice params (tylko gdy opType == opNewDevice)
	newDeviceOpts NewModbusDeviceOptions

	// Result
	resultDevice *ModbusDevice // dla opNewDevice
	resultBools  []bool
	resultUints  []uint16
	err          error

	// napi_async_work handle (potrzebny do delete w complete)
	work     C.napi_async_work
	deferred C.napi_deferred
}

// queueAsyncOp tworzy promise + work, queue'uje go.
// Zwraca napi_value = promise.
func queueAsyncOp(env C.napi_env, op *asyncOp, resourceName string) C.napi_value {
	h := cgo.NewHandle(op)
	// Pakujemy handle do malloc'owanego storage żeby C miało stable pointer.
	handlePtr := C.malloc(C.size_t(unsafe.Sizeof(cgo.Handle(0))))
	*(*cgo.Handle)(handlePtr) = h

	var promise C.napi_value
	var deferred C.napi_deferred
	if C.napi_create_promise(env, &deferred, &promise) != C.napi_ok {
		h.Delete()
		C.free(handlePtr)
		return napiThrow(env, "napi_create_promise failed")
	}
	op.deferred = deferred

	rn := C.CString(resourceName)
	defer C.free(unsafe.Pointer(rn))
	var rnVal C.napi_value
	C.create_string_auto(env, rn, &rnVal)

	if C.make_async_work(env, rnVal, handlePtr, &op.work) != C.napi_ok {
		h.Delete()
		C.free(handlePtr)
		return napiThrow(env, "napi_create_async_work failed")
	}

	if C.napi_queue_async_work(env, op.work) != C.napi_ok {
		C.napi_delete_async_work(env, op.work)
		h.Delete()
		C.free(handlePtr)
		return napiThrow(env, "napi_queue_async_work failed")
	}

	return promise
}

//export asyncExecute
func asyncExecute(env C.napi_env, data unsafe.Pointer) {
	// UWAGA: ten callback leci na thread'zie z uv worker pool — NIE main thread.
	// Nie wolno tu używać żadnego napi_* (oprócz async_work primitives które są
	// thread-safe). Cała logika to czysty Go I/O.
	h := *(*cgo.Handle)(data)
	op, ok := h.Value().(*asyncOp)
	if !ok || op == nil {
		return
	}
	switch op.opType {
	case opNewDevice:
		op.resultDevice, op.err = NewModbusDevice(op.newDeviceOpts)
	case opClose:
		if op.device != nil {
			op.device.Close()
		}
	case opReadCoils:
		op.resultBools, op.err = op.device.ReadCoils(op.slaveID, op.addr, op.count)
	case opReadDiscreteInputs:
		op.resultBools, op.err = op.device.ReadDiscreteInputs(op.slaveID, op.addr, op.count)
	case opReadHoldingRegisters:
		op.resultUints, op.err = op.device.ReadHoldingRegisters(op.slaveID, op.addr, op.count)
	case opReadInputRegisters:
		op.resultUints, op.err = op.device.ReadInputRegisters(op.slaveID, op.addr, op.count)
	case opWriteCoil:
		op.err = op.device.WriteCoil(op.slaveID, op.addr, op.boolVal)
	case opWriteRegister:
		op.err = op.device.WriteRegister(op.slaveID, op.addr, op.uintVal)
	case opWriteMultipleCoils:
		op.err = op.device.WriteMultipleCoils(op.slaveID, op.addr, op.boolVals)
	case opWriteMultipleRegisters:
		op.err = op.device.WriteMultipleRegisters(op.slaveID, op.addr, op.uintVals)
	}
}

//export asyncComplete
func asyncComplete(env C.napi_env, status C.napi_status, data unsafe.Pointer) {
	// Main thread — napi_* OK.
	h := *(*cgo.Handle)(data)
	op, ok := h.Value().(*asyncOp)
	if !ok || op == nil {
		C.free(data)
		return
	}
	defer func() {
		C.napi_delete_async_work(env, op.work)
		h.Delete()
		C.free(data)
	}()

	if status != C.napi_ok {
		cs := C.CString("async work cancelled or failed")
		defer C.free(unsafe.Pointer(cs))
		C.reject_with_string(env, op.deferred, cs)
		return
	}

	// Error → reject (with typed exception jeśli ModbusException)
	if op.err != nil {
		if me, isExc := op.err.(*ModbusException); isExc {
			desc := exceptionDescription(me.ExceptionCode)
			cdesc := C.CString(desc)
			defer C.free(unsafe.Pointer(cdesc))
			C.reject_with_modbus_exception(env, op.deferred,
				C.uint8_t(me.SlaveID), C.uint8_t(me.FunctionCode),
				C.uint8_t(me.ExceptionCode), cdesc)
			return
		}
		msg := C.CString(op.err.Error())
		defer C.free(unsafe.Pointer(msg))
		C.reject_with_string(env, op.deferred, msg)
		return
	}

	// Success → build result, resolve.
	var resultVal C.napi_value
	switch op.opType {
	case opNewDevice:
		// Pakujemy *ModbusDevice w cgo.Handle + napi external z finalize.
		dh := cgo.NewHandle(op.resultDevice)
		dhPtr := C.malloc(C.size_t(unsafe.Sizeof(cgo.Handle(0))))
		*(*cgo.Handle)(dhPtr) = dh
		C.make_external_device(env, dhPtr, &resultVal)

	case opClose:
		// Po close: zwolnij external handle storage (deviceFinalize też to robi,
		// ale jeśli user wywołał explicit close, podwójne wolnenie nieprawidłowe.
		// Decyzja: NIE zwalniamy tutaj — finalize zostanie wywołany przez GC
		// niezależnie. close() jest idempotent (ModbusDevice.Close ma flag).
		C.napi_get_undefined(env, &resultVal)

	case opReadCoils, opReadDiscreteInputs:
		// Build bool array via C helper
		n := len(op.resultBools)
		if n == 0 {
			C.napi_create_array_with_length(env, 0, &resultVal)
		} else {
			cbuf := C.malloc(C.size_t(n))
			defer C.free(cbuf)
			cs := (*[1 << 30]C.uint8_t)(cbuf)
			for i, b := range op.resultBools {
				if b {
					cs[i] = 1
				} else {
					cs[i] = 0
				}
			}
			resultVal = C.build_bool_array(env, (*C.uint8_t)(cbuf), C.size_t(n))
		}

	case opReadHoldingRegisters, opReadInputRegisters:
		n := len(op.resultUints)
		if n == 0 {
			C.napi_create_array_with_length(env, 0, &resultVal)
		} else {
			cbuf := C.malloc(C.size_t(n * 2))
			defer C.free(cbuf)
			cs := (*[1 << 29]C.uint16_t)(cbuf)
			for i, v := range op.resultUints {
				cs[i] = C.uint16_t(v)
			}
			resultVal = C.build_uint16_array(env, (*C.uint16_t)(cbuf), C.size_t(n))
		}

	default: // write ops — zwracają undefined (F3.2 / A19)
		C.napi_get_undefined(env, &resultVal)
	}

	C.napi_resolve_deferred(env, op.deferred, resultVal)
}

// ---------- deviceFinalize (F1.1 / A4) ----------

//export deviceFinalize
func deviceFinalize(env C.napi_env, data unsafe.Pointer, hint unsafe.Pointer) {
	if data == nil {
		return
	}
	h := *(*cgo.Handle)(data)
	if device, ok := h.Value().(*ModbusDevice); ok && device != nil {
		device.Close()
	}
	h.Delete()
	C.free(data)
}

// ---------- *JS handlers ----------

//export NewModbusDeviceJS
func NewModbusDeviceJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	// Args: (portName: string, baudRate: number, opts: { transceiver: string, dePin?: number, rePin?: number, debug?: number })
	// Dla uproszczenia walidacji: przyjmujemy 5 osobnych args zamiast opts object.
	// Args: (portName, baudRate, transceiverType, dePin, rePin)
	args, ok := getArgs(env, info, 5)
	if !ok {
		return undef(env)
	}
	portStr, ok := getStringArg(env, args[0], "port")
	if !ok {
		return undef(env)
	}
	baud, ok := getInt32Arg(env, args[1], "baudRate")
	if !ok {
		return undef(env)
	}
	transceiverType, ok := getStringArg(env, args[2], "transceiverType")
	if !ok {
		return undef(env)
	}
	dePin, ok := getInt32Arg(env, args[3], "dePin")
	if !ok {
		return undef(env)
	}
	rePin, ok := getInt32Arg(env, args[4], "rePin")
	if !ok {
		return undef(env)
	}

	switch transceiverType {
	case "isl43485", "max485", "auto":
		// OK
	default:
		return napiThrow(env, fmt.Sprintf("unknown transceiver type: %s (expected isl43485|max485|auto)", transceiverType))
	}

	op := &asyncOp{
		opType: opNewDevice,
		newDeviceOpts: NewModbusDeviceOptions{
			PortName:        portStr,
			BaudRate:        int(baud),
			TransceiverType: transceiverType,
			DePin:           int(dePin),
			RePin:           int(rePin),
		},
	}
	return queueAsyncOp(env, op, "NewModbusDevice")
}

//export CloseJS
func CloseJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 1)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{opType: opClose, device: device}
	return queueAsyncOp(env, op, "Close")
}

//export ReadCoilsJS
func ReadCoilsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "startAddr")
	if !ok {
		return undef(env)
	}
	count, ok := getUint16Arg(env, args[3], "count")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{
		opType: opReadCoils, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), count: uint16(count),
	}
	return queueAsyncOp(env, op, "ReadCoils")
}

//export ReadDiscreteInputsJS
func ReadDiscreteInputsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "startAddr")
	if !ok {
		return undef(env)
	}
	count, ok := getUint16Arg(env, args[3], "count")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{
		opType: opReadDiscreteInputs, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), count: uint16(count),
	}
	return queueAsyncOp(env, op, "ReadDiscreteInputs")
}

//export ReadHoldingRegistersJS
func ReadHoldingRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "startAddr")
	if !ok {
		return undef(env)
	}
	count, ok := getUint16Arg(env, args[3], "count")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{
		opType: opReadHoldingRegisters, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), count: uint16(count),
	}
	return queueAsyncOp(env, op, "ReadHoldingRegisters")
}

//export ReadInputRegistersJS
func ReadInputRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "startAddr")
	if !ok {
		return undef(env)
	}
	count, ok := getUint16Arg(env, args[3], "count")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{
		opType: opReadInputRegisters, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), count: uint16(count),
	}
	return queueAsyncOp(env, op, "ReadInputRegisters")
}

//export WriteCoilJS
func WriteCoilJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "coilAddr")
	if !ok {
		return undef(env)
	}
	value, ok := getBoolArg(env, args[3], "value")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{
		opType: opWriteCoil, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), boolVal: value,
	}
	return queueAsyncOp(env, op, "WriteCoil")
}

//export WriteRegisterJS
func WriteRegisterJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "regAddr")
	if !ok {
		return undef(env)
	}
	value, ok := getUint16Arg(env, args[3], "value")
	if !ok {
		return undef(env)
	}
	op := &asyncOp{
		opType: opWriteRegister, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), uintVal: uint16(value),
	}
	return queueAsyncOp(env, op, "WriteRegister")
}

//export WriteMultipleCoilsJS
func WriteMultipleCoilsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "startAddr")
	if !ok {
		return undef(env)
	}
	var isArr C.bool
	C.napi_is_array(env, args[3], &isArr)
	if !bool(isArr) {
		return napiThrow(env, "arg values: expected array")
	}
	var length C.uint32_t
	if C.napi_get_array_length(env, args[3], &length) != C.napi_ok {
		return napiThrow(env, "arg values: napi_get_array_length failed")
	}
	goValues := make([]bool, length)
	for i := C.uint32_t(0); i < length; i++ {
		var element C.napi_value
		if C.napi_get_element(env, args[3], i, &element) != C.napi_ok {
			return napiThrow(env, fmt.Sprintf("arg values[%d]: napi_get_element failed", int(i)))
		}
		v, ok := getBoolArg(env, element, fmt.Sprintf("values[%d]", int(i)))
		if !ok {
			return undef(env)
		}
		goValues[i] = v
	}
	op := &asyncOp{
		opType: opWriteMultipleCoils, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), boolVals: goValues,
	}
	return queueAsyncOp(env, op, "WriteMultipleCoils")
}

//export WriteMultipleRegistersJS
func WriteMultipleRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 4)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	slaveID, ok := getUint8Arg(env, args[1], "slaveID")
	if !ok {
		return undef(env)
	}
	addr, ok := getUint16Arg(env, args[2], "startAddr")
	if !ok {
		return undef(env)
	}
	var isArr C.bool
	C.napi_is_array(env, args[3], &isArr)
	if !bool(isArr) {
		return napiThrow(env, "arg values: expected array")
	}
	var length C.uint32_t
	if C.napi_get_array_length(env, args[3], &length) != C.napi_ok {
		return napiThrow(env, "arg values: napi_get_array_length failed")
	}
	goValues := make([]uint16, length)
	for i := C.uint32_t(0); i < length; i++ {
		var element C.napi_value
		if C.napi_get_element(env, args[3], i, &element) != C.napi_ok {
			return napiThrow(env, fmt.Sprintf("arg values[%d]: napi_get_element failed", int(i)))
		}
		v, ok := getUint16Arg(env, element, fmt.Sprintf("values[%d]", int(i)))
		if !ok {
			return undef(env)
		}
		goValues[i] = uint16(v)
	}
	op := &asyncOp{
		opType: opWriteMultipleRegisters, device: device,
		slaveID: byte(slaveID), addr: uint16(addr), uintVals: goValues,
	}
	return queueAsyncOp(env, op, "WriteMultipleRegisters")
}

//export SetDebugJS
func SetDebugJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	args, ok := getArgs(env, info, 2)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	level, ok := getInt32Arg(env, args[1], "level")
	if !ok {
		return undef(env)
	}
	device.SetDebug(int(level))
	return undef(env)
}

//export SetRetryConfigJS
func SetRetryConfigJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	// Args: (device, maxRetries: number, backoffMs: number). 0/0 = disable.
	args, ok := getArgs(env, info, 3)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	maxRetries, ok := getInt32Arg(env, args[1], "maxRetries")
	if !ok {
		return undef(env)
	}
	backoffMs, ok := getInt32Arg(env, args[2], "backoffMs")
	if !ok {
		return undef(env)
	}
	if maxRetries <= 0 {
		device.SetRetryConfig(nil)
	} else {
		device.SetRetryConfig(&RetryConfig{
			MaxRetries: int(maxRetries),
			Backoff:    time.Duration(backoffMs) * time.Millisecond,
		})
	}
	return undef(env)
}

//export StatsJS
func StatsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
	// Synchroniczne (czysty snapshot atomic counters). Zwraca napi object.
	args, ok := getArgs(env, info, 1)
	if !ok {
		return undef(env)
	}
	device, ok := getDeviceArg(env, args[0], "device")
	if !ok {
		return undef(env)
	}
	snap := device.Stats()
	return statsToNapiObject(env, snap)
}

func statsToNapiObject(env C.napi_env, s Stats) C.napi_value {
	var obj C.napi_value
	C.napi_create_object(env, &obj)

	setU64 := func(name string, v uint64) {
		key := C.CString(name)
		defer C.free(unsafe.Pointer(key))
		var nv C.napi_value
		C.napi_create_bigint_uint64(env, C.uint64_t(v), &nv)
		C.napi_set_named_property(env, obj, key, nv)
	}
	setI64 := func(name string, v int64) {
		key := C.CString(name)
		defer C.free(unsafe.Pointer(key))
		var nv C.napi_value
		C.napi_create_bigint_int64(env, C.int64_t(v), &nv)
		C.napi_set_named_property(env, obj, key, nv)
	}

	setU64("opsTotal", s.OpsTotal)
	setI64("lastTxUnixNano", s.LastTxUnixNano)
	setI64("lastRxUnixNano", s.LastRxUnixNano)

	// opsByResult
	var byResult C.napi_value
	C.napi_create_object(env, &byResult)
	for k, v := range s.OpsByResult {
		key := C.CString(k)
		var nv C.napi_value
		C.napi_create_bigint_uint64(env, C.uint64_t(v), &nv)
		C.napi_set_named_property(env, byResult, key, nv)
		C.free(unsafe.Pointer(key))
	}
	keyBR := C.CString("opsByResult")
	C.napi_set_named_property(env, obj, keyBR, byResult)
	C.free(unsafe.Pointer(keyBR))

	// opsBySlave
	var bySlave C.napi_value
	C.napi_create_object(env, &bySlave)
	for slaveID, ss := range s.OpsBySlave {
		var slaveObj C.napi_value
		C.napi_create_object(env, &slaveObj)
		addU := func(name string, v uint64) {
			key := C.CString(name)
			defer C.free(unsafe.Pointer(key))
			var nv C.napi_value
			C.napi_create_bigint_uint64(env, C.uint64_t(v), &nv)
			C.napi_set_named_property(env, slaveObj, key, nv)
		}
		addU("ops", ss.Ops)
		addU("successes", ss.Successes)
		addU("timeouts", ss.Timeouts)
		addU("crcErrors", ss.CRCErrors)
		addU("exceptions", ss.Exceptions)
		addU("ioErrors", ss.IOErrors)
		addU("sumLatencyMicro", ss.SumLatencyMicro)

		slaveKey := C.CString(fmt.Sprintf("%d", slaveID))
		C.napi_set_named_property(env, bySlave, slaveKey, slaveObj)
		C.free(unsafe.Pointer(slaveKey))
	}
	keyBS := C.CString("opsBySlave")
	C.napi_set_named_property(env, obj, keyBS, bySlave)
	C.free(unsafe.Pointer(keyBS))

	return obj
}

// ---------- Init ----------

//export Init
func Init(env C.napi_env, exports C.napi_value) C.napi_value {
	var modbusDevice C.napi_value
	C.napi_create_object(env, &modbusDevice)

	register := func(name string, cb C.napi_callback) {
		cs := C.CString(name)
		defer C.free(unsafe.Pointer(cs))
		C.create_function(env, modbusDevice, cs, cb)
	}

	register("NewModbusDevice", C.napi_callback(C.NewModbusDeviceJS))
	register("ReadCoils", C.napi_callback(C.ReadCoilsJS))
	register("ReadDiscreteInputs", C.napi_callback(C.ReadDiscreteInputsJS))
	register("ReadHoldingRegisters", C.napi_callback(C.ReadHoldingRegistersJS))
	register("ReadInputRegisters", C.napi_callback(C.ReadInputRegistersJS))
	register("WriteCoil", C.napi_callback(C.WriteCoilJS))
	register("WriteRegister", C.napi_callback(C.WriteRegisterJS))
	register("WriteMultipleCoils", C.napi_callback(C.WriteMultipleCoilsJS))
	register("WriteMultipleRegisters", C.napi_callback(C.WriteMultipleRegistersJS))
	register("Close", C.napi_callback(C.CloseJS))
	register("SetDebug", C.napi_callback(C.SetDebugJS))
	register("SetRetryConfig", C.napi_callback(C.SetRetryConfigJS))
	register("Stats", C.napi_callback(C.StatsJS))

	return modbusDevice
}
