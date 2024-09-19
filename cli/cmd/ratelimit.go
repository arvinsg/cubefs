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

package cmd

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/multirate"
	"github.com/spf13/cobra"
)

const (
	cmdRateLimitUse       = "ratelimit [COMMAND]"
	cmdRateLimitShort     = "Manage requests rate limit"
	cmdRateLimitInfoShort = "Current rate limit"
	cmdRateLimitSetShort  = "Set rate limit"
	minRate               = 100
	minPartRate           = 1

	minMonitorSummarySeconds = 5
	minMonitorReportSeconds  = 10
	clusterDbBack            = "cfs_dbBack"
)

func newRateLimitCmd(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   cmdRateLimitUse,
		Short: cmdRateLimitShort,
	}
	cmd.AddCommand(
		newRateLimitInfoCmd(client),
		//newRateLimitSetCmd(client),
	)
	return cmd
}

func newRateLimitInfoCmd(client *master.MasterClient) *cobra.Command {
	var vol string
	var cmd = &cobra.Command{
		Use:   CliOpInfo,
		Short: cmdRateLimitInfoShort,
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			var info *proto.LimitInfo
			if info, err = client.AdminAPI().GetLimitInfo(vol); err != nil {
				errout("Get cluster info fail:\n%v\n", err)
			}
			stdout("[Cluster rate limit]\n")
			stdout(formatRateLimitInfo(info))
			stdout("\n")
		},
	}
	cmd.Flags().StringVar(&vol, "volume", "", "volume (empty volume acts as default)")
	return cmd
}

