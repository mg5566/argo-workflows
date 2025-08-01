package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	gosync "sync"
	"time"

	"github.com/upper/db/v4"

	syncpkg "github.com/argoproj/pkg/sync"
	"golang.org/x/time/rate"
	apiv1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	v1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	apiwatch "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/workqueue"

	"github.com/argoproj/argo-workflows/v3/util/logging"

	"github.com/argoproj/argo-workflows/v3"
	"github.com/argoproj/argo-workflows/v3/config"
	argoErr "github.com/argoproj/argo-workflows/v3/errors"
	"github.com/argoproj/argo-workflows/v3/persist/sqldb"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	wfclientset "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	wfextvv1alpha1 "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/pkg/plugins/spec"
	authutil "github.com/argoproj/argo-workflows/v3/util/auth"
	wfctx "github.com/argoproj/argo-workflows/v3/util/context"
	"github.com/argoproj/argo-workflows/v3/util/deprecation"
	"github.com/argoproj/argo-workflows/v3/util/env"
	"github.com/argoproj/argo-workflows/v3/util/errors"
	"github.com/argoproj/argo-workflows/v3/util/retry"
	"github.com/argoproj/argo-workflows/v3/util/telemetry"
	waitutil "github.com/argoproj/argo-workflows/v3/util/wait"
	"github.com/argoproj/argo-workflows/v3/workflow/artifactrepositories"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
	controllercache "github.com/argoproj/argo-workflows/v3/workflow/controller/cache"
	"github.com/argoproj/argo-workflows/v3/workflow/controller/entrypoint"
	"github.com/argoproj/argo-workflows/v3/workflow/controller/estimation"
	"github.com/argoproj/argo-workflows/v3/workflow/controller/indexes"
	"github.com/argoproj/argo-workflows/v3/workflow/controller/informer"
	"github.com/argoproj/argo-workflows/v3/workflow/controller/pod"
	"github.com/argoproj/argo-workflows/v3/workflow/cron"
	"github.com/argoproj/argo-workflows/v3/workflow/events"
	"github.com/argoproj/argo-workflows/v3/workflow/gccontroller"
	"github.com/argoproj/argo-workflows/v3/workflow/hydrator"
	"github.com/argoproj/argo-workflows/v3/workflow/metrics"
	"github.com/argoproj/argo-workflows/v3/workflow/sync"
	"github.com/argoproj/argo-workflows/v3/workflow/util"
	plugin "github.com/argoproj/argo-workflows/v3/workflow/util/plugins"
)

const maxAllowedStackDepth = 100

type recentlyCompletedWorkflow struct {
	key  string
	when time.Time
}

type recentCompletions struct {
	completions []recentlyCompletedWorkflow
	mutex       gosync.RWMutex
}

// WorkflowController is the controller for workflow resources
type WorkflowController struct {
	// namespace of the workflow controller
	namespace        string
	managedNamespace string

	configController config.Controller
	// Config is the workflow controller's configuration
	Config config.Config
	// get the artifact repository
	artifactRepositories artifactrepositories.Interface
	// get images
	entrypoint entrypoint.Interface

	// cliExecutorImage is the executor image as specified from the command line
	cliExecutorImage string

	// cliExecutorImagePullPolicy is the executor imagePullPolicy as specified from the command line
	cliExecutorImagePullPolicy string

	// cliExecutorLogFormat is the format in which argoexec will log
	// possible options are json/text
	cliExecutorLogFormat string

	// restConfig is used by controller to send a SIGUSR1 to the wait sidecar using remotecommand.NewSPDYExecutor().
	restConfig       *rest.Config
	kubeclientset    kubernetes.Interface
	rateLimiter      *rate.Limiter
	dynamicInterface dynamic.Interface
	wfclientset      wfclientset.Interface

	// maxStackDepth is a configurable limit to the depth of the "stack", which is increased with every nested call to
	// woc.executeTemplate and decreased when such calls return. This is used to prevent infinite recursion
	maxStackDepth int

	// datastructures to support the processing of workflows and workflow pods
	wfInformer            cache.SharedIndexInformer
	nsInformer            cache.SharedIndexInformer
	wftmplInformer        wfextvv1alpha1.WorkflowTemplateInformer
	cwftmplInformer       wfextvv1alpha1.ClusterWorkflowTemplateInformer
	PodController         *pod.Controller // Currently public for woc to access, but would rather an accessor
	configMapInformer     cache.SharedIndexInformer
	wfQueue               workqueue.TypedRateLimitingInterface[string]
	wfArchiveQueue        workqueue.TypedRateLimitingInterface[string]
	throttler             sync.Throttler
	workflowKeyLock       syncpkg.KeyLock // used to lock workflows for exclusive modification or access
	session               db.Session
	offloadNodeStatusRepo sqldb.OffloadNodeStatusRepo
	hydrator              hydrator.Interface
	wfArchive             sqldb.WorkflowArchive
	estimatorFactory      estimation.EstimatorFactory
	syncManager           *sync.Manager
	metrics               *metrics.Metrics
	eventRecorderManager  events.EventRecorderManager
	archiveLabelSelector  labels.Selector
	cacheFactory          controllercache.Factory
	wfTaskSetInformer     wfextvv1alpha1.WorkflowTaskSetInformer
	artGCTaskInformer     wfextvv1alpha1.WorkflowArtifactGCTaskInformer
	taskResultInformer    cache.SharedIndexInformer

	// progressPatchTickDuration defines how often the executor will patch pod annotations if an updated progress is found.
	// Default is 1m and can be configured using the env var ARGO_PROGRESS_PATCH_TICK_DURATION.
	progressPatchTickDuration time.Duration
	// progressFileTickDuration defines how often the progress file is read.
	// Default is 3s and can be configured using the env var ARGO_PROGRESS_FILE_TICK_DURATION
	progressFileTickDuration time.Duration
	executorPlugins          map[string]map[string]*spec.Plugin // namespace -> name -> plugin

	recentCompletions recentCompletions
	// lastUnreconciledWorkflows is a map of workflows that have been recently unreconciled
	lastUnreconciledWorkflows map[string]*wfv1.Workflow
}

const (
	workflowResyncPeriod                = 20 * time.Minute
	workflowTemplateResyncPeriod        = 20 * time.Minute
	podResyncPeriod                     = 30 * time.Minute
	clusterWorkflowTemplateResyncPeriod = 20 * time.Minute
	workflowExistenceCheckPeriod        = 1 * time.Minute
	workflowTaskSetResyncPeriod         = 20 * time.Minute
)

