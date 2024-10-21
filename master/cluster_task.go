// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

type ChooseMetaHostFunc func(c *Cluster, nodeAddr string, mp *MetaPartition, oldHosts []string, excludeNodeSets []uint64, zoneName string, dstStoreMode proto.StoreMode) (oldAddr, addAddr string, err error)

func (c *Cluster) addDataNodeTasks(tasks []*proto.AdminTask) {
	for _, t := range tasks {
		c.addDataNodeTask(t)
	}
}

func (c *Cluster) addDataNodeTask(task *proto.AdminTask) {
	if task == nil {
		return
	}
	if node, err := c.dataNode(task.OperatorAddr); err != nil {
		log.LogWarn(fmt.Sprintf("action[putTasks],nodeAddr:%v,taskID:%v,err:%v", task.OperatorAddr, task.ID, err))
	} else {
		node.TaskManager.AddTask(task)
	}
}

func (c *Cluster) addMetaNodeTasks(tasks []*proto.AdminTask) {

	for _, t := range tasks {
		if t == nil {
			continue
		}
		if node, err := c.metaNode(t.OperatorAddr); err != nil {
			log.LogWarn(fmt.Sprintf("action[putTasks],nodeAddr:%v,taskID:%v,err:%v", t.OperatorAddr, t.ID, err.Error()))
		} else {
			node.Sender.AddTask(t)
		}
	}
}

func (c *Cluster) waitForResponseToLoadDataPartition(partitions []*DataPartition) {

	var wg sync.WaitGroup
	for _, dp := range partitions {
		wg.Add(1)
		go func(dp *DataPartition) {
			defer func() {
				wg.Done()
				if err := recover(); err != nil {
					const size = runtimeStackBufSize
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					log.LogError(fmt.Sprintf("doLoadDataPartition panic %v: %s\n", err, buf))
				}
			}()
			c.doLoadDataPartition(dp)
		}(dp)
	}
	wg.Wait()
}

func (c *Cluster) loadDataPartition(dp *DataPartition) {
	go func() {
		c.doLoadDataPartition(dp)
	}()
}

// taking the given mata partition offline. In strict mode, only if the size of the replica is equal,
// or the number of files is equal, the recovery is considered complete. when it is triggered by migrated metaNode,
// the strict mode is true,otherwise is false.
// 1. checking if the meta partition can be offline.
// There are two cases where the partition is not allowed to be offline:
// (1) the replica is not in the latest host list
// (2) there are too few replicas
// 2. choosing a new available meta node
// 3. synchronized decommission meta partition
// 4. synchronized create a new meta partition
// 5. persistent the new host list
func (c *Cluster) decommissionMetaPartition(nodeAddr string, mp *MetaPartition, chooseMetaHostFunc ChooseMetaHostFunc, destAddr string, strictMode bool, dstStoreMode proto.StoreMode) (err error) {
	var (
		addAddr         string
		excludeNodeSets []uint64
		oldPeerHosts    []string
		vol             *Vol
		isLearner       bool
	)
	mp.offlineMutex.Lock()
	defer func() {
		mp.isOffline = false
		mp.offlineMutex.Unlock()
	}()
	mp.lastOfflineTime = time.Now().Unix()
	mp.isOffline = true
	oldPeerHosts = mp.peerHosts()
	if vol, err = c.getVol(mp.volName); err != nil {
		goto errHandler
	}

	if dstStoreMode == proto.StoreModeDef {
		dstStoreMode = vol.DefaultStoreMode
		for _, replica := range mp.Replicas {
			if replica.Addr == nodeAddr {
				dstStoreMode = replica.StoreMode
				break
			}
		}
	}
	if destAddr != "" {
		if err = c.validateDecommissionMetaPartition(mp, nodeAddr); err != nil {
			goto errHandler
		}
		if _, err = c.metaNode(nodeAddr); err != nil {
			goto errHandler
		}
		if mp.hasPeer(destAddr) {
			err = fmt.Errorf("destinationAddr[%v] must be a new meta node addr,oldPeers[%v]", destAddr, mp.Peers)
			goto errHandler
		}
		if _, err = c.metaNode(destAddr); err != nil {
			goto errHandler
		}
		addAddr = destAddr
	} else {
		if nodeAddr, addAddr, err = chooseMetaHostFunc(c, nodeAddr, mp, oldPeerHosts, excludeNodeSets, vol.zoneName, dstStoreMode); err != nil {
			goto errHandler
		}
	}

	log.LogWarnf("action[decommissionMetaPartition],volName[%v],nodeAddr[%v],partitionID[%v] begin", mp.volName, nodeAddr, mp.PartitionID)

	if contains(mp.Recorders, nodeAddr) {
		err = c.decommissionMetaRecorder(mp, nodeAddr, addAddr, strictMode)
		if err != nil {
			goto errHandler
		}
	} else {
		isLearner, err = c.decommissionMetaReplica(mp, vol.CrossRegionHAType, nodeAddr, addAddr, strictMode, dstStoreMode)
		if err != nil {
			goto errHandler
		}
	}

	if strictMode {
		c.putMigratedMetaPartitions(nodeAddr, mp.PartitionID)
	} else {
		if !isLearner {
			c.putBadMetaPartitions(nodeAddr, mp.PartitionID)
		}
	}
	mp.RLock()
	c.syncUpdateMetaPartition(mp)
	mp.modifyTime = time.Now().Unix()
	mp.RUnlock()
	return
errHandler:
	log.LogError(fmt.Sprintf("action[decommissionMetaPartition],volName: %v,partitionID: %v,err: %v",
		mp.volName, mp.PartitionID, errors.Stack(err)))
	if err != nil {
		err = fmt.Errorf("vol[%v],partition[%v],err[%v]", mp.volName, mp.PartitionID, err)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyMpDecommissionFailed], err.Error())
	}
	return
}

func (c *Cluster) decommissionMetaReplica(mp *MetaPartition, crossRegionHAType proto.CrossRegionHAType, nodeAddr, addAddr string, strictMode bool, dstStoreMode proto.StoreMode) (isLearner bool, err error) {
	var (
		pmConfig    *proto.PromoteConfig
		regionType  proto.RegionType
	)
	if isLearner, pmConfig, err = c.deleteMetaReplica(mp, nodeAddr, false, strictMode); err != nil {
		return
	}
	// if the vol is quorum type and the zone of addAddr is slave region zone, add to learner replica
	if IsCrossRegionHATypeQuorum(crossRegionHAType) {
		if regionType, err = c.getMetaNodeRegionType(addAddr); err != nil {
			return
		}
		if regionType == proto.SlaveRegion {
			isLearner = true
			pmConfig = &proto.PromoteConfig{AutoProm: false, PromThreshold: 100}
		}
	}
	if isLearner {
		if err = c.addMetaReplicaLearner(mp, addAddr, pmConfig.AutoProm, pmConfig.PromThreshold, false, dstStoreMode); err != nil {
			return
		}
	} else {
		if err = c.addMetaReplica(mp, addAddr, dstStoreMode); err != nil {
			return
		}
		mp.IsRecover = true
	}
	return
}

func (c *Cluster) decommissionMetaRecorder(mp *MetaPartition, nodeAddr, addAddr string, strictMode bool) (err error) {
	if err = c.deleteMetaRecorder(mp, nodeAddr, false, strictMode); err != nil {
		return
	}
	if err = c.addMetaRecorder(mp, addAddr); err != nil {
		return
	}
	mp.IsRecover = true
	return
}

func (c *Cluster) selectMetaReplaceAddr(nodeAddr string, mp *MetaPartition, dstStoreMode proto.StoreMode) (dstAddr string, err error) {
	var (
		excludeNodeSets []uint64
		oldHosts        []string
		vol             *Vol
	)
	mp.offlineMutex.Lock()
	defer mp.offlineMutex.Unlock()
	oldHosts = mp.peerHosts()
	if vol, err = c.getVol(mp.volName); err != nil {
		goto errHandler
	}

	if dstStoreMode == proto.StoreModeDef {
		dstStoreMode = vol.DefaultStoreMode
	}
	if nodeAddr, dstAddr, err = getTargetAddressForMetaPartitionDecommission(c, nodeAddr, mp, oldHosts, excludeNodeSets, vol.zoneName, dstStoreMode); err != nil {
		goto errHandler
	}

	return

errHandler:
	msg := fmt.Sprintf("action[selectMetaReplaceAddr] clusterID[%v] volName: [%v] meta partition[%v] addr[%v] select replace failed,err:%v",
		c.Name, mp.volName, mp.PartitionID, nodeAddr, err)
	log.LogError(msg)
	WarnBySpecialKey(gAlarmKeyMap[alarmKeyChooseTargetHost], msg)
	if err != nil {
		err = fmt.Errorf("vol[%v],partition[%v],err[%v]", mp.volName, mp.PartitionID, err)
	}
	return
}

