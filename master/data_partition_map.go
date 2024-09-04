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
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	"runtime"
	"sync"
	"time"

	pb "github.com/gogo/protobuf/proto"
)

// DataPartitionMap stores all the data partitionMap
type DataPartitionMap struct {
	sync.RWMutex
	partitionMap           map[uint64]*DataPartition
	readableAndWritableCnt int    // number of readable and writable partitionMap
	lastLoadedIndex        uint64 // last loaded partition index
	lastReleasedIndex      uint64 // last released partition index
	partitions             []*DataPartition
	responseJsonCache      []byte
	responseProtobufCache  []byte
	volName                string
}

func newDataPartitionMap(volName string) (dpMap *DataPartitionMap) {
	dpMap = new(DataPartitionMap)
	dpMap.partitionMap = make(map[uint64]*DataPartition)
	dpMap.partitions = make([]*DataPartition, 0)
	dpMap.responseJsonCache = make([]byte, 0)
	dpMap.responseProtobufCache = make([]byte, 0)
	dpMap.volName = volName
	return
}

func (dpMap *DataPartitionMap) get(ID uint64) (*DataPartition, error) {
	dpMap.RLock()
	defer dpMap.RUnlock()
	if v, ok := dpMap.partitionMap[ID]; ok {
		return v, nil
	}
	return nil, proto.ErrDataPartitionNotExists
}

func (dpMap *DataPartitionMap) put(dp *DataPartition) {
	dpMap.Lock()
	defer dpMap.Unlock()
	_, ok := dpMap.partitionMap[dp.PartitionID]
	if !ok {
		dpMap.partitions = append(dpMap.partitions, dp)
		dpMap.partitionMap[dp.PartitionID] = dp
		return
	}

	// replace the old partition with dp in the map and array
	dpMap.partitionMap[dp.PartitionID] = dp
	dataPartitions := make([]*DataPartition, 0)
	for index, partition := range dpMap.partitions {
		if partition.PartitionID == dp.PartitionID {
			dataPartitions = append(dataPartitions, dpMap.partitions[:index]...)
			dataPartitions = append(dataPartitions, dp)
			dataPartitions = append(dataPartitions, dpMap.partitions[index+1:]...)
			dpMap.partitions = dataPartitions
			break
		}
	}
}

func (dpMap *DataPartitionMap) del(dp *DataPartition) {
	dpMap.Lock()
	defer dpMap.Unlock()
	delete(dpMap.partitionMap, dp.PartitionID)
	dataPartitions := make([]*DataPartition, 0)
	for index, partition := range dpMap.partitions {
		if partition.PartitionID == dp.PartitionID {
			dataPartitions = append(dataPartitions, dpMap.partitions[:index]...)
			dataPartitions = append(dataPartitions, dpMap.partitions[index+1:]...)
			dpMap.partitions = dataPartitions
			break
		}
	}
}

func (dpMap *DataPartitionMap) setReadWriteDataPartitions(readWrites int) {
	dpMap.Lock()
	defer dpMap.Unlock()
	dpMap.readableAndWritableCnt = readWrites
}

func (dpMap *DataPartitionMap) getDataPartitionResponseJsonCache() []byte {
	dpMap.RLock()
	defer dpMap.RUnlock()
	return dpMap.responseJsonCache
}

func (dpMap *DataPartitionMap) setDataPartitionResponseJsonCache(responseJsonCache []byte) {
	dpMap.Lock()
	defer dpMap.Unlock()
	if responseJsonCache != nil {
		dpMap.responseJsonCache = responseJsonCache
	}
}

func (dpMap *DataPartitionMap) getDataPartitionResponseProtobufCache() []byte {
	dpMap.RLock()
	defer dpMap.RUnlock()
	return dpMap.responseProtobufCache
}

func (dpMap *DataPartitionMap) setDataPartitionResponseProtobufCache(responseProtobufCache []byte) {
	dpMap.Lock()
	defer dpMap.Unlock()
	dpMap.responseProtobufCache = responseProtobufCache
}

func (vol *Vol) updateResponseJsonCache(needsUpdate bool, minPartitionID uint64) (body []byte, err error) {
	dpMap := vol.dataPartitions
	responseJsonCache := dpMap.getDataPartitionResponseJsonCache()
	if responseJsonCache == nil || needsUpdate || len(responseJsonCache) == 0 {
		dpResps := vol.getDataPartitionsResp(minPartitionID)
		if len(dpResps) == 0 {
			log.LogError(fmt.Sprintf("action[updateDpResponseJsonCache],volName[%v] minPartitionID:%v,err:%v",
				dpMap.volName, minPartitionID, proto.ErrNoAvailDataPartition))
			return nil, proto.ErrNoAvailDataPartition
		}
		cv := proto.NewDataPartitionsView()
		cv.DataPartitions = dpResps
		reply := newSuccessHTTPReply(cv)
		if body, err = json.Marshal(reply); err != nil {
			log.LogError(fmt.Sprintf("action[updateDpResponseJsonCache],minPartitionID:%v,err:%v",
				minPartitionID, err.Error()))
			return nil, proto.ErrMarshalData
		}
		dpMap.setDataPartitionResponseJsonCache(body)
		return
	}
	body = make([]byte, len(responseJsonCache))
	copy(body, responseJsonCache)

	return
}

