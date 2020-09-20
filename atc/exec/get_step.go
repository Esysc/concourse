package exec

import (
	"context"
	"fmt"
	"io"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerctx"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/exec/build"
	"github.com/concourse/concourse/atc/resource"
	"github.com/concourse/concourse/atc/runtime"
	"github.com/concourse/concourse/atc/worker"
	"github.com/concourse/concourse/tracing"
)

type ErrPipelineNotFound struct {
	PipelineName string
}

func (e ErrPipelineNotFound) Error() string {
	return fmt.Sprintf("pipeline '%s' not found", e.PipelineName)
}

type ErrResourceNotFound struct {
	ResourceName string
}

func (e ErrResourceNotFound) Error() string {
	return fmt.Sprintf("resource '%s' not found", e.ResourceName)
}

//go:generate counterfeiter . GetDelegateFactory

type GetDelegateFactory interface {
	GetDelegate(state RunState) GetDelegate
}

//go:generate counterfeiter . GetDelegate

type GetDelegate interface {
	ImageVersionDetermined(db.UsedResourceCache) error
	RedactImageSource(source atc.Source) (atc.Source, error)

	Stdout() io.Writer
	Stderr() io.Writer

	Initializing(lager.Logger)
	Starting(lager.Logger)
	Finished(lager.Logger, ExitStatus, runtime.VersionResult)
	SelectedWorker(lager.Logger, string)
	Errored(lager.Logger, string)

	UpdateVersion(lager.Logger, atc.GetPlan, runtime.VersionResult)
}

// GetStep will fetch a version of a resource on a worker that supports the
// resource type.
type GetStep struct {
	planID               atc.PlanID
	plan                 atc.GetPlan
	metadata             StepMetadata
	containerMetadata    db.ContainerMetadata
	resourceFactory      resource.ResourceFactory
	resourceCacheFactory db.ResourceCacheFactory
	strategy             worker.ContainerPlacementStrategy
	workerClient         worker.Client
	delegateFactory      GetDelegateFactory
	succeeded            bool
}

func NewGetStep(
	planID atc.PlanID,
	plan atc.GetPlan,
	metadata StepMetadata,
	containerMetadata db.ContainerMetadata,
	resourceFactory resource.ResourceFactory,
	resourceCacheFactory db.ResourceCacheFactory,
	strategy worker.ContainerPlacementStrategy,
	delegateFactory GetDelegateFactory,
	client worker.Client,
) Step {
	return &GetStep{
		planID:               planID,
		plan:                 plan,
		metadata:             metadata,
		containerMetadata:    containerMetadata,
		resourceFactory:      resourceFactory,
		resourceCacheFactory: resourceCacheFactory,
		strategy:             strategy,
		delegateFactory:      delegateFactory,
		workerClient:         client,
	}
}
func (step *GetStep) Run(ctx context.Context, state RunState) error {
	ctx, span := tracing.StartSpan(ctx, "get", tracing.Attrs{
		"team":     step.metadata.TeamName,
		"pipeline": step.metadata.PipelineName,
		"job":      step.metadata.JobName,
		"build":    step.metadata.BuildName,
		"resource": step.plan.Resource,
		"name":     step.plan.Name,
	})

	err := step.run(ctx, state)
	tracing.End(span, err)

	return err
}

func (step *GetStep) run(ctx context.Context, state RunState) error {
	logger := lagerctx.FromContext(ctx)
	logger = logger.Session("get-step", lager.Data{
		"step-name": step.plan.Name,
		"job-id":    step.metadata.JobID,
	})

	delegate := step.delegateFactory.GetDelegate(state)
	delegate.Initializing(logger)

	source, err := creds.NewSource(state, step.plan.Source).Evaluate()
	if err != nil {
		return err
	}

	params, err := creds.NewParams(state, step.plan.Params).Evaluate()
	if err != nil {
		return err
	}

	resourceTypes, err := creds.NewVersionedResourceTypes(state, step.plan.VersionedResourceTypes).Evaluate()
	if err != nil {
		return err
	}

	version, err := NewVersionSourceFromPlan(&step.plan).Version(state)
	if err != nil {
		return err
	}

	containerSpec := worker.ContainerSpec{
		ImageSpec: worker.ImageSpec{
			ResourceType: step.plan.Type,
		},
		TeamID: step.metadata.TeamID,
		Env:    step.metadata.Env(),
	}
	tracing.Inject(ctx, &containerSpec)

	workerSpec := worker.WorkerSpec{
		ResourceType:  step.plan.Type,
		Tags:          step.plan.Tags,
		TeamID:        step.metadata.TeamID,
		ResourceTypes: resourceTypes,
	}

	imageSpec := worker.ImageFetcherSpec{
		ResourceTypes: resourceTypes,
		Delegate:      delegate,
	}

	resourceCache, err := step.resourceCacheFactory.FindOrCreateResourceCache(
		db.ForBuild(step.metadata.BuildID),
		step.plan.Type,
		version,
		source,
		params,
		resourceTypes,
	)
	if err != nil {
		logger.Error("failed-to-create-resource-cache", err)
		return err
	}

	processSpec := runtime.ProcessSpec{
		Path:         "/opt/resource/in",
		Args:         []string{resource.ResourcesDir("get")},
		StdoutWriter: delegate.Stdout(),
		StderrWriter: delegate.Stderr(),
	}

	resourceToGet := step.resourceFactory.NewResource(
		source,
		params,
		version,
	)

	containerOwner := db.NewBuildStepContainerOwner(step.metadata.BuildID, step.planID, step.metadata.TeamID)

	getResult, err := step.workerClient.RunGetStep(
		ctx,
		logger,
		containerOwner,
		containerSpec,
		workerSpec,
		step.strategy,
		step.containerMetadata,
		imageSpec,
		processSpec,
		delegate,
		resourceCache,
		resourceToGet,
	)
	if err != nil {
		return err
	}

	if getResult.ExitStatus == 0 {
		state.ArtifactRepository().RegisterArtifact(
			build.ArtifactName(step.plan.Name),
			getResult.GetArtifact,
		)

		if step.plan.Resource != "" {
			delegate.UpdateVersion(logger, step.plan, getResult.VersionResult)
		}

		step.succeeded = true
	}

	delegate.Finished(
		logger,
		ExitStatus(getResult.ExitStatus),
		getResult.VersionResult,
	)

	return nil
}

// Succeeded returns true if the resource was successfully fetched.
func (step *GetStep) Succeeded() bool {
	return step.succeeded
}