var getTargetAddressForMetaPartitionDecommission = func(c *Cluster, nodeAddr string, mp *MetaPartition, oldHosts []string,
	excludeNodeSets []uint64, zoneName string, dstStoreMode proto.StoreMode) (oldAddr, addAddr string, err error) {
	var (
		metaNode    *MetaNode
		zone        *Zone
		zones       []string
		ns          *nodeSet
		newPeers    []proto.Peer
		excludeZone string
		vol         *Vol
	)
	oldAddr = nodeAddr

	if vol, err = c.getVol(mp.volName); err != nil {
		return
	}
	if err = c.validateDecommissionMetaPartition(mp, nodeAddr); err != nil {
		return
	}
	if metaNode, err = c.metaNode(nodeAddr); err != nil {
		return
	}
	if zone, err = c.t.getZone(metaNode.ZoneName); err != nil {
		return
	}
	if ns, err = zone.getNodeSet(metaNode.NodeSetID); err != nil {
		return
	}
	if _, newPeers, _, err = ns.getAvailMetaNodeHosts(oldHosts, 1, 0, dstStoreMode); err != nil {
		// choose a meta node in other node set in the same zone
		excludeNodeSets = append(excludeNodeSets, ns.ID)
		if _, newPeers, _, err = zone.getAvailMetaNodeHosts(excludeNodeSets, oldHosts, 1, 0, dstStoreMode); err != nil {
			if IsCrossRegionHATypeQuorum(vol.CrossRegionHAType) {
				//select meta nodes from the other zones in the same region type
				_, newPeers, err = c.chooseTargetMetaNodesFromSameRegionTypeOfOfflineReplica(zone.regionName, vol.zoneName,
					1, excludeNodeSets, oldHosts, dstStoreMode)
				if err != nil {
					return
				}
				if len(newPeers) > 0 {
					addAddr = newPeers[0].Addr
				}
				return
			}
			zones = mp.getLiveZones(nodeAddr)
			if len(zones) == 0 {
				excludeZone = zone.name
			} else {
				excludeZone = zones[0]
			}
			// choose a meta node in other zone
			if _, newPeers, err = c.chooseTargetMetaHostForDecommission(excludeZone, mp, oldHosts, 1, zoneName, dstStoreMode); err != nil {
				return
			}
		}
	}
	if len(newPeers) > 0 {
		addAddr = newPeers[0].Addr
	}
	return
}

func (c *Cluster) validateDecommissionMetaPartition(mp *MetaPartition, nodeAddr string) (err error) {
	mp.RLock()
	defer mp.RUnlock()
	var vol *Vol
	if !contains(mp.Hosts, nodeAddr) && !contains(mp.Recorders, nodeAddr) {
		err = fmt.Errorf("offline address:[%v] is not in meta partition peers:%v", nodeAddr, mp.Peers)
		return
	}

	if vol, err = c.getVol(mp.volName); err != nil {
		return
	}
	if err = mp.canBeOffline(nodeAddr, int(vol.mpReplicaNum), int(vol.mpRecorderNum)); err != nil {
		return
	}

	if err = mp.hasMissingOneReplica(nodeAddr, int(vol.mpReplicaNum), int(vol.mpRecorderNum)); err != nil {
		return
	}

	if IsCrossRegionHATypeQuorum(vol.CrossRegionHAType) {
		if mp.isLearnerReplica(nodeAddr) {
			return
		}
	}
	if mp.IsRecover && !mp.isLatestReplica(nodeAddr) {	// todo recorder引起的recover
		err = fmt.Errorf("vol[%v],meta partition[%v] is recovering,[%v] can't be decommissioned", vol.Name, mp.PartitionID, nodeAddr)
		return
	}
	return
}

func (c *Cluster) checkCorruptMetaPartitions() (inactiveMetaNodes []string, corruptPartitions []*MetaPartition, err error) {
	partitionMap := make(map[uint64]uint8)
	c.metaNodes.Range(func(addr, node interface{}) bool {
		metaNode := node.(*MetaNode)
		if !metaNode.IsActive {
			inactiveMetaNodes = append(inactiveMetaNodes, metaNode.Addr)
		}
		return true
	})
	for _, addr := range inactiveMetaNodes {
		partitions := c.getAllMetaPartitionByMetaNode(addr)

		for _, partition := range partitions {
			partitionMap[partition.PartitionID] = partitionMap[partition.PartitionID] + 1
		}
	}

	for partitionID, badNum := range partitionMap {
		var partition *MetaPartition
		if partition, err = c.getMetaPartitionByID(partitionID); err != nil {
			return
		}
		if badNum > (partition.ReplicaNum+partition.RecorderNum)/2 {
			corruptPartitions = append(corruptPartitions, partition)
		}
	}
	log.LogInfof("clusterID[%v] inactiveMetaNodes:%v  corruptPartitions count:[%v]",
		c.Name, inactiveMetaNodes, len(corruptPartitions))
	return
}

// check corrupt partitions related to this meta node
func (c *Cluster) checkCorruptMetaNode(metaNode *MetaNode) (corruptPartitions []*MetaPartition, panicHostsList, panicRecordersList [][]string, err error) {
	var (
		mn             *MetaNode
		corruptPids    []uint64
		metaPartitions []*MetaPartition
	)
	panicHostsList = make([][]string, 0)
	panicRecordersList = make([][]string, 0)
	metaNode.RLock()
	defer metaNode.RUnlock()
	metaPartitions = c.getAllMetaPartitionByMetaNode(metaNode.Addr)
	for _, partition := range metaPartitions {
		panicHosts := make([]string, 0)
		learnerHosts := partition.getLearnerHosts()
		for _, host := range partition.Hosts {
			if contains(learnerHosts, host) {
				continue
			}
			if mn, err = c.metaNode(host); err != nil {
				return
			}
			if !mn.IsActive {
				panicHosts = append(panicHosts, host)
			}
		}
		panicRecorders := make([]string, 0)
		for _, recorder := range partition.Recorders {
			if mn, err = c.metaNode(recorder); err != nil {
				return
			}
			if !mn.IsActive {
				panicRecorders = append(panicRecorders, recorder)
			}
		}
		if len(panicHosts)+len(panicRecorders) > int((partition.ReplicaNum+partition.RecorderNum)/2) && len(panicHosts) != int(partition.ReplicaNum) {
			corruptPartitions = append(corruptPartitions, partition)
			panicHostsList = append(panicHostsList, panicHosts)
			panicRecordersList = append(panicRecordersList, panicRecorders)
			corruptPids = append(corruptPids, partition.PartitionID)
		}
	}
	log.LogInfof("action[checkCorruptMetaNode],clusterID[%v] metaNodeAddr:[%v], corrupt partitions%v",
		c.Name, metaNode.Addr, corruptPids)
	return
}

func (c *Cluster) checkLackReplicaMetaPartitions() (lackReplicaMetaPartitions []*MetaPartition, err error) {
	vols := c.copyVols()
	for _, vol := range vols {
		vol.mpsLock.RLock()
		for _, mp := range vol.MetaPartitions {
			if mp.ReplicaNum+mp.LearnerNum > uint8(len(mp.Hosts)) || mp.RecorderNum > uint8(len(mp.Recorders)) {
				lackReplicaMetaPartitions = append(lackReplicaMetaPartitions, mp)
			}
		}
		vol.mpsLock.RUnlock()
	}
	log.LogInfof("clusterID[%v] lackReplicaMetaPartitions count:[%v]", c.Name, len(lackReplicaMetaPartitions))
	return
}

func (c *Cluster) resetMetaPartition(mp *MetaPartition, panicHosts, panicRecorders []string) (err error) {
	var msg string
	defer func() {
		if err != nil {
			log.LogErrorf("action[resetMetaPartition],vol[%v],data partition[%v],err[%v]", mp.volName, mp.PartitionID, err)
		}
	}()
	if err = c.forceRemoveMetaRaftPeers(mp, panicHosts, panicRecorders); err != nil {
		goto errHandler
	}
	if len(panicHosts) == 0 && len(panicRecorders) == 0 {
		return
	}
	//record each badAddress, and update the badAddress by a new address in the same zone(nodeSet)
	for _, address := range panicHosts {
		c.putBadMetaPartitions(address, mp.PartitionID)
	}
	for _, address := range panicRecorders {
		c.putBadMetaPartitions(address, mp.PartitionID)
	}
	mp.Lock()
	mp.Status = proto.ReadOnly
	mp.IsRecover = true
	mp.PanicHosts = panicHosts
	mp.PanicRecorders = panicRecorders
	mp.modifyTime = time.Now().Unix()
	c.syncUpdateMetaPartition(mp)
	mp.Unlock()

	log.LogWarnf("clusterID[%v] partitionID:%v  panicHosts:%v panicRecorders:%v reset success,PersistenceHosts:[%v] PersistenceRecorders:[%v]",
		c.Name, mp.PartitionID, panicHosts, panicRecorders, mp.Hosts, mp.Recorders)
	return

errHandler:
	msg = fmt.Sprintf(" clusterID[%v] partitionID:%v  badHosts:%v  badRecorders:%v  "+
		"Err:%v , PersistenceHosts:%v  PersistenceRecorders:%v",
		c.Name, mp.PartitionID, panicHosts, panicRecorders, err, mp.Hosts, mp.Recorders)
	if err != nil {
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyMpReset], msg)
	}
	return
}

