package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"text/template"
	"time"

	"github.com/fatih/color"
	contextCMD "github.com/okteto/okteto/cmd/context"
	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/pkg/config"
	"github.com/okteto/okteto/pkg/errors"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/okteto"
	"github.com/spf13/cobra"
	"github.com/stern/stern/stern"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

type options struct {
	namespace           string
	podQuery            string
	selector            string
	fieldSelector       string
	color               string
	containerQuery      string
	output              string
	containerStates     []string
	timezone            string
	initContainers      bool
	ephemeralContainers bool
	tail                int64
	since               time.Duration
	excludePod          string
	excludeContainer    string
	exclude             []string
	include             []string
	timestamps          bool
}

func Logs(ctx context.Context) *cobra.Command {
	o := &options{
		podQuery:            ".*",
		color:               "auto",
		containerQuery:      ".*",
		output:              "default",
		containerStates:     []string{stern.RUNNING},
		timezone:            "Local",
		initContainers:      true,
		ephemeralContainers: true,
		since:               48 * time.Hour,
		exclude:             []string{},
		include:             []string{},
		tail:                -1,
	}

	cmd := &cobra.Command{
		Use:   "logs [pod-query]",
		Short: "Get logs for a namespace",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctxResource := &model.ContextResource{}
			if err := ctxResource.UpdateNamespace(o.namespace); err != nil {
				return err
			}

			ctxOptions := &contextCMD.ContextOptions{
				Namespace: ctxResource.Namespace,
			}
			if err := contextCMD.Run(ctx, ctxOptions); err != nil {
				return err
			}

			if !okteto.IsOkteto() {
				return errors.ErrContextIsNotOktetoCluster
			}

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			if len(args) > 0 {
				o.podQuery = args[0]
			}

			c, err := getSternConfig(o)
			if err != nil {
				return errors.UserError{
					E: fmt.Errorf("invalid log configuration: %w", err),
				}
			}

			go func() {
				sigint := make(chan os.Signal, 1)
				signal.Notify(sigint, syscall.SIGTERM, syscall.SIGINT)
				<-sigint
				cancel()
			}()

			if err := stern.Run(ctx, c); err != nil {
				return errors.UserError{
					E: fmt.Errorf("failed to get logs: %w", err),
				}
			}
			return nil
		},
		Args: utils.MaximumNArgsAccepted(1, ""),
	}

	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", o.namespace, "overrides the namespace where the pods are deployed")
	cmd.Flags().StringVar(&o.selector, "selector", o.selector, "Selector (label query) to filter on. If present, default to \".*\" for the pod-query.")
	cmd.Flags().StringVar(&o.fieldSelector, "field-selector", o.fieldSelector, "Selector (field query) to filter on. If present, default to \".*\" for the pod-query.")
	cmd.Flags().StringVar(&o.color, "color", o.color, "Force set color output. 'auto':  colorize if tty attached, 'always': always colorize, 'never': never colorize.")
	cmd.Flags().StringVarP(&o.containerQuery, "container", "c", o.containerQuery, "Container name when multiple containers in pod. (regular expression)")
	cmd.Flags().StringVarP(&o.output, "output", "o", o.output, "Specify predefined template. Currently support: [default, raw, json]")
	cmd.Flags().StringSliceVar(&o.containerStates, "container-state", o.containerStates, "Tail containers with state in running, waiting or terminated. To specify multiple states, repeat this or set comma-separated value.")
	cmd.Flags().StringVar(&o.timezone, "timezone", o.timezone, "Set timestamps to specific timezone.")
	cmd.Flags().BoolVar(&o.initContainers, "init-containers", o.initContainers, "Include or exclude init containers.")
	cmd.Flags().BoolVar(&o.ephemeralContainers, "ephemeral-containers", o.ephemeralContainers, "Include or exclude ephemeral containers.")
	cmd.Flags().DurationVarP(&o.since, "since", "s", o.since, "Return logs newer than a relative duration like 5s, 2m, or 3h.")
	cmd.Flags().Int64Var(&o.tail, "tail", o.tail, "The number of lines from the end of the logs to show. Defaults to -1, showing all logs.")
	cmd.Flags().StringSliceVarP(&o.exclude, "exclude", "e", o.exclude, "Log lines to exclude. (regular expression)")
	cmd.Flags().StringVarP(&o.excludeContainer, "exclude-container", "E", o.excludeContainer, "Container name to exclude when multiple containers in pod. (regular expression)")
	cmd.Flags().StringVar(&o.excludePod, "exclude-pod", o.excludePod, "Pod name to exclude. (regular expression)")
	cmd.Flags().StringSliceVarP(&o.include, "include", "i", o.include, "Log lines to include. (regular expression)")
	cmd.Flags().BoolVarP(&o.timestamps, "timestamps", "t", o.timestamps, "Print timestamps")

	return cmd
}

