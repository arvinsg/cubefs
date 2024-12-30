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

package data

import (
	"fmt"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/common"
	"github.com/cubefs/cubefs/sdk/flash"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/sdk/scheduler"
	"github.com/cubefs/cubefs/util/connpool"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/iputil"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
)

var (
	LocalIP                      string
	MinWriteAbleDataPartitionCnt = 10
	MasterNoCacheAPIRetryTimeout = 5 * time.Minute
	checkRemovedDpTimer          *time.Timer
)

const (
	VolNotExistInterceptThresholdMin = 60 * 24
	VolNotExistClearViewThresholdMin = 0

	RefreshHostLatencyInterval = time.Hour
)

type DataPartitionView struct {
	DataPartitions []*DataPartition
}

// Wrapper TODO rename. This name does not reflect what it is doing.
type Wrapper struct {
	sync.RWMutex
	clusterName           string
	volName               string
	zoneName              string
	masters               []string
	umpJmtpAddr           string
	volNotExistCount      int32
	partitions            *sync.Map //key: dpID; value: *DataPartition
	volCreateTime         string
	followerRead          bool
	followerReadClientCfg bool
	nearRead              bool
	forceROW              bool
	enableWriteCache      bool
	notCacheNode          bool // only initialized for the first time, should not be updated, otherwise will cause chaos
	flock                 bool // only initialized for the first time, should not be updated
	extentCacheExpireSec  int64
	dpSelectorChanged     bool
	dpSelectorName        string
	dpSelectorParm        string
	mc                    *masterSDK.MasterClient
	metaWrapper           *meta.MetaWrapper
	stopOnce              sync.Once
	stopC                 chan struct{}
	wg                    sync.WaitGroup

	dpSelector DataPartitionSelector

	HostsStatus map[string]bool

	crossRegionHAType      proto.CrossRegionHAType
	crossRegionHostLatency sync.Map // key: host, value: ping time
	quorum                 int

	connConfig        *proto.ConnConfig
	volConnConfig     *proto.ConnConfig
	zoneConnConfig    *proto.ConnConfig
	clusterConnConfig *proto.ConnConfig

	schedulerClient        *scheduler.SchedulerClient
	dpMetricsReportDomain  string
	dpMetricsReportConfig  *proto.DpMetricsReportConfig
	dpMetricsRefreshCount  uint
	dpMetricsFetchErrCount uint
	ecEnable               bool

	dpFollowerReadDelayConfig *proto.DpFollowerReadDelayConfig
	dpLowestDelayHostWeight   int

	oldCacheBoostStatus     bool
	enableClusterCacheBoost bool
	enableVolCacheBoost     bool
	enableCacheAutoPrepare  bool
	cacheBoostPath          string
	cacheTTL                int64
	cacheReadTimeoutMs      int64
	remoteCache             *flash.RemoteCache
	HostsDelay              sync.Map
	extentClientType        ExtentClientType
	umpKeyPrefix            string
	getStreamerFunc			func(inode uint64) *Streamer
	readAheadController		*ReadAheadController
	readAheadInitMutex		sync.Mutex
}

type DataState struct {
	ClusterName      string
	LocalIP          string
	VolNotExistCount int32
	VolView          *proto.SimpleVolView
	DpView           *proto.DataPartitionsView
	ClusterView      *proto.ClientClusterConf
}

type connConfigLevel uint8

const (
	defaultConfig connConfigLevel = iota
	volumeConfig
	zoneConfig
	clusterConfig
)

func (level *connConfigLevel) String() string {
	if level == nil {
		return ""
	}
	switch *level {
	case defaultConfig:
		return "default"
	case volumeConfig:
		return "volume"
	case zoneConfig:
		return "zone"
	case clusterConfig:
		return "cluster"
	default:
		return "undefined config level"
	}
}

// NewDataPartitionWrapper returns a new data partition wrapper.
func NewDataPartitionWrapper(volName string, masters []string, extentClientType ExtentClientType, readAheadMemMB, readAheadWindowMB int64, getStreamerFunc func(inode uint64) *Streamer) (w *Wrapper, err error) {
	w = new(Wrapper)
	w.stopC = make(chan struct{})
	w.masters = masters
	w.mc = masterSDK.NewMasterClient(masters, false)
	w.schedulerClient = scheduler.NewSchedulerClient(w.dpMetricsReportDomain, false)
	w.volName = volName
	w.extentClientType = extentClientType
	w.partitions = new(sync.Map)
	w.HostsStatus = make(map[string]bool)
	w.SetDefaultConnConfig()
	w.dpMetricsReportConfig = &proto.DpMetricsReportConfig{
		EnableReport:      false,
		ReportIntervalSec: defaultMetricReportSec,
		FetchIntervalSec:  defaultMetricFetchSec,
	}
	w.dpFollowerReadDelayConfig = &proto.DpFollowerReadDelayConfig{
		EnableCollect:        true,
		DelaySummaryInterval: followerReadDelaySummaryInterval,
	}
	w.getStreamerFunc = getStreamerFunc
	if readAheadMemMB > 0 {
		w.updateReadAheadLocalConfig(readAheadMemMB, readAheadWindowMB)
	}
	if err = w.updateClusterInfo(); err != nil {
		err = errors.Trace(err, "NewDataPartitionWrapper:")
		return
	}
	if err = w.getSimpleVolView(); err != nil {
		err = errors.Trace(err, "NewDataPartitionWrapper:")
		return
	}
	if err = w.initDpSelector(); err != nil {
		log.LogErrorf("NewDataPartitionWrapper: init initDpSelector failed, [%v]", err)
		return
	}
	if err = w.updateDataPartition(true); err != nil {
		err = errors.Trace(err, "NewDataPartitionWrapper:")
		return
	}
	if err = w.updateClientClusterView(); err != nil {
		log.LogErrorf("NewDataPartitionWrapper: init DataNodeStatus failed, [%v]", err)
	}

	err = nil
	StreamConnPoolInitOnce.Do(func() {
		StreamConnPool = connpool.NewConnectPoolWithTimeoutAndCap(0, 10, time.Duration(w.connConfig.IdleTimeoutSec)*time.Second, time.Duration(w.connConfig.ConnectTimeoutNs))
	})

	w.wg.Add(4)
	go w.update()
	go w.updateCrossRegionHostStatus()
	go w.ScheduleDataPartitionMetricsReport()
	go w.dpFollowerReadDelayCollect()

	return
}

