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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/cenkalti/backoff/v4"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/types"

	runtimev1alpha1 "antrea.io/nephe/apis/runtime/v1alpha1"
	"antrea.io/nephe/pkg/cloud-provider/cloudapi/common"
	"antrea.io/nephe/pkg/cloud-provider/securitygroup"
)

const (
	awsVpcDefaultSecurityGroupName = "default"
)

var (
	mutex sync.Mutex

	awsAnyProtocolValue = "-1"
	tcpUDPPortStart     = 0
	tcpUDPPortEnd       = 65535
)

var vpcIDToDefaultSecurityGroup = make(map[string]string)

func buildEc2UserIDGroupPairs(addressGroupIdentifiers []*securitygroup.CloudResourceID,
	cloudSGNameToObj map[string]*ec2.SecurityGroup, description *string) []*ec2.UserIdGroupPair {
	var userIDGroupPairs []*ec2.UserIdGroupPair
	for _, addressGroupIdentifier := range addressGroupIdentifiers {
		group := cloudSGNameToObj[addressGroupIdentifier.GetCloudName(true)]
		userIDGroupPair := &ec2.UserIdGroupPair{
			GroupId:     group.GroupId,
			Description: description,
		}
		userIDGroupPairs = append(userIDGroupPairs, userIDGroupPair)
	}
	return userIDGroupPairs
}

// buildEc2CloudSgNamesFromRules builds all needed ec2 security group names from address groups in rules and target appliedTo group.
func buildEc2CloudSgNamesFromRules(appliedToGroupIdentifier *securitygroup.CloudResourceID, ingressRules,
	egressRules []*securitygroup.CloudRule) map[string]struct{} {
	cloudSgNames := make(map[string]struct{})

	for _, obj := range ingressRules {
		rule := obj.Rule.(*securitygroup.IngressRule)
		addressGroupIdentifiers := rule.FromSecurityGroups
		for _, addressGroupIdentifier := range addressGroupIdentifiers {
			cloudSgNames[addressGroupIdentifier.GetCloudName(true)] = struct{}{}
		}
	}

	for _, obj := range egressRules {
		rule := obj.Rule.(*securitygroup.EgressRule)
		addressGroupIdentifiers := rule.ToSecurityGroups
		for _, addressGroupIdentifier := range addressGroupIdentifiers {
			cloudSgNames[addressGroupIdentifier.GetCloudName(true)] = struct{}{}
		}
	}
	cloudSgNames[appliedToGroupIdentifier.GetCloudName(false)] = struct{}{}

	return cloudSgNames
}

func (ec2Cfg *ec2ServiceConfig) createOrGetSecurityGroups(vpcID string, cloudSgNames map[string]struct{}) (
	map[string]*ec2.SecurityGroup, error) {
	// for cloudSgs get details from clouds, if they already exist in cloud.
	vpcIDs := []string{vpcID}
	cloudSgNameToCloudSGObj, err := ec2Cfg.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, cloudSgNames)
	if err != nil {
		return nil, err
	}

	// find the ones which do not exist in cloud and create those
	cloudSgNamesToCreate := make(map[string]struct{})
	for cloudSgName := range cloudSgNames {
		if _, found := cloudSgNameToCloudSGObj[cloudSgName]; !found {
			cloudSgNamesToCreate[cloudSgName] = struct{}{}
		}
	}

	// return the up-to-date cloud objects for SGs
	if len(cloudSgNamesToCreate) == 0 {
		awsPluginLogger().Info("No new security group to be created")
		return cloudSgNameToCloudSGObj, nil
	}

	for cloudSgName := range cloudSgNamesToCreate {
		err := ec2Cfg.createCloudSecurityGroup(cloudSgName, vpcID)
		if err != nil {
			awsPluginLogger().Info("Failed to create the security group", "Error", err, "vpcID", vpcID)
			return nil, err
		}
	}

	return ec2Cfg.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, cloudSgNames)
}

func (ec2Cfg *ec2ServiceConfig) createCloudSecurityGroup(cloudSGName string, vpcID string) error {
	groupInput := &ec2.CreateSecurityGroupInput{
		Description: aws.String("Managed by nephe controller"),
		GroupName:   aws.String(cloudSGName),
		VpcId:       aws.String(vpcID),
	}
	response, err := ec2Cfg.apiClient.createSecurityGroup(groupInput)
	if err != nil {
		return err
	}
	awsPluginLogger().Info("Security group created, wait for two more seconds", "Result", response, "vpcID", vpcID)

	// with previous call to create success, expectation is subsequent get will return created SG. but in rare cases get
	// is not returning the details of created SG. Hence, wait with exponential backoff for max 2 second before declaring error.
	// On error delete created SG
	out, err := ec2Cfg.waitForCloudSecurityGroupCreation(cloudSGName, vpcID, 2*time.Second)
	if err != nil {
		awsPluginLogger().Info("Error declared, security group will be deleted")
		input := &ec2.DeleteSecurityGroupInput{
			GroupId: response.GroupId,
		}
		_, _ = ec2Cfg.apiClient.deleteSecurityGroup(input)
		return err
	}

	// clear default egress rules from newly created cloud security group
	cloudSGObj := out[cloudSGName]
	revokeEgressInput := &ec2.RevokeSecurityGroupEgressInput{
		GroupId:       response.GroupId,
		IpPermissions: cloudSGObj.IpPermissionsEgress,
	}
	_, err = ec2Cfg.apiClient.revokeSecurityGroupEgress(revokeEgressInput)

	return err
}

