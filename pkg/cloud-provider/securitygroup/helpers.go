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

package securitygroup

import (
	"fmt"
	"strings"
)

// IsNepheControllerCreatedSG checks an SG is created by nephe
// and returns if it's an AppliedToGroup/AddressGroup sg and the sg name.
func IsNepheControllerCreatedSG(cloudSgName string) (string, bool, bool) {
	var sgName string
	isNepheControllerCreatedAddressGroup := false
	isNepheControllerCreatedAppliedToGroup := false

	suffix := strings.TrimPrefix(cloudSgName, GetControllerAddressGroupPrefix())
	if len(suffix) < len(cloudSgName) {
		isNepheControllerCreatedAddressGroup = true
		sgName = strings.ToLower(suffix)
	}

	if !isNepheControllerCreatedAddressGroup {
		suffix := strings.TrimPrefix(cloudSgName, GetControllerAppliedToPrefix())
		if len(suffix) < len(cloudSgName) {
			isNepheControllerCreatedAppliedToGroup = true
			sgName = strings.ToLower(suffix)
		}
	}
	return sgName, isNepheControllerCreatedAddressGroup, isNepheControllerCreatedAppliedToGroup
}

func FindResourcesBasedOnKind(cloudResources []*CloudResource) (map[string]struct{}, map[string]struct{}) {
	virtualMachineIDs := make(map[string]struct{})
	networkInterfaceIDs := make(map[string]struct{})

	for _, cloudResource := range cloudResources {
		if strings.Compare(string(cloudResource.Type), string(CloudResourceTypeVM)) == 0 {
			virtualMachineIDs[strings.ToLower(cloudResource.Name)] = struct{}{}
		}
		if strings.Compare(string(cloudResource.Type), string(CloudResourceTypeNIC)) == 0 {
			networkInterfaceIDs[strings.ToLower(cloudResource.Name)] = struct{}{}
		}
	}
	return virtualMachineIDs, networkInterfaceIDs
}

// GenerateCloudDescription generates a CloudRuleDescription object and converts to string.
func GenerateCloudDescription(namespacedName string, appliedToGroup string) (string, error) {
	tokens := strings.Split(namespacedName, "/")
	if len(tokens) != 2 {
		return "", fmt.Errorf("invalid namespacedname %v", namespacedName)
	}
	desc := CloudRuleDescription{
		Name:           tokens[1],
		Namespace:      tokens[0],
		AppliedToGroup: appliedToGroup,
	}
	return desc.String(), nil
}

// ExtractCloudDescription converts a string to a CloudRuleDescription object.
func ExtractCloudDescription(description *string) (*CloudRuleDescription, bool) {
	if description == nil {
		return nil, false
	}
	numKeyValuePair := 3
	descMap := map[string]string{}
	tempSlice := strings.Split(*description, ",")
	if len(tempSlice) != numKeyValuePair {
		return nil, false
	}
	// each key and value are separated by ":"
	for i := range tempSlice {
		keyValuePair := strings.Split(strings.TrimSpace(tempSlice[i]), ":")
		if len(keyValuePair) == 2 {
			descMap[keyValuePair[0]] = keyValuePair[1]
		}
	}

	// check if any of the fields are empty.
	if descMap[Name] == "" || descMap[Namespace] == "" || descMap[AppliedToGroup] == "" {
		return nil, false
	}

	desc := &CloudRuleDescription{
		Name:           descMap[Name],
		Namespace:      descMap[Namespace],
		AppliedToGroup: descMap[AppliedToGroup],
	}
	return desc, true
}
