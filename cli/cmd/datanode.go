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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cubefs/cubefs/cli/cmd/data_check"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/http_client"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/spf13/cobra"
)

const (
	cmdDataNodeShort = "Manage data nodes"
	cmdDataNodeAlias = "dn"
)

func newDataNodeCmd(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     CliResourceDataNode,
		Short:   cmdDataNodeShort,
		Aliases: []string{cmdDataNodeAlias},
	}
	cmd.AddCommand(
		newDataNodeListCmd(client),
		newDataNodeInfoCmd(client),
		//newDataNodeDecommissionCmd(client),
		//newDataNodeDiskDecommissionCmd(client),
		//newResetDataNodeCmd(client),
		//newStopMigratingByDataNode(client),
		newCheckReplicaByDataNodeCmd(client),
		//newResetDataNodeLogLevelCmd(client),
		newDataNodeStartRiskFix(client),
		newDataNodeStopRiskFix(client),
	)
	return cmd
}

const (
	cmdDataNodeListShort                 = "List information of data nodes"
	cmdDataNodeInfoShort                 = "Show information of a data node"
	cmdDataNodeDecommissionInfoShort     = "decommission partitions in a data node to others"
	cmdDataNodeDiskDecommissionInfoShort = "decommission disk of partitions in a data node to others"
	cmdResetDataNodeShort                = "Reset corrupt data partitions related to this node"
	cmdStopMigratingEcByDataNode         = "stop migrating task by data node"
	cmdCheckReplicaByDataNodeShort       = "Check all normal extents which in this data node"
	cmdResetLogLevelShort                = "reset loglevel to error on all datanode"
)

func newDataNodeListCmd(client *master.MasterClient) *cobra.Command {
	var optFilterStatus string
	var optFilterWritable string
	var optShowDp bool
	var cmd = &cobra.Command{
		Use:     CliOpList,
		Short:   cmdDataNodeListShort,
		Aliases: []string{"ls"},
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("List cluster data nodes failed: %v\n", err)
				}
			}()
			var view *proto.ClusterView
			if view, err = client.AdminAPI().GetCluster(); err != nil {
				return
			}
			sort.SliceStable(view.DataNodes, func(i, j int) bool {
				return view.DataNodes[i].ID < view.DataNodes[j].ID
			})
			var info *proto.DataNodeInfo
			var nodeInfoSlice []*proto.DataNodeInfo
			if optShowDp {
				nodeInfoSlice = make([]*proto.DataNodeInfo, len(view.DataNodes), len(view.DataNodes))
				for index, node := range view.DataNodes {
					if info, err = client.NodeAPI().GetDataNode(node.Addr); err != nil {
						return
					}
					nodeInfoSlice[index] = info
				}
			}
			stdout("[Data nodes]\n")
			var header, row string
			if optShowDp {
				header = formatDataNodeViewTableHeader()
			} else {
				header = formatNodeViewTableHeader()
			}
			stdout("%v\n", header)
			for index, node := range view.DataNodes {
				if optFilterStatus != "" &&
					!strings.Contains(formatNodeStatus(node.Status), optFilterStatus) {
					continue
				}
				if optFilterWritable != "" &&
					!strings.Contains(formatYesNo(node.IsWritable), optFilterWritable) {
					continue
				}
				if optShowDp {
					info = nodeInfoSlice[index]
					row = fmt.Sprintf(dataNodeDetailViewTableRowPattern, node.ID, node.Addr, node.Version,
						formatYesNo(node.IsWritable), formatNodeStatus(node.Status), formatSize(info.Used), formatFloat(info.UsageRatio), info.ZoneName, info.DataPartitionCount)
				} else {
					row = formatNodeView(&node, true)
				}
				stdout("%v\n", row)
			}
		},
	}
	cmd.Flags().StringVar(&optFilterWritable, "filter-writable", "", "Filter node writable status")
	cmd.Flags().StringVar(&optFilterStatus, "filter-status", "", "Filter node status [Active, Inactive]")
	cmd.Flags().BoolVarP(&optShowDp, "detail", "d", false, "Show detail information")
	return cmd
}