func (c *Cluster) chooseTargetMetaPartitionHost(oldAddr string, mp *MetaPartition) (newPeer proto.Peer, err error) {
	var (
		excludeNodeSets []uint64
		metaNode        *MetaNode
		zone            *Zone
		ns              *nodeSet
		vol             *Vol
		newPeers        []proto.Peer
		oldHosts        []string
		excludeZone     string
		zones           []string
		msg             string
		dstStoreMode    proto.StoreMode
	)
	oldHosts = mp.peerHosts()
	if metaNode, err = c.metaNode(oldAddr); err != nil {
		goto errHandler
	}
	if zone, err = c.t.getZone(metaNode.ZoneName); err != nil {
		goto errHandler
	}
	if ns, err = zone.getNodeSet(metaNode.NodeSetID); err != nil {
		goto errHandler
	}
	if vol, err = c.getVol(mp.volName); err != nil {
		goto errHandler
	}
	dstStoreMode = vol.DefaultStoreMode
	for _, replica := range mp.Replicas {
		if strings.Contains(replica.Addr, oldAddr) {
			dstStoreMode = replica.StoreMode
			break
		}
	}
	if _, newPeers, _, err = ns.getAvailMetaNodeHosts(oldHosts, 1, 0, dstStoreMode); err != nil {
		// choose a meta node in other node set in the same zone
		excludeNodeSets = append(excludeNodeSets, ns.ID)
		if _, newPeers, _, err = zone.getAvailMetaNodeHosts(excludeNodeSets, oldHosts, 1, 0, dstStoreMode); err != nil {
			if IsCrossRegionHATypeQuorum(vol.CrossRegionHAType) {
				//select meta nodes from the other zones in the same region type
				_, newPeers, err = c.chooseTargetMetaNodesFromSameRegionTypeOfOfflineReplica(zone.regionName, vol.zoneName,
					1, excludeNodeSets, oldHosts, dstStoreMode)
				if err != nil {
					return
				}
				if len(newPeers) > 0 {
					newPeer = newPeers[0]
				}
				return
			}
			zones = mp.getLiveZones(oldAddr)
			if len(zones) == 0 {
				excludeZone = zone.name
			} else {
				excludeZone = zones[0]
			}
			// choose a meta node in other zone
			if _, newPeers, _, err = c.chooseTargetMetaHosts(excludeZone, excludeNodeSets, oldHosts, 1, 0, "", false, dstStoreMode); err != nil {
				goto errHandler
			}
		}
	}
	newPeer = newPeers[0]
	return
errHandler:
	msg = fmt.Sprintf(" clusterID[%v] partitionID:%v  oldAddress:%v  "+
		"Err:%v , PersistenceHosts:%v  ",
		c.Name, mp.PartitionID, oldAddr, err, mp.Hosts)
	if err != nil {
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyChooseTargetHost], msg)
	}
	return
}

func (c *Cluster) forceRemoveMetaRaftPeers(mp *MetaPartition, panicHosts, panicRecorders []string) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[forceRemoveMetaRaftPeers],vol[%v],meta partition[%v],err[%v]", mp.volName, mp.PartitionID, err)
		}
	}()

	getNewPeersFunc := func(oldAddrs, panicAddrs []string) (newAddrPeers []proto.Peer, err error) {
		for _, addr := range oldAddrs {
			if contains(panicAddrs, addr) {
				continue
			}
			var metaNode *MetaNode
			metaNode, err = c.metaNode(addr)
			if err != nil {
				return
			}
			for _, peer := range mp.Peers {
				if peer.ID == metaNode.ID && peer.Addr == addr {
					newAddrPeers = append(newAddrPeers, peer)
				}
			}
		}
		return
	}
	var newHostsPeers, newRecordersPeers []proto.Peer
	mp.RLock()
	if newHostsPeers, err = getNewPeersFunc(mp.Hosts, panicHosts); err != nil {
		mp.RUnlock()
		return
	}
	if newRecordersPeers, err = getNewPeersFunc(mp.Recorders, panicRecorders); err != nil {
		mp.RUnlock()
		return
	}
	mp.RUnlock()
	// Only after reset peers succeed in remote metanode, the meta data can be updated
	newPeers := make([]proto.Peer, 0, len(newHostsPeers)+len(newRecordersPeers))
	newPeers = append(newPeers, newHostsPeers...)
	newPeers = append(newPeers, newRecordersPeers...)
	log.LogInfof("action[forceRemoveMetaRaftPeers],new peers[%v], old peers[%v], err[%v]", newPeers, mp.Peers, err)

	for _, peer := range newPeers {
		if err = c.resetMetaPartitionRaftMember(mp, newPeers, peer); err != nil {
			return
		}
	}

	c.removeMetaPartitionPanicHosts(mp, newPeers, panicHosts)
	c.removeMetaPartitionPanicRecorders(mp, newPeers, panicRecorders)
	return
}

func (c *Cluster) removeMetaPartitionPanicHosts(mp *MetaPartition, newPeers []proto.Peer, panicHosts []string) {
	var (
		metaNode 	*MetaNode
		err 		error
	)
	for _, addr := range panicHosts {
		newMetaPartitions := make([]uint64, 0)
		if metaNode, err = c.metaNode(addr); err != nil {
			log.LogErrorf("action[removeMetaPartitionPanicHosts],new peers[%v], old peers[%v], err[%v]", newPeers, mp.Peers, err)
			continue
		}
		if err = c.deleteMetaPartition(mp, metaNode, false); err != nil {
			log.LogErrorf("action[removeMetaPartitionPanicHosts],new peers[%v], old peers[%v], err[%v]", newPeers, mp.Peers, err)
			continue
		}
		for _, pid := range metaNode.PersistenceMetaPartitions {
			if pid != mp.PartitionID {
				newMetaPartitions = append(newMetaPartitions, pid)
			}
		}
		metaNode.PersistenceMetaPartitions = newMetaPartitions

		_ = c.removeMetaPartitionRaftOnly(mp, proto.Peer{ID: metaNode.ID, Addr: addr}, mp.createTaskToRemoveRaftOnly)
	}
}

func (c *Cluster) removeMetaPartitionPanicRecorders(mp *MetaPartition, newPeers []proto.Peer, panicRecorders []string) {
	var (
		metaNode 	*MetaNode
		err 		error
	)
	for _, addr := range panicRecorders {
		newMetaPartitions := make([]uint64, 0)
		if metaNode, err = c.metaNode(addr); err != nil {
			log.LogErrorf("action[removeMetaPartitionPanicRecorders],new peers[%v], old peers[%v], err[%v]", newPeers, mp.Peers, err)
			continue
		}
		if err = c.deleteMetaRecorderFromNode(mp, metaNode, false); err != nil {
			log.LogErrorf("action[removeMetaPartitionPanicRecorders],new peers[%v], old peers[%v], err[%v]", newPeers, mp.Peers, err)
			continue
		}
		for _, pid := range metaNode.PersistenceMetaRecorders {
			if pid != mp.PartitionID {
				newMetaPartitions = append(newMetaPartitions, pid)
			}
		}
		metaNode.PersistenceMetaRecorders = newMetaPartitions

		_ = c.removeMetaPartitionRaftOnly(mp, proto.Peer{ID: metaNode.ID, Addr: addr, Type: proto.PeerRecorder}, mp.createTaskToRemoveRaftRecorderOnly)
	}
}

func (c *Cluster) resetMetaPartitionRaftMember(mp *MetaPartition, newPeers []proto.Peer, peer proto.Peer) (err error) {
	var (
		metaNode	*MetaNode
		task		*proto.AdminTask
	)
	if metaNode, err = c.metaNode(peer.Addr); err != nil {
		return
	}

	mp.Lock()
	defer mp.Unlock()
	if peer.IsRecorder() {
		task, err = mp.createTaskToResetRaftRecorderMembers(newPeers, metaNode.Addr)
	} else {
		task, err = mp.createTaskToResetRaftMembers(newPeers, metaNode.Addr)
	}
	if err != nil {
		return
	}
	if _, err = metaNode.Sender.syncSendAdminTask(task); err != nil {
		log.LogErrorf("action[resetMetaPartitionRaftMember] vol[%v],meta partition[%v],err[%v]", mp.volName, mp.PartitionID, err)
		return
	}
	newHosts := make([]string, 0)
	newRecorders := make([]string, 0)
	for _, peer := range newPeers {
		if peer.IsRecorder() {
			newRecorders = append(newRecorders, peer.Addr)
		} else {
			newHosts = append(newHosts, peer.Addr)
		}
	}
	newLearners := make([]proto.Learner, 0)
	for _, learner := range mp.Learners {
		if contains(newHosts, learner.Addr) {
			newLearners = append(newLearners, learner)
		}
	}
	if err = mp.persistToRocksDB("resetMetaPartitionRaftMember", mp.volName, newHosts, newPeers, newLearners, newRecorders, c); err != nil {
		return
	}
	return
}

func (c *Cluster) deleteMetaReplica(partition *MetaPartition, addr string, validate, migrationMode bool) (isLearner bool, pmConfig *proto.PromoteConfig, err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[deleteMetaReplica],vol[%v],data partition[%v],err[%v]", partition.volName, partition.PartitionID, err)
		}
	}()
	if validate {
		if err = c.validateDecommissionMetaPartition(partition, addr); err != nil {
			return
		}
	}
	metaNode, err := c.metaNode(addr)
	if err != nil {
		return
	}
	removePeer := proto.Peer{ID: metaNode.ID, Addr: addr}
	if isLearner, pmConfig, err = c.removeMetaPartitionRaftMember(partition, removePeer, migrationMode); err != nil {
		return
	}
	if err = c.deleteMetaPartition(partition, metaNode, migrationMode); err != nil {
		return
	}
	return
}

func (c *Cluster) deleteMetaPartition(partition *MetaPartition, removeMetaNode *MetaNode, migrationMode bool) (err error) {
	partition.Lock()
	mr, err := partition.getMetaReplica(removeMetaNode.Addr)
	if err != nil {
		log.LogErrorf("action[deleteMetaPartition] vol[%v] meta partition[%v],err[%v]", partition.volName, partition.PartitionID, err)
		mr = newMetaReplica(partition.Start, partition.End, removeMetaNode)
	}
	task := mr.createTaskToDeleteReplica(partition.PartitionID)
	partition.removeReplicaByAddr(removeMetaNode.Addr)
	partition.removeMissingReplica(removeMetaNode.Addr)
	partition.Unlock()
	if migrationMode {
		return
	}
	_, err = removeMetaNode.Sender.syncSendAdminTask(task)
	if err != nil {
		log.LogErrorf("action[deleteMetaPartition] vol[%v],meta partition[%v],err[%v]", partition.volName, partition.PartitionID, err)
	}
	return nil
}