var (
	cacheGCPeriod = env.LookupEnvDurationOr(logging.InitLoggerInContext(), "CACHE_GC_PERIOD", 0)

	// semaphoreNotifyDelay is a slight delay when notifying/enqueueing workflows to the workqueue
	// that are waiting on a semaphore. This value is passed to AddAfter(). We delay adding the next
	// workflow because if we add immediately with AddRateLimited(), the next workflow will likely
	// be reconciled at a point in time before we have finished the current workflow reconciliation
	// as well as incrementing the semaphore counter availability, and so the next workflow will
	// believe it cannot run. By delaying for 1s, we would have finished the semaphore counter
	// updates, and the next workflow will see the updated availability.
	semaphoreNotifyDelay = env.LookupEnvDurationOr(logging.InitLoggerInContext(), "SEMAPHORE_NOTIFY_DELAY", time.Second)
)

func init() {
	if cacheGCPeriod != 0 {
		logging.InitLogger().WithField("cacheGCPeriod", cacheGCPeriod).Info(context.Background(), "GC for memoization caches will be performed every")
	}
}

// NewWorkflowController instantiates a new WorkflowController
func NewWorkflowController(ctx context.Context, restConfig *rest.Config, kubeclientset kubernetes.Interface, wfclientset wfclientset.Interface, namespace, managedNamespace, executorImage, executorImagePullPolicy, executorLogFormat, configMap string, executorPlugins bool) (*WorkflowController, error) {
	dynamicInterface, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	wfc := WorkflowController{
		restConfig:                 restConfig,
		kubeclientset:              kubeclientset,
		dynamicInterface:           dynamicInterface,
		wfclientset:                wfclientset,
		namespace:                  namespace,
		managedNamespace:           managedNamespace,
		cliExecutorImage:           executorImage,
		cliExecutorImagePullPolicy: executorImagePullPolicy,
		cliExecutorLogFormat:       executorLogFormat,
		configController:           config.NewController(namespace, configMap, kubeclientset),
		workflowKeyLock:            syncpkg.NewKeyLock(),
		cacheFactory:               controllercache.NewCacheFactory(kubeclientset, namespace),
		eventRecorderManager:       events.NewEventRecorderManager(kubeclientset),
		progressPatchTickDuration:  env.LookupEnvDurationOr(ctx, common.EnvVarProgressPatchTickDuration, 1*time.Minute),
		progressFileTickDuration:   env.LookupEnvDurationOr(ctx, common.EnvVarProgressFileTickDuration, 3*time.Second),
	}

	if executorPlugins {
		wfc.executorPlugins = map[string]map[string]*spec.Plugin{}
	}

	wfc.UpdateConfig(ctx)
	wfc.maxStackDepth = wfc.getMaxStackDepth()
	wfc.metrics, err = metrics.New(ctx,
		`workflows-controller`,
		`argo_workflows`,
		wfc.getMetricsServerConfig(),
		metrics.Callbacks{
			PodPhase:          wfc.getPodPhaseMetrics,
			WorkflowPhase:     wfc.getWorkflowPhaseMetrics,
			WorkflowCondition: wfc.getWorkflowConditionMetrics,
			IsLeader:          wfc.IsLeader,
		})
	if err != nil {
		return nil, err
	}

	deprecation.Initialize(wfc.metrics.DeprecatedFeature)
	wfc.entrypoint = entrypoint.New(kubeclientset, wfc.Config.Images)

	workqueue.SetProvider(wfc.metrics) // must execute SetProvider before we create the queues
	wfc.wfQueue = wfc.metrics.RateLimiterWithBusyWorkers(ctx, &fixedItemIntervalRateLimiter{}, "workflow_queue")
	wfc.throttler = wfc.newThrottler()
	wfc.wfArchiveQueue = wfc.metrics.RateLimiterWithBusyWorkers(ctx, workqueue.DefaultTypedControllerRateLimiter[string](), "workflow_archive_queue")

	return &wfc, nil
}

func (wfc *WorkflowController) newThrottler() sync.Throttler {
	f := func(key string) { wfc.wfQueue.Add(key) }
	return sync.NewMultiThrottler(wfc.Config.Parallelism, wfc.Config.NamespaceParallelism, f)
}

// runGCcontroller runs the workflow garbage collector controller
func (wfc *WorkflowController) runGCcontroller(ctx context.Context, workflowTTLWorkers int) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)
	gcCtrl := gccontroller.NewController(ctx, wfc.wfclientset, wfc.wfInformer, wfc.metrics, wfc.Config.RetentionPolicy)
	err := gcCtrl.Run(ctx, workflowTTLWorkers)
	if err != nil {
		panic(err)
	}
}

func (wfc *WorkflowController) runPodController(ctx context.Context, podGCWorkers int) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	wfc.PodController.Run(ctx, podGCWorkers)
}

func (wfc *WorkflowController) runCronController(ctx context.Context, cronWorkflowWorkers int) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	cronController := cron.NewCronController(ctx, wfc.wfclientset, wfc.dynamicInterface, wfc.namespace, wfc.GetManagedNamespace(), wfc.Config.InstanceID, wfc.metrics, wfc.eventRecorderManager, cronWorkflowWorkers, wfc.wftmplInformer, wfc.cwftmplInformer, wfc.Config.WorkflowDefaults)
	cronController.Run(ctx)
}

var indexers = cache.Indexers{
	indexes.ClusterWorkflowTemplateIndex: indexes.MetaNamespaceLabelIndexFunc(common.LabelKeyClusterWorkflowTemplate),
	indexes.CronWorkflowIndex:            indexes.MetaNamespaceLabelIndexFunc(common.LabelKeyCronWorkflow),
	indexes.WorkflowTemplateIndex:        indexes.MetaNamespaceLabelIndexFunc(common.LabelKeyWorkflowTemplate),
	indexes.SemaphoreConfigIndexName:     indexes.WorkflowSemaphoreKeysIndexFunc(),
	indexes.WorkflowPhaseIndex:           indexes.MetaWorkflowPhaseIndexFunc(),
	indexes.ConditionsIndex:              indexes.ConditionsIndexFunc,
	indexes.UIDIndex:                     indexes.MetaUIDFunc,
	cache.NamespaceIndex:                 cache.MetaNamespaceIndexFunc,
}