func (ec2Cfg *ec2ServiceConfig) waitForCloudSecurityGroupCreation(cloudSGName string, vpcID string, duration time.Duration) (
	map[string]*ec2.SecurityGroup, error) {
	var out map[string]*ec2.SecurityGroup
	vpcIDs := []string{vpcID}
	operation := func() error {
		var err error
		out, err = ec2Cfg.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, map[string]struct{}{cloudSGName: {}})
		if err != nil {
			return err
		}
		if len(out) == 0 {
			return fmt.Errorf("failed to find created cloud-security-group name %v", cloudSGName)
		}
		return nil
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = duration
	err := backoff.Retry(operation, b)
	if err != nil {
		return nil, err
	}
	return out, err
}

func (ec2Cfg *ec2ServiceConfig) getCloudSecurityGroupsWithNameFromCloud(vpcIDs []string, cloudSGNamesSet map[string]struct{}) (
	map[string]*ec2.SecurityGroup, error) {
	filters := buildAwsEc2FilterForSecurityGroupNameMatches(vpcIDs, cloudSGNamesSet)
	input := &ec2.DescribeSecurityGroupsInput{
		Filters: filters,
	}
	output, err := ec2Cfg.apiClient.describeSecurityGroups(input)
	if err != nil {
		awsPluginLogger().Info("Fail to get security group with the name", "vpcIDs", vpcIDs, "filter", input)
		return nil, err
	}

	addressGroupNameToCloudSGObj := make(map[string]*ec2.SecurityGroup)
	cloudSecurityGroups := output.SecurityGroups
	for _, cloudSGObj := range cloudSecurityGroups {
		addressGroupNameToCloudSGObj[strings.ToLower(*cloudSGObj.GroupName)] = cloudSGObj
	}
	return addressGroupNameToCloudSGObj, nil
}

// realizeIngressIPPermissions invokes cloud api and realizes ingress rules on the cloud security group.
func (ec2Cfg *ec2ServiceConfig) realizeIngressIPPermissions(cloudSgObj *ec2.SecurityGroup, rules []*securitygroup.CloudRule,
	cloudSGNameToObj map[string]*ec2.SecurityGroup, isDelete bool) error {
	newIpPermissions := make([]*ec2.IpPermission, 0)
	for _, obj := range rules {
		rule := obj.Rule.(*securitygroup.IngressRule)
		if rule == nil {
			continue
		}
		description, err := securitygroup.GenerateCloudDescription(obj.NetworkPolicy, *cloudSgObj.GroupName)
		if err != nil {
			return fmt.Errorf("unable to generate rule description, err: %v", err)
		}
		idGroupPairs := buildEc2UserIDGroupPairs(rule.FromSecurityGroups, cloudSGNameToObj, &description)
		ipRanges := convertToEc2IpRanges(rule.FromSrcIP, len(rule.FromSecurityGroups) > 0, &description)
		startPort, endPort := convertToIPPermissionPort(rule.FromPort, rule.Protocol)
		ipPermission := &ec2.IpPermission{
			FromPort:         startPort,
			ToPort:           endPort,
			IpProtocol:       convertToIPPermissionProtocol(rule.Protocol),
			IpRanges:         ipRanges,
			UserIdGroupPairs: idGroupPairs,
		}
		newIpPermissions = append(newIpPermissions, ipPermission)
	}

	if len(newIpPermissions) == 0 {
		return nil
	}

	if isDelete {
		awsPluginLogger().V(1).Info("delete ingress rules", "rules", newIpPermissions)
		request := &ec2.RevokeSecurityGroupIngressInput{
			GroupId:       cloudSgObj.GroupId,
			IpPermissions: newIpPermissions,
		}
		_, err := ec2Cfg.apiClient.revokeSecurityGroupIngress(request)
		return err
	} else {
		awsPluginLogger().V(1).Info("add ingress rules", "rules", newIpPermissions)
		request := &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       cloudSgObj.GroupId,
			IpPermissions: newIpPermissions,
		}
		_, err := ec2Cfg.apiClient.authorizeSecurityGroupIngress(request)
		return err
	}
}

