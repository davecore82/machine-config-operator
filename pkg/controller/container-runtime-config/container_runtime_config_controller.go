package containerruntimeconfig

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/clarketm/json"
	ign3types "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/golang/glog"
	apicfgv1 "github.com/openshift/api/config/v1"
	apioperatorsv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	configclientset "github.com/openshift/client-go/config/clientset/versioned"
	cligoinformersv1 "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	cligolistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	operatorinformersv1alpha1 "github.com/openshift/client-go/operator/informers/externalversions/operator/v1alpha1"
	operatorlistersv1alpha1 "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/vincent-petithory/dataurl"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	coreclientsetv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"

	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	mtmpl "github.com/openshift/machine-config-operator/pkg/controller/template"
	mcfgclientset "github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned"
	"github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned/scheme"
	mcfginformersv1 "github.com/openshift/machine-config-operator/pkg/generated/informers/externalversions/machineconfiguration.openshift.io/v1"
	mcfglistersv1 "github.com/openshift/machine-config-operator/pkg/generated/listers/machineconfiguration.openshift.io/v1"
	"github.com/openshift/machine-config-operator/pkg/version"
)

const (
	// maxRetries is the number of times a containerruntimeconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	builtInLabelKey = "machineconfiguration.openshift.io/mco-built-in"
)

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	controllerKind = mcfgv1.SchemeGroupVersion.WithKind("ContainerRuntimeConfig")
)

var updateBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

// Controller defines the container runtime config controller.
type Controller struct {
	templatesDir string
	namespace    string

	client        mcfgclientset.Interface
	configClient  configclientset.Interface
	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	syncHandler                   func(mcp string) error
	syncImgHandler                func(mcp string) error
	enqueueContainerRuntimeConfig func(*mcfgv1.ContainerRuntimeConfig)

	ccLister       mcfglistersv1.ControllerConfigLister
	ccListerSynced cache.InformerSynced

	mccrLister       mcfglistersv1.ContainerRuntimeConfigLister
	mccrListerSynced cache.InformerSynced

	imgLister       cligolistersv1.ImageLister
	imgListerSynced cache.InformerSynced

	icspLister       operatorlistersv1alpha1.ImageContentSourcePolicyLister
	icspListerSynced cache.InformerSynced

	mcpLister       mcfglistersv1.MachineConfigPoolLister
	mcpListerSynced cache.InformerSynced

	clusterVersionLister       cligolistersv1.ClusterVersionLister
	clusterVersionListerSynced cache.InformerSynced

	queue    workqueue.RateLimitingInterface
	imgQueue workqueue.RateLimitingInterface
}

// New returns a new container runtime config controller
func New(
	templatesDir, namespace string,
	mcpInformer mcfginformersv1.MachineConfigPoolInformer,
	ccInformer mcfginformersv1.ControllerConfigInformer,
	mcrInformer mcfginformersv1.ContainerRuntimeConfigInformer,
	imgInformer cligoinformersv1.ImageInformer,
	icspInformer operatorinformersv1alpha1.ImageContentSourcePolicyInformer,
	clusterVersionInformer cligoinformersv1.ClusterVersionInformer,
	kubeClient clientset.Interface,
	mcfgClient mcfgclientset.Interface,
	configClient configclientset.Interface,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&coreclientsetv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	ctrl := &Controller{
		templatesDir:  templatesDir,
		namespace:     namespace,
		client:        mcfgClient,
		configClient:  configClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineconfigcontroller-containerruntimeconfigcontroller"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineconfigcontroller-containerruntimeconfigcontroller"),
		imgQueue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		kubeClient:    kubeClient,
	}

	mcrInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addContainerRuntimeConfig,
		UpdateFunc: ctrl.updateContainerRuntimeConfig,
		DeleteFunc: ctrl.deleteContainerRuntimeConfig,
	})

	imgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.imageConfAdded,
		UpdateFunc: ctrl.imageConfUpdated,
		DeleteFunc: ctrl.imageConfDeleted,
	})

	icspInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.icspConfAdded,
		UpdateFunc: ctrl.icspConfUpdated,
		DeleteFunc: ctrl.icspConfDeleted,
	})

	ctrl.syncHandler = ctrl.syncContainerRuntimeConfig
	ctrl.syncImgHandler = ctrl.syncImageConfig
	ctrl.enqueueContainerRuntimeConfig = ctrl.enqueue

	ctrl.mcpLister = mcpInformer.Lister()
	ctrl.mcpListerSynced = mcpInformer.Informer().HasSynced

	ctrl.ccLister = ccInformer.Lister()
	ctrl.ccListerSynced = ccInformer.Informer().HasSynced

	ctrl.mccrLister = mcrInformer.Lister()
	ctrl.mccrListerSynced = mcrInformer.Informer().HasSynced

	ctrl.imgLister = imgInformer.Lister()
	ctrl.imgListerSynced = imgInformer.Informer().HasSynced

	ctrl.icspLister = icspInformer.Lister()
	ctrl.icspListerSynced = icspInformer.Informer().HasSynced

	ctrl.clusterVersionLister = clusterVersionInformer.Lister()
	ctrl.clusterVersionListerSynced = clusterVersionInformer.Informer().HasSynced
	// Add to the queue to trigger a sync when an upgrade happens
	// this ensures that the seccomp-use-default MC is created on an upgrade
	// This will be removed in the next version
	ctrl.queue.Add("force-sync-on-upgrade")

	return ctrl
}