func (c *Cluster) removeMetaPartitionRaftMember(partition *MetaPartition, removePeer proto.Peer, migrationMode bool) (isLearner bool, pmConfig *proto.PromoteConfig, err error) {
	defer func() {
		if err1 := c.updateMetaPartitionOfflinePeerIDWithLock(partition, 0); err1 != nil {
			err = errors.Trace(err, "updateMetaPartitionOfflinePeerIDWithLock failed, err[%v]", err1)
		}
	}()
	if err = c.updateMetaPartitionOfflinePeerIDWithLock(partition, removePeer.ID); err != nil {
		return
	}
	mr, err := partition.getMetaReplicaLeader()
	if err != nil {
		return
	}
	t, err := partition.createTaskToRemoveRaftMember(removePeer)
	if err != nil {
		return
	}
	t.ReserveResource = migrationMode
	var leaderMetaNode *MetaNode
	leaderMetaNode = mr.metaNode
	if leaderMetaNode == nil {
		leaderMetaNode, err = c.metaNode(mr.Addr)
		if err != nil {
			return
		}
	}
	if _, err = leaderMetaNode.Sender.syncSendAdminTask(t); err != nil {
		return
	}
	newHosts := make([]string, 0, len(partition.Hosts)-1)
	newPeers := make([]proto.Peer, 0, len(partition.Hosts)-1)
	newLearners := make([]proto.Learner, 0)
	for _, host := range partition.Hosts {
		if host == removePeer.Addr {
			continue
		}
		newHosts = append(newHosts, host)
	}
	for _, peer := range partition.Peers {
		if peer.IsEqual(removePeer) {
			continue
		}
		newPeers = append(newPeers, peer)
	}
	for _, learner := range partition.Learners {
		if learner.Addr == removePeer.Addr && learner.ID == removePeer.ID {
			isLearner = true
			pmConfig = learner.PmConfig
			continue
		}
		newLearners = append(newLearners, learner)
	}
	partition.Lock()
	if err = partition.persistToRocksDB("removeMetaPartitionRaftMember", partition.volName, newHosts, newPeers, newLearners, partition.Recorders, c); err != nil {
		partition.Unlock()
		return
	}
	partition.Unlock()
	if mr.Addr != removePeer.Addr {
		return
	}
	metaNode, err := c.metaNode(partition.Hosts[0])
	if err != nil {
		return
	}
	if err = partition.tryToChangeLeader(c, metaNode); err != nil {
		return
	}
	return
}

func (c *Cluster) removeMetaPartitionRaftOnly(partition *MetaPartition, removePeer proto.Peer,
	createTaskFunc func(removePeer proto.Peer) (t *proto.AdminTask)) (err error) {

	partition.Lock()
	defer partition.Unlock()
	t := createTaskFunc(removePeer)
	var leaderMetaNode *MetaNode
	//cycle send to addresses because the leader info is out of date
	for _, host := range partition.Hosts {
		if leaderMetaNode, err = c.metaNode(host); err != nil {
			return
		}
		t.OperatorAddr = host
		_, err = leaderMetaNode.Sender.syncSendAdminTask(t)
		if err == nil {
			break
		}
		time.Sleep(retrySendSyncTaskInternal)
	}
	if err != nil {
		log.LogErrorf("action[removeMetaPartitionRaftOnly] pid[%v] removePeer[%v] err[%v]", partition.PartitionID, removePeer, err)
	}
	return
}

func (c *Cluster) updateMetaPartitionOfflinePeerIDWithLock(mp *MetaPartition, peerID uint64) (err error) {
	mp.Lock()
	defer mp.Unlock()
	mp.OfflinePeerID = peerID
	if err = mp.persistToRocksDB("updateMetaPartitionOfflinePeerIDWithLock", mp.volName, mp.Hosts, mp.Peers, mp.Learners, mp.Recorders, c); err != nil {
		return
	}
	return
}
func (c *Cluster) addMetaReplica(partition *MetaPartition, addr string, storeMode proto.StoreMode) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[addMetaReplica], vol[%v], meta partition[%v], err[%v]", partition.volName, partition.PartitionID, err)
		}
	}()
	volCrossRegionHAType, err := c.getVolCrossRegionHAType(partition.volName)
	if err != nil {
		return
	}
	partition.Lock()
	defer partition.Unlock()
	if partition.hasPeer(addr) {
		err = fmt.Errorf("vol[%v],mp[%v] has contains host[%v]", partition.volName, partition.PartitionID, addr)
		return
	}
	metaNode, err := c.metaNode(addr)
	if err != nil {
		return
	}
	addPeer := proto.Peer{ID: metaNode.ID, Addr: addr}
	if err = c.addMetaPartitionRaftMember(partition, addPeer, partition.createTaskToAddRaftMember); err != nil {
		return
	}
	newHosts := make([]string, 0, len(partition.Hosts)+1)
	newPeers := make([]proto.Peer, 0, len(partition.Peers)+1)
	if IsCrossRegionHATypeQuorum(volCrossRegionHAType) {
		if newHosts, _, err = partition.getNewHostsWithAddedPeer(c, addPeer.Addr); err != nil {
			return
		}
	} else {
		newHosts = append(partition.Hosts, addPeer.Addr)
	}
	newPeers = append(partition.Peers, addPeer)
	if err = partition.persistToRocksDB("addMetaReplica", partition.volName, newHosts, newPeers, partition.Learners, partition.Recorders, c); err != nil {
		return
	}
	if err = c.createMetaReplica(partition, addPeer, storeMode); err != nil {
		return
	}
	if err = partition.afterCreation(addPeer.Addr, c, storeMode); err != nil {
		return
	}
	return
}

func (c *Cluster) addMetaReplicaLearner(partition *MetaPartition, addr string, autoProm bool, threshold uint8, isNeedIncreaseMPLearnerNum bool, storeMode proto.StoreMode) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[addMetaReplicaLearner], vol[%v], meta partition[%v], err[%v]", partition.volName, partition.PartitionID, err)
		}
	}()

	var (
		volCrossRegionHAType proto.CrossRegionHAType
		volMPLearnerNum      uint8
		oldMPLearnerNum      uint8
		isLearnerNumChanged  bool
	)
	vol, err := c.getVol(partition.volName)
	volCrossRegionHAType = vol.CrossRegionHAType
	volMPLearnerNum = vol.mpLearnerNum
	if err != nil {
		return
	}
	partition.Lock()
	defer partition.Unlock()
	if partition.hasPeer(addr) {
		err = fmt.Errorf("vol[%v], mp[%v] has contains peer[%v]", partition.volName, partition.PartitionID, addr)
		return
	}
	metaNode, err := c.metaNode(addr)
	if err != nil {
		return
	}
	addLearner := proto.Learner{ID: metaNode.ID, Addr: addr, PmConfig: &proto.PromoteConfig{AutoProm: autoProm, PromThreshold: threshold}}
	addPeer := proto.Peer{ID: metaNode.ID, Addr: addr}
	if err = c.addMetaPartitionRaftLearner(partition, addLearner); err != nil {
		return
	}
	newHosts := make([]string, 0, len(partition.Hosts)+1)
	newPeers := make([]proto.Peer, 0, len(partition.Peers)+1)
	newLearners := make([]proto.Learner, 0, len(partition.Learners)+1)
	if IsCrossRegionHATypeQuorum(volCrossRegionHAType) {
		if newHosts, _, err = partition.getNewHostsWithAddedPeer(c, addPeer.Addr); err != nil {
			return
		}
	} else {
		newHosts = append(partition.Hosts, addLearner.Addr)
	}
	newPeers = append(partition.Peers, addPeer)
	newLearners = append(partition.Learners, addLearner)

	oldMPLearnerNum = partition.LearnerNum
	if isNeedIncreaseMPLearnerNum {
		newLearnerNum := uint8(len(newLearners))
		if partition.LearnerNum < newLearnerNum && newLearnerNum <= volMPLearnerNum {
			isLearnerNumChanged = true
			partition.LearnerNum = newLearnerNum
		}
	}
	if err = partition.persistToRocksDB("addMetaReplicaLearner", partition.volName, newHosts, newPeers, newLearners, partition.Recorders, c); err != nil {
		if isLearnerNumChanged {
			partition.LearnerNum = oldMPLearnerNum
		}
		return
	}
	if err = c.createMetaReplica(partition, addPeer, storeMode); err != nil {
		return
	}
	if err = partition.afterCreation(addPeer.Addr, c, storeMode); err != nil {
		return
	}
	return
}

func (c *Cluster) promoteMetaReplicaLearner(partition *MetaPartition, addr string) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[promoteMetaReplicaLearner], vol[%v], meta partition[%v], err[%v]", partition.volName, partition.PartitionID, err)
		}
	}()
	partition.Lock()
	defer partition.Unlock()
	if !contains(partition.Hosts, addr) {
		err = fmt.Errorf("vol[%v], mp[%v] has not contain host[%v]", partition.volName, partition.PartitionID, addr)
		return
	}
	metaNode, err := c.metaNode(addr)
	if err != nil {
		return
	}
	isLearnerExist := false
	promoteLearner := proto.Learner{ID: metaNode.ID, Addr: addr}
	for _, learner := range partition.Learners {
		if learner.ID == metaNode.ID {
			isLearnerExist = true
			promoteLearner.PmConfig = learner.PmConfig
			break
		}
	}
	if !isLearnerExist {
		err = fmt.Errorf("vol[%v], mp[%v] has not contain learner[%v]", partition.volName, partition.PartitionID, addr)
		return
	}
	if err = c.promoteMetaPartitionRaftLearner(partition, promoteLearner); err != nil {
		return
	}
	newLearners := make([]proto.Learner, 0)
	for _, learner := range partition.Learners {
		if learner.ID == promoteLearner.ID {
			continue
		}
		newLearners = append(newLearners, learner)
	}
	if err = partition.persistToRocksDB("promoteMetaReplicaLearner", partition.volName, partition.Hosts, partition.Peers, newLearners, partition.Recorders, c); err != nil {
		return
	}
	return
}

