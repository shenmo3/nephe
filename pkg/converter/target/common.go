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
	"antrea.io/nephe/pkg/controllers/config"
	"reflect"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func GetExternalEntityKeyFromSource(source ExternalEntitySource) client.ObjectKey {
	access, _ := meta.Accessor(source)
	return client.ObjectKey{Namespace: access.GetNamespace(),
		Name: getTargetEntityName(source.EmbedType())}
}

func GetExternalNodeKeyFromSource(source ExternalNodeSource) client.ObjectKey {
	access, _ := meta.Accessor(source)
	return client.ObjectKey{Namespace: access.GetNamespace(),
		Name: getTargetEntityName(source.EmbedType())}
}

// GetExternalEntityLabelKind returns value of ExternalEntity kind label.
func GetExternalEntityLabelKind(obj runtime.Object) string {
	return strings.ToLower(reflect.TypeOf(obj).Elem().Name())
}

// getTargetEntityName returns the desired name of the target resource.
func getTargetEntityName(obj runtime.Object) string {
	access, _ := meta.Accessor(obj)
	// ExternalNode/ExternalEntity CR name will be virtualmachine-<VirtualMachine CR name>.
	return strings.ToLower(reflect.TypeOf(obj).Elem().Name()) + "-" + access.GetName()
}

// genTargetEntityLabels labels for any targets of VirtualMachineSource.
func genTargetEntityLabels(source interface{}, cl client.Client) map[string]string {
	// VirtualMachine source implements both ExternalNodeSource and ExternalEntitySource.
	// Either ExternalEntitySource or ExternalNodeSource can be type cast.
	vmSource, _ := source.(ExternalEntitySource)
	labels := make(map[string]string)
	accessor, _ := meta.Accessor(vmSource)
	labels[config.ExternalEntityLabelKeyKind] = GetExternalEntityLabelKind(vmSource.EmbedType())
	labels[config.ExternalEntityLabelKeyVmName] = strings.ToLower(accessor.GetName())
	labels[config.ExternalEntityLabelKeyNamespace] = strings.ToLower(accessor.GetNamespace())
	for key, val := range vmSource.GetLabelsFromClient(cl) {
		labels[key] = val
	}
	for key, val := range vmSource.GetTags() {
		reg, _ := regexp.Compile(LabelExpression)
		fkey := reg.ReplaceAllString(key, "") + config.ExternalEntityLabelKeyTagPostfix
		if len(fkey) > LabelSizeLimit {
			fkey = fkey[:LabelSizeLimit]
		}
		fval := reg.ReplaceAllString(val, "")
		if len(fval) > LabelSizeLimit {
			fval = fval[:LabelSizeLimit]
		}
		labels[strings.ToLower(fkey)] = strings.ToLower(fval)
	}
	return labels
}
