package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatih/color"
)

func init() {
	// Disable ANSI escape codes globally in tests so string comparisons
	// work on plain text without worrying about color sequences.
	color.NoColor = true
}

// ── statusColor ───────────────────────────────────────────────────────────────

func TestStatusColor_PreservesText(t *testing.T) {
	// With color.NoColor=true every color function is a no-op, so the input
	// string must appear verbatim in the output.
	statuses := []string{
		"Running", "Pending", "ContainerCreating", "PodInitializing",
		"Init:0/1", "Init:1/2",
		"Terminating", "Completed",
		"Error", "CrashLoopBackOff", "OOMKilled",
		"ImagePullBackOff", "ErrImagePull",
		"InvalidImageName", "CreateContainerError", "CreateContainerConfigError",
		"ErrSomethingNew", // Err-prefix catch-all
		"UnknownCustomStatus",
	}
	for _, s := range statuses {
		t.Run(s, func(t *testing.T) {
			got := statusColor(s)
			if !strings.Contains(got, s) {
				t.Errorf("statusColor(%q) = %q, does not contain original status", s, got)
			}
		})
	}
}

// ── printColorized: header detection ─────────────────────────────────────────

func TestPrintColorized_DetectsHeader(t *testing.T) {
	input := "NAME     READY   STATUS    RESTARTS   AGE\n" +
		"my-pod   1/1     Running   0          5m\n"

	var out bytes.Buffer
	printColorized(strings.NewReader(input), &out, nil)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected ≥2 output lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "STATUS") {
		t.Errorf("first output line should be the header, got: %q", lines[0])
	}
}

func TestPrintColorized_NoStatusColumn_Passthrough(t *testing.T) {
	// kubectl get deployments has no STATUS column — output must pass through verbatim.
	input := "NAME    READY   UP-TO-DATE   AVAILABLE   AGE\n" +
		"nginx   3/3     3            3           2m\n"

	var out bytes.Buffer
	printColorized(strings.NewReader(input), &out, nil)

	for _, line := range strings.Split(strings.TrimRight(input, "\n"), "\n") {
		if !strings.Contains(out.String(), line) {
			t.Errorf("passthrough line missing from output: %q", line)
		}
	}
}

// ── printColorized: status column ────────────────────────────────────────────

func TestPrintColorized_StatusPresentInOutput(t *testing.T) {
	statuses := []string{
		"Running", "Pending", "ContainerCreating",
		"CrashLoopBackOff", "Error", "OOMKilled",
		"ImagePullBackOff", "ErrImagePull", "Terminating", "Completed",
	}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			padded := status + strings.Repeat(" ", 25-len(status))
			input := "NAME     READY   STATUS                    RESTARTS   AGE\n" +
				"my-pod   1/1     " + padded + "0          5m\n"

			var out bytes.Buffer
			printColorized(strings.NewReader(input), &out, nil)

			if !strings.Contains(out.String(), status) {
				t.Errorf("output missing status %q:\n%s", status, out.String())
			}
		})
	}
}

func TestPrintColorized_StructurePreserved(t *testing.T) {
	// All non-status fields (name, ready, restarts, age) must remain in output.
	input := "NAME              READY   STATUS    RESTARTS   AGE\n" +
		"payment-svc-abc   2/3     Running   4          12m\n"

	var out bytes.Buffer
	printColorized(strings.NewReader(input), &out, nil)
	got := out.String()

	for _, field := range []string{"payment-svc-abc", "2/3", "Running", "4", "12m"} {
		if !strings.Contains(got, field) {
			t.Errorf("output missing field %q:\n%s", field, got)
		}
	}
}

// ── findColumnOffset ──────────────────────────────────────────────────────────

