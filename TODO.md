# TODO — max485-raspberry-nodejs

Lista planowanych zmian w bibliotece w celu wyeliminowania chronicznych
błędów komunikacji Modbus obserwowanych na wiacie produkcyjnej ChargeGo.

Hardware kontekst tych zmian (z `RaspberryPi_Power_Shield_v2`):
- ISL43485IBZ RS485 transceiver z 10kΩ failsafe biasing (R9 A→3V3, R11 B→GND)
- 120Ω termination (R10) na masterze
- 10kΩ pull-up na RO/RE/DE/DI
- 3 slave'y zasilane z tej samej płytki (common GND przez power wire)
- 9600 baud, 8N1
- Środowisko z EMI (Victron MultiPlus II inwerter na tym samym GND railu)

## Workflow zmian — JEDNA ZMIANA NA RAZ

**Złota zasada:** każdy fix jest osobnym commitem na osobnym branchu i osobno
weryfikowany na wiacie produkcyjnej. Nie merge'ujemy kolejnego dopóki
poprzedni nie został potwierdzony jako stabilny.

### Cykl per zmiana

1. **Branch** `fix/<short-name>` z `main`
2. **Implementacja** — minimalna zmiana, najlepiej <20 linii diffu
3. **Build na wiacie** — `~/max485-fix` z patched binding/main:
   ```bash
   cd ~/max485-fix
   make so GO=/home/elineshed2002/.local/share/go/bin/go
   npx node-gyp rebuild
   ```
4. **Smoke test** — odczyt z 3 sterowników (slave 21 LED + 31/35 outlets):
   ```bash
   sudo systemctl stop touchapp touchapp-control
   node /home/elineshed2002/test-min.js   # patrz repo: README "manual smoke"
   ```
   Wymagane: wszystkie 3 sterowniki odpowiadają poprawnymi danymi (jak
   przed zmianą).
