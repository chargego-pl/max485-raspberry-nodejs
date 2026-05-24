package main

// F3.4 / A10 — Stats / observability API.
//
// Counters dla bus operations: per FC, per result, per slave. Consumer
// (touchapp / Prometheus exporter) okresowo woła device.Stats() i publishuje
// jako metryki do VictoriaMetrics.

import (
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	OpsTotal       uint64               `json:"opsTotal"`
	OpsByResult    map[string]uint64    `json:"opsByResult"`
	OpsBySlave     map[byte]*SlaveStats `json:"opsBySlave"`
	LastTxUnixNano int64                `json:"lastTxUnixNano"`
	LastRxUnixNano int64                `json:"lastRxUnixNano"`
}

type SlaveStats struct {
	Ops             uint64 `json:"ops"`
	Successes       uint64 `json:"successes"`
	Timeouts        uint64 `json:"timeouts"`
	CRCErrors       uint64 `json:"crcErrors"`
	Exceptions      uint64 `json:"exceptions"`
	IOErrors        uint64 `json:"ioErrors"`
	SumLatencyMicro uint64 `json:"sumLatencyMicro"` // policz średnią po stronie consumera
}

type atomicStats struct {
	opsTotal       uint64
	successCnt     uint64
	timeoutCnt     uint64
	crcCnt         uint64
	exceptionCnt   uint64
	ioErrCnt       uint64
	lastTxUnixNano int64
	lastRxUnixNano int64

	perSlaveMu sync.Mutex
	perSlave   map[byte]*SlaveStats
}

func newAtomicStats() *atomicStats {
	return &atomicStats{perSlave: map[byte]*SlaveStats{}}
}

// Result tag dla record(). Stałe by uniknąć string allocations w hot path.
const (
	resultSuccess   = "success"
	resultTimeout   = "timeout"
	resultCRC       = "crc_error"
	resultException = "exception"
	resultIO        = "io_error"
)

func classifyResult(err error) string {
	if err == nil {
		return resultSuccess
	}
	if _, ok := err.(*ModbusException); ok {
		return resultException
	}
	// Crude string-based classification — wystarczy do telemetrii.
	msg := err.Error()
	if containsAny(msg, "timeout") {
		return resultTimeout
	}
	if containsAny(msg, "CRC", "crc") {
		return resultCRC
	}
	return resultIO
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}

func (s *atomicStats) markTx() {
	atomic.StoreInt64(&s.lastTxUnixNano, time.Now().UnixNano())
}

func (s *atomicStats) markRx() {
	atomic.StoreInt64(&s.lastRxUnixNano, time.Now().UnixNano())
}

func (s *atomicStats) record(slaveID byte, err error, elapsed time.Duration) {
	result := classifyResult(err)
	atomic.AddUint64(&s.opsTotal, 1)
	switch result {
	case resultSuccess:
		atomic.AddUint64(&s.successCnt, 1)
	case resultTimeout:
		atomic.AddUint64(&s.timeoutCnt, 1)
	case resultCRC:
		atomic.AddUint64(&s.crcCnt, 1)
	case resultException:
		atomic.AddUint64(&s.exceptionCnt, 1)
	case resultIO:
		atomic.AddUint64(&s.ioErrCnt, 1)
	}

	s.perSlaveMu.Lock()
	ss, ok := s.perSlave[slaveID]
	if !ok {
		ss = &SlaveStats{}
		s.perSlave[slaveID] = ss
	}
	ss.Ops++
	switch result {
	case resultSuccess:
		ss.Successes++
	case resultTimeout:
		ss.Timeouts++
	case resultCRC:
		ss.CRCErrors++
	case resultException:
		ss.Exceptions++
	case resultIO:
		ss.IOErrors++
	}
	ss.SumLatencyMicro += uint64(elapsed.Microseconds())
	s.perSlaveMu.Unlock()
}

func (s *atomicStats) snapshot() Stats {
	out := Stats{
		OpsTotal: atomic.LoadUint64(&s.opsTotal),
		OpsByResult: map[string]uint64{
			resultSuccess:   atomic.LoadUint64(&s.successCnt),
			resultTimeout:   atomic.LoadUint64(&s.timeoutCnt),
			resultCRC:       atomic.LoadUint64(&s.crcCnt),
			resultException: atomic.LoadUint64(&s.exceptionCnt),
			resultIO:        atomic.LoadUint64(&s.ioErrCnt),
		},
		OpsBySlave:     map[byte]*SlaveStats{},
		LastTxUnixNano: atomic.LoadInt64(&s.lastTxUnixNano),
		LastRxUnixNano: atomic.LoadInt64(&s.lastRxUnixNano),
	}
	s.perSlaveMu.Lock()
	for k, v := range s.perSlave {
		cp := *v
		out.OpsBySlave[k] = &cp
	}
	s.perSlaveMu.Unlock()
	return out
}
