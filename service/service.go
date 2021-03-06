package service

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	patchtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/cmd/kubeadm/app/util"

	"github.com/argoproj/argo-rollouts/controller/metrics"
	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	clientset "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned"
	informers "github.com/argoproj/argo-rollouts/pkg/client/informers/externalversions/rollouts/v1alpha1"
	controllerutil "github.com/argoproj/argo-rollouts/utils/controller"
	logutil "github.com/argoproj/argo-rollouts/utils/log"
	serviceutil "github.com/argoproj/argo-rollouts/utils/service"
)

const (
	// serviceIndexName is the index by which Service resources are cached
	serviceIndexName    = "byService"
	removeSelectorPatch = `[
		{ "op": "remove", "path": "/spec/selector/` + v1alpha1.DefaultRolloutUniqueLabelKey + `" }
	]`
	removeSelectorAndManagedByPatch = `[
		{ "op": "remove", "path": "/spec/selector/` + v1alpha1.DefaultRolloutUniqueLabelKey + `" },
		{ "op": "remove", "path": "/annotations/` + v1alpha1.ManagedByRolloutsKey + `" }
	]`
)

type ControllerConfig struct {
	Kubeclientset     kubernetes.Interface
	Argoprojclientset clientset.Interface

	RolloutsInformer informers.RolloutInformer
	ServicesInformer coreinformers.ServiceInformer

	RolloutWorkqueue workqueue.RateLimitingInterface
	ServiceWorkqueue workqueue.RateLimitingInterface

	ResyncPeriod time.Duration

	MetricsServer *metrics.MetricsServer
}

type ServiceController struct {
	kubeclientset     kubernetes.Interface
	argoprojclientset clientset.Interface
	rolloutsIndexer   cache.Indexer
	rolloutSynced     cache.InformerSynced
	servicesLister    v1.ServiceLister
	serviceSynced     cache.InformerSynced
	rolloutWorkqueue  workqueue.RateLimitingInterface
	serviceWorkqueue  workqueue.RateLimitingInterface
	resyncPeriod      time.Duration

	metricServer   *metrics.MetricsServer
	enqueueRollout func(obj interface{})
}

// NewServiceController returns a new service controller
func NewServiceController(cfg ControllerConfig) *ServiceController {

	controller := &ServiceController{
		kubeclientset:     cfg.Kubeclientset,
		argoprojclientset: cfg.Argoprojclientset,
		rolloutsIndexer:   cfg.RolloutsInformer.Informer().GetIndexer(),
		rolloutSynced:     cfg.RolloutsInformer.Informer().HasSynced,
		servicesLister:    cfg.ServicesInformer.Lister(),
		serviceSynced:     cfg.ServicesInformer.Informer().HasSynced,

		rolloutWorkqueue: cfg.RolloutWorkqueue,
		serviceWorkqueue: cfg.ServiceWorkqueue,
		resyncPeriod:     cfg.ResyncPeriod,
		metricServer:     cfg.MetricsServer,
	}

	util.CheckErr(cfg.RolloutsInformer.Informer().AddIndexers(cache.Indexers{
		serviceIndexName: func(obj interface{}) (strings []string, e error) {
			if rollout, ok := obj.(*v1alpha1.Rollout); ok {
				return serviceutil.GetRolloutServiceKeys(rollout), nil
			}
			return []string{}, nil
		},
	}))

	cfg.ServicesInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controllerutil.Enqueue(obj, cfg.ServiceWorkqueue)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			controllerutil.Enqueue(newObj, cfg.ServiceWorkqueue)
		},
		DeleteFunc: func(obj interface{}) {
			controllerutil.Enqueue(obj, cfg.ServiceWorkqueue)
		},
	})
	controller.enqueueRollout = func(obj interface{}) {
		controllerutil.EnqueueRateLimited(obj, cfg.RolloutWorkqueue)
	}

	return controller
}

func (c *ServiceController) Run(threadiness int, stopCh <-chan struct{}) error {
	log.Info("Starting Service workers")
	for i := 0; i < threadiness; i++ {
		go wait.Until(func() {
			controllerutil.RunWorker(c.serviceWorkqueue, logutil.ServiceKey, c.syncService, c.metricServer)
		}, time.Second, stopCh)
	}

	log.Info("Started Service workers")
	<-stopCh
	log.Info("Shutting down workers")

	return nil
}

func (c *ServiceController) syncService(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	svc, err := c.servicesLister.Services(namespace).Get(name)
	if errors.IsNotFound(err) {
		log.WithField(logutil.ServiceKey, key).Infof("Service %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}
	rollouts, err := c.getRolloutsByService(svc.Namespace, svc.Name)
	if err != nil {
		return nil
	}

	for i := range rollouts {
		c.enqueueRollout(rollouts[i])
	}
	// Return early if the svc does not have a hash selector or there is a rollout with matching this service
	if _, hasHashSelector := svc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]; !hasHashSelector || len(rollouts) > 0 {
		return nil
	}
	// Handles case where the controller is not watching all Rollouts in the cluster due to instance-ids.
	rolloutName, hasManagedBy := serviceutil.HasManagedByAnnotation(svc)
	if hasManagedBy {
		_, err := c.argoprojclientset.ArgoprojV1alpha1().Rollouts(svc.Namespace).Get(rolloutName, metav1.GetOptions{})
		if err == nil {
			return nil
		}
	}
	updatedSvc := svc.DeepCopy()
	patch := generateRemovePatch(updatedSvc)
	_, err = c.kubeclientset.CoreV1().Services(updatedSvc.Namespace).Patch(updatedSvc.Name, patchtypes.JSONPatchType, []byte(patch))
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func generateRemovePatch(svc *corev1.Service) string {
	if _, ok := svc.Annotations[v1alpha1.ManagedByRolloutsKey]; ok {
		return removeSelectorAndManagedByPatch
	}
	return removeSelectorPatch
}

// getRolloutsByService returns all rollouts which are referencing specified service
func (c *ServiceController) getRolloutsByService(namespace string, serviceName string) ([]*v1alpha1.Rollout, error) {
	objs, err := c.rolloutsIndexer.ByIndex(serviceIndexName, fmt.Sprintf("%s/%s", namespace, serviceName))
	if err != nil {
		return nil, err
	}
	var rollouts []*v1alpha1.Rollout
	for i := range objs {
		if r, ok := objs[i].(*v1alpha1.Rollout); ok {
			rollouts = append(rollouts, r)
		}
	}
	return rollouts, nil
}
