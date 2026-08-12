package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// controlSeq matches ANSI/VT escape sequences and raw control characters.
// Applied to log and event output before writing to the terminal so a
// malicious container cannot inject sequences that clear the screen,
// hijack the window title, or write to the clipboard.
var controlSeq = regexp.MustCompile(
	`\x1b(?:\[[0-9;?]*[A-Za-z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|.)` + // CSI / OSC(BEL or ST) / 2-char
		`|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`, // raw control chars (keep \t)
)

func stripControl(s string) string {
	return controlSeq.ReplaceAllString(s, "")
}

var (
	colDivider = color.New(color.FgHiBlack).SprintFunc()
	colAlert   = color.New(color.FgRed, color.Bold).SprintFunc()
	colSection = color.New(color.FgYellow, color.Bold).SprintFunc()
)

// statusFamily maps each diagnosable status to a semantic group.
// Statuses in the same family share the same underlying cause,
// so only the first one triggers a diagnosis per recovery cycle.
var statusFamily = map[string]string{
	"CrashLoopBackOff":           "crash",
	"Error":                      "crash",
	"OOMKilled":                  "crash",
	"ImagePullBackOff":           "imagepull",
	"ErrImagePull":               "imagepull",
	"CreateContainerError":       "config",
	"CreateContainerConfigError": "config",
	"InvalidImageName":           "config",
	"Evicted":                    "evicted",
}

// familyFor returns the diagnostic family for status, handling both direct
// statuses and init-container-prefixed variants (e.g. "Init:Error" → "init-crash").
func familyFor(status string) string {
	if f := statusFamily[status]; f != "" {
		return f
	}
	// Init container errors appear as "Init:<reason>" in the STATUS column.
	if strings.HasPrefix(status, "Init:") {
		if f := statusFamily[status[len("Init:"):]]; f != "" {
			return "init-" + f
		}
	}
	return ""
}

// diagnosisTimeout caps each API call made during diagnosis so goroutines
// can't hang forever on a slow or unreachable apiserver.
const diagnosisTimeout = 10 * time.Second

// seenTTL is the maximum lifetime of a seen entry. It prevents the map from
// leaking when pods are force-deleted while still in an error state (they stop
// emitting status events, so the recovery timer never fires).
const seenTTL = 2 * time.Hour

// maxConcurrentDiagnoses caps simultaneous log+event API calls so a wave of
// pod failures doesn't flood the API server.
const maxConcurrentDiagnoses = 5

type pendingDiagnosis struct {
	family string
	timer  *time.Timer
}

// Diagnoser watches pod status transitions and automatically fetches
// logs and events when a pod stays in an error state past the grace period.
//
// Deduplication rules:
//   - Statuses in the same family (Error + CrashLoopBackOff = "crash") produce
//     only one diagnosis block per recovery cycle.
//   - A "Running" state is only treated as genuine recovery after a sustained
//     window equal to gracePeriod. Brief Running states inside a crash loop
//     do not reset the family and do not trigger re-diagnosis.
type Diagnoser struct {
	ctx              context.Context
	clientset        *kubernetes.Clientset
	namespace        string
	gracePeriod      time.Duration
	logLines         int
	mu               sync.Mutex
	seen             map[string]string            // podKey → diagnosed family
	pending          map[string]*pendingDiagnosis // podKey → pending diagnosis
	recovering       map[string]*time.Timer       // podKey → recovery confirmation timer
	ttl              map[string]*time.Timer       // podKey → seenTTL cleanup timer
	restarts         map[string]int               // podKey → last seen restart count
	restartDiagnosed map[string]time.Time         // podKey → time of last restart diagnosis
	diagSem          chan struct{}                // semaphore: at most maxConcurrentDiagnoses active

	// Overridable in tests to avoid requiring a live cluster.
	logsFunc   func(podName, namespace string) string
	eventsFunc func(podName, namespace string) string
}

// podKey returns a stable unique key for a pod. In -A mode, namespace is
// non-empty, preventing same-name pods in different namespaces from sharing
// a single entry in seen/pending/recovering.
func podKey(podName, podNamespace string) string {
	if podNamespace != "" {
		return podNamespace + "/" + podName
	}
	return podName
}

func NewDiagnoser(ctx context.Context, clientset *kubernetes.Clientset, namespace string, gracePeriod time.Duration, logLines int) *Diagnoser {
	d := &Diagnoser{
		ctx:              ctx,
		clientset:        clientset,
		namespace:        namespace,
		gracePeriod:      gracePeriod,
		logLines:         logLines,
		seen:             make(map[string]string),
		pending:          make(map[string]*pendingDiagnosis),
		recovering:       make(map[string]*time.Timer),
		ttl:              make(map[string]*time.Timer),
		restarts:         make(map[string]int),
		restartDiagnosed: make(map[string]time.Time),
		diagSem:          make(chan struct{}, maxConcurrentDiagnoses),
	}
	d.logsFunc = d.fetchLogs
	d.eventsFunc = d.fetchEvents
	return d
}

