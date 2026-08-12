package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// tableFormatter builds fixed-width text rows compatible with printColorized.
type tableFormatter struct {
	colNames []string
	widths   []int
	wide     bool
	injectNS bool // prepend NAMESPACE column when listing across all namespaces
}

func newTableFormatter(cols []metav1.TableColumnDefinition, rows []metav1.TableRow, wide, injectNS bool) *tableFormatter {
	f := &tableFormatter{wide: wide, injectNS: injectNS}

	if injectNS {
		f.colNames = []string{"NAMESPACE"}
		f.widths = []int{len("NAMESPACE")}
		for _, row := range rows {
			if ns := objNamespace(row.Object.Raw); len(ns) > f.widths[0] {
				f.widths[0] = len(ns)
			}
		}
	}

	for _, col := range cols {
		if !wide && col.Priority > 0 {
			continue
		}
		f.colNames = append(f.colNames, strings.ToUpper(col.Name))
		f.widths = append(f.widths, len(col.Name))
	}

	nsOffset := 0
	if injectNS {
		nsOffset = 1
	}
	for _, row := range rows {
		fmtIdx := nsOffset
		for origIdx, col := range cols {
			if !wide && col.Priority > 0 {
				continue
			}
			if origIdx < len(row.Cells) {
				if s := fmt.Sprintf("%v", row.Cells[origIdx]); len(s) > f.widths[fmtIdx] {
					f.widths[fmtIdx] = len(s)
				}
			}
			fmtIdx++
		}
	}
	return f
}

func (f *tableFormatter) header() string {
	return f.format(nil)
}

func (f *tableFormatter) rowFromCells(cells []interface{}, cols []metav1.TableColumnDefinition, objRaw json.RawMessage) string {
	var formatted []string
	if f.injectNS {
		formatted = []string{objNamespace(objRaw)}
	}
	for origIdx, col := range cols {
		if !f.wide && col.Priority > 0 {
			continue
		}
		s := ""
		if origIdx < len(cells) {
			s = fmt.Sprintf("%v", cells[origIdx])
		}
		formatted = append(formatted, s)
	}
	return f.format(formatted)
}

func (f *tableFormatter) format(cells []string) string {
	parts := make([]string, len(f.colNames))
	for i, name := range f.colNames {
		v := name
		if cells != nil {
			if i < len(cells) {
				v = cells[i]
			} else {
				v = ""
			}
		}
		if i < len(f.widths)-1 {
			parts[i] = fmt.Sprintf("%-*s", f.widths[i], v)
		} else {
			parts[i] = v
		}
	}
	return strings.Join(parts, "   ")
}

// objNamespace extracts metadata.namespace from a raw JSON object.
func objNamespace(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.Metadata.Namespace
}

// tableAccept matches kubectl's exact Accept header for the Table API.
const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"

func buildTableRESTClient(config *rest.Config, gvr schema.GroupVersionResource) (rest.Interface, error) {
	cfg := rest.CopyConfig(config)
	cfg.GroupVersion = &schema.GroupVersion{Group: gvr.Group, Version: gvr.Version}
	if gvr.Group == "" {
		cfg.APIPath = "/api"
	} else {
		cfg.APIPath = "/apis"
	}
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme).WithoutConversion()
	cfg.AcceptContentTypes = tableAccept
	return rest.RESTClientFor(cfg)
}

// rawWatchEvent is the top-level JSON envelope for Kubernetes watch stream events.
type rawWatchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// watchTable streams any Kubernetes resource via the Table API and feeds
// formatted text into printColorized through a pipe. The watch connection is
// automatically re-established on transient errors or resource-version expiry
// (HTTP 410 Gone).
func watchTable(ctx context.Context, config *rest.Config, gvr schema.GroupVersionResource,
	ns string, allNS, namespaced bool, selector, fieldSelector string, wide bool, w io.Writer, d *Diagnoser) error {

	client, err := buildTableRESTClient(config, gvr)
	if err != nil {
		return fmt.Errorf("REST client: %w", err)
	}

	listOpts := metav1.ListOptions{LabelSelector: selector, FieldSelector: fieldSelector}

	buildReq := func(opts metav1.ListOptions) *rest.Request {
		req := client.Get().Resource(gvr.Resource).
			VersionedParams(&opts, metav1.ParameterCodec).
			SetHeader("Accept", tableAccept)
		if namespaced && !allNS && ns != "" {
			req = req.Namespace(ns)
		}
		// includeObject=Metadata makes the server populate row.Object with metadata
		// (including namespace), which we use to inject the NAMESPACE column when -A.
		if allNS {
			req = req.Param("includeObject", "Metadata")
		}
		return req
	}

	raw, err := buildReq(listOpts).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("list %s: %w", gvr.Resource, err)
	}

	var initialTable metav1.Table
	if err := json.Unmarshal(raw, &initialTable); err != nil {
		return fmt.Errorf("decode table: %w", err)
	}

	formatter := newTableFormatter(initialTable.ColumnDefinitions, initialTable.Rows, wide, allNS)

	pr, pw := io.Pipe()
	// watchErrCh carries the goroutine's terminal result back to the caller.
	// Buffered so the send never blocks before pw.Close().
	watchErrCh := make(chan error, 1)

	go func() {
		defer func() {
			watchErrCh <- nil
			pw.Close()
		}()

		fmt.Fprintln(pw, formatter.header())
		for _, row := range initialTable.Rows {
			fmt.Fprintln(pw, formatter.rowFromCells(row.Cells, initialTable.ColumnDefinitions, row.Object.Raw))
		}

		cols := initialTable.ColumnDefinitions
		rv := initialTable.ResourceVersion
		const maxBackoff = 30 * time.Second
		backoff := time.Second

		for ctx.Err() == nil {
			watchOpts := metav1.ListOptions{
				Watch:           true,
				ResourceVersion: rv,
				LabelSelector:   selector,
				FieldSelector:   fieldSelector,
			}

			stream, err := buildReq(watchOpts).Stream(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if apierrors.IsGone(err) {
					cols, rv = relistWatch(ctx, buildReq, listOpts, formatter, initialTable.ColumnDefinitions, pw)
					backoff = time.Second
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
					backoff = min(backoff*2, maxBackoff)
				}
				continue
			}

			backoff = time.Second // reset on successful connection
			var needRelist bool
			cols, rv, needRelist = consumeWatchStream(stream, cols, rv, formatter, pw)
			stream.Close()

			if ctx.Err() != nil {
				return
			}
			if needRelist {
				cols, rv = relistWatch(ctx, buildReq, listOpts, formatter, initialTable.ColumnDefinitions, pw)
				backoff = time.Second
				continue
			}
			// Server closed the stream normally — retry with backoff.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
		}
	}()

	printColorized(pr, w, d)
	return <-watchErrCh
}