// Run starts a Workflow resource controller
func (wfc *WorkflowController) Run(ctx context.Context, wfWorkers, workflowTTLWorkers, podCleanupWorkers, cronWorkflowWorkers, wfArchiveWorkers int) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	logger := logging.RequireLoggerFromContext(ctx)
	// init DB after leader election (if enabled)
	if err := wfc.initDB(ctx); err != nil {
		logger.WithError(err).WithFatal().Error(ctx, "Failed to init db")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer wfc.wfQueue.ShutDown()

	logger.WithFields(argo.GetVersion().Fields()).WithFields(logging.Fields{
		"instanceID":         wfc.Config.InstanceID,
		"defaultRequeueTime": GetRequeueTime(),
	}).Info(ctx, "Starting Workflow Controller")
	logger.WithFields(logging.Fields{
		"workflowWorkers":     wfWorkers,
		"workflowTtlWorkers":  workflowTTLWorkers,
		"podCleanup":          podCleanupWorkers,
		"cronWorkflowWorkers": cronWorkflowWorkers,
		"workflowArchive":     wfArchiveWorkers,
	}).Info(ctx, "Current Worker Numbers")

	wfc.wfInformer = util.NewWorkflowInformer(ctx, wfc.dynamicInterface, wfc.GetManagedNamespace(), workflowResyncPeriod, wfc.tweakListRequestListOptions, wfc.tweakWatchRequestListOptions, indexers)
	nsInformer, err := wfc.newNamespaceInformer(ctx, wfc.kubeclientset)
	if err != nil {
		logger.WithError(err).WithFatal().Error(ctx, "Failed to create namespace informer")
	}
	wfc.nsInformer = nsInformer
	wfc.wftmplInformer = informer.NewTolerantWorkflowTemplateInformer(wfc.dynamicInterface, workflowTemplateResyncPeriod, wfc.managedNamespace)

	wfc.wfTaskSetInformer = wfc.newWorkflowTaskSetInformer()
	wfc.artGCTaskInformer = wfc.newArtGCTaskInformer()
	wfc.taskResultInformer = wfc.newWorkflowTaskResultInformer(ctx)
	err = wfc.addWorkflowInformerHandlers(ctx)
	if err != nil {
		logger.WithError(err).WithFatal().Error(ctx, "Failed to add workflow informer handlers")
	}
	wfc.PodController = pod.NewController(ctx, &wfc.Config, wfc.restConfig, wfc.GetManagedNamespace(), wfc.kubeclientset, wfc.wfInformer, wfc.metrics, wfc.enqueueWfFromPodLabel)

	wfc.updateEstimatorFactory(ctx)

	wfc.configMapInformer = wfc.newConfigMapInformer(ctx)

	// Create Synchronization Manager
	wfc.createSynchronizationManager(ctx)
	// init managers: throttler and SynchronizationManager
	if err := wfc.initManagers(ctx); err != nil {
		logger.WithError(err).WithFatal().Error(ctx, "Failed to init managers")
	}

	if os.Getenv("WATCH_CONTROLLER_SEMAPHORE_CONFIGMAPS") != "false" {
		go wfc.runConfigMapWatcher(ctx)
	}

	go wfc.nsInformer.Run(ctx.Done())
	go wfc.wfInformer.Run(ctx.Done())
	go wfc.wftmplInformer.Informer().Run(ctx.Done())
	go wfc.configMapInformer.Run(ctx.Done())
	go wfc.wfTaskSetInformer.Informer().Run(ctx.Done())
	go wfc.artGCTaskInformer.Informer().Run(ctx.Done())
	go wfc.taskResultInformer.Run(ctx.Done())
	wfc.createClusterWorkflowTemplateInformer(ctx)
	go wfc.runPodController(ctx, podCleanupWorkers)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(
		ctx.Done(),
		wfc.wfInformer.HasSynced,
		wfc.nsInformer.HasSynced,
		wfc.wftmplInformer.Informer().HasSynced,
		wfc.PodController.HasSynced(),
		wfc.configMapInformer.HasSynced,
		wfc.wfTaskSetInformer.Informer().HasSynced,
		wfc.artGCTaskInformer.Informer().HasSynced,
		wfc.taskResultInformer.HasSynced,
	) {
		logger.WithFatal().Error(ctx, "Timed out waiting for caches to sync")
	}

	go wfc.workflowGarbageCollector(ctx)
	go wfc.archivedWorkflowGarbageCollector(ctx)

	go wfc.runGCcontroller(ctx, workflowTTLWorkers)
	go wfc.runCronController(ctx, cronWorkflowWorkers)

	go wait.UntilWithContext(ctx, wfc.syncManager.CheckWorkflowExistence, workflowExistenceCheckPeriod)

	workerCtx, _ := logger.WithField("component", "workflow_worker").InContext(ctx)
	for i := 0; i < wfWorkers; i++ {
		go wait.UntilWithContext(workerCtx, wfc.runWorker, time.Second)
	}

	archiveCtx, _ := logger.WithField("component", "archive_worker").InContext(ctx)
	for i := 0; i < wfArchiveWorkers; i++ {
		go wait.UntilWithContext(archiveCtx, wfc.runArchiveWorker, time.Second)
	}
	if cacheGCPeriod != 0 {
		go wait.JitterUntilWithContext(ctx, wfc.syncAllCacheForGC, cacheGCPeriod, 0.0, true)
	}
	<-ctx.Done()
}

func (wfc *WorkflowController) RunPrometheusServer(ctx context.Context, isDummy bool) {
	wfc.metrics.RunPrometheusServer(ctx, isDummy)
}

// Create and the Synchronization Manager
func (wfc *WorkflowController) createSynchronizationManager(ctx context.Context) {
	getSyncLimit := func(ctx context.Context, lockKey string) (int, error) {
		lockName, err := sync.DecodeLockName(ctx, lockKey)
		if err != nil {
			return 0, err
		}
		configmapsIf := wfc.kubeclientset.CoreV1().ConfigMaps(lockName.Namespace)
		var configMap *apiv1.ConfigMap
		err = waitutil.Backoff(retry.DefaultRetry(ctx), func() (bool, error) {
			var err error
			configMap, err = configmapsIf.Get(ctx, lockName.ResourceName, metav1.GetOptions{})
			return !errors.IsTransientErr(ctx, err), err
		})
		if err != nil {
			return 0, err
		}

		value, found := configMap.Data[lockName.Key]
		if !found {
			return 0, argoErr.New(argoErr.CodeBadRequest, fmt.Sprintf("Sync configuration key '%s' not found in ConfigMap", lockName.Key))
		}
		return strconv.Atoi(value)
	}

	nextWorkflow := func(key string) {
		wfc.wfQueue.AddAfter(key, semaphoreNotifyDelay)
	}

	isWFDeleted := func(key string) bool {
		_, exists, err := wfc.wfInformer.GetIndexer().GetByKey(key)
		if err != nil {
			logging.RequireLoggerFromContext(ctx).WithField("key", key).WithError(err).Error(ctx, "Failed to get workflow from informer")
			return false
		}
		return exists
	}

	wfc.syncManager = sync.NewLockManager(ctx, wfc.kubeclientset, wfc.namespace, wfc.Config.Synchronization, getSyncLimit, nextWorkflow, isWFDeleted)
}

// list all running workflows to initialize throttler and syncManager
func (wfc *WorkflowController) initManagers(ctx context.Context) error {
	labelSelector := labels.NewSelector().Add(util.InstanceIDRequirement(wfc.Config.InstanceID))
	req, _ := labels.NewRequirement(common.LabelKeyPhase, selection.Equals, []string{string(wfv1.WorkflowRunning)})
	if req != nil {
		labelSelector = labelSelector.Add(*req)
	}
	listOpts := metav1.ListOptions{LabelSelector: labelSelector.String()}
	wfList, err := wfc.wfclientset.ArgoprojV1alpha1().Workflows(wfc.GetManagedNamespace()).List(ctx, listOpts)
	if err != nil {
		return err
	}

	wfc.syncManager.Initialize(ctx, wfList.Items)

	if err := wfc.throttler.Init(wfList.Items); err != nil {
		return err
	}

	return nil
}

