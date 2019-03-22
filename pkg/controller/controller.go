package controller

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/api/v1/endpoints"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"

	"github.com/intel/multus-cni/types"

	netattachdef "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	informers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
)

const (
	selectionsKey       = "k8s.v1.cni.cncf.io/networks"
	statusesKey         = "k8s.v1.cni.cncf.io/networks-status"
	controllerAgentName = "k8s-net-attach-def-controller"
)

// NetworkController is the controller implementation for handling net-attach-def resources and other objects using them
type NetworkController struct {
	k8sClientSet          kubernetes.Interface
	netAttachDefClientSet clientset.Interface

	netAttachDefsSynced cache.InformerSynced

	podsLister corelisters.PodLister
	podsSynced cache.InformerSynced

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointsLister corelisters.EndpointsLister
	endpointsSynced cache.InformerSynced

	workqueue workqueue.RateLimitingInterface

	recorder record.EventRecorder
}

// NewNetworkController returns new NetworkController instance
func NewNetworkController(
	k8sClientSet kubernetes.Interface,
	netAttachDefClientSet clientset.Interface,
	netAttachDefInformer informers.NetworkAttachmentDefinitionInformer,
	serviceInformer coreinformers.ServiceInformer,
	podInformer coreinformers.PodInformer,
	endpointInformer coreinformers.EndpointsInformer) *NetworkController {

	klog.V(3).Info("creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: k8sClientSet.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	NetworkController := &NetworkController{
		k8sClientSet:          k8sClientSet,
		netAttachDefClientSet: netAttachDefClientSet,
		netAttachDefsSynced:   netAttachDefInformer.Informer().HasSynced,
		servicesSynced:        serviceInformer.Informer().HasSynced,
		podsSynced:            podInformer.Informer().HasSynced,
		endpointsSynced:       endpointInformer.Informer().HasSynced,
		serviceLister:         serviceInformer.Lister(),
		podsLister:            podInformer.Lister(),
		endpointsLister:       endpointInformer.Lister(),
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "secondary_endpoints"),
		recorder:              recorder,
	}

	netAttachDefInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: NetworkController.handleNetAttachDefDeleteEvent,
	})

	/* setup handlers for endpoints events */
	endpointInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: NetworkController.handleEndpointEvent,
		UpdateFunc: func(old, updated interface{}) {
			if objectChanged(old, updated) {
				NetworkController.handleEndpointEvent(updated)
			}
		},
		DeleteFunc: NetworkController.handleEndpointEvent,
	})

	/* setup handlers for services events */
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: NetworkController.handleServiceEvent,
		UpdateFunc: func(old, updated interface{}) {
			if objectChanged(old, updated) || networkAnnotationsChanged(old, updated) {
				NetworkController.handleServiceEvent(updated)
			}
		},
		DeleteFunc: NetworkController.handleServiceEvent,
	})

	/* setup handlers for pods events */
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: NetworkController.handlePodEvent,
		UpdateFunc: func(old, updated interface{}) {
			if objectChanged(old, updated) {
				NetworkController.handlePodEvent(updated)
			}
		},
		DeleteFunc: NetworkController.handlePodEvent,
	})

	return NetworkController
}

func (c *NetworkController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *NetworkController) processNextWorkItem() bool {
	key, shouldQuit := c.workqueue.Get()
	if shouldQuit {
		return false
	}
	defer c.workqueue.Done(key)

	err := c.sync(key.(string))
	if err != nil {
		klog.V(4).Infof("sync aborted: %s", err)
	}

	return true
}

func (c *NetworkController) sync(key string) error {
	// get service object from the key
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil {
		return err
	}

	// read network annotations from the service
	annotations := getNetworkAnnotations(svc)
	if len(annotations) == 0 {
		return errors.New("no network annotations")
	}
	klog.V(3).Infof("service network annotation found: %v", annotations)
	networks, err := parsePodNetworkSelections(annotations, namespace)
	if err != nil {
		return err
	}
	if len(networks) > 1 {
		msg := fmt.Sprintf("multiple network selections in the service spec are not supported")
		klog.Warningf(msg)
		c.recorder.Event(svc, corev1.EventTypeWarning, msg, "Endpoints update aborted")
		return errors.New(msg)
	}

	// get pods matching service selector
	selector := labels.Set(svc.Spec.Selector).AsSelector()
	pods, err := c.podsLister.List(selector)
	if err != nil {
		// no selector or no pods running
		klog.V(4).Info("error listing pods matching service selector: %s", err)
		return err
	}

	// get endpoints of the service
	ep, err := c.endpointsLister.Endpoints(namespace).Get(name)
	if err != nil {
		klog.V(4).Info("error getting service endpoints: %s", err)
		return err
	}

	subsets := make([]corev1.EndpointSubset, 0)

	for _, pod := range pods {
		addresses := make([]corev1.EndpointAddress, 0)
		ports := make([]corev1.EndpointPort, 0)

		networksStatus := make([]types.NetworkStatus, 0)
		err := json.Unmarshal([]byte(pod.Annotations[statusesKey]), &networksStatus)
		if err != nil {
			klog.Warningf("error reading pod networks status: %s", err)
			continue
		}
		// find networks used by pod and match network annotation of the service
		for _, status := range networksStatus {
			if isInNetworkSelectionElementsArray(status.Name, networks) {
				klog.V(3).Infof("processing pod %s/%s: found network %s interface %s with IP addresses %s",
					pod.Namespace, pod.Name, annotations, status.Interface, status.IPs)
				// all IPs of matching network are added as endpoints
				for _, ip := range status.IPs {
					epAddress := corev1.EndpointAddress{
						IP:       ip,
						NodeName: &pod.Spec.NodeName,
						TargetRef: &corev1.ObjectReference{
							Kind:            "Pod",
							Name:            pod.GetName(),
							Namespace:       pod.GetNamespace(),
							ResourceVersion: pod.GetResourceVersion(),
							UID:             pod.GetUID(),
						},
					}
					addresses = append(addresses, epAddress)
				}
			}
		}
		for i := range svc.Spec.Ports {
			// check whether pod has the ports needed by service and add them to endpoints if so
			portNumber, err := podutil.FindPort(pod, &svc.Spec.Ports[i])
			if err != nil {
				klog.V(4).Infof("Could not find pod port for service %s/%s: %s, skipping...", svc.Namespace, svc.Name, err)
				continue
			}

			port := corev1.EndpointPort{
				Port:     int32(portNumber),
				Protocol: svc.Spec.Ports[i].Protocol,
				Name:     svc.Spec.Ports[i].Name,
			}
			ports = append(ports, port)
		}
		subset := corev1.EndpointSubset{
			Addresses: addresses,
			Ports:     ports,
		}
		subsets = append(subsets, subset)
	}

	ep.SetOwnerReferences(
		[]metav1.OwnerReference{
			*metav1.NewControllerRef(svc, schema.GroupVersionKind{
				Group:   corev1.SchemeGroupVersion.Group,
				Version: corev1.SchemeGroupVersion.Version,
				Kind:    "Service",
			}),
		},
	)

	// repack subsets - NOTE: too naive? additional checks needed?
	ep.Subsets = endpoints.RepackSubsets(subsets)

	// update endpoints resource
	_, err = c.k8sClientSet.Core().Endpoints(ep.Namespace).Update(ep)
	if err != nil {
		klog.Errorf("error updating endpoint: %s", err)
		return err
	}

	klog.Info("endpoint updated successfully")
	msg := fmt.Sprintf("Updated to use network %s", annotations)

	c.recorder.Event(ep, corev1.EventTypeNormal, msg, "Endpoints update successful")
	c.recorder.Event(svc, corev1.EventTypeNormal, msg, "Endpoints update successful")

	return nil
}

