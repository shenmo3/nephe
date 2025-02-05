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

package cloud

import (
	"fmt"
	"github.com/mohae/deepcopy"
	"k8s.io/apimachinery/pkg/watch"

	runtimev1alpha1 "antrea.io/nephe/apis/runtime/v1alpha1"
	"antrea.io/nephe/pkg/cloud-provider/securitygroup"
	"antrea.io/nephe/pkg/controllers/inventory/common"
)

// syncImpl synchronizes securityGroup memberships with cloud.
// Return true if cloud and controller has same membership.
func (s *securityGroupImpl) syncImpl(csg cloudSecurityGroup, syncContent *securitygroup.SynchronizationContent,
	membershipOnly bool, r *NetworkPolicyReconciler) bool {
	log := r.Log.WithName("CloudSync")
	if syncContent == nil {
		// If syncContent is nil, explicitly set internal sg state to init, so that
		// AddressGroup or AppliedToGroup in cloud can be recreated.
		s.state = securityGroupStateInit
	} else if syncContent != nil {
		s.state = securityGroupStateCreated
		syncMembers := make([]*securitygroup.CloudResource, 0, len(syncContent.Members))
		for i := range syncContent.Members {
			syncMembers = append(syncMembers, &syncContent.Members[i])
		}

		cachedMembers := s.members
		if len(syncMembers) > 0 && syncMembers[0].Type == securitygroup.CloudResourceTypeNIC {
			cachedMembers, _ = r.getNICsOfCloudResources(s.members)
		}
		if compareCloudResources(cachedMembers, syncMembers) {
			return true
		} else {
			log.V(1).Info("Members are not in sync with cloud", "Name", s.id.Name, "State", s.state,
				"Sync members", syncMembers, "Cached SG members", cachedMembers)
		}
	} else if len(s.members) == 0 {
		log.V(1).Info("Empty memberships", "Name", s.id.Name)
		return true
	}

	if s.state == securityGroupStateCreated {
		log.V(1).Info("Update securityGroup", "Name", s.id.Name,
			"MembershipOnly", membershipOnly, "CloudSecurityGroup", syncContent)
		_ = s.updateImpl(csg, nil, nil, membershipOnly, r)
	} else if s.state == securityGroupStateInit {
		log.V(1).Info("Add securityGroup", "Name", s.id.Name,
			"MembershipOnly", membershipOnly, "CloudSecurityGroup", syncContent)
		_ = s.addImpl(csg, membershipOnly, r)
	}
	return false
}

// sync synchronizes addressSecurityGroup with cloud.
func (a *addrSecurityGroup) sync(syncContent *securitygroup.SynchronizationContent, r *NetworkPolicyReconciler) {
	log := r.Log.WithName("CloudSync")
	if a.deletePending {
		log.V(1).Info("AddressSecurityGroup pending delete", "Name", a.id.Name)
		return
	}
	_ = a.syncImpl(a, syncContent, true, r)
}