func (wfc *WorkflowController) runConfigMapWatcher(ctx context.Context) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)
	ctx, logger := logging.RequireLoggerFromContext(ctx).WithField("component", "configmap_watcher").InContext(ctx)
	retryWatcher, err := apiwatch.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return wfc.kubeclientset.CoreV1().ConfigMaps(wfc.managedNamespace).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		panic(err)
	}
	defer retryWatcher.Stop()

	for {
		select {
		case event := <-retryWatcher.ResultChan():
			cm, ok := event.Object.(*apiv1.ConfigMap)
			if !ok {
				logger.Error(ctx, "invalid config map object received in config watcher. Ignored processing")
				continue
			}
			logger.Debugf(ctx, "received config map %s/%s update", cm.Namespace, cm.Name)
			if cm.GetName() == wfc.configController.GetName() && wfc.namespace == cm.GetNamespace() {
				logger.Infof(ctx, "Received Workflow Controller config map %s/%s update", cm.Namespace, cm.Name)
				wfc.UpdateConfig(ctx)
			}
			wfc.notifySemaphoreConfigUpdate(ctx, cm)
		case <-ctx.Done():
			return
		}
	}
}

// notifySemaphoreConfigUpdate will notify semaphore config update to pending workflows
func (wfc *WorkflowController) notifySemaphoreConfigUpdate(ctx context.Context, cm *apiv1.ConfigMap) {
	logger := logging.RequireLoggerFromContext(ctx)
	wfs, err := wfc.wfInformer.GetIndexer().ByIndex(indexes.SemaphoreConfigIndexName, fmt.Sprintf("%s/%s", cm.Namespace, cm.Name))
	if err != nil {
		logger.Errorf(ctx, "failed get the workflow from informer. %v", err)
	}

	for _, obj := range wfs {
		un, ok := obj.(*unstructured.Unstructured)
		if !ok {
			logger.Warnf(ctx, "received object from indexer %s is not an unstructured", indexes.SemaphoreConfigIndexName)
			continue
		}
		logger.Infof(ctx, "Adding workflow %s/%s", un.GetNamespace(), un.GetName())
		wfc.wfQueue.AddRateLimited(fmt.Sprintf("%s/%s", un.GetNamespace(), un.GetName()))
	}
}

// Check if the controller has RBAC access to ClusterWorkflowTemplates
func (wfc *WorkflowController) createClusterWorkflowTemplateInformer(ctx context.Context) {
	cwftGetAllowed, err := authutil.CanIArgo(ctx, wfc.kubeclientset, "get", "clusterworkflowtemplates", wfc.namespace, "")
	errors.CheckError(ctx, err)
	cwftListAllowed, err := authutil.CanIArgo(ctx, wfc.kubeclientset, "list", "clusterworkflowtemplates", wfc.namespace, "")
	errors.CheckError(ctx, err)
	cwftWatchAllowed, err := authutil.CanIArgo(ctx, wfc.kubeclientset, "watch", "clusterworkflowtemplates", wfc.namespace, "")
	errors.CheckError(ctx, err)

	logger := logging.RequireLoggerFromContext(ctx)
	if cwftGetAllowed && cwftListAllowed && cwftWatchAllowed {
		wfc.cwftmplInformer = informer.NewTolerantClusterWorkflowTemplateInformer(wfc.dynamicInterface, clusterWorkflowTemplateResyncPeriod)
		go wfc.cwftmplInformer.Informer().Run(ctx.Done())

		// since the above call is asynchronous, make sure we populate our cache before we try to use it later
		if !cache.WaitForCacheSync(
			ctx.Done(),
			wfc.cwftmplInformer.Informer().HasSynced,
		) {
			logger.WithFatal().Error(ctx, "Timed out waiting for ClusterWorkflowTemplate cache to sync")
		}
	} else {
		logger.Warn(ctx, "Controller doesn't have RBAC access for ClusterWorkflowTemplates")
	}
}

func (wfc *WorkflowController) UpdateConfig(ctx context.Context) {
	logger := logging.RequireLoggerFromContext(ctx)
	c, err := wfc.configController.Get(ctx)
	if err != nil {
		logger.WithError(err).WithFatal().Error(ctx, "Failed to register watch for controller config map")
	}
	wfc.Config = *c
	err = wfc.updateConfig(ctx)
	if err != nil {
		logger.WithError(err).WithFatal().Error(ctx, "Failed to update config")
	}
}

func (wfc *WorkflowController) workflowGarbageCollector(ctx context.Context) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	periodicity := env.LookupEnvDurationOr(ctx, "WORKFLOW_GC_PERIOD", 5*time.Minute)
	logger := logging.RequireLoggerFromContext(ctx)
	ctx, logger = logger.WithField("component", "wf_garbage_collector").InContext(ctx)
	logger.WithField("periodicity", periodicity).Info(ctx, "Performing periodic GC")
	ticker := time.NewTicker(periodicity)
	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C:
			if wfc.offloadNodeStatusRepo.IsEnabled() {
				logger.Info(ctx, "Performing periodic workflow GC")
				oldRecords, err := wfc.offloadNodeStatusRepo.ListOldOffloads(ctx, wfc.GetManagedNamespace())
				if err != nil {
					logger.WithField("err", err).Error(ctx, "Failed to list old offloaded nodes")
					continue
				}
				logger.WithField("len_wfs", len(oldRecords)).Info(ctx, "Deleting old offloads that are not live")
				for uid, versions := range oldRecords {
					if err := wfc.deleteOffloadedNodesForWorkflow(ctx, uid, versions); err != nil {
						logger.WithError(err).WithField("uid", uid).Error(ctx, "Failed to delete old offloaded nodes")
					}
				}
				logger.Info(ctx, "Workflow GC finished")
			}
		}
	}
}

