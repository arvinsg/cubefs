package cfs

import (
	"fmt"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/http_client"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/checktool/ump"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	cfgFixBadPartition         = "fixBadPartition"
	cfgUmpAPiToken             = "umpToken"
	dpNotRecoverEndPoint       = "spark_master_dp_has_not_recover"
	dpFixBadReplicaNumEndPoint = "spark_master_dp_replica_num"
	umpOpenAPiDomain           = "open.ump.jd.com"
	alarmRecordsMethod         = "/alarm/records"
)

var notRecoverDpRegex = regexp.MustCompile("#[1-9][0-9]*:")

var fixPartitionMap = make(map[uint64]time.Time, 0)

func (s *ChubaoFSMonitor) fixOnlineBadDataPartition() {
	if !s.fixBadPartition {
		return
	}
	for _, host := range s.hosts {
		if host.host != DomainSpark {
			continue
		}
		log.LogInfof("fixOnlineBadDataPartition started")
		host.fixPartitions(s.umpClient, dpFixBadReplicaNumEndPoint, parseBadReplicaNumPartition, host.fixBadReplicaNumPartition)
		host.fixPartitions(s.umpClient, dpNotRecoverEndPoint, parseNotRecoverPartition, host.fix24HourNotRecoverPartition)
		log.LogInfof("fixOnlineBadDataPartition finished")
	}
}

func (ch *ClusterHost) fixPartitions(umpClient *ump.UMPClient, endPoint string, parseFunc func(string) []uint64, fixPartitionFunc func(partition uint64)) {
	var err error
	var badPartitions []uint64
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("action[fixPartitions] recover from panic:%v", r)
		}
		if err != nil {
			log.LogErrorf("action[fixPartitions] err:%v", err)
		}
	}()
	log.LogInfo("action[fixPartitions] start")
	badPartitions, err = parseBadPartitionIDsFromUmpRecord(umpClient, endPoint, parseFunc)
	if err != nil {
		return
	}
	if len(badPartitions) == 0 {
		return
	}
	log.LogWarnf("action[fixPartitions] domain[%v] found %v bad partitions:%v, start fix", ch.host, len(badPartitions), badPartitions)
	for _, partition := range badPartitions {
		if _, ok := fixPartitionMap[partition]; !ok {
			fixPartitionMap[partition] = time.Now()
		} else {
			if time.Since(fixPartitionMap[partition]) < time.Minute*10 {
				continue
			}
			fixPartitionMap[partition] = time.Now()
		}
		fixPartitionFunc(partition)
	}
	return
}

func parseBadPartitionIDsFromUmpRecord(umpClient *ump.UMPClient, endPoint string, parseFunc func(string) []uint64) (ids []uint64, err error) {
	var alarmRecords *ump.AlarmRecordResponse
	ids = make([]uint64, 0)
	idsMap := make(map[uint64]bool, 0)
	alarmRecords, err = umpClient.GetAlarmRecords(alarmRecordsMethod, "chubaofs-node", "jdos", endPoint, time.Now().UnixMilli()-60*10*1000, time.Now().UnixMilli())
	if err != nil {
		return
	}
	for _, r := range alarmRecords.Records {
		pids := parseFunc(r.Content)
		for _, pid := range pids {
			if pid > 0 {
				idsMap[pid] = true
			}
		}
	}
	for id := range idsMap {
		ids = append(ids, id)
	}
	return ids, nil
}

func parseBadReplicaNumPartition(content string) (pids []uint64) {
	pids = make([]uint64, 0)
	if !strings.Contains(content, "FIX DataPartition replicaNum") {
		return
	}
	log.LogWarnf("action[parseBadReplicaNumPartition] content:%v", content)
	tmp := strings.Split(content, "partitionID:")[1]
	pidStr := strings.Split(tmp, " ")[0]
	if pidStr == "" {
		return
	}
	pid, err := strconv.ParseUint(pidStr, 10, 64)
	if err != nil {
		log.LogErrorf("parse partition id failed:%v", err)
		return
	}
	pids = append(pids, pid)
	return
}

// eg. action[checkDiskRecoveryProgress] clusterID[spark],has[1] has offlined more than 24 hours,still not recovered,ids[map[72392:1700900266]]
func parseNotRecoverPartition(content string) (dps []uint64) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("recover from panic:%v", r)
		}
	}()
	log.LogWarnf("action[parseNotRecoverPartition] content:%v", content)
	if !strings.Contains(content, "has offlined more than 24 hours") && !strings.Contains(content, "has migrated more than 24 hours") {
		fmt.Printf("1")
		return
	}
	dpStrArr := notRecoverDpRegex.FindAllString(content, -1)
	dps = make([]uint64, 0)
	for _, s := range dpStrArr {
		dp, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(s, "#"), ":"), 10, 64)
		if err != nil {
			log.LogErrorf("action[parseNotRecoverPartition] err:%v, dp string:%v", err, s)
			continue
		}
		dps = append(dps, dp)
	}
	return
}