// sync synchronizes appliedToSecurityGroup with cloud.
func (a *appliedToSecurityGroup) sync(syncContent *securitygroup.SynchronizationContent, r *NetworkPolicyReconciler) {
	log := r.Log.WithName("CloudSync")
	if a.deletePending {
		log.V(1).Info("AppliedSecurityGroup pending delete", "Name", a.id.Name)
		return
	}
	if a.syncImpl(a, syncContent, false, r) && len(a.members) > 0 {
		a.hasMembers = true
	}

	// roughly count rule items in network policies and update rules if any mismatch with syncContent.
	nps, err := r.networkPolicyIndexer.ByIndex(networkPolicyIndexerByAppliedToGrp, a.id.Name)
	if err != nil {
		log.Error(err, "get networkPolicy by indexer", "Index", networkPolicyIndexerByAppliedToGrp, "Key", a.id.Name)
		return
	}
	items := make(map[string]int)
	for _, i := range nps {
		np := i.(*networkPolicy)
		if !np.rulesReady {
			if !np.computeRules(r) {
				log.V(1).Info("np not ready", "Name", np.Name, "Namespace", np.Namespace)
			}
		}
		for _, iRule := range np.ingressRules {
			countIngressRuleItems(iRule, items, false)
		}
		for _, eRule := range np.egressRules {
			countEgressRuleItems(eRule, items, false)
		}
	}

	if syncContent == nil {
		_ = a.updateAllRules(r)
		return
	}

	rules, err := r.cloudRuleIndexer.ByIndex(cloudRuleIndexerByAppliedToGrp, a.id.CloudResourceID.String())
	if err != nil {
		log.Error(err, "get cloudRule indexer", "Key", a.id.CloudResourceID.String())
		return
	}
	indexerUpdate := false
	cloudRuleMap := make(map[string]*securitygroup.CloudRule)
	for _, obj := range rules {
		rule := obj.(*securitygroup.CloudRule)
		cloudRuleMap[rule.Hash] = rule
	}

	// roughly count and compare rules in syncContent against nps.
	// also updates cloudRuleIndexer in the process.
	for _, iRule := range syncContent.IngressRules {
		countIngressRuleItems(&iRule, items, true)
		if updated := a.checkAndUpdateIndexer(r, &iRule, cloudRuleMap); updated {
			indexerUpdate = true
		}
	}
	for _, eRule := range syncContent.EgressRules {
		countEgressRuleItems(&eRule, items, true)
		if updated := a.checkAndUpdateIndexer(r, &eRule, cloudRuleMap); updated {
			indexerUpdate = true
		}
	}
	// remove rules no longer exist in cloud from indexer.
	for _, rule := range cloudRuleMap {
		indexerUpdate = true
		_ = r.cloudRuleIndexer.Delete(rule)
	}

	if indexerUpdate {
		_ = a.updateAllRules(r)
		return
	}

	for k, i := range items {
		if i != 0 {
			log.V(1).Info("Update appliedToSecurityGroup rules", "Name",
				a.id.CloudResourceID.String(), "CloudSecurityGroup", syncContent, "Item", k, "Diff", i)
			_ = a.updateAllRules(r)
			return
		}
	}

	// rule machines
	if len(nps) > 0 {
		if !a.ruleReady {
			a.markDirty(r, false)
		}
		a.ruleReady = true
	}
}

// syncWithCloud synchronizes security group in controller with cloud.
// This is a blocking call intentionally so that no other events are accepted during
// synchronization.
func (r *NetworkPolicyReconciler) syncWithCloud() {
	log := r.Log.WithName("CloudSync")

	if r.bookmarkCnt < npSyncReadyBookMarkCnt {
		return
	}
	ch := securitygroup.CloudSecurityGroup.GetSecurityGroupSyncChan()
	cloudAddrSGs := make(map[securitygroup.CloudResourceID]*securitygroup.SynchronizationContent)
	cloudAppliedToSGs := make(map[securitygroup.CloudResourceID]*securitygroup.SynchronizationContent)
	rscWithUnknownSGs := make(map[securitygroup.CloudResource]struct{})
	for content := range ch {
		log.V(1).Info("Sync from cloud", "SecurityGroup", content)
		indexer := r.addrSGIndexer
		sgNew := newAddrSecurityGroup
		if !content.MembershipOnly {
			indexer = r.appliedToSGIndexer
			sgNew = newAppliedToSecurityGroup
		}
		// Removes unknown sg.
		if _, ok, _ := indexer.GetByKey(content.Resource.CloudResourceID.String()); !ok {
			log.V(0).Info("Delete SecurityGroup not found in cache", "Name", content.Resource.Name, "MembershipOnly", content.MembershipOnly)
			state := securityGroupStateCreated
			_ = sgNew(&content.Resource, []*securitygroup.CloudResource{}, &state).delete(r)
			continue
		}
		// copy channel reference content to a local variable because we use pointer to
		// reference to cloud sg.
		cc := content
		if content.MembershipOnly {
			cloudAddrSGs[content.Resource.CloudResourceID] = &cc
		} else {
			cloudAppliedToSGs[content.Resource.CloudResourceID] = &cc
			for _, rsc := range content.MembersWithOtherSGAttached {
				rscWithUnknownSGs[rsc] = struct{}{}
			}
		}
	}
	r.syncedWithCloud = true
	for _, i := range r.addrSGIndexer.List() {
		sg := i.(*addrSecurityGroup)
		sg.sync(cloudAddrSGs[sg.getID()], r)
	}
	for _, i := range r.appliedToSGIndexer.List() {
		sg := i.(*appliedToSecurityGroup)
		sg.sync(cloudAppliedToSGs[sg.getID()], r)
	}
	// For cloud resource with any non nephe created SG, tricking plug-in to remove them by explicitly
	// updating a single instance of associated security group.
	for rsc := range rscWithUnknownSGs {
		i, ok, _ := r.cloudResourceNPTrackerIndexer.GetByKey(rsc.String())
		if !ok {
			log.Info("Unable to find resource in tracker", "CloudResource", rsc)
			continue
		}
		tracker := i.(*cloudResourceNPTracker)
		for _, sg := range tracker.appliedToSGs {
			_ = sg.update(nil, nil, r)
			break
		}
	}
}

