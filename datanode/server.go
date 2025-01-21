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

package datanode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util/connman"
	"github.com/cubefs/cubefs/util/settings"

	"github.com/cubefs/cubefs/cmd/common"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/repl"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/async"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/cpu"
	utilErrors "github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/iputil"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/multirate"
	"github.com/cubefs/cubefs/util/statinfo"
	"github.com/cubefs/cubefs/util/statistics"
	"github.com/cubefs/cubefs/util/topology"
	"github.com/cubefs/cubefs/util/unit"
)

var (
	ErrIncorrectStoreType       = errors.New("Incorrect store type")
	ErrNoSpaceToCreatePartition = errors.New("No disk space to create a data partition")
	ErrNewSpaceManagerFailed    = errors.New("Creater new space manager failed")
	ErrPartitionNil             = errors.New("partition is nil")
	LocalIP                     string
	LocalServerPort             string
	gConnPool                   = connman.NewConnectionManager(connman.NewDefaultConfig())
	MasterClient                = masterSDK.NewMasterClient(nil, false)
	MasterLBDomainClient        = masterSDK.NewMasterClientWithLBDomain("", false)
	gHasLoadDataPartition       bool
	gHasFinishedLoadDisks       bool

	maybeServerFaultOccurred bool // 是否判定当前节点大概率出现过系统断电
)

var (
	AutoRepairStatus = true
)

const (
	DefaultZoneName          = proto.DefaultZoneName
	DefaultRaftLogsToRetain  = 1000 // Count of raft logs per data partition
	DefaultDiskMaxErr        = 1
	DefaultDiskReservedSpace = 5 * unit.GB // GB
	DefaultDiskUsableRatio   = float64(0.90)
	DefaultDiskReservedRatio = 0.1
)

const (
	ModuleName          = "dataNode"
	SystemStartTimeFile = "SYS_START_TIME"
	DataSettingsFile    = "data_settings"
	MAX_OFFSET_OF_TIME  = 5
)

const (
	ConfigKeyLocalIP        = "localIP"        // string
	ConfigKeyPort           = "port"           // int
	ConfigKeyMasterAddr     = "masterAddr"     // array
	ConfigKeyZone           = "zoneName"       // string
	ConfigKeyDisks          = "disks"          // array
	ConfigKeyRaftDir        = "raftDir"        // string
	ConfigKeyRaftHeartbeat  = "raftHeartbeat"  // string
	ConfigKeyRaftReplica    = "raftReplica"    // string
	cfgTickIntervalMs       = "tickIntervalMs" // int
	ConfigKeyMasterLBDomain = "masterLBDomain"
	ConfigKeyEnableRootDisk = "enableRootDisk"
)

// DataNode defines the structure of a data node.
type DataNode struct {
	space                    *SpaceManager
	port                     string
	httpPort                 string
	zoneName                 string
	medium                   proto.MediumType
	clusterID                string
	localIP                  string
	localServerAddr          string
	nodeID                   uint64
	raftDir                  string
	raftHeartbeat            string
	raftReplica              string
	raftStore                raftstore.RaftStore
	tickInterval             int
	tcpListener              net.Listener
	stopC                    chan bool
	fixTinyDeleteRecordLimit uint64
	control                  common.Control
	processStatInfo          *statinfo.ProcessStatInfo
	topoManager              *topology.TopologyManager
	transferDeleteLock       sync.Mutex
	settings                 *settings.KeyValues
}

func NewServer() *DataNode {
	return &DataNode{}
}

func (s *DataNode) Start(cfg *config.Config) (err error) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	return s.control.Start(s, cfg, doStart)
}

// Shutdown shuts down the current data node.
func (s *DataNode) Shutdown() {
	s.control.Shutdown(s, doShutdown)
}

// Sync keeps data node in sync.
func (s *DataNode) Sync() {
	s.control.Sync()
}