func (c *Cluster) addMetaRecorder(partition *MetaPartition, addr string) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[addMetaRecorder], vol[%v], meta partition[%v], err[%v]", partition.volName, partition.PartitionID, err)
		}
	}()

	partition.Lock()
	defer partition.Unlock()
	if partition.hasPeer(addr) {
		err = fmt.Errorf("vol[%v],mp[%v] has contains host[%v]", partition.volName, partition.PartitionID, addr)
		return
	}
	metaNode, err := c.metaNode(addr)
	if err != nil {
		return
	}
	addPeer := proto.Peer{ID: metaNode.ID, Addr: addr, Type: proto.PeerRecorder}
	if err = c.addMetaPartitionRaftMember(partition, addPeer, partition.createTaskToAddRaftRecorder); err != nil {
		return
	}
	newPeers := make([]proto.Peer, 0, len(partition.Peers)+1)
	newPeers = append(partition.Peers, addPeer)
	newRecorders := make([]string, 0, len(partition.Recorders)+1)
	newRecorders = append(partition.Recorders, addr)
	if err = partition.persistToRocksDB("addMetaRecorder", partition.volName, partition.Hosts, newPeers, partition.Learners, newRecorders, c); err != nil {
		return
	}
	if err = c.syncCreateMetaRecorderToMetaNode(addPeer.Addr, partition); err != nil {
		return
	}
	if err = partition.createRecorderView(addPeer.Addr, c); err != nil {
		return
	}
	return
}

func (c *Cluster) deleteMetaRecorder(partition *MetaPartition, addr string, validate, migrationMode bool) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[deleteMetaRecorder],vol[%v], meta partition[%v],err[%v]", partition.volName, partition.PartitionID, err)
		}
	}()
	if validate {
		if err = c.validateDecommissionMetaPartition(partition, addr); err != nil {
			return
		}
	}
	var metaNode *MetaNode
	if metaNode, err = c.metaNode(addr); err != nil {
		return
	}
	removePeer := proto.Peer{ID: metaNode.ID, Addr: addr, Type: proto.PeerRecorder}
	if err = c.removeMetaPartitionRaftRecorder(partition, removePeer, migrationMode); err != nil {
		return
	}
	if err = c.deleteMetaRecorderFromNode(partition, metaNode, migrationMode); err != nil {
		return
	}
	return
}

func (c *Cluster) removeMetaPartitionRaftRecorder(partition *MetaPartition, removePeer proto.Peer, migrationMode bool) (err error) {
	defer func() {
		if err1 := c.updateMetaPartitionOfflinePeerIDWithLock(partition, 0); err1 != nil {
			err = errors.Trace(err, "updateMetaPartitionOfflinePeerIDWithLock failed, err[%v]", err1)
		}
	}()
	var (
		mr	*MetaReplica
		t	*proto.AdminTask
	)
	if err = c.updateMetaPartitionOfflinePeerIDWithLock(partition, removePeer.ID); err != nil {
		return
	}
	mr, err = partition.getMetaReplicaLeader()
	if err != nil {
		return
	}
	t, err = partition.createTaskToRemoveRaftRecorder(removePeer)
	if err != nil {
		return
	}
	t.ReserveResource = migrationMode
	var leaderMetaNode *MetaNode
	leaderMetaNode = mr.metaNode
	if leaderMetaNode == nil {
		leaderMetaNode, err = c.metaNode(mr.Addr)
		if err != nil {
			return
		}
	}
	if _, err = leaderMetaNode.Sender.syncSendAdminTask(t); err != nil {
		return
	}
	newPeers := make([]proto.Peer, 0, len(partition.Peers)-1)
	newRecorders := make([]string, 0, len(partition.Recorders))
	for _, peer := range partition.Peers {
		if peer.IsEqual(removePeer) {
			continue
		}
		newPeers = append(newPeers, peer)
	}
	for _, recorder := range partition.Recorders {
		if recorder == removePeer.Addr {
			continue
		}
		newRecorders = append(newRecorders, recorder)
	}
	partition.Lock()
	if err = partition.persistToRocksDB("removeMetaPartitionRaftRecorder", partition.volName, partition.Hosts, newPeers, partition.Learners, newRecorders, c); err != nil {
		partition.Unlock()
		return
	}
	partition.Unlock()
	return
}

func (c *Cluster) deleteMetaRecorderFromNode(partition *MetaPartition, removeMetaNode *MetaNode, migrationMode bool) (err error) {
	var mr *MetaRecorder
	partition.Lock()
	mr, err = partition.getMetaRecorder(removeMetaNode.Addr)
	if err != nil {
		log.LogErrorf("action[deleteMetaRecorderFromNode] vol[%v] meta partition[%v],err[%v]", partition.volName, partition.PartitionID, err)
		mr = newMetaRecorder(removeMetaNode)
	}
	task := mr.createTaskToDeleteRecorder(partition.PartitionID)
	partition.removeRecorderByAddr(removeMetaNode.Addr)
	partition.removeMissingReplica(removeMetaNode.Addr)
	partition.Unlock()
	if migrationMode {
		return
	}
	_, err = removeMetaNode.Sender.syncSendAdminTask(task)
	if err != nil {
		log.LogErrorf("action[deleteMetaRecorderFromNode] vol[%v],meta partition[%v],err[%v]", partition.volName, partition.PartitionID, err)
	}
	return nil
}

func (c *Cluster) createMetaReplica(partition *MetaPartition, addPeer proto.Peer, storeMode proto.StoreMode) (err error) {
	task, err := partition.createTaskToCreateReplica(addPeer.Addr, storeMode)
	if err != nil {
		return
	}
	metaNode, err := c.metaNode(addPeer.Addr)
	if err != nil {
		return
	}
	if _, err = metaNode.Sender.syncSendAdminTask(task); err != nil {
		return
	}
	return
}

func (c *Cluster) buildAddMetaPartitionRaftMemberTaskAndSyncSend(mp *MetaPartition, addPeer proto.Peer, leaderAddr string,
	createTaskFunc func(proto.Peer, string) (*proto.AdminTask, error)) (resp *proto.Packet, err error) {
	defer func() {
		var resultCode uint8
		if resp != nil {
			resultCode = resp.ResultCode
		}
		if err != nil {
			log.LogErrorf("action[addMetaRaftMemberAndSend],vol[%v],meta partition[%v],resultCode[%v],err[%v]", mp.volName, mp.PartitionID, resultCode, err)
		} else {
			log.LogWarnf("action[addMetaRaftMemberAndSend],vol[%v],meta partition[%v],resultCode[%v]", mp.volName, mp.PartitionID, resultCode)
		}
	}()
	var t *proto.AdminTask
	t, err = createTaskFunc(addPeer, leaderAddr)
	if err != nil {
		return
	}
	leaderMetaNode, err := c.metaNode(leaderAddr)
	if err != nil {
		return
	}
	if resp, err = leaderMetaNode.Sender.syncSendAdminTask(t); err != nil {
		return
	}
	return
}

func (c *Cluster) buildAddMetaPartitionRaftLearnerTaskAndSyncSend(mp *MetaPartition, addLearner proto.Learner, leaderAddr string) (resp *proto.Packet, err error) {
	defer func() {
		var resultCode uint8
		if resp != nil {
			resultCode = resp.ResultCode
		}
		log.LogErrorf("action[addMetaRaftLearnerAndSend],vol[%v],meta partition[%v],resultCode[%v],err[%v]", mp.volName, mp.PartitionID, resultCode, err)
	}()
	t, err := mp.createTaskToAddRaftLearner(addLearner, leaderAddr)
	if err != nil {
		return
	}
	leaderMetaNode, err := c.metaNode(leaderAddr)
	if err != nil {
		return
	}
	if resp, err = leaderMetaNode.Sender.syncSendAdminTask(t); err != nil {
		return
	}
	return
}

func (c *Cluster) buildPromoteMetaPartitionRaftLearnerTaskAndSyncSend(mp *MetaPartition, promoteLearner proto.Learner, leaderAddr string) (resp *proto.Packet, err error) {
	defer func() {
		var resultCode uint8
		if resp != nil {
			resultCode = resp.ResultCode
		}
		log.LogErrorf("action[promoteMetaRaftLearnerAndSend],vol[%v],meta partition[%v],resultCode[%v],err[%v]", mp.volName, mp.PartitionID, resultCode, err)
	}()
	t, err := mp.createTaskToPromoteRaftLearner(promoteLearner, leaderAddr)
	if err != nil {
		return
	}
	leaderMetaNode, err := c.metaNode(leaderAddr)
	if err != nil {
		return
	}
	if resp, err = leaderMetaNode.Sender.syncSendAdminTask(t); err != nil {
		return
	}
	return
}

func (c *Cluster) addMetaPartitionRaftMember(partition *MetaPartition, addPeer proto.Peer,
	createTaskFunc func(proto.Peer, string) (*proto.AdminTask, error) ) (err error) {

	var (
		candidateAddrs []string
		leaderAddr     string
	)
	candidateAddrs = make([]string, 0, len(partition.Hosts))
	leaderMr, err := partition.getMetaReplicaLeader()
	if err == nil {
		leaderAddr = leaderMr.Addr
		if contains(partition.Hosts, leaderAddr) {
			candidateAddrs = append(candidateAddrs, leaderAddr)
		} else {
			leaderAddr = ""
		}
	}
	for _, host := range partition.Hosts {
		if host == leaderAddr {
			continue
		}
		candidateAddrs = append(candidateAddrs, host)
	}
	//send task to leader addr first,if need to retry,then send to other addr
	for index, host := range candidateAddrs {
		//wait for a new leader
		if leaderAddr == "" && len(candidateAddrs) < int(partition.ReplicaNum) {
			time.Sleep(retrySendSyncTaskInternal)
		}
		_, err = c.buildAddMetaPartitionRaftMemberTaskAndSyncSend(partition, addPeer, host, createTaskFunc)
		if err == nil {
			break
		}
		if index < len(candidateAddrs)-1 {
			time.Sleep(retrySendSyncTaskInternal)
		}
	}
	return
}

