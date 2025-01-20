package topology

import (
	"context"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/multirate"
)

var rateLimitProperties = multirate.Properties{
	{multirate.PropertyTypeOp, strconv.Itoa(int(proto.OpFetchDataPartitionView_))},
}

const (
	sleepForUpdateVolConf                       = time.Second * 1
	intervalToUpdateVolConf                     = time.Minute * 5
	defIntervalMinToFetchDataPartitionView      = 60  //min
	defIntervalSecToForceFetchDataPartitionView = 300 //second
	forceFetchDataPartitionViewChSize           = 1024
	defPostByDomainMaxErrorCount                = 1000
	updateDataPartitionsCountPerBatch           = 128
)

type ForceFetchDataPartition struct {
	volumeName      string
	dataPartitionID uint64
}

type BatchFetchDataPartitionsMap map[string][]uint64

type TopologyManager struct {
	vols                          *sync.Map
	forceFetchDPViewCh            chan *ForceFetchDataPartition
	stopCh                        chan bool
	masterClient                  *master.MasterClient
	masterDomainClient            *master.MasterClient
	masterDomainRequestErrorCount uint64
	needFetchVolAllDPView         bool
	needUpdateVolsConf            bool
	fetchTimerIntervalMin         int64
	forceFetchTimerIntervalSec    int64
}

func NewTopologyManager(fetchTimerIntervalMin, forceFetchTimerIntervalSec int64, masterClient, masterDomainClient *master.MasterClient,
	needFetchVolAllDPView, needUpdateVolsConf bool) *TopologyManager {
	if fetchTimerIntervalMin == 0 {
		fetchTimerIntervalMin = defIntervalMinToFetchDataPartitionView
	}
	if forceFetchTimerIntervalSec == 0 {
		forceFetchTimerIntervalSec = defIntervalSecToForceFetchDataPartitionView
	}
	return &TopologyManager{
		vols:                       new(sync.Map),
		forceFetchDPViewCh:         make(chan *ForceFetchDataPartition, forceFetchDataPartitionViewChSize),
		stopCh:                     make(chan bool),
		fetchTimerIntervalMin:      fetchTimerIntervalMin,
		forceFetchTimerIntervalSec: forceFetchTimerIntervalSec,
		masterClient:               masterClient,
		masterDomainClient:         masterDomainClient,
		needFetchVolAllDPView:      needFetchVolAllDPView,
		needUpdateVolsConf:         needUpdateVolsConf,
	}
}

func (f *TopologyManager) Start() (err error) {
	if err = f.updateVolumeConfSchedule(); err != nil {
		err = fmt.Errorf("TopologyManager Start updateVolumeConfSchedule failed: %v", err)
		return
	}
	go f.backGroundFetchDataPartitions()
	return
}

func (f *TopologyManager) AddVolume(name string) {
	f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	return
}

func (f *TopologyManager) DeleteVolume(name string) {
	f.vols.Delete(name)
}

func (f *TopologyManager) GetVolume(name string) (volumeTopo *VolumeTopologyInfo) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo = value.(*VolumeTopologyInfo)
	return
}

func (f *TopologyManager) FetchDataPartitionView(volName string, dpID uint64) {
	select {
	case f.forceFetchDPViewCh <- &ForceFetchDataPartition{volumeName: volName, dataPartitionID: dpID}:
		log.LogDebugf("ForceFetchDataPartitionView volumeName: %s, dataPartitionID: %v", volName, dpID)
	default:
		log.LogDebugf("ForceFetchDataPartitionView dropInfo, volumeName: %s, dataPartitionID: %v", volName, dpID)
	}
}

// 获取缓存中的partition视图信息
func (f *TopologyManager) GetPartitionFromCache(volName string, dpID uint64) *DataPartition {
	topoInfoValue, ok := f.vols.Load(volName)
	if !ok {
		return nil
	}

	topoInfo := topoInfoValue.(*VolumeTopologyInfo)
	var dataPartitionViewValue interface{}
	dataPartitionViewValue, ok = topoInfo.dataPartitionsView.Load(dpID)
	if !ok {
		return nil
	}
	return dataPartitionViewValue.(*DataPartition)
}

