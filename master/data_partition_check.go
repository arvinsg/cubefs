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
	"fmt"
	"math"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
)

func (partition *DataPartition) checkStatus(clusterName string, needLog bool, dpTimeOutSec int64, dpWriteableThreshold float64,
	crossRegionHAType proto.CrossRegionHAType, c *Cluster, quorum int) {
	partition.Lock()
	defer partition.Unlock()
	if partition.isRecover || partition.IsFrozen {
		partition.Status = proto.ReadOnly
		return
	}
	liveReplicas := partition.getLiveReplicasFromHosts(dpTimeOutSec)
	if len(partition.Replicas) > len(partition.Hosts) {
		partition.Status = proto.ReadOnly
		msg := fmt.Sprintf("action[extractStatus],partitionID:%v has exceed repica, replicaNum:%v  liveReplicas:%v   Status:%v  RocksDBHost:%v ",
			partition.PartitionID, partition.ReplicaNum, len(liveReplicas), partition.Status, partition.Hosts)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpCheckStatus], msg)
		return
	}
	if partition.IsManual {
		partition.Status = proto.ReadOnly
	} else if IsCrossRegionHATypeQuorum(crossRegionHAType) {
		partition.SetCrossRegionQuorumVolStatus(liveReplicas, dpWriteableThreshold, c, quorum)
	} else {
		partition.Status = proto.ReadOnly
		if len(liveReplicas) >= (int)(partition.ReplicaNum) {
			if partition.checkReplicaStatusOnLiveNode(liveReplicas) == true &&
				partition.canWrite() && partition.canResetStatusToWrite(dpWriteableThreshold) {
				partition.Status = proto.ReadWrite
			}
		}
	}
	if partition.Status != partition.lastStatus {
		partition.lastModifyStatusTime = time.Now().Unix()
		partition.lastStatus = partition.Status
	}
	if partition.isOffline {
		return
	}
	if needLog == true && len(liveReplicas) != int(partition.ReplicaNum) {
		liveHosts := make([]string, 0, len(liveReplicas))
		for _, r := range liveReplicas {
			liveHosts = append(liveHosts, r.Addr)
		}
		msg := fmt.Sprintf("action[extractStatus],partitionID:%v  replicaNum:%v  liveReplicas:%v   Status:%v  RocksDBHost:%v,liveHosts:%v ",
			partition.PartitionID, partition.ReplicaNum, len(liveReplicas), partition.Status, partition.Hosts, liveHosts)
		log.LogInfo(msg)
		if time.Now().Unix()-partition.lastWarnTime > intervalToWarnDataPartition {
			WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpCheckStatus], msg)
			partition.lastWarnTime = time.Now().Unix()
		}
	}
}

func (partition *DataPartition) checkTransferStatus(dpTimeOutSec int64, dpWriteableThreshold float64, crossRegionHAType proto.CrossRegionHAType, c *Cluster, quorum int) {
	partition.Lock()
	defer partition.Unlock()
	log.LogInfof("checkTransferStatus, dpTimeOutSec: %v, dpWriteableThreshold: %v, crossRegionHAType: %v, partitionID: %v",
		dpTimeOutSec, dpWriteableThreshold, crossRegionHAType, partition.PartitionID)

	if !partition.isFrozen() {
		return
	}

	liveReplicas := partition.getLiveReplicasFromHosts(dpTimeOutSec)
	log.LogInfof("checkTransferStatus, partitionID: %v, liveReplicasLength: %v, liveReplicas: %v",
		partition.PartitionID, len(liveReplicas), liveReplicas)

	if len(partition.Replicas) > len(partition.Hosts) {
		partition.TransferStatus = proto.ReadOnly
		log.LogInfof("checkTransferStatus, Replicas more then hosts, partitionID: %v, liveReplicas: %v",
			partition.PartitionID, liveReplicas)
		return
	}
	if partition.IsManual {
		partition.TransferStatus = proto.ReadOnly
	} else if IsCrossRegionHATypeQuorum(crossRegionHAType) {
		partition.SetCrossRegionQuorumVolTransferStatus(liveReplicas, dpWriteableThreshold, c, quorum)
	} else {
		switch len(liveReplicas) {
		case (int)(partition.ReplicaNum):
			partition.TransferStatus = proto.ReadOnly
			log.LogInfof("checkTransferStatus, partitionID: %v, checkReplicaStatusOnLiveNode: %v, canWrite: %v, canResetStatusToWrite: %v",
				partition.PartitionID, partition.checkReplicaStatusOnLiveNode(liveReplicas), partition.canWrite(), partition.canResetStatusToWrite(dpWriteableThreshold))

			if partition.checkReplicaStatusOnLiveNode(liveReplicas) == true &&
				partition.canWrite() && partition.canResetStatusToWrite(dpWriteableThreshold) {
				partition.TransferStatus = proto.ReadWrite
			}
		default:
			partition.TransferStatus = proto.ReadOnly
		}
	}
}