func (wfc *WorkflowController) deleteOffloadedNodesForWorkflow(ctx context.Context, uid string, versions []string) error {
	workflows, err := wfc.wfInformer.GetIndexer().ByIndex(indexes.UIDIndex, uid)
	if err != nil {
		return err
	}
	var wf *wfv1.Workflow
	logger := logging.RequireLoggerFromContext(ctx)
	switch l := len(workflows); l {
	case 0:
		logger.WithField("uid", uid).Info(ctx, "Workflow missing, probably deleted")
	case 1:
		un, ok := workflows[0].(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("object %+v is not an unstructured", workflows[0])
		}
		key := un.GetNamespace() + "/" + un.GetName()
		wfc.workflowKeyLock.Lock(key)
		defer wfc.workflowKeyLock.Unlock(key)

		obj, ok := wfc.getWorkflowByKey(ctx, key)
		if !ok {
			return fmt.Errorf("failed to get workflow by key after locking")
		}
		un, ok = obj.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("object %+v is not an unstructured", obj)
		}
		wf, err = util.FromUnstructured(un)
		if err != nil {
			return err
		}

		// workflow might still be hydrated
		if wfc.hydrator.IsHydrated(wf) {
			logger.WithField("uid", wf.UID).Info(ctx, "Hydrated workflow encountered")
			err = wfc.hydrator.Dehydrate(ctx, wf)
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("expected no more than 1 workflow, got %d", l)
	}
	for _, version := range versions {
		// skip delete if offload is live
		if wf != nil && wf.Status.OffloadNodeStatusVersion == version {
			continue
		}
		if err := wfc.offloadNodeStatusRepo.Delete(ctx, uid, version); err != nil {
			return err
		}
	}
	return nil
}

func (wfc *WorkflowController) archivedWorkflowGarbageCollector(ctx context.Context) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	logger := logging.RequireLoggerFromContext(ctx)
	logger = logger.WithField("component", "archived_wf_garbage_collector")
	ctx = logging.WithLogger(ctx, logger)
	periodicity := env.LookupEnvDurationOr(ctx, "ARCHIVED_WORKFLOW_GC_PERIOD", 24*time.Hour)
	if wfc.Config.Persistence == nil {
		logger.Info(ctx, "Persistence disabled - so archived workflow GC disabled - you must restart the controller if you enable this")
		return
	}
	if !wfc.Config.Persistence.Archive {
		logger.Info(ctx, "Archive disabled - so archived workflow GC disabled - you must restart the controller if you enable this")
		return
	}
	ttl := wfc.Config.Persistence.ArchiveTTL
	if ttl == config.TTL(0) {
		logger.Info(ctx, "Archived workflows TTL zero - so archived workflow GC disabled - you must restart the controller if you enable this")
		return
	}
	logger.WithFields(logging.Fields{"ttl": ttl, "periodicity": periodicity}).Info(ctx, "Performing archived workflow GC")
	ticker := time.NewTicker(periodicity)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info(ctx, "Performing archived workflow GC")
			err := wfc.wfArchive.DeleteExpiredWorkflows(ctx, time.Duration(ttl))
			if err != nil {
				logger.WithField("err", err).Error(ctx, "Failed to delete archived workflows")
			}
		}
	}
}

func (wfc *WorkflowController) runWorker(ctx context.Context) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	for wfc.processNextItem(ctx) {
	}
}

func (wfc *WorkflowController) runArchiveWorker(ctx context.Context) {
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	for wfc.processNextArchiveItem(ctx) {
	}
}

// processNextItem is the worker logic for handling workflow updates
func (wfc *WorkflowController) processNextItem(ctx context.Context) bool {
	key, quit := wfc.wfQueue.Get()
	if quit {
		return false
	}
	defer wfc.wfQueue.Done(key)

	wfc.workflowKeyLock.Lock(key)
	defer wfc.workflowKeyLock.Unlock(key)

	obj, ok := wfc.getWorkflowByKey(ctx, key)
	if !ok {
		return true
	}

	logger := logging.RequireLoggerFromContext(ctx)
	// The workflow informer receives unstructured objects to deal with the possibility of invalid
	// workflow manifests that are unable to unmarshal to workflow objects
	un, ok := obj.(*unstructured.Unstructured)
	if !ok {
		logger.WithField("key", key).Warn(ctx, "Index is not an unstructured")
		return true
	}

	if !reconciliationNeeded(un) {
		logger.WithField("key", key).Debug(ctx, "Won't process Workflow since it's completed")
		return true
	}

	wf, err := util.FromUnstructured(un)
	if err != nil {
		logger.WithField("key", key).WithError(err).Warn(ctx, "Failed to unmarshal key to workflow object")
		woc := newWorkflowOperationCtx(ctx, wf, wfc)
		ctx = logging.WithLogger(ctx, woc.log)
		woc.markWorkflowFailed(ctx, fmt.Sprintf("cannot unmarshall spec: %s", err.Error()))
		woc.persistUpdates(ctx)
		return true
	}

	if wf.Status.Phase != "" && wfc.checkRecentlyCompleted(wf.Name) {
		logger.WithField("name", wf.ObjectMeta.Name).Warn(ctx, "Cache: Rejecting recently deleted")
		return true
	}

	// this will ensure we process every incomplete workflow once every 20m
	wfc.wfQueue.AddAfter(key, workflowResyncPeriod)

	woc := newWorkflowOperationCtx(ctx, wf, wfc)
	ctx = logging.WithLogger(ctx, woc.log)

	if (!woc.GetShutdownStrategy().Enabled() || woc.GetShutdownStrategy() != wfv1.ShutdownStrategyTerminate) && !wfc.throttler.Admit(key) {
		woc.log.WithField("key", key).Info(ctx, "Workflow processing has been postponed due to max parallelism limit")
		if woc.wf.Status.Phase == wfv1.WorkflowUnknown {
			woc.markWorkflowPhase(ctx, wfv1.WorkflowPending, "Workflow processing has been postponed because too many workflows are already running")
			woc.persistUpdates(ctx)
		}
		return true
	}

	// make sure this is removed from the throttler is complete
	defer func() {
		// must be done with woc
		if !reconciliationNeeded(woc.wf) {
			wfc.throttler.Remove(key)
		}
	}()

	err = wfc.hydrator.Hydrate(ctx, woc.wf)
	if err != nil {
		woc.log.Errorf(ctx, "hydration failed: %v", err)
		woc.markWorkflowError(ctx, err)
		woc.persistUpdates(ctx)
		return true
	}
	ctx = wfctx.InjectObjectMeta(ctx, &woc.wf.ObjectMeta)
	startTime := time.Now()
	woc.operate(ctx)
	wfc.metrics.OperationCompleted(ctx, time.Since(startTime).Seconds())

	// TODO: operate should return error if it was unable to operate properly
	// so we can requeue the work for a later time
	// See: https://github.com/kubernetes/client-go/blob/master/examples/workqueue/main.go
	// c.handleErr(err, key)
	return true
}

func (wfc *WorkflowController) processNextArchiveItem(ctx context.Context) bool {
	key, quit := wfc.wfArchiveQueue.Get()
	if quit {
		return false
	}
	logger := logging.RequireLoggerFromContext(ctx)
	defer wfc.wfArchiveQueue.Done(key)

	obj, exists, err := wfc.wfInformer.GetIndexer().GetByKey(key)
	if err != nil {
		logger.WithField("key", key).WithError(err).Error(ctx, "Failed to get workflow from informer")
		return true
	}
	if !exists {
		return true
	}

	wfc.archiveWorkflow(ctx, obj)
	return true
}