func RebuildDataPartitionWrapper(volName string, masters []string, dataState *DataState, extentClientType ExtentClientType, localReadAheadMemMB, localReadAheadWindowMB int64, getStreamerFunc func(inode uint64) *Streamer) (w *Wrapper) {
	w = new(Wrapper)
	w.stopC = make(chan struct{})
	w.masters = masters
	w.mc = masterSDK.NewMasterClient(masters, false)
	w.schedulerClient = scheduler.NewSchedulerClient(w.dpMetricsReportDomain, false)
	w.volName = volName
	w.extentClientType = extentClientType
	w.partitions = new(sync.Map)
	w.HostsStatus = make(map[string]bool)
	w.SetDefaultConnConfig()
	w.dpMetricsReportConfig = &proto.DpMetricsReportConfig{
		EnableReport:      false,
		ReportIntervalSec: defaultMetricReportSec,
		FetchIntervalSec:  defaultMetricFetchSec,
	}
	w.dpFollowerReadDelayConfig = &proto.DpFollowerReadDelayConfig{
		EnableCollect:        true,
		DelaySummaryInterval: followerReadDelaySummaryInterval,
	}
	w.clusterName = dataState.ClusterName
	w.getStreamerFunc = getStreamerFunc
	LocalIP = dataState.LocalIP

	view := dataState.VolView
	w.volCreateTime = view.CreateTime
	w.followerRead = view.FollowerRead
	w.nearRead = view.NearRead
	w.forceROW = view.ForceROW
	w.notCacheNode = view.NotCacheNode
	w.flock = view.Flock
	w.zoneName = view.ZoneName
	w.dpSelectorName = view.DpSelectorName
	w.dpSelectorParm = view.DpSelectorParm
	w.crossRegionHAType = view.CrossRegionHAType
	w.quorum = view.Quorum
	w.ecEnable = view.EcEnable
	w.extentCacheExpireSec = view.ExtentCacheExpireSec
	w.umpKeyPrefix = view.UmpKeyPrefix
	w.updateConnConfig(view.ConnConfig, defaultConfig)
	w.updateDpMetricsReportConfig(view.DpMetricsReportConfig)
	w.updateDpFollowerReadDelayConfig(&view.DpFolReadDelayConfig)
	w.initDpSelector()

	w.updateReadAheadRemoteConfig(view.ReadAheadMemMB, view.ReadAheadWindowMB)
	w.updateReadAheadLocalConfig(localReadAheadMemMB, localReadAheadWindowMB)

	w.volNotExistCount = dataState.VolNotExistCount
	if !w.VolNotExists() {
		w.convertDataPartition(dataState.DpView, true)
	}

	w.updateDataNodeStatus(&dataState.ClusterView.DataNodes, &dataState.ClusterView.EcNodes)

	StreamConnPoolInitOnce.Do(func() {
		StreamConnPool = connpool.NewConnectPoolWithTimeoutAndCap(0, 10, time.Duration(w.connConfig.IdleTimeoutSec)*time.Second, time.Duration(w.connConfig.ConnectTimeoutNs))
	})

	w.wg.Add(4)
	go w.update()
	go w.updateCrossRegionHostStatus()
	go w.ScheduleDataPartitionMetricsReport()
	go w.dpFollowerReadDelayCollect()

	return
}

func (w *Wrapper) setClusterCacheReadConnTimeoutMs(timeoutMs int64) {
	if timeoutMs <= 0 {
		return
	}
	w.cacheReadTimeoutMs = timeoutMs
}

func (w *Wrapper) setClusterBoostEnable(enableBoost bool) {
	w.oldCacheBoostStatus = w.IsCacheBoostEnabled()

	oldEnableBoost := w.enableClusterCacheBoost
	w.enableClusterCacheBoost = enableBoost
	if oldEnableBoost != enableBoost {
		log.LogInfof("setClusterBoostEnable: from old(%v) to new(%v)", oldEnableBoost, enableBoost)
	}
}

func (w *Wrapper) IsCacheBoostEnabled() bool {
	return w.enableClusterCacheBoost && w.enableVolCacheBoost
}

func (w *Wrapper) initRemoteCache() {
	cacheConfig := &flash.CacheConfig{
		Cluster:       w.clusterName,
		Volume:        w.volName,
		Masters:       w.masters,
		MW:            w.metaWrapper,
		ReadTimeoutMs: w.cacheReadTimeoutMs,
	}
	w.remoteCache = flash.NewRemoteCache(cacheConfig)
	if !w.remoteCache.ResetCacheBoostPathToBloom(w.cacheBoostPath) {
		w.cacheBoostPath = ""
	}
	return
}

func (w *Wrapper) saveDataState() *DataState {
	dataState := new(DataState)
	dataState.ClusterName = w.clusterName
	dataState.LocalIP = LocalIP
	dataState.VolNotExistCount = w.volNotExistCount

	dataState.VolView = w.saveSimpleVolView()
	dataState.DpView = w.saveDataPartition()
	dataState.ClusterView = w.saveClientClusterView()

	return dataState
}

func (w *Wrapper) Stop() {
	w.stopOnce.Do(func() {
		if w.readAheadController != nil {
			w.readAheadController.Stop()
		}
		if w.remoteCache != nil {
			w.remoteCache.Stop()
		}
		close(w.stopC)
		w.wg.Wait()
	})
}

func (w *Wrapper) InitFollowerRead(clientConfig bool) {
	w.followerReadClientCfg = clientConfig
	w.followerRead = w.followerReadClientCfg || w.followerRead
}

func (w *Wrapper) FollowerRead() bool {
	if proto.IsDbBack {
		return true
	}
	return w.followerRead
}

func (w *Wrapper) updateClusterInfo() (err error) {
	var (
		info    *proto.ClusterInfo
		localIp string
	)
	if info, err = w.mc.AdminAPI().GetClusterInfo(); err != nil {
		log.LogWarnf("UpdateClusterInfo: get cluster info fail: err(%v)", err)
		return
	}
	log.LogInfof("UpdateClusterInfo: get cluster info: cluster(%v) localIP(%v)", info.Cluster, info.Ip)
	w.clusterName = info.Cluster
	if localIp, err = iputil.GetLocalIPByDial(w.mc.Nodes(), iputil.GetLocalIPTimeout); err != nil {
		log.LogWarnf("UpdateClusterInfo: get local ip fail: err(%v)", err)
		return
	}
	LocalIP = localIp
	return
}

