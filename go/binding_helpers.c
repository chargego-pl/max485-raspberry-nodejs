// cgo //export rules: kod w preamble pliku Go z //export NIE może referować
// eksportowanych funkcji. Workaround: helper C wrappery w osobnym pliku który
// inkluduje _cgo_export.h (auto-generated przez cgo).
//
// Te wrappery są wołane z Go (binding.go) zamiast bezpośrednich napi_*
// funkcji wymagających przekazania pointerów na exported Go callbacks.

#include <node_api.h>
#include <stddef.h>
#include "_cgo_export.h"

napi_status make_async_work(napi_env env, napi_value name, void* data, napi_async_work* result) {
    return napi_create_async_work(env, NULL, name,
                                  asyncExecute,
                                  asyncComplete,
                                  data, result);
}

napi_status make_external_device(napi_env env, void* data, napi_value* result) {
    return napi_create_external(env, data, deviceFinalize, NULL, result);
}
