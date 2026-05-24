//go:build cgo
// +build cgo

package main

/*
#include <node_api.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>

// Function declarations
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

// Helper functions to convert types
static uint8_t get_uint8(napi_env env, napi_value value) {
    uint32_t result;
    napi_get_value_uint32(env, value, &result);
    return (uint8_t)result;
}

static uint16_t get_uint16(napi_env env, napi_value value) {
    uint32_t result;
    napi_get_value_uint32(env, value, &result);
    return (uint16_t)result;
}

// Helper function to create error
static napi_value create_error(napi_env env, const char* message) {
    napi_value error_msg;
    napi_create_string_utf8(env, message, strlen(message), &error_msg);
    napi_value error;
    napi_create_error(env, NULL, error_msg, &error);
    return error;
}

// M6 FIX: prawdziwy JS throw (zamiast return Error object jako value).
// Wcześniej NewModbusDeviceJS zwracało Error na connect failure, index.js
// zapisywał to jako this.device (truthy!), kolejne wywołania crashowały
// Go z nil pointer panic. Throw → JS musi try/catch, propagacja jasna.
static napi_value throw_error(napi_env env, const char* message) {
    napi_throw_error(env, NULL, message);
    napi_value undef;
    napi_get_undefined(env, &undef);
    return undef;
}

// Helper function to create success response
static napi_value create_success(napi_env env) {
    napi_value result;
    napi_create_string_utf8(env, "success", 7, &result);
    return result;
}

// Helper function to create function
static void create_function(napi_env env, napi_value exports, const char* name, napi_callback cb) {
    napi_value fn;
    napi_create_function(env, NULL, 0, cb, NULL, &fn);
    napi_set_named_property(env, exports, name, fn);
}
*/
import "C"
import (
    "encoding/json"
    "fmt"
    "runtime/cgo"
    "unsafe"
)

// =====================================================================
// M4 + M5: validation helpers
//
// Wcześniej każda *JS funkcja robiła `napi_get_cb_info` i `napi_get_value_*`
// bez sprawdzania return status ani napi_typeof. Przy błędnym wywołaniu z JS
// (brak args, zły typ) bus dostawał garbage values — w najgorszym scenariuszu
// destructive write do losowego slave'a (M4 z review).
//
// Te helpery centralizują walidację: każda *JS funkcja używa np.
// `args, ok := getArgs(env, info, 4); if !ok { return undef(env) }`
// — w razie problemu rzucamy JS exception i wracamy undefined.
// =====================================================================

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

