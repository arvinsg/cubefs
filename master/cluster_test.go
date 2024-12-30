package master

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/assert"
)

func buildPanicCluster() *Cluster {
	c := newCluster(server.cluster.Name, server.cluster.leaderInfo, server.cluster.fsm, server.cluster.partition, server.config)
	v := buildPanicVol()
	c.putVol(v)
	return c
}

func buildPanicVol() *Vol {
	id, err := server.cluster.idAlloc.allocateCommonID()
	if err != nil {
		return nil
	}
	var createTime = time.Now().Unix() // record create time of this volume
	vol := newVol(id, commonVol.Name, commonVol.Owner, testZone1+","+testZone2, commonVol.dataPartitionSize, commonVol.Capacity,
		defaultReplicaNum, defaultReplicaNum, false, false, true,
		true, false, false, false, false, createTime, createTime,
		"", "", "", 0, 0, 0, 0, 0.0, 30,
		0, proto.StoreModeMem, proto.VolConvertStInit, proto.MetaPartitionLayout{0, 0},
		strings.Split(testSmartRules, ","), proto.CompactDefault, proto.DpFollowerReadDelayConfig{false, 0},
		0, 0, 0, 0, 0, 0)

	vol.dataPartitions = nil
	return vol
}

func TestCheckDataPartitions(t *testing.T) {
	server.cluster.checkDataPartitions()
}

func TestPanicCheckDataPartitions(t *testing.T) {
	c := buildPanicCluster()
	c.checkDataPartitions()
}

func TestCheckReleaseDataPartitions(t *testing.T) {
	server.cluster.doReleaseDataPartition()
}
func TestPanicCheckReleaseDataPartitions(t *testing.T) {
	c := buildPanicCluster()
	c.doReleaseDataPartition()
}

func TestCheckHeartbeat(t *testing.T) {
	server.cluster.doCheckDataNodeHeartbeat()
	server.cluster.doCheckMetaNodeHeartbeat()
}

func TestCheckMetaPartitions(t *testing.T) {
	server.cluster.doCheckMetaPartitions()
}

func TestPanicCheckMetaPartitions(t *testing.T) {
	c := buildPanicCluster()
	vol, err := c.getVol(commonVolName)
	assert.NoError(t, err)
	partitionID, err := server.cluster.idAlloc.allocateMetaPartitionID()
	assert.NoError(t, err)
	mp := newMetaPartition(partitionID, 1, defaultMaxMetaPartitionInodeID, vol.mpReplicaNum, vol.mpLearnerNum, vol.mpRecorderNum, vol.Name, vol.ID)
	_ = vol.addMetaPartition(mp, c.Name)
	mp = nil
	c.doCheckMetaPartitions()
}

func TestCheckAvailSpace(t *testing.T) {
	server.cluster.doCheckAvailSpace()
}

func TestPanicCheckAvailSpace(t *testing.T) {
	c := buildPanicCluster()
	c.dataNodeStatInfo = nil
	c.doCheckAvailSpace()
}

func TestCheckCreateDataPartitions(t *testing.T) {
	server.cluster.doCheckCreateDataPartitions()
	//time.Sleep(150 * time.Second)
}

func TestPanicCheckCreateDataPartitions(t *testing.T) {
	c := buildPanicCluster()
	c.doCheckCreateDataPartitions()
}

func TestPanicCheckBadDiskRecovery(t *testing.T) {
	c := buildPanicCluster()
	vol, err := c.getVol(commonVolName)
	assert.NoError(t, err)
	partitionID, err := server.cluster.idAlloc.allocateDataPartitionID()
	assert.NoError(t, err)
	dp := newDataPartition(partitionID, vol.dpReplicaNum, vol.Name, vol.ID)
	c.BadDataPartitionIds.Store(fmt.Sprintf("%v", dp.PartitionID), dp)
	c.doCheckBadDiskRecovery()
}

func TestPanicCheckMigratedDataPartitionsRecovery(t *testing.T) {
	c := buildPanicCluster()
	vol, err := c.getVol(commonVolName)
	assert.NoError(t, err)
	partitionID, err := server.cluster.idAlloc.allocateDataPartitionID()
	assert.NoError(t, err)
	dp := newDataPartition(partitionID, vol.dpReplicaNum, vol.Name, vol.ID)
	c.MigratedDataPartitionIds.Store(fmt.Sprintf("%v", dp.PartitionID), dp)
	c.checkMigratedDataPartitionsRecoveryProgress()
}