// Run executes the container runtime config controller.
func (ctrl *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ctrl.queue.ShutDown()
	defer ctrl.imgQueue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, ctrl.mcpListerSynced, ctrl.mccrListerSynced, ctrl.ccListerSynced,
		ctrl.imgListerSynced, ctrl.icspListerSynced, ctrl.clusterVersionListerSynced) {
		return
	}

	glog.Info("Starting MachineConfigController-ContainerRuntimeConfigController")
	defer glog.Info("Shutting down MachineConfigController-ContainerRuntimeConfigController")

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}

	// Just need one worker for the image config
	go wait.Until(ctrl.imgWorker, time.Second, stopCh)

	<-stopCh
}

func ctrConfigTriggerObjectChange(old, new *mcfgv1.ContainerRuntimeConfig) bool {
	if old.DeletionTimestamp != new.DeletionTimestamp {
		return true
	}
	if !reflect.DeepEqual(old.Spec, new.Spec) {
		return true
	}
	return false
}

func (ctrl *Controller) imageConfAdded(obj interface{}) {
	ctrl.imgQueue.Add("openshift-config")
}

func (ctrl *Controller) imageConfUpdated(oldObj, newObj interface{}) {
	ctrl.imgQueue.Add("openshift-config")
}

func (ctrl *Controller) imageConfDeleted(obj interface{}) {
	ctrl.imgQueue.Add("openshift-config")
}

func (ctrl *Controller) icspConfAdded(obj interface{}) {
	ctrl.imgQueue.Add("openshift-config")
}

func (ctrl *Controller) icspConfUpdated(oldObj, newObj interface{}) {
	ctrl.imgQueue.Add("openshift-config")
}

func (ctrl *Controller) icspConfDeleted(obj interface{}) {
	ctrl.imgQueue.Add("openshift-config")
}

func (ctrl *Controller) updateContainerRuntimeConfig(oldObj, newObj interface{}) {
	oldCtrCfg := oldObj.(*mcfgv1.ContainerRuntimeConfig)
	newCtrCfg := newObj.(*mcfgv1.ContainerRuntimeConfig)

	if ctrConfigTriggerObjectChange(oldCtrCfg, newCtrCfg) {
		glog.V(4).Infof("Update ContainerRuntimeConfig %s", oldCtrCfg.Name)
		ctrl.enqueueContainerRuntimeConfig(newCtrCfg)
	}
}

func (ctrl *Controller) addContainerRuntimeConfig(obj interface{}) {
	cfg := obj.(*mcfgv1.ContainerRuntimeConfig)
	glog.V(4).Infof("Adding ContainerRuntimeConfig %s", cfg.Name)
	ctrl.enqueueContainerRuntimeConfig(cfg)
}

func (ctrl *Controller) deleteContainerRuntimeConfig(obj interface{}) {
	cfg, ok := obj.(*mcfgv1.ContainerRuntimeConfig)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		cfg, ok = tombstone.Obj.(*mcfgv1.ContainerRuntimeConfig)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a ContainerRuntimeConfig %#v", obj))
			return
		}
	}
	if err := ctrl.cascadeDelete(cfg); err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't delete object %#v: %v", cfg, err))
	} else {
		glog.V(4).Infof("Deleted ContainerRuntimeConfig %s and restored default config", cfg.Name)
	}
}

func (ctrl *Controller) cascadeDelete(cfg *mcfgv1.ContainerRuntimeConfig) error {
	if len(cfg.GetFinalizers()) == 0 {
		return nil
	}
	mcName := cfg.GetFinalizers()[0]
	err := ctrl.client.MachineconfigurationV1().MachineConfigs().Delete(context.TODO(), mcName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if err := ctrl.popFinalizerFromContainerRuntimeConfig(cfg); err != nil {
		return err
	}
	return nil
}

func (ctrl *Controller) enqueue(cfg *mcfgv1.ContainerRuntimeConfig) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(cfg)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", cfg, err))
		return
	}
	ctrl.queue.Add(key)
}

func (ctrl *Controller) enqueueRateLimited(cfg *mcfgv1.ContainerRuntimeConfig) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(cfg)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", cfg, err))
		return
	}
	ctrl.queue.AddRateLimited(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (ctrl *Controller) worker() {
	for ctrl.processNextWorkItem() {
	}
}

func (ctrl *Controller) imgWorker() {
	for ctrl.processNextImgWorkItem() {
	}
}

func (ctrl *Controller) processNextWorkItem() bool {
	key, quit := ctrl.queue.Get()
	if quit {
		return false
	}
	defer ctrl.queue.Done(key)

	err := ctrl.syncHandler(key.(string))
	ctrl.handleErr(err, key)

	return true
}

func (ctrl *Controller) processNextImgWorkItem() bool {
	key, quit := ctrl.imgQueue.Get()
	if quit {
		return false
	}
	defer ctrl.imgQueue.Done(key)

	err := ctrl.syncImgHandler(key.(string))
	ctrl.handleImgErr(err, key)

	return true
}

func (ctrl *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		ctrl.queue.Forget(key)
		return
	}

	if ctrl.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing containerruntimeconfig %v: %v", key, err)
		ctrl.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping containerruntimeconfig %q out of the queue: %v", key, err)
	ctrl.queue.Forget(key)
	ctrl.queue.AddAfter(key, 1*time.Minute)
}

