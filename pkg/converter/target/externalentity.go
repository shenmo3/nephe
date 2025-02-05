// Copyright 2022 Antrea Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package target

import (
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	antreatypes "antrea.io/antrea/pkg/apis/crd/v1alpha2"
)

const (
	// LabelSizeLimit K8s label requirements, https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/.
	LabelSizeLimit  = 64
	LabelExpression = "[^a-zA-Z0-9_-]+"
)

type ExternalEntitySource interface {
	client.Object
	// GetEndPointAddresses returns IP addresses of ExternalEntitySource.
	// Passing client in case there are references needs to be retrieved from local cache.
	GetEndPointAddresses() ([]string, error)
	// GetEndPointPort returns port and port name, if applicable, of ExternalEntitySource.
	GetEndPointPort(client client.Client) []antreatypes.NamedPort
	// GetTags returns tags of ExternalEntitySource.
	GetTags() map[string]string
	// GetLabelsFromClient returns labels specific to ExternalEntitySource.
	GetLabelsFromClient(client client.Client) map[string]string
	// GetExternalNodeName returns controller associated with VirtualMachine.
	GetExternalNodeName(client client.Client) string
	// Copy return a duplicate of current ExternalEntitySource.
	Copy() (duplicate interface{})
	// EmbedType returns the underlying ExternalEntitySource source resource.
	EmbedType() client.Object
}

// NewExternalEntityFrom generate a new ExternalEntity from source.
func NewExternalEntityFrom(
	source ExternalEntitySource, name, namespace string, cl client.Client,
	scheme *runtime.Scheme) *antreatypes.ExternalEntity {
	externalEntity := &antreatypes.ExternalEntity{}
	_ = populateExternalEntityFrom(source, externalEntity, cl)
	externalEntity.SetName(name)
	externalEntity.SetNamespace(namespace)
	return externalEntity
}

// PatchExternalEntityFrom generate a patch for existing ExternalEntity from source.
func PatchExternalEntityFrom(
	source ExternalEntitySource, patch *antreatypes.ExternalEntity, cl client.Client) (*antreatypes.ExternalEntity, bool) {
	changed := populateExternalEntityFrom(source, patch, cl)
	return patch, changed
}

func populateExternalEntityFrom(source ExternalEntitySource, externalEntity *antreatypes.ExternalEntity,
	cl client.Client) bool {
	changed := false
	if !reflect.DeepEqual(externalEntity.GetLabels(), genTargetEntityLabels(source, cl)) {
		externalEntity.SetLabels(genTargetEntityLabels(source, cl))
		changed = true
	}
	ipAddrs, _ := source.GetEndPointAddresses()
	endpoints := make([]antreatypes.Endpoint, 0, len(ipAddrs))
	for _, ip := range ipAddrs {
		endpoints = append(endpoints, antreatypes.Endpoint{IP: ip})
	}
	if externalEntity.Spec.ExternalNode != source.GetExternalNodeName(cl) {
		externalEntity.Spec.ExternalNode = source.GetExternalNodeName(cl)
		changed = true
	}
	sourcePorts := source.GetEndPointPort(cl)
	sortNamedPort(externalEntity.Spec.Ports)
	sortNamedPort(sourcePorts)
	if !reflect.DeepEqual(externalEntity.Spec.Ports, sourcePorts) {
		externalEntity.Spec.Ports = sourcePorts
		changed = true
	}

	sortEndpoint(externalEntity.Spec.Endpoints)
	sortEndpoint(endpoints)
	if !reflect.DeepEqual(externalEntity.Spec.Endpoints, endpoints) {
		externalEntity.Spec.Endpoints = endpoints
		changed = true
	}

	return changed
}

func sortNamedPort(ports []antreatypes.NamedPort) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Name != ports[j].Name {
			return strings.Compare(ports[i].Name, ports[j].Name) < 0
		}
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return strings.Compare(string(ports[i].Protocol), string(ports[j].Protocol)) < 0
	})
}

func sortEndpoint(endpoints []antreatypes.Endpoint) {
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Name != endpoints[j].Name {
			return strings.Compare(endpoints[i].Name, endpoints[j].Name) < 0
		}
		return strings.Compare(endpoints[i].IP, endpoints[j].IP) < 0
	})
}