func isNeedFix(client *master.MasterClient, partition uint64) (fix bool, dp *proto.DataPartitionInfo, err error) {
	dp, err = client.AdminAPI().GetDataPartition("", partition)
	if err != nil {
		return
	}
	if dp.ReplicaNum != 2 {
		return
	}
	if len(dp.Hosts) != 1 {
		return
	}
	leader := false
	for _, r := range dp.Replicas {
		if r.IsLeader {
			leader = true
		}
	}
	if !leader {
		err = fmt.Errorf("partition:%v no leader", partition)
		return
	}
	// len(hosts)==1, retry 20s later
	time.Sleep(time.Second * 20)
	dp, err = client.AdminAPI().GetDataPartition("", partition)
	if err != nil {
		return
	}
	if len(dp.Hosts) != 1 {
		return
	}
	fix = true
	return
}

func (ch *ClusterHost) fixBadReplicaNumPartition(partition uint64) {
	var (
		dn         *proto.DataNodeInfo
		dp         *proto.DataPartitionInfo
		err        error
		needFix    bool
		extraHost  string
		remainHost string
	)
	defer func() {
		if err != nil {
			log.LogErrorf("action[fixBadReplicaNumPartition] err:%v", err)
		}
	}()
	client := master.NewMasterClient([]string{ch.host}, false)
	if needFix, dp, err = isNeedFix(client, partition); err != nil {
		log.LogErrorf("action[fixBadReplicaNumPartition] err:%v", err)
		return
	}
	if !needFix {
		return
	}
	for _, replica := range dp.Replicas {
		if replica.Addr == dp.Hosts[0] {
			continue
		}
		extraHost = replica.Addr
		break
	}
	remainHost = dp.Hosts[0]

	dn, err = client.NodeAPI().GetDataNode(extraHost)
	if err != nil {
		log.LogErrorf("action[fixBadReplicaNumPartition] err:%v", err)
		return
	}
	allNodeViews := make([]proto.NodeView, 0)
	topologyView, err := client.AdminAPI().GetTopology()
	if err != nil {
		return
	}
	for _, zone := range topologyView.Zones {
		if zone.Name != dn.ZoneName {
			continue
		}
		for _, ns := range zone.NodeSet {
			allNodeViews = append(allNodeViews, ns.DataNodes...)
		}
	}
	log.LogInfof("action[fixBadReplicaNumPartition] try to add learner to fix one replica partition:%v", partition)
	retry := 20
	for i := 0; i < retry; i++ {
		rand.Seed(time.Now().UnixNano())
		index := rand.Intn(len(allNodeViews) - 1)
		destNode := allNodeViews[index]
		if destNode.Addr == extraHost || destNode.Addr == remainHost {
			continue
		}
		var destNodeView *proto.DataNodeInfo
		destNodeView, err = client.NodeAPI().GetDataNode(destNode.Addr)
		if err != nil {
			log.LogErrorf("action[fixBadReplicaNumPartition] err:%v", err)
			continue
		}
		if destNodeView.UsageRatio > 0.8 {
			continue
		}
		if !destNodeView.IsActive {
			continue
		}
		err = client.AdminAPI().AddDataLearner(partition, destNode.Addr, true, 90)
		if err != nil {
			log.LogErrorf("action[fixBadReplicaNumPartition] partition:%v, err:%v", partition, err)
			break
		}
		warnBySpecialUmpKeyWithPrefix(UMPCFSSparkFixPartitionKey, fmt.Sprintf("Domain[%v] fix one replica partition:%v success, add learner:%v", ch.host, partition, destNode.Addr))
		break
	}
	return
}

func (ch *ClusterHost) fix24HourNotRecoverPartition(partition uint64) {
	var (
		err error
		dp  *proto.DataPartitionInfo
	)
	defer func() {
		if err != nil {
			log.LogErrorf("action[fix24HourNotRecoverPartition] partition:%v err:%v", partition, err)
		}
	}()
	client := master.NewMasterClient([]string{ch.host}, false)
	dp, err = client.AdminAPI().GetDataPartition("", partition)
	if err != nil {
		return
	}
	if !dp.IsRecover {
		err = fmt.Errorf("partition has recovered")
		return
	}
	if len(dp.Replicas) == 0 {
		err = fmt.Errorf("replicas number is 0")
		return
	}
	if len(dp.MissingNodes) > 0 {
		err = fmt.Errorf("missing nodes:%v", dp.MissingNodes)
		return
	}
	err = fixSizeNoEqual(dp, ch.host)
}