func (w *Wrapper) getSimpleVolView() (err error) {
	var view *proto.SimpleVolView

	if view, err = w.mc.AdminAPI().GetVolumeSimpleInfo(w.volName); err != nil {
		log.LogWarnf("getSimpleVolView: get volume simple info fail: volume(%v) err(%v)", w.volName, err)
		return
	}
	w.volCreateTime = view.CreateTime
	w.followerRead = view.FollowerRead
	w.nearRead = view.NearRead
	w.forceROW = view.ForceROW
	w.notCacheNode = view.NotCacheNode
	w.flock = view.Flock
	w.zoneName = view.ZoneName
	w.dpSelectorName = view.DpSelectorName
	w.dpSelectorParm = view.DpSelectorParm
	w.crossRegionHAType = view.CrossRegionHAType
	w.quorum = view.Quorum
	w.ecEnable = view.EcEnable
	w.extentCacheExpireSec = view.ExtentCacheExpireSec
	if view.UmpCollectWay != exporter.UMPCollectMethodUnknown {
		exporter.SetUMPCollectMethod(view.UmpCollectWay)
	}
	w.enableVolCacheBoost = view.RemoteCacheBoostEnable
	w.cacheBoostPath = view.RemoteCacheBoostPath
	w.enableCacheAutoPrepare = view.RemoteCacheAutoPrepare
	w.cacheTTL = view.RemoteCacheTTL
	w.umpKeyPrefix = view.UmpKeyPrefix
	w.updateConnConfig(view.ConnConfig, volumeConfig)
	w.updateDpMetricsReportConfig(view.DpMetricsReportConfig)
	w.updateDpFollowerReadDelayConfig(&view.DpFolReadDelayConfig)
	_ = w.updateReadAheadRemoteConfig(view.ReadAheadMemMB, view.ReadAheadWindowMB)

	log.LogInfof("getSimpleVolView: get volume simple info: ID(%v) name(%v) owner(%v) status(%v) capacity(%v) "+
		"metaReplicas(%v) dataReplicas(%v) mpCnt(%v) dpCnt(%v) followerRead(%v) forceROW(%v) enableWriteCache(%v) createTime(%v) dpSelectorName(%v) "+
		"dpSelectorParm(%v) quorum(%v) extentCacheExpireSecond(%v) dpFolReadDelayConfig(%v) connConfig(%v) readAheadMemMB(%v) readAheadWindowMB(%v)",
		view.ID, view.Name, view.Owner, view.Status, view.Capacity, view.MpReplicaNum, view.DpReplicaNum, view.MpCnt,
		view.DpCnt, view.FollowerRead, view.ForceROW, view.EnableWriteCache, view.CreateTime, view.DpSelectorName, view.DpSelectorParm,
		view.Quorum, view.ExtentCacheExpireSec, view.DpFolReadDelayConfig, view.ConnConfig, view.ReadAheadMemMB, view.ReadAheadWindowMB)
	return nil
}

func (w *Wrapper) saveSimpleVolView() *proto.SimpleVolView {
	view := &proto.SimpleVolView{
		CreateTime:           w.volCreateTime,
		FollowerRead:         w.followerRead,
		NearRead:             w.nearRead,
		ForceROW:             w.forceROW,
		NotCacheNode:         w.notCacheNode,
		Flock:                w.flock,
		DpSelectorName:       w.dpSelectorName,
		DpSelectorParm:       w.dpSelectorParm,
		CrossRegionHAType:    w.crossRegionHAType,
		Quorum:               w.quorum,
		EcEnable:             w.ecEnable,
		ExtentCacheExpireSec: w.extentCacheExpireSec,
		UmpKeyPrefix:         w.umpKeyPrefix,
		ReadAheadMemMB: 	  w.readAheadController.getMemoryMB(),
		ReadAheadWindowMB:	  w.readAheadController.getWindowMB(),
	}
	view.ConnConfig = &proto.ConnConfig{
		IdleTimeoutSec:   w.connConfig.IdleTimeoutSec,
		ConnectTimeoutNs: w.connConfig.ConnectTimeoutNs,
		WriteTimeoutNs:   w.connConfig.WriteTimeoutNs,
		ReadTimeoutNs:    w.connConfig.ReadTimeoutNs,
	}

	view.DpMetricsReportConfig = &proto.DpMetricsReportConfig{
		EnableReport:      w.dpMetricsReportConfig.EnableReport,
		ReportIntervalSec: w.dpMetricsReportConfig.ReportIntervalSec,
		FetchIntervalSec:  w.dpMetricsReportConfig.FetchIntervalSec,
	}
	view.DpFolReadDelayConfig = proto.DpFollowerReadDelayConfig{
		EnableCollect:        w.dpFollowerReadDelayConfig.EnableCollect,
		DelaySummaryInterval: w.dpFollowerReadDelayConfig.DelaySummaryInterval,
	}
	return view
}

func (w *Wrapper) update() {
	defer w.wg.Done()
	for {
		err := w.updateWithRecover()
		if err == nil {
			break
		}
		log.LogErrorf("updateDataInfo: err(%v) try next update", err)
	}
}

func (w *Wrapper) updateWithRecover() (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("updateWithRecover panic: err(%v) stack(%v)", r, string(debug.Stack()))
			msg := fmt.Sprintf("updateDataInfo panic: err(%v)", r)
			common.HandleUmpAlarm(w.clusterName, w.volName, "updateDataInfo", msg)
			err = errors.New(msg)
		}
	}()
	ticker := time.NewTicker(time.Minute)
	checkRemovedDpTimer = time.NewTimer(0)
	refreshLatency := time.NewTimer(0)

	defer func() {
		ticker.Stop()
		checkRemovedDpTimer.Stop()
		refreshLatency.Stop()
	}()

	var (
		retryHosts map[string]bool
		hostsLock  sync.Mutex
	)
	for {
		select {
		case <-w.stopC:
			return
		case <-ticker.C:
			w.updateClientClusterView()
			w.updateSimpleVolView()
			w.updateDataPartition(false)

			hostsLock.Lock()
			retryHosts = w.retryHostsPingtime(retryHosts)
			hostsLock.Unlock()
		case <-checkRemovedDpTimer.C:
			leastCheckCount := w.checkDpForOverWrite()
			d := time.Second
			if leastCheckCount == 0 {
				d = time.Minute
			} else if leastCheckCount > 20 {
				d = 10 * time.Second
			} else if leastCheckCount > 10 {
				d = 5 * time.Second
			}
			checkRemovedDpTimer.Reset(d)
		case <-refreshLatency.C:
			if w.IsCacheBoostEnabled() {
				hostsLock.Lock()
				retryHosts = w.updateHostsPingtime()
				hostsLock.Unlock()
			}

			refreshLatency.Reset(RefreshHostLatencyInterval)
		}
	}
}

func (w *Wrapper) updateHostsPingtime() map[string]bool {
	failedHosts := make(map[string]bool)
	allHosts := make(map[string]bool)
	w.partitions.Range(func(id, value interface{}) bool {
		dp := value.(*DataPartition)
		for _, host := range dp.Hosts {
			if _, ok := allHosts[host]; ok {
				continue
			}
			allHosts[host] = true
			avgTime, err := iputil.PingWithTimeout(strings.Split(host, ":")[0], pingCount, pingTimeout*pingCount)
			if err != nil {
				avgTime = time.Duration(0)
				failedHosts[host] = true
				log.LogWarnf("updateHostsPingtime: host(%v) err(%v)", host, err)
			} else {
				log.LogDebugf("updateHostsPingtime: host(%v) ping time(%v)", host, avgTime)
			}
			w.HostsDelay.Store(host, avgTime)
		}
		return true
	})
	return failedHosts
}

