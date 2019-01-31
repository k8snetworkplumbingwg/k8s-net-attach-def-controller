package main

import (
	"encoding/json"
	"flag"
	"github.com/pkg/errors"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sinformers "k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/intel/multus-cni/types"

	netattachdef "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	sharedInformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"
	informers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
)

var (
	master     string
	kubeconfig string

	// defines default resync period for net-attach-def objects between k8s API server and controller
	syncPeriod = time.Second * 30
)

type NetworkController struct {
	k8sClientSet          kubernetes.Interface
	netAttachDefClientSet clientset.Interface
	netAttachDefsSynced   cache.InformerSynced
	podsLister            corelisters.PodLister
	// NOTE: implement workqueue for queuing incoming requests
}

func NewNetworkController(
	k8sClientSet kubernetes.Interface,
	netAttachDefClientSet clientset.Interface,
	netAttachDefInformer informers.NetworkAttachmentDefinitionInformer,
	podInformer coreinformers.PodInformer) *NetworkController {

	networkController := &NetworkController{
		k8sClientSet:          k8sClientSet,
		netAttachDefClientSet: netAttachDefClientSet,
		netAttachDefsSynced:   netAttachDefInformer.Informer().HasSynced,
		podsLister:            podInformer.Lister(),
	}

	netAttachDefInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		/* NOTE:
		instead of calling handleNetAttachDefDeleteEvent directly, it would be better to add item
		to a workqueue and handle requests from there
		*/
		DeleteFunc: networkController.handleNetAttachDefDeleteEvent,
	})

	return networkController
}

func (c *NetworkController) handleNetAttachDefDeleteEvent(obj interface{}) {
	log.Printf("net-attach-def delete event received")
	//	netAttachDef, ok := obj.(netattachdef.NetworkAttachmentDefinition)
	netAttachDef, ok := obj.(metav1.Object)
	if ok {
		name := netAttachDef.GetName()
		namespace := netAttachDef.GetNamespace()
		log.Printf("handling deletion of %s/%s", namespace, name)
		/* NOTE: try to do something smarter - searching in pods based on the annotation if possible? */
		pods, _ := c.podsLister.Pods("").List(labels.Everything())
		/* check whether net-attach-def requested to be removed is still in use by any of the pods */
		for _, pod := range pods {
			netAnnotations, ok := pod.ObjectMeta.Annotations["k8s.v1.cni.cncf.io/networks"]
			if !ok {
				continue
			}
			podNetworks, err := parsePodNetworkSelections(netAnnotations, pod.ObjectMeta.Namespace)
			if err != nil {
				continue
			}
			for _, net := range podNetworks {
				if net.Namespace == namespace && net.Name == name {
					log.Printf("pod %s uses net-attach-def %s/%s which needs to be recreated\n", pod.ObjectMeta.Name, namespace, name)
					/* check whether object somehow still exists */
					_, err := c.netAttachDefClientSet.K8sCniCncfIo().NetworkAttachmentDefinitions(netAttachDef.GetNamespace()).Get(netAttachDef.GetName(), metav1.GetOptions{})
					if err != nil {
						/* recover deleted object */
						recovered := obj.(*netattachdef.NetworkAttachmentDefinition).DeepCopy()
						recovered.ObjectMeta.ResourceVersion = "" // ResourceVersion field needs to be cleared before recreating the object
						_, err = c.netAttachDefClientSet.K8sCniCncfIo().NetworkAttachmentDefinitions(netAttachDef.GetNamespace()).Create(recovered)
						if err != nil {
							log.Printf("error recreating recovered object: %s", err.Error())
						}
						log.Printf("net-attach-def recovered: %v", recovered)
						return
					}
				}
			}
		}
	}
}

func (c *NetworkController) Start(stopChan <-chan struct{}) {
	log.Printf("starting network controller")

	if ok := cache.WaitForCacheSync(stopChan, c.netAttachDefsSynced); !ok {
		log.Fatalf("failed to wait for caches to sync")
	}

	<-stopChan
	log.Printf("shutting down network controller")
	return
}