func (wfc *WorkflowController) getWorkflowByKey(ctx context.Context, key string) (interface{}, bool) {
	logger := logging.RequireLoggerFromContext(ctx)
	obj, exists, err := wfc.wfInformer.GetIndexer().GetByKey(key)
	if err != nil {
		logger.WithField("key", key).WithError(err).Error(ctx, "Failed to get workflow from informer")
		return nil, false
	}
	if !exists {
		// This happens after a workflow was labeled with completed=true
		// or was deleted, but the work queue still had an entry for it.
		return nil, false
	}
	return obj, true
}

func reconciliationNeeded(wf metav1.Object) bool {
	return wf.GetLabels()[common.LabelKeyCompleted] != "true" || slices.Contains(wf.GetFinalizers(), common.FinalizerArtifactGC)
}

// enqueueWfFromPodLabel will extract the workflow name from pod label and
// enqueue workflow for processing
func (wfc *WorkflowController) enqueueWfFromPodLabel(pod *apiv1.Pod) error {
	if pod.Labels == nil {
		return fmt.Errorf("Pod did not have labels")
	}
	workflowName, ok := pod.Labels[common.LabelKeyWorkflow]
	if !ok {
		// Ignore pods unrelated to workflow (this shouldn't happen unless the watch is setup incorrectly)
		return fmt.Errorf("Watch returned pod unrelated to any workflow")
	}
	wfc.wfQueue.AddRateLimited(pod.Namespace + "/" + workflowName)
	return nil
}

func (wfc *WorkflowController) tweakListRequestListOptions(options *metav1.ListOptions) {
	labelSelector := labels.NewSelector().
		Add(util.InstanceIDRequirement(wfc.Config.InstanceID))
	options.LabelSelector = labelSelector.String()
	// `ResourceVersion=0` does not honor the `limit` in API calls, which results in making significant List calls
	// without `limit`. For details, see https://github.com/argoproj/argo-workflows/pull/11343
	options.ResourceVersion = ""
}

func (wfc *WorkflowController) tweakWatchRequestListOptions(options *metav1.ListOptions) {
	labelSelector := labels.NewSelector().
		Add(util.InstanceIDRequirement(wfc.Config.InstanceID))
	options.LabelSelector = labelSelector.String()
}

func getWfPriority(obj interface{}) (int32, time.Time) {
	un, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return 0, time.Now()
	}
	priority, hasPriority, err := unstructured.NestedInt64(un.Object, "spec", "priority")
	if err != nil {
		return 0, un.GetCreationTimestamp().Time
	}
	if !hasPriority {
		priority = 0
	}

	return int32(priority), un.GetCreationTimestamp().Time
}

// 10 minutes in the past
const maxCompletedStoreTime = time.Second * -600

// This is a helper function for expiring old records of workflows
// completed more than maxCompletedStoreTime ago
func (wfc *WorkflowController) cleanCompletedWorkflowsRecord() {
	cutoff := time.Now().Add(maxCompletedStoreTime)
	removeIndex := -1
	wfc.recentCompletions.mutex.Lock()
	defer wfc.recentCompletions.mutex.Unlock()

	for i, val := range wfc.recentCompletions.completions {
		if val.when.After(cutoff) {
			removeIndex = i - 1
			break
		}
	}
	if removeIndex >= 0 {
		wfc.recentCompletions.completions = wfc.recentCompletions.completions[removeIndex+1:]
	}
}

// Records a workflow as recently completed in the list
// if it isn't already in the list
func (wfc *WorkflowController) recordCompletedWorkflow(key string) {
	if !wfc.checkRecentlyCompleted(key) {
		wfc.recentCompletions.mutex.Lock()
		defer wfc.recentCompletions.mutex.Unlock()
		wfc.recentCompletions.completions = append(wfc.recentCompletions.completions,
			recentlyCompletedWorkflow{
				key:  key,
				when: time.Now(),
			})
	}
}

// Returns true if the workflow given by key is in the recently completed
// list. Will perform expiry cleanup before checking.
func (wfc *WorkflowController) checkRecentlyCompleted(key string) bool {
	wfc.cleanCompletedWorkflowsRecord()
	recent := false
	wfc.recentCompletions.mutex.RLock()
	defer wfc.recentCompletions.mutex.RUnlock()
	for _, val := range wfc.recentCompletions.completions {
		if val.key == key {
			recent = true
			break
		}
	}
	return recent
}

func (wfc *WorkflowController) addWorkflowInformerHandlers(ctx context.Context) error {
	logger := logging.RequireLoggerFromContext(ctx)
	_, err := wfc.wfInformer.AddEventHandler(
		cache.FilteringResourceEventHandler{
			// FilterFunc is called for every operation affecting the
			// informer cache and can be used to reject things from
			// the cache. When they are rejected (this returns false)
			// they will be deleted.
			FilterFunc: func(obj interface{}) bool {
				un, ok := obj.(*unstructured.Unstructured)
				if !ok {
					logger.Warnf(ctx, "Workflow FilterFunc: '%v' is not an unstructured", obj)
					return false
				}
				needed := reconciliationNeeded(un)
				if !needed {
					key, _ := cache.MetaNamespaceKeyFunc(un)
					wfc.recordCompletedWorkflow(key)
				}
				return needed
			},
			Handler: cache.ResourceEventHandlerFuncs{
				// This function is called when a new to the informer object
				// is to be added to the informer
				AddFunc: func(obj interface{}) {
					key, err := cache.MetaNamespaceKeyFunc(obj)
					if err == nil {
						// for a new workflow, we do not want to rate limit its execution using AddRateLimited
						wfc.wfQueue.AddAfter(key, wfc.Config.InitialDelay.Duration)
						priority, creation := getWfPriority(obj)
						wfc.throttler.Add(key, priority, creation)
					}
				},
				// This function is called when an updated (we already know about this object)
				// is to be updated in the informer
				UpdateFunc: func(old, new interface{}) {
					oldWf, newWf := old.(*unstructured.Unstructured), new.(*unstructured.Unstructured)
					// this check is very important to prevent doing many reconciliations we do not need to do
					if oldWf.GetResourceVersion() == newWf.GetResourceVersion() {
						return
					}
					key, err := cache.MetaNamespaceKeyFunc(new)
					if err == nil {
						wfc.wfQueue.AddRateLimited(key)
						priority, creation := getWfPriority(new)
						wfc.throttler.Add(key, priority, creation)
					}
				},
				// This function is called when an object is to be removed
				// from the informer
				DeleteFunc: func(obj interface{}) {
					// IndexerInformer uses a delta queue, therefore for deletes we have to use this
					// key function.

					// Remove finalizers from Pods if they exist before deletion
					pods := wfc.kubeclientset.CoreV1().Pods(wfc.GetManagedNamespace())
					podList, err := pods.List(ctx, metav1.ListOptions{
						LabelSelector: fmt.Sprintf("%s=%s", common.LabelKeyWorkflow, obj.(*unstructured.Unstructured).GetName()),
					})
					if err != nil {
						logger.WithError(err).Error(ctx, "Failed to list pods")
					}
					for _, p := range podList.Items {
						if slices.Contains(p.Finalizers, common.FinalizerPodStatus) {
							wfc.PodController.RemoveFinalizer(ctx, p.Namespace, p.Name)
						}
					}

					key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
					if err == nil {
						wfc.releaseAllWorkflowLocks(ctx, obj)
						wfc.recordCompletedWorkflow(key)
						// no need to add to the queue - this workflow is done
						wfc.throttler.Remove(key)
					}
				},
			},
		},
	)
	if err != nil {
		return err
	}
	_, err = wfc.wfInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			un, ok := obj.(*unstructured.Unstructured)
			// no need to check the `common.LabelKeyCompleted` as we already know it must be complete
			return ok && un.GetLabels()[common.LabelKeyWorkflowArchivingStatus] == "Pending"
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					wfc.wfArchiveQueue.Add(key)
				}
			},
			UpdateFunc: func(_, obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					wfc.wfArchiveQueue.Add(key)
				}
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = wfc.wfInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			wf, ok := obj.(*unstructured.Unstructured)
			if ok { // maybe cache.DeletedFinalStateUnknown
				wfc.metrics.DeleteRealtimeMetricsForWfUID(string(wf.GetUID()))
			}
		},
	})
	if err != nil {
		return err
	}
	return nil
}

