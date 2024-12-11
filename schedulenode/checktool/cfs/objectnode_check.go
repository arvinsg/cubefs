package cfs

import (
	"fmt"
	"github.com/cubefs/cubefs/schedulenode/common/jdos"
	"github.com/cubefs/cubefs/util/log"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	ObjectNodeListen     = 1601
	MaxConnectivityRetry = 5
)

var ObjectNodeAppName string

func ResetObjectNodeAppName(name string) {
	ObjectNodeAppName = name
}

func (s *ChubaoFSMonitor) scheduleToCheckObjectNodeAlive() {
	if ObjectNodeAppName == "" {
		log.LogInfo("action[scheduleToCheckMasterLbPodStatus] object node app name is nil")
		return
	}
	ticker := time.NewTicker(time.Duration(s.scheduleInterval) * time.Second)
	defer func() {
		ticker.Stop()
	}()
	s.checkObjectNodeAlive()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkObjectNodeAlive()
		}
	}
}

func (s *ChubaoFSMonitor) checkObjectNodeAlive() {
	var err error
	startTime := time.Now()
	log.LogInfof("checkObjectNodeAlive start")
	jdosAPI := jdos.NewJDOSOpenApi(s.envConfig.Jdos.JdosSysName, ObjectNodeAppName, s.envConfig.Jdos.JdosURL, s.envConfig.Jdos.JdosErp, s.envConfig.Jdos.JdosToken)
	var groups jdos.Groups
	if groups, err = jdosAPI.GetAllGroupsDetails(); err != nil {
		log.LogErrorf("get all ObjectNode groups details failed: %v", err)
		return
	}
	var badPodIpsMap = make(map[*jdos.Group][]string) // group name to bad pod ip addresses
	var totalPods int
	var badPodIpsMapMu sync.Mutex
	var wg = new(sync.WaitGroup)
	for _, group := range groups {
		if group.Environment == GroupEnvironmentPre {
			continue // 跳过预发分组
		}
		wg.Add(1)
		go func(group *jdos.Group) {
			defer wg.Done()
			var badPodIps []string
			var totalPodsInGroup int
			var checkErr error
			if badPodIps, totalPodsInGroup, checkErr = s.checkObjectNodeAliveByGroup(group); checkErr != nil {
				log.LogErrorf("check ObjectNode alive by group [%v %v] failed: %v", group.GroupName, group.Nickname, err)
				return
			}
			if len(badPodIps) == 0 {
				return
			}
			badPodIpsMapMu.Lock()
			badPodIpsMap[group] = badPodIps
			totalPods += totalPodsInGroup
			badPodIpsMapMu.Unlock()
		}(group)
	}
	wg.Wait()
	if badPods := len(badPodIpsMap); badPods > 0 && totalPods > 0 && float64(badPods)/float64(totalPods) >= 0.1 {
		detailBuilder := strings.Builder{}
		for group, badPodIps := range badPodIpsMap {
			if len(badPodIps) > 0 {
				detailBuilder.WriteString(fmt.Sprintf("%v (%v): %v\n", group.Nickname, len(badPodIps), strings.Join(badPodIps, ", ")))
			}
		}
		msg := fmt.Sprintf("Bad ObjectNode pods found！\n%v", detailBuilder.String())
		warnBySpecialUmpKeyWithPrefix(UMPCFSNormalWarnKey, msg)
	}
	log.LogInfof("checkObjectNodeAlive end,totalPods:%v cost [%v]", totalPods, time.Since(startTime))
}

func (s *ChubaoFSMonitor) checkObjectNodeAliveByGroup(group *jdos.Group) (badPodIps []string, totalPods int, err error) {
	api := jdos.NewJDOSOpenApi(s.envConfig.Jdos.JdosSysName, ObjectNodeAppName, s.envConfig.Jdos.JdosURL, s.envConfig.Jdos.JdosErp, s.envConfig.Jdos.JdosToken)
	var pods jdos.Pods
	if pods, err = api.GetGroupAllPods(group.GroupName); err != nil {
		return
	}
	totalPods = len(pods)
	for _, pod := range pods {
		if pod.Status == PodStatusTerminating {
			continue
		}
		if len(pod.PodIP) == 0 {
			continue
		}
		if pod.Status != PodStatusRunning {
			badPodIps = append(badPodIps, pod.PodIP)
			continue
		}
		if checkErr := checkDestinationPortConnectivity(pod.PodIP, ObjectNodeListen, MaxConnectivityRetry); checkErr != nil {
			badPodIps = append(badPodIps, pod.PodIP)
			log.LogWarnf("check objectnode %v failed: %v", pod.PodIP, checkErr)
		}
	}
	return
}

func checkDestinationPortConnectivity(podIp string, port int, maxRetry int) (err error) {
	var addr = fmt.Sprintf("%v:%v", podIp, port)
	var conn net.Conn
	for i := 0; i < maxRetry; i++ {
		conn, err = net.DialTimeout("tcp", addr, time.Second*5)
		if err != nil {
			if i < maxRetry {
				time.Sleep(time.Second * 1)
			}
			continue
		}
		_ = conn.Close()
		return
	}
	return
}
