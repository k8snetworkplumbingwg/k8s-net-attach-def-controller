package controller

import (
	"encoding/json"
	"github.com/pkg/errors"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/intel/multus-cni/types"
)

func objectChanged(previous, current interface{}) bool {
	prev := previous.(metav1.Object)
	cur := current.(metav1.Object)
	return prev.GetResourceVersion() != cur.GetResourceVersion()
}

func networkAnnotationsChanged(previous, current interface{}) bool {
	oldAnnotations := getNetworkAnnotations(previous)
	updatedAnnotations := getNetworkAnnotations(current)
	return oldAnnotations != updatedAnnotations
}

// FIXME
func networkStatusChanged(previous, current interface{}) bool {
	return true
}

func getNetworkAnnotations(obj interface{}) string {
	metaObject := obj.(metav1.Object)
	annotations, ok := metaObject.GetAnnotations()[selectionsKey]
	if !ok {
		return ""
	}
	return annotations
}

func isInNetworkSelectionElementsArray(name string, networks []*types.NetworkSelectionElement) bool {
	// NOTE: what about namespaces
	for i := range networks {
		if name == networks[i].Name {
			return true
		}
	}
	return false
}

// NOTE: two below functions are copied from the net-attach-def admission controller, to be replaced with better implementation
func parsePodNetworkSelections(podNetworks, defaultNamespace string) ([]*types.NetworkSelectionElement, error) {
	var networkSelections []*types.NetworkSelectionElement

	if len(podNetworks) == 0 {
		err := errors.New("empty string passed as network selection elements list")
		klog.Error(err)
		return nil, err
	}

	/* try to parse as JSON array */
	err := json.Unmarshal([]byte(podNetworks), &networkSelections)

	/* if failed, try to parse as comma separated */
	if err != nil {
		klog.V(4).Infof("'%s' is not in JSON format: %s... trying to parse as comma separated network selections list", podNetworks, err)
		for _, networkSelection := range strings.Split(podNetworks, ",") {
			networkSelection = strings.TrimSpace(networkSelection)
			networkSelectionElement, err := parsePodNetworkSelectionElement(networkSelection, defaultNamespace)
			if err != nil {
				err := errors.Wrap(err, "error parsing network selection element")
				klog.Error(err)
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
		klog.Error(err)
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
		klog.Error(err)
		return networkSelectionElement, err
	}

	validNameRegex, _ := regexp.Compile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, unit := range []string{namespace, name, netInterface} {
		ok := validNameRegex.MatchString(unit)
		if !ok && len(unit) > 0 {
			err := errors.Errorf("at least one of the network selection units is invalid: error found at '%s'", unit)
			klog.Error(err)
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