func fixSizeNoEqual(dp *proto.DataPartitionInfo, host string) (err error) {
	var (
		minReplica *proto.DataReplica
		maxReplica *proto.DataReplica
	)
	minReplica = dp.Replicas[0]
	maxReplica = dp.Replicas[0]
	for _, r := range dp.Replicas {
		if r.Used == 0 {
			err = fmt.Errorf("used size is 0, maybe restarted not long ago")
			return
		}
		if r.IsRecover {
			err = fmt.Errorf("extent is in repairing")
			return
		}
		if r.Used < minReplica.Used {
			minReplica = r
		}
		if r.Used > maxReplica.Used {
			maxReplica = r
		}
	}

	var maxSum, minSum uint64
	maxSum, err = sumTinyAvailableSize(maxReplica.Addr, dp.PartitionID)
	if err != nil {
		return
	}
	minSum, err = sumTinyAvailableSize(minReplica.Addr, dp.PartitionID)
	if err != nil {
		return
	}
	var reason, execute string
	if maxSum < minSum+unit.GB*9/10 {
		partitionPath := fmt.Sprintf("datapartition_%v_%v", dp.PartitionID, maxReplica.Total)
		err = stopReloadReplica(maxReplica.Addr, dp.PartitionID, partitionPath)
		if err != nil {
			log.LogErrorf("action[fix24HourNotRecoverPartition] stopReloadReplica max replica failed:%v", err)
		}
		time.Sleep(time.Second * 10)
		err = stopReloadReplica(minReplica.Addr, dp.PartitionID, partitionPath)
		if err != nil {
			log.LogErrorf("action[fix24HourNotRecoverPartition] stopReloadReplica min replica failed:%v", err)
		}
		reason = "size calculation is wrong"
		execute = "stop-reload data partition"
	} else if maxSum-minSum >= unit.GB {
		err = playBackTinyDeleteRecord(maxReplica.Addr, dp.PartitionID)
		reason = "tiny extent deletion is not synchronized"
		execute = "playback tiny extent delete record"
	} else {
		//todo decommission dp with smaller used size
		reason = "normal extent repair after deleted, decommission replica with smaller size"
		execute = "decommission replicas with smaller used size"
	}
	warnBySpecialUmpKeyWithPrefix(UMPCFSSparkFixPartitionKey, fmt.Sprintf("Domain[%v] fix used size, partition(%v), replica(%v), reason(%s), execute(%s)", host, dp.PartitionID, maxReplica.Addr, reason, execute))
	return
}

func sumTinyAvailableSize(addr string, partitionID uint64) (sum uint64, err error) {
	dHost := fmt.Sprintf("%v:%v", strings.Split(addr, ":")[0], profPortMap[strings.Split(addr, ":")[1]])
	dataClient := http_client.NewDataClient(dHost, false)
	for i := uint64(1); i <= uint64(64); i++ {
		var extentHoleInfo *proto.DNTinyExtentInfo
		extentHoleInfo, err = dataClient.GetExtentHoles(partitionID, i)
		if err != nil {
			return
		}
		sum += extentHoleInfo.ExtentAvaliSize
	}
	return sum, nil
}

func stopReloadReplica(addr string, partitionID uint64, partitionPath string) (err error) {
	dHost := fmt.Sprintf("%v:%v", strings.Split(addr, ":")[0], profPortMap[strings.Split(addr, ":")[1]])
	dataClient := http_client.NewDataClient(dHost, false)
	partition, err := dataClient.GetPartitionSimple(partitionID)
	if err != nil {
		return
	}
	err = dataClient.StopPartition(partitionID)
	if err != nil {
		return err
	}
	time.Sleep(time.Second * 10)
	for i := 0; i < 3; i++ {
		if err = dataClient.ReLoadPartition(partitionPath, strings.Split(partition.Path, "/datapartition")[0]); err == nil {
			break
		}
	}
	return
}

func playBackTinyDeleteRecord(addr string, partitionID uint64) (err error) {
	dHost := fmt.Sprintf("%v:%v", strings.Split(addr, ":")[0], profPortMap[strings.Split(addr, ":")[1]])
	dataClient := http_client.NewDataClient(dHost, false)
	err = dataClient.PlaybackPartitionTinyDelete(partitionID)
	return err
}