func (vol *Vol) updateResponseProtobufCache(needsUpdate bool, minPartitionID uint64) (body []byte, err error) {
	dpMap := vol.dataPartitions
	responseProtobufCache := dpMap.getDataPartitionResponseProtobufCache()
	if responseProtobufCache == nil || needsUpdate || len(responseProtobufCache) == 0 {
		dpResps := vol.getDataPartitionsResp(minPartitionID)
		if len(dpResps) == 0 {
			log.LogError(fmt.Sprintf("action[updateDpResponseProtobufCache],volName[%v] minPartitionID:%v,err:%v",
				dpMap.volName, minPartitionID, proto.ErrNoAvailDataPartition))
			return nil, proto.ErrNoAvailDataPartition
		}
		cv := proto.NewDataPartitionsView()
		cv.DataPartitions = dpResps
		cvPb := proto.ConvertDataPartitionsView(cv)

		pData := make([]byte, 0)
		if pData, err = pb.Marshal(cvPb); err != nil {
			log.LogError(fmt.Sprintf("action[updateDpResponseProtobufCache],minPartitionID:%v,err:%v",
				minPartitionID, err.Error()))
			return nil, proto.ErrMarshalData
		}

		reply := &proto.HTTPReplyPb{Code: proto.ErrCodeSuccess, Msg: proto.ErrSuc.Error(), Data: pData}
		if body, err = pb.Marshal(reply); err != nil {
			log.LogError(fmt.Sprintf("action[updateDpResponseProtobufCache],minPartitionID:%v,err:%v",
				minPartitionID, err.Error()))
			return nil, proto.ErrMarshalData
		}
		dpMap.setDataPartitionResponseProtobufCache(body)
		return
	}
	body = make([]byte, len(responseProtobufCache))
	copy(body, responseProtobufCache)

	return
}

func (vol *Vol) getDataPartitionsResp(minPartitionID uint64) (dpResps []*proto.DataPartitionResponse) {
	dpMap := vol.dataPartitions
	eps := vol.ecDataPartitions
	dpResps = make([]*proto.DataPartitionResponse, 0)
	dpMap.RLock()
	log.LogDebugf("volName[%v] DataPartitionMapLen[%v],DataPartitionsLen[%v],minPartitionID[%v]",
		dpMap.volName, len(dpMap.partitionMap), len(dpMap.partitions), minPartitionID)
	for _, dp := range dpMap.partitionMap {
		if dp.PartitionID <= minPartitionID {
			continue
		}
		dpResp := dp.convertToDataPartitionResponse()
		if dp.EcMigrateStatus == proto.FinishEC {
			if ecDp, err := eps.get(dp.PartitionID); err == nil {
				ecDp.appendEcInfoToDataPartitionResponse(dpResp)
			}
		}
		dpReplicaNum := int(vol.dpReplicaNum)
		if vol.NeedToLowerReplica && len(dpResp.Hosts) > dpReplicaNum && vol.FollowerRead == false {
			dpResp.Hosts = dpResp.Hosts[0:dpReplicaNum]
		}
		dpResps = append(dpResps, dpResp)
	}
	dpMap.RUnlock()

	eps.RLock()
	defer eps.RUnlock()
	for _, ep := range eps.partitions {
		if ep.DataPartition.PartitionID <= minPartitionID || ep.EcMigrateStatus != proto.OnlyEcExist {
			continue
		}
		dpResp := ep.DataPartition.convertToDataPartitionResponse()
		ep.appendEcInfoToDataPartitionResponse(dpResp)
		dpReplicaNum := int(vol.dpReplicaNum)
		if vol.NeedToLowerReplica && len(dpResp.Hosts) > dpReplicaNum && vol.FollowerRead == false {
			dpResp.Hosts = dpResp.Hosts[0:dpReplicaNum]
		}
		dpResps = append(dpResps, dpResp)
	}

	return
}

