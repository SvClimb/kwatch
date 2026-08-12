package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureWriter counts Write calls and accumulates all output.
type captureWriter struct {
	mu  sync.Mutex
	n   int
	buf bytes.Buffer
}

func (cw *captureWriter) Write(p []byte) (int, error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.n++
	return cw.buf.Write(p)
}

func (cw *captureWriter) Writes() int {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.n
}

func (cw *captureWriter) String() string {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.buf.String()
}

// newTestDiagnoser creates a Diagnoser with mock fetch functions so tests
// never require a running Kubernetes cluster.
func newTestDiagnoser(grace time.Duration) *Diagnoser {
	d := NewDiagnoser(context.Background(), nil, "", grace, 50)
	d.logsFunc = func(podName, namespace string) string { return "log line one\nlog line two\n" }
	d.eventsFunc = func(podName, namespace string) string { return "" }
	return d
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// ── statusFamily mapping ──────────────────────────────────────────────────────

func TestStatusFamily(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"CrashLoopBackOff", "crash"},
		{"Error", "crash"},
		{"OOMKilled", "crash"},
		{"ImagePullBackOff", "imagepull"},
		{"ErrImagePull", "imagepull"},
		{"CreateContainerError", "config"},
		{"CreateContainerConfigError", "config"},
		{"InvalidImageName", "config"},
		{"Evicted", "evicted"},
		// Healthy statuses must NOT map to a diagnosable family.
		{"Running", ""},
		{"Pending", ""},
		{"Terminating", ""},
		{"Completed", ""},
		{"ContainerCreating", ""},
		{"PodInitializing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			if got := statusFamily[tc.status]; got != tc.want {
				t.Errorf("statusFamily[%q] = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// ── familyFor: init container statuses ───────────────────────────────────────

func TestFamilyFor_InitContainerErrors(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"Init:CrashLoopBackOff", "init-crash"},
		{"Init:Error", "init-crash"},
		{"Init:OOMKilled", "init-crash"},
		{"Init:ImagePullBackOff", "init-imagepull"},
		{"Init:ErrImagePull", "init-imagepull"},
		// Progress states must not be diagnosable.
		{"Init:0/3", ""},
		{"Init:1/2", ""},
		// Plain statuses must still resolve correctly via familyFor.
		{"CrashLoopBackOff", "crash"},
		{"Running", ""},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			if got := familyFor(tc.status); got != tc.want {
				t.Errorf("familyFor(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestInitContainerError_Diagnosed(t *testing.T) {
	// A pod stuck in Init:CrashLoopBackOff must trigger a diagnosis block.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "Init:CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	out := w.String()
	if !strings.Contains(out, "pod-a") {
		t.Errorf("diagnosis output missing pod name:\n%s", out)
	}
	if !strings.Contains(out, "Init:CrashLoopBackOff") {
		t.Errorf("diagnosis output missing status:\n%s", out)
	}
}

func TestInitContainerError_SameFamilyDedup(t *testing.T) {
	// Init:Error and Init:CrashLoopBackOff are in the same family — only one block.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "Init:Error", w)
	d.Check("pod-a", "", "Init:CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() >= 1 })
	time.Sleep(grace * 3)

	if n := w.Writes(); n != 1 {
		t.Errorf("expected 1 diagnosis block for same init family, got %d", n)
	}
}

// ── NoteRestarts: restart diagnosis ──────────────────────────────────────────

func TestNoteRestarts_TriggersDiagnosis(t *testing.T) {
	// When restart count increases, NoteRestarts must emit both the ↺ line and
	// a full diagnosis block with logs from the previous container.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	d.logsFunc = func(podName, namespace string) string { return "previous crash log\n" }
	w := &captureWriter{}

	d.NoteRestarts("pod-a", "", "1", w) // first observation — no previous count
	d.NoteRestarts("pod-a", "", "2", w) // restart detected: 1 → 2
	waitFor(t, grace*10, func() bool {
		return strings.Contains(w.String(), "previous crash log")
	})

	out := w.String()
	if !strings.Contains(out, "restarted") {
		t.Errorf("expected ↺ restart line in output:\n%s", out)
	}
	if !strings.Contains(out, "RESTART DETECTED") {
		t.Errorf("expected RESTART DETECTED header in output:\n%s", out)
	}
}

func TestNoteRestarts_DeduplicatesRapidRestarts(t *testing.T) {
	// Multiple restarts within the grace period must produce exactly one diagnosis
	// block (the ↺ informational line appears for every restart, but the expensive
	// fetch is suppressed).
	//
	// grace=100ms, 4 restarts fired immediately → only the first triggers diagnosis.
	const grace = 100 * time.Millisecond
	d := newTestDiagnoser(grace)
	d.logsFunc = func(podName, namespace string) string { return "log\n" }
	w := &captureWriter{}

	d.NoteRestarts("pod-a", "", "1", w) // baseline
	d.NoteRestarts("pod-a", "", "2", w) // triggers diagnosis
	d.NoteRestarts("pod-a", "", "3", w) // within grace — suppressed
	d.NoteRestarts("pod-a", "", "4", w) // within grace — suppressed

	waitFor(t, grace*5, func() bool {
		return strings.Contains(w.String(), "RESTART DETECTED")
	})
	time.Sleep(grace / 2) // ensure no late second block arrives

	if n := strings.Count(w.String(), "RESTART DETECTED"); n != 1 {
		t.Errorf("expected 1 restart diagnosis block for rapid restarts, got %d", n)
	}
}

func TestDiagnoser_ConcurrencyLimit(t *testing.T) {
	// With maxConcurrentDiagnoses=5, at most 5 diagnoses run simultaneously.
	// Verify that a burst of 20 pods all crashing at once still completes
	// without deadlock and produces exactly 20 diagnosis blocks.
	const grace = 5 * time.Millisecond
	d := newTestDiagnoser(grace)
	d.logsFunc = func(podName, namespace string) string {
		time.Sleep(20 * time.Millisecond) // simulate slow API
		return "log\n"
	}
	w := &captureWriter{}

	const pods = 20
	for i := range pods {
		d.Check(fmt.Sprintf("pod-%d", i), "", "Error", w)
	}
	waitFor(t, 10*time.Second, func() bool {
		return strings.Count(w.String(), "CRASH DETECTED") == pods
	})
}

// ── grace period ──────────────────────────────────────────────────────────────

func TestGracePeriod_TransientErrorIgnored(t *testing.T) {
	// Pod enters Error but recovers before the grace period expires.
	// Expected: zero diagnosis blocks.
	//
	// grace=100ms, sleep=10ms gives a 10x ratio — robust against OS scheduling
	// jitter that previously caused this test to flake at grace/4=15ms.
	const grace = 100 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	time.Sleep(10 * time.Millisecond) // recover well within grace window
	d.Check("pod-a", "", "Running", w)
	time.Sleep(grace * 3) // wait past grace period

	if n := w.Writes(); n != 0 {
		t.Errorf("expected 0 diagnosis blocks for transient error, got %d", n)
	}
}

func TestGracePeriod_PersistentErrorDiagnosed(t *testing.T) {
	// Pod stays in error past the grace period.
	// Expected: exactly one diagnosis block.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })
}

// ── family deduplication ──────────────────────────────────────────────────────

func TestFamilyDedup_SameFamilyProducesOneBlock(t *testing.T) {
	// Error and CrashLoopBackOff are both in family "crash".
	// Expected: only one diagnosis block regardless of order.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	d.Check("pod-a", "", "CrashLoopBackOff", w) // same family — must be suppressed
	waitFor(t, grace*10, func() bool { return w.Writes() >= 1 })
	time.Sleep(grace * 3) // ensure no delayed second write arrives

	if n := w.Writes(); n != 1 {
		t.Errorf("expected 1 diagnosis block for same family, got %d", n)
	}
}

func TestFamilyDedup_DifferentFamiliesEachDiagnosed(t *testing.T) {
	// imagepull failure → recovery → crash failure: two distinct diagnoses.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	// First error cycle: image pull
	d.Check("pod-a", "", "ImagePullBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	// Genuine recovery: Running for longer than grace period
	d.Check("pod-a", "", "Running", w)
	time.Sleep(grace * 3) // recovery timer fires, seen cleared

	// Second error cycle: different family
	d.Check("pod-a", "", "CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 2 })
}

// ── recovery timer ────────────────────────────────────────────────────────────

func TestRecoveryTimer_BriefRunningDoesNotResetFamily(t *testing.T) {
	// Crash loop: Error → diagnosed → brief Running → Error again.
	// The brief Running must not clear seen, so the second Error is suppressed.
	//
	// grace=50ms, sleep=5ms gives a 10x ratio — robust against OS jitter.
	const grace = 50 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	// First crash diagnosed
	d.Check("pod-a", "", "Error", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	// Brief Running (inside crash loop — shorter than grace period)
	d.Check("pod-a", "", "Running", w)
	time.Sleep(5 * time.Millisecond)

	// Crash again — same crash loop iteration
	d.Check("pod-a", "", "CrashLoopBackOff", w)
	time.Sleep(grace * 3) // wait to confirm no second block

	if n := w.Writes(); n != 1 {
		t.Errorf("expected 1 diagnosis block (same crash loop), got %d", n)
	}
}

func TestRecoveryTimer_SustainedRunningClearsFamily(t *testing.T) {
	// Pod genuinely recovers (Running for > grace period), then crashes again.
	// Expected: second crash produces a new diagnosis block.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	// First crash
	d.Check("pod-a", "", "CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	// Genuine recovery
	d.Check("pod-a", "", "Running", w)
	time.Sleep(grace * 3) // recovery timer fires, seen cleared

	// New crash — should be diagnosed independently
	d.Check("pod-a", "", "Error", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 2 })
}

// ── healthy statuses ──────────────────────────────────────────────────────────

func TestHealthyStatuses_NeverDiagnosed(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	healthy := []string{"Running", "Pending", "ContainerCreating",
		"PodInitializing", "Completed", "Terminating"}
	for _, s := range healthy {
		d.Check("pod-a", "", s, w)
	}
	time.Sleep(grace * 3)

	if n := w.Writes(); n != 0 {
		t.Errorf("expected 0 diagnosis blocks for healthy statuses, got %d", n)
	}
}

// ── output content ────────────────────────────────────────────────────────────

func TestDiagnosis_ContainsLogOutput(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	d.logsFunc = func(podName, namespace string) string { return "panic: nil pointer dereference\n" }
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	if !strings.Contains(w.String(), "panic: nil pointer dereference") {
		t.Errorf("diagnosis output missing log content:\n%s", w.String())
	}
}

func TestDiagnosis_ContainsEventOutput(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	d.logsFunc = func(podName, namespace string) string { return "" }
	d.eventsFunc = func(podName, namespace string) string {
		return "Warning   BackOff   Back-off restarting failed container\n"
	}
	w := &captureWriter{}

	d.Check("pod-a", "", "CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	if !strings.Contains(w.String(), "Back-off restarting") {
		t.Errorf("diagnosis output missing events:\n%s", w.String())
	}
}

func TestDiagnosis_ContainsPodNameAndStatus(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("my-service-abc123", "", "OOMKilled", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	out := w.String()
	if !strings.Contains(out, "my-service-abc123") {
		t.Errorf("diagnosis output missing pod name:\n%s", out)
	}
	if !strings.Contains(out, "OOMKilled") {
		t.Errorf("diagnosis output missing status:\n%s", out)
	}
}

// ── pod isolation ─────────────────────────────────────────────────────────────

func TestMultiplePods_DiagnosedIndependently(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	d.Check("pod-b", "", "CrashLoopBackOff", w)
	d.Check("pod-c", "", "ImagePullBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 3 })
}

func TestMultiplePods_OneRecoveryDoesNotAffectOthers(t *testing.T) {
	// pod-a recovers within grace period; pod-b stays in error.
	// Expected: only pod-b gets diagnosed.
	//
	// grace=100ms, sleep=10ms — see TestGracePeriod_TransientErrorIgnored.
	const grace = 100 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	d.Check("pod-b", "", "Error", w)

	time.Sleep(10 * time.Millisecond)
	d.Check("pod-a", "", "Running", w) // pod-a recovers

	waitFor(t, grace*5, func() bool { return w.Writes() == 1 })
	time.Sleep(grace) // confirm no second write for pod-a

	if n := w.Writes(); n != 1 {
		t.Errorf("expected 1 diagnosis block (pod-b only), got %d", n)
	}
	if !strings.Contains(w.String(), "pod-b") {
		t.Errorf("expected diagnosis for pod-b, got:\n%s", w.String())
	}
}

// ── namespace isolation (-A mode) ─────────────────────────────────────────────

func TestNamespaceIsolation_SameNameDifferentNamespace(t *testing.T) {
	// Two pods with the same name in different namespaces must be tracked
	// independently. Diagnosing one must not suppress the other.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	d.Check("app", "ns-a", "Error", w)
	d.Check("app", "ns-b", "CrashLoopBackOff", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 2 })
}

// ── terminal escape injection ─────────────────────────────────────────────────

func TestDiagnosis_StripControlSequencesFromLogs(t *testing.T) {
	// A malicious container can print ANSI escape sequences to its log output.
	// kwatch must strip them before writing to the terminal.
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	d.logsFunc = func(podName, namespace string) string {
		return "\x1b[2Jclean text\x1b]0;evil title\x07more text\x1b[?1049h"
	}
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	waitFor(t, grace*10, func() bool { return w.Writes() == 1 })

	out := w.String()
	if strings.ContainsAny(out, "\x1b") {
		t.Errorf("diagnosis output must not contain escape sequences:\n%q", out)
	}
	if !strings.Contains(out, "clean text") || !strings.Contains(out, "more text") {
		t.Errorf("legitimate log content must survive stripping:\n%s", out)
	}
}

// ── context cancellation ──────────────────────────────────────────────────────

func TestContextCancellation_PendingDiagnosisDropped(t *testing.T) {
	// When the context is cancelled before the grace period fires,
	// no diagnosis block should be written.
	const grace = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	d := NewDiagnoser(ctx, nil, "", grace, 50)
	d.logsFunc = func(string, string) string { return "logs" }
	d.eventsFunc = func(string, string) string { return "" }
	w := &captureWriter{}

	d.Check("pod-a", "", "Error", w)
	cancel() // cancel before grace period expires
	time.Sleep(grace * 3)

	if n := w.Writes(); n != 0 {
		t.Errorf("expected 0 diagnosis blocks after context cancel, got %d", n)
	}
}