// realizeEgressIPPermissions invokes cloud api and realizes egress rules on the cloud security group.
func (ec2Cfg *ec2ServiceConfig) realizeEgressIPPermissions(cloudSgObj *ec2.SecurityGroup, rules []*securitygroup.CloudRule,
	cloudSGNameToObj map[string]*ec2.SecurityGroup, isDelete bool) error {
	newIpPermissions := make([]*ec2.IpPermission, 0)
	for _, obj := range rules {
		rule := obj.Rule.(*securitygroup.EgressRule)
		if rule == nil {
			continue
		}
		description, err := securitygroup.GenerateCloudDescription(obj.NetworkPolicy, *cloudSgObj.GroupName)
		if err != nil {
			return fmt.Errorf("unable to generate rule description, err: %v", err)
		}

		idGroupPairs := buildEc2UserIDGroupPairs(rule.ToSecurityGroups, cloudSGNameToObj, &description)
		ipRanges := convertToEc2IpRanges(rule.ToDstIP, len(rule.ToSecurityGroups) > 0, &description)
		startPort, endPort := convertToIPPermissionPort(rule.ToPort, rule.Protocol)
		ipPermission := &ec2.IpPermission{
			FromPort:         startPort,
			ToPort:           endPort,
			IpProtocol:       convertToIPPermissionProtocol(rule.Protocol),
			IpRanges:         ipRanges,
			UserIdGroupPairs: idGroupPairs,
		}
		newIpPermissions = append(newIpPermissions, ipPermission)
	}

	if len(newIpPermissions) == 0 {
		return nil
	}

	if isDelete {
		awsPluginLogger().V(1).Info("delete egress rules", "rule", newIpPermissions)
		request := &ec2.RevokeSecurityGroupEgressInput{
			GroupId:       cloudSgObj.GroupId,
			IpPermissions: newIpPermissions,
		}
		_, err := ec2Cfg.apiClient.revokeSecurityGroupEgress(request)
		return err
	} else {
		awsPluginLogger().V(1).Info("add egress rules", "rule", newIpPermissions)
		request := &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId:       cloudSgObj.GroupId,
			IpPermissions: newIpPermissions,
		}
		_, err := ec2Cfg.apiClient.authorizeSecurityGroupEgress(request)
		return err
	}
}

func (ec2Cfg *ec2ServiceConfig) getVpcDefaultSecurityGroupID(vpcID string) (string, error) {
	sgID, found := vpcIDToDefaultSecurityGroup[vpcID]
	if found {
		return sgID, nil
	}
	vpcIDs := []string{vpcID}
	out, err := ec2Cfg.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, map[string]struct{}{awsVpcDefaultSecurityGroupName: {}})
	if err != nil {
		return "", err
	}
	defaultSGObj := out[awsVpcDefaultSecurityGroupName]
	defaultCloudSGID := defaultSGObj.GroupId
	vpcIDToDefaultSecurityGroup[vpcID] = *defaultCloudSGID

	return *defaultCloudSGID, nil
}

func (ec2Cfg *ec2ServiceConfig) updateNetworkInterfaceSecurityGroups(interfaceID string, vpcID string,
	sgIDSet map[string]struct{}) error {
	var sgIDs []*string
	if len(sgIDSet) == 0 {
		defaultSGId, err := ec2Cfg.getVpcDefaultSecurityGroupID(vpcID)
		if err != nil {
			return err
		}
		sgIDs = append(sgIDs, &defaultSGId)
	} else {
		for sgID := range sgIDSet {
			sgIDCopy := sgID
			sgIDs = append(sgIDs, &sgIDCopy)
		}
	}

	input := &ec2.ModifyNetworkInterfaceAttributeInput{
		Groups:             sgIDs,
		NetworkInterfaceId: aws.String(interfaceID),
	}
	_, err := ec2Cfg.apiClient.modifyNetworkInterfaceAttribute(input)

	return err
}

func (ec2Cfg *ec2ServiceConfig) getNetworkInterfacesOfVpc(vpcIDs map[string]struct{}) ([]*ec2.NetworkInterface, error) {
	filters := buildAwsEc2FilterForVpcIDOnlyMatches(vpcIDs)
	request := &ec2.DescribeNetworkInterfacesInput{
		MaxResults: aws.Int64(common.MaxCloudResourceResponse),
		Filters:    filters,
	}
	networkInterfaces, err := ec2Cfg.apiClient.pagedDescribeNetworkInterfaces(request)
	if err != nil {
		return nil, err
	}
	return networkInterfaces, nil
}

func (ec2Cfg *ec2ServiceConfig) getSecurityGroupsOfVpc(vpcIDs map[string]struct{}) ([]*ec2.SecurityGroup, error) {
	filters := buildAwsEc2FilterForVpcIDOnlyMatches(vpcIDs)
	input := &ec2.DescribeSecurityGroupsInput{
		Filters: filters,
	}
	output, err := ec2Cfg.apiClient.describeSecurityGroups(input)
	if err != nil {
		return nil, err
	}

	return output.SecurityGroups, nil
}