func (partition *DataPartition) SetCrossRegionQuorumVolStatus(liveReplicas []*DataReplica, dpWriteableThreshold float64, c *Cluster, quorum int) {
	var (
		masterRegionHosts []string
		err               error
	)
	partition.Status = proto.ReadOnly
	if quorum > maxQuorumVolDataPartitionReplicaNum || quorum < defaultQuorumDataPartitionMasterRegionCount {
		quorum = defaultQuorumDataPartitionMasterRegionCount
	}
	if len(partition.Hosts) >= maxQuorumVolDataPartitionReplicaNum {
		masterRegionHosts = partition.Hosts[:quorum]
	} else {
		if masterRegionHosts, _, err = c.getMasterAndSlaveRegionAddrsFromDataNodeAddrs(partition.Hosts); err != nil {
			msg := fmt.Sprintf("action[SetCrossRegionQuorumVolStatus] partitionID[%v] hosts[%v],err[%v]",
				partition.PartitionID, partition.Hosts, err)
			WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpCheckStatus], msg)
			return
		}
	}
	if partition.isAllMasterRegionReplicasWritable(liveReplicas, masterRegionHosts, quorum) &&
		partition.canWrite() && partition.canResetStatusToWrite(dpWriteableThreshold) {
		partition.Status = proto.ReadWrite
	}
}

func (partition *DataPartition) SetCrossRegionQuorumVolTransferStatus(liveReplicas []*DataReplica, dpWriteableThreshold float64, c *Cluster, quorum int) {
	var (
		masterRegionHosts []string
		err               error
	)
	partition.TransferStatus = proto.ReadOnly
	if quorum > maxQuorumVolDataPartitionReplicaNum || quorum < defaultQuorumDataPartitionMasterRegionCount {
		quorum = defaultQuorumDataPartitionMasterRegionCount
	}
	if len(partition.Hosts) >= maxQuorumVolDataPartitionReplicaNum {
		masterRegionHosts = partition.Hosts[:quorum]
	} else {
		if masterRegionHosts, _, err = c.getMasterAndSlaveRegionAddrsFromDataNodeAddrs(partition.Hosts); err != nil {
			msg := fmt.Sprintf("action[SetCrossRegionQuorumVolTransferStatus] partitionID[%v] hosts[%v],err[%v]",
				partition.PartitionID, partition.Hosts, err)
			Warn(c.Name, msg)
			return
		}
	}
	if partition.isAllMasterRegionReplicasWritable(liveReplicas, masterRegionHosts, quorum) &&
		partition.canWrite() && partition.canResetStatusToWrite(dpWriteableThreshold) {
		partition.TransferStatus = proto.ReadWrite
	}
}

func (partition *DataPartition) isAllMasterRegionReplicasWritable(liveReplicas []*DataReplica, masterRegionHosts []string, quorum int) bool {
	if len(masterRegionHosts) < quorum {
		return false
	}
	rwReplicaCount := 0
	for _, masterRegionHost := range masterRegionHosts {
		for _, replica := range liveReplicas {
			if replica.Addr == masterRegionHost && replica.Status == proto.ReadWrite {
				rwReplicaCount++
				break
			}
		}
	}
	return rwReplicaCount >= quorum
}