func (c *Cluster) addMetaPartitionRaftLearner(partition *MetaPartition, addLearner proto.Learner) (err error) {

	var (
		candidateAddrs []string
		leaderAddr     string
	)
	candidateAddrs = make([]string, 0, len(partition.Hosts))
	leaderMr, err := partition.getMetaReplicaLeader()
	if err == nil {
		leaderAddr = leaderMr.Addr
		if contains(partition.Hosts, leaderAddr) {
			candidateAddrs = append(candidateAddrs, leaderAddr)
		} else {
			leaderAddr = ""
		}
	}
	for _, host := range partition.Hosts {
		if host == leaderAddr {
			continue
		}
		candidateAddrs = append(candidateAddrs, host)
	}
	//send task to leader addr first,if need to retry,then send to other addr
	for index, host := range candidateAddrs {
		//wait for a new leader
		if leaderAddr == "" && len(candidateAddrs) < int(partition.ReplicaNum) {
			time.Sleep(retrySendSyncTaskInternal)
		}
		_, err = c.buildAddMetaPartitionRaftLearnerTaskAndSyncSend(partition, addLearner, host)
		if err == nil {
			break
		}
		if index < len(candidateAddrs)-1 {
			time.Sleep(retrySendSyncTaskInternal)
		}
	}
	return
}

func (c *Cluster) promoteMetaPartitionRaftLearner(partition *MetaPartition, addLearner proto.Learner) (err error) {

	var (
		candidateAddrs []string
		leaderAddr     string
	)
	candidateAddrs = make([]string, 0, len(partition.Hosts))
	leaderMr, err := partition.getMetaReplicaLeader()
	if err == nil {
		leaderAddr = leaderMr.Addr
		if contains(partition.Hosts, leaderAddr) {
			candidateAddrs = append(candidateAddrs, leaderAddr)
		} else {
			leaderAddr = ""
		}
	}
	for _, host := range partition.Hosts {
		if host == leaderAddr {
			continue
		}
		candidateAddrs = append(candidateAddrs, host)
	}
	//send task to leader addr first,if need to retry,then send to other addr
	for index, host := range candidateAddrs {
		//wait for a new leader
		if leaderAddr == "" && len(candidateAddrs) < int(partition.ReplicaNum) {
			time.Sleep(retrySendSyncTaskInternal)
		}
		_, err = c.buildPromoteMetaPartitionRaftLearnerTaskAndSyncSend(partition, addLearner, host)
		if err == nil {
			break
		}
		if index < len(candidateAddrs)-1 {
			time.Sleep(retrySendSyncTaskInternal)
		}
	}
	return
}

func (c *Cluster) loadMetaPartitionAndCheckResponse(mp *MetaPartition) {
	go func() {
		c.doLoadMetaPartition(mp)
	}()
}

func (c *Cluster) doLoadMetaPartition(mp *MetaPartition) {
	var wg sync.WaitGroup
	mp.Lock()
	hosts := make([]string, len(mp.Hosts))
	copy(hosts, mp.Hosts)
	mp.LoadResponse = make([]*proto.MetaPartitionLoadResponse, 0)
	mp.Unlock()
	errChannel := make(chan error, len(hosts))
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer func() {
				wg.Done()
			}()
			mp.RLock()
			mr, err := mp.getMetaReplica(host)
			if err != nil {
				mp.RUnlock()
				errChannel <- err
				return
			}
			mp.RUnlock()
			task := mr.createTaskToLoadMetaPartition(mp.PartitionID)
			response, err := mr.metaNode.Sender.syncSendAdminTask(task)
			if err != nil {
				errChannel <- err
				return
			}
			loadResponse := &proto.MetaPartitionLoadResponse{}
			if err = json.Unmarshal(response.Data, loadResponse); err != nil {
				errChannel <- err
				return
			}
			loadResponse.Addr = host
			mp.addOrReplaceLoadResponse(loadResponse)
		}(host)
	}
	wg.Wait()
	select {
	case err := <-errChannel:
		msg := fmt.Sprintf("action[doLoadMetaPartition] vol[%v],mpID[%v],err[%v]", mp.volName, mp.PartitionID, err.Error())
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyMpLoadFailed], msg)
		return
	default:
	}
	mp.checkSnapshot(c.Name)
}

func (c *Cluster) doLoadDataPartition(dp *DataPartition) {
	log.LogInfo(fmt.Sprintf("action[doLoadDataPartition],partitionID:%v", dp.PartitionID))
	if !dp.needsToCompareCRC() {
		log.LogInfo(fmt.Sprintf("action[doLoadDataPartition],partitionID:%v isRecover[%v] don't need compare", dp.PartitionID, dp.isRecover))
		return
	}
	dp.resetFilesWithMissingReplica()
	loadTasks := dp.createLoadTasks()
	c.addDataNodeTasks(loadTasks)
	for i := 0; i < timeToWaitForResponse; i++ {
		if c.leaderHasChanged() {
			return
		}
		if dp.checkLoadResponse(c.cfg.DataPartitionTimeOutSec) {
			log.LogDebugf("action[checkLoadResponse]  all replica has responded,partitionID:%v ", dp.PartitionID)
			break
		}
		time.Sleep(time.Second)
	}

	if dp.checkLoadResponse(c.cfg.DataPartitionTimeOutSec) == false {
		return
	}

}

func (c *Cluster) handleMetaNodeTaskResponse(nodeAddr string, task *proto.AdminTask) (err error) {
	if task == nil {
		return
	}
	log.LogDebugf(fmt.Sprintf("action[handleMetaNodeTaskResponse] receive Task response:%v from %v", task.ID, nodeAddr))
	var (
		metaNode *MetaNode
	)

	if metaNode, err = c.metaNode(nodeAddr); err != nil {
		goto errHandler
	}
	metaNode.Sender.DelTask(task)
	if err = unmarshalTaskResponse(task); err != nil {
		goto errHandler
	}

	switch task.OpCode {
	case proto.OpMetaNodeHeartbeat:
		response := task.Response.(*proto.MetaNodeHeartbeatResponse)
		err = c.dealMetaNodeHeartbeatResp(task.OperatorAddr, response)
	case proto.OpDeleteMetaPartition:
		response := task.Response.(*proto.DeleteMetaPartitionResponse)
		err = c.dealDeleteMetaPartitionResp(task.OperatorAddr, response)
	case proto.OpUpdateMetaPartition:
		response := task.Response.(*proto.UpdateMetaPartitionResponse)
		err = c.dealUpdateMetaPartitionResp(task.OperatorAddr, response)
	default:
		err := fmt.Errorf("unknown operate code %v", task.OpCode)
		log.LogError(err)
	}

	if err != nil {
		log.LogErrorf("process task:%v failed,status:%v,err:%v ", task.ID, task.Status, err)
	} else {
		log.LogInfof("process task:%v status:%v success", task.ID, task.Status)
	}
	return
errHandler:
	log.LogError(fmt.Sprintf("action[handleMetaNodeTaskResponse],nodeAddr %v,taskId %v,err %v",
		nodeAddr, task.ID, err.Error()))
	return
}

func (c *Cluster) dealUpdateMetaPartitionResp(nodeAddr string, resp *proto.UpdateMetaPartitionResponse) (err error) {
	if resp.Status == proto.TaskFailed {
		msg := fmt.Sprintf("action[dealUpdateMetaPartitionResp],clusterID[%v] nodeAddr %v update meta partition failed,err %v",
			c.Name, nodeAddr, resp.Result)
		log.LogError(msg)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyAdminTaskException], msg)
	}
	return
}

func (c *Cluster) dealDeleteMetaPartitionResp(nodeAddr string, resp *proto.DeleteMetaPartitionResponse) (err error) {
	if resp.Status == proto.TaskFailed {
		msg := fmt.Sprintf("action[dealDeleteMetaPartitionResp],clusterID[%v] nodeAddr %v "+
			"delete meta partition failed,err %v", c.Name, nodeAddr, resp.Result)
		log.LogError(msg)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyAdminTaskException], msg)
		return
	}
	var mr *MetaReplica
	mp, err := c.getMetaPartitionByID(resp.PartitionID)
	if err != nil {
		goto errHandler
	}
	mp.Lock()
	defer mp.Unlock()
	if mr, err = mp.getMetaReplica(nodeAddr); err != nil {
		goto errHandler
	}
	mp.removeReplica(mr)
	return

errHandler:
	log.LogError(fmt.Sprintf("dealDeleteMetaPartitionResp %v", err))
	return
}