// 缓存中partition的视图信息不存在立即通过接口从master获取一次
func (f *TopologyManager) GetPartition(volName string, dpID uint64) (dataPartition *DataPartition, err error) {
	dataPartition = f.GetPartitionFromCache(volName, dpID)
	if dataPartition != nil {
		return
	}

	return f.GetPartitionFromMaster(volName, dpID)
}

// 调用master接口立即获取一次partition的信息,仅给data node使用
func (f *TopologyManager) GetPartitionFromMaster(volName string, dpID uint64) (dataPartition *DataPartition, err error) {
	_ = multirate.Wait(context.Background(), rateLimitProperties)
	var dataPartitionInfo *proto.DataPartitionInfo
	client := f.masterDomainClient
	if client == nil || len(client.Nodes()) == 0 {
		client = f.masterClient
	}
	dataPartitionInfo, err = client.AdminAPI().GetDataPartition(volName, dpID)
	if err != nil {
		return
	}
	dataPartition = &DataPartition{
		PartitionID: dataPartitionInfo.PartitionID,
		Hosts:       dataPartitionInfo.Hosts,
	}

	value, _ := f.vols.LoadOrStore(volName, NewVolumeTopologyInfo(volName))
	volTopologyInfo := value.(*VolumeTopologyInfo)
	volTopologyInfo.updateDataPartitionsView([]*DataPartition{dataPartition})
	return
}

// 调用master接口立即获取一次partition raft peer的信息,仅给data node使用
func (f *TopologyManager) GetPartitionRaftPeerFromMaster(volName string, dpID uint64) (offlinePeerID uint64, peers []proto.Peer, err error) {
	_ = multirate.Wait(context.Background(), rateLimitProperties)
	var dataPartitionInfo *proto.DataPartitionInfo
	dataPartitionInfo, err = f.masterClient.AdminAPI().GetDataPartition(volName, dpID)
	if err != nil {
		return
	}
	dataPartition := &DataPartition{
		PartitionID: dataPartitionInfo.PartitionID,
		Hosts:       dataPartitionInfo.Hosts,
	}

	value, _ := f.vols.LoadOrStore(volName, NewVolumeTopologyInfo(volName))
	volTopologyInfo := value.(*VolumeTopologyInfo)
	volTopologyInfo.updateDataPartitionsView([]*DataPartition{dataPartition})
	peers = dataPartitionInfo.Peers
	offlinePeerID = dataPartitionInfo.OfflinePeerID
	return
}

func (f *TopologyManager) GetVolConf(name string) *VolumeConfig {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	return volumeTopo.config
}

func (f *TopologyManager) GetBatchDelInodeCntConf(name string) (batchDelInodeCnt uint64) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config == nil {
		batchDelInodeCnt = 0
	} else {
		batchDelInodeCnt = uint64(volumeTopo.config.GetBatchDelInodeCount())
	}
	return
}

func (f *TopologyManager) GetDelInodeIntervalConf(name string) (interval uint64) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config == nil {
		interval = 0
	} else {
		interval = uint64(volumeTopo.config.GetDelInodeInterval())
	}
	return
}

func (f *TopologyManager) GetBitMapAllocatorEnableFlag(name string) (enableState bool, err error) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config == nil {
		err = fmt.Errorf("get vol(%s) enableBitMapAllocator flag failed", name)
	} else {
		enableState = volumeTopo.config.GetEnableBitMapFlag()
	}
	return
}

func (f *TopologyManager) GetCleanTrashItemMaxDurationEachTimeConf(name string) (durationTime int32) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config == nil {
		durationTime = 0
	} else {
		durationTime = volumeTopo.config.GetCleanTrashItemMaxDuration()
	}

	return
}

func (f *TopologyManager) GetCleanTrashItemMaxCountEachTimeConf(name string) (maxCount int32) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config == nil {
		maxCount = 0
	} else {
		maxCount = volumeTopo.config.GetCleanTrashItemMaxCount()
	}

	return
}

func (f *TopologyManager) GetTruncateEKCountConf(name string) (count int) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config == nil {
		count = 0
	} else {
		count = volumeTopo.config.GetTruncateEKCount()
	}
	return
}

func (f *TopologyManager) GetEnableCheckDeleteEKFlag(name string) (flag bool) {
	value, _ := f.vols.LoadOrStore(name, NewVolumeTopologyInfo(name))
	volumeTopo := value.(*VolumeTopologyInfo)
	if volumeTopo.config != nil {
		return volumeTopo.config.GetEnableCheckDeleteEKFlag()
	}
	return
}