func (ctrl *Controller) handleImgErr(err error, key interface{}) {
	if err == nil {
		ctrl.imgQueue.Forget(key)
		return
	}

	if ctrl.imgQueue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing image config %v: %v", key, err)
		ctrl.imgQueue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping image config %q out of the queue: %v", key, err)
	ctrl.imgQueue.Forget(key)
	ctrl.imgQueue.AddAfter(key, 1*time.Minute)
}

// generateOriginalContainerRuntimeConfigs returns rendered default storage, registries and policy config files
func generateOriginalContainerRuntimeConfigs(templateDir string, cc *mcfgv1.ControllerConfig, role string) (*ign3types.File, *ign3types.File, *ign3types.File, error) {
	// Render the default templates
	rc := &mtmpl.RenderConfig{ControllerConfigSpec: &cc.Spec}
	generatedConfigs, err := mtmpl.GenerateMachineConfigsForRole(rc, role, templateDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generateMachineConfigsforRole failed with error %s", err)
	}
	// Find generated storage.conf, registries.conf, and policy.json
	var (
		config, gmcStorageConfig, gmcRegistriesConfig, gmcPolicyJSON *ign3types.File
		errStorage, errRegistries, errPolicy                         error
	)
	// Find storage config
	for _, gmc := range generatedConfigs {
		config, errStorage = findStorageConfig(gmc)
		if errStorage == nil {
			gmcStorageConfig = config
			break
		}
	}
	// Find Registries config
	for _, gmc := range generatedConfigs {
		config, errRegistries = findRegistriesConfig(gmc)
		if errRegistries == nil {
			gmcRegistriesConfig = config
			break
		}
	}
	// Find Policy JSON
	for _, gmc := range generatedConfigs {
		config, errPolicy = findPolicyJSON(gmc)
		if errPolicy == nil {
			gmcPolicyJSON = config
			break
		}
	}
	if errStorage != nil || errRegistries != nil || errPolicy != nil {
		return nil, nil, nil, fmt.Errorf("could not generate old container runtime configs: %v, %v, %v", errStorage, errRegistries, errPolicy)
	}

	return gmcStorageConfig, gmcRegistriesConfig, gmcPolicyJSON, nil
}

func (ctrl *Controller) syncStatusOnly(cfg *mcfgv1.ContainerRuntimeConfig, err error, args ...interface{}) error {
	statusUpdateErr := retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, getErr := ctrl.mccrLister.Get(cfg.Name)
		if getErr != nil {
			return getErr
		}
		// Update the observedGeneration
		if newcfg.GetGeneration() != newcfg.Status.ObservedGeneration {
			newcfg.Status.ObservedGeneration = newcfg.GetGeneration()
		}
		// To avoid a long list of same statuses, only append a status if it is the first status
		// or if the status message is different from the message of the last status recorded
		// If the last status message is the same as the new one, then update the last status to
		// reflect the latest time stamp from the new status message.
		newStatusCondition := wrapErrorWithCondition(err, args...)
		if len(newcfg.Status.Conditions) == 0 || newStatusCondition.Message != newcfg.Status.Conditions[len(newcfg.Status.Conditions)-1].Message {
			newcfg.Status.Conditions = append(newcfg.Status.Conditions, newStatusCondition)
		} else if newcfg.Status.Conditions[len(newcfg.Status.Conditions)-1].Message == newStatusCondition.Message {
			newcfg.Status.Conditions[len(newcfg.Status.Conditions)-1] = newStatusCondition
		}
		_, updateErr := ctrl.client.MachineconfigurationV1().ContainerRuntimeConfigs().UpdateStatus(context.TODO(), newcfg, metav1.UpdateOptions{})
		return updateErr
	})
	// If an error occurred in updating the status just log it
	if statusUpdateErr != nil {
		glog.Warningf("error updating container runtime config status: %v", statusUpdateErr)
	}
	// Want to return the actual error received from the sync function
	return err
}

// addAnnotation adds the annotions for a ctrcfg object with the given annotationKey and annotationVal
func (ctrl *Controller) addAnnotation(cfg *mcfgv1.ContainerRuntimeConfig, annotationKey, annotationVal string) error {
	annotationUpdateErr := retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, getErr := ctrl.mccrLister.Get(cfg.Name)
		if getErr != nil {
			return getErr
		}
		newcfg.SetAnnotations(map[string]string{
			annotationKey: annotationVal,
		})
		_, updateErr := ctrl.client.MachineconfigurationV1().ContainerRuntimeConfigs().Update(context.TODO(), newcfg, metav1.UpdateOptions{})
		return updateErr
	})
	if annotationUpdateErr != nil {
		glog.Warningf("error updating the container runtime config with annotation key %q and value %q: %v", annotationKey, annotationVal, annotationUpdateErr)
	}
	return annotationUpdateErr
}

