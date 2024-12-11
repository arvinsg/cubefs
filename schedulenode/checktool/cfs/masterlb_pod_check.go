package cfs

import (
	"fmt"
	"github.com/cubefs/cubefs/schedulenode/common/jdos"
	"github.com/cubefs/cubefs/util/log"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	PodStatusRunning       = "Running"
	PodStatusTerminating   = "Terminating"
	GroupEnvironmentPro    = "pro" // 分组类型,生产分组,对应jdos.Group中Environment字段
	GroupEnvironmentPre    = "pre" // 分组类型,预发分组,对应jdos.Group中Environment字段
	MasterLBAPPNameCFSpark = "overwrite-master-lb"
	MasterLBHostCFSpark    = DomainSpark

	MasterLBAPPNameCFSDbbak = "dbbakmasterlb"
	MasterLBHostCFSDbbak    = DomainDbbak

	MasterLBAPPNameCFSMysql     = "elasticdb-master-lb"
	MasterLBHostCFSMysql        = DomainMysql
	MasterLBPort                = 80
	MaxCheckConnRetryCount      = 3
	MasterLBPortHealthyCheckAPI = "/admin/getIp"
	PodStatusWarningThreshold   = 0.1
	minMasterLBFaultTelCount    = 5
	PodStatusBatchCheckCount    = 10
	minMasterLBWarnCount        = 3
)

func (s *ChubaoFSMonitor) scheduleToCheckMasterLbPodStatus() {
	s.checkMasterLbPodStatus()
	for {
		t := time.NewTimer(time.Duration(s.scheduleInterval) * time.Second)
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.checkMasterLbPodStatus()
		}
	}
}

func (s *ChubaoFSMonitor) checkMasterLbPodStatus() {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("checkMasterLbPodStatus panic:%v", r)
		}
	}()
	// check cfs spark
	s.checkPodsStatusOfAppAndAlarm(MasterLBAPPNameCFSpark, MasterLBHostCFSpark, PodStatusWarningThreshold)
	// check cfs dbback
	s.checkPodsStatusOfAppAndAlarm(MasterLBAPPNameCFSDbbak, MasterLBHostCFSDbbak, PodStatusWarningThreshold)
	// check cfs mysql
	s.checkPodsStatusOfAppAndAlarm(MasterLBAPPNameCFSMysql, MasterLBHostCFSMysql, PodStatusWarningThreshold)
}

func (s *ChubaoFSMonitor) checkPodsStatusOfAppAndAlarm(appName, host string, threshold float32) {
	totalPodsCounts, notRunningPodIps, err := checkPodsStatFromJDOS(s.envConfig.Jdos.JdosSysName, appName, host, s)
	if err != nil {
		log.LogErrorf("action[checkPodsStatusOfAppAndAlarm] err:%v", err)
		return
	}
	key := s.envConfig.Jdos.JdosSysName + appName
	masterLBWarnInfo, ok := s.masterLbLastWarnInfo[key]
	if !ok || masterLBWarnInfo == nil {
		masterLBWarnInfo = &MasterLBWarnInfo{}
		s.masterLbLastWarnInfo[key] = masterLBWarnInfo
	}
	if totalPodsCounts == 0 || len(notRunningPodIps) == 0 {
		masterLBWarnInfo.ContinuedTimes = 0
		log.LogInfof("action[checkPodsStatusOfAppAndAlarm] masterlb check systemName:%v, appName:%v, PodsCounts:%v, notRunningPodIps:%v",
			s.envConfig.Jdos.JdosSysName, appName, totalPodsCounts, notRunningPodIps)
		return
	}
	if float32(len(notRunningPodIps))/float32(totalPodsCounts) > threshold || len(notRunningPodIps) > minMasterLBFaultTelCount {
		msg := fmt.Sprintf("masterlb check systemName:%v, appName:%v, PodsCounts:%v, notRunningPodIps:%v",
			s.envConfig.Jdos.JdosSysName, appName, totalPodsCounts, notRunningPodIps)
		warnBySpecialUmpKeyWithPrefix(UMPKeyMasterLbPodStatus, msg)
	} else {
		log.LogWarnf("masterlb check systemName:%v, appName:%v, PodsCounts:%v, notRunningPodIps:%v",
			s.envConfig.Jdos.JdosSysName, appName, totalPodsCounts, notRunningPodIps)
		// 连续minMasterLBWarnCount次再执行普通告警, 每次告警时间间隔十分钟
		//masterLBWarnInfo.ContinuedTimes++
		//if time.Since(masterLBWarnInfo.LastWarnTime) >= time.Minute*5 && masterLBWarnInfo.ContinuedTimes >= minMasterLBWarnCount {
		//	masterLBWarnInfo.LastWarnTime = time.Now()
		//	masterLBWarnInfo.ContinuedTimes = 0
		//	msg := fmt.Sprintf("masterlb check systemName:%v, appName:%v, PodsCounts:%v, notRunningPodIps:%v",
		//		systemName, appName, totalPodsCounts, notRunningPodIps)
		//	warnBySpecialUmpKeyWithPrefix(UMPCFSNormalWarnKey, msg)
		//} else {
		//	msg := fmt.Sprintf("masterlb check systemName:%v, appName:%v, PodsCounts:%v, notRunningPodIps:%v, masterLBWarnInfo:%v",
		//		systemName, appName, totalPodsCounts, notRunningPodIps, *masterLBWarnInfo)
		//	log.LogInfo(msg)
		//}
	}
}