// processBookMark process bookmark event and return true.
func (r *NetworkPolicyReconciler) processBookMark(event watch.EventType) bool {
	if event != watch.Bookmark {
		return false
	}
	if r.syncedWithCloud {
		return true
	}
	r.bookmarkCnt++
	r.syncWithCloud()
	return true
}

// getNICsOfCloudResources returns NICs of cloud resources if available.
func (r *NetworkPolicyReconciler) getNICsOfCloudResources(resources []*securitygroup.CloudResource) (
	[]*securitygroup.CloudResource, error) {
	if len(resources) == 0 {
		return nil, nil
	}
	if resources[0].Type == securitygroup.CloudResourceTypeNIC {
		return resources, nil
	}

	nics := make([]*securitygroup.CloudResource, 0, len(resources))
	for _, rsc := range resources {
		id := rsc.Name
		vmItems, err := r.Inventory.GetVmFromIndexer(common.VirtualMachineIndexerByCloudId, id)
		if err != nil {
			r.Log.Error(err, "failed to get VMs from VM cache")
			return resources, err
		}

		for _, item := range vmItems {
			vm := item.(*runtimev1alpha1.VirtualMachine)
			for _, nic := range vm.Status.NetworkInterfaces {
				nics = append(nics, &securitygroup.CloudResource{Type: securitygroup.CloudResourceTypeNIC,
					CloudResourceID: securitygroup.CloudResourceID{Name: nic.Name, Vpc: rsc.Vpc}})
			}
		}
	}
	return nics, nil
}

// checkAndUpdateIndexer checks if rule is present in indexer and updates the indexer if not present.
// Returns true if indexer is updated.
func (a *appliedToSecurityGroup) checkAndUpdateIndexer(r *NetworkPolicyReconciler, rule securitygroup.Rule,
	existingRuleMap map[string]*securitygroup.CloudRule) bool {
	indexerUpdate := false

	// deep copy the rule and construct CloudRule object from it.
	ruleCopy := deepcopy.Copy(rule).(securitygroup.Rule)
	cr := &securitygroup.CloudRule{
		Rule:         ruleCopy,
		AppliedToGrp: a.id.CloudResourceID.String(),
	}
	cr.Hash = cr.GetHash()

	// update rule if not found in indexer, otherwise remove from map to indicate a matching rule is found.
	if _, found := existingRuleMap[cr.Hash]; !found {
		indexerUpdate = true
		_ = r.cloudRuleIndexer.Update(cr)
	} else {
		delete(existingRuleMap, cr.Hash)
	}

	// return if indexer is updated or not.
	return indexerUpdate
}

// countIngressRuleItems updates the count of corresponding items in the given map based on contents of the specified ingress rule.
func countIngressRuleItems(iRule *securitygroup.IngressRule, items map[string]int, subtract bool) {
	proto := 0
	if iRule.Protocol != nil {
		proto = *iRule.Protocol
	}
	port := 0
	if iRule.FromPort != nil {
		port = *iRule.FromPort
	}
	if proto > 0 || port > 0 {
		portStr := fmt.Sprintf("protocol=%v,port=%v", proto, port)
		updateCountForItem(portStr, items, subtract)
	}
	for _, ip := range iRule.FromSrcIP {
		updateCountForItem(ip.String(), items, subtract)
	}
	for _, sg := range iRule.FromSecurityGroups {
		updateCountForItem(sg.String(), items, subtract)
	}
}

// countEgressRuleItems updates the count of corresponding items in the given map based on contents of the specified egress rule.
func countEgressRuleItems(eRule *securitygroup.EgressRule, items map[string]int, subtract bool) {
	proto := 0
	if eRule.Protocol != nil {
		proto = *eRule.Protocol
	}
	port := 0
	if eRule.ToPort != nil {
		port = *eRule.ToPort
	}
	if proto > 0 || port > 0 {
		portStr := fmt.Sprintf("protocol=%v,port=%v", proto, port)
		updateCountForItem(portStr, items, subtract)
	}
	for _, ip := range eRule.ToDstIP {
		updateCountForItem(ip.String(), items, subtract)
	}
	for _, sg := range eRule.ToSecurityGroups {
		updateCountForItem(sg.String(), items, subtract)
	}
}

// updateCountForItem adds or subtracts the item count in the items map.
func updateCountForItem(item string, items map[string]int, subtract bool) {
	if subtract {
		items[item]--
	} else {
		items[item]++
	}
}