func (w *Wrapper) retryHostsPingtime(retryHosts map[string]bool) map[string]bool {
	if retryHosts == nil || len(retryHosts) == 0 {
		return nil
	}
	failedHosts := make(map[string]bool)
	for host, _ := range retryHosts {
		avgTime, err := iputil.PingWithTimeout(strings.Split(host, ":")[0], pingCount, pingTimeout*pingCount)
		if err != nil {
			avgTime = time.Duration(0)
			failedHosts[host] = true
		} else {
			w.HostsDelay.Store(host, avgTime)
		}
	}
	return failedHosts
}

func (w *Wrapper) updateSimpleVolView() (err error) {
	var view *proto.SimpleVolView

	if view, err = w.mc.AdminAPI().GetVolumeSimpleInfo(w.volName); err != nil {
		log.LogWarnf("updateSimpleVolView: get volume simple info fail: volume(%v) err(%v)", w.volName, err)
		return
	}

	if w.volCreateTime != "" && w.volCreateTime != view.CreateTime {
		log.LogWarnf("updateSimpleVolView: update volCreateTime from old(%v) to new(%v) and clear data partitions", w.volCreateTime, view.CreateTime)
		w.volCreateTime = view.CreateTime
		w.partitions = new(sync.Map)
	}

	if w.followerRead != view.FollowerRead && !w.followerReadClientCfg {
		log.LogInfof("updateSimpleVolView: update followerRead from old(%v) to new(%v)",
			w.followerRead, view.FollowerRead)
		w.followerRead = view.FollowerRead
	}

	if w.nearRead != view.NearRead {
		log.LogInfof("updateSimpleVolView: update nearRead from old(%v) to new(%v)", w.nearRead, view.NearRead)
		w.nearRead = view.NearRead
	}

	if w.zoneName != view.ZoneName {
		log.LogInfof("updateSimpleVolView: update zoneName from old(%v) to new(%v)", w.zoneName, view.ZoneName)
		w.zoneName = view.ZoneName
	}

	if exporter.GetUmpCollectMethod() != view.UmpCollectWay && view.UmpCollectWay != exporter.UMPCollectMethodUnknown {
		log.LogInfof("updateSimpleVolView: update umpCollectWay from old(%v) to new(%v)", exporter.GetUmpCollectMethod(), view.UmpCollectWay)
		exporter.SetUMPCollectMethod(view.UmpCollectWay)
	}

	if w.dpSelectorName != view.DpSelectorName || w.dpSelectorParm != view.DpSelectorParm || w.quorum != view.Quorum {
		log.LogInfof("updateSimpleVolView: update dpSelector from old(%v %v) to new(%v %v), update quorum from old(%v) to new(%v)",
			w.dpSelectorName, w.dpSelectorParm, view.DpSelectorName, view.DpSelectorParm, w.quorum, view.Quorum)
		w.Lock()
		w.dpSelectorName = view.DpSelectorName
		w.dpSelectorParm = view.DpSelectorParm
		w.quorum = view.Quorum
		w.dpSelectorChanged = true
		w.Unlock()
	}

	if w.forceROW != view.ForceROW {
		log.LogInfof("updateSimpleVolView: update forceROW from old(%v) to new(%v)", w.forceROW, view.ForceROW)
		w.forceROW = view.ForceROW
	}

	if w.crossRegionHAType != view.CrossRegionHAType {
		log.LogInfof("updateSimpleVolView: update crossRegionHAType from old(%v) to new(%v)", w.crossRegionHAType, view.CrossRegionHAType)
		w.crossRegionHAType = view.CrossRegionHAType
	}

	if w.extentCacheExpireSec != view.ExtentCacheExpireSec {
		log.LogInfof("updateSimpleVolView: update ExtentCacheExpireSec from old(%v) to new(%v)", w.extentCacheExpireSec, view.ExtentCacheExpireSec)
		w.extentCacheExpireSec = view.ExtentCacheExpireSec
	}

	if w.ecEnable != view.EcEnable {
		log.LogInfof("updateSimpleVolView: update EcEnable from old(%v) to new(%v)", w.ecEnable, view.EcEnable)
		w.ecEnable = view.EcEnable
	}

	if w.umpKeyPrefix != view.UmpKeyPrefix {
		log.LogInfof("updateSimpleVolView: update umpKeyPrefix from old(%v) to new(%v)", w.umpKeyPrefix, view.UmpKeyPrefix)
		w.umpKeyPrefix = view.UmpKeyPrefix
	}

	w.updateRemoteCacheConfig(view)
	w.updateConnConfig(view.ConnConfig, volumeConfig)
	w.updateDpMetricsReportConfig(view.DpMetricsReportConfig)
	w.updateDpFollowerReadDelayConfig(&view.DpFolReadDelayConfig)
	w.updateReadAheadRemoteConfig(view.ReadAheadMemMB, view.ReadAheadWindowMB)
	if w.dpLowestDelayHostWeight != view.FolReadHostWeight {
		log.LogInfof("updateSimpleVolView: update FolReadHostWeight from old(%v) to new(%v)", w.dpLowestDelayHostWeight, view.FolReadHostWeight)
		w.dpLowestDelayHostWeight = view.FolReadHostWeight
	}
	return nil
}

func (w *Wrapper) updateRemoteCacheConfig(view *proto.SimpleVolView) {
	if w.enableVolCacheBoost != view.RemoteCacheBoostEnable {
		log.LogInfof("updateRemoteCacheConfig: RemoteCacheBoostEnable from old(%v) to new(%v)", w.enableVolCacheBoost, view.RemoteCacheBoostEnable)
		w.enableVolCacheBoost = view.RemoteCacheBoostEnable
	}
	if w.oldCacheBoostStatus != w.IsCacheBoostEnabled() {
		log.LogInfof("updateRemoteCacheConfig: enable from old(%v) to new(%v)", w.oldCacheBoostStatus, w.IsCacheBoostEnabled())
	}
	// remoteCache may be nil if the first initialization failed, it will not be set nil anymore even if remote cache is disabled
	if w.IsCacheBoostEnabled() {
		if !w.oldCacheBoostStatus || w.remoteCache == nil {
			log.LogInfof("updateRemoteCacheConfig: initRemoteCache: enable(%v -> %v) remoteCache isNil(%v)", w.oldCacheBoostStatus, w.IsCacheBoostEnabled(), w.remoteCache == nil)
			w.initRemoteCache()
		}
	} else if w.oldCacheBoostStatus && w.remoteCache != nil {
		w.remoteCache.Stop()
		log.LogInfof("updateRemoteCacheConfig: stop remoteCache")
	}

	if w.cacheBoostPath != view.RemoteCacheBoostPath {
		oldBoostPath := w.cacheBoostPath
		w.cacheBoostPath = view.RemoteCacheBoostPath
		if w.IsCacheBoostEnabled() && w.remoteCache != nil {
			if !w.remoteCache.ResetCacheBoostPathToBloom(view.RemoteCacheBoostPath) {
				w.cacheBoostPath = ""
			}
		}
		log.LogInfof("updateRemoteCacheConfig: RemoteCacheBoostPath from old(%v) to want(%v), but(%v)", oldBoostPath, view.RemoteCacheBoostPath, w.cacheBoostPath)
	}

	if w.enableCacheAutoPrepare != view.RemoteCacheAutoPrepare {
		log.LogInfof("updateRemoteCacheConfig: RemoteCacheAutoPrepare from old(%v) to new(%v)", w.enableCacheAutoPrepare, view.RemoteCacheAutoPrepare)
		w.enableCacheAutoPrepare = view.RemoteCacheAutoPrepare
	}

	if w.cacheTTL != view.RemoteCacheTTL {
		log.LogInfof("updateRemoteCacheConfig: RemoteCacheTTL from old(%d) to new(%d)", w.cacheTTL, view.RemoteCacheTTL)
		w.cacheTTL = view.RemoteCacheTTL
	}

}