5. **Deploy do node_modules** — zastąp pliki w
   `/home/elineshed2002/elinetouch/node_modules/max485-raspberry-nodejs/`
   lub przez `npm install` z github tag (po push'u brancha).
6. **Restart serwisów**:
   ```bash
   sudo systemctl reset-failed touchapp touchapp-watchdog
   sudo systemctl start touchapp-control touchapp
   ```
7. **Monitoring 30 min** — porównaj wskaźniki PRZED vs PO:
   - Liczba "invalid response length" w `journalctl -u touchapp-control`
   - Liczba "invalid slave ID" tamże
   - Liczba MODBUS_TIMEOUT w `journalctl -u touchapp`
   - Czy wiata trzyma się jako klient w `emqx ctl clients list`
   - **Brak nowych typów błędów** w logu (regression check)
8. **Decyzja**:
   - ✅ liczba błędów spadła + brak regresji → merge na `main`, tag `v3.0.x`,
     `npm publish` (gdy token), update `elinetouch/package.json`
   - ❌ regresja lub brak poprawy → revert, analiza, pivot

### Quick rollback

Każda zmiana jest publikowana jako osobny tag, więc rollback to:
```bash
cd ~/elinetouch
# Cofnij package.json do poprzedniego tagu max485:
sed -i 's|max485-raspberry-nodejs.*|max485-raspberry-nodejs": "github:chargego-pl/max485-raspberry-nodejs#v3.0.X",|' package.json
npm install
sudo systemctl restart touchapp-control touchapp
```

## Lista zmian

### Priorytet ⭐⭐⭐ (krytyczne)

- [ ] **A. `port.Flush()` przed każdym `sendModbusRequest`**
  - **Plik:** `go/main.go:131` (początek `sendModbusRequest`)
  - **Zmiana:** dodać `d.port.Flush()` jako pierwszą linię metody
  - **Motywacja:** zalegające bajty w RX bufferze z poprzedniego cyklu
    (np. response który nadszedł po timeout) mieszają się z aktualną
    response → "invalid slave ID: got X, expected Y" lub "invalid
    response length"
  - **Spodziewany efekt:** ~80% spadek tych dwóch typów błędów
  - **Acceptance:** journalctl po 30 min ma <20% errorów względem baseline
  - **Wersja docelowa:** v3.0.3
  - **Status:** PENDING

- [ ] **E. `flock()` na otwartym serial port**
  - **Plik:** `go/main.go:50` (po `serial.OpenPort`)
  - **Zmiana:** wymusić `LOCK_EX|LOCK_NB` na fd — drugi proces dostaje
    explicit error zamiast cichej walki o port
  - **Motywacja:** obecny incydent — `services/pro.js` w touchapp ma własną
    instancję `ModbusRTU('/dev/serial0', ...)` równolegle z touch-control →
    dwa drivery na bus → gwarantowany chaos
  - **Blocker:** `tarm/serial.Port` nie wystawia fd → wymaga przejścia na
    `go.bug.st/serial` (też daje `.Drain()` dla zmiany B)
  - **Spodziewany efekt:** twardy fail przy próbie drugiego openu zamiast
    obecnego niewidocznego corruption
  - **Acceptance:** smoke test pokazuje że pojedynczy proces działa; próba
    uruchomienia drugiego z tej samej lib daje czysty error
  - **Wersja docelowa:** v3.1.0 (większy refaktor — switch lib)
  - **Status:** PENDING — czeka na pierwszą walidację `tarm/serial` →
    `go.bug.st/serial`

### Priorytet ⭐⭐ (znaczące)

- [ ] **B. Drain UART po Write, proporcjonalne do długości packet'u**
  - **Plik:** `go/main.go:152-156` (po byte-by-byte write loop, przed
    `enableRX()`)
  - **Zmiana opcja 1 (bez zmiany lib):** zastąpić `postSendDelay = 3ms`
    fixed na `(len(request) * 10 / baudRate) seconds + safety margin`
  - **Zmiana opcja 2 (recommended):** switch `tarm/serial` →
    `go.bug.st/serial`, użyć `port.Drain()` (proper `tcdrain()` syscall)
  - **Motywacja:** kernel UART tx buffer absorbuje cały packet PRZED
    transmisją. `Write` zwraca po copy do bufora, nie po shift-out. Przy
    długich packetach (writeMultipleCoils 30+ bajtów) `enableRX()`
    przełącza DE LOW zanim ostatni bajt wyjdzie z UART → ucięcie końca
    transmisji → slave ignoruje całość → timeout
  - **Spodziewany efekt:** eliminuje rzadkie ale powtarzające się "no
    response" dla long writes
  - **Acceptance:** brak MODBUS_TIMEOUT przy writeMultipleCoils w 30 min
    workloadzie
  - **Wersja docelowa:** v3.0.4 (opcja 1) lub v3.1.0 (opcja 2, razem z E)
  - **Status:** PENDING

### Priorytet ⭐ (poprawa diagnostyki)

- [ ] **C. Rozróżnić timeout od corrupted response**
  - **Plik:** `go/main.go:181-183`
  - **Zmiana:** jeśli `totalRead == 0` → `"MODBUS_TIMEOUT: no response from
    slave N"` zamiast generic "invalid response length"
  - **Motywacja:** modbus-server.js `_withReconnect` ma osobną logikę dla
    timeout vs corruption. Obecnie obie idą tą samą ścieżką z 300ms sleep
    między retry — przy genuine timeout to za krótkie, przy corruption za
    długie
  - **Acceptance:** żadne "got 0, expected N" w nowym logu, zamiast tego
    `MODBUS_TIMEOUT`
  - **Wersja docelowa:** v3.0.5
  - **Status:** PENDING

- [ ] **D. Parse Modbus exception responses**
  - **Plik:** `go/main.go:131-203`
  - **Zmiana:** po przeczytaniu pierwszych 2 bajtów, jeśli
    `response[1] == request[1]|0x80` → exception frame, czytaj jeszcze 3
    bajty (errCode + CRC), zwróć typed error z exception code
  - **Motywacja:** slave odpowiada exception (Illegal Function/Address/Data
    Value) jako 5-bajtowy frame. Obecnie lib oczekuje pełnego
    `expectedLength` → break, "invalid response length", diagnostyka
    stracona
  - **Spodziewany efekt:** lepsza diagnostyka *poza* obecnym zestawem
    błędów. Sam liczbę errorów nie zmieni (większość to corruption/timeout)
  - **Wersja docelowa:** v3.0.6
  - **Status:** PENDING

- [ ] **F. `errors.Is(err, io.EOF)` zamiast string compare**
  - **Plik:** `go/main.go:169`
  - **Zmiana:** `err.Error() == "EOF"` → `errors.Is(err, io.EOF)`
  - **Motywacja:** string compare łamie się jak `tarm/serial` zmieni
    error message. Robustness.
  - **Wersja docelowa:** v3.0.7 lub piggyback na innym fixie
  - **Status:** PENDING

### Niska priorytetu / nice-to-have

- [ ] Configurable timing constants (`postSendDelay`, `byteSendDelay` jako
      params konstruktora) — żeby user mógł tunować bez fork'a libki
- [ ] Bulk write zamiast byte-by-byte (jeśli switch na go.bug.st/serial z
      proper Drain) — performance, ~50% szybszy `sendModbusRequest`
- [ ] Brak unit testów dla `calculateCRC`, packet encoding — łatwe do
      dodania (no hardware required)
- [ ] `rpio.Close()` w `(*ModbusDevice).Close()` zamyka globalne GPIO mapping
      → bug jeśli kiedyś chcielibyśmy >1 instancji w procesie (obecnie N/A)
- [ ] Configurable read timeout (obecnie hardcoded 5s w
      `NewModbusDevice`) — pozwala szybciej fail dla niewspieranych slave'ów

## Historia (zrobione)

- [x] **v3.0.2** (commit `4779094`, 2026-05-21) — `cgo.Handle` dla
      `*ModbusDevice` w napi external. Eliminuje SIGSEGV po ~9h pracy
      gdy Go GC przeniósł obiekt. Plus unifikacja error format w
      `ReadCoilsJS` (był napi error object, teraz string "Error: ..."
      jak inne metody).

## Pliki referencyjne

- `docs/edge-process-architecture.md` w `elinetouch` repo — IPC contract,
  why touch-control owns /dev/serial0 exclusively (relevant dla fix E)
- `docs/wiata-spec-produktu.md` — gdzie są slave'y w bus topology
- Schemat HW: `~/Desktop/pliki projektowe elines/RaspberryPi_Power_Shield_v2/Schematic.pdf`