func newRateLimitSetCmd(client *master.MasterClient) *cobra.Command {
	var (
		info          proto.RateLimitInfo
		opName        string
		rateLimitName string
	)
	var cmd = &cobra.Command{
		Use:   CliOpSet,
		Short: cmdRateLimitSetShort,
		Run: func(cmd *cobra.Command, args []string) {
			if client.IsOnline() && info.RemoteCacheBoostEnableState >= 0 {
				errout("set RemoteCacheBoostEnable for %v is not permited\n", client.Nodes())
			}

			var err error
			if (info.ClientReadVolRate > 0 && info.ClientReadVolRate < minRate) ||
				(info.ClientWriteVolRate > 0 && info.ClientWriteVolRate < minRate) ||
				(info.MetaNodeReqRate > 0 && info.MetaNodeReqRate < minRate) ||
				(info.MetaNodeReqOpRate > 0 && info.MetaNodeReqOpRate < minRate) ||
				(info.DataNodeReqRate > 0 && info.DataNodeReqRate < minRate) ||
				(info.DataNodeReqOpRate > 0 && info.DataNodeReqOpRate < minRate) ||
				(info.DataNodeReqVolOpRate > 0 && info.DataNodeReqVolOpRate < minRate) ||
				(info.DataNodeMarkDeleteRate > 0 && info.DataNodeMarkDeleteRate < minRate) {
				errout("limit rate can't be less than %d\n", minRate)
			}
			if (info.DataNodeReqVolPartRate > 0 && info.DataNodeReqVolPartRate < minPartRate) ||
				(info.DataNodeReqVolOpPartRate > 0 && info.DataNodeReqVolOpPartRate < minPartRate) {
				errout("limit rate can't be less than %d\n", minPartRate)
			}
			if info.ClientVolOpRate < -2 {
				errout("client meta op limit rate can't be less than %d\n", -1)
			}
			if info.ObjectVolActionRate < -2 {
				errout("object node action limit rate can't be less than %d\n", -2)
			}
			if info.DataNodeRepairTaskZoneCount >= 0 && info.ZoneName == "" {
				errout("if DataNodeRepairTaskZoneCount is set , ZoneName can not be empty")
			}
			if (info.MonitorSummarySecond > 0 && info.MonitorSummarySecond < minMonitorSummarySeconds) ||
				(info.MonitorReportSecond > 0 && info.MonitorReportSecond < minMonitorReportSeconds) ||
				(info.MonitorSummarySecond > info.MonitorReportSecond) {
				errout("summary seconds for monitor can't be less than %d, report seconds for monitor can't be less than monitor seconds(%d) or %d\n",
					minMonitorSummarySeconds, info.MonitorSummarySecond, minMonitorReportSeconds)
			}
			if info.NetworkFlowRatio > 100 {
				errout("networkFlowRatio can't be greater than 100\n")
			}
			if opName != "" {
				opcode := getOpCode(opName)
				if opcode < 0 {
					errout("invalid opName %v\n", opName)
				}
				info.Opcode = int64(opcode)
			}
			info.RateLimitIndex = -1
			if rateLimitName != "" {
				index := multirate.GetIndexByName(rateLimitName)
				if index < 0 {
					errout("invalid rateLimitName %v\n", rateLimitName)
				}
				info.RateLimitIndex = int64(index)
			}
			msg := ""
			if info.ClientReadVolRate >= 0 {
				msg += fmt.Sprintf("clientReadVolRate: %d, volume: %s, ", info.ClientReadVolRate, info.Volume)
			}
			if info.ClientWriteVolRate >= 0 {
				msg += fmt.Sprintf("clientWriteVolRate: %d, volume: %s, ", info.ClientWriteVolRate, info.Volume)
			}
			if info.ClientVolOpRate > -2 {
				msg += fmt.Sprintf("clientVolOpRate: %v, ", info.ClientVolOpRate)
			}
			if info.ObjectVolActionRate > -2 {
				msg += fmt.Sprintf("objectVolActionRate: %v, ", info.ObjectVolActionRate)
			}
			if info.DataNodeRepairTaskCount > 0 {
				msg += fmt.Sprintf("dataNodeRepairTaskCount: %d, ", info.DataNodeRepairTaskCount)
			}
			if info.DataNodeRepairTaskSSDZone > 0 {
				msg += fmt.Sprintf("dataNodeRepairTaskSSDZoneCount: %d, ", info.DataNodeRepairTaskSSDZone)
			}
			if info.MetaNodeReqRate >= 0 {
				msg += fmt.Sprintf("metaNodeReqRate: %d, ", info.MetaNodeReqRate)
			}
			if info.MetaNodeReqOpRate != 0 {
				msg += fmt.Sprintf("metaNodeReqOpRate: %d, opcode: %d, ", info.MetaNodeReqOpRate, info.Opcode)
			}
			if info.DataNodeRepairTaskZoneCount >= 0 {
				msg += fmt.Sprintf("DataNodeRepairTaskZoneCount: %d, zone: %s, ", info.DataNodeRepairTaskZoneCount, info.ZoneName)
			}
			if info.DataNodeMarkDeleteRate >= 0 {
				msg += fmt.Sprintf("dataNodeMarkDeleteRate: %d, ", info.DataNodeMarkDeleteRate)
			}
			if info.NetworkFlowRatio >= 0 {
				msg += fmt.Sprintf("networkFlowRatio: %d, ", info.NetworkFlowRatio)
			}
			if info.DataNodeReqRate >= 0 {
				msg += fmt.Sprintf("dataNodeReqZoneRate: %d, zone: %s, ", info.DataNodeReqRate, info.ZoneName)
			}
			if info.DataNodeReqOpRate >= 0 {
				msg += fmt.Sprintf("dataNodeReqZoneOpRate: %d, zone: %s, opcode: %d, ", info.DataNodeReqOpRate, info.ZoneName, info.Opcode)
			}
			if info.DataNodeReqVolOpRate >= 0 {
				msg += fmt.Sprintf("dataNodeReqZoneVolOpRate: %d, zone: %s, vol:%s, opcode: %d, ", info.DataNodeReqVolOpRate, info.ZoneName, info.Volume, info.Opcode)
			}
			if info.DataNodeReqVolPartRate >= 0 {
				msg += fmt.Sprintf("dataNodeReqVolPartRate: %d, volume: %s, ", info.DataNodeReqVolPartRate, info.Volume)
			}
			if info.DataNodeReqVolOpPartRate >= 0 {
				msg += fmt.Sprintf("dataNodeReqVolOpPartRate: %d, volume: %s, opcode: %d, ", info.DataNodeReqVolOpPartRate, info.Volume, info.Opcode)
			}
			if info.FlashNodeRate >= 0 {
				msg += fmt.Sprintf("flashNodeRate: %d, zone:%s, ", info.FlashNodeRate, info.ZoneName)
			}
			if info.RateLimit >= 0 {
				msg += fmt.Sprintf("module:%s, zone:%s, volume:%s, rateLimit: %d, rateLimitIndex:%d, ", info.Module, info.ZoneName, info.Volume, info.RateLimit, info.RateLimitIndex)
			}
			if info.FlashNodeVolRate >= 0 {
				msg += fmt.Sprintf("flashNodeVolRate: %d, zone:%s, volume: %s, ", info.FlashNodeVolRate, info.ZoneName, info.Volume)
			}
			if info.DataNodeFlushFDInterval >= 0 {
				msg += fmt.Sprintf("dataNodeFlushFDInterval: %d, ", info.DataNodeFlushFDInterval)
			}
			if info.DataNodeFlushFDParallelismOnDisk > 0 {
				msg += fmt.Sprintf("dataNodeFlushFDParallelismOnDisk: %d, ", info.DataNodeFlushFDParallelismOnDisk)
			}
			if info.DNNormalExtentDeleteExpire > 0 {
				msg += fmt.Sprintf("normalExtentDeleteExpire: %d, ", info.DNNormalExtentDeleteExpire)
			}
			if info.MetaNodeDumpWaterLevel > 0 {
				msg += fmt.Sprintf("dumpWaterLevel    : %d, ", info.MetaNodeDumpWaterLevel)
			}
			if info.MonitorSummarySecond > 0 {
				msg += fmt.Sprintf("monitorSummarySec : %d, ", info.MonitorSummarySecond)
			}
			if info.MonitorReportSecond > 0 {
				msg += fmt.Sprintf("monitorReportSec  : %d, ", info.MonitorReportSecond)
			}
			if info.LogMaxMB > 0 {
				msg += fmt.Sprintf("log max MB        : %d, ", info.LogMaxMB)
			}
			if info.RocksDBDiskReservedSpace > 0 {
				msg += fmt.Sprintf("MN RocksDB Disk Reserved MB  : %d, ", info.RocksDBDiskReservedSpace)
			}
			if info.MetaRockDBWalFileMaxMB > 0 {
				msg += fmt.Sprintf("MN RocksDB Wal File Max MB   : %d, ", info.MetaRockDBWalFileMaxMB)
			}
			if info.MetaRocksWalMemMaxMB > 0 {
				msg += fmt.Sprintf("MN RocksDB Wal Mem Max MB    : %d, ", info.MetaRocksWalMemMaxMB)
			}
			if info.MetaRocksLogMaxMB > 0 {
				msg += fmt.Sprintf("MN RocksDB Log Max MB        : %d, ", info.MetaRocksLogMaxMB)
			}
			if info.MetaRocksLogReservedDay > 0 {
				msg += fmt.Sprintf("MN RocksDB Log Reserved Day  : %d, ", info.MetaRocksLogReservedDay)
			}
			if info.MetaRocksLogReservedCnt > 0 {
				msg += fmt.Sprintf("MN RocksDB Log Reserved Cnt  : %d, ", info.MetaRocksLogReservedCnt)
			}
			if info.MetaRocksFlushWalInterval > 0 {
				msg += fmt.Sprintf("MN RocksDB Flush Wal Interval: %d, ", info.MetaRocksFlushWalInterval)
			}
			if info.MetaRocksDisableFlushFlag == 0 {
				msg += fmt.Sprintf("MN RocksDB Wal Flush         : enable, ")
			} else if info.MetaRocksDisableFlushFlag == 1 {
				msg += fmt.Sprintf("MN RocksDB Wal Flush         : disable, ")
			}
			if info.MetaRocksWalTTL > 0 {
				msg += fmt.Sprintf("MN RocksDB Wal Log TTL       : %d, ", info.MetaRocksWalTTL)
			}

			if info.MetaDelEKRecordFileMaxMB > 0 {
				msg += fmt.Sprintf("MN DelEK Record File Max MB  : %d, ", info.MetaDelEKRecordFileMaxMB)
			}
			if info.MetaTrashCleanInterval > 0 {
				msg += fmt.Sprintf("MN trash clean interval : %d Min, ", info.MetaTrashCleanInterval)
			}
			if info.MetaRaftLogSize >= 0 {
				msg += fmt.Sprintf("MN Raft log size MB  : %d, ", info.MetaRaftLogSize)
			}
			if info.MetaRaftLogCap >= 0 {
				msg += fmt.Sprintf("MN Raft log cap  : %d, ", info.MetaRaftLogCap)
			}
			if info.MetaSyncWALEnableState == 0 {
				msg += fmt.Sprintf("MN WAL Sync On Unstable      : disable, ")
			} else if info.MetaSyncWALEnableState == 1 {
				msg += fmt.Sprintf("MN WAL Sync On Unstable      : enable, ")
			}

			if info.DataSyncWALEnableState == 0 {
				msg += fmt.Sprintf("DN WAL Sync On Unstable      : disable, ")
			} else if info.DataSyncWALEnableState == 1 {
				msg += fmt.Sprintf("DN WAL Sync On Unstable      : enable, ")
			}
			if info.DisableStrictVolZone == 0 {
				msg += fmt.Sprintf("Strict Vol Zone:enable, ")
			} else if info.DisableStrictVolZone == 1 {
				msg += fmt.Sprintf("Strict Vol Zone:disable, ")
			}
			if info.AutoUpdatePartitionReplicaNum == 0 {
				msg += fmt.Sprintf("Auto Update Partition Replica Num:disable, ")
			} else if info.AutoUpdatePartitionReplicaNum == 1 {
				msg += fmt.Sprintf("Auto Update Partition Replica Num:enable, ")
			}

			if info.AllocatorMaxUsedFactor > 0 && info.AllocatorMaxUsedFactor <= 1 {
				msg += fmt.Sprintf("BitMap Allocator Max Used Factor: %v, ", info.AllocatorMaxUsedFactor)
			}
			if info.AllocatorMinFreeFactor > 0 && info.AllocatorMinFreeFactor <= 1 {
				msg += fmt.Sprintf("BitMap Allocator Min Free Factor: %v, ", info.AllocatorMinFreeFactor)
			}

			if info.TrashCleanDurationEachTime >= 0 {
				msg += fmt.Sprintf("Trash Clean Duration         : %v, ", info.TrashCleanDurationEachTime)
			}
			if info.TrashCleanMaxCountEachTime >= 0 {
				msg += fmt.Sprintf("Trash Clean Max Count        : %v, ", info.TrashCleanMaxCountEachTime)
			}
			if info.DeleteMarkDelVolInterval >= 0 {
				msg += fmt.Sprintf("DeleteMarkDelVolInterval     : %v, ", info.DeleteMarkDelVolInterval)
			}
			if info.RemoteCacheBoostEnableState == 0 {
				msg += fmt.Sprintf("RemoteCacheBoostEnable       : disable, ")
			} else if info.RemoteCacheBoostEnableState == 1 {
				msg += fmt.Sprintf("RemoteCacheBoostEnable       : enable, ")
			}
			if info.RemoteReadConnTimeoutMs >= 0 {
				msg += fmt.Sprintf("RemoteReadConnTimeoutMs      : %v, ", info.RemoteReadConnTimeoutMs)
			}
			if info.ConnTimeoutMs >= 0 {
				msg += fmt.Sprintf("ConnTimeoutMs                : %v, ", info.ConnTimeoutMs)
			}
			if info.ReadConnTimeoutMs >= 0 {
				msg += fmt.Sprintf("ReadConnTimeoutMs            : %v, ", info.ReadConnTimeoutMs)
			}
			if info.WriteConnTimeoutMs >= 0 {
				msg += fmt.Sprintf("WriteConnTimeoutMs           : %v, ", info.WriteConnTimeoutMs)
			}
			if mode := proto.ConsistencyModeFromInt32(info.DataPartitionConsistencyMode); mode.Valid() {
				msg += fmt.Sprintf("DataPartitionConsistencyMode: %v", mode.String())
			}
			if info.DpTimeoutCntThreshold >= 0 {
				msg += fmt.Sprintf("DP Timeout Continuous Count  : %v, ", info.DpTimeoutCntThreshold)
			}
			if info.ClientReqRecordsReservedCount > 0 {
				msg += fmt.Sprintf("Client Req Reserved Count    : %d, ", info.ClientReqRecordsReservedCount)
			}
			if info.ClientReqRecordsReservedMin > 0 {
				msg += fmt.Sprintf("Client Req Reserved Min      : %d, ", info.ClientReqRecordsReservedMin)
			}
			if info.ClientReqRemoveDupFlag == 0 {
				msg += fmt.Sprintf("Client Req Remove Dup        : disable, ")
			} else if info.ClientReqRemoveDupFlag == 1 {
				msg += fmt.Sprintf("Client Req Remove Dup        : enable, ")
			}
			if info.MetaNodeDelEKZoneRate >= 0 {
				msg += fmt.Sprintf("MetaNodeDelEKZoneRate, Zone: %s, Rate: %v, ", info.ZoneName, info.MetaNodeDelEKZoneRate)
			}
			if info.MetaNodeDelEKVolumeRate >= 0 {
				msg += fmt.Sprintf("MetaNodeDelEKVolRate, Vol: %s, Rate: %v, ", info.Volume, info.MetaNodeDelEKVolumeRate)
			}
			if info.MetaNodeDumpSnapCount != -1 {
				msg += fmt.Sprintf("Metanode Dump Snap Count     : %d, ", info.MetaNodeDumpSnapCount)
			}
			if info.TopologyFetchIntervalMin > 0 {
				msg += fmt.Sprintf("Topology Fetch Interval: %v min, ", info.TopologyFetchIntervalMin)
			}
			if info.TopologyForceFetchIntervalSec > 0 {
				msg += fmt.Sprintf("Topology Force Fetch Interval: %v second, ", info.TopologyForceFetchIntervalSec)
			}
			if info.DataNodeDiskReservedRatio >= 0 {
				msg += fmt.Sprintf("Data Node Disk Reserved Ratio: %v, ", info.DataNodeDiskReservedRatio)
			}
			if msg == "" {
				stdout("No valid parameters\n")
				return
			}

			stdout("Set rate limit: %s\n", strings.TrimRight(msg, " ,"))

			if err = client.AdminAPI().SetRateLimit(&info); err != nil {
				errout("Set rate limit fail:\n%v\n", err)
			}
			stdout("Set rate limit success: %s\n", strings.TrimRight(msg, " ,"))
		},
	}
	cmd.Flags().Int64Var(&info.DataNodeRepairTaskCount, "dataNodeRepairTaskHDDCount", -1, "data node repair task count of hdd zones")
	cmd.Flags().Int64Var(&info.DataNodeRepairTaskSSDZone, "dataNodeRepairTaskSSDCount", -1, "data node repair task count of ssd zones")
	cmd.Flags().StringVar(&info.Module, "module", "", "module (master,datanode,metanode,flashnode)")
	cmd.Flags().StringVar(&info.ZoneName, "zone", "", "zone")
	cmd.Flags().StringVar(&info.Volume, "volume", "", "volume")
	cmd.Flags().StringVar(&info.Action, "action", "", "object node action")
	cmd.Flags().StringVar(&opName, "opname", "", "opcode name")
	cmd.Flags().Int64Var(&info.Opcode, "opcode", 0, "opcode (zero opcode acts as default)")
	cmd.Flags().Int64Var(&info.RateLimit, "rateLimit", -1, "rate limit")
	cmd.Flags().StringVar(&rateLimitName, "rateLimitName", "", "rate limit name, timeout(ms),count,inBytes,outBytes,countPerDisk,inBytesPerDisk,outBytesPerDisk,countPerPartition,inBytesPerPartition,outBytesPerPartition,concurrency")
	cmd.Flags().Int64Var(&info.MetaNodeReqRate, "metaNodeReqRate", -1, "meta node request rate limit")
	cmd.Flags().Int64Var(&info.MetaNodeReqOpRate, "metaNodeReqOpRate", 0, "meta node request rate limit for opcode")
	cmd.Flags().Int64Var(&info.DataNodeMarkDeleteRate, proto.DataNodeMarkDeleteRateKey, -1, "data node mark delete request rate limit")
	cmd.Flags().Int64Var(&info.NetworkFlowRatio, "networkFlowRatio", -1, "network flow ratio (percent)")
	cmd.Flags().Int64Var(&info.DataNodeReqRate, "dataNodeReqZoneRate", -1, "data node request rate limit")
	cmd.Flags().Int64Var(&info.DataNodeReqOpRate, "dataNodeReqZoneOpRate", -1, "data node request rate limit for opcode")
	cmd.Flags().Int64Var(&info.DataNodeReqVolOpRate, "dataNodeReqZoneVolOpRate", -1, "data node request rate limit for a given vol & opcode")
	cmd.Flags().Int64Var(&info.DataNodeReqVolPartRate, proto.DataNodeReqVolPartRateKey, -1, "data node per partition request rate limit for a given volume")
	cmd.Flags().Int64Var(&info.DataNodeReqVolOpPartRate, proto.DataNodeReqVolOpPartRateKey, -1, "data node per partition request rate limit for a given volume & opcode")
	cmd.Flags().Int64Var(&info.FlashNodeRate, proto.FlashNodeRateKey, -1, "flash node cache read request rate limit")
	cmd.Flags().Int64Var(&info.FlashNodeVolRate, proto.FlashNodeVolRateKey, -1, "flash node cache read request rate limit for a volume")
	cmd.Flags().Int64Var(&info.ClientReadVolRate, "clientReadVolRate", -1, "client read rate limit for volume")
	cmd.Flags().Int64Var(&info.ClientWriteVolRate, "clientWriteVolRate", -1, "client write limit rate for volume")
	cmd.Flags().Int64Var(&info.ClientVolOpRate, "clientVolOpRate", -2, "client meta op limit rate")
	cmd.Flags().Int64Var(&info.ObjectVolActionRate, "objectVolActionRate", -2, "object node vol action limit rate")
	cmd.Flags().Int64Var(&info.DnFixTinyDeleteRecordLimit, "fixTinyDeleteRecordLimit", -1, "data node fix tiny delete record limit")
	cmd.Flags().Int64Var(&info.DataNodeRepairTaskZoneCount, "dataNodeRepairTaskZoneCount", -1, "data node repair task count of target zone")
	cmd.Flags().Int64Var(&info.MetaNodeDumpWaterLevel, "metaNodeDumpWaterLevel", -1, "meta node dump snap shot water level")
	cmd.Flags().Uint64Var(&info.MonitorSummarySecond, "monitorSummarySecond", 0, "summary seconds for monitor")
	cmd.Flags().Uint64Var(&info.MonitorReportSecond, "monitorReportSecond", 0, "report seconds for monitor")
	cmd.Flags().Uint64Var(&info.RocksDBDiskReservedSpace, "rocksDBDiskReservedSpace", 0, "rocksdb disk reserved space, unit:MB")
	cmd.Flags().Uint64Var(&info.LogMaxMB, "logMaxMB", 0, "log max MB")
	cmd.Flags().Uint64Var(&info.MetaRockDBWalFileMaxMB, "metaRocksWalFileMaxMB", 0, "Meta node RocksDB config:wal_size_limit_mb, unit:MB")
	cmd.Flags().Uint64Var(&info.MetaRocksWalMemMaxMB, "metaRocksWalMemMaxMB", 0, "Meta node RocksDB config:max_total_wal_size, unit:MB")
	cmd.Flags().Uint64Var(&info.MetaRocksLogMaxMB, "metaRocksLogMaxMB", 0, "Meta node RocksDB config:max_log_file_size, unit:MB")
	cmd.Flags().Uint64Var(&info.MetaRocksLogReservedDay, "metaRocksLogReservedDay", 0, "Meta node RocksDB config:log_file_time_to_roll, unit:Day")
	cmd.Flags().Uint64Var(&info.MetaRocksLogReservedCnt, "metaRocksLogReservedCount", 0, "Meta node RocksDB config:keep_log_file_num")
	cmd.Flags().Uint64Var(&info.MetaRocksFlushWalInterval, "metaRocksWalFlushInterval", 0, "Meta node RocksDB config:flush wal interval, unit:min")
	cmd.Flags().Int64Var(&info.MetaRocksDisableFlushFlag, "metaRocksDisableWalFlush", -1, "Meta node RocksDB config:flush wal flag, 0: enable flush wal log, 1:disable flush wal log")
	cmd.Flags().Uint64Var(&info.MetaRocksWalTTL, "metaRocksWalTTL", 0, "Meta node RocksDB config:wal_ttl_seconds")
	cmd.Flags().Uint64Var(&info.MetaDelEKRecordFileMaxMB, "metaDelEKRecordFileMaxMB", 0, "meta node delete ek record file max mb")
	cmd.Flags().Uint64Var(&info.MetaTrashCleanInterval, "metaTrashCleanInterval", 0, "meta node clean del inode interval, unit:min")
	cmd.Flags().Int64Var(&info.MetaRaftLogSize, "metaRaftLogSize", -1, "meta node raft log size")
	cmd.Flags().Int64Var(&info.MetaRaftLogCap, "metaRaftLogCap", -1, "meta node raft log cap")
	cmd.Flags().Int64Var(&info.MetaSyncWALEnableState, "metaSyncWALFlag", -1, "0:disable, 1:enable")
	cmd.Flags().Int64Var(&info.DataSyncWALEnableState, "dataSyncWALFlag", -1, "0:disable, 1:enable")
	cmd.Flags().Int64Var(&info.DisableStrictVolZone, "disableStrictVolZone", -1, "0:false, 1:true")
	cmd.Flags().Int64Var(&info.AutoUpdatePartitionReplicaNum, "autoUpdatePartitionReplicaNum", -1, "0:false, 1:true")
	cmd.Flags().Int64Var(&info.DataNodeFlushFDInterval, "dataNodeFlushFDInterval", -1, "time interval for flushing WAL and open FDs on DataNode, unit is seconds.")
	cmd.Flags().Int64Var(&info.DataNodeFlushFDParallelismOnDisk, "dataNodeFlushFDParallelismOnDisk", 0, "parallelism for flushing WAL and open FDs on DataNode per disk.")
	cmd.Flags().Int64Var(&info.DNNormalExtentDeleteExpire, "dnNormalExtentDeleteExpire", 0, "datanode normal extent delete record expire time(second, >=600)")
	cmd.Flags().Float64Var(&info.AllocatorMaxUsedFactor, "allocatorMaxUsedFactor", 0, "float64, bit map allocator max used factor for available")
	cmd.Flags().Float64Var(&info.AllocatorMinFreeFactor, "allocatorMinFreeFactor", 0, "float64, bit map allocator min free factor for available")
	cmd.Flags().Int32Var(&info.TrashCleanDurationEachTime, "trashCleanMaxDurationEachTime", -1, "trash clean max duration for each time")
	cmd.Flags().Int32Var(&info.TrashCleanMaxCountEachTime, "trashCleanMaxCountEachTime", -1, "trash clean max count for each time")
	cmd.Flags().Int64Var(&info.DeleteMarkDelVolInterval, "deleteMarkDelVolInterval", -1, "delete mark del vol interval, unit is seconds.")
	cmd.Flags().Int64Var(&info.RemoteCacheBoostEnableState, "RemoteCacheBoostEnable", -1, "set cluster RemoteCacheBoostEnable, 0:disable, 1:enable")
	cmd.Flags().Int32Var(&info.DataPartitionConsistencyMode, "dataPartitionConsistencyMode", -1, fmt.Sprintf("cluster consistency mode for data partitions [%v:%v, %v:%v] ",
		proto.StandardMode.Int32(), proto.StandardMode.String(), proto.StrictMode.Int32(), proto.StrictMode.String()))
	cmd.Flags().IntVar(&info.DpTimeoutCntThreshold, "dpTimeoutCntThreshold", -1, "continuous timeout count to exclude dp")
	cmd.Flags().Uint32Var(&info.ClientReqRecordsReservedCount, "clientReqReservedCount", 0, "client req records reserved count")
	cmd.Flags().Uint32Var(&info.ClientReqRecordsReservedMin, "clientReqReservedMin", 0, "client req records reserved min")
	cmd.Flags().Int32Var(&info.ClientReqRemoveDupFlag, "clientReqRemoveDupFlag", -1, "client req remove dup flag")
	cmd.Flags().Int64Var(&info.RemoteReadConnTimeoutMs, "RemoteReadConnTimeoutMs", -1, "set remoteCache client read/write connection timeout, unit: ms")
	cmd.Flags().Int64Var(&info.ConnTimeoutMs, "ConnTimeoutMs", -1, "set zone or cluster(omit zone acts on cluster) connection timeout, unit: ms")
	cmd.Flags().Int64Var(&info.ReadConnTimeoutMs, "ReadConnTimeoutMs", -1, "set zone or cluster(omit zone acts on cluster) read connection timeout, unit: ms")
	cmd.Flags().Int64Var(&info.WriteConnTimeoutMs, "WriteConnTimeoutMs", -1, "set zone or cluster(omit zone acts on cluster) write connection timeout, unit: ms")
	cmd.Flags().Int64Var(&info.MetaNodeDelEKVolumeRate, "metaNodeDelEKVolRate", -1, "del ek rate limit for volume")
	cmd.Flags().Int64Var(&info.MetaNodeDelEKZoneRate, "metaNodeDelEKZoneRate", -1, "del ek rate limit for zone")
	cmd.Flags().Int64Var(&info.MetaNodeDumpSnapCount, proto.MetaNodeDumpSnapCountKey, -1, "set metanode dump snap count")
	cmd.Flags().Int64Var(&info.TopologyFetchIntervalMin, proto.TopologyFetchIntervalMinKey, 0, "topology fetch interval, unit: min")
	cmd.Flags().Int64Var(&info.TopologyForceFetchIntervalSec, proto.TopologyForceFetchIntervalSecKey, 0, "topology force fetch interval, unit: second")
	cmd.Flags().Float64Var(&info.DataNodeDiskReservedRatio, proto.DataNodeDiskReservedRatioKey, -1, "data node disk reserved ratio, greater than or equal 0")
	return cmd
}