func (w *Wrapper) updateDataPartition(isInit bool) (err error) {
	var dpv *proto.DataPartitionsView
	if dpv, err = w.fetchDataPartition(); err != nil {
		return
	}
	return w.convertDataPartition(dpv, isInit)
}

func (w *Wrapper) fetchDataPartition() (dpv *proto.DataPartitionsView, err error) {
	var f func(volName string, dpIDs []uint64) (view *proto.DataPartitionsView, err error)
	if w.extentClientType == Normal {
		f = w.mc.ClientAPI().GetDataPartitions
	} else if w.extentClientType == Smart {
		f = w.mc.AdminAPI().GetHDDDataPartitions
	} else {
		err = errors.NewErrorf("ExtentClientType(%v) is incorrect", w.extentClientType)
		return
	}
	if dpv, err = f(w.volName, nil); err != nil {
		if err == proto.ErrVolNotExists {
			w.volNotExistCount++
		}
		log.LogWarnf("updateDataPartition: get data partitions fail: volume(%v) notExistCount(%v) err(%v)", w.volName, w.volNotExistCount, err)
		return
	}
	if w.volNotExistCount > VolNotExistClearViewThresholdMin {
		w.partitions = new(sync.Map)
		log.LogInfof("updateDataPartition: clear volNotExistCount(%v) and data partitions", w.volNotExistCount)
	}
	w.volNotExistCount = 0
	log.LogInfof("updateDataPartition: get data partitions: volume(%v) partitions(%v) notExistCount(%v)", w.volName, len(dpv.DataPartitions), w.volNotExistCount)
	return
}

func (w *Wrapper) convertDataPartition(dpv *proto.DataPartitionsView, isInit bool) (err error) {
	var convert = func(response *proto.DataPartitionResponse) *DataPartition {
		return &DataPartition{
			DataPartitionResponse: *response,
			ClientWrapper:         w,
			CrossRegionMetrics:    NewCrossRegionMetrics(),
		}
	}

	rwPartitionGroups := make([]*DataPartition, 0)
	for _, partition := range dpv.DataPartitions {
		dp := convert(partition)
		if len(dp.Hosts) == 0 {
			log.LogWarnf("updateDataPartition: no host in dp(%v)", dp)
			continue
		}
		//log.LogInfof("updateDataPartition: dp(%v)", dp)
		actualDp := w.replaceOrInsertPartition(dp)
		if w.extentClientType == Normal {
			if actualDp.Status == proto.ReadWrite {
				rwPartitionGroups = append(rwPartitionGroups, actualDp)
			}
		} else if w.extentClientType == Smart {
			if actualDp.MediumType == proto.MediumHDDName &&
				actualDp.TransferStatus == proto.ReadWrite {
				rwPartitionGroups = append(rwPartitionGroups, actualDp)
			}
		} else {
			err = errors.NewErrorf("updateDataPartition: extentClientType(%v) is incorrect", w.extentClientType)
			return err
		}
	}

	// isInit used to identify whether this call is caused by mount action
	if isInit || (len(rwPartitionGroups) >= MinWriteAbleDataPartitionCnt) {
		log.LogInfof("updateDataPartition: update rwPartitionGroups count(%v)", len(rwPartitionGroups))
		w.refreshDpSelector(rwPartitionGroups)
	} else {
		err = errors.New("updateDataPartition: no writable data partition")
	}

	log.LogInfof("updateDataPartition: finish")
	return err
}

func (w *Wrapper) saveDataPartition() *proto.DataPartitionsView {
	dpv := &proto.DataPartitionsView{
		DataPartitions: make([]*proto.DataPartitionResponse, 0),
	}
	w.partitions.Range(func(k, v interface{}) bool {
		dp := v.(*DataPartition)
		dpv.DataPartitions = append(dpv.DataPartitions, &dp.DataPartitionResponse)
		return true
	})
	return dpv
}

func (w *Wrapper) replaceOrInsertPartition(dp *DataPartition) (actualDp *DataPartition) {
	if w.CrossRegionHATypeQuorum() {
		w.initCrossRegionHostStatus(dp)
		dp.CrossRegionMetrics.Lock()
		dp.CrossRegionMetrics.CrossRegionHosts = w.classifyCrossRegionHosts(dp.Hosts)
		log.LogDebugf("classifyCrossRegionHosts: dp(%v) hosts(%v) crossRegionMetrics(%v)", dp.PartitionID, dp.Hosts, dp.CrossRegionMetrics)
		dp.CrossRegionMetrics.Unlock()
	} else if w.followerRead && w.nearRead {
		dp.NearHosts = w.sortHostsByDistance(dp.Hosts)
	}

	w.Lock()
	value, ok := w.partitions.Load(dp.PartitionID)
	if ok {
		old := value.(*DataPartition)
		if old.Status != dp.Status || old.ReplicaNum != dp.ReplicaNum ||
			old.EcMigrateStatus != dp.EcMigrateStatus || old.ecEnable != w.ecEnable ||
			strings.Join(old.EcHosts, ",") != strings.Join(dp.EcHosts, ",") ||
			strings.Join(old.Hosts, ",") != strings.Join(dp.Hosts, ",") {
			log.LogInfof("updateDataPartition: dp (%v) --> (%v)", old, dp)
		}
		if !isLeaderExist(old.GetLeaderAddr(), dp.Hosts) {
			if dp.GetLeaderAddr() != "" {
				old.LeaderAddr = proto.NewAtomicString(dp.GetLeaderAddr())
			} else {
				old.LeaderAddr = proto.NewAtomicString(dp.Hosts[0])
			}
		}
		old.Status = dp.Status
		old.TransferStatus = dp.TransferStatus
		old.ReplicaNum = dp.ReplicaNum
		old.Hosts = dp.Hosts
		old.NearHosts = dp.NearHosts
		old.EcMigrateStatus = dp.EcMigrateStatus
		old.EcHosts = dp.EcHosts
		old.EcMaxUnitSize = dp.EcMaxUnitSize
		old.EcDataNum = dp.EcDataNum
		old.MediumType = dp.MediumType
		old.CrossRegionMetrics.Lock()
		old.CrossRegionMetrics.CrossRegionHosts = dp.CrossRegionMetrics.CrossRegionHosts
		old.CrossRegionMetrics.Unlock()
		old.ecEnable = w.ecEnable
		actualDp = old
	} else {
		dp.Metrics = proto.NewDataPartitionMetrics()
		dp.ReadMetrics = proto.NewDPReadMetrics()
		dp.ecEnable = w.ecEnable
		w.partitions.Store(dp.PartitionID, dp)
		actualDp = dp
		log.LogInfof("updateDataPartition: new dp (%v) EcMigrateStatus (%v)", dp, dp.EcMigrateStatus)
	}
	w.Unlock()
	return actualDp
}

