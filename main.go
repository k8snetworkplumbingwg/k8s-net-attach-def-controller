package main

import (
	"flag"
	"os"
	"os/signal"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"

	clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	sharedInformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"

	"github.com/K8sNetworkPlumbingWG/k8s-net-attach-def-controller/pkg/controller"
)

var (
	master     string
	kubeconfig string

	// defines default resync period between k8s API server and controller
	syncPeriod = time.Second * 5
)

func main() {
	flag.StringVar(&master, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Required if out-of-cluster.")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Required if out-of-cluster.")

	flag.Parse()

	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			f2.Value.Set(value)
		}
	})

	cfg, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		klog.Fatalf("error building kubeconfig: %s", err.Error())
	}

	k8sClientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("error creating kubernetes clientset: %s", err.Error())
	}

	netAttachDefClientSet, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("error creating net-attach-def clientset: %s", err.Error())
	}

	netAttachDefInformerFactory := sharedInformers.NewSharedInformerFactory(netAttachDefClientSet, syncPeriod)
	k8sInformerFactory := informers.NewSharedInformerFactory(k8sClientSet, syncPeriod)

	networkController := controller.NewNetworkController(
		k8sClientSet,
		netAttachDefClientSet,
		netAttachDefInformerFactory.K8sCniCncfIo().V1().NetworkAttachmentDefinitions(),
		k8sInformerFactory.Core().V1().Services(),
		k8sInformerFactory.Core().V1().Pods(),
		k8sInformerFactory.Core().V1().Endpoints(),
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
	networkController.Start(stopChan)
}