func (ec2Cfg *ec2ServiceConfig) updateSecurityGroupMembers(groupCloudSgID *string, groupCloudSgName string, vpcID string,
	cloudResourceIdentifiers []*securitygroup.CloudResource, membershipOnly bool) error {
	// find all network interfaces using this security group within VPC
	networkInterfaces, err := ec2Cfg.getNetworkInterfacesOfVpc(map[string]struct{}{vpcID: {}})
	if err != nil {
		return err
	}

	// get default sg ID for the VPC
	vpcDefaultSgID, err := ec2Cfg.getVpcDefaultSecurityGroupID(vpcID)
	if err != nil {
		return err
	}

	// find all network interfaces which needs to be attached to SG
	memberVirtualMachines, memberNetworkInterfaces := securitygroup.FindResourcesBasedOnKind(cloudResourceIdentifiers)

	// find network interfaces which are using or need to use the provided SG
	networkInterfacesToModify := make(map[string]map[string]struct{})
	for _, networkInterface := range networkInterfaces {
		// for network interfaces not attached to any virtual machines, skip processing
		attachment := networkInterface.Attachment
		if attachment == nil || attachment.InstanceId == nil {
			continue
		}

		isGroupSgAttached := false
		numAppliedToGroupSgsAttached := 0
		networkInterfaceNepheControllerCreatedCloudSgsSet := make(map[string]struct{})
		networkInterfaceOtherCloudSgsSet := make(map[string]struct{})
		networkInterfaceCloudSgs := networkInterface.Groups
		for _, group := range networkInterfaceCloudSgs {
			cloudSgName := strings.ToLower(*group.GroupName)
			_, isNepheControllerCreatedAddrGroup, isNepheControllerCreatedAppliedToGroup :=
				securitygroup.IsNepheControllerCreatedSG(cloudSgName)
			if !isNepheControllerCreatedAppliedToGroup && !isNepheControllerCreatedAddrGroup {
				networkInterfaceOtherCloudSgsSet[*group.GroupId] = struct{}{}
				continue
			}

			if isNepheControllerCreatedAppliedToGroup {
				numAppliedToGroupSgsAttached++
			}

			if strings.Compare(cloudSgName, groupCloudSgName) == 0 {
				isGroupSgAttached = true
			}
			networkInterfaceNepheControllerCreatedCloudSgsSet[*group.GroupId] = struct{}{}
		}

		// if network interface is owned by any of member virtual machines or member interface, its sg needs update
		_, isNicAttachedToMemberVM := memberVirtualMachines[*attachment.InstanceId]
		_, isNicMemberNetworkInterface := memberNetworkInterfaces[*networkInterface.NetworkInterfaceId]
		if isGroupSgAttached {
			if !isNicAttachedToMemberVM && !isNicMemberNetworkInterface {
				delete(networkInterfaceNepheControllerCreatedCloudSgsSet, *groupCloudSgID)

				networkInterfaceCloudSgsSetToAttach := networkInterfaceNepheControllerCreatedCloudSgsSet

				// If network interface has only one AT sg attached, and we are processing AT sg to be removed, network interface
				// will be attached to default sg along with any attached AG sg(s)
				if !membershipOnly && numAppliedToGroupSgsAttached == 1 {
					networkInterfaceCloudSgsSetToAttach[vpcDefaultSgID] = struct{}{}
				}
				// if network interface is not attached to AT sg, and we're processing detach from AG sg, keep all sgs. Also, if member-only
				// address group will be the only sg attached to network interface, attach default sg along with AG security group.
				if membershipOnly && numAppliedToGroupSgsAttached == 0 {
					networkInterfaceCloudSgsSetToAttach = buildEc2SgsToAttachForCaseMemberOnlySgWithNoATSgAttached(
						networkInterfaceNepheControllerCreatedCloudSgsSet, networkInterfaceOtherCloudSgsSet, vpcDefaultSgID)
				}

				networkInterfacesToModify[*networkInterface.NetworkInterfaceId] = networkInterfaceCloudSgsSetToAttach
			}
		} else {
			if isNicAttachedToMemberVM || isNicMemberNetworkInterface {
				networkInterfaceNepheControllerCreatedCloudSgsSet[*groupCloudSgID] = struct{}{}

				networkInterfaceCloudSgsSetToAttach := networkInterfaceNepheControllerCreatedCloudSgsSet

				// if network interface is not attached to AT sg, and we're processing attach of AG sg, keep all existing sgs. Also,
				// if AG sg will be the only sg attached to network interface, attach default sg along with AG sg.
				if membershipOnly && numAppliedToGroupSgsAttached == 0 {
					networkInterfaceCloudSgsSetToAttach = buildEc2SgsToAttachForCaseMemberOnlySgWithNoATSgAttached(
						networkInterfaceNepheControllerCreatedCloudSgsSet, networkInterfaceOtherCloudSgsSet, vpcDefaultSgID)
				}

				networkInterfacesToModify[*networkInterface.NetworkInterfaceId] = networkInterfaceCloudSgsSetToAttach
			}
		}
	}

	// update network interface security groups
	return ec2Cfg.processNetworkInterfaceModifyConcurrently(networkInterfacesToModify, vpcID)
}