func (f *TopologyManager) Stop() {
	close(f.stopCh)
}

func (f *TopologyManager) UpdateFetchTimerIntervalMin(fetchIntervalMin, forceFetchIntervalSec int64) {
	if fetchIntervalMin > 0 && atomic.LoadInt64(&f.fetchTimerIntervalMin) != fetchIntervalMin {
		log.LogDebugf("fetch timer new value: %v Min", fetchIntervalMin)
		atomic.StoreInt64(&f.fetchTimerIntervalMin, fetchIntervalMin)
	}

	if forceFetchIntervalSec > 0 && atomic.LoadInt64(&f.forceFetchTimerIntervalSec) != forceFetchIntervalSec {
		log.LogDebugf("force fetch timer new value: %v Sec", forceFetchIntervalSec)
		atomic.StoreInt64(&f.forceFetchTimerIntervalSec, forceFetchIntervalSec)
	}
}

func (f *TopologyManager) getAllVolumesName() []string {
	allVolsName := make([]string, 0)
	f.vols.Range(func(key, value interface{}) bool {
		allVolsName = append(allVolsName, key.(string))
		return true
	})
	return allVolsName
}

func (f *TopologyManager) updateDataPartitionsViewByResp(volName string, dpsViewResp []*proto.DataPartitionResponse) {
	partitions := make([]*DataPartition, 0, len(dpsViewResp))
	for _, item := range dpsViewResp {
		info := &DataPartition{
			PartitionID:     item.PartitionID,
			Hosts:           item.Hosts,
			EcHosts:         item.EcHosts,
			EcMigrateStatus: item.EcMigrateStatus,
		}
		partitions = append(partitions, info)
		log.LogDebugf("fetch vol(%s) data partition info: %v", volName, info)
	}
	value, _ := f.vols.LoadOrStore(volName, NewVolumeTopologyInfo(volName))
	volTopologyInfo := value.(*VolumeTopologyInfo)
	volTopologyInfo.updateDataPartitionsView(partitions)
}

func (f *TopologyManager) updateDataPartitions(volName string) {
	partitionsInfo, err := f.fetchDataPartitionsView(volName, nil)
	if err != nil {
		log.LogErrorf("fetch vol(%s) data partitions view failed: %v", volName, err)
		if err == proto.ErrVolNotExists {
			f.vols.Delete(volName)
		}
		return
	}
	f.updateDataPartitionsViewByResp(volName, partitionsInfo.DataPartitions)
}

func (f *TopologyManager) updateDataPartitionsInBatches(volName string, dpIDs []uint64) {
	if len(dpIDs) == 0 {
		f.updateDataPartitions(volName)
		return
	}

	start := 0
	for {
		if start >= len(dpIDs) {
			break
		}

		end := start + updateDataPartitionsCountPerBatch
		if end > len(dpIDs) {
			end = len(dpIDs)
		}
		partitionsInfo, err := f.fetchDataPartitionsView(volName, dpIDs[start:end])
		if err != nil {
			log.LogErrorf("fetch vol(%s) data partition(%v) view failed: %v", volName, dpIDs, err)
			if err == proto.ErrVolNotExists {
				f.vols.Delete(volName)
			}
			return
		}
		f.updateDataPartitionsViewByResp(volName, partitionsInfo.DataPartitions)
		start = end
	}
}