// syncContainerRuntimeConfig will sync the ContainerRuntimeconfig with the given key.
// This function is not meant to be invoked concurrently with the same key.
// nolint: gocyclo
func (ctrl *Controller) syncContainerRuntimeConfig(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing ContainerRuntimeconfig %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing ContainerRuntimeconfig %q (%v)", key, time.Since(startTime))
	}()

	// First let's create the MC for the drop in seccomp use default crio.conf file
	// This will be removed in the next version
	if err := ctrl.createSeccompUseDefaultMC(); err != nil {
		return fmt.Errorf("failed to create the crio-seccomp-use-default MC: %v", err)
	}
	// If the key is set to force-sync-on-upgrade, then we can return after creating
	// the capabilities MC.
	if key == "force-sync-on-upgrade" {
		return nil
	}

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// Fetch the ContainerRuntimeConfig
	cfg, err := ctrl.mccrLister.Get(name)
	if errors.IsNotFound(err) {
		glog.V(2).Infof("ContainerRuntimeConfig %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	// Deep-copy otherwise we are mutating our cache.
	cfg = cfg.DeepCopy()

	// Check for Deleted ContainerRuntimeConfig and optionally delete finalizers
	if cfg.DeletionTimestamp != nil {
		if len(cfg.GetFinalizers()) > 0 {
			return ctrl.cascadeDelete(cfg)
		}
		return nil
	}

	// Validate the ContainerRuntimeConfig CR
	if err := validateUserContainerRuntimeConfig(cfg); err != nil {
		return ctrl.syncStatusOnly(cfg, err)
	}

	// Get ControllerConfig
	controllerConfig, err := ctrl.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return fmt.Errorf("could not get ControllerConfig %v", err)
	}

	// Find all MachineConfigPools
	mcpPools, err := ctrl.getPoolsForContainerRuntimeConfig(cfg)
	if err != nil {
		return ctrl.syncStatusOnly(cfg, err)
	}

	for _, pool := range mcpPools {
		role := pool.Name
		// Get MachineConfig
		managedKey, err := getManagedKeyCtrCfg(pool, ctrl.client, cfg)
		if err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not get ctrcfg key: %v", err)
		}
		mc, err := ctrl.client.MachineconfigurationV1().MachineConfigs().Get(context.TODO(), managedKey, metav1.GetOptions{})
		isNotFound := errors.IsNotFound(err)
		if err != nil && !isNotFound {
			return ctrl.syncStatusOnly(cfg, err, "could not find MachineConfig: %v", managedKey)
		}
		// If we have seen this generation and the sync didn't fail, then skip
		if !isNotFound && cfg.Status.ObservedGeneration >= cfg.Generation && cfg.Status.Conditions[len(cfg.Status.Conditions)-1].Type == mcfgv1.ContainerRuntimeConfigSuccess {
			// But we still need to compare the generated controller version because during an upgrade we need a new one
			if mc.Annotations[ctrlcommon.GeneratedByControllerVersionAnnotationKey] == version.Hash {
				continue
			}
		}
		// Generate the original ContainerRuntimeConfig
		originalStorageIgn, _, _, err := generateOriginalContainerRuntimeConfigs(ctrl.templatesDir, controllerConfig, role)
		if err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not generate origin ContainerRuntime Configs: %v", err)
		}

		var configFileList []generatedConfigFile
		ctrcfg := cfg.Spec.ContainerRuntimeConfig
		if !ctrcfg.OverlaySize.IsZero() {
			storageTOML, err := mergeConfigChanges(originalStorageIgn, cfg, updateStorageConfig)
			if err != nil {
				glog.V(2).Infoln(cfg, err, "error merging user changes to storage.conf: %v", err)
				ctrl.syncStatusOnly(cfg, err)
			} else {
				configFileList = append(configFileList, generatedConfigFile{filePath: storageConfigPath, data: storageTOML})
				ctrl.syncStatusOnly(cfg, nil)
			}
		}

		// Create the cri-o drop-in files
		if ctrcfg.LogLevel != "" || ctrcfg.PidsLimit != nil || !ctrcfg.LogSizeMax.IsZero() {
			crioFileConfigs := createCRIODropinFiles(cfg)
			configFileList = append(configFileList, crioFileConfigs...)
		}

		if isNotFound {
			tempIgnCfg := ctrlcommon.NewIgnConfig()
			mc, err = ctrlcommon.MachineConfigFromIgnConfig(role, managedKey, tempIgnCfg)
			if err != nil {
				return ctrl.syncStatusOnly(cfg, err, "could not create MachineConfig from new Ignition config: %v", err)
			}
			_, ok := cfg.GetAnnotations()[ctrlcommon.MCNameSuffixAnnotationKey]
			arr := strings.Split(managedKey, "-")
			// If the MC name suffix annotation does not exist and the managed key value returned has a suffix, then add the MC name
			// suffix annotation and suffix value to the ctrcfg object
			if len(arr) > 4 && !ok {
				_, err := strconv.Atoi(arr[len(arr)-1])
				if err == nil {
					if err := ctrl.addAnnotation(cfg, ctrlcommon.MCNameSuffixAnnotationKey, arr[len(arr)-1]); err != nil {
						return ctrl.syncStatusOnly(cfg, err, "could not update annotation for containerRuntimeConfig")
					}
				}
			}
		}

		ctrRuntimeConfigIgn := createNewIgnition(configFileList)
		rawCtrRuntimeConfigIgn, err := json.Marshal(ctrRuntimeConfigIgn)
		if err != nil {
			return ctrl.syncStatusOnly(cfg, err, "error marshalling container runtime config Ignition: %v", err)
		}
		mc.Spec.Config.Raw = rawCtrRuntimeConfigIgn

		mc.SetAnnotations(map[string]string{
			ctrlcommon.GeneratedByControllerVersionAnnotationKey: version.Hash,
		})
		oref := metav1.NewControllerRef(cfg, controllerKind)
		mc.SetOwnerReferences([]metav1.OwnerReference{*oref})

		// Create or Update, on conflict retry
		if err := retry.RetryOnConflict(updateBackoff, func() error {
			var err error
			if isNotFound {
				_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Create(context.TODO(), mc, metav1.CreateOptions{})
			} else {
				_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Update(context.TODO(), mc, metav1.UpdateOptions{})
			}
			return err
		}); err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not Create/Update MachineConfig: %v", err)
		}
		// Add Finalizers to the ContainerRuntimeConfigs
		if err := ctrl.addFinalizerToContainerRuntimeConfig(cfg, mc); err != nil {
			return ctrl.syncStatusOnly(cfg, err, "could not add finalizers to ContainerRuntimeConfig: %v", err)
		}
		glog.Infof("Applied ContainerRuntimeConfig %v on MachineConfigPool %v", key, pool.Name)
	}
	if err := ctrl.cleanUpDuplicatedMC(); err != nil {
		return err
	}

	return ctrl.syncStatusOnly(cfg, nil)
}