func (ec2Cfg *ec2ServiceConfig) processNetworkInterfaceModifyConcurrently(networkInterfacesToModify map[string]map[string]struct{},
	vpcID string) error {
	ch := make(chan error)
	var err error
	var wg sync.WaitGroup

	wg.Add(len(networkInterfacesToModify))
	go func() {
		wg.Wait()
		close(ch)
	}()

	for networkInterfaceID, cloudSgIDSet := range networkInterfacesToModify {
		go func(interfaceID string, sgIDSet map[string]struct{}, ch chan error) {
			defer wg.Done()
			ch <- ec2Cfg.updateNetworkInterfaceSecurityGroups(interfaceID, vpcID, sgIDSet)
		}(networkInterfaceID, cloudSgIDSet, ch)
	}
	for e := range ch {
		if e != nil {
			err = multierr.Append(err, e)
		}
	}

	return err
}

func buildEc2SgsToAttachForCaseMemberOnlySgWithNoATSgAttached(networkInterfaceNepheControllerCreatedCloudSgsSet map[string]struct{},
	networkInterfaceOtherCloudSgsSet map[string]struct{}, vpcDefaultSgID string) map[string]struct{} {
	networkInterfaceCloudSgsSet := make(map[string]struct{})
	// add all nephe created sgs
	for key, value := range networkInterfaceNepheControllerCreatedCloudSgsSet {
		networkInterfaceCloudSgsSet[key] = value
	}
	// add all others sgs
	for key, value := range networkInterfaceOtherCloudSgsSet {
		networkInterfaceCloudSgsSet[key] = value
	}
	// add vpc default sg id, if network interface is going to have all member-only sgs.
	// so the first time, when nic is getting attached to member-only nephe sg(s), it will not be
	// moved out of its existing non-nephe created sg. And if there are no non-antrea created sg(s)
	// attached then vpc default sg will also be attached to it
	if len(networkInterfaceOtherCloudSgsSet) == 0 {
		networkInterfaceCloudSgsSet[vpcDefaultSgID] = struct{}{}
	}

	return networkInterfaceCloudSgsSet
}

func (ec2Cfg *ec2ServiceConfig) getNepheControllerManagedSecurityGroupsCloudView() []securitygroup.SynchronizationContent {
	vpcIDs := ec2Cfg.getManagedVpcIDs()
	if len(vpcIDs) == 0 {
		return []securitygroup.SynchronizationContent{}
	}

	// get all network interfaces for managed vpcs
	networkInterfaces, err := ec2Cfg.getNetworkInterfacesOfVpc(vpcIDs)
	if err != nil {
		awsPluginLogger().Error(err, "failed to get network interfaces of vpcs", "vpc-ids", vpcIDs)
		return []securitygroup.SynchronizationContent{}
	}

	// get all security groups for managed vpcs and build cloud-sg-id to sgObj map by sg managed/unmanaged type
	cloudSecurityGroups, err := ec2Cfg.getSecurityGroupsOfVpc(vpcIDs)
	if err != nil {
		awsPluginLogger().Error(err, "failed to get security groups of vpcs", "vpc-ids", vpcIDs)
		return []securitygroup.SynchronizationContent{}
	}
	managedSgIDToCloudSGObj, unmanagedSgIDToCloudSGObj := getCloudSecurityGroupsByType(cloudSecurityGroups)

	// find all member network-interfaces-ids for managed cloud-security-groups.
	// also find all member network-interface-ids attached to non nephe created sgs.
	managedSgIDToMemberCloudResourcesMap := make(map[string][]securitygroup.CloudResource)
	memberCloudResourcesWithOtherSGsAttachedMap := make(map[string]struct{})
	for _, networkInterface := range networkInterfaces {
		isAttachedToOtherSG := false
		isAttachedToNepheControllerSG := false

		networkInterfaceID := *networkInterface.NetworkInterfaceId
		networkInterfaceCloudSgs := networkInterface.Groups
		for _, group := range networkInterfaceCloudSgs {
			sgID := *group.GroupId
			_, isManagedSg := managedSgIDToCloudSGObj[*group.GroupId]
			if !isManagedSg {
				isAttachedToOtherSG = true
				continue
			}
			cloudResource := securitygroup.CloudResource{
				Type: securitygroup.CloudResourceTypeNIC,
				CloudResourceID: securitygroup.CloudResourceID{
					Name: networkInterfaceID,
					Vpc:  *networkInterface.VpcId,
				},
				AccountID:     ec2Cfg.accountNamespacedName.String(),
				CloudProvider: string(runtimev1alpha1.AWSCloudProvider),
			}
			cloudResources := managedSgIDToMemberCloudResourcesMap[sgID]
			cloudResources = append(cloudResources, cloudResource)
			managedSgIDToMemberCloudResourcesMap[sgID] = cloudResources
			isAttachedToNepheControllerSG = true
		}

		if isAttachedToNepheControllerSG && isAttachedToOtherSG {
			memberCloudResourcesWithOtherSGsAttachedMap[networkInterfaceID] = struct{}{}
		}
	}

	// build sync objects for managed security groups
	var enforcedSecurityCloudView []securitygroup.SynchronizationContent
	for sgID, cloudSgObj := range managedSgIDToCloudSGObj {
		cloudSgName := *cloudSgObj.GroupName
		vpcID := *cloudSgObj.VpcId

		// find AT or AG
		isMembershipOnly := false
		SgName, isAG, _ := securitygroup.IsNepheControllerCreatedSG(cloudSgName)
		if isAG {
			isMembershipOnly = true
		}

		// find members and membersAttachedToOtherSGs
		var membersWithOtherSGAttached []securitygroup.CloudResource
		members, found := managedSgIDToMemberCloudResourcesMap[sgID]
		if found {
			membersWithOtherSGAttached = getMemberNicCloudResourcesAttachedToOtherSGs(members, memberCloudResourcesWithOtherSGsAttachedMap)
		}

		// build ingress and egress rules
		inRules := convertFromIPPermissionToIngressRule(cloudSgObj.IpPermissions, managedSgIDToCloudSGObj, unmanagedSgIDToCloudSGObj)
		egRules := convertFromIPPermissionToEgressRule(cloudSgObj.IpPermissionsEgress, managedSgIDToCloudSGObj, unmanagedSgIDToCloudSGObj)

		// build sync object
		groupSyncObj := securitygroup.SynchronizationContent{
			Resource: securitygroup.CloudResource{
				Type: securitygroup.CloudResourceTypeVM,
				CloudResourceID: securitygroup.CloudResourceID{
					Name: SgName,
					Vpc:  vpcID,
				},
				AccountID:     ec2Cfg.accountNamespacedName.String(),
				CloudProvider: string(runtimev1alpha1.AWSCloudProvider),
			},
			MembershipOnly:             isMembershipOnly,
			Members:                    members,
			MembersWithOtherSGAttached: membersWithOtherSGAttached,
			IngressRules:               inRules,
			EgressRules:                egRules,
		}

		enforcedSecurityCloudView = append(enforcedSecurityCloudView, groupSyncObj)
	}

	return enforcedSecurityCloudView
}