// Workflow of starting up a data node.
func doStart(server common.Server, cfg *config.Config) (err error) {
	s, ok := server.(*DataNode)
	if !ok {
		return errors.New("Invalid Node Type!")
	}
	s.stopC = make(chan bool, 0)
	if err = s.parseSysStartTime(); err != nil {
		return
	}
	// parse the config file
	if err = s.parseConfig(cfg); err != nil {
		return
	}

	var settingPath = path.Join(getBasePath(), DataSettingsFile)
	if s.settings, err = settings.OpenKeyValues(settingPath); err != nil {
		return
	}

	repl.SetConnectPool(gConnPool)
	if err = s.register(); err != nil {
		err = fmt.Errorf("regiter failed: %v", err)
		return
	}
	exporter.Init(exporter.NewOptionFromConfig(cfg).WithCluster(s.clusterID).WithModule(ModuleName).WithZone(s.zoneName))

	_, err = multirate.InitLimiterManager(s.clusterID, multirate.ModuleDataNode, s.zoneName, MasterLBDomainClient.AdminAPI().GetLimitInfo)
	if err != nil {
		return err
	}
	s.topoManager = topology.NewTopologyManager(0, 0, MasterClient, MasterLBDomainClient,
		false, false)
	if err = s.topoManager.Start(); err != nil {
		return
	}

	// start the raft server
	if err = s.startRaftServer(cfg); err != nil {
		return
	}

	// create space manager (disk, partition, etc.)
	if err = s.startSpaceManager(cfg); err != nil {
		exporter.Warning(err.Error())
		return
	}

	// start tcp listening
	if err = s.startTCPService(); err != nil {
		return
	}

	log.LogErrorf("doStart startTCPService finish")

	// Start all loaded data partitions which managed by space manager,
	// this operation will start raft partitions belong to data partitions.
	s.space.StartPartitions()

	async.RunWorker(s.space.AsyncLoadExtent)
	async.RunWorker(s.registerHandler)
	async.RunWorker(s.startUpdateNodeInfo)
	async.RunWorker(s.startUpdateProcessStatInfo)

	statistics.InitStatistics(cfg, s.clusterID, statistics.ModelDataNode, s.zoneName, LocalIP, s.rangeMonitorData)

	return
}

func doShutdown(server common.Server) {
	s, ok := server.(*DataNode)
	if !ok {
		return
	}
	close(s.stopC)
	s.space.Stop()
	s.stopUpdateNodeInfo()
	s.stopTCPService()
	s.stopRaftServer()
	if gHasFinishedLoadDisks {
		deleteSysStartTimeFile()
	}
	multirate.Stop()
}

func (s *DataNode) parseConfig(cfg *config.Config) (err error) {
	var (
		port       string
		regexpPort *regexp.Regexp
	)
	LocalIP = cfg.GetString(ConfigKeyLocalIP)
	port = cfg.GetString(proto.ListenPort)
	LocalServerPort = port
	if regexpPort, err = regexp.Compile("^(\\d)+$"); err != nil {
		return fmt.Errorf("Err:no port")
	}
	if !regexpPort.MatchString(port) {
		return fmt.Errorf("Err:port must string")
	}
	s.port = port
	s.httpPort = cfg.GetString(proto.HttpPort)

	masterDomain, masterAddrs := s.parseMasterAddrs(cfg)
	if len(masterAddrs) == 0 {
		return fmt.Errorf("Err:masterAddr unavalid")
	}

	for _, ip := range masterAddrs {
		MasterClient.AddNode(ip)
	}
	MasterClient.SetMasterDomain(masterDomain)

	masterLBDomain := s.parseMasterLBDomain(cfg)
	if masterLBDomain != "" {
		MasterLBDomainClient.AddNode(masterLBDomain)
	}
	s.zoneName = cfg.GetString(ConfigKeyZone)
	if s.zoneName == "" {
		s.zoneName = DefaultZoneName
	}
	s.medium = proto.ParseMediumTypeFromZoneName(s.zoneName)

	s.tickInterval = int(cfg.GetFloat(cfgTickIntervalMs))
	if s.tickInterval <= 300 {
		log.LogWarnf("DataNode: get config %s(%v) less than 300 so set it to 500 ", cfgTickIntervalMs, cfg.GetString(cfgTickIntervalMs))
		s.tickInterval = 500
	}

	log.LogInfof("DataNode: parse config: masterAddrs %v ", MasterClient.Nodes())
	log.LogInfof("DataNode: parse config: master domain %s", masterDomain)
	log.LogInfof("DataNode: parse config: master lb domain %s", masterLBDomain)
	log.LogInfof("DataNode: parse config: port %v", s.port)
	log.LogInfof("DataNode: parse config: zoneName %v ", s.zoneName)
	return
}

