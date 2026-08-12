package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// `go run`/`go build` without that flag fall back to "dev".
var version = "dev"

var (
	namespace     string
	kubeContext   string
	kubeconfig    string
	selector      string
	fieldSelector string
	allNamespaces bool
	output        string
	colorMode     string
	diagnose      bool
	diagnoseDelay time.Duration
	logLines      int
	watchTimeout  time.Duration
)

func main() {
	klog.SetLogger(logr.Discard())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	rootCmd := &cobra.Command{
		Use:   "kwatch [resource] [name]",
		Short: "Watch Kubernetes resources",
		Long: `kwatch watches Kubernetes resources with colorized status and automatic crash diagnosis.
Accepts the same flags as kubectl — anyone who knows kubectl can use it right away.

Examples:
  kwatch                              watch pods in current namespace
  kwatch deployments                  watch deployments
  kwatch pods -n production           watch pods in production namespace
  kwatch pods my-pod -n staging       watch a specific pod
  kwatch -l app=my-app                watch pods matching a label selector
  kwatch pods -A                      watch pods across all namespaces
  kwatch deployments --context prod   watch deployments in a specific context
  kwatch -o json                      print pods as JSON (no watch)
  kwatch -o yaml                      print pods as YAML (no watch)`,
		Args:    cobra.RangeArgs(0, 2),
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(ctx, cmd, args)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace")
	rootCmd.Flags().StringVar(&kubeContext, "context", "", "Name of the kubeconfig context to use")
	rootCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig file")
	rootCmd.Flags().StringVarP(&selector, "selector", "l", "", "Label selector (e.g. app=my-app,env=prod)")
	rootCmd.Flags().StringVar(&fieldSelector, "field-selector", "", "Field selector (e.g. status.phase=Running)")
	rootCmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Watch resources across all namespaces")
	rootCmd.Flags().StringVarP(&output, "output", "o", "", "Output format: wide|json|yaml|name")
	rootCmd.Flags().StringVar(&colorMode, "color", "auto", "Color output: auto|always|never")
	rootCmd.Flags().BoolVar(&diagnose, "diagnose", true, "Automatically show logs and events when a pod enters an error state")
	rootCmd.Flags().DurationVar(&diagnoseDelay, "diagnose-delay", 15*time.Second, "Grace period before diagnosing a failing pod (transient errors that recover within this window are ignored)")
	rootCmd.Flags().IntVar(&logLines, "log-lines", 50, "Number of log lines to fetch per diagnosis")
	rootCmd.Flags().DurationVar(&watchTimeout, "timeout", 0, "Exit after this duration (0 = watch indefinitely)")

	// First argument: resource type (static list matching common kubectl resources).
	rootCmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completionResources, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// --namespace / -n: dynamic from the cluster.
	_ = rootCmd.RegisterFlagCompletionFunc("namespace", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		config, err := buildRestConfig(kubeconfig, kubeContext)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return fetchNamespaces(config), cobra.ShellCompDirectiveNoFileComp
	})

	// --context: from local kubeconfig.
	_ = rootCmd.RegisterFlagCompletionFunc("context", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return fetchContexts(kubeconfig), cobra.ShellCompDirectiveNoFileComp
	})

	// Cobra only auto-registers a "help" command when the root command has
	// real subcommands (see (*cobra.Command).InitDefaultHelpCmd); kwatch has
	// none, since it mimics kubectl's flag-only interface. Without this, "kwatch
	// help" would be parsed as a positional resource argument and fail with
	// "unknown resource \"help\"" instead of printing usage.
	if len(os.Args) == 2 && os.Args[1] == "help" {
		os.Args[1] = "--help"
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runWatch(ctx context.Context, cmd *cobra.Command, args []string) error {
	if watchTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, watchTimeout)
		defer cancel()
	}

	switch colorMode {
	case "always":
		color.NoColor = false
	case "never":
		color.NoColor = true
	case "auto":
		// fatih/color already checks isatty on os.Stdout at init — nothing to do
	default:
		return fmt.Errorf("invalid --color value %q: must be auto, always, or never", colorMode)
	}

	resource := "pods"
	if len(args) > 0 {
		resource = args[0]
	}

	// Specific resource name (e.g. "kwatch pods my-pod") — append as field selector.
	specificName := ""
	if len(args) > 1 {
		specificName = args[1]
	}

	config, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}

	gvr, namespaced, err := resolveGVR(config, resource)
	if err != nil {
		return err
	}

	// For a specific named resource, add a field selector.
	fs := fieldSelector
	if specificName != "" {
		if fs != "" {
			fs += ","
		}
		fs += "metadata.name=" + specificName
	}

	// Resolve the effective namespace: explicit flag > kubeconfig context default.
	// When allNamespaces is set, namespace is intentionally left empty.
	ns := namespace
	if !allNamespaces && ns == "" {
		ns = resolveNamespace(kubeconfig, kubeContext)
	}

	switch output {
	case "", "wide":
		var d *Diagnoser
		if diagnose {
			cs, err := buildClientset(config)
			if err != nil {
				return fmt.Errorf("clientset: %w", err)
			}
			d = NewDiagnoser(ctx, cs, ns, diagnoseDelay, logLines)
		}
		if err := watchTable(ctx, config, gvr, ns, allNamespaces, namespaced, selector, fs, output == "wide", os.Stdout, d); err != nil {
			if ctx.Err() == nil {
				return err
			}
		}
	case "json", "yaml", "name":
		if err := watchDynamic(ctx, config, gvr, ns, allNamespaces, namespaced, selector, fs, output, os.Stdout); err != nil {
			if ctx.Err() == nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported output format %q: use wide, json, yaml, or name", output)
	}

	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "kwatch: timed out after %s\n", watchTimeout)
	}
	return nil
}
