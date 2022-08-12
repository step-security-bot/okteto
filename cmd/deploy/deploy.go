// Copyright 2022 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deploy

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	buildv2 "github.com/okteto/okteto/cmd/build/v2"
	contextCMD "github.com/okteto/okteto/cmd/context"
	"github.com/okteto/okteto/cmd/namespace"
	pipelineCMD "github.com/okteto/okteto/cmd/pipeline"
	stackCMD "github.com/okteto/okteto/cmd/stack"
	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/cmd/utils/executor"
	"github.com/okteto/okteto/pkg/analytics"
	"github.com/okteto/okteto/pkg/cmd/pipeline"
	"github.com/okteto/okteto/pkg/cmd/stack"
	"github.com/okteto/okteto/pkg/errors"
	oktetoErrors "github.com/okteto/okteto/pkg/errors"
	"github.com/okteto/okteto/pkg/k8s/diverts"
	"github.com/okteto/okteto/pkg/k8s/ingresses"
	"github.com/okteto/okteto/pkg/k8s/kubeconfig"
	oktetoLog "github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/okteto"
	oktetoPath "github.com/okteto/okteto/pkg/path"
	"github.com/okteto/okteto/pkg/types"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	headerUpgrade          = "Upgrade"
	succesfullyDeployedmsg = "Development environment '%s' successfully deployed"
)

var tempKubeConfigTemplate = "%s/.okteto/kubeconfig-%s-%d"

// Options options for deploy command
type Options struct {
	// ManifestPathFlag is the option -f as introduced by the user when executing this command.
	// This is stored at the configmap as filename to redeploy from the ui.
	ManifestPathFlag string
	// ManifestPath is the patah to the manifest used through the command execution.
	// This might change its value during execution
	ManifestPath     string
	Name             string
	Namespace        string
	K8sContext       string
	Variables        []string
	Build            bool
	Dependencies     bool
	servicesToDeploy []string

	Repository string
	Branch     string
	Wait       bool
	Timeout    time.Duration

	ShowCTA bool
}

// DeployCommand defines the config for deploying an app
type DeployCommand struct {
	GetManifest func(path string) (*model.Manifest, error)

	Proxy              proxyInterface
	Kubeconfig         kubeConfigHandler
	Executor           executor.ManifestExecutor
	TempKubeconfigFile string
	K8sClientProvider  okteto.K8sClientProvider
	Builder            *buildv2.OktetoBuilder

	PipelineType model.Archetype
}

