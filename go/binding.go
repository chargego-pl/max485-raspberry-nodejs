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
    "runtime/cgo"
    "unsafe"
)

// readDevice extracts the *ModbusDevice from a napi external value.
// The external stores a pointer to a cgo.Handle (allocated via C.malloc)
// rather than a raw Go pointer — Go's GC can move/relocate Go objects,
// which invalidates raw pointers held on the C side and was the cause
// of intermittent SIGSEGV in long-running processes (cgo rules forbid
// storing Go pointers in C memory).
//
// See https://pkg.go.dev/runtime/cgo#Handle.
func readDevice(env C.napi_env, jsExternal C.napi_value) *ModbusDevice {
    var handlePtr unsafe.Pointer
    C.napi_get_value_external(env, jsExternal, &handlePtr)
    h := *(*cgo.Handle)(handlePtr)
    return h.Value().(*ModbusDevice)
}

//export NewModbusDeviceJS
func NewModbusDeviceJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    var portLen C.size_t
    C.napi_get_value_string_utf8(env, args[0], nil, 0, &portLen)
    port := make([]C.char, portLen+1)
    C.napi_get_value_string_utf8(env, args[0], &port[0], portLen+1, nil)
    portStr := C.GoString(&port[0])

    var baudRate C.int32_t
    C.napi_get_value_int32(env, args[1], &baudRate)

    var dePin C.int32_t
    C.napi_get_value_int32(env, args[2], &dePin)

    var rePin C.int32_t
    C.napi_get_value_int32(env, args[3], &rePin)

    device, err := NewModbusDevice(portStr, int(baudRate), int(dePin), int(rePin))
    if err != nil {
        // M6: throw zamiast return Error object — JS dostaje exception
        errStr := C.CString(err.Error())
        defer C.free(unsafe.Pointer(errStr))
        return C.throw_error(env, errStr)
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
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    startAddr := C.get_uint16(env, args[2])
    count := C.get_uint16(env, args[3])

    values, err := device.ReadCoils(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil {
        // Spójność z pozostałymi *JS — zwracamy string "Error: ..."
        // żeby index.js (`result.startsWith('Error:')`) nie crashował przy
        // Modbus timeoutach.
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: "+err.Error())), &result)
        return result
    }

    jsonData, _ := json.Marshal(values)
    jsonStr := C.CString(string(jsonData))
    defer C.free(unsafe.Pointer(jsonStr))
    var result C.napi_value
    C.napi_create_string_utf8(env, jsonStr, C.size_t(len(jsonData)), &result)
    return result
}

//export ReadDiscreteInputsJS
func ReadDiscreteInputsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    startAddr := C.get_uint16(env, args[2])
    count := C.get_uint16(env, args[3])

    values, err := device.ReadDiscreteInputs(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    jsonData, _ := json.Marshal(values)
    jsonStr := C.CString(string(jsonData))
    defer C.free(unsafe.Pointer(jsonStr))
    var result C.napi_value
    C.napi_create_string_utf8(env, jsonStr, C.size_t(len(jsonData)), &result)
    return result
}

//export ReadHoldingRegistersJS
func ReadHoldingRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    startAddr := C.get_uint16(env, args[2])
    count := C.get_uint16(env, args[3])

    values, err := device.ReadHoldingRegisters(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    jsonData, _ := json.Marshal(values)
    jsonStr := C.CString(string(jsonData))
    defer C.free(unsafe.Pointer(jsonStr))
    var result C.napi_value
    C.napi_create_string_utf8(env, jsonStr, C.size_t(len(jsonData)), &result)
    return result
}

//export ReadInputRegistersJS
func ReadInputRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    startAddr := C.get_uint16(env, args[2])
    count := C.get_uint16(env, args[3])

    values, err := device.ReadInputRegisters(byte(slaveID), uint16(startAddr), uint16(count))
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    jsonData, _ := json.Marshal(values)
    jsonStr := C.CString(string(jsonData))
    defer C.free(unsafe.Pointer(jsonStr))
    var result C.napi_value
    C.napi_create_string_utf8(env, jsonStr, C.size_t(len(jsonData)), &result)
    return result
}

//export WriteCoilJS
func WriteCoilJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    coilAddr := C.get_uint16(env, args[2])
    
    var value C.bool
    C.napi_get_value_bool(env, args[3], &value)

    err := device.WriteCoil(byte(slaveID), uint16(coilAddr), bool(value))
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    return C.create_success(env)
}