func (c *Cluster) dealMetaNodeHeartbeatResp(nodeAddr string, resp *proto.MetaNodeHeartbeatResponse) (err error) {
	var (
		metaNode *MetaNode
		logMsg   string
	)
	log.LogInfof("action[dealMetaNodeHeartbeatResp],clusterID[%v] receive nodeAddr[%v] heartbeat MpReportsCount[%v] MrReportsCount[%v]",
		c.Name, nodeAddr, len(resp.MetaPartitionReports), len(resp.MetaRecorderReports))
	if resp.Status == proto.TaskFailed {
		msg := fmt.Sprintf("action[dealMetaNodeHeartbeatResp],clusterID[%v] nodeAddr %v heartbeat failed,err %v",
			c.Name, nodeAddr, resp.Result)
		log.LogError(msg)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyNodeHeartbeat], msg)
		return
	}

	if metaNode, err = c.metaNode(nodeAddr); err != nil {
		goto errHandler
	}

	if metaNode.ToBeOffline {
		return
	}
	if resp.ZoneName == "" {
		resp.ZoneName = DefaultZoneName
	}

	metaNode.Version = resp.Version

	if metaNode.ZoneName != resp.ZoneName {
		c.t.deleteMetaNode(metaNode)
		oldZoneName := metaNode.ZoneName
		metaNode.ZoneName = resp.ZoneName
		c.adjustMetaNode(metaNode)
		log.LogWarnf("metaNode zone changed from [%v] to [%v]", oldZoneName, resp.ZoneName)
	}
	metaNode.updateMetric(resp, c.cfg.MetaNodeThreshold, c.cfg.MetaNodeRocksdbDiskThreshold, c.cfg.MetaNodeMemModeRocksdbDiskThreshold)
	metaNode.setNodeActive()
	metaNode.updateRocksdbDisks(resp)

	if err = c.t.putMetaNode(metaNode); err != nil {
		log.LogErrorf("action[dealMetaNodeHeartbeatResp],metaNode[%v] error[%v]", metaNode.Addr, err)
	}
	c.updateMetaNode(metaNode, resp.MetaPartitionReports, resp.MetaRecorderReports, metaNode.reachesThreshold())
	metaNode.metaPartitionInfos = nil
	metaNode.metaRecorderInfos = nil
	logMsg = fmt.Sprintf("action[dealMetaNodeHeartbeatResp],metaNode:%v,zone[%v], ReportTime:%v  success", metaNode.Addr, metaNode.ZoneName, time.Now().Unix())
	log.LogInfof(logMsg)
	return
errHandler:
	logMsg = fmt.Sprintf("nodeAddr %v heartbeat error :%v", nodeAddr, errors.Stack(err))
	log.LogError(logMsg)
	return
}

func (c *Cluster) adjustMetaNode(metaNode *MetaNode) {
	c.mnMutex.Lock()
	defer c.mnMutex.Unlock()
	oldNodeSetID := metaNode.NodeSetID
	var err error
	defer func() {
		if err != nil {
			err = fmt.Errorf("action[adjustMetaNode],clusterID[%v] addr:%v,zone[%v] err:%v ", c.Name, metaNode.Addr, metaNode.ZoneName, err.Error())
			log.LogError(errors.Stack(err))
			WarnBySpecialKey(gAlarmKeyMap[alarmKeyAdjustNodeSet], err.Error())
		}
	}()
	var zone *Zone
	zone, err = c.t.getZone(metaNode.ZoneName)
	if err != nil {
		zone = newZone(metaNode.ZoneName)
		c.t.putZone(zone)
	}
	ns := zone.getAvailNodeSetForMetaNode()
	if ns == nil {
		if ns, err = zone.createNodeSet(c); err != nil {
			return
		}
	}

	metaNode.NodeSetID = ns.ID
	if err = c.syncUpdateMetaNode(metaNode); err != nil {
		metaNode.NodeSetID = oldNodeSetID
		return
	}
	if err = c.syncUpdateNodeSet(ns); err != nil {
		return
	}
	err = c.t.putMetaNode(metaNode)
	return
}

func (c *Cluster) handleDataNodeTaskResponse(nodeAddr string, task *proto.AdminTask) {
	if task == nil {
		log.LogInfof("action[handleDataNodeTaskResponse] receive addr[%v] task response,but task is nil", nodeAddr)
		return
	}
	log.LogDebugf("action[handleDataNodeTaskResponse] receive addr[%v] task response:%v", nodeAddr, task.ToString())
	var (
		err      error
		dataNode *DataNode
	)

	if dataNode, err = c.dataNode(nodeAddr); err != nil {
		goto errHandler
	}
	dataNode.TaskManager.DelTask(task)
	if err = unmarshalTaskResponse(task); err != nil {
		goto errHandler
	}

	switch task.OpCode {
	case proto.OpDeleteDataPartition:
		response := task.Response.(*proto.DeleteDataPartitionResponse)
		err = c.dealDeleteDataPartitionResponse(task.OperatorAddr, response)
	case proto.OpLoadDataPartition:
		response := task.Response.(*proto.LoadDataPartitionResponse)
		err = c.handleResponseToLoadDataPartition(task.OperatorAddr, response)
	case proto.OpDataNodeHeartbeat:
		response := task.Response.(*proto.DataNodeHeartbeatResponse)
		err = c.handleDataNodeHeartbeatResp(task.OperatorAddr, response)
	default:
		err = fmt.Errorf(fmt.Sprintf("unknown operate code %v", task.OpCode))
		goto errHandler
	}

	if err != nil {
		goto errHandler
	}
	return

errHandler:
	log.LogErrorf("process task[%v] failed,status:%v, err:%v", task.ID, task.Status, err)
	return
}

func (c *Cluster) dealDeleteDataPartitionResponse(nodeAddr string, resp *proto.DeleteDataPartitionResponse) (err error) {
	var (
		dp *DataPartition
	)
	if resp.Status == proto.TaskSucceeds {
		if dp, err = c.getDataPartitionByID(resp.PartitionId); err != nil {
			return
		}
		dp.Lock()
		defer dp.Unlock()
		dp.removeReplicaByAddr(nodeAddr)

	} else {
		msg := fmt.Sprintf("clusterID[%v] delete data partition[%v] failed,err[%v]", c.Name, nodeAddr, resp.Result)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyAdminTaskException], msg)
	}

	return
}

func (c *Cluster) handleResponseToLoadDataPartition(nodeAddr string, resp *proto.LoadDataPartitionResponse) (err error) {
	if resp.Status == proto.TaskFailed || resp.PartitionSnapshot == nil {
		return
	}
	var (
		dataNode *DataNode
		dp       *DataPartition
		vol      *Vol
	)
	if dataNode, err = c.dataNode(nodeAddr); err != nil {
		return
	}
	if resp.VolName != "" {
		vol, err = c.getVol(resp.VolName)
		if err != nil {
			return
		}
		dp, err = vol.getDataPartitionByID(resp.PartitionId)
	} else {
		dp, err = c.getDataPartitionByID(resp.PartitionId)
	}
	if err != nil {
		return
	}
	dp.loadFile(dataNode, resp)

	return
}

func (c *Cluster) handleDataNodeHeartbeatResp(nodeAddr string, resp *proto.DataNodeHeartbeatResponse) (err error) {

	var (
		dataNode *DataNode
		logMsg   string
	)
	log.LogInfof("action[handleDataNodeHeartbeatResp] clusterID[%v] receive dataNode[%v] heartbeat, ", c.Name, nodeAddr)
	if resp.Status != proto.TaskSucceeds {
		msg := fmt.Sprintf("action[handleDataNodeHeartbeatResp] clusterID[%v] dataNode[%v] heartbeat task failed",
			c.Name, nodeAddr)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyNodeHeartbeat], msg)
		return
	}

	if dataNode, err = c.dataNode(nodeAddr); err != nil {
		goto errHandler
	}
	dataNode.Version = resp.Version
	if dataNode.ToBeOffline {
		return
	}
	if resp.ZoneName == "" {
		resp.ZoneName = DefaultZoneName
	}
	if dataNode.ZoneName != resp.ZoneName {
		c.t.deleteDataNode(dataNode)
		oldZoneName := dataNode.ZoneName
		dataNode.ZoneName = resp.ZoneName
		zone, zoneErr := c.t.getZone(dataNode.ZoneName)
		if zoneErr == nil {
			dataNode.MType = zone.MType.String()
		}
		c.adjustDataNode(dataNode)
		log.LogWarnf("dataNode zone changed from [%v] to [%v]", oldZoneName, resp.ZoneName)
	}

	dataNode.updateNodeMetric(resp)

	if err = c.t.putDataNode(dataNode); err != nil {
		log.LogErrorf("action[handleDataNodeHeartbeatResp] dataNode[%v],zone[%v],node set[%v], err[%v]", dataNode.Addr, dataNode.ZoneName, dataNode.NodeSetID, err)
	}
	c.updateDataNode(dataNode, resp.PartitionReports)
	if dataNode.BadDisks != nil && len(dataNode.BadDisks) > 0 {
		c.DataNodeBadDisks.Store(dataNode.Addr, dataNode.BadDisks)
	} else {
		c.DataNodeBadDisks.Delete(dataNode.Addr)
	}
	logMsg = fmt.Sprintf("action[handleDataNodeHeartbeatResp],dataNode:%v,zone[%v], ReportTime:%v  success", dataNode.Addr, dataNode.ZoneName, time.Now().Unix())
	log.LogInfof(logMsg)
	return
errHandler:
	logMsg = fmt.Sprintf("nodeAddr %v heartbeat error :%v", nodeAddr, err.Error())
	log.LogError(logMsg)
	return
}

func (c *Cluster) adjustDataNode(dataNode *DataNode) {
	c.dnMutex.Lock()
	defer c.dnMutex.Unlock()
	oldNodeSetID := dataNode.NodeSetID
	var err error
	defer func() {
		if err != nil {
			err = fmt.Errorf("action[adjustDataNode],clusterID[%v] dataNodeAddr:%v,zone[%v] err:%v ", c.Name, dataNode.Addr, dataNode.ZoneName, err.Error())
			log.LogError(errors.Stack(err))
			WarnBySpecialKey(gAlarmKeyMap[alarmKeyAdjustNodeSet], err.Error())
		}
	}()
	var zone *Zone
	zone, err = c.t.getZone(dataNode.ZoneName)
	if err != nil {
		zone = newZone(dataNode.ZoneName)
		c.t.putZone(zone)
	}
	ns := zone.getAvailNodeSetForDataNode()
	if ns == nil {
		if ns, err = zone.createNodeSet(c); err != nil {
			return
		}
	}

	dataNode.NodeSetID = ns.ID
	if err = c.syncUpdateDataNode(dataNode); err != nil {
		dataNode.NodeSetID = oldNodeSetID
		return
	}
	if err = c.syncUpdateNodeSet(ns); err != nil {
		return
	}
	err = c.t.putDataNode(dataNode)
	return
}