func newDataNodeInfoCmd(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   CliOpInfo + " [NODE ADDRESS]",
		Short: cmdDataNodeInfoShort,
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			var nodeAddr string
			var datanodeInfo *proto.DataNodeInfo
			defer func() {
				if err != nil {
					errout("Show data node info failed: %v\n", err)
				}
			}()
			nodeAddr = args[0]
			if datanodeInfo, err = client.NodeAPI().GetDataNode(nodeAddr); err != nil {
				return
			}
			dataClient := http_client.NewDataClient(fmt.Sprintf("%s:%d", strings.Split(nodeAddr, ":")[0], client.DataNodeProfPort), false)
			//check dataPartition by dataNode api
			var dnPartitions, _ = dataClient.GetPartitionsFromNode()
			stdout("[Data node info]\n")
			stdout(formatDataNodeDetail(datanodeInfo, dnPartitions, false))

		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}
func newDataNodeDecommissionCmd(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   CliOpDecommission + " [NODE ADDRESS]",
		Short: cmdDataNodeDecommissionInfoShort,
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if client.IsOnline() {
				errout("%v for %v is not permited\n", CliOpDecommission, client.Nodes())
			}

			var err error
			var nodeAddr string
			defer func() {
				if err != nil {
					errout("decommission data node failed, err[%v]\n", err)
				}
			}()
			nodeAddr = args[0]
			if err = client.NodeAPI().DataNodeDecommission(nodeAddr); err != nil {
				return
			}
			stdout("Decommission data node successfully\n")
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func newDataNodeDiskDecommissionCmd(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   CliOpDecommissionDisk + " [NODE ADDRESS]" + "[DISK PATH]",
		Short: cmdDataNodeDiskDecommissionInfoShort,
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if client.IsOnline() {
				errout("%v for %v is not permited\n", CliOpDecommissionDisk, client.Nodes())
			}

			var err error
			var nodeAddr string
			var diskAddr string
			defer func() {
				if err != nil {
					errout("decommission disk failed, err[%v]\n", err)
				}
			}()
			nodeAddr = args[0]
			diskAddr = args[1]
			if err = client.NodeAPI().DataNodeDiskDecommission(nodeAddr, diskAddr, false); err != nil {
				return
			}
			stdout("Decommission disk successfully\n")
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func newResetDataNodeCmd(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   CliOpReset + " [ADDRESS]",
		Short: cmdResetDataNodeShort,
		Long: `If more than half replicas of a partition are on the corrupt nodes, the few remaining replicas can 
not reach an agreement with one leader. In this case, you can use the "reset" command to fix the problem. This command
is used to reset all the corrupt partitions related to a chosen corrupt node. However this action may lead to data 
loss, be careful to do this.`,
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var (
				address string
				confirm string
				err     error
			)
			defer func() {
				if err != nil {
					errout("Error:%v", err)
					OsExitWithLogFlush()
				}
			}()
			address = args[0]
			stdout(fmt.Sprintf("The action may risk the danger of losing data, please confirm(y/n):"))
			_, _ = fmt.Scanln(&confirm)
			if "y" != confirm && "yes" != confirm {
				return
			}
			if err = client.AdminAPI().ResetCorruptDataNode(address); err != nil {
				return
			}
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func newStopMigratingByDataNode(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   CliOpStopMigratingEc + " [NODE ADDRESS]",
		Short: cmdStopMigratingEcByDataNode,
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var (
				nodeAddr string
			)
			nodeAddr = args[0]
			stdout("%v\n", client.NodeAPI().StopMigratingByDataNode(nodeAddr))
		},
	}
	return cmd
}

func newCheckReplicaByDataNodeCmd(client *master.MasterClient) *cobra.Command {
	var limitRate int
	var optCheckType int
	var extentModifyMinTime string
	var checkTiny bool
	var quickCheck bool
	var cmd = &cobra.Command{
		Use:   CliOpCheckReplica + " [ADDRESS]",
		Short: cmdCheckReplicaByDataNodeShort,
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			var nodeAddr string
			defer func() {
				if err != nil {
					errout("Show data node info failed: %v\n", err)
				}
			}()
			if limitRate < 1 {
				limitRate = 1
			} else if limitRate > 200 {
				limitRate = 200
			}
			nodeAddr = args[0]
			var checkEngine *data_check.CheckEngine
			outputDir, _ := os.Getwd()
			config := proto.CheckTaskInfo{
				CheckMod: proto.NodeExtent,
				Filter: proto.Filter{
					NodeFilter: []string{
						nodeAddr,
					},
					InodeFilter: make([]uint64, 0),
					DpFilter:    make([]uint64, 0),
				},
				Concurrency:         uint32(limitRate),
				ExtentModifyTimeMin: extentModifyMinTime,
				QuickCheck:          quickCheck,
				CheckTiny:           checkTiny,
			}
			checkEngine, err = data_check.NewCheckEngine(config, outputDir, client, optCheckType, "", false)
			if err != nil {
				return
			}
			defer checkEngine.Close()
			err = checkEngine.Start()
			if err != nil {
				stdout(err.Error())
				return
			}
			stdout("finish datanode replica crc check")
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().IntVar(&limitRate, "limit-rate", 10, "specify dp check limit rate, default:10, max:200")
	cmd.Flags().IntVar(&optCheckType, "check-type", 0, "specify check type : 0 crc, 1 inode ek num, 2 nlink")
	cmd.Flags().StringVar(&extentModifyMinTime, "from-time", "1970-01-01 00:00:00", "specify extent modify from time to check, format:yyyy-mm-dd hh:mm:ss")
	cmd.Flags().BoolVar(&checkTiny, "tiny-only", false, "check tiny extent only")
	cmd.Flags().BoolVar(&quickCheck, "quick-check", false, "quick check: check crc from meta data first, if not the same, then check md5")
	return cmd
}

func newResetDataNodeLogLevelCmd(client *master.MasterClient) *cobra.Command {
	var resetNum uint64
	var resetInterval int
	var cmd = &cobra.Command{
		Use:   CliOpResetLogLevel,
		Short: cmdResetLogLevelShort,
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("reset loglevel failed: %v\n", err)
				}
			}()
			if resetNum < 1 {
				resetNum = 1
			}
			if resetInterval < 1 {
				resetInterval = 1
			}

			tick := time.NewTicker(time.Second)
			for {
				select {
				case <-tick.C:
					resetDataNodeLogLevel(client)
					resetNum--
					if resetNum == 0 {
						return
					}
					tick.Reset(time.Hour * time.Duration(resetInterval))
				}
			}
		},
	}
	cmd.Flags().Uint64Var(&resetNum, "num", 1, "specify execute count of reset, max:unlimited")
	cmd.Flags().IntVar(&resetInterval, "interval", 6, "specify interval between reset, max:48")
	return cmd
}