//export WriteRegisterJS
func WriteRegisterJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    regAddr := C.get_uint16(env, args[2])
    value := C.get_uint16(env, args[3])

    err := device.WriteRegister(byte(slaveID), uint16(regAddr), uint16(value))
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    return C.create_success(env)
}

//export WriteMultipleCoilsJS
func WriteMultipleCoilsJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    startAddr := C.get_uint16(env, args[2])

    var values C.napi_value = args[3]
    var length C.uint32_t
    C.napi_get_array_length(env, values, (*C.uint32_t)(&length))

    goValues := make([]bool, length)
    for i := C.uint32_t(0); i < length; i++ {
        var element C.napi_value
        C.napi_get_element(env, values, i, &element)
        var value C.bool
        C.napi_get_value_bool(env, element, &value)
        goValues[i] = bool(value)
    }

    err := device.WriteMultipleCoils(byte(slaveID), uint16(startAddr), goValues)
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    return C.create_success(env)
}

//export WriteMultipleRegistersJS
func WriteMultipleRegistersJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [4]C.napi_value
    var argc C.size_t = 4
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    device := readDevice(env, args[0])

    slaveID := C.get_uint8(env, args[1])
    startAddr := C.get_uint16(env, args[2])

    var values C.napi_value = args[3]
    var length C.uint32_t
    C.napi_get_array_length(env, values, (*C.uint32_t)(&length))

    goValues := make([]uint16, length)
    for i := C.uint32_t(0); i < length; i++ {
        var element C.napi_value
        C.napi_get_element(env, values, i, &element)
        value := C.get_uint16(env, element)
        goValues[i] = uint16(value)
    }

    err := device.WriteMultipleRegisters(byte(slaveID), uint16(startAddr), goValues)
    if err != nil {
        errStr := C.CString("Error: " + err.Error())
        defer C.free(unsafe.Pointer(errStr))
        var result C.napi_value
        C.napi_create_string_utf8(env, errStr, C.size_t(len("Error: " + err.Error())), &result)
        return result
    }

    return C.create_success(env)
}

//export CloseJS
func CloseJS(env C.napi_env, info C.napi_callback_info) C.napi_value {
    var args [1]C.napi_value
    var argc C.size_t = 1
    C.napi_get_cb_info(env, info, &argc, &args[0], nil, nil)

    var handlePtr unsafe.Pointer
    C.napi_get_value_external(env, args[0], &handlePtr)
    h := *(*cgo.Handle)(handlePtr)
    device := h.Value().(*ModbusDevice)

    device.Close()
    h.Delete()
    C.free(handlePtr)

    return C.create_success(env)
}

//export Init
func Init(env C.napi_env, exports C.napi_value) C.napi_value {
    var modbusDevice C.napi_value
    C.napi_create_object(env, &modbusDevice)

    C.create_function(env, modbusDevice, C.CString("NewModbusDevice"), (C.napi_callback)(C.NewModbusDeviceJS))
    C.create_function(env, modbusDevice, C.CString("ReadCoils"), (C.napi_callback)(C.ReadCoilsJS))
    C.create_function(env, modbusDevice, C.CString("ReadDiscreteInputs"), (C.napi_callback)(C.ReadDiscreteInputsJS))
    C.create_function(env, modbusDevice, C.CString("ReadHoldingRegisters"), (C.napi_callback)(C.ReadHoldingRegistersJS))
    C.create_function(env, modbusDevice, C.CString("ReadInputRegisters"), (C.napi_callback)(C.ReadInputRegistersJS))
    C.create_function(env, modbusDevice, C.CString("WriteCoil"), (C.napi_callback)(C.WriteCoilJS))
    C.create_function(env, modbusDevice, C.CString("WriteRegister"), (C.napi_callback)(C.WriteRegisterJS))
    C.create_function(env, modbusDevice, C.CString("WriteMultipleCoils"), (C.napi_callback)(C.WriteMultipleCoilsJS))
    C.create_function(env, modbusDevice, C.CString("WriteMultipleRegisters"), (C.napi_callback)(C.WriteMultipleRegistersJS))
    C.create_function(env, modbusDevice, C.CString("Close"), (C.napi_callback)(C.CloseJS))

    return modbusDevice
}