func TestPanicCheckMigratedMetaPartitionsRecovery(t *testing.T) {
	c := buildPanicCluster()
	vol, err := c.getVol(commonVolName)
	assert.NoError(t, err)
	partitionID, err := server.cluster.idAlloc.allocateMetaPartitionID()
	assert.NoError(t, err)
	mp := newMetaPartition(partitionID, 1, defaultMaxMetaPartitionInodeID, vol.mpReplicaNum, vol.mpLearnerNum, vol.mpRecorderNum, vol.Name, vol.ID)
	_ = vol.addMetaPartition(mp, c.Name)
	c.MigratedMetaPartitionIds.Store(fmt.Sprintf("%v", mp.PartitionID), mp)
	mp = nil
	c.checkMigratedMetaPartitionRecoveryProgress()
}

func TestCheckBadDiskRecovery(t *testing.T) {

	server.cluster.doCheckDataNodeHeartbeat()
	time.Sleep(5 * time.Second)
	vol, err := server.cluster.getVol(commonVolName)
	if !assert.NoError(t, err) {
		return
	}
	vol.RLock()
	dps := make([]*DataPartition, 0)
	for _, dp := range vol.dataPartitions.partitions {
		dps = append(dps, dp)
	}
	dpsMapLen := vol.getDpCnt()
	vol.RUnlock()
	dpsLen := len(dps)
	partitionIdMap := make(map[uint64]bool, dpsLen)
	if !assert.Equalf(t, dpsLen, dpsMapLen, "dpsLen[%v],dpsMapLen[%v]", dpsLen, dpsMapLen) {
		return
	}
	//clear
	server.cluster.BadDataPartitionIds.Range(func(key, value interface{}) bool {
		server.cluster.BadDataPartitionIds.Delete(key)
		return true
	})
	for _, dp := range dps {
		if !dp.allReplicaHasRecovered() || !dp.isDataCatchUp() {
			dpsLen--
			continue
		}
		dp.RLock()
		if len(dp.Replicas) < int(vol.dpReplicaNum) {
			dpsLen--
			dp.RUnlock()
			continue
		}
		addr := dp.Replicas[0].dataNode.Addr
		server.cluster.putBadDataPartitionIDs(dp.Replicas[0], addr, dp.PartitionID)
		partitionIdMap[dp.PartitionID] = true
		dp.RUnlock()
	}
	count := 0
	server.cluster.BadDataPartitionIds.Range(func(key, value interface{}) bool {
		count++
		delete(partitionIdMap, value.(uint64))
		return true
	})
	if !assert.Equalf(t, dpsLen, count, "expect bad partition num[%v],real num[%v]", dpsLen, count) {
		return
	}
	assert.Equal(t, 0, len(partitionIdMap))
	//check recovery
	server.cluster.checkDiskRecoveryProgress()

	count = 0
	server.cluster.BadDataPartitionIds.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	if !assert.Equalf(t, 0, count, "expect bad partition num[0],real num[%v]", count) {
		return
	}
}

func TestPanicCheckBadMetaPartitionRecovery(t *testing.T) {
	c := buildPanicCluster()
	vol, err := c.getVol(commonVolName)
	assert.NoError(t, err)
	partitionID, err := server.cluster.idAlloc.allocateMetaPartitionID()
	assert.NoError(t, err)
	dp := newMetaPartition(partitionID, 0, defaultMaxMetaPartitionInodeID, vol.mpReplicaNum, vol.mpLearnerNum, vol.mpRecorderNum, vol.Name, vol.ID)
	c.BadMetaPartitionIds.Store(fmt.Sprintf("%v", dp.PartitionID), dp)
	c.doCheckMetaPartitionRecoveryProgress()
}