func TestFindColumnOffset_StandaloneWord(t *testing.T) {
	// "NAMESPACE   NAME   STATUS": NAME must match at 12, NOT at 0 inside NAMESPACE.
	header := "NAMESPACE   NAME   READY   STATUS"

	if got := findColumnOffset(header, "NAMESPACE"); got != 0 {
		t.Errorf("NAMESPACE: got %d, want 0", got)
	}
	nameOff := findColumnOffset(header, "NAME")
	if nameOff == 0 {
		t.Error("NAME matched at 0 — matched inside NAMESPACE (word-boundary check failed)")
	}
	if nameOff < 0 {
		t.Error("NAME not found at all")
	}
	if got := findColumnOffset(header, "STATUS"); got < 0 {
		t.Error("STATUS not found")
	}

	// Normal (non -A) mode: NAME at position 0.
	simple := "NAME   READY   STATUS"
	if got := findColumnOffset(simple, "NAME"); got != 0 {
		t.Errorf("NAME at start of line: got %d, want 0", got)
	}

	// Absent column.
	if got := findColumnOffset("NAME   READY", "STATUS"); got != -1 {
		t.Errorf("absent STATUS: got %d, want -1", got)
	}
}

// ── printColorized: all-namespaces format ─────────────────────────────────────

func TestPrintColorized_AllNamespacesFormat(t *testing.T) {
	// With -A, kubectl prefixes each row with NAMESPACE — output must contain
	// all expected fields.
	input := "NAMESPACE   NAME              READY   STATUS    RESTARTS   AGE\n" +
		"default     payment-svc-abc   1/1     Running   0          5m\n" +
		"staging     api-gateway-xyz   0/1     Error     3          2m\n"

	var out bytes.Buffer
	printColorized(strings.NewReader(input), &out, nil)
	got := out.String()

	for _, want := range []string{"payment-svc-abc", "Running", "api-gateway-xyz", "Error"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q in -A format:\n%s", want, got)
		}
	}
}

func TestPrintColorized_AllNamespacesExtractsPodName(t *testing.T) {
	// The diagnoser must receive the pod name, not the namespace name.
	// Before the nameOffset fix, "production" was passed instead of "my-pod".
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	input := "NAMESPACE    NAME     READY   STATUS    RESTARTS   AGE\n" +
		"production   my-pod   0/1     Error     3          1m\n"

	printColorized(strings.NewReader(input), w, d)
	waitFor(t, grace*10, func() bool {
		return strings.Contains(w.String(), "CRASH DETECTED")
	})

	if !strings.Contains(w.String(), "my-pod") {
		t.Errorf("diagnosis must reference pod name 'my-pod', got:\n%s", w.String())
	}
}

// ── printColorized: diagnoser integration ────────────────────────────────────

func TestPrintColorized_TriggersDiagnoserForErrorStatus(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	// captureWriter is mutex-protected — safe for concurrent printer + diagnoser writes.
	w := &captureWriter{}

	input := "NAME     READY   STATUS             RESTARTS   AGE\n" +
		"my-pod   0/1     CrashLoopBackOff   3          1m\n"

	printColorized(strings.NewReader(input), w, d)
	waitFor(t, grace*10, func() bool {
		return strings.Contains(w.String(), "CRASH DETECTED")
	})

	if !strings.Contains(w.String(), "my-pod") {
		t.Errorf("diagnoser output should reference the pod name:\n%s", w.String())
	}
}

func TestPrintColorized_DoesNotTriggerDiagnoserForRunning(t *testing.T) {
	const grace = 20 * time.Millisecond
	d := newTestDiagnoser(grace)
	w := &captureWriter{}

	input := "NAME     READY   STATUS    RESTARTS   AGE\n" +
		"my-pod   1/1     Running   0          5m\n"

	printColorized(strings.NewReader(input), w, d)
	time.Sleep(grace * 3)

	if strings.Contains(w.String(), "CRASH DETECTED") {
		t.Errorf("diagnoser must not fire for Running pods, got:\n%s", w.String())
	}
}

// ── syncWriter ────────────────────────────────────────────────────────────────

func TestSyncWriter_SerializesWrites(t *testing.T) {
	var buf bytes.Buffer
	sw := &syncWriter{w: &buf}

	const goroutines = 50
	var wg sync.WaitGroup // imported via diagnose_test.go (same package)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = sw.Write([]byte("line\n"))
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != goroutines {
		t.Errorf("expected %d lines after concurrent writes, got %d", goroutines, len(lines))
	}
}