// relistWatch does a fresh list, writes all current rows to pw, and returns
// the updated column definitions and resource version. On failure it returns
// the fallback values and an empty rv so the retry watch starts from "now".
func relistWatch(ctx context.Context, buildReq func(metav1.ListOptions) *rest.Request,
	listOpts metav1.ListOptions, formatter *tableFormatter,
	fallbackCols []metav1.TableColumnDefinition, pw *io.PipeWriter) ([]metav1.TableColumnDefinition, string) {

	raw, err := buildReq(listOpts).Do(ctx).Raw()
	if err != nil || ctx.Err() != nil {
		return fallbackCols, ""
	}
	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return fallbackCols, ""
	}
	cols := fallbackCols
	if len(table.ColumnDefinitions) > 0 {
		cols = table.ColumnDefinitions
	}
	for _, row := range table.Rows {
		fmt.Fprintln(pw, formatter.rowFromCells(row.Cells, cols, row.Object.Raw))
	}
	return cols, table.ResourceVersion
}

// consumeWatchStream drains a single watch stream, writing formatted rows to pw.
// Returns updated columns, latest resource version, and whether a relist is needed
// (server sent an ERROR event with HTTP status 410).
func consumeWatchStream(stream io.ReadCloser, cols []metav1.TableColumnDefinition, rv string,
	formatter *tableFormatter, pw *io.PipeWriter) ([]metav1.TableColumnDefinition, string, bool) {

	scanner := bufio.NewScanner(stream)
	// 1 MiB max line: a single wide-format or JSON-serialized table row can
	// exceed bufio.Scanner's 64 KiB default and truncate the stream.
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var event rawWatchEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type == "ERROR" {
			var status metav1.Status
			if json.Unmarshal(event.Object, &status) == nil && status.Code == 410 {
				return cols, rv, true
			}
			continue
		}
		var table metav1.Table
		if err := json.Unmarshal(event.Object, &table); err != nil {
			continue
		}
		if table.ResourceVersion != "" {
			rv = table.ResourceVersion
		}
		if len(table.ColumnDefinitions) > 0 {
			cols = table.ColumnDefinitions
		}
		for _, row := range table.Rows {
			fmt.Fprintln(pw, formatter.rowFromCells(row.Cells, cols, row.Object.Raw))
		}
	}
	return cols, rv, false
}

// watchDynamic handles -o json/yaml/name: a single list snapshot, not a
// continuous watch — the point of these formats is a one-shot structured
// dump for scripting (e.g. piping into jq), not an unbounded stream.
func watchDynamic(ctx context.Context, config *rest.Config, gvr schema.GroupVersionResource,
	ns string, allNS, namespaced bool, selector, fieldSelector, outputFmt string, w io.Writer) error {

	dynClient, err := buildDynamicClient(config)
	if err != nil {
		return err
	}

	ri := dynClient.Resource(gvr)
	listOpts := metav1.ListOptions{LabelSelector: selector, FieldSelector: fieldSelector}

	printUnstructured := func(obj map[string]interface{}) error {
		switch outputFmt {
		case "name":
			name, _, _ := unstructured.NestedString(obj, "metadata", "name")
			fmt.Fprintf(w, "%s/%s\n", gvr.Resource, name)
		case "json":
			b, err := json.MarshalIndent(obj, "", "    ")
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\n", b)
		case "yaml":
			b, err := yaml.Marshal(obj)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "---\n%s", b)
		}
		return nil
	}

	var list *unstructured.UnstructuredList
	if !namespaced || allNS || ns == "" {
		list, err = ri.List(ctx, listOpts)
	} else {
		list, err = ri.Namespace(ns).List(ctx, listOpts)
	}
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	for i := range list.Items {
		if err := printUnstructured(list.Items[i].Object); err != nil {
			return err
		}
	}
	return nil
}