func (s *DataNode) parseMasterAddrs(cfg *config.Config) (masterDomain string, masterAddrs []string) {
	var err error
	masterDomain = cfg.GetString(proto.MasterDomain)
	if masterDomain != "" && !strings.Contains(masterDomain, ":") {
		masterDomain = masterDomain + ":" + proto.MasterDefaultPort
	}

	masterAddrs, err = iputil.LookupHost(masterDomain)
	if err != nil {
		masterAddrs = cfg.GetStringSlice(proto.MasterAddr)
	}
	return
}

func (s *DataNode) parseMasterLBDomain(cfg *config.Config) (masterLBDomain string) {
	return cfg.GetString(proto.MasterLBDomain)
}

// parseSysStartTime maybeServerFaultOccurred is set true only in these two occasions:
// system power off, then restart
// kill -9 the program, then reboot or power off, then restart
func (s *DataNode) parseSysStartTime() (err error) {
	baseDir := getBasePath()
	sysStartFile := path.Join(baseDir, SystemStartTimeFile)
	if _, err = os.Stat(sysStartFile); err != nil {
		if !os.IsNotExist(err) {
			return
		}
		maybeServerFaultOccurred = false
		if err = initSysStartTimeFile(); err != nil {
			log.LogErrorf("parseSysStartTime set system start time has err:%v", err)
		}
	} else {
		bs, err := ioutil.ReadFile(sysStartFile)
		if err != nil {
			return err
		}
		if len(bs) == 0 {
			maybeServerFaultOccurred = false
			if err = initSysStartTimeFile(); err != nil {
				log.LogErrorf("parseSysStartTime set system start time has err:%v", err)
			}
			return err
		}
		localSysStart, err := strconv.ParseInt(strings.TrimSpace(string(bs)), 10, 64)
		if err != nil {
			return err
		}
		newSysStart, err := cpu.SysStartTime()
		if err != nil {
			return err
		}
		log.LogInfof("DataNode: load sys start time: record %d, current %d", localSysStart, newSysStart)

		if maybeServerFaultOccurred = newSysStart-localSysStart > MAX_OFFSET_OF_TIME; maybeServerFaultOccurred {
			log.LogWarnf("DataNode: the program may be started after power off, record %d, current %d", localSysStart, newSysStart)
		}
	}
	return
}

