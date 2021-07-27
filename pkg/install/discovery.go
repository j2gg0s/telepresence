package install

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/datawire/ambassador/pkg/kates"
)

// svcPortByNameOrNumber iterates through a list of ports in a service and
// only returns the ports that match the given nameOrNumber
func svcPortByNameOrNumber(svc *kates.Service, nameOrNumber string) []*kates.ServicePort {
	svcPorts := make([]*kates.ServicePort, 0)
	ports := svc.Spec.Ports
	var isName bool
	validName := validation.IsValidPortName(nameOrNumber)
	if len(validName) > 0 {
		isName = false
	} else {
		isName = true
	}
	for i := range ports {
		port := &ports[i]
		matchFound := false
		// If no nameOrNumber has been specified, we include it
		if nameOrNumber == "" {
			matchFound = true
		}
		// If the nameOrNumber is a valid name, we compare it to the
		// name listed in the servicePort
		if isName {
			if nameOrNumber == port.Name {
				matchFound = true
			}
		} else {
			// Otherwise we compare it to the port number
			givenPort, err := strconv.Atoi(nameOrNumber)
			if err == nil && int32(givenPort) == port.Port {
				matchFound = true
			}
		}
		if matchFound {
			svcPorts = append(svcPorts, port)
		}
	}
	return svcPorts
}

func FindMatchingServices(c context.Context, client *kates.Client, portNameOrNumber, svcName, namespace string, labels map[string]string) ([]*kates.Service, error) {
	// TODO: Expensive on large clusters but the problem goes away once we move the installer to the traffic-manager
	var svcs []*kates.Service
	if err := client.List(c, kates.Query{Name: svcName, Kind: "Service", Namespace: namespace}, &svcs); err != nil {
		return nil, err
	}

	// Returns true if selector is completely included in labels
	labelsMatch := func(selector map[string]string) bool {
		if len(selector) == 0 || len(labels) < len(selector) {
			return false
		}
		for k, v := range selector {
			if labels[k] != v {
				return false
			}
		}
		return true
	}

	var matching []*kates.Service
	for _, svc := range svcs {
		if (svcName == "" || svc.Name == svcName) && labelsMatch(svc.Spec.Selector) && len(svcPortByNameOrNumber(svc, portNameOrNumber)) > 0 {
			matching = append(matching, svc)
		}
	}
	return matching, nil
}

func FindMatchingService(c context.Context, client *kates.Client, portNameOrNumber, svcName, namespace string, labels map[string]string) (*kates.Service, error) {
	matchingSvcs, err := FindMatchingServices(c, client, portNameOrNumber, svcName, namespace, labels)
	if err != nil {
		return nil, err
	}
	if len(matchingSvcs) == 1 {
		return matchingSvcs[0], nil
	}

	count := "no"
	suffix := ""
	portRef := ""
	if len(matchingSvcs) > 0 {
		svcNames := make([]string, len(matchingSvcs))
		for i, svc := range matchingSvcs {
			svcNames[i] = svc.Name
		}
		count = "multiple"
		suffix = fmt.Sprintf(", use --service and one of: %s", strings.Join(svcNames, ","))
	}
	if portNameOrNumber != "" {
		portRef = fmt.Sprintf(" and a port referenced by name or port number %s", portNameOrNumber)
	}
	return nil, fmt.Errorf("found %s services with a selector matching labels %v%s in namespace %s%s", count, labels, portRef, namespace, suffix)
}

// FindMatchingPort finds the matching container associated with portNameOrNumber
// in the given service.
func FindMatchingPort(cns []corev1.Container, portNameOrNumber string, svc *kates.Service) (
	sPort *kates.ServicePort,
	cn *kates.Container,
	cPortIndex int,
	err error,
) {
	// For now, we only support intercepting one port on a given service.
	ports := svcPortByNameOrNumber(svc, portNameOrNumber)
	switch numPorts := len(ports); {
	case numPorts == 0:
		// this may happen when portNameOrNumber is specified but none of the
		// ports match
		return nil, nil, 0, errors.New("found no Service with a port that matches any container in this workload")

	case numPorts > 1:
		return nil, nil, 0, errors.New(`found matching Service with multiple matching ports.
Please specify the Service port you want to intercept by passing the --port=local:svcPortName flag.`)
	default:
	}
	port := ports[0]
	var matchingServicePort *corev1.ServicePort
	var matchingContainer *corev1.Container
	var containerPortIndex int

	if port.TargetPort.Type == intstr.String {
		portName := port.TargetPort.StrVal
		for ci := 0; ci < len(cns) && matchingContainer == nil; ci++ {
			cn := &cns[ci]
			for pi := range cn.Ports {
				if cn.Ports[pi].Name == portName {
					matchingServicePort = port
					matchingContainer = cn
					containerPortIndex = pi
					break
				}
			}
		}
	} else {
		portNum := port.TargetPort.IntVal
		// First see if we have a container with a matching port
	containerLoop:
		for ci := range cns {
			cn := &cns[ci]
			for pi := range cn.Ports {
				if cn.Ports[pi].ContainerPort == portNum {
					matchingServicePort = port
					matchingContainer = cn
					containerPortIndex = pi
					break
				}
			}
		}
		// If no container matched, then use the first container with no ports at all. This
		// enables intercepts of containers that indeed do listen a port but lack a matching
		// port description in the manifest, which is what you get if you do:
		//     kubectl create deploy my-deploy --image my-image
		//     kubectl expose deploy my-deploy --port 80 --target-port 8080
		if matchingContainer == nil {
			for ci := 0; ci < len(cns); ci++ {
				cn := &cns[ci]
				if len(cn.Ports) == 0 {
					matchingServicePort = port
					matchingContainer = cn
					containerPortIndex = -1
					break
				}	
			}
		}
	}

	if matchingServicePort == nil {
		return nil, nil, 0, errors.New("found no Service with a port that matches any container in this workload")
	}
	return matchingServicePort, matchingContainer, containerPortIndex, nil
}