func isLeaderExist(addr string, hosts []string) bool {
	for _, host := range hosts {
		if addr == host {
			return true
		}
	}
	return false
}

func (w *Wrapper) getDataPartitionFromMaster(partitionID uint64) (err error) {
	if partitionID == 0 {
		err = fmt.Errorf("invalid partitionID(0)")
	}
	var dpInfo *proto.DataPartitionInfo
	start := time.Now()
	for {
		if dpInfo, err = w.mc.AdminAPI().GetDataPartition(w.volName, partitionID); err == nil {
			if len(dpInfo.Hosts) > 0 {
				log.LogInfof("getDataPartitionFromMaster: pid(%v) vol(%v)", partitionID, w.volName)
				break
			}
			err = fmt.Errorf("master return empty host list")
		}
		if err != nil && time.Since(start) > MasterNoCacheAPIRetryTimeout {
			log.LogWarnf("getDataPartitionFromMaster: err(%v) pid(%v) vol(%v) retry timeout(%v)", err, partitionID, w.volName, time.Since(start))
			return
		}
		log.LogWarnf("getDataPartitionFromMaster: err(%v) pid(%v) vol(%v) retry next round", err, partitionID, w.volName)
		time.Sleep(1 * time.Second)
	}
	var convert = func(dpInfo *proto.DataPartitionInfo) *DataPartition {
		dp := &DataPartition{
			ClientWrapper: w,
			DataPartitionResponse: proto.DataPartitionResponse{
				PartitionID: dpInfo.PartitionID,
				Status:      dpInfo.Status,
				ReplicaNum:  dpInfo.ReplicaNum,
				Hosts:       dpInfo.Hosts,
				LeaderAddr:  proto.NewAtomicString(getDpInfoLeaderAddr(dpInfo)),
			},
			CrossRegionMetrics: NewCrossRegionMetrics(),
		}
		return dp
	}
	dp := convert(dpInfo)
	log.LogInfof("getDataPartitionFromMaster: dp(%v) leader(%v)", dp, dp.GetLeaderAddr())
	w.replaceOrInsertPartition(dp)
	return nil
}

func getDpInfoLeaderAddr(partition *proto.DataPartitionInfo) (leaderAddr string) {
	for _, replica := range partition.Replicas {
		if replica.IsLeader {
			return replica.Addr
		}
	}
	return
}

// GetDataPartition returns the data partition based on the given partition ID.
func (w *Wrapper) GetDataPartition(partitionID uint64) (dp *DataPartition, err error) {
	value, ok := w.partitions.Load(partitionID)
	if !ok {
		w.getDataPartitionFromMaster(partitionID)
		value, ok = w.partitions.Load(partitionID)
		if !ok {
			return nil, fmt.Errorf("partition[%v] not exsit", partitionID)
		}
	}
	dp = value.(*DataPartition)
	return dp, nil
}

func (w *Wrapper) fetchClusterView() (cv *proto.ClusterView, err error) {
	cv, err = w.mc.AdminAPI().GetCluster()
	if err != nil {
		log.LogWarnf("updateDataNodeStatus: get cluster fail: err(%v)", err)
	}
	return
}

func (w *Wrapper) fetchClientClusterView() (cf *proto.ClientClusterConf, err error) {
	cf, err = w.mc.AdminAPI().GetClientConf()
	if err != nil {
		log.LogWarnf("fetchClientConfView: getClientConf fail: err(%v)", err)
	}
	return
}

func (w *Wrapper) updateClientClusterView() (err error) {
	var (
		dataNodes *[]proto.NodeView
		ecNodes   *[]proto.NodeView
		cv        *proto.ClusterView
		cf        *proto.ClientClusterConf
	)
	if proto.IsDbBack {
		if cv, err = w.fetchClusterView(); err != nil {
			return
		}
		dataNodes = &cv.DataNodes
		ecNodes = &cv.EcNodes
	} else {
		if cf, err = w.fetchClientClusterView(); err != nil {
			return
		}
		dataNodes = &cf.DataNodes
		ecNodes = &cf.EcNodes
	}
	w.updateDataNodeStatus(dataNodes, ecNodes)
	if proto.IsDbBack {
		return
	}

	if w.dpMetricsReportDomain != cf.SchedulerDomain {
		log.LogInfof("updateDataNodeStatus: update scheduler domain from old(%v) to new(%v)", w.dpMetricsReportDomain, cf.SchedulerDomain)
		w.dpMetricsReportDomain = cf.SchedulerDomain
		w.schedulerClient.UpdateSchedulerDomain(w.dpMetricsReportDomain)
	}

	w.umpJmtpAddr = cf.UmpJmtpAddr
	exporter.SetUMPJMTPAddress(w.umpJmtpAddr)
	exporter.SetUmpJMTPBatch(uint(cf.UmpJmtpBatch))

	w.setClusterBoostEnable(cf.RemoteCacheBoostEnable)
	w.setClusterCacheReadConnTimeoutMs(cf.RemoteReadTimeoutMs)
	if w.IsCacheBoostEnabled() && w.remoteCache != nil {
		w.remoteCache.ResetConnConfig(cf.RemoteReadTimeoutMs * int64(time.Millisecond))
	}

	if len(cf.ZoneConnConfig) != 0 {
		if clusterCfg, ok := cf.ZoneConnConfig[""]; ok {
			w.updateConnConfig(&clusterCfg, clusterConfig)
		}
		zoneCfg := getVolZoneConnConfig(w.zoneName, cf.ZoneConnConfig)
		if zoneCfg != nil {
			w.updateConnConfig(zoneCfg, zoneConfig)
		}
	}
	return
}