func (partition *DataPartition) canResetStatusToWrite(dpWriteableThreshold float64) bool {
	if dpWriteableThreshold <= defaultMinDpWriteableThreshold {
		return true
	}
	if partition.lastStatus == proto.ReadOnly && partition.hasReplicaReachDiskUsageThreshold(dpWriteableThreshold) {
		return false
	}

	if partition.lastStatus == proto.ReadOnly && time.Now().Unix()-partition.lastModifyStatusTime < 10*60 {
		return false
	}
	return true
}

func (partition *DataPartition) hasReplicaReachDiskUsageThreshold(diskUsageThreshold float64) bool {
	hasReplicaDiskUsageReachThreshold := false
	for _, replica := range partition.Replicas {
		if replica.dataNode == nil || replica.dataNode.DiskInfos == nil {
			break
		}

		if replica.dataNode.isReachThresholdByDisk(replica.DiskPath, diskUsageThreshold) {
			hasReplicaDiskUsageReachThreshold = true
			break
		}
	}
	return hasReplicaDiskUsageReachThreshold
}

func (partition *DataPartition) canWrite() bool {
	avail := partition.total - partition.used
	if int64(avail) > 10*unit.GB {
		return true
	}
	return false
}

func (partition *DataPartition) checkReplicaStatusOnLiveNode(liveReplicas []*DataReplica) (equal bool) {
	for _, replica := range liveReplicas {
		if replica.Status != proto.ReadWrite {
			return
		}
	}

	return true
}

func (partition *DataPartition) checkReplicaStatus(clusterName string, timeOutSec int64) {
	partition.Lock()
	defer partition.Unlock()
	for _, replica := range partition.Replicas {
		if !replica.isLive(timeOutSec) {
			if replica.Status != proto.Unavailable {
				replica.Status = proto.ReadOnly
			}
			if replica.loadFailedByDataNode(3 * timeOutSec) {
				msg := fmt.Sprintf("cluster[%v],vol[%v],dp[%v],replica[%v] load failed by datanode", clusterName, partition.VolName, partition.PartitionID, replica.Addr)
				WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpCheckStatus], msg)
			}
		}
	}
}

func (partition *DataPartition) checkLeader(timeOut int64) {
	partition.Lock()
	defer partition.Unlock()
	for _, dr := range partition.Replicas {
		if !dr.isLive(timeOut) {
			dr.IsLeader = false
		}
	}
	return
}

// Check if there is any missing replica for a data partition.
func (partition *DataPartition) checkMissingReplicas(clusterID, leaderAddr string, dataPartitionMissSec, dataPartitionWarnInterval int64) {
	partition.Lock()
	defer partition.Unlock()
	if partition.isOffline == true {
		return
	}
	for _, replica := range partition.Replicas {
		if partition.hasHost(replica.Addr) && replica.isMissing(dataPartitionMissSec) == true && partition.needToAlarmMissingDataPartition(replica.Addr, dataPartitionWarnInterval) {
			dataNode := replica.getReplicaNode()
			var (
				lastReportTime time.Time
			)
			isActive := true
			if dataNode != nil {
				lastReportTime = dataNode.ReportTime
				isActive = dataNode.isActive
			}
			msg := fmt.Sprintf("action[checkMissErr],clusterID[%v] paritionID:%v  on Node:%v  "+
				"miss time > %v  lastRepostTime:%v   dnodeLastReportTime:%v  nodeisActive:%v So Migrate by manual",
				clusterID, partition.PartitionID, replica.Addr, dataPartitionMissSec, replica.ReportTime, lastReportTime, isActive)
			msg = msg + fmt.Sprintf(" decommissionDataPartitionURL is http://%v/dataPartition/decommission?id=%v&addr=%v", leaderAddr, partition.PartitionID, replica.Addr)
			WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpMissReplica], msg)
		}
	}

	for _, addr := range partition.Hosts {
		if partition.hasMissingDataPartition(addr) == true && partition.needToAlarmMissingDataPartition(addr, dataPartitionWarnInterval) {
			msg := fmt.Sprintf("action[checkMissErr],clusterID[%v] partitionID:%v  on Node:%v  "+
				"miss time  > :%v  but server not exsit So Migrate", clusterID, partition.PartitionID, addr, dataPartitionMissSec)
			msg = msg + fmt.Sprintf(" decommissionDataPartitionURL is http://%v/dataPartition/decommission?id=%v&addr=%v", leaderAddr, partition.PartitionID, addr)
			WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpMissReplica], msg)
		}
	}
}