func getSternConfig(o *options) (*stern.Config, error) {
	ctxStore := okteto.ContextStore()
	kubeConfig := config.GetKubeconfigPath()[0]
	kubeCtx := okteto.UrlToKubernetesContext(ctxStore.CurrentContext)
	location, err := time.LoadLocation(o.timezone)
	if err != nil {
		return nil, err
	}

	if o.color == "always" {
		color.NoColor = false
	} else if o.color == "never" {
		color.NoColor = true
	} else if o.color != "auto" {
		return nil, fmt.Errorf("color should be one of 'always', 'never', or 'auto'")
	}

	funs := map[string]interface{}{
		"json": func(in interface{}) (string, error) {
			b, err := json.Marshal(in)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
		"parseJSON": func(text string) (map[string]interface{}, error) {
			obj := make(map[string]interface{})
			if err := json.Unmarshal([]byte(text), &obj); err != nil {
				return obj, err
			}
			return obj, nil
		},
		"color": func(color color.Color, text string) string {
			return color.SprintFunc()(text)
		},
	}

	t := "{{color .PodColor .PodName}} {{color .ContainerColor .ContainerName}} {{.Message}}\n"
	if color.NoColor {
		t = "{{.PodName}} {{.ContainerName}} {{.Message}}\n"
	}
	switch o.output {
	case "raw":
		t = "{{.Message}}\n"
	case "json":
		t = "{{json .}}\n"
	}

	tmpl, err := template.New("logs").Funcs(funs).Parse(t)
	if err != nil {
		return nil, err
	}

	podQuery, err := regexp.Compile(o.podQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regular expression from query: %w", err)
	}
	var excludePodQuery *regexp.Regexp
	if o.excludePod != "" {
		excludePodQuery, err = regexp.Compile(o.excludePod)
		if err != nil {
			return nil, fmt.Errorf("failed to compile regular expression for excluded pod query: %w", err)
		}
	}

	containerQuery, err := regexp.Compile(o.containerQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regular expression for container query: %w", err)
	}

	var excludeContainerQuery *regexp.Regexp
	if o.excludeContainer != "" {
		excludeContainerQuery, err = regexp.Compile(o.excludeContainer)
		if err != nil {
			return nil, fmt.Errorf("failed to compile regular expression for exclude container query: %w", err)
		}
	}

	var exclude []*regexp.Regexp
	for _, ex := range o.exclude {
		rex, err := regexp.Compile(ex)
		if err != nil {
			return nil, fmt.Errorf("failed to compile regular expression for exclusion filter: %w", err)
		}

		exclude = append(exclude, rex)
	}

	var include []*regexp.Regexp
	for _, inc := range o.include {
		rin, err := regexp.Compile(inc)
		if err != nil {
			return nil, fmt.Errorf("failed to compile regular expression for inclusion filter: %w", err)
		}

		include = append(include, rin)
	}

	containerStates := []stern.ContainerState{}
	if o.containerStates != nil {
		for _, containerStateStr := range o.containerStates {
			containerState, err := stern.NewContainerState(containerStateStr)
			if err != nil {
				return nil, err
			}
			containerStates = append(containerStates, containerState)
		}
	}

	labelSelector := labels.Everything()
	if o.selector != "" {
		labelSelector, err = labels.Parse(o.selector)
		if err != nil {
			return nil, fmt.Errorf("failed to parse selector as label selector: %w", err)
		}
	}

	fieldSelector := fields.Everything()
	if o.fieldSelector != "" {
		fieldSelector, err = fields.ParseSelector(o.fieldSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to parse selector as field selector: %w", err)
		}
	}

	var tailLines *int64
	if o.tail != -1 {
		tailLines = &o.tail
	}

	return &stern.Config{
		KubeConfig:            kubeConfig,
		ContextName:           kubeCtx,
		Namespaces:            []string{o.namespace},
		PodQuery:              podQuery,
		ExcludePodQuery:       excludePodQuery,
		ContainerQuery:        containerQuery,
		ExcludeContainerQuery: excludeContainerQuery,
		Exclude:               exclude,
		Include:               include,
		InitContainers:        o.initContainers,
		EphemeralContainers:   o.ephemeralContainers,
		Since:                 o.since,
		Template:              tmpl,
		ContainerStates:       containerStates,
		Location:              location,
		LabelSelector:         labelSelector,
		FieldSelector:         fieldSelector,
		TailLines:             tailLines,
		Timestamps:            o.timestamps,
		AllNamespaces:         false,
		ErrOut:                os.Stderr,
		Out:                   os.Stdout,
	}, nil
}