// cleanUpDuplicatedMC removes the MC of uncorrected version if format of its name contains 'generated-xxx'.
// BZ 1955517: upgrade when there are more than one configs, these generated MC will be duplicated
// by upgraded MC with number suffixed name (func getManagedKeyCtrCfg()) and fails the upgrade.
func (ctrl *Controller) cleanUpDuplicatedMC() error {
	generatedCtrCfg := "generated-containerruntime"
	// Get all machine configs
	mcList, err := ctrl.client.MachineconfigurationV1().MachineConfigs().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing containerruntime machine configs: %v", err)
	}
	for _, mc := range mcList.Items {
		if !strings.Contains(mc.Name, generatedCtrCfg) {
			continue
		}
		// delete the containerruntime mc if its degraded
		if mc.Annotations[ctrlcommon.GeneratedByControllerVersionAnnotationKey] != version.Hash {
			if err := ctrl.client.MachineconfigurationV1().MachineConfigs().Delete(context.TODO(), mc.Name, metav1.DeleteOptions{}); err != nil {
				return fmt.Errorf("error deleting degraded containerruntime machine config %s: %v", mc.Name, err)
			}

		}
	}
	return nil
}

// mergeConfigChanges retrieves the original/default config data from the templates, decodes it and merges in the changes given by the Custom Resource.
// It then encodes the new data and returns it.
func mergeConfigChanges(origFile *ign3types.File, cfg *mcfgv1.ContainerRuntimeConfig, update updateConfigFunc) ([]byte, error) {
	if origFile.Contents.Source == nil {
		return nil, fmt.Errorf("original Container Runtime config is empty")
	}
	dataURL, err := dataurl.DecodeString(*origFile.Contents.Source)
	if err != nil {
		return nil, fmt.Errorf("could not decode original Container Runtime config: %v", err)
	}
	cfgTOML, err := update(dataURL.Data, cfg.Spec.ContainerRuntimeConfig)
	if err != nil {
		return nil, fmt.Errorf("could not update container runtime config with new changes: %v", err)
	}
	return cfgTOML, nil
}