func (partition *DataPartition) needToAlarmMissingDataPartition(addr string, interval int64) (shouldAlarm bool) {
	t, ok := partition.MissingNodes[addr]
	if !ok {
		partition.MissingNodes[addr] = time.Now().Unix()
		shouldAlarm = true
	} else {
		if time.Now().Unix()-t > interval {
			shouldAlarm = true
			partition.MissingNodes[addr] = time.Now().Unix()
		}
	}

	return
}

func (partition *DataPartition) hasMissingDataPartition(addr string) (isMissing bool) {
	_, ok := partition.hasReplica(addr)

	if ok == false {
		isMissing = true
	}

	return
}

func (partition *DataPartition) checkReplicaDiskError(clusterID, leaderAddr string) (badReplicas map[string]string) {
	partition.Lock()
	defer partition.Unlock()
	for _, addr := range partition.Hosts {
		replica, ok := partition.hasReplica(addr)
		if !ok {
			continue
		}
		if replica.Status == proto.Unavailable {
			if badReplicas == nil {
				badReplicas = make(map[string]string)
			}
			badReplicas[replica.Addr] = replica.DiskPath
		}
	}

	if badReplicas == nil {
		return
	}

	if len(badReplicas) != (int)(partition.ReplicaNum) && len(badReplicas) > 0 {
		partition.Status = proto.ReadOnly
	}

	for addr, diskPath := range badReplicas {
		msg := fmt.Sprintf("action[%v],clusterID[%v],partitionID:%v  replica:%v  disk:%v occurred disk error,so decommission it",
			checkDataPartitionDiskErr, clusterID, partition.PartitionID, addr, diskPath)
		msg = msg + fmt.Sprintf(" decommissionURL is http://%v/dataPartition/decommission?id=%v&addr=%v", leaderAddr, partition.PartitionID, addr)
		log.LogWarn(msg)
	}
	return badReplicas
}

//func (partition *DataPartition) checkDiskError(clusterID, leaderAddr string) (diskErrorAddrs map[string]string) {
//	diskErrorAddrs = make(map[string]string, 0)
//	partition.Lock()
//	defer partition.Unlock()
//	for _, addr := range partition.Hosts {
//		replica, ok := partition.hasReplica(addr)
//		if !ok {
//			continue
//		}
//		if replica.Status == proto.Unavailable {
//			diskErrorAddrs[replica.Addr] = replica.DiskPath
//		}
//	}
//
//	if len(diskErrorAddrs) != (int)(partition.ReplicaNum) && len(diskErrorAddrs) > 0 {
//		partition.Status = proto.ReadOnly
//	}
//
//	for addr, diskPath := range diskErrorAddrs {
//		msg := fmt.Sprintf("action[%v],clusterID[%v],partitionID:%v  On :%v  Disk Error,So Remove it From RocksDBHost",
//			checkDataPartitionDiskErr, clusterID, partition.PartitionID, addr)
//		msg = msg + fmt.Sprintf(" decommissionDiskURL is http://%v/disk/decommission?addr=%v&disk=%v", leaderAddr, addr, diskPath)
//		log.LogWarn(msg)
//	}
//
//	return
//}