func getCloudSecurityGroupsByType(cloudSecurityGroups []*ec2.SecurityGroup) (map[string]*ec2.SecurityGroup, map[string]*ec2.SecurityGroup) {
	managedSgIDToCloudSecurityGroupObj := make(map[string]*ec2.SecurityGroup)
	unmanagedSgIDToCloudSecurityGroupObj := make(map[string]*ec2.SecurityGroup)
	for _, cloudSecurityGroup := range cloudSecurityGroups {
		sgID := *cloudSecurityGroup.GroupId
		cloudSgName := *cloudSecurityGroup.GroupName

		_, isAG, isAT := securitygroup.IsNepheControllerCreatedSG(cloudSgName)
		if isAG || isAT {
			managedSgIDToCloudSecurityGroupObj[sgID] = cloudSecurityGroup
		} else {
			unmanagedSgIDToCloudSecurityGroupObj[sgID] = cloudSecurityGroup
		}
	}

	return managedSgIDToCloudSecurityGroupObj, unmanagedSgIDToCloudSecurityGroupObj
}

func getMemberNicCloudResourcesAttachedToOtherSGs(members []securitygroup.CloudResource,
	memberNicsAttachedToOtherSGs map[string]struct{}) []securitygroup.CloudResource {
	var nicCloudResources []securitygroup.CloudResource

	for _, member := range members {
		memberID := member.Name
		_, found := memberNicsAttachedToOtherSGs[member.Name]
		if !found {
			continue
		}
		cloudResource := securitygroup.CloudResource{
			Type: securitygroup.CloudResourceTypeNIC,
			CloudResourceID: securitygroup.CloudResourceID{
				Name: memberID,
				Vpc:  member.Vpc,
			},
			AccountID:     member.AccountID,
			CloudProvider: member.CloudProvider,
		}
		nicCloudResources = append(nicCloudResources, cloudResource)
	}
	return nicCloudResources
}

// ////////////////////////////////////////////////////////
//
//	SecurityInterface Implementation
//
// ////////////////////////////////////////////////////////.

// CreateSecurityGroup invokes cloud api and creates the cloud security group based on securityGroupIdentifier.
func (c *awsCloud) CreateSecurityGroup(securityGroupIdentifier *securitygroup.CloudResource, membershipOnly bool) (*string, error) {
	mutex.Lock()
	defer mutex.Unlock()

	vpcID := securityGroupIdentifier.Vpc
	accCfg, found := c.cloudCommon.GetCloudAccountByAccountId(&securityGroupIdentifier.AccountID)
	if !found {
		return nil, fmt.Errorf("aws account not found managing virtual private cloud [%v]", vpcID)
	}
	serviceCfg, err := accCfg.GetServiceConfigByName(awsComputeServiceNameEC2)
	if err != nil {
		return nil, err
	}
	ec2Service := serviceCfg.(*ec2ServiceConfig)

	cloudSgName := securityGroupIdentifier.GetCloudName(membershipOnly)
	resp, err := ec2Service.createOrGetSecurityGroups(securityGroupIdentifier.Vpc, map[string]struct{}{cloudSgName: {}})
	if err != nil {
		return nil, err
	}
	securityGroupObj := resp[cloudSgName]

	return securityGroupObj.GroupId, nil
}