//Deploy deploys the okteto manifest
func Deploy(ctx context.Context) *cobra.Command {
	options := &Options{}

	cmd := &cobra.Command{
		Use:   "deploy [service...]",
		Short: "Execute the list of commands specified in the 'deploy' section of your okteto manifest",
		RunE: func(_ *cobra.Command, args []string) error {

			// validate cmd options
			if options.Dependencies && !okteto.IsOkteto() {
				return fmt.Errorf("'dependencies' is only supported in clusters that have Okteto installed")
			}

			if err := validateAndSet(options.Variables, os.Setenv); err != nil {
				return err
			}

			// This is needed because the deploy command needs the original kubeconfig configuration even in the execution within another
			// deploy command. If not, we could be proxying a proxy and we would be applying the incorrect deployed-by label
			os.Setenv(model.OktetoSkipConfigCredentialsUpdate, "false")
			if options.ManifestPath != "" {
				// if path is absolute, its transformed to rel from root
				initialCWD, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("failed to get the current working directory: %w", err)
				}
				manifestPathFlag, err := oktetoPath.GetRelativePathFromCWD(initialCWD, options.ManifestPath)
				if err != nil {
					return err
				}
				// as the installer uses root for executing the pipeline, we save the rel path from root as ManifestPathFlag option
				options.ManifestPathFlag = manifestPathFlag

				// when the manifest path is set by the cmd flag, we are moving cwd so the cmd is executed from that dir
				uptManifestPath, err := model.UpdateCWDtoManifestPath(options.ManifestPath)
				if err != nil {
					return err
				}
				options.ManifestPath = uptManifestPath
			}
			if err := contextCMD.LoadContextFromPath(ctx, options.Namespace, options.K8sContext, options.ManifestPath); err != nil {
				if err.Error() == fmt.Errorf(oktetoErrors.ErrNotLogged, okteto.CloudURL).Error() {
					return err
				}
				if err := contextCMD.NewContextCommand().Run(ctx, &contextCMD.ContextOptions{Namespace: options.Namespace}); err != nil {
					return err
				}
			}

			if okteto.IsOkteto() {
				create, err := utils.ShouldCreateNamespace(ctx, okteto.Context().Namespace)
				if err != nil {
					return err
				}
				if create {
					nsCmd, err := namespace.NewCommand()
					if err != nil {
						return err
					}
					nsCmd.Create(ctx, &namespace.CreateOptions{Namespace: okteto.Context().Namespace})
				}
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get the current working directory: %w", err)
			}
			name := options.Name
			if options.Name == "" {
				name = utils.InferName(cwd)
				if err != nil {
					return fmt.Errorf("could not infer environment name")
				}
			}

			options.ShowCTA = oktetoLog.IsInteractive()
			options.servicesToDeploy = args

			kubeconfig := NewKubeConfig()

			proxy, err := NewProxy(kubeconfig)
			if err != nil {
				oktetoLog.Infof("could not configure local proxy: %s", err)
				return err
			}

			c := &DeployCommand{
				GetManifest:        model.GetManifestV2,
				Kubeconfig:         kubeconfig,
				Executor:           executor.NewExecutor(oktetoLog.GetOutputFormat()),
				Proxy:              proxy,
				TempKubeconfigFile: GetTempKubeConfigFile(name),
				K8sClientProvider:  okteto.NewK8sClientProvider(),
				Builder:            buildv2.NewBuilderFromScratch(),
			}
			startTime := time.Now()
			manifest, err := c.RunDeploy(ctx, options)

			deployType := "custom"
			hasDependencySection := false
			hasBuildSection := false
			if manifest != nil && manifest.IsV2 {
				if manifest.Deploy != nil &&
					manifest.Deploy.ComposeSection != nil &&
					manifest.Deploy.ComposeSection.ComposesInfo != nil {
					deployType = "compose"
				}

				hasDependencySection = len(manifest.Dependencies) > 0
				hasBuildSection = len(manifest.Build) > 0
			}

			analytics.TrackDeploy(analytics.TrackDeployMetadata{
				Success:                err == nil,
				IsOktetoRepo:           utils.IsOktetoRepo(),
				Duration:               time.Since(startTime),
				PipelineType:           c.PipelineType,
				DeployType:             deployType,
				IsPreview:              os.Getenv(model.OktetoCurrentDeployBelongsToPreview) == "true",
				HasDependenciesSection: hasDependencySection,
				HasBuildSection:        hasBuildSection,
			})

			return err
		},
	}

	cmd.Flags().StringVar(&options.Name, "name", "", "development environment name")
	cmd.Flags().StringVarP(&options.ManifestPath, "file", "f", "", "path to the okteto manifest file")
	cmd.Flags().StringVarP(&options.Namespace, "namespace", "n", "", "overwrites the namespace where the development environment is deployed")
	cmd.Flags().StringVarP(&options.K8sContext, "context", "c", "", "context where the development environment is deployed")
	cmd.Flags().StringArrayVarP(&options.Variables, "var", "v", []string{}, "set a variable (can be set more than once)")
	cmd.Flags().BoolVarP(&options.Build, "build", "", false, "force build of images when deploying the development environment")
	cmd.Flags().BoolVarP(&options.Dependencies, "dependencies", "", false, "deploy the dependencies from manifest")

	cmd.Flags().BoolVarP(&options.Wait, "wait", "w", false, "wait until the development environment is deployed (defaults to false)")
	cmd.Flags().DurationVarP(&options.Timeout, "timeout", "t", (5 * time.Minute), "the length of time to wait for completion, zero means never. Any other values should contain a corresponding time unit e.g. 1s, 2m, 3h ")

	return cmd
}