func (ctrl *Controller) syncImageConfig(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing ImageConfig %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing ImageConfig %q (%v)", key, time.Since(startTime))
	}()

	// Fetch the ImageConfig
	imgcfg, err := ctrl.imgLister.Get("cluster")
	if errors.IsNotFound(err) {
		glog.V(2).Infof("ImageConfig 'cluster' does not exist or has been deleted")
		return nil
	}
	if err != nil {
		return err
	}
	// Deep-copy otherwise we are mutating our cache.
	imgcfg = imgcfg.DeepCopy()

	// Fetch the ClusterVersionConfig needed to get the registry being used by the payload
	// so that we can avoid adding that registry to blocked registries in /etc/containers/registries.conf
	clusterVersionCfg, err := ctrl.clusterVersionLister.Get("version")
	if errors.IsNotFound(err) {
		glog.Infof("ClusterVersionConfig 'version' does not exist or has been deleted")
		return nil
	}
	if err != nil {
		return err
	}

	var blockedRegs []string
	if clusterVersionCfg != nil {
		// Go through the registries in the image spec to get and validate the blocked registries
		blockedRegs, err = getValidBlockedRegistries(clusterVersionCfg.Status.Desired.Image, &imgcfg.Spec)
		if err != nil && err != errParsingReference {
			glog.V(2).Infof("%v, skipping....", err)
		} else if err == errParsingReference {
			return err
		}
	}

	// Get ControllerConfig
	controllerConfig, err := ctrl.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return fmt.Errorf("could not get ControllerConfig %v", err)
	}

	// Find all ImageContentSourcePolicy objects
	icspRules, err := ctrl.icspLister.List(labels.Everything())
	if err != nil && errors.IsNotFound(err) {
		icspRules = []*apioperatorsv1alpha1.ImageContentSourcePolicy{}
	} else if err != nil {
		return err
	}

	sel, err := metav1.LabelSelectorAsSelector(metav1.AddLabelToSelector(&metav1.LabelSelector{}, builtInLabelKey, ""))
	if err != nil {
		return err
	}
	// Find all the MCO built in MachineConfigPools
	mcpPools, err := ctrl.mcpLister.List(sel)
	if err != nil {
		return err
	}
	for _, pool := range mcpPools {
		// To keep track of whether we "actually" got an updated image config
		applied := true
		role := pool.Name
		// Get MachineConfig
		managedKey, err := getManagedKeyReg(pool, ctrl.client)
		if err != nil {
			return err
		}
		if err := retry.RetryOnConflict(updateBackoff, func() error {
			registriesIgn, err := registriesConfigIgnition(ctrl.templatesDir, controllerConfig, role,
				imgcfg.Spec.RegistrySources.InsecureRegistries, blockedRegs, imgcfg.Spec.RegistrySources.AllowedRegistries,
				imgcfg.Spec.RegistrySources.ContainerRuntimeSearchRegistries, icspRules)
			if err != nil {
				return err
			}
			rawRegistriesIgn, err := json.Marshal(registriesIgn)
			if err != nil {
				return fmt.Errorf("could not encode registries Ignition config: %v", err)
			}
			mc, err := ctrl.client.MachineconfigurationV1().MachineConfigs().Get(context.TODO(), managedKey, metav1.GetOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("could not find MachineConfig: %v", err)
			}
			isNotFound := errors.IsNotFound(err)
			if !isNotFound && equality.Semantic.DeepEqual(rawRegistriesIgn, mc.Spec.Config.Raw) {
				// if the configuration for the registries is equal, we still need to compare
				// the generated controller version because during an upgrade we need a new one
				mcCtrlVersion := mc.Annotations[ctrlcommon.GeneratedByControllerVersionAnnotationKey]
				if mcCtrlVersion == version.Hash {
					applied = false
					return nil
				}
			}
			if isNotFound {
				tempIgnCfg := ctrlcommon.NewIgnConfig()
				mc, err = ctrlcommon.MachineConfigFromIgnConfig(role, managedKey, tempIgnCfg)
				if err != nil {
					return fmt.Errorf("could not create MachineConfig from new Ignition config: %v", err)
				}
			}
			mc.Spec.Config.Raw = rawRegistriesIgn
			mc.ObjectMeta.Annotations = map[string]string{
				ctrlcommon.GeneratedByControllerVersionAnnotationKey: version.Hash,
			}
			mc.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: apicfgv1.SchemeGroupVersion.String(),
					Kind:       "Image",
					Name:       imgcfg.Name,
					UID:        imgcfg.UID,
				},
			}
			// Create or Update, on conflict retry
			if isNotFound {
				_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Create(context.TODO(), mc, metav1.CreateOptions{})
			} else {
				_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Update(context.TODO(), mc, metav1.UpdateOptions{})
			}

			return err
		}); err != nil {
			return fmt.Errorf("could not Create/Update MachineConfig: %v", err)
		}
		if applied {
			glog.Infof("Applied ImageConfig cluster on MachineConfigPool %v", pool.Name)
		}
	}

	return nil
}

func registriesConfigIgnition(templateDir string, controllerConfig *mcfgv1.ControllerConfig, role string,
	insecureRegs, blockedRegs, allowedRegs, searchRegs []string, icspRules []*apioperatorsv1alpha1.ImageContentSourcePolicy) (*ign3types.Config, error) {

	var (
		registriesTOML []byte
		policyJSON     []byte
	)

	// Generate the original registries config
	_, originalRegistriesIgn, originalPolicyIgn, err := generateOriginalContainerRuntimeConfigs(templateDir, controllerConfig, role)
	if err != nil {
		return nil, fmt.Errorf("could not generate origin ContainerRuntime Configs: %v", err)
	}

	if insecureRegs != nil || blockedRegs != nil || len(icspRules) != 0 {
		if originalRegistriesIgn.Contents.Source == nil {
			return nil, fmt.Errorf("original registries config is empty")
		}
		dataURL, err := dataurl.DecodeString(*originalRegistriesIgn.Contents.Source)
		if err != nil {
			return nil, fmt.Errorf("could not decode original registries config: %v", err)
		}
		registriesTOML, err = updateRegistriesConfig(dataURL.Data, insecureRegs, blockedRegs, icspRules)
		if err != nil {
			return nil, fmt.Errorf("could not update registries config with new changes: %v", err)
		}
	}
	if blockedRegs != nil || allowedRegs != nil {
		if originalPolicyIgn.Contents.Source == nil {
			return nil, fmt.Errorf("original policy json is empty")
		}
		dataURL, err := dataurl.DecodeString(*originalPolicyIgn.Contents.Source)
		if err != nil {
			return nil, fmt.Errorf("could not decode original policy json: %v", err)
		}
		policyJSON, err = updatePolicyJSON(dataURL.Data, blockedRegs, allowedRegs)
		if err != nil {
			return nil, fmt.Errorf("could not update policy json with new changes: %v", err)
		}
	}
	generatedConfigFileList := []generatedConfigFile{
		{filePath: registriesConfigPath, data: registriesTOML},
		{filePath: policyConfigPath, data: policyJSON},
	}
	if searchRegs != nil {
		generatedConfigFileList = append(generatedConfigFileList, updateSearchRegistriesConfig(searchRegs)...)
	}

	registriesIgn := createNewIgnition(generatedConfigFileList)
	return &registriesIgn, nil
}

