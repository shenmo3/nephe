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

package aws

import (
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/apimachinery/pkg/types"

	runtimev1alpha1 "antrea.io/nephe/apis/runtime/v1alpha1"
	"antrea.io/nephe/pkg/cloud-provider/utils"
)

const ResourceNameTagKey = "Name"

// ec2InstanceToInternalVirtualMachineObject converts ec2 instance to VirtualMachine runtime object.
func ec2InstanceToInternalVirtualMachineObject(instance *ec2.Instance, namespace string, account *types.NamespacedName,
	region string) *runtimev1alpha1.VirtualMachine {
	tags := make(map[string]string)
	vmTags := instance.Tags
	if len(vmTags) > 0 {
		for _, tag := range vmTags {
			tags[*tag.Key] = *tag.Value
		}
	}

	// Network interfaces associated with Virtual machine
	instNetworkInterfaces := instance.NetworkInterfaces
	networkInterfaces := make([]runtimev1alpha1.NetworkInterface, 0, len(instNetworkInterfaces))

	for _, nwInf := range instNetworkInterfaces {
		var ipAddressCRDs []runtimev1alpha1.IPAddress
		privateIPAddresses := nwInf.PrivateIpAddresses
		if len(privateIPAddresses) > 0 {
			for _, ipAddress := range privateIPAddresses {
				ipAddressCRD := runtimev1alpha1.IPAddress{
					AddressType: runtimev1alpha1.AddressTypeInternalIP,
					Address:     *ipAddress.PrivateIpAddress,
				}
				ipAddressCRDs = append(ipAddressCRDs, ipAddressCRD)

				association := ipAddress.Association
				if association != nil {
					ipAddressCRD := runtimev1alpha1.IPAddress{
						AddressType: runtimev1alpha1.AddressTypeExternalIP,
						Address:     *association.PublicIp,
					}
					ipAddressCRDs = append(ipAddressCRDs, ipAddressCRD)
				}
			}
		}
		networkInterface := runtimev1alpha1.NetworkInterface{
			Name: *nwInf.NetworkInterfaceId,
			MAC:  *nwInf.MacAddress,
			IPs:  ipAddressCRDs,
		}
		networkInterfaces = append(networkInterfaces, networkInterface)
	}

	cloudName := tags[ResourceNameTagKey]
	cloudID := *instance.InstanceId
	cloudNetwork := *instance.VpcId

	return utils.GenerateInternalVirtualMachineObject(cloudID, strings.ToLower(cloudName), strings.ToLower(cloudID), strings.ToLower(region),
		namespace, strings.ToLower(cloudNetwork), cloudNetwork, runtimev1alpha1.VMState(*instance.State.Name), tags, networkInterfaces,
		providerType, account)
}

// ec2VpcToInternalVpcObject converts ec2 vpc object to vpc runtime object.
func ec2VpcToInternalVpcObject(vpc *ec2.Vpc, accountNamespace, accountName, region string, managed bool) *runtimev1alpha1.Vpc {
	cloudName := ""
	tags := make(map[string]string, 0)
	if len(vpc.Tags) != 0 {
		for _, tag := range vpc.Tags {
			tags[*(tag.Key)] = *(tag.Value)
		}
		if value, found := tags[ResourceNameTagKey]; found {
			cloudName = value
		}
	}
	cidrs := make([]string, 0)
	if len(vpc.CidrBlockAssociationSet) != 0 {
		for _, cidr := range vpc.CidrBlockAssociationSet {
			cidrs = append(cidrs, *cidr.CidrBlock)
		}
	}

	return utils.GenerateInternalVpcObject(*vpc.VpcId, accountNamespace, accountName, strings.ToLower(cloudName),
		strings.ToLower(*vpc.VpcId), tags, runtimev1alpha1.AWSCloudProvider, region, cidrs, managed)
}