// getArgs walidates argument count (>= expected) i zwraca napi values + true/false.
// Na error throws JS exception i zwraca (zeros, false) — wywołujący ma return undef(env).
func getArgs(env C.napi_env, info C.napi_callback_info, expected int) ([8]C.napi_value, bool) {
    var args [8]C.napi_value
    var argc C.size_t = C.size_t(expected)
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

// M7 (v4.0.0): wszystkie błędy operacji bus → napi_throw_error → JS dostaje
// natywny throw (await odrzuca). Wcześniej zwracaliśmy string "Error: ..." i
// index.js robił `result.startsWith('Error:') && throw new Error(result)` —
// fragile (pomyłka konwencji = błąd cicho ignorowany), nieczytelny stack trace,
// brak typed errors (ModbusException już nie odróżnialny od generic).
// BREAKING: consumers nie polegający na await/try-catch (np. .then bez .catch)
// muszą się dostosować. v3.x → v4.0 wymaga porządnego try/catch wokół async ops.

func returnJSON(env C.napi_env, v interface{}) C.napi_value {
    data, _ := json.Marshal(v)
    cs := C.CString(string(data))
    defer C.free(unsafe.Pointer(cs))
    var r C.napi_value
    C.napi_create_string_utf8(env, cs, C.size_t(len(data)), &r)
    return r
}

// readDevice was deprecated by M4/M5 — zastąpione getDeviceArg() które robi
// pełną walidację typu i nullability. Zachowujemy komentarz historyczny:
//
// External handle jest opaque pointer do cgo.Handle (allocated via C.malloc).
// cgo.Handle (Go 1.17+) chroni przed Go GC ruszającym obiektem — wcześniejsza
// implementacja przekazywała raw Go pointer co powodowało intermittent SIGSEGV
// w long-running procesach (cgo rules forbid storing Go pointers in C memory).
// See https://pkg.go.dev/runtime/cgo#Handle.

//export NewModbusDeviceJS
func NewModbusDeviceJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4)
    if !ok {
        return undef(env)
    }
    portStr, ok := getStringArg(env, args[0], "port")
    if !ok {
        return undef(env)
    }
    baudRate, ok := getInt32Arg(env, args[1], "baudRate")
    if !ok {
        return undef(env)
    }
    dePin, ok := getInt32Arg(env, args[2], "dePin")
    if !ok {
        return undef(env)
    }
    rePin, ok := getInt32Arg(env, args[3], "rePin")
    if !ok {
        return undef(env)
    }

    device, err := NewModbusDevice(portStr, int(baudRate), int(dePin), int(rePin))
    if err != nil {
        // M6: throw zamiast return Error object — JS dostaje exception
        return napiThrow(env, err.Error())
    }

    // Zapakuj *ModbusDevice w cgo.Handle (Go 1.17+) — Go GC nie ruszy
    // obiektu dopóki handle żyje, a C trzyma tylko opaque uintptr.
    // Sam handle jest kopiowany do C-allocated pamięci żeby napi-external
    // mógł go bezpiecznie przechowywać przez cały lifetime obiektu po
    // stronie JS.
    h := cgo.NewHandle(device)
    handlePtr := C.malloc(C.size_t(unsafe.Sizeof(cgo.Handle(0))))
    *(*cgo.Handle)(handlePtr) = h

    var result C.napi_value
    C.napi_create_external(env, handlePtr, nil, nil, &result)
    return result
}

//export ReadCoilsJS
func ReadCoilsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    startAddr, ok := getUint16Arg(env, args[2], "startAddr"); if !ok { return undef(env) }
    count, ok := getUint16Arg(env, args[3], "count"); if !ok { return undef(env) }
    values, err := device.ReadCoils(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil { return napiThrow(env, err.Error()) }
    return returnJSON(env, values)
}

//export ReadDiscreteInputsJS
func ReadDiscreteInputsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    startAddr, ok := getUint16Arg(env, args[2], "startAddr"); if !ok { return undef(env) }
    count, ok := getUint16Arg(env, args[3], "count"); if !ok { return undef(env) }
    values, err := device.ReadDiscreteInputs(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil { return napiThrow(env, err.Error()) }
    return returnJSON(env, values)
}

//export ReadHoldingRegistersJS
func ReadHoldingRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    startAddr, ok := getUint16Arg(env, args[2], "startAddr"); if !ok { return undef(env) }
    count, ok := getUint16Arg(env, args[3], "count"); if !ok { return undef(env) }
    values, err := device.ReadHoldingRegisters(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil { return napiThrow(env, err.Error()) }
    return returnJSON(env, values)
}

//export ReadInputRegistersJS
func ReadInputRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    startAddr, ok := getUint16Arg(env, args[2], "startAddr"); if !ok { return undef(env) }
    count, ok := getUint16Arg(env, args[3], "count"); if !ok { return undef(env) }
    values, err := device.ReadInputRegisters(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil { return napiThrow(env, err.Error()) }
    return returnJSON(env, values)
}

//export WriteCoilJS
func WriteCoilJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    coilAddr, ok := getUint16Arg(env, args[2], "coilAddr"); if !ok { return undef(env) }
    value, ok := getBoolArg(env, args[3], "value"); if !ok { return undef(env) }
    err := device.WriteCoil(byte(slaveID), uint16(coilAddr), value)
    if err != nil { return napiThrow(env, err.Error()) }
    return C.create_success(env)
}

//export WriteRegisterJS
func WriteRegisterJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    regAddr, ok := getUint16Arg(env, args[2], "regAddr"); if !ok { return undef(env) }
    value, ok := getUint16Arg(env, args[3], "value"); if !ok { return undef(env) }
    err := device.WriteRegister(byte(slaveID), uint16(regAddr), uint16(value))
    if err != nil { return napiThrow(env, err.Error()) }
    return C.create_success(env)
}