func (ctrl *Controller) createSeccompUseDefaultMC() error {
	var configMapName = "crio-seccomp-use-default-when-empty"

	// Check if the crio-seccomp-use-default-when-empty config map exists in the openshift-machine-config-operator namespace
	seccompUseDefaultCM, err := ctrl.kubeClient.CoreV1().ConfigMaps(ctrl.namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	seccompCMIsNotFound := errors.IsNotFound(err)
	if err != nil && !seccompCMIsNotFound {
		return fmt.Errorf("error checking for %s config map: %v", configMapName, err)
	}
	// If the crio-seccomp-use-default-when-empty config map exists, that means the crio-seccomp-use-default MC was already created
	// so we should not create this MC again and return
	if seccompUseDefaultCM != nil && !seccompCMIsNotFound {
		return nil
	}

	sel, err := metav1.LabelSelectorAsSelector(metav1.AddLabelToSelector(&metav1.LabelSelector{}, builtInLabelKey, ""))
	if err != nil {
		return err
	}
	// Find all the MachineConfigPools
	mcpPoolsAll, err := ctrl.mcpLister.List(sel)
	if err != nil {
		return err
	}

	// Create the crio-seccomp-use-default MC for all the available pools
	for _, pool := range mcpPoolsAll {
		managedKey := getManagedKeySeccomp(pool)
		mc, err := ctrl.client.MachineconfigurationV1().MachineConfigs().Get(context.TODO(), managedKey, metav1.GetOptions{})
		isNotFound := errors.IsNotFound(err)
		if err != nil && !isNotFound {
			return fmt.Errorf("error checking for %s machine config: %v", managedKey, err)
		}
		// continue to the next MC if this already exists
		if mc != nil && !isNotFound {
			continue
		}

		tempIgnCfg := ctrlcommon.NewIgnConfig()
		mc, err = ctrlcommon.MachineConfigFromIgnConfig(pool.Name, managedKey, tempIgnCfg)
		if err != nil {
			return fmt.Errorf("could not create crio-seccomp-use-default MachineConfig from new Ignition config: %v", err)
		}
		rawCapsIgnition, err := json.Marshal(createNewIgnition(createDefaultSeccompUseDefaultWhenEmptyFile()))
		if err != nil {
			return fmt.Errorf("error marshalling crio-seccomp-use-default config ignition: %v", err)
		}
		mc.Spec.Config.Raw = rawCapsIgnition
		// Create the crio-seccomp-use-default MC
		if err := retry.RetryOnConflict(updateBackoff, func() error {
			_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Create(context.TODO(), mc, metav1.CreateOptions{})
			return err
		}); err != nil {
			return fmt.Errorf("could not create MachineConfig for crio-seccomp-use-default: %v", err)
		}
		glog.Infof("Applied Seccomp Use Default MC %v on MachineConfigPool %v", managedKey, pool.Name)
	}

	// Create the config map for crio-seccomp-use-default so we know that the crio-seccomp-use-default MC has been created
	seccompUseDefaultCM.Name = configMapName
	seccompUseDefaultCM.Namespace = ctrl.namespace
	if _, err := ctrl.kubeClient.CoreV1().ConfigMaps(ctrl.namespace).Create(context.TODO(), seccompUseDefaultCM, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("error creating %s config map: %v", configMapName, err)
	}
	return nil
}

// RunSeccompUseDefaultBootstrap creates the crio-seccomp-use-default mc on bootstrap
func RunSeccompUseDefaultBootstrap(mcpPools []*mcfgv1.MachineConfigPool) ([]*mcfgv1.MachineConfig, error) {
	var res []*mcfgv1.MachineConfig
	for _, pool := range mcpPools {
		seccompUseDefaultIgn := createNewIgnition(createDefaultSeccompUseDefaultWhenEmptyFile())
		mc, err := ctrlcommon.MachineConfigFromIgnConfig(pool.Name, getManagedKeySeccomp(pool), seccompUseDefaultIgn)
		if err != nil {
			return nil, fmt.Errorf("could not create MachineConfig from new Ignition config: %v", err)
		}
		res = append(res, mc)
	}
	return res, nil
}

// RunImageBootstrap generates MachineConfig objects for mcpPools that would have been generated by syncImageConfig,
// except that mcfgv1.Image is not available.
func RunImageBootstrap(templateDir string, controllerConfig *mcfgv1.ControllerConfig, mcpPools []*mcfgv1.MachineConfigPool, icspRules []*apioperatorsv1alpha1.ImageContentSourcePolicy, imgCfg *apicfgv1.Image) ([]*mcfgv1.MachineConfig, error) {
	var (
		insecureRegs []string
		blockedRegs  []string
		allowedRegs  []string
		searchRegs   []string
		err          error
	)

	// Read the search, insecure, blocked, and allowed registries from the cluster-wide Image CR if it is not nil
	if imgCfg != nil {
		insecureRegs = imgCfg.Spec.RegistrySources.InsecureRegistries
		allowedRegs = imgCfg.Spec.RegistrySources.AllowedRegistries
		searchRegs = imgCfg.Spec.RegistrySources.ContainerRuntimeSearchRegistries
		blockedRegs, err = getValidBlockedRegistries(controllerConfig.Spec.ReleaseImage, &imgCfg.Spec)
		if err != nil && err != errParsingReference {
			glog.V(2).Infof("%v, skipping....", err)
		} else if err == errParsingReference {
			return nil, err
		}
	}

	var res []*mcfgv1.MachineConfig
	for _, pool := range mcpPools {
		role := pool.Name
		managedKey, err := getManagedKeyReg(pool, nil)
		if err != nil {
			return nil, err
		}
		registriesIgn, err := registriesConfigIgnition(templateDir, controllerConfig, role,
			insecureRegs, blockedRegs, allowedRegs, searchRegs, icspRules)
		if err != nil {
			return nil, err
		}
		mc, err := ctrlcommon.MachineConfigFromIgnConfig(role, managedKey, registriesIgn)
		if err != nil {
			return nil, err
		}
		// Explicitly do NOT set GeneratedByControllerVersionAnnotationKey so that the first run of the non-bootstrap controller
		// always rebuilds registries.conf (with the insecureRegs/blockedRegs values actually available).
		mc.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: apicfgv1.SchemeGroupVersion.String(),
				Kind:       "Image",
				// Name and UID is not set, the first run of syncImageConfig will overwrite these values.
			},
		}
		res = append(res, mc)
	}
	return res, nil
}

