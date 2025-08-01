package controller

import (
	"context"
	"encoding/json"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/util/expr/argoexpr"
	"github.com/argoproj/argo-workflows/v3/util/expr/env"
	"github.com/argoproj/argo-workflows/v3/util/template"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
	"github.com/argoproj/argo-workflows/v3/workflow/templateresolution"
)

func (woc *wfOperationCtx) runOnExitNode(ctx context.Context, exitHook *wfv1.LifecycleHook, parentNode *wfv1.NodeStatus, boundaryID string, tmplCtx *templateresolution.TemplateContext, prefix string, scope *wfScope) (bool, *wfv1.NodeStatus, error) {
	outputs := parentNode.Outputs
	if lastChildNode := woc.possiblyGetRetryChildNode(parentNode); lastChildNode != nil {
		outputs = lastChildNode.Outputs
	}

	if exitHook != nil && woc.GetShutdownStrategy().ShouldExecute(true) {
		execute := true
		var err error
		if exitHook.Expression != "" {
			execute, err = argoexpr.EvalBool(exitHook.Expression, env.GetFuncMap(template.EnvMap(woc.globalParams.Merge(scope.getParameters()))))
			if err != nil {
				return true, nil, err
			}
		}
		if execute {
			woc.log.WithField("lifeCycleHook", exitHook).Infof(ctx, "Running OnExit handler")
			onExitNodeName := common.GenerateOnExitNodeName(parentNode.Name)
			resolvedArgs := exitHook.Arguments
			if !resolvedArgs.IsEmpty() {
				resolvedArgs, err = woc.resolveExitTmplArgument(ctx, exitHook.Arguments, prefix, outputs, scope)
				if err != nil {
					return true, nil, err
				}

			}
			onExitNode, err := woc.executeTemplate(ctx, onExitNodeName, &wfv1.WorkflowStep{Template: exitHook.Template, TemplateRef: exitHook.TemplateRef}, tmplCtx, resolvedArgs, &executeTemplateOpts{
				boundaryID:     boundaryID,
				onExitTemplate: true,
				nodeFlag:       &wfv1.NodeFlag{Hooked: true},
			})
			woc.addChildNode(ctx, parentNode.Name, onExitNodeName)
			return true, onExitNode, err
		}
	}
	return false, nil, nil
}

func (woc *wfOperationCtx) resolveExitTmplArgument(ctx context.Context, args wfv1.Arguments, prefix string, outputs *wfv1.Outputs, scope *wfScope) (wfv1.Arguments, error) {
	if scope == nil {
		scope = createScope(nil)
	}
	if outputs != nil {
		for _, param := range outputs.Parameters {
			value := ""
			if param.Value != nil {
				value = param.Value.String()
			}
			scope.addParamToScope(fmt.Sprintf("%s.outputs.parameters.%s", prefix, param.Name), value)
		}
		for _, arts := range outputs.Artifacts {
			scope.addArtifactToScope(fmt.Sprintf("%s.outputs.artifacts.%s", prefix, arts.Name), arts)
		}
	}

	stepBytes, err := json.Marshal(args)
	if err != nil {
		return args, err
	}
	newStepStr, err := template.Replace(ctx, string(stepBytes), woc.globalParams.Merge(scope.getParameters()), true)
	if err != nil {
		return args, err
	}
	var newArgs wfv1.Arguments
	err = json.Unmarshal([]byte(newStepStr), &newArgs)
	if err != nil {
		return args, err
	}
	// Step 2: replace all artifact references
	for j, art := range newArgs.Artifacts {
		if art.From == "" && art.FromExpression == "" {
			continue
		}
		resolvedArt, err := scope.resolveArtifact(ctx, &art)
		if err != nil {
			if art.Optional {
				continue
			}
			return args, fmt.Errorf("unable to resolve references: %s", err)
		}
		resolvedArt.Name = art.Name
		newArgs.Artifacts[j] = *resolvedArt
	}
	return newArgs, nil
}
