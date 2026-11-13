package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Crash-test workload shape: one collection, a fixed number of clients each
// owning a fixed number of keys, and a fixed number of kill/restart rounds.
const (
	collection    = "crash"
	crashClients  = 4
	keysPerClient = 3
	crashRounds   = 6
)

// crashHTTPClient talks to the child server. Its timeout bounds every request,
// so a lost response surfaces as a transport error instead of a hang.
var crashHTTPClient = &http.Client{Timeout: 5 * time.Second}

// TestMain re-enters this binary as the server child when SRAS_CRASH_CHILD is
// set: main() binds, serves and blocks until the test SIGKILLs it. In the
// normal (parent) mode it just runs the tests. flag.Parse is deliberately not
// called here; main's config.Load parses the child's -addr/-data.
func TestMain(m *testing.M) {
	if os.Getenv("SRAS_CRASH_CHILD") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// keyState is the model of one document's state: whether it exists and, if it
// does, its server-side _rev and the client sequence number that produced it.
type keyState struct {
	exists bool
	rev    int64
	seq    int64
}

// putBody is the workload's PUT payload: the revision the client believes is
// current (0 when the document should not exist yet) plus a client-side
// sequence number the server echoes back in the saved document.
type putBody struct {
	Rev int64 `json:"_rev"`
	Seq int64 `json:"seq"`
}

// crashModel tracks every key's last acknowledged state plus the optional
// after-state of an operation whose response was never received. Clients own
// disjoint keys, but the maps are shared, so access is mutex-guarded.
type crashModel struct {
	mu      sync.Mutex
	acked   map[string]keyState
	pending map[string]keyState
}

// newCrashModel returns a model in which every key is absent (rev 0).
func newCrashModel() *crashModel {
	m := &crashModel{
		acked:   make(map[string]keyState, crashClients*keysPerClient),
		pending: make(map[string]keyState),
	}
	for _, key := range allKeys() {
		m.acked[key] = keyState{}
	}
	return m
}

// ackedOf returns the key's last acknowledged state.
func (m *crashModel) ackedOf(key string) keyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked[key]
}

// setAcked records a completed operation's state for the key.
func (m *crashModel) setAcked(key string, st keyState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked[key] = st
}

// setPending records the after-state of an operation whose response was never
// received, so the verifier can accept either outcome for that key.
func (m *crashModel) setPending(key string, st keyState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[key] = st
}

// clearPending forgets all pending after-states, called once the restarted
// server's true state has been observed.
func (m *crashModel) clearPending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = make(map[string]keyState)
}

// syncBuf is a mutex-guarded byte buffer safe for the concurrent writers exec
// starts for the child's stdout and stderr.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// childProc is one run of the server in a separate process, together with a
// channel that closes once the process has been reaped.
type childProc struct {
	cmd     *exec.Cmd
	out     *syncBuf
	waited  chan struct{} // closed after cmd.Wait returned
	waitErr error         // valid only after waited closes
}

// freeAddr returns a free 127.0.0.1 address for the child to bind. The same
// address is reused across restarts.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	// shortcut: another process may grab the port between close and child bind; upgrade to a server-reported address if this ever flakes.
	return addr
}

// startChild launches this test binary again in child mode (TestMain branches
// on SRAS_CRASH_CHILD and calls main()) bound to addr with dataDir. The
// child's stdout and stderr are captured into a race-safe buffer for
// diagnostics. A cleanup SIGKILLs and reaps a still-running child, so a
// failing test never leaves a server process behind.
func startChild(t *testing.T, addr, dataDir string) *childProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-addr", addr, "-data", dataDir)
	// drop ambient SRAS_* config so the child runs with the defaults this
	// test was written against
	cmd.Env = []string{"SRAS_CRASH_CHILD=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SRAS_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child server: %v", err)
	}
	c := &childProc{cmd: cmd, out: out, waited: make(chan struct{})}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.waited)
	}()
	t.Cleanup(func() { stopChild(c) })
	return c
}

// stopChild SIGKILLs the child if it is still running and waits until it has
// been reaped. It is safe to call more than once.
func stopChild(c *childProc) {
	select {
	case <-c.waited:
		return
	default:
	}
	// Signal fails only if the process exited in a race with the check
	// above; the wait below reaps it either way.
	_ = c.cmd.Process.Signal(syscall.SIGKILL)
	<-c.waited
}

// killChild stops the child with SIGKILL, as a crash would, and reports a
// child that died on its own instead of being killed.
func killChild(t *testing.T, c *childProc) {
	t.Helper()
	stopChild(c)
	var ee *exec.ExitError
	isExit := errors.As(c.waitErr, &ee)
	ws, sysOK := syscall.WaitStatus(0), false
	if isExit {
		ws, sysOK = ee.Sys().(syscall.WaitStatus)
	}
	if !isExit || !sysOK || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("child was not killed by SIGKILL: %v\nchild output:\n%s", c.waitErr, c.out.String())
	}
}