// RunDeploy runs the deploy sequence
func (dc *DeployCommand) RunDeploy(ctx context.Context, deployOptions *Options) (*model.Manifest, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get the current working directory: %w", err)
	}

	c, _, err := dc.K8sClientProvider.Provide(okteto.Context().Cfg)
	if err != nil {
		return nil, err
	}

	if err := addEnvVars(ctx, cwd); err != nil {
		return nil, err
	}
	oktetoLog.Debugf("creating temporal kubeconfig file '%s'", dc.TempKubeconfigFile)
	if err := dc.Kubeconfig.Modify(dc.Proxy.GetPort(), dc.Proxy.GetToken(), dc.TempKubeconfigFile); err != nil {
		oktetoLog.Infof("could not create temporal kubeconfig %s", err)
		return nil, err
	}
	oktetoLog.SetStage("Load manifest")
	// TODO: See the usage of the manifest and come up with a custom struct that separates the "parsed yaml" from the "inferred options"
	manifest, err := dc.GetManifest(deployOptions.ManifestPath)
	if err != nil {
		return manifest, err
	}
	oktetoLog.Debug("found okteto manifest")

	if manifest.Deploy == nil {
		return manifest, oktetoErrors.ErrManifestFoundButNoDeployCommands
	}
	if len(deployOptions.servicesToDeploy) > 0 && manifest.Deploy.ComposeSection == nil {
		return manifest, oktetoErrors.ErrDeployCantDeploySvcsIfNotCompose
	}

	// Usage of manifest
	if err := setDeployOptionsValuesFromManifest(ctx, deployOptions, manifest, cwd, c); err != nil {
		return manifest, err
	}

	data := &pipeline.CfgData{
		Name:       deployOptions.Name,
		Namespace:  manifest.Namespace,
		Repository: os.Getenv(model.GithubRepositoryEnvVar),
		Branch:     os.Getenv(model.OktetoGitBranchEnvVar),
		Filename:   deployOptions.ManifestPathFlag,
		Status:     pipeline.ProgressingStatus,
		Manifest:   manifest.Manifest, // Usage of manifest
		Icon:       manifest.Icon,
	}

	// USAGE OF MANIFEST
	if !manifest.IsV2 && manifest.Type == model.StackType {
		data.Manifest = manifest.Deploy.ComposeSection.Stack.Manifest
	}

	dc.Proxy.SetName(deployOptions.Name)
	// don't divert if current namespace is the diverted namespace
	if manifest.Deploy.Divert != nil {
		if !okteto.IsOkteto() {
			return manifest, errors.ErrDivertNotSupported
		}
		if manifest.Deploy.Divert.Namespace != manifest.Namespace {
			dc.Proxy.SetDivert(manifest.Deploy.Divert.Namespace) // USAGE OF MANIFEST
		}
	}
	oktetoLog.SetStage("")

	dc.PipelineType = manifest.Type

	os.Setenv(model.OktetoNameEnvVar, deployOptions.Name)

	// USAGE OF MANIFEST
	if err := setDeployOptionsValuesFromManifest(ctx, deployOptions, manifest, cwd, c); err != nil {
		return manifest, err
	}

	// starting PROXY
	oktetoLog.Debugf("starting server on %d", dc.Proxy.GetPort())
	dc.Proxy.Start()

	cfg, err := getConfigMapFromData(ctx, data, c)
	if err != nil {
		return manifest, err
	}

	// USAGE OF MANIFEST
	// TODO: take this out to a new function deploy dependencies
	for depName, dep := range manifest.Dependencies {
		oktetoLog.Information("Deploying dependency '%s'", depName)
		dep.Variables = append(dep.Variables, model.EnvVar{
			Name:  "OKTETO_ORIGIN",
			Value: "okteto-deploy",
		})
		pipOpts := &pipelineCMD.DeployOptions{
			Name:         depName,
			Repository:   dep.Repository,
			Branch:       dep.Branch,
			File:         dep.ManifestPath,
			Variables:    model.SerializeBuildArgs(dep.Variables),
			Wait:         dep.Wait,
			Timeout:      deployOptions.Timeout,
			SkipIfExists: !deployOptions.Dependencies,
		}
		pc, err := pipelineCMD.NewCommand()
		if err != nil {
			return manifest, err
		}
		if err := pc.ExecuteDeployPipeline(ctx, pipOpts); err != nil {
			if errStatus := updateConfigMapStatus(ctx, cfg, c, data, err); errStatus != nil {
				return manifest, errStatus
			}

			return manifest, err
		}
	}

	// USAGE OF MANIFEST
	// TODO: take this out to a new function build images
	if deployOptions.Build {
		buildOptions := &types.BuildOptions{
			EnableStages: true,
			Manifest:     manifest,
			CommandArgs:  deployOptions.servicesToDeploy,
		}
		oktetoLog.Debug("force build from manifest definition")
		if errBuild := dc.Builder.Build(ctx, buildOptions); errBuild != nil {
			return manifest, updateConfigMapStatusError(ctx, cfg, c, data, errBuild)
		}
	} else {
		svcsToBuild, errBuild := dc.Builder.GetServicesToBuild(ctx, manifest, deployOptions.servicesToDeploy)
		if errBuild != nil {
			return manifest, updateConfigMapStatusError(ctx, cfg, c, data, errBuild)
		}
		if len(svcsToBuild) != 0 {
			buildOptions := &types.BuildOptions{
				CommandArgs:  svcsToBuild,
				EnableStages: true,
				Manifest:     manifest,
			}

			if errBuild := dc.Builder.Build(ctx, buildOptions); errBuild != nil {
				return manifest, updateConfigMapStatusError(ctx, cfg, c, data, errBuild)
			}
		}
	}

	oktetoLog.AddToBuffer(oktetoLog.InfoLevel, "Deploying '%s'...", deployOptions.Name)

	defer dc.cleanUp(ctx)

	for _, variable := range deployOptions.Variables {
		value := strings.SplitN(variable, "=", 2)[1]
		if strings.TrimSpace(value) != "" {
			oktetoLog.AddMaskedWord(value)
		}
	}
	deployOptions.Variables = append(
		deployOptions.Variables,
		// Set KUBECONFIG environment variable as environment for the commands to be executed
		fmt.Sprintf("%s=%s", model.KubeConfigEnvVar, dc.TempKubeconfigFile),
		// Set OKTETO_WITHIN_DEPLOY_COMMAND_CONTEXT env variable, so all okteto commands ran inside this deploy
		// know they are running inside another okteto deploy
		fmt.Sprintf("%s=true", model.OktetoWithinDeployCommandContextEnvVar),
		// Set OKTETO_SKIP_CONFIG_CREDENTIALS_UPDATE env variable, so all the Okteto commands executed within this command execution
		// should not overwrite the server and the credentials in the kubeconfig
		fmt.Sprintf("%s=true", model.OktetoSkipConfigCredentialsUpdate),
		// Set OKTETO_DISABLE_SPINNER=true env variable, so all the Okteto commands disable spinner which leads to errors
		fmt.Sprintf("%s=true", model.OktetoDisableSpinnerEnvVar),
		// Set OKTETO_NAMESPACE=namespace-name env variable, so all the commandsruns on the same namespace
		fmt.Sprintf("%s=%s", model.OktetoNamespaceEnvVar, okteto.Context().Namespace),
	)
	oktetoLog.EnableMasking()
	err = dc.deploy(ctx, deployOptions, manifest) // USAGE OF MANIFEST
	oktetoLog.DisableMasking()
	oktetoLog.SetStage("done")
	oktetoLog.AddToBuffer(oktetoLog.InfoLevel, "EOF")
	oktetoLog.SetStage("")

	if err != nil {
		if err == oktetoErrors.ErrIntSig {
			return manifest, nil
		}
		err = oktetoErrors.UserError{
			E:    err,
			Hint: "Update the 'deploy' section of your okteto manifest and try again",
		}
		oktetoLog.AddToBuffer(oktetoLog.InfoLevel, err.Error())
		data.Status = pipeline.ErrorStatus
	} else {
		oktetoLog.SetStage("")
		hasDeployed, err := pipeline.HasDeployedSomething(ctx, deployOptions.Name, manifest.Namespace, c)
		if err != nil {
			return manifest, err
		}
		if hasDeployed {
			if deployOptions.Wait {
				if err := dc.wait(ctx, deployOptions, manifest.Name, manifest.Namespace); err != nil {
					return manifest, err
				}
			}
			if !utils.LoadBoolean(model.OktetoWithinDeployCommandContextEnvVar) {
				if err := dc.showEndpoints(ctx, &EndpointsOptions{Name: deployOptions.Name, Namespace: manifest.Namespace}); err != nil {
					oktetoLog.Infof("could not retrieve endpoints: %s", err)
				}
			}
			if deployOptions.ShowCTA {
				oktetoLog.Success(succesfullyDeployedmsg, deployOptions.Name)
				if oktetoLog.IsInteractive() {
					oktetoLog.Information("Run 'okteto up' to activate your development container")
				}
			}
			pipeline.AddDevAnnotations(ctx, manifest, c)
		}
		data.Status = pipeline.DeployedStatus
	}

	if err := pipeline.UpdateConfigMap(ctx, cfg, data, c); err != nil {
		return manifest, err
	}

	return manifest, err
}

