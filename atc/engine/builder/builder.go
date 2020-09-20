package builder

import (
	"code.cloudfoundry.org/lager"

	"errors"
	"strconv"
	"strings"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/exec"
)

const supportedSchema = "exec.v2"

//go:generate counterfeiter . StepFactory

type StepFactory interface {
	GetStep(atc.Plan, exec.StepMetadata, db.ContainerMetadata, DelegateFactory) exec.Step
	PutStep(atc.Plan, exec.StepMetadata, db.ContainerMetadata, DelegateFactory) exec.Step
	TaskStep(atc.Plan, exec.StepMetadata, db.ContainerMetadata, DelegateFactory) exec.Step
	CheckStep(atc.Plan, exec.StepMetadata, db.ContainerMetadata, DelegateFactory) exec.Step
	SetPipelineStep(atc.Plan, exec.StepMetadata, DelegateFactory) exec.Step
	LoadVarStep(atc.Plan, exec.StepMetadata, DelegateFactory) exec.Step
	ArtifactInputStep(atc.Plan, db.Build) exec.Step
	ArtifactOutputStep(atc.Plan, db.Build) exec.Step
}

func NewStepBuilder(
	stepFactory StepFactory,
	externalURL string,
) *stepBuilder {
	return &stepBuilder{
		stepFactory: stepFactory,
		externalURL: externalURL,
	}
}

type stepBuilder struct {
	stepFactory     StepFactory
	delegateFactory DelegateFactory
	externalURL     string
}

func (builder *stepBuilder) BuildStep(logger lager.Logger, build db.Build) (exec.Step, error) {
	if build == nil {
		return exec.IdentityStep{}, errors.New("must provide a build")
	}

	if build.Schema() != supportedSchema {
		return exec.IdentityStep{}, errors.New("schema not supported")
	}

	return builder.buildStep(build, build.PrivatePlan()), nil
}

func (builder *stepBuilder) CheckStep(logger lager.Logger, check db.Check) (exec.Step, error) {
	if check == nil {
		return exec.IdentityStep{}, errors.New("must provide a check")
	}

	if check.Schema() != supportedSchema {
		return exec.IdentityStep{}, errors.New("schema not supported")
	}

	return builder.buildCheckStep(check, check.Plan()), nil
}

func (builder *stepBuilder) buildStep(build db.Build, plan atc.Plan) exec.Step {
	if plan.Aggregate != nil {
		return builder.buildAggregateStep(build, plan)
	}

	if plan.InParallel != nil {
		return builder.buildParallelStep(build, plan)
	}

	if plan.Across != nil {
		return builder.buildAcrossStep(build, plan)
	}

	if plan.Do != nil {
		return builder.buildDoStep(build, plan)
	}

	if plan.Timeout != nil {
		return builder.buildTimeoutStep(build, plan)
	}

	if plan.Try != nil {
		return builder.buildTryStep(build, plan)
	}

	if plan.OnAbort != nil {
		return builder.buildOnAbortStep(build, plan)
	}

	if plan.OnError != nil {
		return builder.buildOnErrorStep(build, plan)
	}

	if plan.OnSuccess != nil {
		return builder.buildOnSuccessStep(build, plan)
	}

	if plan.OnFailure != nil {
		return builder.buildOnFailureStep(build, plan)
	}

	if plan.Ensure != nil {
		return builder.buildEnsureStep(build, plan)
	}

	if plan.Task != nil {
		return builder.buildTaskStep(build, plan)
	}

	if plan.SetPipeline != nil {
		return builder.buildSetPipelineStep(build, plan)
	}

	if plan.LoadVar != nil {
		return builder.buildLoadVarStep(build, plan)
	}

	if plan.Get != nil {
		return builder.buildGetStep(build, plan)
	}

	if plan.Put != nil {
		return builder.buildPutStep(build, plan)
	}

	if plan.Retry != nil {
		return builder.buildRetryStep(build, plan)
	}

	if plan.ArtifactInput != nil {
		return builder.buildArtifactInputStep(build, plan)
	}

	if plan.ArtifactOutput != nil {
		return builder.buildArtifactOutputStep(build, plan)
	}

	return exec.IdentityStep{}
}

func (builder *stepBuilder) buildAggregateStep(build db.Build, plan atc.Plan) exec.Step {

	agg := exec.AggregateStep{}

	for _, innerPlan := range *plan.Aggregate {
		innerPlan.Attempts = plan.Attempts
		step := builder.buildStep(build, innerPlan)
		agg = append(agg, step)
	}

	return agg
}

func (builder *stepBuilder) buildParallelStep(build db.Build, plan atc.Plan) exec.Step {

	var steps []exec.Step

	for _, innerPlan := range plan.InParallel.Steps {
		innerPlan.Attempts = plan.Attempts
		step := builder.buildStep(build, innerPlan)
		steps = append(steps, step)
	}

	return exec.InParallel(steps, plan.InParallel.Limit, plan.InParallel.FailFast)
}