func (partition *DataPartition) checkReplicationTask(c *Cluster, dataPartitionSize uint64, dpReplicaNum int) {
	if partition.isOffline == true {
		return
	}
	var msg string
	if excessAddr, excessErr := partition.deleteIllegalReplica(); excessErr != nil {
		msg = fmt.Sprintf("action[%v], partitionID:%v  Excess Replication"+
			" On :%v  Err:%v  rocksDBRecords:%v",
			deleteIllegalReplicaErr, partition.PartitionID, excessAddr, excessErr.Error(), partition.Hosts)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpIllegalReplica], msg)
		dn, _ := c.dataNode(excessAddr)
		if dn != nil && dn.isActive {
			partition.RLock()
			leaderAddr := partition.getLeaderAddr()
			partition.RUnlock()
			if leaderAddr != "" {
				c.removeDataPartitionRaftOnly(partition, proto.Peer{ID: dn.ID, Addr: dn.Addr})
			}
			c.deleteDataReplica(partition, dn, false)
		} else if dn != nil {
			partition.Lock()
			partition.removeReplicaByAddr(dn.Addr)
			partition.Unlock()
		}
	}
	if partition.Status == proto.ReadWrite {
		return
	}

	ms := time.Now().Unix()
	if ms < c.getMetaLoadedTime() || ms-c.getMetaLoadedTime() < 120 {
		return
	}

	if lackAddr, lackErr := partition.missingReplicaAddress(dataPartitionSize); lackErr != nil && !partition.isOffline {
		msg = fmt.Sprintf("action[%v], partitionID:%v  Lack Replication"+
			" On :%v  Err:%v  Hosts:%v  new task to create DataReplica",
			addMissingReplicaErr, partition.PartitionID, lackAddr, lackErr.Error(), partition.Hosts)
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpMissReplica], msg)
		if partition.ReplicaNum == 2 && partition.isRecover && time.Now().Unix()-partition.lastOfflineTime > defaultDecommissionDuration {
			c.decommissionDataPartition(lackAddr, partition, getTargetAddressForDataPartitionDecommission, offlineDataPartitionByCheckReplicationTaskErr, "", "", false)
		}
		return
	}

	if partition.isRecover && len(partition.Hosts) == dpReplicaNum && (time.Now().Unix()-partition.modifyTime > defaultCheckRecoverDuration) && partition.isDataCatchUpInStrictMode() &&
		partition.allReplicaHasRecovered() {
		partition.RLock()
		if partition.isRecover {
			partition.isRecover = false
			c.syncUpdateDataPartition(partition)
			log.LogWarn(fmt.Sprintf("action[checkReplicationTask],partitionID:%v reset isRecover", partition.PartitionID))
		}
		partition.RUnlock()
	}

	return
}

func (partition *DataPartition) deleteIllegalReplica() (excessAddr string, err error) {
	partition.Lock()
	defer partition.Unlock()
	for i := 0; i < len(partition.Replicas); i++ {
		replica := partition.Replicas[i]
		if ok := partition.hasHost(replica.Addr); !ok {
			excessAddr = replica.Addr
			err = proto.ErrIllegalDataReplica
			break
		}
	}
	if !(len(partition.Replicas) >= 2 && len(partition.Hosts) >= int(partition.ReplicaNum)/2) {
		return "", nil
	}
	return
}

func (partition *DataPartition) missingReplicaAddress(dataPartitionSize uint64) (addr string, err error) {
	partition.Lock()
	defer partition.Unlock()

	if time.Now().Unix()-partition.createTime < 120 {
		return
	}

	// go through all the hosts to find the missing replica
	for _, host := range partition.Hosts {
		if _, ok := partition.hasReplica(host); !ok {
			log.LogError(fmt.Sprintf("action[missingReplicaAddress],partitionID:%v lack replication:%v",
				partition.PartitionID, host))
			err = proto.ErrMissingReplica
			addr = host
			break
		}
	}

	return
}

// todo if dp has not loaded finish,don't alarm it
func (partition *DataPartition) checkReplicaSize(diffSpaceUsage uint64) {
	if len(partition.Replicas) == 0 {
		return
	}
	if partition.isRecover {
		return
	}
	diff := 0.0
	sentry := float64(partition.Replicas[0].Used)
	for _, dr := range partition.Replicas {
		temp := math.Abs(float64(dr.Used) - sentry)
		if temp > diff {
			diff = temp
		}
	}

	if diff > float64(diffSpaceUsage) && diff < float64(unit.DefaultDataPartitionSize) {
		msg := fmt.Sprintf("action[checkReplicaSize] vol[%v],partition[%v] difference space usage [%v] larger than %v, ",
			partition.VolName, partition.PartitionID, diff, diffSpaceUsage)
		for _, dr := range partition.Replicas {
			msg = msg + fmt.Sprintf("replica[%v],used[%v];", dr.Addr, dr.Used)
		}
		WarnBySpecialKey(gAlarmKeyMap[alarmKeyDpReplicaSize], msg)
	}
}