func (dc *DeployCommand) deploy(ctx context.Context, opts *Options, manifest *model.Manifest) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	exit := make(chan error, 1)
	go func() {
		// deploy commands if any
		for _, command := range manifest.Deploy.Commands {
			oktetoLog.Information("Running %s", command.Name)
			oktetoLog.SetStage(command.Name)
			if err := dc.Executor.Execute(command, opts.Variables); err != nil {
				oktetoLog.AddToBuffer(oktetoLog.ErrorLevel, "error executing command '%s': %s", command.Name, err.Error())
				exit <- fmt.Errorf("error executing command '%s': %s", command.Name, err.Error())
				return
			}
			oktetoLog.SetStage("")
		}

		// deploy compose if any
		if manifest.Deploy.ComposeSection != nil {
			if err := dc.deployStack(ctx, opts, manifest.Deploy.ComposeSection); err != nil {
				exit <- err
				return
			}
		}

		// deploy endpoits if any
		if manifest.Deploy.Endpoints != nil {
			if err := dc.deployEndpoints(ctx, manifest.Name, manifest.Namespace, manifest.Deploy.Endpoints); err != nil {
				exit <- err
				return
			}
		}

		// deploy diver if any
		if manifest.Deploy.Divert != nil && manifest.Deploy.Divert.Namespace != manifest.Namespace {
			if err := dc.deployDivert(ctx, opts, manifest); err != nil {
				exit <- err
				return
			}
			oktetoLog.Success("Divert from '%s' successfully configured", manifest.Deploy.Divert.Namespace)
		}

		exit <- nil
	}()

	select {
	case <-stop:
		oktetoLog.Infof("CTRL+C received, starting shutdown sequence")
		sp := utils.NewSpinner("Shutting down...")
		sp.Start()
		defer sp.Stop()
		dc.Executor.CleanUp(oktetoErrors.ErrIntSig)
		return oktetoErrors.ErrIntSig
	case err := <-exit:
		return err
	}
}