// waitReady polls GET /healthz every 10 ms until the child answers 200, and
// fails with the child's captured output if it exits first or is not ready in
// time. A restart that comes up ready is itself the proof that recovery
// accepted the data dir (including a torn active tail).
func waitReady(t *testing.T, c *childProc, addr string) {
	t.Helper()
	url := "http://" + addr + "/healthz"
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		resp, err := crashHTTPClient.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-c.waited:
			t.Fatalf("child exited before becoming ready: %v\nchild output:\n%s", c.waitErr, c.out.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child not ready after 10s\nchild output:\n%s", c.out.String())
}

// keyName returns the id of key j, which client c owns exclusively.
func keyName(c, j int) string {
	return fmt.Sprintf("c%d-k%d", c, j)
}

// allKeys lists every key the workload touches, in a stable order.
func allKeys() []string {
	keys := make([]string, 0, crashClients*keysPerClient)
	for c := 0; c < crashClients; c++ {
		for j := 0; j < keysPerClient; j++ {
			keys = append(keys, keyName(c, j))
		}
	}
	return keys
}

// intField reads one JSON number field as an int64; JSON decodes numbers as
// float64, so an absent or non-integral field reports not-ok.
func intField(doc map[string]any, name string) (int64, bool) {
	f, ok := doc[name].(float64)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// docRevSeq extracts _rev and seq from a saved document.
func docRevSeq(doc map[string]any) (rev, seq int64, ok bool) {
	rev, ok = intField(doc, "_rev")
	if !ok {
		return 0, 0, false
	}
	seq, ok = intField(doc, "seq")
	return rev, seq, ok
}

// clientLoop runs the workload for client c: each iteration picks one of the
// keys the client owns and does one PUT or DELETE against the child. counter
// points at the client's operation counter, which lives for the whole test so
// every seq is unique per key across rounds (a delete/recreate restarts _rev
// at 1, so only seq distinguishes the incarnations). The loop ends when the
// round is over (done closed), after a failed expectation, or once a transport
// error leaves an operation in flight.
func clientLoop(t *testing.T, done <-chan struct{}, addr string, c int, counter *int64, m *crashModel, acked *atomic.Int64) {
	for {
		select {
		case <-done:
			return
		default:
		}
		*counter++
		key := keyName(c, rand.IntN(keysPerClient))
		st := m.ackedOf(key)
		if st.exists && rand.Float64() >= 0.75 {
			if !clientDelete(t, addr, key, st, m, acked) {
				return
			}
		} else if !clientPut(t, addr, key, st, *counter, m, acked) {
			return
		}
	}
}

// clientPut sends one PUT for key and updates the model from the response. It
// reports false when the client must stop for the round: a transport error
// leaves the operation in flight and its after-state pending — including a 2xx
// whose body cannot be fully read, which is an incomplete response, not an
// acknowledged one. Only a non-2xx status, a malformed fully-read body, or a
// wrong _rev/seq in it fails the test.
func clientPut(t *testing.T, addr, key string, st keyState, seq int64, m *crashModel, acked *atomic.Int64) bool {
	t.Helper()
	body, _ := json.Marshal(putBody{Rev: st.rev, Seq: seq})
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("http://%s/db/%s/%s", addr, collection, key), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := crashHTTPClient.Do(req)
	if err != nil {
		// The write may or may not have been applied before the child
		// died; its after-state becomes pending and the verifier accepts
		// either outcome for the key.
		m.setPending(key, keyState{exists: true, rev: st.rev + 1, seq: seq})
		return false
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		t.Errorf("PUT %s: status %d, want 200 or 201", key, resp.StatusCode)
		return false
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		// The connection broke mid-body: the response is incomplete, so
		// the operation is in flight like any other transport error.
		m.setPending(key, keyState{exists: true, rev: st.rev + 1, seq: seq})
		return false
	}
	var saved map[string]any
	if err := json.Unmarshal(respBody, &saved); err != nil {
		t.Errorf("PUT %s: unreadable saved document (status %d): %v (body %q)", key, resp.StatusCode, err, respBody)
		return false
	}
	rev, gotSeq, ok := docRevSeq(saved)
	if !ok {
		t.Errorf("PUT %s: saved document (status %d) lacks integral _rev and seq: %v", key, resp.StatusCode, saved)
		return false
	}
	if rev != st.rev+1 || gotSeq != seq {
		t.Errorf("PUT %s: response has _rev=%d seq=%d, want _rev=%d seq=%d", key, rev, gotSeq, st.rev+1, seq)
		return false
	}
	m.setAcked(key, keyState{exists: true, rev: st.rev + 1, seq: seq})
	acked.Add(1)
	return true
}

// clientDelete sends one DELETE with a revision precondition for key and
// updates the model from the response. It reports false when the client must
// stop for the round, like clientPut.
func clientDelete(t *testing.T, addr, key string, st keyState, m *crashModel, acked *atomic.Int64) bool {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://%s/db/%s/%s?rev=%d", addr, collection, key, st.rev), nil)
	resp, err := crashHTTPClient.Do(req)
	if err != nil {
		m.setPending(key, keyState{}) // after-state: the document is gone
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE %s: status %d, want 204", key, resp.StatusCode)
		return false
	}
	m.setAcked(key, keyState{})
	acked.Add(1)
	return true
}

// getState reads one key from the restarted server and translates it into the
// model's terms: 404 is an absent document, 200 is an existing one identified
// by its _rev and the seq that last wrote it. It reports false (after failing
// the test) when the response is unusable, leaving the model unchanged.
func getState(t *testing.T, addr, key string) (keyState, bool) {
	t.Helper()
	resp, err := crashHTTPClient.Get(fmt.Sprintf("http://%s/db/%s/%s", addr, collection, key))
	if err != nil {
		t.Errorf("GET %s: %v", key, err)
		return keyState{}, false
	}
	var doc map[string]any
	decErr := json.NewDecoder(resp.Body).Decode(&doc)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return keyState{}, true
	case http.StatusOK:
		if decErr != nil {
			t.Errorf("GET %s: decode body: %v", key, decErr)
			return keyState{}, false
		}
		if id, _ := doc["_id"].(string); id != key {
			t.Errorf("GET %s: response carries _id %q", key, id)
		}
		rev, seq, ok := docRevSeq(doc)
		if !ok {
			t.Errorf("GET %s: document %v lacks integral _rev and seq", key, doc)
			return keyState{}, false
		}
		return keyState{exists: true, rev: rev, seq: seq}, true
	default:
		t.Errorf("GET %s: status %d", key, resp.StatusCode)
		return keyState{}, false
	}
}

// verifyRound compares every key's state on the restarted server with the
// model: the observed state must equal the last acknowledged state or, for a
// key with an operation left in flight, that operation's after-state. The
// model then adopts the observed state for the next round.
func verifyRound(t *testing.T, addr string, m *crashModel) {
	t.Helper()
	for _, key := range allKeys() {
		observed, ok := getState(t, addr, key)
		if !ok {
			continue
		}
		m.mu.Lock()
		want := m.acked[key]
		pending, inFlight := m.pending[key]
		m.mu.Unlock()
		matched := observed == want
		if inFlight && observed == pending {
			matched = true
		}
		if !matched {
			t.Errorf("key %s: observed %+v, acked %+v, pending %+v (set: %t)", key, observed, want, pending, inFlight)
		}
		m.setAcked(key, observed)
	}
	m.clearPending()
}

// TestCrashRecovery proves that a server killed with SIGKILL mid-write
// restarts on the same data dir with every acknowledged write intact, while
// each unacknowledged in-flight operation is left in either its before or its
// after state. Six rounds of concurrent load, kill, restart and verification
// run against one shared data dir; a run with fewer than 100 acknowledged
// operations cannot pass.
func TestCrashRecovery(t *testing.T) {
	dataDir := t.TempDir()
	addr := freeAddr(t)
	m := newCrashModel()
	var acked atomic.Int64
	// One op counter per client, living for the whole test: _rev restarts at 1
	// after a delete/recreate, so only a never-repeating seq keeps the
	// (exists, rev, seq) tuple unique per key across rounds.
	counters := make([]int64, crashClients)

	child := startChild(t, addr, dataDir)
	waitReady(t, child, addr)

	for round := 1; round <= crashRounds; round++ {
		before := acked.Load()
		done := make(chan struct{})
		var wg sync.WaitGroup
		for c := 0; c < crashClients; c++ {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				clientLoop(t, done, addr, c, &counters[c], m, &acked)
			}(c)
		}
		time.Sleep(time.Duration(20+rand.Int64N(181)) * time.Millisecond)
		killChild(t, child)
		close(done)
		wg.Wait()

		child = startChild(t, addr, dataDir)
		waitReady(t, child, addr)
		verifyRound(t, addr, m)
		t.Logf("round %d: %d acked ops (total %d)", round, acked.Load()-before, acked.Load())
	}

	if total := acked.Load(); total < 100 {
		t.Errorf("only %d acknowledged ops across %d rounds, want at least 100", total, crashRounds)
	}
}