// UpdateSecurityGroupRules invokes cloud api and updates cloud security group with addRules and rmRules.
func (c *awsCloud) UpdateSecurityGroupRules(appliedToGroupIdentifier *securitygroup.CloudResource,
	addRules, rmRules, _ []*securitygroup.CloudRule) error {
	mutex.Lock()
	defer mutex.Unlock()

	addIRule := make([]*securitygroup.CloudRule, 0)
	rmIRule := make([]*securitygroup.CloudRule, 0)
	addERule := make([]*securitygroup.CloudRule, 0)
	rmERule := make([]*securitygroup.CloudRule, 0)
	for _, rule := range addRules {
		switch rule.Rule.(type) {
		case *securitygroup.IngressRule:
			addIRule = append(addIRule, rule)
		case *securitygroup.EgressRule:
			addERule = append(addERule, rule)
		}
	}
	for _, rule := range rmRules {
		switch rule.Rule.(type) {
		case *securitygroup.IngressRule:
			rmIRule = append(rmIRule, rule)
		case *securitygroup.EgressRule:
			rmERule = append(rmERule, rule)
		}
	}

	vpcID := appliedToGroupIdentifier.Vpc
	accCfg, found := c.cloudCommon.GetCloudAccountByAccountId(&appliedToGroupIdentifier.AccountID)
	if !found {
		return fmt.Errorf("aws account not found managing virtual private cloud [%v]", vpcID)
	}

	serviceCfg, err := accCfg.GetServiceConfigByName(awsComputeServiceNameEC2)
	if err != nil {
		return err
	}
	ec2Service := serviceCfg.(*ec2ServiceConfig)

	// build from addressGroups, cloudSgNames from rules
	cloudSgNames := buildEc2CloudSgNamesFromRules(&appliedToGroupIdentifier.CloudResourceID, append(addIRule, rmIRule...),
		append(addERule, rmERule...))

	// make sure all required security groups pre-exist
	vpcIDs := []string{vpcID}
	vpcPeerIDs := ec2Service.getVpcPeers(vpcID)
	vpcIDs = append(vpcIDs, vpcPeerIDs...)
	cloudSGNameToCloudSGObj, err := ec2Service.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, cloudSgNames)
	if err != nil {
		return err
	}
	if len(cloudSGNameToCloudSGObj) != len(cloudSgNames) {
		return fmt.Errorf("failed to find security groups")
	}

	cloudSGObjToAddRules := cloudSGNameToCloudSGObj[appliedToGroupIdentifier.GetCloudName(false)]

	// rollback operation for cloud api failures
	rollbackRmIngress := false
	rollbackAddIngress := false
	rollbackRmEgress := false
	defer func() {
		if rollbackRmIngress {
			_ = ec2Service.realizeIngressIPPermissions(cloudSGObjToAddRules, rmIRule, cloudSGNameToCloudSGObj, false)
		}
		if rollbackAddIngress {
			_ = ec2Service.realizeIngressIPPermissions(cloudSGObjToAddRules, addIRule, cloudSGNameToCloudSGObj, true)
		}
		if rollbackRmEgress {
			_ = ec2Service.realizeEgressIPPermissions(cloudSGObjToAddRules, rmERule, cloudSGNameToCloudSGObj, false)
		}
	}()

	// realize security group ingress and egress permissions
	if err = ec2Service.realizeIngressIPPermissions(cloudSGObjToAddRules, rmIRule, cloudSGNameToCloudSGObj, true); err != nil {
		return err
	}
	if err = ec2Service.realizeIngressIPPermissions(cloudSGObjToAddRules, addIRule, cloudSGNameToCloudSGObj, false); err != nil {
		rollbackRmIngress = true
		return err
	}
	if err = ec2Service.realizeEgressIPPermissions(cloudSGObjToAddRules, rmERule, cloudSGNameToCloudSGObj, true); err != nil {
		rollbackRmIngress = true
		rollbackAddIngress = true
		return err
	}
	if err = ec2Service.realizeEgressIPPermissions(cloudSGObjToAddRules, addERule, cloudSGNameToCloudSGObj, false); err != nil {
		rollbackRmIngress = true
		rollbackAddIngress = true
		rollbackRmEgress = true
		return err
	}

	return nil
}

