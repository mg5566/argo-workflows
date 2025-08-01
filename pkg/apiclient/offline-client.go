package apiclient

import (
	"context"
	"fmt"

	"github.com/argoproj/argo-workflows/v3/pkg/apiclient/clusterworkflowtemplate"
	"github.com/argoproj/argo-workflows/v3/pkg/apiclient/cronworkflow"
	infopkg "github.com/argoproj/argo-workflows/v3/pkg/apiclient/info"
	workflowpkg "github.com/argoproj/argo-workflows/v3/pkg/apiclient/workflow"
	workflowarchivepkg "github.com/argoproj/argo-workflows/v3/pkg/apiclient/workflowarchive"
	"github.com/argoproj/argo-workflows/v3/pkg/apiclient/workflowtemplate"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/util/file"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
	"github.com/argoproj/argo-workflows/v3/workflow/templateresolution"
)

type offlineWorkflowTemplateGetterMap map[string]templateresolution.WorkflowTemplateNamespacedGetter

func (m offlineWorkflowTemplateGetterMap) GetNamespaceGetter(namespace string) templateresolution.WorkflowTemplateNamespacedGetter {
	v := m[namespace]
	if v == nil {
		return offlineWorkflowTemplateNamespacedGetter{
			workflowTemplates: map[string]*wfv1.WorkflowTemplate{},
			namespace:         namespace,
		}
	}

	return m[namespace]
}

type offlineClient struct {
	clusterWorkflowTemplateGetter       templateresolution.ClusterWorkflowTemplateGetter
	namespacedWorkflowTemplateGetterMap offlineWorkflowTemplateGetterMap
}

var ErrOffline = fmt.Errorf("not supported when you are in offline mode")

var _ Client = &offlineClient{}

// newOfflineClient creates a client that keeps all files (or files recursively contained within a path) given to it in memory.
// It is useful for linting a set of files without having to connect to a cluster.
func newOfflineClient(ctx context.Context, paths []string) (context.Context, Client, error) {
	clusterWorkflowTemplateGetter := &offlineClusterWorkflowTemplateGetter{
		clusterWorkflowTemplates: map[string]*wfv1.ClusterWorkflowTemplate{},
	}
	workflowTemplateGetters := offlineWorkflowTemplateGetterMap{}
	for _, basePath := range paths {
		err := file.WalkManifests(ctx, basePath, func(path string, bytes []byte) error {
			for _, pr := range common.ParseObjects(ctx, bytes, false) {
				obj, err := pr.Object, pr.Err
				if err != nil {
					return fmt.Errorf("failed to parse YAML from file %s: %w", path, err)
				}

				if obj == nil {
					continue // could not parse to kubernetes object
				}

				objName := obj.GetName()
				namespace := obj.GetNamespace()

				switch v := obj.(type) {
				case *wfv1.ClusterWorkflowTemplate:
					if _, ok := clusterWorkflowTemplateGetter.clusterWorkflowTemplates[objName]; ok {
						return fmt.Errorf("duplicate ClusterWorkflowTemplate found: %q", objName)
					}
					clusterWorkflowTemplateGetter.clusterWorkflowTemplates[objName] = v

				case *wfv1.WorkflowTemplate:
					getter, ok := workflowTemplateGetters[namespace]
					if !ok {
						getter = &offlineWorkflowTemplateNamespacedGetter{
							namespace:         namespace,
							workflowTemplates: map[string]*wfv1.WorkflowTemplate{},
						}
						workflowTemplateGetters[namespace] = getter
					}

					if _, ok := getter.(*offlineWorkflowTemplateNamespacedGetter).workflowTemplates[objName]; ok {
						return fmt.Errorf("duplicate WorkflowTemplate found: %q", objName)
					}
					getter.(*offlineWorkflowTemplateNamespacedGetter).workflowTemplates[objName] = v
				}

			}
			return nil
		})

		if err != nil {
			return nil, nil, err
		}
	}

	return ctx, &offlineClient{
		clusterWorkflowTemplateGetter:       clusterWorkflowTemplateGetter,
		namespacedWorkflowTemplateGetterMap: workflowTemplateGetters,
	}, nil
}

func (c *offlineClient) NewWorkflowServiceClient(_ context.Context) workflowpkg.WorkflowServiceClient {
	return &errorTranslatingWorkflowServiceClient{OfflineWorkflowServiceClient{
		clusterWorkflowTemplateGetter:       c.clusterWorkflowTemplateGetter,
		namespacedWorkflowTemplateGetterMap: c.namespacedWorkflowTemplateGetterMap,
	}}
}

func (c *offlineClient) NewCronWorkflowServiceClient() (cronworkflow.CronWorkflowServiceClient, error) {
	return &errorTranslatingCronWorkflowServiceClient{OfflineCronWorkflowServiceClient{
		clusterWorkflowTemplateGetter:       c.clusterWorkflowTemplateGetter,
		namespacedWorkflowTemplateGetterMap: c.namespacedWorkflowTemplateGetterMap,
	}}, nil
}

func (c *offlineClient) NewWorkflowTemplateServiceClient() (workflowtemplate.WorkflowTemplateServiceClient, error) {
	return &errorTranslatingWorkflowTemplateServiceClient{OfflineWorkflowTemplateServiceClient{
		clusterWorkflowTemplateGetter:       c.clusterWorkflowTemplateGetter,
		namespacedWorkflowTemplateGetterMap: c.namespacedWorkflowTemplateGetterMap,
	}}, nil
}

func (c *offlineClient) NewClusterWorkflowTemplateServiceClient() (clusterworkflowtemplate.ClusterWorkflowTemplateServiceClient, error) {
	return &errorTranslatingWorkflowClusterTemplateServiceClient{OfflineClusterWorkflowTemplateServiceClient{
		clusterWorkflowTemplateGetter:       c.clusterWorkflowTemplateGetter,
		namespacedWorkflowTemplateGetterMap: c.namespacedWorkflowTemplateGetterMap,
	}}, nil
}

func (c *offlineClient) NewArchivedWorkflowServiceClient() (workflowarchivepkg.ArchivedWorkflowServiceClient, error) {
	return nil, ErrNoArgoServer
}

func (c *offlineClient) NewInfoServiceClient() (infopkg.InfoServiceClient, error) {
	return nil, ErrNoArgoServer
}

type offlineWorkflowTemplateNamespacedGetter struct {
	namespace         string
	workflowTemplates map[string]*wfv1.WorkflowTemplate
}

func (w offlineWorkflowTemplateNamespacedGetter) Get(_ context.Context, name string) (*wfv1.WorkflowTemplate, error) {
	if v, ok := w.workflowTemplates[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("couldn't find workflow template %q in namespace %q", name, w.namespace)
}

type offlineClusterWorkflowTemplateGetter struct {
	clusterWorkflowTemplates map[string]*wfv1.ClusterWorkflowTemplate
}

func (o offlineClusterWorkflowTemplateGetter) Get(_ context.Context, name string) (*wfv1.ClusterWorkflowTemplate, error) {
	if v, ok := o.clusterWorkflowTemplates[name]; ok {
		return v, nil
	}

	return nil, fmt.Errorf("couldn't find cluster workflow template %q", name)
}