func (f *TopologyManager) backGroundFetchDataPartitions() {
	fetchAllTickerValue := atomic.LoadInt64(&f.fetchTimerIntervalMin)
	tickerValue := atomic.LoadInt64(&f.forceFetchTimerIntervalSec)
	fetchAllTicker := time.NewTicker(time.Minute * time.Duration(fetchAllTickerValue))
	ticker := time.NewTicker(time.Second * time.Duration(tickerValue))

	if f.needFetchVolAllDPView {
		allVolsName := f.getAllVolumesName()
		for _, volName := range allVolsName {
			log.LogDebugf("backGroundFetchDataPartitions start fetch volume(%s) view", volName)
			f.updateDataPartitions(volName)
		}
	}

	var needForceFetchDataPartitionsMap = make(map[string]map[uint64]bool, 0)
	for {

		select {
		case <-f.stopCh:
			fetchAllTicker.Stop()
			ticker.Stop()
			return

		case <-fetchAllTicker.C:
			if !f.needFetchVolAllDPView {
				continue
			}

			allVolsName := f.getAllVolumesName()
			for _, volName := range allVolsName {
				log.LogDebugf("backGroundFetchDataPartitions start fetch volume(%s) view", volName)
				f.updateDataPartitions(volName)
			}

			newFetchAllTickerValue := atomic.LoadInt64(&f.fetchTimerIntervalMin)
			if newFetchAllTickerValue > 0 && newFetchAllTickerValue != fetchAllTickerValue {
				log.LogDebugf("backGroundFetchDataPartitions fetch all ticker change from (%v min) to (%v min)",
					fetchAllTickerValue, newFetchAllTickerValue)
				fetchAllTicker.Reset(time.Minute * time.Duration(newFetchAllTickerValue))
				fetchAllTickerValue = newFetchAllTickerValue
			}

		case <-ticker.C:
			var forceFetchInfo = make(BatchFetchDataPartitionsMap, 0)
			for volName, dpsIDMap := range needForceFetchDataPartitionsMap {
				dpsID := make([]uint64, 0, len(dpsIDMap))
				for dpID := range dpsIDMap {
					dpsID = append(dpsID, dpID)
				}
				forceFetchInfo[volName] = dpsID
			}

			for volName, dpsID := range forceFetchInfo {
				log.LogDebugf("backGroundFetchDataPartitions start force fetch volume(%s) dpIDs(%v) view", volName, dpsID)
				f.updateDataPartitionsInBatches(volName, dpsID)
			}
			needForceFetchDataPartitionsMap = make(map[string]map[uint64]bool, 0)

			newTickerValue := atomic.LoadInt64(&f.forceFetchTimerIntervalSec)
			if newTickerValue > 0 && tickerValue != newTickerValue {
				log.LogDebugf("backGroundFetchDataPartitions force fetch ticker change from (%v Sec) to (%v Sec)", tickerValue, newTickerValue)
				ticker.Reset(time.Second * time.Duration(newTickerValue))
				tickerValue = newTickerValue
			}

		case fetchInfo := <-f.forceFetchDPViewCh:
			if _, ok := needForceFetchDataPartitionsMap[fetchInfo.volumeName]; !ok {
				needForceFetchDataPartitionsMap[fetchInfo.volumeName] = make(map[uint64]bool, 0)
			}
			needForceFetchDataPartitionsMap[fetchInfo.volumeName][fetchInfo.dataPartitionID] = true
		}
	}
}

func (f *TopologyManager) updateVolumeConf() (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("updateVolumeConf panic: err(%v) stack(%v)", r, string(debug.Stack()))
		}
	}()
	var volsConf []*proto.VolInfo

	volsConf, err = f.getVolsConf()
	if err != nil {
		log.LogErrorf("updateVolumeConf, err: %v", err)
		return
	}
	if len(volsConf) == 0 {
		return
	}

	for _, volConf := range volsConf {
		value, ok := f.vols.Load(volConf.Name)
		if !ok {
			continue
		}
		volTopo := value.(*VolumeTopologyInfo)
		volTopo.updateVolConf(&VolumeConfig{
			trashDay:                          int32(volConf.TrashRemainingDays),
			childFileMaxCnt:                   volConf.ChildFileMaxCnt,
			trashCleanInterval:                volConf.TrashCleanInterval,
			batchDelInodeCnt:                  volConf.BatchInodeDelCnt,
			delInodeInterval:                  volConf.DelInodeInterval,
			enableBitMapAllocator:             volConf.EnableBitMapAllocator,
			cleanTrashItemMaxDurationEachTime: volConf.CleanTrashMaxDurationEachTime,
			cleanTrashItemMaxCountEachTime:    volConf.CleanTrashMaxCountEachTime,
			enableRemoveDupReq:                volConf.EnableRemoveDupReq,
			reqRecordsReservedTime:            volConf.ReqRecordsReservedTime,
			reqRecordsMaxCount:                volConf.ReqRecordMaxCount,
			truncateEKCount:                   volConf.TruncateEKCountEveryTime,
			bitmapSnapFrozenHour:              volConf.BitMapSnapFrozenHour,
			enableCheckDeleteEK:               volConf.EnableCheckDeleteEK,
			persistenceMode:                   volConf.PersistenceMode,
			volID:                             volConf.VolID,
		})
		log.LogDebugf("updateVolConf: vol: %v, remaining days: %v, childFileMaxCount: %v, trashCleanInterval: %v, "+
			"enableBitMapAllocator: %v, trashCleanMaxDurationEachTime: %v, cleanTrashItemMaxCountEachTime: %v,"+
			" enableRemoveDupReq:%v, reqRecordReservedTime: %vmin, reqRecordMaxCount: %v, batchInodeDelCnt: %v,"+
			" delInodeInterval: %v, truncateEKCountEveryTime: %v, bitmapSnapFrozenHour: %v, enableCheckDeleteEK: %v",
			volConf.Name, volConf.TrashRemainingDays, volConf.ChildFileMaxCnt, volConf.TrashCleanInterval,
			strconv.FormatBool(volConf.EnableBitMapAllocator), volConf.CleanTrashMaxDurationEachTime,
			volConf.CleanTrashMaxCountEachTime, strconv.FormatBool(volConf.EnableRemoveDupReq), volConf.ReqRecordsReservedTime,
			volConf.ReqRecordMaxCount, volConf.BatchInodeDelCnt, volConf.DelInodeInterval, volConf.TruncateEKCountEveryTime,
			volConf.BitMapSnapFrozenHour, strconv.FormatBool(volConf.EnableCheckDeleteEK))
	}
	return
}

