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

- [x] ~~**A. `port.Flush()` przed każdym `sendModbusRequest`**~~ — **REVERTED**
  - **Plik:** `go/main.go:131` (początek `sendModbusRequest`)
  - **Zmiana:** dodać `d.port.Flush()` jako pierwszą linię metody
  - **Motywacja:** zalegające bajty w RX bufferze z poprzedniego cyklu
    (np. response który nadszedł po timeout) mieszają się z aktualną
    response → "invalid slave ID: got X, expected Y" lub "invalid
    response length"
  - **Wynik testu (2026-05-21, branch `fix/flush-before-send`):**
    | Metryka | Baseline (v3.0.2, 22 min) | Po fix A (21 min) | Zmiana |
    |---|---|---|---|
    | touch-control errors | 17 | 93 | **+447%** |
    | invalid response length | 12 | 74 | +517% |
    | invalid slave ID | 5 | 18 | +260% |
    | touchapp errors | 33 | 120 | +264% |
  - **Hipoteza dlaczego pogorszyło:** `tarm/serial.Flush()` wywołuje
    `tcflush(TCIOFLUSH)` — flushuje OBA kierunki. To prawdopodobnie
    obcina in-flight transmisję (kernel UART tx buffer ma 64-byte FIFO
    który shift-uje przez ~7ms przy 9600 baud). Jeśli następne wywołanie
    Flush dzieje się gdy poprzednia response ledwo wpadła do RX FIFO,
    tcflush(TCIFLUSH) ją wymiata zanim Read zdąży skopiować. Efekt
    netto: WIĘCEJ partial responses zamiast mniej.
  - **Implication:** ten fix wymaga **selektywnego flush'a tylko input
    buffera** (`tcflush(TCIFLUSH)` zamiast `TCIOFLUSH`). `tarm/serial`
    nie wystawia tej granularności — wymaga przejścia na
    `go.bug.st/serial` która ma `ResetInputBuffer()` (tylko RX).
  - **Re-plan:** Fix A musi być powiązany z migracją na `go.bug.st/serial`
    (razem z fix B/E). Pojedynczy fix Flush jest niemożliwy do
    zaimplementowania bezpiecznie z aktualnym tarm/serial.
  - **Status:** REVERTED — wymaga przepisania na inną bibliotekę serial

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

- [x] ~~**C. Rozróżnić timeout od corrupted response**~~ — **REVERTED**
  - **Plik:** `go/main.go:181-183`
  - **Zmiana:** jeśli `totalRead == 0` → `"MODBUS_TIMEOUT: no response from
    slave N"` zamiast generic "invalid response length"
  - **Wynik testu (2026-05-21, branch `fix/timeout-error-message`):**
    | Metryka | Baseline (9.5 min v3.0.2) | Po Fix C (10.5 min) | Zmiana |
    |---|---|---|---|
    | touch-control errors | 11 | 121 | **+1000%** |
    | invalid response length | 8 | 88 | +1000% |
    | invalid slave ID | 3 | 24 | +700% |
    | MODBUS_TIMEOUT (nowy) | 0 | 8 | nowy |
    | touchapp errors | 15 | 152 | +913% |
    | touchapp.service | active | **FAILED** (start-limit) | ALARM |
  - **Diagnoza dlaczego pogorszyło (mimo że to "tylko zmiana stringa"):**
    Po zmianie komunikatu, `modbus-server.js _withReconnect` regex
    `/file already closed|EBADF|ENOENT|timeout/i` zaczął matchować
    `MODBUS_TIMEOUT` → kick'nął retry logic (3 attempts × 300ms sleep).
    Każdy timeout to teraz **3× więcej calls na busie**:
    - przed: 1 call → "invalid response length: got 0" → throw, idziemy dalej
    - po: 1 call → 5s wait → "MODBUS_TIMEOUT" → matchuje retry regex
      → sleep 300ms → call 2 (timeout 5s) → sleep 300ms → call 3 (5s) → throw
    - **15.6s + 3 calls** zamiast 5s + 1 call
    Skutek: bus saturated, każdy timeout blokuje queue na ~15s, w międzyczasie
    inne slave'y nie są odpytywane → ich own pollery (touchapp pro.js, metrics.js)
    też timeoutują → kaskada, touchapp wpada w restart-loop.
  - **Implication:** **fix biblioteki SAM nie wystarczy**. Musi być
    skoordynowany z modbus-server.js:
    - Wariant A: `MODBUS_TIMEOUT` to "give up, throw immediately, no retry"
      (slave realnie milczał 5s — dodatkowe próby tylko marnują czas)
    - Wariant B: dla MODBUS_TIMEOUT retry max 1× zamiast 3, bez sleep
    - Najlepiej: typed error class zamiast regex matching w error message
  - **Status:** REVERTED — wymaga skoordynowanej zmiany w
    `elinetouch/src/control/modbus-server.js` i `services/pro.js`

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