// UpdateSecurityGroupMembers invokes cloud api and attaches/detaches nics to/from the cloud security group.
func (c *awsCloud) UpdateSecurityGroupMembers(securityGroupIdentifier *securitygroup.CloudResource,
	cloudResourceIdentifiers []*securitygroup.CloudResource, membershipOnly bool) error {
	mutex.Lock()
	defer mutex.Unlock()

	vpcID := securityGroupIdentifier.Vpc
	accCfg, found := c.cloudCommon.GetCloudAccountByAccountId(&securityGroupIdentifier.AccountID)
	if !found {
		return fmt.Errorf("aws account not found managing virtual private cloud [%v]", vpcID)
	}

	serviceCfg, err := accCfg.GetServiceConfigByName(awsComputeServiceNameEC2)
	if err != nil {
		return err
	}
	ec2Service := serviceCfg.(*ec2ServiceConfig)

	cloudSgName := securityGroupIdentifier.GetCloudName(membershipOnly)

	// get addressGroup cloudSgID
	vpcIDs := []string{vpcID}
	cloudSgNames := map[string]struct{}{cloudSgName: {}}
	out, err := ec2Service.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, cloudSgNames)
	if err != nil {
		return err
	}
	if len(out) != len(cloudSgNames) {
		return fmt.Errorf("failed to find cloud sg (%v) corresponding to address group (%v)",
			securityGroupIdentifier.Name, cloudSgName)
	}
	cloudSgID := out[cloudSgName].GroupId

	err = ec2Service.updateSecurityGroupMembers(cloudSgID, cloudSgName, vpcID, cloudResourceIdentifiers, membershipOnly)
	if err != nil {
		return err
	}

	return nil
}

// DeleteSecurityGroup invokes cloud api and deletes the cloud security group. Any attached resource will be moved to default sg.
func (c *awsCloud) DeleteSecurityGroup(securityGroupIdentifier *securitygroup.CloudResource, membershipOnly bool) error {
	mutex.Lock()
	defer mutex.Unlock()

	vpcID := securityGroupIdentifier.Vpc
	accCfg, found := c.cloudCommon.GetCloudAccountByAccountId(&securityGroupIdentifier.AccountID)
	if !found {
		return fmt.Errorf("aws account not found managing virtual private cloud [%v]", vpcID)
	}

	serviceCfg, err := accCfg.GetServiceConfigByName(awsComputeServiceNameEC2)
	if err != nil {
		return err
	}
	ec2Service := serviceCfg.(*ec2ServiceConfig)

	// check if sg exists in cloud and get its cloud sg id to delete
	vpcIDs := []string{vpcID}
	cloudSgNameToDelete := securityGroupIdentifier.GetCloudName(membershipOnly)
	out, err := ec2Service.getCloudSecurityGroupsWithNameFromCloud(vpcIDs, map[string]struct{}{cloudSgNameToDelete: {}})
	if err != nil || len(out) == 0 {
		return err
	}

	// Detach security group from interfaces before deleting.
	cloudSgIDToDelete := out[cloudSgNameToDelete].GroupId
	err = ec2Service.updateSecurityGroupMembers(cloudSgIDToDelete, cloudSgNameToDelete, vpcID, nil, membershipOnly)
	if err != nil {
		return err
	}

	// delete security group
	input := &ec2.DeleteSecurityGroupInput{
		GroupId: cloudSgIDToDelete,
	}
	_, err = ec2Service.apiClient.deleteSecurityGroup(input)
	if err != nil {
		return err
	}

	return nil
}

func (c *awsCloud) GetEnforcedSecurity() []securitygroup.SynchronizationContent {
	inventoryInitWaitDuration := 30 * time.Second

	var accNamespacedNames []types.NamespacedName
	accountConfigs := c.cloudCommon.GetCloudAccounts()
	for _, accCfg := range accountConfigs {
		accNamespacedNames = append(accNamespacedNames, *accCfg.GetNamespacedName())
	}

	var enforcedSecurityCloudView []securitygroup.SynchronizationContent
	var wg sync.WaitGroup
	ch := make(chan []securitygroup.SynchronizationContent)
	wg.Add(len(accNamespacedNames))
	go func() {
		wg.Wait()
		close(ch)
	}()

	for _, accNamespacedName := range accNamespacedNames {
		accNamespacedNameCopy := &types.NamespacedName{
			Namespace: accNamespacedName.Namespace,
			Name:      accNamespacedName.Name,
		}

		go func(name *types.NamespacedName, sendCh chan<- []securitygroup.SynchronizationContent) {
			defer wg.Done()

			accCfg, found := c.cloudCommon.GetCloudAccountByName(name)
			if !found {
				awsPluginLogger().Info("enforced-security-cloud-view GET for account skipped (account no longer exists)", "account", name)
				return
			}

			serviceCfg, err := accCfg.GetServiceConfigByName(awsComputeServiceNameEC2)
			if err != nil {
				awsPluginLogger().Error(err, "enforced-security-cloud-view GET for account skipped", "account", accCfg.GetNamespacedName())
				return
			}
			ec2Service := serviceCfg.(*ec2ServiceConfig)
			err = ec2Service.waitForInventoryInit(inventoryInitWaitDuration)
			if err != nil {
				awsPluginLogger().Error(err, "enforced-security-cloud-view GET for account skipped", "account", accCfg.GetNamespacedName())
				return
			}
			sendCh <- ec2Service.getNepheControllerManagedSecurityGroupsCloudView()
		}(accNamespacedNameCopy, ch)
	}

	for val := range ch {
		if val != nil {
			enforcedSecurityCloudView = append(enforcedSecurityCloudView, val...)
		}
	}
	return enforcedSecurityCloudView
}