func (s *DataNode) startSpaceManager(cfg *config.Config) (err error) {
	s.space = NewSpaceManager(s)
	if len(strings.TrimSpace(s.port)) == 0 {
		err = ErrNewSpaceManagerFailed
		return
	}

	var limitInfo *proto.LimitInfo
	limitInfo, err = MasterLBDomainClient.AdminAPI().GetLimitInfo("")
	if err == nil && limitInfo != nil {
		s.space.SetConsistencyMode(limitInfo.DataPartitionConsistencyMode)
		s.space.SetPersistenceMode(limitInfo.PersistenceMode)
		s.space.SetTrashKeepTimeSec(limitInfo.DataNodeTrashKeepTimeSec)
		s.space.SetDiskReservedRatio(limitInfo.DataNodeDiskReservedRatio)
	}

	s.space.SetRaftStore(s.raftStore)
	s.space.SetNodeID(s.nodeID)
	s.space.SetClusterID(s.clusterID)

	var startTime = time.Now()

	// Prepare and validate disk config
	var getDeviceID = func(path string) (devID uint64, err error) {
		var stat = new(syscall.Stat_t)
		if err = syscall.Stat(path, stat); err != nil {
			return
		}
		devID = stat.Dev
		return
	}

	var rootDevID uint64
	if rootDevID, err = getDeviceID("/"); err != nil {
		return
	}
	log.LogInfof("root device: / (%v)", rootDevID)

	var diskPaths = make(map[uint64]*DiskPath) // DevID -> DiskPath
	for _, d := range cfg.GetSlice(ConfigKeyDisks) {
		var diskPath, ok = ParseDiskPath(d.(string))
		if !ok {
			err = fmt.Errorf("invalid disks configuration: %v", d)
			return
		}
		var devID, derr = getDeviceID(diskPath.Path())
		if derr != nil {
			log.LogErrorf("get device ID for %v failed: %v", diskPath.Path(), derr)
			continue
		}
		if !cfg.GetBool(ConfigKeyEnableRootDisk) && devID == rootDevID {
			err = fmt.Errorf("root device in disks configuration: %v (%v), ", d, devID)
			return
		}
		if p, exists := diskPaths[devID]; exists {
			err = fmt.Errorf("dependent device in disks configuration: [%v,%v]", d, p.Path())
			return
		}

		log.LogInfof("disk device: %v, path %v, device %v", d, diskPath.Path(), devID)
		diskPaths[devID] = diskPath
	}

	var checkExpired CheckExpired
	var requires, fetchErr = s.fetchPersistPartitionIDsFromMaster()
	if fetchErr == nil {
		checkExpired = func(id uint64) bool {
			if len(requires) == 0 {
				return true
			}
			for _, existId := range requires {
				if existId == id {
					return false
				}
			}
			return true
		}
	}

	var futures = make(map[string]*async.Future)
	for devID, diskPath := range diskPaths {
		var future = async.NewFuture()
		go func(path string, future *async.Future) {
			if log.IsInfoEnabled() {
				log.LogInfof("SPCMGR: loading disk: devID=%v, path=%v", devID, diskPath)
			}
			var err = s.space.LoadDisk(path, checkExpired)
			future.Respond(nil, err)
		}(diskPath.Path(), future)
		futures[diskPath.Path()] = future
	}
	for p, future := range futures {
		if _, ferr := future.Response(); ferr != nil {
			log.LogErrorf("load disk [%v] failed: %v", p, ferr)
			continue
		}
	}

	// Check missed partitions
	var misses = make([]uint64, 0)
	for _, id := range requires {
		if dp := s.space.Partition(id); dp == nil {
			misses = append(misses, id)
		}
	}
	if len(misses) > 0 {
		err = fmt.Errorf("lack partitions: %v", misses)
		return
	}

	deleteSysStartTimeFile()
	_ = initSysStartTimeFile()
	maybeServerFaultOccurred = false

	gHasFinishedLoadDisks = true
	log.LogInfof("SPCMGR: loaded all %v disks elapsed %v", len(diskPaths), time.Since(startTime))
	return nil
}

func (s *DataNode) fetchPersistPartitionIDsFromMaster() (ids []uint64, err error) {
	var dataNode *proto.DataNodeInfo
	for i := 0; i < 3; i++ {
		dataNode, err = MasterClient.NodeAPI().GetDataNode(s.localServerAddr)
		if err != nil {
			log.LogErrorf("DataNode: fetch node info from master failed: %v", err)
			continue
		}
		break
	}
	if err != nil {
		return
	}
	ids = dataNode.PersistenceDataPartitions
	return
}

// registers the data node on the master to report the information such as IsIPV4 address.
// The startup of a data node will be blocked until the registration succeeds.
func (s *DataNode) register() (err error) {
	var (
		regInfo = &masterSDK.RegNodeInfoReq{
			Role:     proto.RoleData,
			ZoneName: s.zoneName,
			Version:  DataNodeLatestVersion,
			ProfPort: s.httpPort,
			SrvPort:  s.port,
		}
		regRsp *proto.RegNodeRsp
	)

	for retryCount := registerMaxRetryCount; retryCount > 0; retryCount-- {
		regRsp, err = MasterClient.RegNodeInfo(proto.AuthFilePath, regInfo)
		if err == nil {
			break
		}
		time.Sleep(registerRetryWaitInterval)
	}

	if err != nil {
		log.LogErrorf("DataNode register failed: %v", err)
		return
	}
	ipAddr := strings.Split(regRsp.Addr, ":")[0]
	if !unit.IsIPV4(ipAddr) {
		err = fmt.Errorf("got invalid local IP %v fetched from Master", ipAddr)
		return
	}
	s.clusterID = regRsp.Cluster
	if LocalIP == "" {
		LocalIP = ipAddr
	}
	s.localServerAddr = fmt.Sprintf("%s:%v", LocalIP, s.port)
	s.nodeID = regRsp.Id
	if err = iputil.VerifyLocalIP(LocalIP); err != nil {
		log.LogErrorf("DataNode register verify local ip failed: %v", err)
		return
	}
	return
}