func (w *Wrapper) updateDataNodeStatus(dataNodes *[]proto.NodeView, ecNodes *[]proto.NodeView) {
	newHostsStatus := make(map[string]bool)
	for _, node := range *dataNodes {
		newHostsStatus[node.Addr] = node.Status
	}

	for _, node := range *ecNodes {
		newHostsStatus[node.Addr] = node.Status
	}
	log.LogInfof("updateDataNodeStatus: update %d hosts status", len(newHostsStatus))

	w.Lock()
	w.HostsStatus = newHostsStatus
	w.Unlock()
	return
}

func (w *Wrapper) saveClientClusterView() *proto.ClientClusterConf {
	w.RLock()
	defer w.RUnlock()
	cf := &proto.ClientClusterConf{
		DataNodes:       make([]proto.NodeView, 0, len(w.HostsStatus)),
		SchedulerDomain: w.dpMetricsReportDomain,
	}
	for addr, status := range w.HostsStatus {
		cf.DataNodes = append(cf.DataNodes, proto.NodeView{Addr: addr, Status: status})
	}
	return cf
}

func (w *Wrapper) SetNearRead(nearRead bool) {
	w.nearRead = w.nearRead || nearRead
	log.LogInfof("SetNearRead: set nearRead to %v", w.nearRead)
}

func (w *Wrapper) NearRead() bool {
	return w.nearRead
}

func (w *Wrapper) SetMetaWrapper(metaWrapper *meta.MetaWrapper) {
	w.metaWrapper = metaWrapper
}

func (w *Wrapper) SetDpFollowerReadDelayConfig(enableCollect bool, delaySummaryInterval int64) {
	if w.dpFollowerReadDelayConfig == nil {
		w.dpFollowerReadDelayConfig = &proto.DpFollowerReadDelayConfig{}
	}
	w.dpFollowerReadDelayConfig.EnableCollect = enableCollect
	w.dpFollowerReadDelayConfig.DelaySummaryInterval = delaySummaryInterval
}

func (w *Wrapper) CrossRegionHATypeQuorum() bool {
	return w.crossRegionHAType == proto.CrossRegionHATypeQuorum
}

// Sort hosts by distance form local
func (w *Wrapper) sortHostsByDistance(dpHosts []string) []string {
	nearHost := make([]string, len(dpHosts))
	copy(nearHost, dpHosts)
	for i := 0; i < len(nearHost); i++ {
		for j := i + 1; j < len(nearHost); j++ {
			if distanceFromLocal(nearHost[i]) > distanceFromLocal(nearHost[j]) {
				nearHost[i], nearHost[j] = nearHost[j], nearHost[i]
			}
		}
	}
	return nearHost
}

func (w *Wrapper) SetDefaultConnConfig() {
	w.connConfig = &proto.ConnConfig{
		IdleTimeoutSec:   IdleConnTimeoutData,
		ConnectTimeoutNs: ConnectTimeoutDataMs * int64(time.Millisecond),
		WriteTimeoutNs:   WriteTimeoutData * int64(time.Second),
		ReadTimeoutNs:    ReadTimeoutData * int64(time.Second),
	}
}

func (w *Wrapper) updateConnConfig(config *proto.ConnConfig, level connConfigLevel) (isUpdate bool) {
	if config == nil {
		return
	}
	if config.ReadTimeoutNs == 0 && config.WriteTimeoutNs == 0 {
		return
	}
	var changed = func(oldCfg, newCfg *proto.ConnConfig) bool {
		if oldCfg == nil || oldCfg.ReadTimeoutNs != newCfg.ReadTimeoutNs || oldCfg.WriteTimeoutNs != newCfg.WriteTimeoutNs {
			return true
		}
		return false
	}
	switch level {
	case defaultConfig:
		if changed(w.connConfig, config) {
			goto doUpdate
		}
		return
	case volumeConfig:
		if changed(w.volConnConfig, config) {
			w.volConnConfig = config
			goto doUpdate
		}
		return
	case zoneConfig:
		if changed(w.zoneConnConfig, config) {
			if w.volConnConfig == nil {
				w.zoneConnConfig = config
				goto doUpdate
			}
		}
		return
	case clusterConfig:
		if changed(w.clusterConnConfig, config) {
			if w.volConnConfig == nil && w.zoneConnConfig == nil {
				w.clusterConnConfig = config
				goto doUpdate
			}
		}
		return
	}
doUpdate:
	log.LogInfof("updateConnConfig: old(%v) new(%v) level(%s)", w.connConfig, config, level.String())
	updateConnPool := false
	if config.IdleTimeoutSec > 0 && config.IdleTimeoutSec != w.connConfig.IdleTimeoutSec {
		w.connConfig.IdleTimeoutSec = config.IdleTimeoutSec
		updateConnPool = true
	}
	if config.ConnectTimeoutNs > 0 && config.ConnectTimeoutNs != w.connConfig.ConnectTimeoutNs {
		w.connConfig.ConnectTimeoutNs = config.ConnectTimeoutNs
		updateConnPool = true
	}
	if config.WriteTimeoutNs > 0 && config.WriteTimeoutNs != w.connConfig.WriteTimeoutNs {
		atomic.StoreInt64(&w.connConfig.WriteTimeoutNs, config.WriteTimeoutNs)
		isUpdate = true
	}
	if config.ReadTimeoutNs > 0 && config.ReadTimeoutNs != w.connConfig.ReadTimeoutNs {
		atomic.StoreInt64(&w.connConfig.ReadTimeoutNs, config.ReadTimeoutNs)
		isUpdate = true
	}
	if updateConnPool && StreamConnPool != nil {
		StreamConnPool.UpdateTimeout(time.Duration(w.connConfig.IdleTimeoutSec)*time.Second, time.Duration(w.connConfig.ConnectTimeoutNs))
	}
	return updateConnPool || isUpdate
}

func (w *Wrapper) updateDpMetricsReportConfig(config *proto.DpMetricsReportConfig) {
	if config == nil {
		return
	}
	log.LogInfof("updateDpMetricsReportConfig: (%v)", config)
	if w.dpMetricsReportConfig.EnableReport != config.EnableReport {
		w.dpMetricsReportConfig.EnableReport = config.EnableReport
	}
	if config.ReportIntervalSec > 0 && w.dpMetricsReportConfig.ReportIntervalSec != config.ReportIntervalSec {
		atomic.StoreInt64(&w.dpMetricsReportConfig.ReportIntervalSec, config.ReportIntervalSec)
	}
	if config.FetchIntervalSec > 0 && w.dpMetricsReportConfig.FetchIntervalSec != config.FetchIntervalSec {
		atomic.StoreInt64(&w.dpMetricsReportConfig.FetchIntervalSec, config.FetchIntervalSec)
	}
}