func (builder *stepBuilder) buildAcrossStep(build db.Build, plan atc.Plan) exec.Step {
	stepMetadata := builder.stepMetadata(
		build,
		builder.externalURL,
	)

	varNames := make([]string, len(plan.Across.Vars))
	for i, v := range plan.Across.Vars {
		varNames[i] = v.Var
	}

	return exec.Across(
		step,
		varNames,
		buildDelegateFactory(build, plan.ID, buildVars),
		stepMetadata,
	)
}

func (builder *stepBuilder) buildAcrossInParallelStep(build db.Build, varIndex int, plan atc.AcrossPlan, buildVars *vars.BuildVariables) exec.InParallelStep {
	if varIndex == len(plan.Vars)-1 {
		var steps []exec.Step
		for _, step := range plan.Steps {
			scopedBuildVars := buildVars.NewLocalScope()
			for i, v := range plan.Vars {
				// Don't redact because the `list` operation of a var_source should return identifiers
				// which should be publicly accessible. For static across steps, the static list is
				// embedded directly in the pipeline
				scopedBuildVars.AddLocalVar(v.Var, step.Values[i], false)
			}
			steps = append(steps, builder.buildStep(build, step.Step, scopedBuildVars))
		}
		return exec.InParallel(steps, plan.Vars[varIndex].MaxInFlight, plan.FailFast)
	}
	stepsPerValue := 1
	for _, v := range plan.Vars[varIndex+1:] {
		stepsPerValue *= len(v.Values)
	}
	numValues := len(plan.Vars[varIndex].Values)
	substeps := make([]exec.Step, numValues)
	for i := range substeps {
		startIndex := i * stepsPerValue
		endIndex := (i + 1) * stepsPerValue
		planCopy := plan
		planCopy.Steps = plan.Steps[startIndex:endIndex]
		substeps[i] = builder.buildAcrossInParallelStep(build, varIndex+1, planCopy, buildVars)
	}
	return exec.InParallel(substeps, plan.Vars[varIndex].MaxInFlight, plan.FailFast)
}

func (builder *stepBuilder) buildDoStep(build db.Build, plan atc.Plan) exec.Step {
	var step exec.Step = exec.IdentityStep{}

	for i := len(*plan.Do) - 1; i >= 0; i-- {
		innerPlan := (*plan.Do)[i]
		innerPlan.Attempts = plan.Attempts
		previous := builder.buildStep(build, innerPlan)
		step = exec.OnSuccess(previous, step)
	}

	return step
}

func (builder *stepBuilder) buildTimeoutStep(build db.Build, plan atc.Plan) exec.Step {
	innerPlan := plan.Timeout.Step
	innerPlan.Attempts = plan.Attempts
	step := builder.buildStep(build, innerPlan)
	return exec.Timeout(step, plan.Timeout.Duration)
}

func (builder *stepBuilder) buildTryStep(build db.Build, plan atc.Plan) exec.Step {
	innerPlan := plan.Try.Step
	innerPlan.Attempts = plan.Attempts
	step := builder.buildStep(build, innerPlan)
	return exec.Try(step)
}

func (builder *stepBuilder) buildOnAbortStep(build db.Build, plan atc.Plan) exec.Step {
	plan.OnAbort.Step.Attempts = plan.Attempts
	step := builder.buildStep(build, plan.OnAbort.Step)
	plan.OnAbort.Next.Attempts = plan.Attempts
	next := builder.buildStep(build, plan.OnAbort.Next)
	return exec.OnAbort(step, next)
}

func (builder *stepBuilder) buildOnErrorStep(build db.Build, plan atc.Plan) exec.Step {
	plan.OnError.Step.Attempts = plan.Attempts
	step := builder.buildStep(build, plan.OnError.Step)
	plan.OnError.Next.Attempts = plan.Attempts
	next := builder.buildStep(build, plan.OnError.Next)
	return exec.OnError(step, next)
}

func (builder *stepBuilder) buildOnSuccessStep(build db.Build, plan atc.Plan) exec.Step {
	plan.OnSuccess.Step.Attempts = plan.Attempts
	step := builder.buildStep(build, plan.OnSuccess.Step)
	plan.OnSuccess.Next.Attempts = plan.Attempts
	next := builder.buildStep(build, plan.OnSuccess.Next)
	return exec.OnSuccess(step, next)
}

func (builder *stepBuilder) buildOnFailureStep(build db.Build, plan atc.Plan) exec.Step {
	plan.OnFailure.Step.Attempts = plan.Attempts
	step := builder.buildStep(build, plan.OnFailure.Step)
	plan.OnFailure.Next.Attempts = plan.Attempts
	next := builder.buildStep(build, plan.OnFailure.Next)
	return exec.OnFailure(step, next)
}

func (builder *stepBuilder) buildEnsureStep(build db.Build, plan atc.Plan) exec.Step {
	plan.Ensure.Step.Attempts = plan.Attempts
	step := builder.buildStep(build, plan.Ensure.Step)
	plan.Ensure.Next.Attempts = plan.Attempts
	next := builder.buildStep(build, plan.Ensure.Next)
	return exec.Ensure(step, next)
}

