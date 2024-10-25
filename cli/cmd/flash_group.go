package cmd

import (
	"fmt"
	util_sdk "github.com/cubefs/cubefs/cli/cmd/util/sdk"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/flash"
	"github.com/cubefs/cubefs/sdk/http_client"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/spf13/cobra"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	cmdFlashGroupUse   = "flashGroup [COMMAND]"
	cmdFlashGroupShort = "cluster flashGroup info"
)

func newFlashGroupCommand(client *master.MasterClient) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   cmdFlashGroupUse,
		Short: cmdFlashGroupShort,
	}
	cmd.AddCommand(
		newListFlashGroupsCmd(client),
		//newFlashGroupCreateCmd(client),
		newFlashGroupGetCmd(client),
		//newFlashGroupSetCmd(client),
		//newFlashGroupRemoveCmd(client),
		//newFlashGroupAddFlashNodeCmd(client),
		//newFlashGroupRemoveFlashNodeCmd(client),
		newFlashGroupSearchCmd(client),
	)
	return cmd
}

type slotInfo struct {
	fgID    uint64
	slot    uint32
	percent float64
}

func newListFlashGroupsCmd(client *master.MasterClient) *cobra.Command {
	var (
		isActive      bool
		showSortSlots bool
	)
	var cmd = &cobra.Command{
		Use:   CliOpList + " [IsActive] ",
		Short: "list active(true) or inactive(false) flash groups",
		Args:  cobra.MinimumNArgs(0),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			listAllStatus := true
			if len(args) != 0 {
				listAllStatus = false
				if isActive, err = strconv.ParseBool(args[0]); err != nil {
					return
				}
			}
			fgView, err := client.AdminAPI().ListFlashGroups(isActive, listAllStatus)
			if err != nil {
				return
			}
			var row string
			stdout("[Flash Groups]\n")
			stdout("%v\n", formatFlashGroupViewHeader())
			sort.Slice(fgView.FlashGroups, func(i, j int) bool {
				return fgView.FlashGroups[i].ID < fgView.FlashGroups[j].ID
			})
			sortSlots := make([]*slotInfo, 0)
			for _, group := range fgView.FlashGroups {
				sort.Slice(group.Slots, func(i, j int) bool {
					return group.Slots[i] < group.Slots[j]
				})
				for _, slot := range group.Slots {
					sortSlots = append(sortSlots, &slotInfo{
						fgID: group.ID,
						slot: slot,
					})
				}
				row = fmt.Sprintf(formatFlashGroupViewPattern, group.ID, group.Status, group.FlashNodeCount, len(group.Slots))
				stdout("%v\n", row)
			}

			if showSortSlots != true {
				return
			}

			sort.Slice(sortSlots, func(i, j int) bool {
				return sortSlots[i].slot < sortSlots[j].slot
			})

			stdout("sortSlots:\n")
			for i, info := range sortSlots {
				if i < len(sortSlots)-1 {
					info.percent = float64(sortSlots[i+1].slot-info.slot) * 100 / math.MaxUint32
				} else {
					info.percent = float64(math.MaxUint32-info.slot) * 100 / math.MaxUint32
				}
				stdout("num:%v slot:%v fg:%v percent:%0.5f%% \n", i+1, info.slot, info.fgID, info.percent)
			}
		},
	}
	cmd.Flags().BoolVar(&showSortSlots, "showSortSlots", false, fmt.Sprintf("show all fg sort slots"))
	return cmd
}

func newFlashGroupCreateCmd(client *master.MasterClient) *cobra.Command {
	var (
		optSlots string
	)
	var cmd = &cobra.Command{
		Use:   CliOpCreate,
		Short: "create a new flash group",
		Args:  cobra.MinimumNArgs(0),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			fgView, err := client.AdminAPI().CreateFlashGroup(optSlots)
			if err != nil {
				return
			}
			stdout("[Flash Group info]\n")
			sort.Slice(fgView.Slots, func(i, j int) bool {
				return fgView.Slots[i] < fgView.Slots[j]
			})
			stdout(formatFlashGroupDetail(fgView))
		},
	}
	cmd.Flags().StringVar(&optSlots, CliFlagGroupSlots, "", fmt.Sprintf("set group slots, --slots=slot1,slot2,..."))
	return cmd
}