func formatRateLimitInfo(info *proto.LimitInfo) string {
	var sb = strings.Builder{}
	sb.WriteString(fmt.Sprintf("  Cluster name                     : %v\n", info.Cluster))
	sb.WriteString(fmt.Sprintf("  DnFixTinyDeleteRecordLimit       : %v\n", info.DataNodeFixTinyDeleteRecordLimitOnDisk))
	sb.WriteString(fmt.Sprintf("  NetworkFlowRatio                 : %v\n", info.NetworkFlowRatio))
	sb.WriteString(fmt.Sprintf("  RateLimit                        :\n"))
	sb.WriteString("    (Each opcode has three limit groups: total, per disk, per partition. Each group has three limits: count, in bytes, out bytes.)\n")
	sb.WriteString(getRateLimitDesc(info.RateLimit))
	sb.WriteString(fmt.Sprintf("  MetaNodeReqRate                  : %v\n", info.MetaNodeReqRateLimit))
	sb.WriteString(fmt.Sprintf("  MetaNodeReqOpRateMap             : %v\n", info.MetaNodeReqOpRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[opcode]limit)\n"))
	sb.WriteString(fmt.Sprintf("  MetaNodeReqVolOpRateMap          : %v\n", info.MetaNodeReqVolOpRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[string]map[opcode]limit)\n"))
	sb.WriteString("\n")
	if strings.EqualFold(clusterDbBack, info.Cluster) {
		sb.WriteString(fmt.Sprintf("  DataNodeRepairTaskHDDZone        : %v\n", info.DataNodeRepairTaskLimitOnDisk))
	} else {
		sb.WriteString(fmt.Sprintf("  DataNodeRepairTaskHDDZone        : %v\n", info.DataNodeRepairClusterTaskLimitOnDisk))
	}
	sb.WriteString(fmt.Sprintf("  DataNodeRepairTaskSSDZone        : %v\n", info.DataNodeRepairSSDZoneTaskLimitOnDisk))
	sb.WriteString(fmt.Sprintf("  DataNodeReqZoneRateMap           : %v\n", info.DataNodeReqZoneRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[zone]limit)\n"))
	sb.WriteString(fmt.Sprintf("  DataNodeReqZoneOpRateMap         : %v\n", info.DataNodeReqZoneOpRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[zone]map[opcode]limit)\n"))
	sb.WriteString(fmt.Sprintf("  DataNodeReqZoneVolOpRateMap      : %v\n", info.DataNodeReqZoneVolOpRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[zone]map[vol]map[opcode]limit)\n"))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  DataNodeReqVolPartRateMap        : %v\n", info.DataNodeReqVolPartRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[volume]limit - per partition)\n"))
	sb.WriteString(fmt.Sprintf("  DataNodeReqVolOpPartRateMap      : %v\n", info.DataNodeReqVolOpPartRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[volume]map[opcode]limit - per partition)\n"))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  FlashNodeZoneRateMap             : %v\n", info.FlashNodeLimitMap))
	sb.WriteString(fmt.Sprintf("  FlashNodeZoneVolRateMap          : %v\n", info.FlashNodeVolLimitMap))
	sb.WriteString(fmt.Sprintf("  ClientReadVolRateMap             : %v\n", info.ClientReadVolRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[volume]limit of specified volume)\n"))
	sb.WriteString(fmt.Sprintf("  ClientWriteVolRateMap            : %v\n", info.ClientWriteVolRateLimitMap))
	sb.WriteString(fmt.Sprintf("    (map[volume]limit of specified volume)\n"))
	sb.WriteString(fmt.Sprintf("  ClientVolOpRate                  : %v\n", info.ClientVolOpRateLimit))
	sb.WriteString(fmt.Sprintf("    (map[opcode]limit of specified volume)\n"))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  ObjectVolActionRate              : %v\n", info.ObjectNodeActionRateLimit))
	sb.WriteString(fmt.Sprintf("    (map[action]limit of specified volume)\n"))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  DataNodeRepairTaskZoneLimit      : %v\n", info.DataNodeRepairTaskCountZoneLimit))
	sb.WriteString(fmt.Sprintf("    (map[zone]limit)\n"))
	sb.WriteString(fmt.Sprintf("  MetaNodeDumpWaterLevel           : %v\n", info.MetaNodeDumpWaterLevel))
	sb.WriteString(fmt.Sprintf("  MetaTrashCleanInterval      	 : %v\n", info.MetaTrashCleanInterval))
	sb.WriteString(fmt.Sprintf("  MetaRaftLogSize                  : %v\n", info.MetaRaftLogSize))
	sb.WriteString(fmt.Sprintf("  MetaRaftLogCap                   : %v\n", info.MetaRaftCap))
	sb.WriteString(fmt.Sprintf("  DeleteEKRecordFileMaxSize        : %vMB\n", info.DeleteEKRecordFileMaxMB))
	sb.WriteString(fmt.Sprintf("  MetaSyncWalEnableState           : %s\n", formatEnabledDisabled(info.MetaSyncWALOnUnstableEnableState)))
	sb.WriteString(fmt.Sprintf("  DataSyncWalEnableState           : %s\n", formatEnabledDisabled(info.DataSyncWALOnUnstableEnableState)))
	sb.WriteString(fmt.Sprintf("  StrictVolZone                    : %s\n", formatEnabledDisabled(!info.DisableStrictVolZone)))
	sb.WriteString(fmt.Sprintf("  AutoUpdatePartitionReplicaNum    : %s\n", formatEnabledDisabled(info.AutoUpdatePartitionReplicaNum)))
	sb.WriteString(fmt.Sprintf("  MonitorSummarySecond             : %v\n", info.MonitorSummarySec))
	sb.WriteString(fmt.Sprintf("  MonitorReportSecond              : %v\n", info.MonitorReportSec))
	sb.WriteString(fmt.Sprintf("  DataNodeMarkDeleteRate           : %v \n", info.DataNodeDeleteLimitRate))
	sb.WriteString(fmt.Sprintf("  DataNodeFlushFDInterval          : %v s\n", info.DataNodeFlushFDInterval))
	sb.WriteString(fmt.Sprintf("  DataNodeFlushFDParallelismOnDisk : %v \n", info.DataNodeFlushFDParallelismOnDisk))
	sb.WriteString(fmt.Sprintf("  DNNormalExtentDeleteExpire       : %v\n", info.DataNodeNormalExtentDeleteExpire))
	sb.WriteString(fmt.Sprintf("  BitMapAllocatorMaxUsedFactor     : %v\n", info.BitMapAllocatorMaxUsedFactor))
	sb.WriteString(fmt.Sprintf("  BitMapAllocatorMinFreeFactor     : %v\n", info.BitMapAllocatorMinFreeFactor))
	sb.WriteString(fmt.Sprintf("  TrashCleanMaxDurationEachTime    : %v\n", info.TrashCleanDurationEachTime))
	sb.WriteString(fmt.Sprintf("  TrashCleanMaxCountEachTime       : %v\n", info.TrashItemCleanMaxCountEachTime))
	sb.WriteString(fmt.Sprintf("  DeleteMarkDelVolInterval         : %v(%v sec)\n", formatTimeInterval(info.DeleteMarkDelVolInterval), info.DeleteMarkDelVolInterval))
	sb.WriteString(fmt.Sprintf("  RemoteCacheBoostEnable           : %v\n", info.RemoteCacheBoostEnable))
	sb.WriteString(fmt.Sprintf("  DataPartitionConsistencyMode     : %v\n", info.DataPartitionConsistencyMode.String()))
	sb.WriteString(fmt.Sprintf("  DpTimeoutCntThreshold            : %v\n", info.DpTimeoutCntThreshold))
	sb.WriteString(fmt.Sprintf("  RemoveDupReq                     : %v\n", formatEnabledDisabled(info.ClientReqRemoveDupFlag)))
	sb.WriteString(fmt.Sprintf("  ReqRecordsReservedMin            : %v\n", info.ClientReqRecordsReservedMin))
	sb.WriteString(fmt.Sprintf("  ReqRecordsReservedCount          : %v\n", info.ClientReqRecordsReservedCount))
	sb.WriteString(fmt.Sprintf("  RemoteReadConnTimeoutMs          : %v(ms)\n", info.RemoteReadConnTimeout))
	sb.WriteString(fmt.Sprintf("  ZoneNetConnConfig                : %v\n", info.ZoneNetConnConfig))
	sb.WriteString(fmt.Sprintf("  MetaNodeDelEKZoneRateLimit       : %v\n", info.MetaNodeDelEkZoneRateLimitMap))
	sb.WriteString(fmt.Sprintf("   (map[zoneName]limit of delete ek)\n"))
	sb.WriteString(fmt.Sprintf("  MetaNodeDelEKVolRateLimit       : %v\n", info.MetaNodeDelEkVolRateLimitMap))
	sb.WriteString(fmt.Sprintf("   (map[volName]limit of delete ek)\n"))
	sb.WriteString(fmt.Sprintf("  MetaDumpSnapCount                : %v\n", info.MetaNodeDumpSnapCountByZone))
	sb.WriteString(fmt.Sprintf("    (map[zoneName]SnapCount of specified zone)\n"))
	sb.WriteString(fmt.Sprintf("  TopologyFetchInterval            : %v Min\n", info.TopologyFetchIntervalMin))
	sb.WriteString(fmt.Sprintf("  TopologyFroceFetchInterval       : %v Sec\n", info.TopologyForceFetchIntervalSec))
	sb.WriteString(fmt.Sprintf("  DataNodeDiskReservedRatio        : %v\n", info.DataNodeDiskReservedRatio))
	sb.WriteString(fmt.Sprintf("  ClusterCheckDeleteEK             : %v\n", formatEnabledDisabled(!info.DisableClusterCheckDeleteEK)))
	return sb.String()
}

func getRateLimitDesc(rateLimit map[string]map[string]map[int]proto.AllLimitGroup) string {
	var sb strings.Builder
	for module, zoneVolOpMap := range rateLimit {
		sb.WriteString(fmt.Sprintf("    %v  ", module))
		var allZoneVol []string
		for zoneVol := range zoneVolOpMap {
			allZoneVol = append(allZoneVol, zoneVol)
		}
		sort.Slice(allZoneVol, func(i, j int) bool {
			// put vol first
			if allZoneVol[i][0] != allZoneVol[j][0] {
				return allZoneVol[j][0] < allZoneVol[i][0]
			}
			return allZoneVol[i] < allZoneVol[j]
		})
		for _, zoneVol := range allZoneVol {
			opMap := zoneVolOpMap[zoneVol]
			if zoneVol == multirate.ZonePrefix || zoneVol == multirate.VolPrefix {
				zoneVol += "all"
			}
			sb.WriteString(fmt.Sprintf("\n      %v ", zoneVol))
			opIndex := 0
			for op, g := range opMap {
				if opIndex > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(getOpDesc(module, op))
				sb.WriteString(":{")
				sb.WriteString(multirate.GetLimitGroupDesc(g))
				sb.WriteString("}")
				opIndex++
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func getOpDesc(module string, opcode int) string {
	if msg := strings.TrimPrefix(proto.GetOpMsgExtend(opcode), "Op"); msg != "" {
		return msg
	}
	return strconv.Itoa(opcode)
}

func getOpCode(name string) int {
	if name == "" {
		return 0
	}
	m := make(map[string]int)
	for opcode := 0; opcode < math.MaxUint8; opcode++ {
		m[proto.GetOpMsg(uint8(opcode))] = opcode
	}

	if op, ok := m["Op"+name]; ok {
		return op
	}
	if op := proto.GetOpCodeExtend("Op" + name); op > 0 {
		return op
	}
	return -1
}
