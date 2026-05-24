package main

// F1.3 / A16 — exclusive port lock via UNIX lockfile convention.
//
// Linux serial drivery (PL011, USB↔serial) NIE narzucają exclusive access
// na poziomie kernel'a (TIOCEXCL trzeba explicit). Domyślnie dwa procesy mogą
// otworzyć ten sam /dev/serial0 i kolidować się na busie — bus chaos.
//
// Mamy IPC broker (touchapp-control modbus-server.js) który serializuje access
// w produkcji, ale gdyby ktoś omyłkowo uruchomił test.js w drugim shellu —
// race nieoczywisty do zdebugowania (intermittent CRC errors, garbage bytes).
//
// Zaimplementowane przez stary-dobry UNIX lockfile pattern (respektowany
// przez minicom, screen, etc.): /var/lock/LCK..<basename(port)>, treść = PID.
// Stale-detection: jeśli plik istnieje ale PID w nim jest martwy (kill -0
// failed) → reclaim. Jeśli PID żyje → return error.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const lockDir = "/var/lock"

// portLockfilePath zwraca standard UNIX lockfile path dla danego serial portu.
// Konwencja: LCK..<basename(portPath)>. Slashes w nazwie zamienione na "_".
func portLockfilePath(portName string) string {
	base := filepath.Base(portName)
	// gdyby ktoś przekazał coś dziwnego (np. "serial:0"), normalize:
	base = strings.ReplaceAll(base, "/", "_")
	return filepath.Join(lockDir, "LCK.."+base)
}

// acquirePortLock próbuje zaalokować lockfile dla portu.
// - Jeśli już istnieje + zawiera PID żywego procesu → error.
// - Jeśli już istnieje + PID stale → reclaim + write nowy PID.
// - Jeśli nie istnieje → create + write PID.
//
// Zwraca path do utworzonego lockfile (do release) i error.
// Jeśli /var/lock nie istnieje lub nie ma uprawnień zapisu → graceful
// degradation: return ("", nil) — feature jest "best effort", nie hard fail
// dla środowisk gdzie nie ma /var/lock (rzadkie, ale możliwe).
func acquirePortLock(portName string) (string, error) {
	lockPath := portLockfilePath(portName)

	// Sprawdź czy katalog dostępny do zapisu. Jeśli nie — degradacja.
	if _, err := os.Stat(lockDir); err != nil {
		// Brak /var/lock — silent skip (logujemy w debug).
		return "", nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			// Got it. Write PID.
			pid := strconv.Itoa(os.Getpid())
			_, _ = f.WriteString(pid + "\n")
			_ = f.Close()
			return lockPath, nil
		}
		// EEXIST — sprawdź czy stale.
		if !errors.Is(err, os.ErrExist) {
			// Inna usterka (no permission, FS error) — degradacja.
			return "", nil
		}
		if isStaleLock(lockPath) {
			// Stale — usuń i retry (max 2 attempts żeby uniknąć infinite loop).
			_ = os.Remove(lockPath)
			continue
		}
		// Żywy proces trzyma lock — fail.
		owner := readLockOwner(lockPath)
		return "", fmt.Errorf("port %s already locked (lockfile %s, owner pid %s)",
			portName, lockPath, owner)
	}
	return "", fmt.Errorf("port %s lockfile contention (gave up after retry)", portName)
}

func releasePortLock(lockPath string) {
	if lockPath == "" {
		return
	}
	// Sanity check: tylko usuwamy lockfile jeśli zawiera nasze PID (nie kradniemy
	// cudzego po dziwnej race condition).
	owner := readLockOwner(lockPath)
	if owner == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(lockPath)
	}
}

func readLockOwner(lockPath string) string {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func isStaleLock(lockPath string) bool {
	owner := readLockOwner(lockPath)
	if owner == "" {
		return true // pusty/uszkodzony lockfile = stale
	}
	pid, err := strconv.Atoi(owner)
	if err != nil || pid <= 0 {
		return true
	}
	// kill -0 sprawdza czy proces żyje (bez wysyłania sygnału).
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		// errno ESRCH = no such process; EPERM = proces żyje ale nie nasz user
		// (też uznajemy za żywy żeby nie kradnąć).
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
	}
	return false
}