func newFlashGroupGetCmd(client *master.MasterClient) *cobra.Command {
	var (
		flashGroupID uint64
		detail       bool
	)
	var cmd = &cobra.Command{
		Use:   CliOpInfo + " [FlashGroupID]  [detail ture/false] ",
		Short: "get flash group by id, default don't show hit rate",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()

			flashGroupID, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return
			}
			fgView, err := client.AdminAPI().GetFlashGroup(flashGroupID)
			if err != nil {
				return
			}
			stdout("[Flash Group info]\n")
			sort.Slice(fgView.Slots, func(i, j int) bool {
				return fgView.Slots[i] < fgView.Slots[j]
			})
			stdout(formatFlashGroupDetail(fgView))

			if len(args) > 1 {
				detail, _ = strconv.ParseBool(args[1])
			}

			stdout("[Flash nodes]\n")
			if detail {
				stdout("%v\n", formatFlashNodeViewTableHeader())
			} else {
				stdout("%v\n", formatFlashNodeSimpleViewTableHeader())
			}
			for _, flashNodeViewInfos := range fgView.ZoneFlashNodes {
				for _, fn := range flashNodeViewInfos {
					stdout("%v\n", formatFlashNodeRowInfo(fn, detail))
				}
			}
		},
	}
	return cmd
}

func formatFlashNodeRowInfo(fn *proto.FlashNodeViewInfo, detail bool) string {
	var (
		hitRate     = "N/A"
		evicts      = "N/A"
		version     = "N/A"
		commit      = "N/A"
		enablePing  = "N/A"
		enableStack = "N/A"
		timeoutMs   = "N/A"
	)
	if !detail {
		return fmt.Sprintf(flashNodeViewTableSimpleRowPattern, fn.ZoneName, fn.ID, fn.Addr, formatYesNo(fn.IsActive),
			fn.FlashGroupID, formatTime(fn.ReportTime.Unix()), fn.IsEnable)
	}
	if fn.IsActive {
		stat, err1 := getFlashNodeStat(fn.Addr, client.FlashNodeProfPort)
		if err1 == nil {
			hitRate = fmt.Sprintf("%.2f%%", stat.CacheStatus.HitRate*100)
			evicts = strconv.Itoa(stat.CacheStatus.Evicts)
			enablePing = fmt.Sprintf("%v", stat.EnablePing)
			enableStack = fmt.Sprintf("%v", stat.EnableStack)
			timeoutMs = fmt.Sprintf("%v", stat.CacheReadTimeoutMs)
		}
		versionInfo, e := getFlashNodeVersion(fn.Addr, client.FlashNodeProfPort)
		if e == nil {
			version = versionInfo.Version
			commit = versionInfo.CommitID
		}
	}
	if len(commit) > 7 && "N/A" != commit {
		commit = commit[:7]
	}
	return fmt.Sprintf(flashNodeViewTableRowPattern, fn.ZoneName, fn.ID, fn.Addr, version, commit,
		formatYesNo(fn.IsActive), fn.FlashGroupID, hitRate, evicts, formatTime(fn.ReportTime.Unix()), fn.IsEnable, enablePing, enableStack, timeoutMs)
}

func getFlashNodeStat(host string, port uint16) (*proto.FlashNodeStat, error) {
	fnClient := http_client.NewFlashClient(fmt.Sprintf("%v:%v", strings.Split(host, ":")[0], port), false)
	return fnClient.GetStat()
}

func getKeys(host string, port uint16) ([]interface{}, error) {
	fnClient := http_client.NewFlashClient(fmt.Sprintf("%v:%v", strings.Split(host, ":")[0], port), false)
	return fnClient.GetKeys()
}

func getFlashNodeVersion(host string, port uint16) (*proto.VersionValue, error) {
	fnClient := http_client.NewFlashClient(fmt.Sprintf("%v:%v", strings.Split(host, ":")[0], port), false)
	return fnClient.GetVersion()
}

func setFlashNodePing(host string, port uint16, enable bool) error {
	fnClient := http_client.NewFlashClient(fmt.Sprintf("%v:%v", strings.Split(host, ":")[0], port), false)
	return fnClient.SetFlashNodePing(enable)
}

func setFlashNodeStack(host string, port uint16, enable bool) error {
	fnClient := http_client.NewFlashClient(fmt.Sprintf("%v:%v", strings.Split(host, ":")[0], port), false)
	return fnClient.SetFlashNodeStack(enable)
}

func setFlashNodeReadTimeout(host string, port uint16, ms int) error {
	fnClient := http_client.NewFlashClient(fmt.Sprintf("%v:%v", strings.Split(host, ":")[0], port), false)
	return fnClient.SetFlashNodeReadTimeout(ms)
}