func (wfc *WorkflowController) archiveWorkflow(ctx context.Context, obj interface{}) {
	logger := logging.RequireLoggerFromContext(ctx)
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		logger.Error(ctx, "failed to get key for object")
		return
	}
	wfc.workflowKeyLock.Lock(key)
	defer wfc.workflowKeyLock.Unlock(key)
	key, err = cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		logger.Error(ctx, "failed to get key for object after locking")
		return
	}
	err = wfc.archiveWorkflowAux(ctx, obj)
	if err != nil {
		logger.WithField("key", key).WithError(err).Error(ctx, "failed to archive workflow")
	}
}

func (wfc *WorkflowController) archiveWorkflowAux(ctx context.Context, obj interface{}) error {
	un, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	wf, err := util.FromUnstructured(un)
	if err != nil {
		return fmt.Errorf("failed to convert to workflow from unstructured: %w", err)
	}
	err = wfc.hydrator.Hydrate(ctx, wf)
	if err != nil {
		return fmt.Errorf("failed to hydrate workflow: %w", err)
	}
	logger := logging.RequireLoggerFromContext(ctx)
	logger.WithFields(logging.Fields{"namespace": wf.Namespace, "workflow": wf.Name, "uid": wf.UID}).Info(ctx, "archiving workflow")
	err = wfc.wfArchive.ArchiveWorkflow(ctx, wf)
	if err != nil {
		return fmt.Errorf("failed to archive workflow: %w", err)
	}
	data, err := json.Marshal(map[string]interface{}{
		"metadata": metav1.ObjectMeta{
			Labels: map[string]string{
				common.LabelKeyWorkflowArchivingStatus: "Archived",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}
	_, err = wfc.wfclientset.ArgoprojV1alpha1().Workflows(un.GetNamespace()).Patch(
		ctx,
		un.GetName(),
		types.MergePatchType,
		data,
		metav1.PatchOptions{},
	)
	if err != nil {
		// from this point on we have successfully archived the workflow, and it is possible for the workflow to have actually
		// been deleted, so it's not a problem to get a `IsNotFound` error
		if apierr.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to archive workflow: %w", err)
	}
	return nil
}

func (wfc *WorkflowController) instanceIDReq() labels.Requirement {
	return util.InstanceIDRequirement(wfc.Config.InstanceID)
}

func (wfc *WorkflowController) newConfigMapInformer(ctx context.Context) cache.SharedIndexInformer {
	indexInformer := v1.NewFilteredConfigMapInformer(wfc.kubeclientset, wfc.GetManagedNamespace(), 20*time.Minute, cache.Indexers{
		indexes.ConfigMapLabelsIndex: indexes.ConfigMapIndexFunc,
	}, func(opts *metav1.ListOptions) {
		opts.LabelSelector = common.LabelKeyConfigMapType
	})
	ctx, logger := logging.RequireLoggerFromContext(ctx).WithField("component", "config_map_informer").InContext(ctx)
	logger.WithField("executorPlugins", wfc.executorPlugins != nil).Info(ctx, "Plugins")
	if wfc.executorPlugins != nil {
		//nolint:errcheck // the error only happens if the informer was stopped, and it hasn't even started (https://github.com/kubernetes/client-go/blob/46588f2726fa3e25b1704d6418190f424f95a990/tools/cache/shared_informer.go#L580)
		indexInformer.AddEventHandler(cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				cm, err := meta.Accessor(obj)
				if err != nil {
					return false
				}
				return cm.GetLabels()[common.LabelKeyConfigMapType] == common.LabelValueTypeConfigMapExecutorPlugin
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					cm := obj.(*apiv1.ConfigMap)
					p, err := plugin.FromConfigMap(cm)
					if err != nil {
						logger.WithFields(logging.Fields{
							"namespace": cm.GetNamespace(),
							"name":      cm.GetName(),
						}).WithError(err).Error(ctx, "failed to convert configmap to plugin")
						return
					}
					if _, ok := wfc.executorPlugins[cm.GetNamespace()]; !ok {
						wfc.executorPlugins[cm.GetNamespace()] = map[string]*spec.Plugin{}
					}
					wfc.executorPlugins[cm.GetNamespace()][cm.GetName()] = p
					logger.WithFields(logging.Fields{
						"namespace": cm.GetNamespace(),
						"name":      cm.GetName(),
					}).Info(ctx, "Executor plugin added")
				},
				UpdateFunc: func(_, obj interface{}) {
					cm := obj.(*apiv1.ConfigMap)
					p, err := plugin.FromConfigMap(cm)
					if err != nil {
						logger.WithFields(logging.Fields{
							"namespace": cm.GetNamespace(),
							"name":      cm.GetName(),
						}).WithError(err).Error(ctx, "failed to convert configmap to plugin")
						return
					}

					wfc.executorPlugins[cm.GetNamespace()][cm.GetName()] = p
					logger.WithFields(logging.Fields{
						"namespace": cm.GetNamespace(),
						"name":      cm.GetName(),
					}).Info(ctx, "Executor plugin updated")
				},
				DeleteFunc: func(obj interface{}) {
					key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
					namespace, name, _ := cache.SplitMetaNamespaceKey(key)
					delete(wfc.executorPlugins[namespace], name)
					logger.WithFields(logging.Fields{
						"namespace": namespace,
						"name":      name,
					}).Info(ctx, "Executor plugin removed")
				},
			},
		})
	}
	return indexInformer
}