func (c *NetworkController) handleServiceEvent(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

func (c *NetworkController) handlePodEvent(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	// if no network annotation discard
	_, ok = pod.GetAnnotations()[selectionsKey]
	if !ok {
		klog.V(4).Info("skipping pod event: network annotations missing")
		return
	}

	// if not behind any service discard
	services, err := c.serviceLister.GetPodServices(pod)
	if err != nil {
		klog.V(4).Info("skipping pod event: %s", err)
		return
	}
	for _, svc := range services {
		c.handleServiceEvent(svc)
	}
}

func (c *NetworkController) handleEndpointEvent(obj interface{}) {
	ep := obj.(*corev1.Endpoints)

	// get service associated with endpoints instance
	svc, err := c.serviceLister.Services(ep.GetNamespace()).Get(ep.GetName())
	if err != nil {
		// errors are returned for service-less endpoints such as kube-scheduler and kube-controller-manager
		return
	}

	c.handleServiceEvent(svc)
}

func (c *NetworkController) handleNetAttachDefDeleteEvent(obj interface{}) {
	klog.V(3).Info("net-attach-def delete event received")
	netAttachDef, ok := obj.(metav1.Object)
	if ok {
		name := netAttachDef.GetName()
		namespace := netAttachDef.GetNamespace()
		klog.Infof("handling deletion of %s/%s", namespace, name)
		/* NOTE: try to do something smarter - searching in pods based on the annotation if possible? */
		pods, _ := c.podsLister.Pods("").List(labels.Everything())
		/* check whether net-attach-def requested to be removed is still in use by any of the pods */
		for _, pod := range pods {
			netAnnotations, ok := pod.ObjectMeta.Annotations[selectionsKey]
			if !ok {
				continue
			}
			podNetworks, err := parsePodNetworkSelections(netAnnotations, pod.ObjectMeta.Namespace)
			if err != nil {
				continue
			}
			for _, net := range podNetworks {
				if net.Namespace == namespace && net.Name == name {
					klog.Infof("pod %s uses net-attach-def %s/%s which needs to be recreated\n", pod.ObjectMeta.Name, namespace, name)
					/* check whether the object somehow still exists */
					_, err := c.netAttachDefClientSet.K8sCniCncfIo().
						NetworkAttachmentDefinitions(netAttachDef.GetNamespace()).
						Get(netAttachDef.GetName(), metav1.GetOptions{})
					if err != nil {
						/* recover deleted object */
						recovered := obj.(*netattachdef.NetworkAttachmentDefinition).DeepCopy()
						recovered.ObjectMeta.ResourceVersion = "" // ResourceVersion field needs to be cleared before recreating the object
						_, err = c.netAttachDefClientSet.
							K8sCniCncfIo().
							NetworkAttachmentDefinitions(netAttachDef.GetNamespace()).
							Create(recovered)
						if err != nil {
							klog.Errorf("error recreating recovered object: %s", err.Error())
						}
						klog.V(4).Infof("net-attach-def recovered: %v", recovered)
						return
					}
				}
			}
		}
	}
}

// Start runs worker thread after performing cache synchronization
func (c *NetworkController) Start(stopChan <-chan struct{}) {
	klog.V(4).Infof("starting network controller")
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopChan, c.netAttachDefsSynced, c.endpointsSynced, c.servicesSynced, c.podsSynced); !ok {
		klog.Fatalf("failed waiting for caches to sync")
	}

	go wait.Until(c.worker, time.Second, stopChan)

	<-stopChan
	klog.V(4).Infof("shutting down network controller")
	return
}