func (builder *stepBuilder) buildRetryStep(build db.Build, plan atc.Plan) exec.Step {
	steps := []exec.Step{}

	for index, innerPlan := range *plan.Retry {
		innerPlan.Attempts = append(plan.Attempts, index+1)

		step := builder.buildStep(build, innerPlan)
		steps = append(steps, step)
	}

	return exec.Retry(steps...)
}

func (builder *stepBuilder) buildGetStep(build db.Build, plan atc.Plan) exec.Step {

	containerMetadata := builder.containerMetadata(
		build,
		db.ContainerTypeGet,
		plan.Get.Name,
		plan.Attempts,
	)

	stepMetadata := builder.stepMetadata(
		build,
		builder.externalURL,
	)

	return builder.stepFactory.GetStep(
		plan,
		stepMetadata,
		containerMetadata,
		buildDelegateFactory(build, plan.ID),
	)
}

func (builder *stepBuilder) buildPutStep(build db.Build, plan atc.Plan) exec.Step {

	containerMetadata := builder.containerMetadata(
		build,
		db.ContainerTypePut,
		plan.Put.Name,
		plan.Attempts,
	)

	stepMetadata := builder.stepMetadata(
		build,
		builder.externalURL,
	)

	return builder.stepFactory.PutStep(
		plan,
		stepMetadata,
		containerMetadata,
		buildDelegateFactory(build, plan.ID),
	)
}

func (builder *stepBuilder) buildCheckStep(check db.Check, plan atc.Plan) exec.Step {

	containerMetadata := db.ContainerMetadata{
		Type: db.ContainerTypeCheck,
	}

	stepMetadata := exec.StepMetadata{
		TeamID:                check.TeamID(),
		TeamName:              check.TeamName(),
		PipelineID:            check.PipelineID(),
		PipelineName:          check.PipelineName(),
		ResourceConfigScopeID: check.ResourceConfigScopeID(),
		ResourceConfigID:      check.ResourceConfigID(),
		BaseResourceTypeID:    check.BaseResourceTypeID(),
		ExternalURL:           builder.externalURL,
	}

	return builder.stepFactory.CheckStep(
		plan,
		stepMetadata,
		containerMetadata,
		checkDelegateFactory(check, plan.ID),
	)
}

func (builder *stepBuilder) buildTaskStep(build db.Build, plan atc.Plan) exec.Step {

	containerMetadata := builder.containerMetadata(
		build,
		db.ContainerTypeTask,
		plan.Task.Name,
		plan.Attempts,
	)

	stepMetadata := builder.stepMetadata(
		build,
		builder.externalURL,
	)

	return builder.stepFactory.TaskStep(
		plan,
		stepMetadata,
		containerMetadata,
		buildDelegateFactory(build, plan.ID),
	)
}

func (builder *stepBuilder) buildSetPipelineStep(build db.Build, plan atc.Plan) exec.Step {

	stepMetadata := builder.stepMetadata(
		build,
		builder.externalURL,
	)

	return builder.stepFactory.SetPipelineStep(
		plan,
		stepMetadata,
		buildDelegateFactory(build, plan.ID),
	)
}

func (builder *stepBuilder) buildLoadVarStep(build db.Build, plan atc.Plan) exec.Step {

	stepMetadata := builder.stepMetadata(
		build,
		builder.externalURL,
	)

	return builder.stepFactory.LoadVarStep(
		plan,
		stepMetadata,
		buildDelegateFactory(build, plan.ID),
	)
}

func (builder *stepBuilder) buildArtifactInputStep(build db.Build, plan atc.Plan) exec.Step {
	return builder.stepFactory.ArtifactInputStep(
		plan,
		build,
	)
}

func (builder *stepBuilder) buildArtifactOutputStep(build db.Build, plan atc.Plan) exec.Step {
	return builder.stepFactory.ArtifactOutputStep(
		plan,
		build,
	)
}

func (builder *stepBuilder) containerMetadata(
	build db.Build,
	containerType db.ContainerType,
	stepName string,
	attempts []int,
) db.ContainerMetadata {
	attemptStrs := []string{}
	for _, a := range attempts {
		attemptStrs = append(attemptStrs, strconv.Itoa(a))
	}

	return db.ContainerMetadata{
		Type: containerType,

		PipelineID: build.PipelineID(),
		JobID:      build.JobID(),
		BuildID:    build.ID(),

		PipelineName: build.PipelineName(),
		JobName:      build.JobName(),
		BuildName:    build.Name(),

		StepName: stepName,
		Attempt:  strings.Join(attemptStrs, "."),
	}
}

func (builder *stepBuilder) stepMetadata(
	build db.Build,
	externalURL string,
) exec.StepMetadata {
	return exec.StepMetadata{
		BuildID:      build.ID(),
		BuildName:    build.Name(),
		TeamID:       build.TeamID(),
		TeamName:     build.TeamName(),
		JobID:        build.JobID(),
		JobName:      build.JobName(),
		PipelineID:   build.PipelineID(),
		PipelineName: build.PipelineName(),
		ExternalURL:  externalURL,
	}
}