/*if node report data partition infos,so range data partition infos,then update data partition info*/
func (c *Cluster) updateDataNode(dataNode *DataNode, dps []*proto.PartitionReport) {
	for _, vr := range dps {
		if vr == nil {
			continue
		}
		if vr.VolName != "" {
			vol, err := c.getVol(vr.VolName)
			if err != nil {
				if dp, err1 := c.getDataPartitionByID(vr.PartitionID); err1 == nil {
					dp.updateMetric(vr, dataNode, c)
				}
				continue
			}
			if vol.isRealDelete(c.cfg.DeleteMarkDelVolInterval) {
				continue
			}
			if dp, err := vol.getDataPartitionByID(vr.PartitionID); err == nil {
				dp.updateMetric(vr, dataNode, c)
			}
		} else {
			if dp, err := c.getDataPartitionByID(vr.PartitionID); err == nil {
				dp.updateMetric(vr, dataNode, c)
			}
		}
	}
}

func (c *Cluster) updateMetaNode(metaNode *MetaNode, metaPartitions []*proto.MetaPartitionReport, metaRecorders []*proto.MetaRecorderReport, threshold bool) {
	var (
		vol *Vol
		err error
	)
	var getMp = func(volName string, partitionID uint64) *MetaPartition {
		var mp *MetaPartition
		if volName != "" {
			vol, err = c.getVol(volName)
			if err != nil {
				if mp, err = c.getMetaPartitionByID(partitionID); err == nil {
					return mp
				}
				return nil
			}
			if vol.isRealDelete(c.cfg.DeleteMarkDelVolInterval) {
				return nil
			}
			mp, err = vol.metaPartition(partitionID)
			if err != nil {
				return nil
			}
			return mp
		} else {
			mp, err = c.getMetaPartitionByID(partitionID)
			if err != nil {
				return nil
			}
			return mp
		}
	}
	for _, mr := range metaPartitions {
		if mr == nil {
			continue
		}
		if mp := getMp(mr.VolName, mr.PartitionID); mp != nil {
			mp.updateMetric(mr, metaNode, c, threshold)
		}
	}
	for _, mr := range metaRecorders {
		if mr == nil {
			continue
		}
		if mp := getMp(mr.VolName, mr.PartitionID); mp != nil {
			mp.updateMetaRecorder(mr, metaNode)
		}
	}
}

func (mp *MetaPartition) updateMetric(mr *proto.MetaPartitionReport, metaNode *MetaNode, c *Cluster, threshold bool) {

	//send latest end to replica
	if mr.End != mp.End {
		log.LogDebugf("[updateMetaNode] different end, PartitionID(%v) Addr(%s) master(%v) metaNode(%v)",
			mp.PartitionID, metaNode.Addr, mp.End, mr.End)
		mp.updateMetaPartitionReplicaEnd(c)
	}
	mp.updateMetaPartition(mr, metaNode)
	c.removePromotedLearners(mp, mr.IsLearner, metaNode.ID)
	c.updateInodeIDUpperBound(mp, mr, threshold, metaNode)
}

func (c *Cluster) updateInodeIDUpperBound(mp *MetaPartition, mr *proto.MetaPartitionReport, hasArriveThreshold bool, metaNode *MetaNode) (err error) {
	if !hasArriveThreshold {
		return
	}
	ctx := c.buildCreateMetaPartitionContext()
	var vol *Vol
	if vol, err = c.getVol(mp.volName); err != nil {
		log.LogWarnf("action[updateInodeIDUpperBound] vol[%v] not found", mp.volName)
		return
	}
	maxPartitionID := vol.maxPartitionID()
	if mr.PartitionID != maxPartitionID {
		return
	}
	end := mp.calculateEnd(vol.MpSplitStep)
	log.LogWarnf("mpId[%v],start[%v],end[%v],addr[%v],used[%v]", mp.PartitionID, mp.Start, mp.End, metaNode.Addr, metaNode.Used)
	if err = vol.splitMetaPartition(c, mp, end, ctx); err != nil {
		log.LogErrorf("action[updateInodeIDUpperBound] splits failed,volume[%v],mp[%v],step[%v],err:%v", mp.volName, mp.PartitionID, vol.MpSplitStep, err)
	}
	return
}

func (c *Cluster) adjustNodeSetForDataNode(zoneName, dataNodeAddr string, sourceID, targetID uint64) (err error) {
	var (
		source, target *nodeSet
		dataNode       *DataNode
		zone           *Zone
	)
	if zone, err = c.t.getZone(zoneName); err != nil {
		return
	}
	if source, err = zone.getNodeSet(sourceID); err != nil {
		return
	}
	if source.dataNodeCount() == 0 {
		err = fmt.Errorf("action[adjustNodeSetForDataNode] space err, source count is 0")
		return
	}
	if target, err = zone.getNodeSet(targetID); err != nil {
		return
	}
	if source.zoneName != target.zoneName || sourceID == targetID {
		err = fmt.Errorf("action[adjustNodeSetForDataNode] zoneName[%v,%v] not same or sourceID[%v] equal to targetID[%v]",
			source.zoneName, target.zoneName, sourceID, targetID)
		return
	}
	// select one node from source node set
	if dataNodeAddr == "" {
		source.dataNodes.Range(func(key, value interface{}) bool {
			dataNodeAddr = key.(string)
			return false
		})
	}
	if dataNode, err = zone.getDataNode(dataNodeAddr); err != nil {
		return
	}
	// start merge
	zone.nsLock.Lock()
	defer zone.nsLock.Unlock()
	if dataNode.NodeSetID != source.ID {
		err = fmt.Errorf("action[adjustNodeSetForDataNode] dataNode NodeSetID[%v] and sourceID[%v] not equal", dataNode.NodeSetID, source.ID)
		return
	}
	targetNodeCount := target.dataNodeCount()
	if targetNodeCount >= target.Capacity {
		err = fmt.Errorf("action[adjustNodeSetForDataNode] target count[%v] no more space for new node", targetNodeCount)
		return
	}
	oldNodeSetID := dataNode.NodeSetID
	dataNode.NodeSetID = target.ID
	if err = c.syncUpdateDataNode(dataNode); err != nil {
		dataNode.NodeSetID = oldNodeSetID
		err = fmt.Errorf("action[adjustNodeSetForDataNode] syncUpdateDataNode node[%v] err[%v]", dataNode.Addr, err)
		return
	}
	source.deleteDataNode(dataNode)
	target.putDataNode(dataNode)
	return
}

func (c *Cluster) adjustNodeSetForMetaNode(zoneName, metaNodeAddr string, sourceID, targetID uint64) (err error) {
	var (
		source, target *nodeSet
		metaNode       *MetaNode
		zone           *Zone
	)
	if zone, err = c.t.getZone(zoneName); err != nil {
		return
	}
	if source, err = zone.getNodeSet(sourceID); err != nil {
		return
	}
	if source.metaNodeCount() == 0 {
		err = fmt.Errorf("action[adjustNodeSetForMetaNode] space err, source count is 0")
		return
	}
	if target, err = zone.getNodeSet(targetID); err != nil {
		return
	}
	if source.zoneName != target.zoneName || sourceID == targetID {
		err = fmt.Errorf("action[adjustNodeSetForMetaNode] zoneName[%v,%v] not same or sourceID[%v] equal to targetID[%v]",
			source.zoneName, target.zoneName, sourceID, targetID)
		return
	}
	// select one node from source node set
	if metaNodeAddr == "" {
		source.metaNodes.Range(func(key, value interface{}) bool {
			metaNodeAddr = key.(string)
			return false
		})
	}
	if metaNode, err = zone.getMetaNode(metaNodeAddr); err != nil {
		return
	}
	// start merge
	zone.nsLock.Lock()
	defer zone.nsLock.Unlock()
	if metaNode.NodeSetID != source.ID {
		err = fmt.Errorf("action[adjustNodeSetForMetaNode] metaNode NodeSetID[%v] and sourceID[%v] not equal", metaNode.NodeSetID, source.ID)
		return
	}
	targetNodeCount := target.metaNodeCount()
	if targetNodeCount >= target.Capacity {
		err = fmt.Errorf("action[adjustNodeSetForMetaNode] target count[%v] no more space for new node", targetNodeCount)
		return
	}
	oldNodeSetID := metaNode.NodeSetID
	metaNode.NodeSetID = target.ID
	if err = c.syncUpdateMetaNode(metaNode); err != nil {
		metaNode.NodeSetID = oldNodeSetID
		err = fmt.Errorf("action[adjustNodeSetForMetaNode] syncUpdateMetaNode node[%v] err[%v]", metaNode.Addr, err)
		return
	}
	source.deleteMetaNode(metaNode)
	target.putMetaNode(metaNode)
	return
}

func (c *Cluster) removePromotedLearners(mp *MetaPartition, isLearner bool, nodeID uint64) {
	mp.Lock()
	defer mp.Unlock()
	if !isLearner {
		// learner had been promoted, remove the learner
		index := -1
		for i, learner := range mp.Learners {
			if learner.ID == nodeID {
				index = i
				break
			}
		}
		if index != -1 {
			newLearners := append(mp.Learners[:index], mp.Learners[index+1:]...)
			oldLearners := make([]proto.Learner, len(mp.Learners))
			copy(oldLearners, mp.Learners)
			mp.Learners = newLearners
			if err := c.syncUpdateMetaPartition(mp); err != nil {
				mp.Learners = oldLearners
				log.LogErrorf("mp[%v] auto remove learner [nodeID: %v] err: persist to rocksDB err [%v]", mp.PartitionID, nodeID, err)
				return
			}
		}
	}
}