func (f *TopologyManager) updateVolumeConfSchedule() (err error) {
	if !f.needUpdateVolsConf {
		return
	}
	for retryCount := 10; retryCount > 0; retryCount-- {
		err = f.updateVolumeConf()
		if err == nil {
			break
		}
		time.Sleep(sleepForUpdateVolConf)
	}
	if err != nil {
		log.LogWarnf("updateVolsConfScheduler, err: %v", err.Error())
		return
	}

	go func() {
		ticker := time.NewTicker(intervalToUpdateVolConf)
		for {
			select {
			case <-f.stopCh:
				ticker.Stop()
				return
			case <-ticker.C:
				_ = f.updateVolumeConf()
			}
		}
	}()
	return
}

func (f *TopologyManager) fetchDataPartitionsView(volumeName string, dpsID []uint64) (dataPartitions *proto.DataPartitionsView, err error) {
	if f.masterDomainClient == nil || len(f.masterDomainClient.Nodes()) == 0 {
		return f.masterClient.ClientAPI().GetDataPartitions(volumeName, dpsID)
	}

	masterDomainRequestErrorCount := atomic.LoadUint64(&f.masterDomainRequestErrorCount)
	if masterDomainRequestErrorCount > defPostByDomainMaxErrorCount && masterDomainRequestErrorCount%100 != 0 {
		atomic.AddUint64(&f.masterDomainRequestErrorCount, 1)
		return f.masterClient.ClientAPI().GetDataPartitions(volumeName, dpsID)
	}

	dataPartitions, err = f.masterDomainClient.ClientAPI().GetDataPartitions(volumeName, dpsID)
	if err != nil {
		atomic.AddUint64(&f.masterDomainRequestErrorCount, 1)
		return
	}
	atomic.StoreUint64(&f.masterDomainRequestErrorCount, 0)
	return
}

func (f *TopologyManager) getVolsConf() (volsConf []*proto.VolInfo, err error) {
	if f.masterDomainClient == nil || len(f.masterDomainClient.Nodes()) == 0 {
		return f.masterClient.AdminAPI().ListVols("")
	}

	domainRequestErrorCount := atomic.LoadUint64(&f.masterDomainRequestErrorCount)
	if domainRequestErrorCount > defPostByDomainMaxErrorCount && domainRequestErrorCount%100 != 0 {
		atomic.AddUint64(&f.masterDomainRequestErrorCount, 1)
		return f.masterClient.AdminAPI().ListVols("")
	}

	volsConf, err = f.masterDomainClient.AdminAPI().ListVols("")
	if err != nil {
		atomic.AddUint64(&f.masterDomainRequestErrorCount, 1)
		return
	}
	atomic.StoreUint64(&f.masterDomainRequestErrorCount, 0)
	return
}