func (w *Wrapper) updateDpFollowerReadDelayConfig(config *proto.DpFollowerReadDelayConfig) {
	if config == nil || w.dpFollowerReadDelayConfig == nil {
		return
	}
	if w.dpFollowerReadDelayConfig.EnableCollect != config.EnableCollect {
		w.dpFollowerReadDelayConfig.EnableCollect = config.EnableCollect
	}
	if config.DelaySummaryInterval >= 0 && w.dpFollowerReadDelayConfig.DelaySummaryInterval != config.DelaySummaryInterval {
		atomic.StoreInt64(&w.dpFollowerReadDelayConfig.DelaySummaryInterval, config.DelaySummaryInterval)
	}
	log.LogInfof("updateDpFollowerReadDelayConfig: (%v)", w.dpFollowerReadDelayConfig)
}

func (w *Wrapper) VolNotExists() bool {
	if w.volNotExistCount > VolNotExistInterceptThresholdMin {
		log.LogWarnf("VolNotExists: vol(%v) count(%v) threshold(%v)", w.volName, w.volNotExistCount, VolNotExistInterceptThresholdMin)
		return true
	}
	return false
}

func (w *Wrapper) updateReadAheadLocalConfig(localReadAheadMemMB, localReadAheadWindowMB int64) (err error) {
	var (
		newMemMB	int64
		newWindowMB	int64
	)
	if err = validateReadAheadConfig(localReadAheadMemMB, localReadAheadWindowMB); err != nil {
		log.LogErrorf("invalid read ahead config: memMB(%v), windowMB(%v)", localReadAheadMemMB, localReadAheadWindowMB)
		return
	}
	defer func() {
		if err == nil && w.readAheadController != nil {
			if localReadAheadMemMB != 0 {
				w.readAheadController.localMemMB = localReadAheadMemMB
			}
			if localReadAheadWindowMB != 0 {
				w.readAheadController.localWindowMB = localReadAheadWindowMB
			}
		}
	}()

	if localReadAheadMemMB < 0 && w.readAheadController == nil {
		return
	} else if localReadAheadMemMB < 0 && w.readAheadController.remoteMemMB > 0 {
		newMemMB = w.readAheadController.remoteMemMB
	} else {
		newMemMB = localReadAheadMemMB
	}

	if localReadAheadWindowMB < 0 && w.readAheadController != nil && w.readAheadController.remoteWindowMB > 0 {
		newWindowMB = w.readAheadController.remoteWindowMB
	} else {
		newWindowMB = localReadAheadWindowMB
	}

	if err = w.updateReadAheadConfig(newMemMB, newWindowMB); err != nil {
		log.LogErrorf("update local read ahead config failed: %v", err)
		return
	}
	return
}

func (w *Wrapper) updateReadAheadRemoteConfig(remoteReadAheadMemMB, remoteReadAheadWindowMB int64) (err error) {
	if err = validateReadAheadConfig(remoteReadAheadMemMB, remoteReadAheadWindowMB); err != nil {
		return
	}
	defer func() {
		if err == nil && w.readAheadController != nil {
			w.readAheadController.remoteMemMB = remoteReadAheadMemMB
			w.readAheadController.remoteWindowMB = remoteReadAheadWindowMB
		}
	}()

	newMemMB := remoteReadAheadMemMB
	newWindowMB := remoteReadAheadWindowMB
	if w.readAheadController != nil && w.readAheadController.localMemMB > 0 {
		newMemMB = 0
	}
	if w.readAheadController != nil && w.readAheadController.localWindowMB > 0 {
		newWindowMB = 0
	}
	if newMemMB == 0 && newWindowMB == 0 {
		return
	}

	if err = w.updateReadAheadConfig(newMemMB, newWindowMB); err != nil {
		log.LogErrorf("update remote read ahead config failed: %v", err)
		return
	}
	return
}

func (w *Wrapper) updateReadAheadConfig(readAheadMemMB, readAheadWindowMB int64) (err error) {
	if err = validateReadAheadConfig(readAheadMemMB, readAheadWindowMB); err != nil {
		return
	}
	w.readAheadInitMutex.Lock()
	defer w.readAheadInitMutex.Unlock()

	if readAheadMemMB > 0 && w.readAheadController == nil {
		readAheadWindowSize := uint64(DefaultReadAheadWindowMB) * unit.MB
		if readAheadWindowMB > 0 {
			readAheadWindowSize = uint64(readAheadWindowMB) * unit.MB
		}
		var controller *ReadAheadController
		if controller, err = NewReadAheadController(w, readAheadMemMB, readAheadWindowSize); err != nil {
			return
		}
		w.readAheadController = controller
		return
	}
	if readAheadMemMB != 0 {
		w.readAheadController.updateBlockCntThreshold(readAheadMemMB)
	}
	if readAheadWindowMB != 0 {
		w.readAheadController.updateWindowSize(readAheadWindowMB)
	}
	return
}

func distanceFromLocal(b string) int {
	remote := strings.Split(b, ":")[0]

	return iputil.GetDistance(net.ParseIP(LocalIP), net.ParseIP(remote))
}

func handleUmpAlarm(cluster, vol, act, msg string) {
	umpKeyCluster := fmt.Sprintf("%s_client_warning", cluster)
	umpMsgCluster := fmt.Sprintf("volume(%s) %s", vol, msg)
	exporter.WarningBySpecialUMPKey(umpKeyCluster, umpMsgCluster)

	umpKeyVol := fmt.Sprintf("%s_%s_warning", cluster, vol)
	umpMsgVol := fmt.Sprintf("act(%s) - %s", act, msg)
	exporter.WarningBySpecialUMPKey(umpKeyVol, umpMsgVol)
}

func getVolZoneConnConfig(zones string, zonesCfg map[string]proto.ConnConfig) (cfg *proto.ConnConfig) {
	log.LogInfof("getVolZoneConnConfig: zone(%v) configs(%v)", zones, zonesCfg)
	var (
		maxReadTimeout  int64
		maxWriteTimeout int64
	)
	zoneList := strings.Split(zones, ",")
	for _, zone := range zoneList {
		if zoneCfg, ok := zonesCfg[zone]; ok {
			if zoneCfg.ReadTimeoutNs > maxReadTimeout {
				maxReadTimeout = zoneCfg.ReadTimeoutNs
			}
			if zoneCfg.WriteTimeoutNs > maxWriteTimeout {
				maxWriteTimeout = zoneCfg.WriteTimeoutNs
			}
		}
	}
	if maxReadTimeout == 0 && maxWriteTimeout == 0 {
		return nil
	}
	return &proto.ConnConfig{
		ReadTimeoutNs:  maxReadTimeout,
		WriteTimeoutNs: maxWriteTimeout,
	}
}