func (dc *DeployCommand) deployStack(ctx context.Context, opts *Options, composeSection *model.ComposeSectionInfo) error {
	oktetoLog.SetStage("Deploying compose")
	defer oktetoLog.SetStage("")
	composeSectionInfo := composeSection
	composeSectionInfo.Stack.Namespace = okteto.Context().Namespace // TODO: stack is mixed with deploy, should be separated

	var composeFiles []string
	for _, composeInfo := range composeSectionInfo.ComposesInfo {
		composeFiles = append(composeFiles, composeInfo.File)
	}
	stackOpts := &stack.StackDeployOptions{
		StackPaths:       composeFiles,
		ForceBuild:       false,
		Wait:             opts.Wait,
		Timeout:          opts.Timeout,
		ServicesToDeploy: opts.servicesToDeploy,
		InsidePipeline:   true,
	}

	c, cfg, err := dc.K8sClientProvider.Provide(kubeconfig.Get([]string{dc.TempKubeconfigFile}))
	if err != nil {
		return err
	}
	stackCommand := stackCMD.DeployCommand{
		K8sClient:      c,
		Config:         cfg,
		IsInsideDeploy: true,
	}
	return stackCommand.RunDeploy(ctx, composeSectionInfo.Stack, stackOpts)
}

func (dc *DeployCommand) deployDivert(ctx context.Context, opts *Options, manifest *model.Manifest) error {
	oktetoLog.SetStage("Divert configuration")
	defer oktetoLog.SetStage("")

	sp := utils.NewSpinner(fmt.Sprintf("Diverting namespace %s...", manifest.Deploy.Divert.Namespace))
	sp.Start()
	defer sp.Stop()

	c, _, err := dc.K8sClientProvider.Provide(okteto.Context().Cfg)
	if err != nil {
		return err
	}

	result, err := c.NetworkingV1().Ingresses(manifest.Deploy.Divert.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	for i := range result.Items {
		select {
		case <-ctx.Done():
			oktetoLog.Infof("deployDivert context cancelled")
			return ctx.Err()
		default:
			sp.Update(fmt.Sprintf("Diverting ingress %s/%s...", result.Items[i].Namespace, result.Items[i].Name))
			if err := diverts.DivertIngress(ctx, manifest, &result.Items[i], c); err != nil {
				return err
			}
		}
	}
	return nil
}

func (dc *DeployCommand) deployEndpoints(ctx context.Context, name, namespace string, endpoints map[string]model.Endpoint) error {
	oktetoLog.SetStage("Endpoints configuration")
	defer oktetoLog.SetStage("")

	c, _, err := dc.K8sClientProvider.Provide(okteto.Context().Cfg)
	if err != nil {
		return err
	}

	iClient, err := ingresses.GetClient(ctx, c)
	if err != nil {
		return fmt.Errorf("error getting ingress client: %s", err.Error())
	}

	translateOptions := &ingresses.TranslateOptions{
		Namespace: namespace,
		Name:      name,
	}

	for endpointName, endpoint := range endpoints {
		ingress := ingresses.Translate(endpointName, endpoint, translateOptions)
		if err := iClient.Deploy(ctx, ingress); err != nil {
			return err
		}
	}

	return nil
}

func (dc *DeployCommand) cleanUp(ctx context.Context) {
	oktetoLog.Debugf("removing temporal kubeconfig file '%s'", dc.TempKubeconfigFile)
	if err := os.Remove(dc.TempKubeconfigFile); err != nil {
		oktetoLog.Infof("could not remove temporal kubeconfig file: %s", err)
	}

	oktetoLog.Debugf("stopping local server...")
	if err := dc.Proxy.Shutdown(ctx); err != nil {
		oktetoLog.Infof("could not stop local server: %s", err)
	}
}