## Meta-lessons learned

Po **trzech** revertach (A, C, oraz pro.js→IPC w elinetouch) z dramatycznymi
regresjami w produkcji:

1. **Każdy "tylko mały fix" w hot path Modbus może mieć non-obvious
   konsekwencje.** Fix C był zmianą stringa — i wywołał 10× regresję
   przez interakcję z regex w modbus-server.js. Lib + caller są
   silnie sprzężone, separation of concerns iluzoryczna.

2. **Hot path obecnie operuje na granicy stabilności.** Każde
   dodatkowe obciążenie (więcej retries, dodatkowy syscall flush,
   więcej delays) wpycha system w spiralę: więcej calls → więcej
   timeouts → więcej retries → kaskada → touchapp restart-loop.

3. **Pojedyncze fixy w libie wymagają skoordynowanej zmiany u
   caller'a.** Następne iteracje muszą obejmować JEDNOCZEŚNIE:
   - lib change (np. typed errors)
   - modbus-server.js change (retry policy per error type)
   - PR ze zmianą obu repos, deploy razem, rollback razem

4. **Workflow "1 fix → test → decide" jest pomocny ale nie wystarcza
   gdy zmiana jest cross-repo.** Trzeba przejść na "feature flag
   + ramp + rollback per slave" lub po prostu większe atomy zmian
   (cały coordinated fix razem).

5. **Magistrala jest bardziej fragile niż zakładaliśmy.** Te ~70/h
   errorów to nie "bug do naprawy" — to **wskaźnik nasycenia bus'a**.
   Każda dodatkowa transmisja zwiększa szansę kolizji. Pierwszą rzeczą
   do zrobienia powinno być **zmniejszenie liczby transmisji**, nie
   ich naprawa.

6. **Wszystkie fixy są PO TYM JAK coś już poszło źle** (timeout,
   corruption, exception). Większy zysk byłby z prevention.

7. **(NOWE po 3-cim revercie)** Fix pro.js→IPC był koncepcyjnie poprawny
   — eliminował podwójnego ownera serial port (lsof potwierdził). ALE:
   spowodował 9× wzrost timeoutów. Diagnoza: pro.js wcześniej miał
   **własną równoległą queue** (chaotic parallel throughput), teraz wszystko
   serializuje się przez jedną queue touch-control (SLEEP_MS=500 + Modbus
   call) → metrics scrape + mqtt report + monitor + IPC callers stoją
   w kolejce → 5s read timeouts strzelają.
   **Wniosek:** nie istnieje "prosty" fix. Każda strukturalna zmiana
   ujawnia inny bottleneck. System jest w stanie nasycenia magistrali —
   wszelkie modyfikacje timing/serialization wpychają go głębiej.

8. **Najpilniejsza prawdziwa potrzeba:** redukcja LICZBY operacji na busie,
   nie ich naprawa. Plan na osobną sesję:
   - **Shared cache w touch-control:** monitor poll co X, cached state,
     IPC clients dostają cached state bez nowego Modbus call dopóki
     świeży (np. TTL 2s dla statuses, 10s dla power, 30s dla LED).
   - **Tuning polling intervals:** metrics scrape rzadziej (co 30s
     zamiast 5s), mqtt report rzadziej (co 60s zamiast 15s).
   - **DOPIERO POTEM** spadek baseline → nowe pole do prób fix lib.

9. **70/h errorów na v3.0.2 to nowy normal.** System operacyjnie działa
   (intent ACK lecą, MQTT trzyma się, UI responsywny). Retry logic
   maskuje większość. Te errors są **wskaźnikiem nasycenia bus**, nie
   service degradation z perspektywy użytkownika.

## Wnioski na dalszą pracę

**Nie ruszać biblioteki przed redukcją base load na magistrali.** Lib jest
napięta — każda zmiana destabilizuje. Najpierw:
1. Shared cache w touch-control (elinetouch, osobny task)
2. Tune polling intervals (elinetouch)
3. **Dopiero potem** wracać do fixów lib (A z `go.bug.st/serial`, B drain,
   E flock — wszystkie razem jako v3.1.0)

## Pliki referencyjne

- `docs/edge-process-architecture.md` w `elinetouch` repo — IPC contract,
  why touch-control owns /dev/serial0 exclusively (relevant dla fix E)
- `docs/wiata-spec-produktu.md` — gdzie są slave'y w bus topology
- Schemat HW: `~/Desktop/pliki projektowe elines/RaspberryPi_Power_Shield_v2/Schematic.pdf`