// Check is called for every pod status update.
// podNamespace is "" in single-namespace mode and the actual namespace in -A mode.
func (d *Diagnoser) Check(podName, podNamespace, status string, w io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := podKey(podName, podNamespace)
	family := familyFor(status)

	if family == "" {
		// Healthy status: cancel any pending error diagnosis.
		if p, ok := d.pending[key]; ok {
			p.timer.Stop()
			delete(d.pending, key)
		}

		// If we already diagnosed this pod, don't clear seen immediately —
		// wait gracePeriod to confirm this is genuine recovery and not a
		// brief Running blip inside a crash loop.
		if _, diagnosed := d.seen[key]; diagnosed {
			if _, alreadyWaiting := d.recovering[key]; !alreadyWaiting {
				t := time.AfterFunc(d.gracePeriod, func() {
					d.mu.Lock()
					// Cancel the seenTTL timer for this cycle so it can't fire
					// into a future crash cycle and spuriously delete seen.
					if ttlT, ok := d.ttl[key]; ok {
						ttlT.Stop()
						delete(d.ttl, key)
					}
					delete(d.seen, key)
					delete(d.recovering, key)
					d.mu.Unlock()
				})
				d.recovering[key] = t
			}
		}
		return
	}

	// Error status: cancel any pending recovery confirmation (pod crashed again).
	if t, ok := d.recovering[key]; ok {
		t.Stop()
		delete(d.recovering, key)
	}

	// Already diagnosed this family in the current error cycle.
	if d.seen[key] == family {
		return
	}
	// Already waiting to diagnose the same family.
	if p, ok := d.pending[key]; ok && p.family == family {
		return
	}

	// Cancel any pending diagnosis for a different family.
	if p, ok := d.pending[key]; ok {
		p.timer.Stop()
		delete(d.pending, key)
	}

	// Resolve the namespace to use for API calls. In single-namespace mode
	// podNamespace is "" and we fall back to the global flag value.
	effectiveNs := podNamespace
	if effectiveNs == "" {
		effectiveNs = d.namespace
	}

	// Schedule diagnosis after the grace period.
	timer := time.AfterFunc(d.gracePeriod, func() {
		// Bail out if the process is shutting down (Ctrl+C / SIGTERM).
		select {
		case <-d.ctx.Done():
			d.mu.Lock()
			delete(d.pending, key)
			d.mu.Unlock()
			return
		default:
		}

		d.mu.Lock()
		p, exists := d.pending[key]
		if !exists || p.family != family {
			d.mu.Unlock()
			return
		}
		delete(d.pending, key)
		d.seen[key] = family
		// Stop any TTL timer from a previous cycle before starting a new one.
		// Without this, the old timer could fire into the current cycle and
		// delete seen[key] mid-crash, triggering a spurious re-diagnosis.
		if old, ok := d.ttl[key]; ok {
			old.Stop()
		}
		ttlT := time.AfterFunc(seenTTL, func() {
			d.mu.Lock()
			delete(d.seen, key)
			delete(d.ttl, key)
			delete(d.restarts, key)
			d.mu.Unlock()
		})
		d.ttl[key] = ttlT
		d.mu.Unlock()

		d.diagnose(podName, effectiveNs, colAlert("⚠  CRASH DETECTED:"), status, w)
	})
	d.pending[key] = &pendingDiagnosis{family: family, timer: timer}
}

func (d *Diagnoser) diagnose(podName, podNamespace, header, status string, w io.Writer) {
	select {
	case d.diagSem <- struct{}{}:
		defer func() { <-d.diagSem }()
	case <-d.ctx.Done():
		return
	}

	div := strings.Repeat("─", 66)
	var buf bytes.Buffer

	var logs, events string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		logs = d.logsFunc(podName, podNamespace)
	}()
	go func() {
		defer wg.Done()
		events = d.eventsFunc(podName, podNamespace)
	}()
	wg.Wait()

	fmt.Fprintln(&buf, colDivider(div))
	fmt.Fprintf(&buf, " %s %s  [%s]\n", header, podName, colRed(status))
	fmt.Fprintln(&buf, colDivider(div))

	if logs != "" {
		fmt.Fprintln(&buf, colSection(" ▶ LOGS (previous container):"))
		for _, line := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
			fmt.Fprintln(&buf, "   "+stripControl(line))
		}
		fmt.Fprintln(&buf)
	}

	if events != "" {
		fmt.Fprintln(&buf, colSection(" ▶ EVENTS:"))
		for _, line := range strings.Split(strings.TrimRight(events, "\n"), "\n") {
			fmt.Fprintln(&buf, "   "+stripControl(line))
		}
		fmt.Fprintln(&buf)
	}

	if logs == "" && events == "" {
		fmt.Fprintln(&buf, "   no data: logs unavailable, no events found")
		fmt.Fprintln(&buf)
	}

	fmt.Fprintln(&buf, colDivider(div))
	_, _ = w.Write(buf.Bytes())
}

