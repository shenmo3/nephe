// Copyright 2023 Antrea Authors.
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

package virtualmachine

import (
	"context"
	"strings"

	logger "github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metatable "k8s.io/apimachinery/pkg/api/meta/table"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	runtimev1alpha1 "antrea.io/nephe/apis/runtime/v1alpha1"
	"antrea.io/nephe/pkg/controllers/config"
	"antrea.io/nephe/pkg/controllers/inventory"
	"antrea.io/nephe/pkg/controllers/inventory/common"
	"antrea.io/nephe/pkg/controllers/inventory/store"
)

// REST implements rest.Storage for VirtualMachine Inventory.
type REST struct {
	cloudInventory inventory.Interface
	logger         logger.Logger
}

var (
	_ rest.Scoper  = &REST{}
	_ rest.Getter  = &REST{}
	_ rest.Watcher = &REST{}
	_ rest.Lister  = &REST{}
)

// NewREST returns a REST object that will work against API services.
func NewREST(cloudInventory inventory.Interface, l logger.Logger) *REST {
	return &REST{
		cloudInventory: cloudInventory,
		logger:         l,
	}
}

func (r *REST) New() runtime.Object {
	return &runtimev1alpha1.VirtualMachine{}
}

func (r *REST) NewList() runtime.Object {
	return &runtimev1alpha1.VirtualMachine{}
}

func (r *REST) ShortNames() []string {
	return []string{"vm"}
}

func (r *REST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	ns, ok := request.NamespaceFrom(ctx)
	if !ok || len(ns) == 0 {
		return nil, errors.NewBadRequest("Namespace cannot be empty.")
	}

	namespacedName := ns + "/" + name
	vm, ok := r.cloudInventory.GetVmByKey(namespacedName)
	if !ok {
		return nil, errors.NewNotFound(runtimev1alpha1.Resource("virtualmachine"), name)
	}
	return vm, nil
}

func (r *REST) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	// List only supports three types of input options:
	// 1. All namespaces.
	// 2. Labelselector with only the specific namespace, the only valid labelselectors are "cpa.name=<accountname>"
	//    and "cpa.namespace=<accountNamespace>".
	// 3. Specific Namespace.
	accountName := ""
	accountNamespace := ""
	if options != nil && options.LabelSelector != nil && options.LabelSelector.String() != "" {
		labelSelectorStrings := strings.Split(options.LabelSelector.String(), ",")
		for _, labelSelectorString := range labelSelectorStrings {
			labelKeyAndValue := strings.Split(labelSelectorString, "=")
			if labelKeyAndValue[0] == config.LabelCloudAccountName {
				accountName = labelKeyAndValue[1]
			} else if labelKeyAndValue[0] == config.LabelCloudAccountNamespace {
				accountNamespace = labelKeyAndValue[1]
			} else {
				return nil, errors.NewBadRequest("unsupported label selector, supported labels are: cpa.name and cpa.namespace")
			}
		}
	}

	// Check if both cpa.name and cpa.namespace are specified.
	if (accountName != "" && accountNamespace == "") || (accountName == "" && accountNamespace != "") {
		return nil, errors.NewBadRequest("unsupported query, both cpa.name and cpa.namespace labels should be specified")
	}

	namespace, _ := request.NamespaceFrom(ctx)
	// Check if account namespace and namespace are same.
	if namespace != "" && accountNamespace != "" && accountNamespace != namespace {
		return nil, errors.NewBadRequest("namespace in label selector is different from namespace specified")
	}

	var objs []interface{}
	if namespace == "" {
		objs = r.cloudInventory.GetAllVms()
	} else if accountName != "" && accountNamespace != "" {
		accountNameSpacedName := types.NamespacedName{
			Name:      accountName,
			Namespace: accountNamespace,
		}
		objs, _ = r.cloudInventory.GetVmFromIndexer(common.VirtualMachineIndexerByNameSpacedAccountName, accountNameSpacedName.String())
	} else {
		objs, _ = r.cloudInventory.GetVmFromIndexer(common.IndexerByNamespace, namespace)
	}

	vmList := &runtimev1alpha1.VirtualMachineList{}
	for _, obj := range objs {
		vm := obj.(*runtimev1alpha1.VirtualMachine)
		vmList.Items = append(vmList.Items, *vm)
	}
	return vmList, nil
}

func (r *REST) NamespaceScoped() bool {
	return true
}

func (r *REST) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "NAME", Type: "string", Description: "Name"},
			{Name: "CLOUD-PROVIDER", Type: "string", Description: "Cloud Provider"},
			{Name: "REGION", Type: "string", Description: "Region"},
			{Name: "VIRTUAL-PRIVATE-CLOUD", Type: "string", Description: "VPC/VNET"},
			{Name: "STATE", Type: "string", Description: "Running state"},
			{Name: "AGENTED", Type: "bool", Description: "Agent installed"},
		},
	}

	if m, err := meta.ListAccessor(obj); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
		table.Continue = m.GetContinue()
		table.RemainingItemCount = m.GetRemainingItemCount()
	} else {
		if m, err := meta.CommonAccessor(obj); err == nil {
			table.ResourceVersion = m.GetResourceVersion()
		}
	}

	var err error
	table.Rows, err = metatable.MetaToTableRow(obj,
		func(obj runtime.Object, _ metav1.Object, _, _ string) ([]interface{}, error) {
			vm := obj.(*runtimev1alpha1.VirtualMachine)
			if vm.Name == "" {
				return nil, nil
			}
			return []interface{}{vm.Name, vm.Status.Provider, vm.Status.Region,
				vm.Labels[config.LabelCloudVPCName], vm.Status.State, vm.Status.Agented}, nil
		})
	return table, err
}

func (r *REST) Watch(ctx context.Context, options *internalversion.ListOptions) (watch.Interface, error) {
	key, label, field := store.GetSelectors(options)
	return r.cloudInventory.WatchVms(ctx, key, label, field)
}