func newFlashGroupSetCmd(client *master.MasterClient) *cobra.Command {
	var (
		flashGroupID uint64
		isActive     bool
	)
	var cmd = &cobra.Command{
		Use:   CliOpSet + " [FlashGroupID] [IsActive] ",
		Short: "set flash group to active or inactive by id",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			flashGroupID, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return
			}
			if isActive, err = strconv.ParseBool(args[1]); err != nil {
				return
			}
			fgView, err := client.AdminAPI().SetFlashGroup(flashGroupID, isActive)
			if err != nil {
				return
			}
			stdout("[Flash Group info]\n")
			sort.Slice(fgView.Slots, func(i, j int) bool {
				return fgView.Slots[i] < fgView.Slots[j]
			})
			stdout(formatFlashGroupDetail(fgView))
		},
	}
	return cmd
}

func newFlashGroupRemoveCmd(client *master.MasterClient) *cobra.Command {
	var (
		flashGroupID uint64
	)
	var cmd = &cobra.Command{
		Use:   CliOpDelete + " [FlashGroupID] ",
		Short: "remove flash group by id",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			flashGroupID, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return
			}
			result, err := client.AdminAPI().RemoveFlashGroup(flashGroupID)
			if err != nil {
				return
			}
			stdout("%v\n", result)
		},
	}
	return cmd
}

func newFlashGroupAddFlashNodeCmd(client *master.MasterClient) *cobra.Command {
	var (
		flashGroupID uint64
		optAddr      string
		optZoneName  string
		optCount     int
	)
	var cmd = &cobra.Command{
		Use:   "addNode [FlashGroupID] ",
		Short: "add flash node to given flash group",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var (
				err    error
				fgView proto.FlashGroupAdminView
			)
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			flashGroupID, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return
			}
			if optAddr != "" {
				fgView, err = client.AdminAPI().FlashGroupAddFlashNode(flashGroupID, 0, "", optAddr)
			} else if optZoneName != "" && optCount > 0 {
				fgView, err = client.AdminAPI().FlashGroupAddFlashNode(flashGroupID, optCount, optZoneName, "")
			} else {
				err = fmt.Errorf("addr or zonename and count should not be empty")
				return
			}
			if err != nil {
				return
			}
			stdout("[Flash Group info]\n")
			sort.Slice(fgView.Slots, func(i, j int) bool {
				return fgView.Slots[i] < fgView.Slots[j]
			})
			stdout(formatFlashGroupDetail(fgView))
		},
	}
	cmd.Flags().StringVar(&optAddr, CliFlagAddress, "", fmt.Sprintf("Add flash node of given addr"))
	cmd.Flags().StringVar(&optZoneName, CliFlagZoneName, "", fmt.Sprintf("Add flash node from given zone"))
	cmd.Flags().IntVar(&optCount, CliFlagCount, 0, fmt.Sprintf("Add given count flash node from zone"))
	return cmd
}

func newFlashGroupRemoveFlashNodeCmd(client *master.MasterClient) *cobra.Command {
	var (
		flashGroupID uint64
		optAddr      string
		optZoneName  string
		optCount     int
	)
	var cmd = &cobra.Command{
		Use:   "deleteNode [FlashGroupID] ",
		Short: "delete flash node to given flash group",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var (
				err    error
				fgView proto.FlashGroupAdminView
			)
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			flashGroupID, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return
			}
			if optAddr != "" {
				fgView, err = client.AdminAPI().FlashGroupRemoveFlashNode(flashGroupID, 0, "", optAddr)
			} else if optZoneName != "" && optCount > 0 {
				fgView, err = client.AdminAPI().FlashGroupRemoveFlashNode(flashGroupID, optCount, optZoneName, "")
			} else {
				err = fmt.Errorf("addr or zonename and count should not be empty")
				return
			}
			if err != nil {
				return
			}
			stdout("[Flash Group info]\n")
			sort.Slice(fgView.Slots, func(i, j int) bool {
				return fgView.Slots[i] < fgView.Slots[j]
			})
			stdout(formatFlashGroupDetail(fgView))
		},
	}
	cmd.Flags().StringVar(&optAddr, CliFlagAddress, "", fmt.Sprintf("remove flash node of given addr"))
	cmd.Flags().StringVar(&optZoneName, CliFlagZoneName, "", fmt.Sprintf("remove flash node from given zone"))
	cmd.Flags().IntVar(&optCount, CliFlagCount, 0, fmt.Sprintf("remove given count flash node from zone"))
	return cmd
}

