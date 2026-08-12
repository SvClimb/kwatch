package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/fatih/color"
)

var (
	colGreen   = color.New(color.FgGreen).SprintFunc()
	colYellow  = color.New(color.FgYellow).SprintFunc()
	colRed     = color.New(color.FgRed, color.Bold).SprintFunc()
	colCyan    = color.New(color.FgCyan).SprintFunc()
	colGray    = color.New(color.FgHiBlack).SprintFunc()
	colBold    = color.New(color.Bold).SprintFunc()
	colMagenta = color.New(color.FgMagenta).SprintFunc()
)

func statusColor(s string) string {
	switch {
	case s == "Running":
		return colGreen(s)
	case s == "Pending", s == "ContainerCreating", s == "PodInitializing":
		return colYellow(s)
	case s == "Terminating":
		return colMagenta(s)
	case s == "Completed":
		return colGray(s)
	case familyFor(s) != "":
		// familyFor is the same status→family classification diagnose.go uses
		// to decide when to trigger a diagnosis — one place defines "what
		// counts as an error" for both coloring and diagnosis.
		return colRed(s)
	case strings.HasPrefix(s, "Init:"):
		// Progress states (Init:0/3) that aren't a known error family.
		return colCyan(s)
	case strings.HasPrefix(s, "Err"):
		// Catch-all for error reasons not in statusFamily (e.g. future
		// Kubernetes reason strings we haven't special-cased yet).
		return colRed(s)
	default:
		return s
	}
}

// syncWriter serializes concurrent writes from the printer loop and
// background diagnoser goroutines so their output doesn't interleave.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

// findColumnOffset returns the byte offset of col as a standalone word in
// header, or -1 if not found. Requires a space (or line boundary) on both
// sides, so "NAME" does not match at position 0 inside "NAMESPACE".
func findColumnOffset(header, col string) int {
	n := len(col)
	for i := 0; i <= len(header)-n; i++ {
		if header[i:i+n] != col {
			continue
		}
		before := i == 0 || header[i-1] == ' '
		after := i+n >= len(header) || header[i+n] == ' '
		if before && after {
			return i
		}
	}
	return -1
}

// extractField returns the first whitespace-separated token starting at offset
// within line, or "" when offset is out of range.
func extractField(line string, offset int) string {
	if offset < 0 || offset >= len(line) {
		return ""
	}
	if f := strings.Fields(line[offset:]); len(f) > 0 {
		return f[0]
	}
	return ""
}

// printColorized reads kubectl watch output line-by-line, colorizes the STATUS
// column, and optionally triggers crash diagnosis via d (may be nil).
func printColorized(r io.Reader, w io.Writer, d *Diagnoser) {
	sw := &syncWriter{w: w}
	scanner := bufio.NewScanner(r)
	namespaceOffset := -1
	nameOffset := -1
	statusOffset := -1
	restartsOffset := -1

	for scanner.Scan() {
		line := scanner.Text()

		// Detect the header: record byte offsets for NAMESPACE, NAME, STATUS, RESTARTS.
		if statusOffset == -1 {
			if idx := findColumnOffset(line, "STATUS"); idx >= 0 {
				statusOffset = idx
				nameOffset = findColumnOffset(line, "NAME")
				namespaceOffset = findColumnOffset(line, "NAMESPACE")
				restartsOffset = findColumnOffset(line, "RESTARTS")
				fmt.Fprintln(sw, colBold(line))
			} else {
				fmt.Fprintln(sw, line)
			}
			continue
		}

		// Colorize status and optionally diagnose.
		if len(line) > statusOffset {
			prefix := line[:statusOffset]
			rest := line[statusOffset:]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				status := fields[0]
				pos := strings.Index(rest, status)
				fmt.Fprintln(sw, prefix+rest[:pos]+statusColor(status)+rest[pos+len(status):])

				if d != nil {
					podName := extractField(line, nameOffset)
					podNamespace := extractField(line, namespaceOffset)
					if podName != "" {
						d.Check(podName, podNamespace, status, sw)
						if familyFor(status) == "" {
							d.NoteRestarts(podName, podNamespace, extractField(line, restartsOffset), sw)
						}
					}
				}
				continue
			}
		}

		fmt.Fprintln(sw, line)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(sw, "kwatch: read error: %v\n", err)
	}
}