func main() {
	flag.StringVar(&master, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		log.Fatalf("error building kubeconfig: %s", err.Error())
	}

	k8sClientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("error creating kubernetes clientset: %s", err.Error())
	}

	netAttachDefClientSet, err := clientset.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("error creating net-attach-def clientset: %s", err.Error())
	}

	netAttachDefInformerFactory := sharedInformers.NewSharedInformerFactory(netAttachDefClientSet, syncPeriod)
	k8sInformerFactory := k8sinformers.NewSharedInformerFactory(k8sClientSet, syncPeriod)

	controller := NewNetworkController(
		k8sClientSet,
		netAttachDefClientSet,
		netAttachDefInformerFactory.K8sCniCncfIo().V1().NetworkAttachmentDefinitions(),
		k8sInformerFactory.Core().V1().Pods(),
	)

	stopChan := make(chan struct{})
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		close(stopChan)
		<-c
		os.Exit(1)
	}()

	netAttachDefInformerFactory.Start(stopChan)
	k8sInformerFactory.Start(stopChan)
	controller.Start(stopChan)
}

// NOTE: two below functions are copied from the net-attach-def admission controller, to be replaced with better implementation
func parsePodNetworkSelections(podNetworks, defaultNamespace string) ([]*types.NetworkSelectionElement, error) {
	var networkSelections []*types.NetworkSelectionElement

	if len(podNetworks) == 0 {
		err := errors.New("empty string passed as network selection elements list")
		log.Print(err)
		return nil, err
	}

	/* try to parse as JSON array */
	err := json.Unmarshal([]byte(podNetworks), &networkSelections)

	/* if failed, try to parse as comma separated */
	if err != nil {
		log.Printf("'%s' is not in JSON format: %s... trying to parse as comma separated network selections list", podNetworks, err)
		for _, networkSelection := range strings.Split(podNetworks, ",") {
			networkSelection = strings.TrimSpace(networkSelection)
			networkSelectionElement, err := parsePodNetworkSelectionElement(networkSelection, defaultNamespace)
			if err != nil {
				err := errors.Wrap(err, "error parsing network selection element")
				log.Print(err)
				return nil, err
			}
			networkSelections = append(networkSelections, networkSelectionElement)
		}
	}

	/* fill missing namespaces with default value */
	for _, networkSelection := range networkSelections {
		if networkSelection.Namespace == "" {
			networkSelection.Namespace = defaultNamespace
		}
	}

	return networkSelections, nil
}

func parsePodNetworkSelectionElement(selection, defaultNamespace string) (*types.NetworkSelectionElement, error) {
	var namespace, name, netInterface string
	var networkSelectionElement *types.NetworkSelectionElement

	units := strings.Split(selection, "/")
	switch len(units) {
	case 1:
		namespace = defaultNamespace
		name = units[0]
	case 2:
		namespace = units[0]
		name = units[1]
	default:
		err := errors.Errorf("invalid network selection element - more than one '/' rune in: '%s'", selection)
		log.Print(err)
		return networkSelectionElement, err
	}

	units = strings.Split(name, "@")
	switch len(units) {
	case 1:
		name = units[0]
		netInterface = ""
	case 2:
		name = units[0]
		netInterface = units[1]
	default:
		err := errors.Errorf("invalid network selection element - more than one '@' rune in: '%s'", selection)
		log.Print(err)
		return networkSelectionElement, err
	}

	validNameRegex, _ := regexp.Compile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, unit := range []string{namespace, name, netInterface} {
		ok := validNameRegex.MatchString(unit)
		if !ok && len(unit) > 0 {
			err := errors.Errorf("at least one of the network selection units is invalid: error found at '%s'", unit)
			log.Print(err)
			return networkSelectionElement, err
		}
	}

	networkSelectionElement = &types.NetworkSelectionElement{
		Namespace:        namespace,
		Name:             name,
		InterfaceRequest: netInterface,
	}

	return networkSelectionElement, nil
}