func newFlashGroupSearchCmd(client *master.MasterClient) *cobra.Command {
	var volume string
	var inode uint64
	var optShowHost bool
	var optStart uint64
	var optEnd uint64
	var fgView proto.FlashGroupsAdminView
	var err error
	var cmd = &cobra.Command{
		Use:   "search [volume] [inode]",
		Short: "search flashGroup by inode offset",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			defer func() {
				if err != nil {
					errout("Error: %v", err)
				}
			}()
			volume = args[0]
			if volume == "" {
				err = fmt.Errorf("volume is empty")
				return
			}
			inode, err = strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				return
			}
			fgView, err = client.AdminAPI().ListFlashGroups(true, true)
			if err != nil {
				return
			}
			slotsMap := make(map[uint32]uint64, 0)
			slots := make([]uint32, 0)
			for _, fg := range fgView.FlashGroups {
				if fg.Status != proto.FlashGroupStatus_Active {
					continue
				}
				for _, slot := range fg.Slots {
					slotsMap[slot] = fg.ID
					slots = append(slots, slot)
				}
			}
			var leader string
			var mpID uint64
			var inodeInfoView *proto.InodeInfoView
			leader, mpID, err = util_sdk.LocateInode(inode, client, volume)
			if err != nil {
				return
			}
			mtClient := meta.NewMetaHttpClient(fmt.Sprintf("%v:%v", strings.Split(leader, ":")[0], client.MetaNodeProfPort), false)
			inodeInfoView, err = mtClient.GetInode(mpID, inode)
			if err != nil {
				return
			}
			if optEnd > inodeInfoView.Size || optEnd == 0 {
				optEnd = inodeInfoView.Size
			}
			sort.Slice(slots, func(i, j int) bool {
				return slots[i] < slots[j]
			})
			fmt.Printf("Volume:        %v\n", volume)
			fmt.Printf("Inode:         %v\n", inode)
			fmt.Printf("FileSize:      %v\n", inodeInfoView.Size)
			fmt.Printf("StartOffset:   %v\n", optStart)
			fmt.Printf("EndOffset:     %v\n", optEnd)
			fmt.Printf("CacheBlocks:\n")
			fmt.Printf("%v\n", formatCacheBlockViewHeader())
			for off := optStart; off < optEnd; {
				fixedOffset := off / proto.CACHE_BLOCK_SIZE * proto.CACHE_BLOCK_SIZE
				size := uint64(proto.CACHE_BLOCK_SIZE)
				if size > optEnd-fixedOffset {
					size = optEnd - fixedOffset
				}
				slot := flash.ComputeCacheBlockSlot(volume, inode, fixedOffset)
				var slotIndex int
				for i, s := range slots {
					if slot > s {
						continue
					}
					slotIndex = i
					break
				}
				var slotAfter uint32
				var fgID uint64
				slotAfter = slots[(slotIndex)%len(slots)]
				fgID = slotsMap[slotAfter]
				fmt.Printf("%v\n", formatCacheBlockRow(fixedOffset, fixedOffset+size, fgID, size))
				off = fixedOffset + size
				if !optShowHost {
					continue
				}
				fg, e := client.AdminAPI().GetFlashGroup(fgID)
				if e != nil {
					fmt.Printf("getFlashGroup:%v err:%v\n", fgID, err)
					continue
				}
				wg := sync.WaitGroup{}
				for _, zfg := range fg.ZoneFlashNodes {
					for _, flashNode := range zfg {
						wg.Add(1)
						go func(fv *proto.FlashNodeViewInfo) {
							defer wg.Done()
							keys, err1 := getKeys(fv.Addr, client.FlashNodeProfPort)
							if err1 != nil {
								fmt.Printf("get addr:%v err:%v\n", fv.Addr, err)
								return
							}
							for _, k := range keys {
								if strings.HasPrefix(k.(string), fmt.Sprintf("%v/%v#%v", volume, inode, fixedOffset)) {
									fmt.Printf("reportTime: %v, blockKey: %v, zone: %v, host: %v\n", fv.ReportTime.Format("2006-01-02 15:04:05"), k, fv.ZoneName, fv.Addr)
								}
							}
						}(flashNode)
					}
				}
				wg.Wait()
			}
		},
	}
	cmd.Flags().Uint64Var(&optStart, "start", 0, fmt.Sprintf("file start offset"))
	cmd.Flags().Uint64Var(&optEnd, "end", 0, fmt.Sprintf("file end offset"))
	cmd.Flags().BoolVar(&optShowHost, "show-host", false, fmt.Sprintf("show host info if cache block exist"))
	return cmd
}