func TestCheckBadMetaPartitionRecovery(t *testing.T) {
	server.cluster.doCheckMetaNodeHeartbeat()
	time.Sleep(5 * time.Second)
	//clear
	server.cluster.BadMetaPartitionIds.Range(func(key, value interface{}) bool {
		server.cluster.BadMetaPartitionIds.Delete(key)
		return true
	})
	vol, err := server.cluster.getVol(commonVolName)
	if !assert.NoError(t, err) {
		return
	}
	vol.RLock()
	mps := make([]*MetaPartition, 0)
	for _, mp := range vol.MetaPartitions {
		mps = append(mps, mp)
	}
	mpsMapLen := len(vol.MetaPartitions)
	partitionIdMap := make(map[uint64]bool, mpsMapLen)
	vol.RUnlock()
	mpsLen := len(mps)
	if !assert.Equalf(t, mpsLen, mpsMapLen, "mpsLen[%v],mpsMapLen[%v]", mpsLen, mpsMapLen) {
		return
	}
	for _, mp := range mps {
		mp.RLock()
		if len(mp.Replicas) == 0 {
			mpsLen--
			mp.RUnlock()
			return
		}
		addr := mp.Replicas[0].metaNode.Addr
		server.cluster.putBadMetaPartitions(addr, mp.PartitionID)
		partitionIdMap[mp.PartitionID] = true
		mp.RUnlock()
	}
	count := 0
	server.cluster.BadMetaPartitionIds.Range(func(key, value interface{}) bool {
		count++
		delete(partitionIdMap, value.(uint64))
		return true
	})

	if !assert.Equalf(t, mpsLen, count, "expect bad partition num[%v],real num[%v]", mpsLen, count) {
		return
	}

	assert.Equal(t, 0, len(partitionIdMap))
	//check recovery
	server.cluster.checkMetaPartitionRecoveryProgress()

	count = 0
	server.cluster.BadMetaPartitionIds.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	if !assert.Equalf(t, 0, count, "expect bad partition num[0],real num[%v]", count) {
		return
	}
}

func TestUpdateInodeIDUpperBound(t *testing.T) {
	vol, err := server.cluster.getVol(commonVolName)
	if !assert.NoError(t, err) {
		return
	}
	maxPartitionID := vol.maxPartitionID()
	vol.RLock()
	mp := vol.MetaPartitions[maxPartitionID]
	mpLen := len(vol.MetaPartitions)
	vol.RUnlock()
	mr := &proto.MetaPartitionReport{
		PartitionID: mp.PartitionID,
		Start:       mp.Start,
		End:         mp.End,
		Status:      int(mp.Status),
		MaxInodeID:  mp.Start + 1,
		IsLeader:    false,
		VolName:     mp.volName,
	}
	metaNode, err := server.cluster.metaNode(mp.Hosts[0])
	if !assert.NoError(t, err) {
		return
	}
	if err = server.cluster.updateInodeIDUpperBound(mp, mr, true, metaNode); !assert.NoError(t, err) {
		return
	}
	curMpLen := len(vol.MetaPartitions)
	assert.NotEqualf(t, mpLen, curMpLen, "split failed,oldMpLen[%v],curMpLen[%v]", mpLen, curMpLen)
}

func TestUpdateDataNodeBadDisks(t *testing.T) {
	c := &Cluster{DataNodeBadDisks: new(sync.Map)}
	addr1 := "192.168.0.31"
	addr2 := "192.168.0.32"
	allBadDisks := make([]map[string][]string, 0)
	dataNodeBadDisksOfVol := make(map[string][]string)
	dataNodeBadDisksOfVol[addr1] = append(dataNodeBadDisksOfVol[addr1], "/diskPath1")
	allBadDisks = append(allBadDisks, dataNodeBadDisksOfVol)
	allBadDisks = append(allBadDisks, dataNodeBadDisksOfVol)

	// one bad disk
	c.updateDataNodeBadDisks(allBadDisks)
	badDiskView := c.getDataNodeBadDisks()
	assert.Equalf(t, 1, len(badDiskView), "getDataNodeBadDisks should be 1 but get :%v detail:%v", len(badDiskView), badDiskView)
	assert.Equalf(t, 1, len(badDiskView[0].BadDiskPath), "getDataNodeBadDisks should be 1 but get :%v detail:%v", len(badDiskView), badDiskView)

	// one datanode with more than one bad disk
	allBadDisks = append(allBadDisks, map[string][]string{addr1: {"/diskPath2"}})
	c.updateDataNodeBadDisks(allBadDisks)
	badDiskView = c.getDataNodeBadDisks()
	assert.Equalf(t, 1, len(badDiskView), "getDataNodeBadDisks should be 1 but get :%v detail:%v", len(badDiskView), badDiskView)
	assert.Equalf(t, 2, len(badDiskView[0].BadDiskPath), "bad disks should be 2 but get :%v detail:%v", len(badDiskView), badDiskView)

	// two datanode
	dataNodeBadDisksOfVol[addr2] = append(dataNodeBadDisksOfVol[addr2], "/diskPath3")
	allBadDisks = append(allBadDisks, map[string][]string{addr2: {"/diskPath3"}})
	c.updateDataNodeBadDisks(allBadDisks)
	badDiskView = c.getDataNodeBadDisks()
	assert.Equalf(t, 2, len(badDiskView), "getDataNodeBadDisks should be 2 but get :%v detail:%v", len(badDiskView), badDiskView)

	// when there is no bad disks
	c.updateDataNodeBadDisks(make([]map[string][]string, 0))
	badDiskView = c.getDataNodeBadDisks()
	assert.Equalf(t, 0, len(badDiskView), "getDataNodeBadDisks should be 0 but get :%v detail:%v", len(badDiskView), badDiskView)
}