//export WriteMultipleCoilsJS
func WriteMultipleCoilsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    startAddr, ok := getUint16Arg(env, args[2], "startAddr"); if !ok { return undef(env) }
    // values musi być array
    var t C.napi_valuetype
    C.napi_typeof(env, args[3], &t)
    var isArr C.bool
    C.napi_is_array(env, args[3], &isArr)
    if !bool(isArr) {
        return napiThrow(env, fmt.Sprintf("arg values: expected array (typeof=%d)", int(t)))
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
        if !ok { return undef(env) }
        goValues[i] = v
    }
    err := device.WriteMultipleCoils(byte(slaveID), uint16(startAddr), goValues)
    if err != nil { return napiThrow(env, err.Error()) }
    return C.create_success(env)
}

//export WriteMultipleRegistersJS
func WriteMultipleRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 4); if !ok { return undef(env) }
    device, ok := getDeviceArg(env, args[0], "device"); if !ok { return undef(env) }
    slaveID, ok := getUint8Arg(env, args[1], "slaveID"); if !ok { return undef(env) }
    startAddr, ok := getUint16Arg(env, args[2], "startAddr"); if !ok { return undef(env) }
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
        if !ok { return undef(env) }
        goValues[i] = uint16(v)
    }
    err := device.WriteMultipleRegisters(byte(slaveID), uint16(startAddr), goValues)
    if err != nil { return napiThrow(env, err.Error()) }
    return C.create_success(env)
}

//export CloseJS
func CloseJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    args, ok := getArgs(env, info, 1); if !ok { return undef(env) }
    // Direct external read (nie używamy getDeviceArg bo musimy też dostać handlePtr do Delete)
    var t C.napi_valuetype
    if C.napi_typeof(env, args[0], &t) != C.napi_ok || t != C.napi_external {
        return napiThrow(env, fmt.Sprintf("arg device: expected external (typeof=%d)", int(t)))
    }
    var handlePtr unsafe.Pointer
    if C.napi_get_value_external(env, args[0], &handlePtr) != C.napi_ok || handlePtr == nil {
        return napiThrow(env, "arg device: invalid handle")
    }
    h := *(*cgo.Handle)(handlePtr)
    device, ok2 := h.Value().(*ModbusDevice)
    if !ok2 || device == nil {
        return napiThrow(env, "arg device: handle does not point to ModbusDevice")
    }

    device.Close()
    h.Delete()
    C.free(handlePtr)

    return C.create_success(env)
}

//export Init
func Init(env C.napi_env, exports C.napi_value) C.napi_value {
    var modbusDevice C.napi_value
    C.napi_create_object(env, &modbusDevice)

    // N6 FIX: helper żeby uniknąć leaków C.CString (poprzednio 10× CString
    // alokowane bez free — ~150B leak per module init, mały ale niepotrzebny).
    register := func(name string, cb C.napi_callback) {
        cs := C.CString(name)
        defer C.free(unsafe.Pointer(cs))
        C.create_function(env, modbusDevice, cs, cb)
    }

    register("NewModbusDevice", (C.napi_callback)(C.NewModbusDeviceJS))
    register("ReadCoils", (C.napi_callback)(C.ReadCoilsJS))
    register("ReadDiscreteInputs", (C.napi_callback)(C.ReadDiscreteInputsJS))
    register("ReadHoldingRegisters", (C.napi_callback)(C.ReadHoldingRegistersJS))
    register("ReadInputRegisters", (C.napi_callback)(C.ReadInputRegistersJS))
    register("WriteCoil", (C.napi_callback)(C.WriteCoilJS))
    register("WriteRegister", (C.napi_callback)(C.WriteRegisterJS))
    register("WriteMultipleCoils", (C.napi_callback)(C.WriteMultipleCoilsJS))
    register("WriteMultipleRegisters", (C.napi_callback)(C.WriteMultipleRegistersJS))
    register("Close", (C.napi_callback)(C.CloseJS))

    return modbusDevice
}