func checkPodsStatFromJDOS(systemName, appName, host string, s *ChubaoFSMonitor) (totalPodsCount int, notRunningPodIps []string, err error) {
	notRunningPodIps = make([]string, 0)
	jdosOpenApi := jdos.NewJDOSOpenApi(systemName, appName, s.envConfig.Jdos.JdosURL, s.envConfig.Jdos.JdosErp, s.envConfig.Jdos.JdosToken)
	groupsDetails, err := jdosOpenApi.GetAllGroupsDetails()
	if err != nil {
		return
	}
	podIps := make([]string, 0, PodStatusBatchCheckCount)
	for _, group := range groupsDetails {
		groupAllPods, err1 := jdosOpenApi.GetGroupAllPods(group.GroupName)
		if err1 != nil {
			err = fmt.Errorf("action[GetGroupAllPods] from group:%v err:%v", group.GroupName, err1)
			return
		}
		totalPodsCount += len(groupAllPods)
		for _, pod := range groupAllPods {
			if pod.LbStatus != jdos.LbStatusActivce {
				continue
			}
			if pod.Status == PodStatusRunning {
				// pod状态正常 检查nginx实例
				podIps = append(podIps, pod.PodIP)
				if len(podIps) >= PodStatusBatchCheckCount {
					badIps := batchCheckPodHealth(podIps, MasterLBPort)
					notRunningPodIps = append(notRunningPodIps, badIps...)
					podIps = make([]string, 0, PodStatusBatchCheckCount)
				}
			} else {
				notRunningPodIps = append(notRunningPodIps, pod.PodIP)
			}
		}
	}
	return
}

func batchCheckPodHealth(podIps []string, port int) (badIps []string) {
	badIps = make([]string, 0)
	badIpsLock := new(sync.Mutex)
	wg := new(sync.WaitGroup)
	for _, podIp := range podIps {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			if err := checkMasterLbPodAlive(ip, port, MaxCheckConnRetryCount); err != nil {
				log.LogErrorf("action[checkMasterLbPodAlive] ip:%v port:%v err:%v", ip, port, err)
				badIpsLock.Lock()
				badIps = append(badIps, ip)
				badIpsLock.Unlock()
			}
		}(podIp)
	}
	wg.Wait()
	return
}

func doServiceHealthyRequest(podIp string, port string, api string, host string) (data []byte, err error) {
	var resp *http.Response
	url := fmt.Sprintf("http://%v:%v%v", podIp, port, api)
	// 设置3s超时
	client := http.Client{Timeout: time.Duration(3 * time.Second)}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Host = host
	if resp, err = client.Do(req); err != nil {
		return
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if data, err = ioutil.ReadAll(resp.Body); err != nil {
		return
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("action[checkServiceHealthy] podIp[%v] port[%v] api[%v] host[%v],resp.Status[%v],body[%v],err[%v]", podIp, port, api, host, resp.Status, string(data), err)
		err = fmt.Errorf(msg)
		return
	}
	log.LogDebugf("masterlb check:%v,%v\n", url, string(data))
	return
}

func checkMasterLbPodAlive(podIp string, port int, maxRetry int) (err error) {
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