// call this func whenever the configuration changes, or when the workflow informer changes
func (wfc *WorkflowController) updateEstimatorFactory(ctx context.Context) {
	wfc.estimatorFactory = estimation.NewEstimatorFactory(ctx, wfc.wfInformer, wfc.hydrator, wfc.wfArchive)
}

// setWorkflowDefaults sets values in the workflow.Spec with defaults from the
// workflowController. Values in the workflow will be given the upper hand over the defaults.
// The defaults for the workflow controller are set in the workflow-controller config map
func (wfc *WorkflowController) setWorkflowDefaults(wf *wfv1.Workflow) error {
	if wfc.Config.WorkflowDefaults != nil {
		err := util.MergeTo(wfc.Config.WorkflowDefaults, wf)
		if err != nil {
			return err
		}
	}
	return nil
}

func (wfc *WorkflowController) GetManagedNamespace() string {
	if wfc.managedNamespace != "" {
		return wfc.managedNamespace
	}
	return wfc.Config.Namespace
}

func (wfc *WorkflowController) getMaxStackDepth() int {
	return maxAllowedStackDepth
}

func (wfc *WorkflowController) getMetricsServerConfig() *telemetry.Config {
	// Metrics config
	modifiers := make(map[string]telemetry.Modifier)
	for name, modifier := range wfc.Config.MetricsConfig.Modifiers {
		modifiers[name] = telemetry.Modifier{
			Disabled:           modifier.Disabled,
			DisabledAttributes: modifier.DisabledAttributes,
			HistogramBuckets:   modifier.HistogramBuckets,
		}
	}

	metricsConfig := telemetry.Config{
		Enabled:      wfc.Config.MetricsConfig.Enabled == nil || *wfc.Config.MetricsConfig.Enabled,
		Path:         wfc.Config.MetricsConfig.Path,
		Port:         wfc.Config.MetricsConfig.Port,
		TTL:          time.Duration(wfc.Config.MetricsConfig.MetricsTTL),
		IgnoreErrors: wfc.Config.MetricsConfig.IgnoreErrors,
		Secure:       wfc.Config.MetricsConfig.GetSecure(true),
		Modifiers:    modifiers,
		Temporality:  wfc.Config.MetricsConfig.GetTemporality(),
	}
	return &metricsConfig
}

func (wfc *WorkflowController) releaseAllWorkflowLocks(ctx context.Context, obj interface{}) {
	un, ok := obj.(*unstructured.Unstructured)
	logger := logging.RequireLoggerFromContext(ctx)
	if !ok {
		logger.WithField("key", obj).Warn(ctx, "Key in index is not an unstructured")
		return
	}
	wf, err := util.FromUnstructured(un)
	if err != nil {
		logger.WithField("key", obj).Warn(ctx, "Invalid workflow object")
		return
	}
	if wf.Status.Synchronization != nil {
		wfc.syncManager.ReleaseAll(ctx, wf)
	}
}

func (wfc *WorkflowController) isArchivable(wf *wfv1.Workflow) bool {
	return wfc.archiveLabelSelector.Matches(labels.Set(wf.Labels))
}

func (wfc *WorkflowController) getWorkflowPhaseMetrics(ctx context.Context) map[string]int64 {
	result := make(map[string]int64, 0)
	if wfc.wfInformer != nil {
		for _, phase := range []wfv1.NodePhase{wfv1.NodePending, wfv1.NodeRunning, wfv1.NodeSucceeded, wfv1.NodeFailed, wfv1.NodeError} {
			keys, err := wfc.wfInformer.GetIndexer().IndexKeys(indexes.WorkflowPhaseIndex, string(phase))
			errors.CheckError(ctx, err)
			result[string(phase)] = int64(len(keys))
		}
	}
	return result
}

func (wfc *WorkflowController) getWorkflowConditionMetrics(ctx context.Context) map[wfv1.Condition]int64 {
	result := make(map[wfv1.Condition]int64, 0)
	if wfc.wfInformer != nil {
		for _, x := range []wfv1.Condition{
			{Type: wfv1.ConditionTypePodRunning, Status: metav1.ConditionTrue},
			{Type: wfv1.ConditionTypePodRunning, Status: metav1.ConditionFalse},
		} {
			keys, err := wfc.wfInformer.GetIndexer().IndexKeys(indexes.ConditionsIndex, indexes.ConditionValue(x))
			errors.CheckError(ctx, err)
			result[x] = int64(len(keys))
		}
	}
	return result
}

func (wfc *WorkflowController) getPodPhaseMetrics(ctx context.Context) map[string]int64 {
	// During startup we need this callback to exist, but it won't function until the PodController is started
	if wfc.PodController != nil {
		return wfc.PodController.GetPodPhaseMetrics(ctx)
	}
	return make(map[string]int64)
}

func (wfc *WorkflowController) newWorkflowTaskSetInformer() wfextvv1alpha1.WorkflowTaskSetInformer {
	informer := externalversions.NewSharedInformerFactoryWithOptions(
		wfc.wfclientset,
		workflowTaskSetResyncPeriod,
		externalversions.WithNamespace(wfc.GetManagedNamespace()),
		externalversions.WithTweakListOptions(func(x *metav1.ListOptions) {
			r := util.InstanceIDRequirement(wfc.Config.InstanceID)
			x.LabelSelector = r.String()
		})).Argoproj().V1alpha1().WorkflowTaskSets()
	//nolint:errcheck // the error only happens if the informer was stopped, and it hasn't even started (https://github.com/kubernetes/client-go/blob/46588f2726fa3e25b1704d6418190f424f95a990/tools/cache/shared_informer.go#L580)
	informer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					wfc.wfQueue.AddRateLimited(key)
				}
			},
		})
	return informer
}

func (wfc *WorkflowController) newArtGCTaskInformer() wfextvv1alpha1.WorkflowArtifactGCTaskInformer {
	informer := externalversions.NewSharedInformerFactoryWithOptions(
		wfc.wfclientset,
		workflowTaskSetResyncPeriod,
		externalversions.WithNamespace(wfc.GetManagedNamespace()),
		externalversions.WithTweakListOptions(func(x *metav1.ListOptions) {
			r := util.InstanceIDRequirement(wfc.Config.InstanceID)
			x.LabelSelector = r.String()
		})).Argoproj().V1alpha1().WorkflowArtifactGCTasks()
	//nolint:errcheck // the error only happens if the informer was stopped, and it hasn't even started (https://github.com/kubernetes/client-go/blob/46588f2726fa3e25b1704d6418190f424f95a990/tools/cache/shared_informer.go#L580)
	informer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					wfc.wfQueue.AddRateLimited(key)
				}
			},
		})
	return informer
}

func (wfc *WorkflowController) IsLeader() bool {
	// the wfc.wfInformer is nil if it is not the leader
	return wfc.wfInformer != nil
}