// NoteRestarts is called on every pod row that is in a healthy (non-error)
// status. It tracks the restart counter and emits a single-line warning
// when it increases — the signal that a pod cycled without entering an
// error state visible in the STATUS column (e.g. failing startupProbe).
func (d *Diagnoser) NoteRestarts(podName, podNamespace, restartsStr string, w io.Writer) {
	count, err := strconv.Atoi(restartsStr)
	if err != nil {
		return
	}
	key := podKey(podName, podNamespace)
	d.mu.Lock()
	prev, seen := d.restarts[key]
	d.restarts[key] = count
	var shouldDiagnose bool
	if seen && count > prev {
		if time.Since(d.restartDiagnosed[key]) >= d.gracePeriod {
			d.restartDiagnosed[key] = time.Now()
			shouldDiagnose = true
		}
	}
	d.mu.Unlock()

	if !seen {
		// First time seeing this pod — schedule cleanup so the entry doesn't
		// accumulate for pods that never enter an error state (no seenTTL timer).
		time.AfterFunc(seenTTL, func() {
			d.mu.Lock()
			delete(d.restarts, key)
			delete(d.restartDiagnosed, key)
			d.mu.Unlock()
		})
	}
	if seen && count > prev {
		fmt.Fprintf(w, "%s %s  restarted (%d → %d)\n",
			colSection(" ↺ "), podName, prev, count)
		if shouldDiagnose {
			effectiveNs := podNamespace
			if effectiveNs == "" {
				effectiveNs = d.namespace
			}
			go d.diagnose(podName, effectiveNs, colSection("↺  RESTART DETECTED:"), fmt.Sprintf("Restart #%d", count), w)
		}
	}
}

// crashedContainers returns container names in priority order for log fetching:
// containers with a known error state or restart history first, then the rest.
// Both regular and init containers are checked.
func crashedContainers(pod *corev1.Pod) []string {
	var crashed, rest []string
	for _, cs := range pod.Status.ContainerStatuses {
		if (cs.State.Waiting != nil && statusFamily[cs.State.Waiting.Reason] != "") ||
			(cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0) ||
			cs.LastTerminationState.Terminated != nil {
			crashed = append(crashed, cs.Name)
		} else {
			rest = append(rest, cs.Name)
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if (cs.State.Waiting != nil && cs.State.Waiting.Reason != "") ||
			(cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0) {
			crashed = append(crashed, cs.Name)
		}
	}
	return append(crashed, rest...)
}

func (d *Diagnoser) fetchLogs(podName, podNamespace string) string {
	ctx, cancel := context.WithTimeout(d.ctx, diagnosisTimeout)
	pod, err := d.clientset.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
	cancel()
	if err != nil {
		// Fall back to no-container-name fetch (works for single-container pods).
		return d.fetchContainerLogs(podName, podNamespace, "")
	}
	for _, name := range crashedContainers(pod) {
		if logs := d.fetchContainerLogs(podName, podNamespace, name); logs != "" {
			return logs
		}
	}
	return ""
}

func (d *Diagnoser) fetchContainerLogs(podName, podNamespace, containerName string) string {
	tailLines := int64(d.logLines)
	doFetch := func(previous bool) string {
		opts := &corev1.PodLogOptions{
			TailLines: &tailLines,
			Previous:  previous,
			Container: containerName,
		}
		ctx, cancel := context.WithTimeout(d.ctx, diagnosisTimeout)
		defer cancel()
		req := d.clientset.CoreV1().Pods(podNamespace).GetLogs(podName, opts)
		rc, err := req.Stream(ctx)
		if err != nil {
			return ""
		}
		defer rc.Close()
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rc)
		s := buf.String()
		// The log proxy can return HTTP 200 with an error sentence in the body
		// when the previous container was garbage-collected before we fetched
		// its logs. Treat such responses as empty so we fall back to current
		// container logs.
		if strings.HasPrefix(strings.TrimSpace(s), "unable to retrieve container logs") {
			return ""
		}
		return s
	}

	if out := doFetch(true); out != "" {
		return out
	}
	return doFetch(false)
}

func (d *Diagnoser) fetchEvents(podName, podNamespace string) string {
	ctx, cancel := context.WithTimeout(d.ctx, diagnosisTimeout)
	defer cancel()

	// Filter by UID so we don't show events from previous pods with the same name.
	fieldSel := "involvedObject.name=" + podName
	if pod, err := d.clientset.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{}); err == nil && pod.UID != "" {
		fieldSel += ",involvedObject.uid=" + string(pod.UID)
	}

	events, err := d.clientset.CoreV1().Events(podNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: fieldSel,
	})
	if err != nil || len(events.Items) == 0 {
		return ""
	}

	sort.Slice(events.Items, func(i, j int) bool {
		return events.Items[i].LastTimestamp.Before(&events.Items[j].LastTimestamp)
	})

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%-8s  %-7s  %-30s  %s\n", "TIME", "TYPE", "REASON", "MESSAGE")
	for _, e := range events.Items {
		ts := e.LastTimestamp.Time.Format("15:04:05")
		fmt.Fprintf(&buf, "%s  %-7s  %-30s  %s\n", ts, e.Type, e.Reason, e.Message)
	}
	return buf.String()
}