func (s *DataNode) startTCPService() (err error) {
	log.LogInfo("Start: startTCPService")
	addr := fmt.Sprintf(":%v", s.port)
	l, err := net.Listen(NetworkProtocol, addr)
	log.LogDebugf("action[startTCPService] listen %v address(%v).", NetworkProtocol, addr)
	if err != nil {
		log.LogError("failed to listen, err:", err)
		return
	}
	s.tcpListener = l
	go func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.LogErrorf("action[startTCPService] failed to accept, err:%s", err.Error())
				time.Sleep(time.Second * 5)
				continue
			}
			log.LogDebugf("action[startTCPService] accept connection from %s.", conn.RemoteAddr().String())
			go s.serveConn(conn)
		}
	}(l)
	return
}

func (s *DataNode) stopTCPService() (err error) {
	if s.tcpListener != nil {

		s.tcpListener.Close()
		log.LogDebugf("action[stopTCPService] stop tcp service.")
	}
	return
}

func (s *DataNode) serveConn(conn net.Conn) {
	space := s.space
	space.Stats().AddConnection()
	c, _ := conn.(*net.TCPConn)
	c.SetKeepAlive(true)
	c.SetNoDelay(true)
	packetProcessor := repl.NewReplProtocol(c, s.Prepare, s.OperatePacket, s.Post)
	packetProcessor.ServerConn()
}

// Increase the disk error count by one.
func (s *DataNode) incDiskErrCnt(partitionID uint64, err error, flag uint8) {
	if err == nil {
		return
	}
	dp := s.space.Partition(partitionID)
	if dp == nil {
		return
	}
	d := dp.Disk()
	if d == nil {
		return
	}
	if !IsDiskErr(err) {
		return
	}
	if flag == WriteFlag {
		d.incWriteErrCnt()
	} else if flag == ReadFlag {
		d.incReadErrCnt()
	}
}

var (
	staticReflectedErrnoType = reflect.TypeOf(syscall.Errno(0))
)

func IsSysErr(err error) (is bool) {
	if err == nil {
		return
	}
	return reflect.TypeOf(err) == staticReflectedErrnoType
}

func IsDiskErr(err error) bool {
	return err != nil &&
		(strings.Contains(err.Error(), syscall.EIO.Error()) ||
			strings.Contains(err.Error(), syscall.EROFS.Error()))
}

func (s *DataNode) rangeMonitorData(deal func(data *statistics.MonitorData, vol, path string, pid uint64)) {
	s.space.WalkDisks(func(disk *Disk) bool {
		for _, md := range disk.monitorData {
			deal(md, "", disk.Path, 0)
		}
		return true
	})

	s.space.WalkPartitions(func(partition *DataPartition) bool {
		for _, md := range partition.monitorData {
			deal(md, partition.volumeID, partition.Disk().Path, partition.partitionID)
		}
		return true
	})
}

func getBasePath() string {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		panic(err)
	}
	return dir
}

func initSysStartTimeFile() (err error) {
	baseDir := getBasePath()
	sysStartTime, err := cpu.SysStartTime()
	if err != nil {
		return
	}
	if err = ioutil.WriteFile(path.Join(baseDir, SystemStartTimeFile), []byte(strconv.FormatUint(uint64(sysStartTime), 10)), os.ModePerm); err != nil {
		return err
	}
	return
}

func deleteSysStartTimeFile() {
	baseDir := getBasePath()
	os.Remove(path.Join(baseDir, SystemStartTimeFile))
}

