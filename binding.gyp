{
  "targets": [{
    "target_name": "modbus",
    "sources": [ "src/binding.cc" ],
    "include_dirs": [
      "<!@(node -p \"require('node-addon-api').include\")"
    ],
    "dependencies": [
      "<!(node -p \"require('node-addon-api').gyp\")"
    ],
    "libraries": [
      "-L<(module_root_dir)",
      "-lmodbus",
      "-Wl,-rpath,\\$$ORIGIN/../.."
    ],
    "cflags!": [ "-fno-exceptions" ],
    "cflags_cc!": [ "-fno-exceptions" ]
  }]
}