func newDataNodeStartRiskFix(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "start-risk-fix" + " [NODE ADDRESS]",
		Short: cmdDataNodeInfoShort,
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			var nodeAddr string
			nodeAddr = args[0]
			dataClient := http_client.NewDataClient(fmt.Sprintf("%s:%d", strings.Split(nodeAddr, ":")[0], client.DataNodeProfPort), false)
			//check dataPartition by dataNode api
			if err = dataClient.StartRiskFix(); err != nil {
				stdout("Start risk fix failed: %v\n", err)
				return
			}
			stdout("Start risk fix success\n")
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func newDataNodeStopRiskFix(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "stop-risk-fix" + " [NODE ADDRESS]",
		Short: cmdDataNodeInfoShort,
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			var nodeAddr string
			nodeAddr = args[0]
			dataClient := http_client.NewDataClient(fmt.Sprintf("%s:%d", strings.Split(nodeAddr, ":")[0], client.DataNodeProfPort), false)
			//check dataPartition by dataNode api
			if err = dataClient.StopRiskFix(); err != nil {
				stdout("Stop risk fix failed: %v\n", err)
				return
			}
			stdout("Stop risk fix success\n")
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return validDataNodes(client, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	return cmd
}

func parseTime(timeStr string) (t time.Time, err error) {
	if timeStr != "" {
		t, err = time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.Now().Location())
		if err != nil {
			return
		}
	} else {
		t = time.Unix(0, 0)
	}
	return
}