const (
	DefaultMarkDeleteLimitBurst           = 512
	UpdateNodeBaseInfoTicket              = 1 * time.Minute
	DefaultFixTinyDeleteRecordLimitOnDisk = 1
	DefaultNormalExtentDeleteExpireTime   = 4 * 3600
	DefaultLazyLoadParallelismPerDisk     = 2
	DefaultTrashKeepTimeSec               = int64(3 * 24 * 60 * 60)
)

var (
	nodeInfoStopC = make(chan struct{}, 0)
	logMaxSize    uint64
)

func (s *DataNode) startUpdateNodeInfo() {
	updateNodeBaseInfoTicker := time.NewTicker(UpdateNodeBaseInfoTicket)
	defer func() {
		updateNodeBaseInfoTicker.Stop()
	}()

	// call once on init before first tick
	s.updateNodeBaseInfo()
	for {
		select {
		case <-nodeInfoStopC:
			log.LogInfo("datanode nodeinfo goroutine stopped")
			return
		case <-updateNodeBaseInfoTicker.C:
			s.updateNodeBaseInfo()
		}
	}
}

func (s *DataNode) stopUpdateNodeInfo() {
	nodeInfoStopC <- struct{}{}
}

func (s *DataNode) updateNodeBaseInfo() {
	limitInfo, err := MasterClient.AdminAPI().GetLimitInfo("")
	limitInfo, err := MasterLBDomainClient.AdminAPI().GetLimitInfo("")
	if err != nil {
		log.LogWarnf("[updateNodeBaseInfo] get limit info err: %s", err.Error())
		return
	}

	s.space.SetDiskFixTinyDeleteRecordLimit(limitInfo.DataNodeFixTinyDeleteRecordLimitOnDisk)
	s.space.SetForceFlushFDInterval(limitInfo.DataNodeFlushFDInterval)
	s.space.SetSyncWALOnUnstableEnableState(limitInfo.DataSyncWALOnUnstableEnableState)
	s.space.SetForceFlushFDParallelismOnDisk(limitInfo.DataNodeFlushFDParallelismOnDisk)
	s.space.SetNormalExtentDeleteExpireTime(limitInfo.DataNodeNormalExtentDeleteExpire)
	s.space.SetConsistencyMode(limitInfo.DataPartitionConsistencyMode)
	s.space.SetPersistenceMode(limitInfo.PersistenceMode)

	if statistics.StatisticsModule != nil {
		statistics.StatisticsModule.UpdateMonitorSummaryTime(limitInfo.MonitorSummarySec)
		statistics.StatisticsModule.UpdateMonitorReportTime(limitInfo.MonitorReportSec)
	}

	s.updateLogMaxSize(limitInfo.LogMaxSize)

	if s.topoManager != nil {
		s.topoManager.UpdateFetchTimerIntervalMin(limitInfo.TopologyFetchIntervalMin, limitInfo.TopologyForceFetchIntervalSec)
	}

	s.space.SetTrashKeepTimeSec(limitInfo.DataNodeTrashKeepTimeSec)
	s.space.SetDiskReservedRatio(limitInfo.DataNodeDiskReservedRatio)
}

func (s *DataNode) updateLogMaxSize(val uint64) {
	if val != 0 && logMaxSize != val {
		oldLogMaxSize := logMaxSize
		logMaxSize = val
		log.SetLogMaxSize(int64(val))
		log.LogInfof("updateLogMaxSize, logMaxSize(old:%v, new:%v)", oldLogMaxSize, logMaxSize)
	}
}

func (s *DataNode) startUpdateProcessStatInfo() {
	s.processStatInfo = statinfo.NewProcessStatInfo()
	s.processStatInfo.ProcessStartTime = time.Now().Format("2006-01-02 15:04:05")
	go s.processStatInfo.UpdateStatInfoSchedule()
}