func (dpMap *DataPartitionMap) getDataPartitionsViewByIdsResponseProtobuf(eps *EcDataPartitionCache, dpIDs []uint64,
	minPartitionID uint64) (body []byte, err error) {
	dataPartitionsView := dpMap.getDataPartitionsViewByIDs(eps, dpIDs, minPartitionID)
	if len(dataPartitionsView) == 0 {
		log.LogError(fmt.Sprintf("action[getDataPartitionsViewByIdsResponseJson],volName[%v]"+
			" minPartitionID:%v,err:%v", dpMap.volName, minPartitionID, proto.ErrNoAvailDataPartition))
		return nil, proto.ErrNoAvailDataPartition
	}
	cv := proto.NewDataPartitionsView()
	cv.DataPartitions = dataPartitionsView
	cvPb := proto.ConvertDataPartitionsView(cv)

	pData := make([]byte, 0)
	if pData, err = pb.Marshal(cvPb); err != nil {
		log.LogError(fmt.Sprintf("action[updateDpResponseProtobufCache],minPartitionID:%v,err:%v",
			minPartitionID, err.Error()))
		return nil, proto.ErrMarshalData
	}

	reply := &proto.HTTPReplyPb{Code: proto.ErrCodeSuccess, Msg: proto.ErrSuc.Error(), Data: pData}
	if body, err = pb.Marshal(reply); err != nil {
		log.LogError(fmt.Sprintf("action[updateDpResponseProtobufCache],minPartitionID:%v,err:%v",
			minPartitionID, err.Error()))
		return nil, proto.ErrMarshalData
	}
	return
}

func (dpMap *DataPartitionMap) getDataPartitionsViewByIdsResponseJson(eps *EcDataPartitionCache, dpIDs []uint64,
	minPartitionID uint64) (body []byte, err error) {
	dataPartitionsView := dpMap.getDataPartitionsViewByIDs(eps, dpIDs, minPartitionID)
	if len(dataPartitionsView) == 0 {
		log.LogError(fmt.Sprintf("action[getDataPartitionsViewByIdsResponseProtobuf],volName[%v]"+
			" minPartitionID:%v,err:%v", dpMap.volName, minPartitionID, proto.ErrNoAvailDataPartition))
		return nil, proto.ErrNoAvailDataPartition
	}
	cv := proto.NewDataPartitionsView()
	cv.DataPartitions = dataPartitionsView
	reply := newSuccessHTTPReply(cv)
	if body, err = json.Marshal(reply); err != nil {
		log.LogError(fmt.Sprintf("action[updateDpResponseJsonCache],minPartitionID:%v,err:%v",
			minPartitionID, err.Error()))
		return nil, proto.ErrMarshalData
	}
	return
}

func (dpMap *DataPartitionMap) getDataPartitionsViewByIDs(eps *EcDataPartitionCache, dpIDs []uint64, minPartitionID uint64) (dpResps []*proto.DataPartitionResponse) {
	dpResps = make([]*proto.DataPartitionResponse, 0)
	log.LogDebugf("volName[%v] DataPartitionMapLen[%v],DataPartitionsLen[%v],minPartitionID[%v]",
		dpMap.volName, len(dpMap.partitionMap), len(dpMap.partitions), minPartitionID)
	for _, id := range dpIDs {
		if id <= minPartitionID {
			continue
		}
		dp, _ := dpMap.get(id)
		if dp == nil {
			continue
		}
		dpResp := dp.convertToDataPartitionResponse()
		if dp.EcMigrateStatus == proto.FinishEC {
			if ecDp, err := eps.get(dp.PartitionID); err == nil {
				ecDp.appendEcInfoToDataPartitionResponse(dpResp)
			}
		}
		dpResps = append(dpResps, dpResp)
	}

	for _, id := range dpIDs {
		if id <= minPartitionID {
			continue
		}
		ep, _ := eps.get(id)
		if ep == nil {
			continue
		}
		if ep.EcMigrateStatus != proto.OnlyEcExist {
			continue
		}
		dpResp := ep.DataPartition.convertToDataPartitionResponse()
		ep.appendEcInfoToDataPartitionResponse(dpResp)
		dpResps = append(dpResps, dpResp)
	}
	return
}

func (dpMap *DataPartitionMap) getDataPartitionsToBeReleased(numberOfDataPartitionsToFree int, secondsToFreeDataPartitionAfterLoad int64) (partitions []*DataPartition, startIndex uint64) {
	partitions = make([]*DataPartition, 0)
	dpMap.RLock()
	defer dpMap.RUnlock()
	dpLen := len(dpMap.partitions)
	if dpLen == 0 {
		return
	}
	startIndex = dpMap.lastReleasedIndex
	count := numberOfDataPartitionsToFree
	if dpLen < numberOfDataPartitionsToFree {
		count = dpLen
	}
	for i := 0; i < count; i++ {
		if dpMap.lastReleasedIndex >= uint64(dpLen) {
			dpMap.lastReleasedIndex = 0
		}
		dp := dpMap.partitions[dpMap.lastReleasedIndex]
		dpMap.lastReleasedIndex++
		if time.Now().Unix()-dp.LastLoadedTime >= secondsToFreeDataPartitionAfterLoad {
			partitions = append(partitions, dp)
		}
	}

	return
}