func TestBadDataPartitionsContainHasDeletedDp(t *testing.T) {
	c := server.cluster
	var partitionID uint64
	partitionID, _ = c.idAlloc.allocateDataPartitionID()
	c.BadDataPartitionIds = &sync.Map{}
	c.BadDataPartitionIds.Store("", partitionID)
	c.checkDiskRecoveryProgress()
	badDpIds := make([]uint64, 0)
	c.BadDataPartitionIds.Range(func(key, value any) bool {
		badDpIds = append(badDpIds, value.(uint64))
		return true
	})
	assert.Equal(t, 0, len(badDpIds))
}

func TestMigratedDataPartitionsContainHasDeletedDp(t *testing.T) {
	c := server.cluster
	var partitionID uint64
	partitionID, _ = c.idAlloc.allocateDataPartitionID()
	c.MigratedDataPartitionIds = &sync.Map{}
	c.MigratedDataPartitionIds.Store("", partitionID)
	c.checkMigratedDataPartitionsRecoveryProgress()
	badDpIds := make([]uint64, 0)
	c.MigratedDataPartitionIds.Range(func(key, value any) bool {
		badDpIds = append(badDpIds, value.(uint64))
		return true
	})
	assert.Equal(t, 0, len(badDpIds))
}

func TestBadMetaPartitionsContainHasDeletedMp(t *testing.T) {
	c := server.cluster
	var partitionID uint64
	partitionID, _ = c.idAlloc.allocateMetaPartitionID()
	c.BadMetaPartitionIds = &sync.Map{}
	c.BadMetaPartitionIds.Store("", partitionID)
	c.checkMetaPartitionRecoveryProgress()
	badMpIDs := make([]uint64, 0)
	c.BadMetaPartitionIds.Range(func(key, value any) bool {
		badMpIDs = append(badMpIDs, value.(uint64))
		return true
	})
	assert.Equal(t, 0, len(badMpIDs))
}

func TestMigratedMetaPartitionsContainHasDeletedMp(t *testing.T) {
	c := server.cluster
	var partitionID uint64
	partitionID, _ = c.idAlloc.allocateMetaPartitionID()
	c.MigratedMetaPartitionIds = &sync.Map{}
	c.MigratedMetaPartitionIds.Store("", partitionID)
	c.checkMigratedMetaPartitionRecoveryProgress()
	badMpIDs := make([]uint64, 0)
	c.MigratedMetaPartitionIds.Range(func(key, value any) bool {
		badMpIDs = append(badMpIDs, value.(uint64))
		return true
	})
	assert.Equal(t, 0, len(badMpIDs))
}

func TestUpdateServerLimitInfoRespCache(t *testing.T) {
	c := server.cluster
	c.updateServerLimitInfoRespCache()
	data, err := c.getServerLimitInfoRespCache("")
	assert.NoError(t, err)
	assert.NotEmpty(t, data)
	data, err = c.getServerLimitInfoRespCache(testZone1)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)
	c.doCheckVolStatus()
	if c.mustUsedVolLimitInfoRespCache(commonVolName) {
		data, err = c.getVolLimitInfoRespCache(commonVolName)
		assert.NoError(t, err)
		assert.NotEmpty(t, data)
	} else {
		data, err = c.getServerLimitInfoRespCache("")
		assert.NoError(t, err)
		assert.NotEmpty(t, data)
	}
	reqURL := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.AdminGetLimitInfo, commonVolName)
	httpReply := processReturnRawReply(reqURL, t)
	limitInfo := &proto.LimitInfo{}
	err = json.Unmarshal(httpReply.Data, limitInfo)
	assert.NoError(t, err)
}