func (s *DataNode) parseRaftConfig(cfg *config.Config) (err error) {
	s.raftDir = cfg.GetString(ConfigKeyRaftDir)
	if s.raftDir == "" {
		return fmt.Errorf("bad raftDir config")
	}
	s.raftHeartbeat = cfg.GetString(ConfigKeyRaftHeartbeat)
	s.raftReplica = cfg.GetString(ConfigKeyRaftReplica)
	log.LogDebugf("[parseRaftConfig] load raftDir(%v).", s.raftDir)
	log.LogDebugf("[parseRaftConfig] load raftHearbeat(%v).", s.raftHeartbeat)
	log.LogDebugf("[parseRaftConfig] load raftReplica(%v).", s.raftReplica)
	return
}

func (s *DataNode) startRaftServer(cfg *config.Config) (err error) {
	log.LogInfo("Start: startRaftServer")

	s.parseRaftConfig(cfg)

	constCfg := config.ConstConfig{
		Listen:           s.port,
		RaftHeartbetPort: s.raftHeartbeat,
		RaftReplicaPort:  s.raftReplica,
	}
	var ok = false
	if ok, err = config.CheckOrStoreConstCfg(s.raftDir, config.DefaultConstConfigFile, &constCfg); !ok {
		log.LogErrorf("constCfg check failed %v %v %v %v", s.raftDir, config.DefaultConstConfigFile, constCfg, err)
		return fmt.Errorf("constCfg check failed %v %v %v %v", s.raftDir, config.DefaultConstConfigFile, constCfg, err)
	}

	if _, err = os.Stat(s.raftDir); err != nil {
		if err = os.MkdirAll(s.raftDir, 0755); err != nil {
			err = utilErrors.NewErrorf("create raft server dir: %s", err.Error())
			log.LogErrorf("action[startRaftServer] cannot start raft server err(%v)", err)
			return
		}
	}

	heartbeatPort, err := strconv.Atoi(s.raftHeartbeat)
	if err != nil {
		err = utilErrors.NewErrorf("Raft heartbeat port configuration error: %s", err.Error())
		return
	}
	replicatePort, err := strconv.Atoi(s.raftReplica)
	if err != nil {
		err = utilErrors.NewErrorf("Raft replica port configuration error: %s", err.Error())
		return
	}

	raftConf := &raftstore.Config{
		NodeID:            s.nodeID,
		RaftPath:          s.raftDir,
		TickInterval:      s.tickInterval,
		IPAddr:            LocalIP,
		HeartbeatPort:     heartbeatPort,
		ReplicaPort:       replicatePort,
		NumOfLogsToRetain: DefaultRaftLogsToRetain,
	}
	s.raftStore, err = raftstore.NewRaftStore(raftConf)
	if err != nil {
		err = utilErrors.NewErrorf("new raftStore: %s", err.Error())
		log.LogErrorf("action[startRaftServer] cannot start raft server err(%v)", err)
	}

	return
}

func (s *DataNode) stopRaftServer() {
	if s.raftStore != nil {
		s.raftStore.Stop()
	}
}

func (s *DataNode) asyncLoadDataPartition(task *proto.AdminTask) {
	var (
		err error
	)
	request := &proto.LoadDataPartitionRequest{}
	response := &proto.LoadDataPartitionResponse{}
	if task.OpCode == proto.OpLoadDataPartition {
		bytes, _ := json.Marshal(task.Request)
		json.Unmarshal(bytes, request)
		dp := s.space.Partition(request.PartitionId)
		if dp == nil {
			response.Status = proto.TaskFailed
			response.PartitionId = uint64(request.PartitionId)
			err = fmt.Errorf(fmt.Sprintf("DataPartition(%v) not found", request.PartitionId))
			response.Result = err.Error()
		} else {
			response = dp.Load()
			response.PartitionId = uint64(request.PartitionId)
			response.Status = proto.TaskSucceeds
		}
	} else {
		response.PartitionId = uint64(request.PartitionId)
		response.Status = proto.TaskFailed
		err = fmt.Errorf("illegal opcode")
		response.Result = err.Error()
	}
	task.Response = response
	if err = MasterClient.NodeAPI().ResponseDataNodeTask(task); err != nil {
		err = utilErrors.Trace(err, "load DataPartition failed,PartitionID(%v)", request.PartitionId)
		log.LogError(utilErrors.Stack(err))
	}
}