func (dpMap *DataPartitionMap) freeMemOccupiedByDataPartitions(partitions []*DataPartition) {
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
					log.LogError(fmt.Sprintf("[%v] freeMemOccupiedByDataPartitions panic %v: %s\n", dpMap.volName, err, buf))
				}
			}()
			dp.releaseDataPartition()
		}(dp)
	}
	wg.Wait()

}

func (dpMap *DataPartitionMap) getDataPartitionsToBeChecked(loadFrequencyTime int64) (partitions []*DataPartition, startIndex uint64) {
	partitions = make([]*DataPartition, 0)
	dpMap.RLock()
	defer dpMap.RUnlock()
	dpLen := len(dpMap.partitions)
	if dpLen == 0 {
		return
	}
	startIndex = dpMap.lastLoadedIndex

	// determine the number of data partitions to load
	count := dpLen / intervalToLoadDataPartition
	if count == 0 {
		count = 1
	}

	for i := 0; i < count; i++ {
		if dpMap.lastLoadedIndex >= (uint64)(len(dpMap.partitions)) {
			dpMap.lastLoadedIndex = 0
		}
		dp := dpMap.partitions[dpMap.lastLoadedIndex]
		dpMap.lastLoadedIndex++

		if time.Now().Unix()-dp.LastLoadedTime >= loadFrequencyTime {
			partitions = append(partitions, dp)
		}
	}

	return
}

func (dpMap *DataPartitionMap) totalUsedSpace() (totalUsed uint64) {
	dpMap.RLock()
	defer dpMap.RUnlock()
	for _, dp := range dpMap.partitions {
		if proto.IsEcFinished(dp.EcMigrateStatus) {
			continue
		}
		totalUsed = totalUsed + dp.getMaxUsedSpace()
	}
	return
}

func (dpMap *DataPartitionMap) setAllDataPartitionsToReadOnly() {
	dpMap.Lock()
	defer dpMap.Unlock()
	for _, dp := range dpMap.partitions {
		dp.Status = proto.ReadOnly
	}
}

func (dpMap *DataPartitionMap) checkBadDiskDataPartitions(diskPath, nodeAddr string) (partitions []*DataPartition) {
	dpMap.RLock()
	defer dpMap.RUnlock()
	partitions = make([]*DataPartition, 0)
	for _, dp := range dpMap.partitionMap {
		if dp.containsBadDisk(diskPath, nodeAddr) {
			partitions = append(partitions, dp)
		}
	}
	return
}

func (vol *Vol) getRWDataPartitionsOfGivenCount(count int, medium string, c *Cluster) (partitions []*DataPartition, err error) {
	minRestRwDpCount := 10
	if vol.getDpCnt() > 100 {
		minRestRwDpCount = 20
	}
	if vol.dataPartitions.readableAndWritableCnt-count <= minRestRwDpCount {
		err = fmt.Errorf("readableAndWritableCnt[%v] count[%v] less than minRestRwDpCount[%v]",
			vol.dataPartitions.readableAndWritableCnt, count, minRestRwDpCount)
		return
	}
	dataPartitionMap := vol.cloneDataPartitionMap()
	partitions = make([]*DataPartition, 0)
	for _, dp := range dataPartitionMap {
		if dp.Status == proto.ReadWrite {
			ok, err1 := dp.isTargetMediumType(medium, c)
			if err1 != nil {
				err = fmt.Errorf("action[getRWDataPartitionsOfGivenCount] err:%v", err1)
				return
			}
			if ok {
				partitions = append(partitions, dp)
			}
		}
		if len(partitions) >= count {
			return
		}
	}
	return
}

func (vol *Vol) getDataPartitionsFromStartIDToEndID(startID, endID uint64, medium string, c *Cluster) (partitions []*DataPartition, err error) {
	dataPartitionMap := vol.cloneDataPartitionMap()
	partitions = make([]*DataPartition, 0)
	for _, dp := range dataPartitionMap {
		if dp.PartitionID >= startID && dp.PartitionID <= endID {
			ok, err1 := dp.isTargetMediumType(medium, c)
			if err1 != nil {
				err = fmt.Errorf("action[getDataPartitionsFromStartIDToEndID] err:%v", err1)
				return
			}
			if ok {
				partitions = append(partitions, dp)
			}
		}
	}
	return
}