func (ctrl *Controller) popFinalizerFromContainerRuntimeConfig(ctrCfg *mcfgv1.ContainerRuntimeConfig) error {
	return retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, err := ctrl.mccrLister.Get(ctrCfg.Name)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		curJSON, err := json.Marshal(newcfg)
		if err != nil {
			return err
		}

		ctrCfgTmp := newcfg.DeepCopy()
		ctrCfgTmp.Finalizers = append(ctrCfg.Finalizers[:0], ctrCfg.Finalizers[1:]...)

		modJSON, err := json.Marshal(ctrCfgTmp)
		if err != nil {
			return err
		}

		patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(curJSON, modJSON, curJSON)
		if err != nil {
			return err
		}
		return ctrl.patchContainerRuntimeConfigs(ctrCfg.Name, patch)
	})
}

func (ctrl *Controller) patchContainerRuntimeConfigs(name string, patch []byte) error {
	_, err := ctrl.client.MachineconfigurationV1().ContainerRuntimeConfigs().Patch(context.TODO(), name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (ctrl *Controller) addFinalizerToContainerRuntimeConfig(ctrCfg *mcfgv1.ContainerRuntimeConfig, mc *mcfgv1.MachineConfig) error {
	return retry.RetryOnConflict(updateBackoff, func() error {
		newcfg, err := ctrl.mccrLister.Get(ctrCfg.Name)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		curJSON, err := json.Marshal(newcfg)
		if err != nil {
			return err
		}

		ctrCfgTmp := newcfg.DeepCopy()
		// Only append the mc name if it is already not in the list of finalizers.
		// When we update an existing ctrcfg, the generation number increases causing
		// a resync to happen. When this happens, the mc name is the same, so we don't
		// want to add duplicate entries to the list of finalizers.
		if !ctrlcommon.InSlice(mc.Name, ctrCfgTmp.Finalizers) {
			ctrCfgTmp.Finalizers = append(ctrCfgTmp.Finalizers, mc.Name)
		}

		modJSON, err := json.Marshal(ctrCfgTmp)
		if err != nil {
			return err
		}

		patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(curJSON, modJSON, curJSON)
		if err != nil {
			return err
		}
		return ctrl.patchContainerRuntimeConfigs(ctrCfg.Name, patch)
	})
}

func (ctrl *Controller) getPoolsForContainerRuntimeConfig(config *mcfgv1.ContainerRuntimeConfig) ([]*mcfgv1.MachineConfigPool, error) {
	pList, err := ctrl.mcpLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	selector, err := metav1.LabelSelectorAsSelector(config.Spec.MachineConfigPoolSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector: %v", err)
	}

	var pools []*mcfgv1.MachineConfigPool
	for _, p := range pList {
		// If a pool with a nil or empty selector creeps in, it should match nothing, not everything.
		if selector.Empty() || !selector.Matches(labels.Set(p.Labels)) {
			continue
		}
		pools = append(pools, p)
	}

	if len(pools) == 0 {
		return nil, fmt.Errorf("could not find any MachineConfigPool set for ContainerRuntimeConfig %s", config.Name)
	}

	return pools, nil
}
