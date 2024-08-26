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

package repl

import (
	"container/list"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/util/exporter"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/connpool"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
)

var (
	gConnPool       *connpool.ConnectPool
	ReplProtocalMap sync.Map
)

// ReplProtocol defines the struct of the replication protocol.
// 1. ServerConn reads a packet from the client socket, and analyzes the addresses of the followers.
// 2. After the preparation, the packet is send to toBeProcessedCh. If failure happens, send it to the response channel.
// 3. OperatorAndForwardPktGoRoutine fetches a packet from toBeProcessedCh, and determine if it needs to be forwarded to the followers.
// 4. receiveResponse fetches a reply from responseCh, executes postFunc, and writes a response to the client if necessary.
type ReplProtocol struct {
	packetListLock sync.RWMutex

	packetList *list.List    // stores all the received packets from the client
	ackCh      chan struct{} // if sending to all the replicas succeeds, then a signal to this channel

	toBeProcessedCh chan *Packet // the goroutine receives an available packet and then sends it to this channel
	responseCh      chan *Packet // this chan is used to write response to the client

	sourceConn *net.TCPConn
	exitC      chan bool
	exited     int32
	exitedMu   sync.RWMutex

	followerConnects map[string]*FollowerTransport
	lock             sync.RWMutex

	prepareFunc  func(p *Packet, remote string) error  // prepare packet
	operatorFunc func(p *Packet, c *net.TCPConn) error // operator
	postFunc     func(p *Packet) error                 // post-processing packet

	replId               int64
	startTime            int64
	allThreadStats       []int
	allThreadStatsLock   sync.Mutex
	getNumFromDataPool   int64
	putNumToDataPool     int64
	getPacketFromPoolCnt int64
	putPacketToPoolCnt   int64
	isError              int32
	remote               string
	stopError            string

	forwardPacketCheckList     *list.List
	forwardPacketCheckListLock sync.RWMutex
	forwardPacketCheckCnt      uint64
	globalErr                  error
	firstErrPkg                *Packet
}

func NewReplProtocol(inConn *net.TCPConn, prepareFunc func(p *Packet, remote string) error,
	operatorFunc func(p *Packet, c *net.TCPConn) error, postFunc func(p *Packet) error) *ReplProtocol {
	rp := new(ReplProtocol)
	rp.packetList = list.New()
	rp.ackCh = make(chan struct{}, RequestChanSize)
	rp.toBeProcessedCh = make(chan *Packet, RequestChanSize)
	rp.responseCh = make(chan *Packet, RequestChanSize)
	rp.exitC = make(chan bool, 1)
	rp.sourceConn = inConn
	rp.followerConnects = make(map[string]*FollowerTransport)
	rp.prepareFunc = prepareFunc
	rp.operatorFunc = operatorFunc
	rp.startTime = time.Now().Unix()
	rp.getPacketFromPoolCnt = 0
	rp.putPacketToPoolCnt = 0
	rp.getNumFromDataPool = 0
	rp.putNumToDataPool = 0
	rp.postFunc = postFunc
	rp.allThreadStats = make([]int, 3)
	rp.exited = ReplRuning
	rp.forwardPacketCheckList = list.New()
	rp.replId = proto.GenerateRequestID()
	ReplProtocalMap.Store(rp.replId, rp)
	rp.remote = rp.sourceConn.RemoteAddr().String()
	go rp.OperatorAndForwardPktGoRoutine()
	go rp.writeResponseToClientGoroutine()

	return rp
}

const (
	ReplProtocalThreadRuning = 1
	ReplProtocalThreadExit   = -1
)

func (rp *ReplProtocol) GetID() int64 {
	return rp.replId
}

// ServerConn keeps reading data from the socket to analyze the follower address, execute the prepare function,
// and throw the packets to the to-be-processed channel.
func (rp *ReplProtocol) ServerConn() {
	var (
		err error
	)
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("ReplProtocol(%v): ServerConn: occurred panic. \n"+
				"message: %v\n"+
				"stack:\n%v",
				rp.remote, r, string(debug.Stack()))
			log.LogCritical(msg)
			exporter.WarningPanic(msg)
		}
	}()
	defer func() {
		rp.Stop(err)
		rp.exitGoRoutine(0)

	}()
	rp.allThreadStatsLock.Lock()
	rp.allThreadStats[0] = ReplProtocalThreadRuning
	rp.allThreadStatsLock.Unlock()
	for {
		select {
		case <-rp.exitC:
			return
		default:
			if err = rp.readPkgAndPrepare(); err != nil {
				return
			}
		}
	}

}

// Receive response from all followers.
//func (rp *ReplProtocol) ReceiveResponseFromFollowersGoRoutine() {
//	for {
//		select {
//		case <-rp.ackCh:
//			rp.checkLocalResultAndReciveAllFollowerResponse()
//		case <-rp.exitC:
//			rp.exitedMu.Lock()
//			if atomic.AddInt32(&rp.exited, -1) == ReplHasExited {
//				rp.sourceConn.Close()
//				rp.cleanResource()
//			}
//			rp.exitedMu.Unlock()
//			return
//		}
//	}
//}

func (rp *ReplProtocol) readPkgAndPrepare() (err error) {
	request := GetPacketFromPool()
	var isUsedBufferPool bool
	isUsedBufferPool, err = request.ReadFromConnFromCli(rp.sourceConn, ReplProtocalServerTimeOut)
	if isUsedBufferPool {
		rp.addGetNumFromDataPoolCnt()
	}
	request.replSource = rp.remote
	rp.addGetNumFromPacketPoolCnt()
	if err != nil {
		rp.forceCleanDataPoolFlag(request, "readPkgAndPrepare")
		rp.forceCleanPacketPoolFlag(request, "readPkgAndPrepare")
		err = fmt.Errorf("%v local(%v)->remote(%v) recive error(%v)", ActionreadPkgAndPrepare, rp.sourceConn.LocalAddr().String(),
			rp.sourceConn.RemoteAddr().String(), err)
		return
	}
	request.OrgBuffer = request.Data
	if log.IsDebugEnabled() {
		log.LogDebugf("action[readPkgAndPrepare] packet(%v) from remote(%v) ",
			request.GetUniqueLogId(), rp.remote)
	}

	request.ResetElapse()
	if err = request.resolveFollowersAddr(rp.remote); err != nil {
		err = rp.putResponse(request)
		return
	}
	if err = rp.prepareFunc(request, rp.sourceConn.RemoteAddr().String()); err != nil {
		err = fmt.Errorf("%v  packet(%v) from remote(%v) error(%v)",
			ActionPreparePkt, request.GetUniqueLogId(), rp.remote, err.Error())
		log.LogErrorf(err.Error())
		err = rp.putResponse(request)
		return
	}
	err = rp.putToBeProcess(request)
	if err == nil {
		request.addPacketPoolRefCnt()
		request.addDataPoolRefCnt()
	}

	return
}

func (rp *ReplProtocol) sendRequestToAllFollowers(request *Packet) (err error) {
	var failure = 0
	var maxFailure int
	if request.quorum > 0 && len(request.followersAddrs)+1 >= request.quorum {
		maxFailure = len(request.followersAddrs) - (request.quorum - 1)
	} else {
		maxFailure = 0
	}
	request.errorCh = make(chan error, len(request.followersAddrs))
	var forwardErr error
	var multiErr error
	var incFailure = func(err error) {
		failure += 1
		request.errorCh <- err
		if multiErr == nil {
			multiErr = err
		} else {
			multiErr = fmt.Errorf("%v: %v", err, multiErr)
		}
	}
	for index := 0; index < len(request.followersAddrs); index++ {
		var transport *FollowerTransport
		if transport, forwardErr = rp.allocateFollowersConns(request, index); forwardErr != nil {
			rp.setGlobalErrAndFirstPkg(request, forwardErr)
			log.LogErrorf("replID(%v) firstErrAndPkg(%v,%v),reqID(%v) Op(%v) forwardErr(%v)",
				rp.replId, rp.globalErr, rp.firstErrPkg, request.ReqID, request.GetOpMsg(), forwardErr)
			incFailure(forwardErr)
			if failure > maxFailure {
				err = forwardErr
				request.PackErrorBody(ActionSendToFollowers, fmt.Sprintf("send to followers meet max failure: %v", multiErr))
				log.LogErrorf("packet[id: %v, op: %v, followers: %v, quorum: %v] send to followers meet max failure: %v",
					request.ReqID, request.GetOpMsg(), len(request.followersAddrs), request.quorum, multiErr)
				return
			}
			continue
		}
		followerRequest := NewFollowerPacket(request.Ctx(), request)
		copyPacket(request, followerRequest)
		followerRequest.RemainingFollowers = 0
		request.followerPackets[index] = followerRequest
		if forwardErr = transport.Write(followerRequest); forwardErr != nil {
			rp.setGlobalErrAndFirstPkg(request, forwardErr)
			log.LogErrorf("replID(%v) firstErrAndPkg(%v,%v),reqID(%v) Op(%v) forwardErr(%v)",
				rp.replId, rp.globalErr, rp.firstErrPkg, request.ReqID, request.GetOpMsg(), forwardErr)
			incFailure(err)
			if failure > maxFailure {
				err = forwardErr
				request.PackErrorBody(ActionSendToFollowers, fmt.Sprintf("send to followers meet max failure: %v", multiErr))
				log.LogErrorf("packet[id: %v, op: %v, followers: %v, quorum: %v] send to followers meet max failure: %v",
					request.ReqID, request.GetOpMsg(), len(request.followersAddrs), request.quorum, multiErr)
				return
			}
			err = nil
		}
		request.addDataPoolRefCnt()
		request.addPacketPoolRefCnt()
	}

	return
}

func (rp *ReplProtocol) setGlobalErrAndFirstPkg(request *Packet, err error) {
	if rp.globalErr == nil {
		rp.globalErr = err
		rp.firstErrPkg = new(Packet)
		copyReplPacket(request, rp.firstErrPkg)
	}
}

// OperatorAndForwardPktGoRoutine reads packets from the to-be-processed channel and writes responses to the client.
//  1. Read a packet from toBeProcessCh, and determine if it needs to be forwarded or not. If the answer is no, then
//     process the packet locally and put it into responseCh.
//  2. If the packet needs to be forwarded, the first send it to the followers, and execute the operator function.
//     Then notify receiveResponse to read the followers' responses.
//  3. Read a reply from responseCh, and write to the client.
func (rp *ReplProtocol) OperatorAndForwardPktGoRoutine() {
	var currRequest *Packet
	defer func() {
		if r := recover(); r != nil {
			var reqMsg string
			if currRequest != nil {
				reqMsg = currRequest.GetUniqueLogId()
			} else {
				reqMsg = "nil"
			}
			msg := fmt.Sprintf("ReplProtocol(%v): dealRequest(%v) OperatorAndForwardPktGoRoutine: occurred panic. \n"+
				"message: %v\n"+
				"stack:\n%v",
				rp.remote, reqMsg, r, string(debug.Stack()))
			log.LogCritical(msg)
			exporter.WarningPanic(msg)
			rp.exitGoRoutine(1)
		}
	}()
	ticker := time.NewTicker(time.Minute)
	defer func() {
		ticker.Stop()
	}()
	rp.allThreadStatsLock.Lock()
	rp.allThreadStats[1] = ReplProtocalThreadRuning
	rp.allThreadStatsLock.Unlock()
	for {
		select {
		case request := <-rp.toBeProcessedCh:
			currRequest = request
			rp.processRequest(request)
		case <-ticker.C:
			rp.autoReleaseFollowerTransport()
		case <-rp.exitC:
			rp.exitGoRoutine(1)
			return
		}
	}

}

func (rp *ReplProtocol) processRequest(request *Packet) {
	if !request.IsForwardPacket() {
		_ = rp.operatorFunc(request, rp.sourceConn)
		request.DecDataPoolRefCnt()
		request.DecPacketPoolRefCnt()
		_ = rp.putResponse(request)
		return
	}

	if err := rp.sendRequestToAllFollowers(request); err != nil {
		_ = rp.putResponse(request)
		return
	}
	rp.pushPacketToList(request)
	_ = rp.operatorFunc(request, rp.sourceConn)
	request.DecDataPoolRefCnt()
	request.DecPacketPoolRefCnt()
	_ = rp.putAck(request)

}

func (rp *ReplProtocol) autoReleaseFollowerTransport() {
	deleteTransportsKeys := make([]string, 0)
	rp.lock.Lock()
	if len(rp.followerConnects) == 0 {
		rp.lock.Unlock()
		return
	}
	for key, transport := range rp.followerConnects {
		release := transport.needAutoDestory()
		if release {
			deleteTransportsKeys = append(deleteTransportsKeys, key)
		}
	}
	for _, k := range deleteTransportsKeys {
		delete(rp.followerConnects, k)
	}
	rp.lock.Unlock()
}

func (rp *ReplProtocol) putLeaderPacketToCheckList(request *Packet) {
	if request.IsLeaderPacket() {
		rp.packetListLock.Lock()
		atomic.AddUint64(&rp.forwardPacketCheckCnt, 1)
		rp.forwardPacketCheckList.PushBack(request)
		rp.packetListLock.Unlock()
	}
}

const (
	MaxForwardPacketCheckCnt = 1000
)

func (rp *ReplProtocol) checkForwardPacketPost() {
	if atomic.LoadUint64(&rp.forwardPacketCheckCnt)%MaxForwardPacketCheckCnt == 0 {
		return
	}
	maxFreeCnt := 100
	freeCnt := 0
	rp.packetListLock.Lock()
	defer rp.packetListLock.Unlock()
	if rp.forwardPacketCheckList.Len() == 0 {
		return
	}
	for e := rp.forwardPacketCheckList.Front(); e != nil; e = e.Next() {
		p := e.Value.(*Packet)
		if p.canPutToDataPool() {
			rp.cleanDataPoolFlag(p, "checkForwardPacketPost")
			rp.forwardPacketCheckList.Remove(e)
			freeCnt++
			continue
		}
		if freeCnt >= maxFreeCnt {
			break
		}
	}
	if freeCnt > 0 {
		log.LogDebugf(fmt.Sprintf("repl(%v) ReplProtocol(%v) "+
			"getNumFromDataPool(%v) putNumToDataPool(%v)  currentFreeCnt(%v)",
			rp.replId, rp.sourceConn.RemoteAddr().String(), atomic.LoadInt64(&rp.getNumFromDataPool),
			atomic.LoadInt64(&rp.putNumToDataPool), freeCnt))
	}
	atomic.StoreUint64(&rp.forwardPacketCheckCnt, 0)

}

func (rp *ReplProtocol) exitGoRoutine(index int) {
	rp.exitedMu.Lock()
	rp.allThreadStatsLock.Lock()
	rp.allThreadStats[index] = ReplProtocalThreadExit
	rp.allThreadStatsLock.Unlock()
	atomic.AddInt32(&rp.exited, -1)
	if atomic.LoadInt32(&rp.exited) == ReplHasExited {
		_ = rp.sourceConn.Close()
		rp.cleanResource()
	}
	rp.exitedMu.Unlock()
}

func (rp *ReplProtocol) writeResponseToClientGoroutine() {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("ReplProtocol(%v): writeResponseToClientGoroutine: occurred panic. \n"+
				"message: %v\n"+
				"stack:\n%v",
				rp.remote, r, string(debug.Stack()))
			log.LogCritical(msg)
			exporter.WarningPanic(msg)
			rp.exitGoRoutine(2)
		}
	}()
	rp.allThreadStatsLock.Lock()
	rp.allThreadStats[2] = ReplProtocalThreadRuning
	rp.allThreadStatsLock.Unlock()
	var e *list.Element
	for {
		select {
		case <-rp.ackCh:
			if e = rp.getNextPacket(); e == nil {
				continue
			}
			request := e.Value.(*Packet)
			rp.checkLocalResultAndReceiveAllFollowerResponse(request)
			rp.deletePacket(request, e)
		case request := <-rp.responseCh:
			rp.writeResponse(request)
		case <-rp.exitC:
			rp.exitGoRoutine(2)
			return
		}
	}
}

type ReplProtocalBufferDetail struct {
	Addr     string
	Cnt      int64
	UseBytes int64
	ReplID   int64
}

func GetReplProtocolDetail() (allReplDetail []*ReplProtocalBufferDetail) {
	allReplDetail = make([]*ReplProtocalBufferDetail, 0)
	ReplProtocalMap.Range(func(key, value interface{}) bool {
		rp := value.(*ReplProtocol)
		if atomic.LoadInt64(&rp.getNumFromDataPool) <= 0 {
			return true
		}
		rd := new(ReplProtocalBufferDetail)
		rd.Addr = rp.sourceConn.RemoteAddr().String()
		rd.Cnt = atomic.LoadInt64(&rp.getNumFromDataPool) - atomic.LoadInt64(&rp.putNumToDataPool)
		rd.ReplID = rp.replId
		rd.UseBytes = rd.Cnt * unit.BlockSize
		allReplDetail = append(allReplDetail, rd)
		return true
	})
	return
}

// Read a packet from the list, scan all the connections of the followers of this packet and read the responses.
// If failed to read the response, then mark the packet as failure, and delete it from the list.
// If all the reads succeed, then mark the packet as success.
func (rp *ReplProtocol) checkLocalResultAndReceiveAllFollowerResponse(request *Packet) {
	if request.IsErrPacket() {
		return
	}
	var (
		// 向Follower转发复制链路的成功与失败计数器
		forwardSuccess = 0
		forwardFailure = 0

		// 最小成功数量，判定成功的边界数值，既当成功的数量满足该数值(大于等于)，则可判定为成功
		// 数值计算原则:
		// 若启用了quorum且quorum有效时，该数值等于quorum-1 (quorum意为本地执行及Follower响应成功的最小阈值, 以下逻辑仅检查Follower响应，所以减去1)；
		// 否则该值为Follower数量
		minForwardSuccess int

		// 最大失败数量，判定失败的边界数值，既当失败的数量超过该数值(大于)，则可判定为失败。
		// 数值计算原则:
		// 若启用了quorum且quorum有效时，该数值等于Follower数量+1-quorum；否则改制为0，即任何失败均判定该消息主备复制失败。
		maxForwardFailure int

		multiError error
	)
	if request.quorum > 0 && len(request.followersAddrs)+1 >= request.quorum {
		// Quorum有效
		minForwardSuccess = request.quorum - 1
		maxForwardFailure = len(request.followersAddrs) + 1 - request.quorum
	} else {
		// Quorum未设置或无效
		minForwardSuccess = len(request.followersAddrs)
		maxForwardFailure = 0
	}

	for index := 0; index < len(request.followersAddrs); index++ {
		if forwardErr := <-request.errorCh; forwardErr != nil {
			// 来自某Follower的失败响应
			request.DecPacketPoolRefCnt()
			// 组合所有错误
			if multiError == nil {
				multiError = forwardErr
			} else {
				multiError = fmt.Errorf("%v: %v", forwardErr, multiError)
			}
			if forwardFailure += 1; forwardFailure > maxForwardFailure {
				// 已失败数量超过了允许范围内的最大失败数量，判定为失败
				request.PackErrorBody(ActionReceiveFromFollower, fmt.Sprintf("follower response meet max failure: %v", multiError))
				log.LogErrorf("packet[id: %v, op: %v, followers: %v, quorum: %v] follower response meet max failure: %v",
					request.ReqID, request.GetOpMsg(), len(request.followersAddrs), request.quorum, multiError)
				return
			}

		} else {
			// 来自某Follower的成功响应
			request.DecPacketPoolRefCnt()
			if forwardSuccess += 1; forwardSuccess >= minForwardSuccess {
				// 已成功数量满足了最小成功数量要求，判定为成功
				return
			}
		}
	}
	return
}

// Write a reply to the client.
func (rp *ReplProtocol) writeResponse(reply *Packet) {
	var err error
	defer func() {
		rp.cleanDataPoolFlag(reply, "writeResponse")
		rp.cleanPacketPoolFlag(reply, "writeResponse")
	}()
	_ = rp.postFunc(reply)
	if !reply.NeedReply {
		return
	}
	if reply.IsErrPacket() {
		err = fmt.Errorf(reply.LogMessage(ActionWriteToClient, rp.sourceConn.RemoteAddr().String(),
			reply.StartT, fmt.Errorf(string(reply.Data[:reply.Size]))))
		log.LogErrorf(err.Error())
	}

	if err = reply.WriteToConn(rp.sourceConn, proto.WriteDeadlineTime); err != nil {
		err = fmt.Errorf(reply.LogMessage(ActionWriteToClient, fmt.Sprintf("local(%v)->remote(%v)", rp.sourceConn.LocalAddr().String(),
			rp.sourceConn.RemoteAddr().String()), reply.StartT, err))
		err = fmt.Errorf("ReplProtocol(%v) ReplProtocalID (%v) will exit error(%v)",
			rp.sourceConn.RemoteAddr(), rp.remote, err)
		log.LogErrorf(err.Error())
		rp.Stop(err)
	}
	if log.IsDebugEnabled() {
		log.LogDebugf(reply.LogMessage(ActionWriteToClient,
			rp.sourceConn.RemoteAddr().String(), reply.StartT, err))
	}

}

// Stop stops the replication protocol.
func (rp *ReplProtocol) Stop(stopErr error) {
	rp.exitedMu.Lock()
	defer rp.exitedMu.Unlock()
	if stopErr != nil && rp.stopError == "" {
		rp.stopError = stopErr.Error()
	}
	if atomic.CompareAndSwapInt32(&rp.exited, ReplRuning, ReplExiting) && rp.exitC != nil {
		close(rp.exitC)
	}
}

// Allocate the connections to the followers. We use partitionId + extentId + followerAddr as the key.
// Note that we need to ensure the order of packets sent to the datanode is consistent here.
func (rp *ReplProtocol) allocateFollowersConns(p *Packet, index int) (transport *FollowerTransport, err error) {
	rp.lock.RLock()
	transport = rp.followerConnects[p.followersAddrs[index]]
	if transport != nil {
		atomic.StoreInt64(&transport.lastActiveTime, time.Now().Unix())
	}
	rp.lock.RUnlock()
	if transport == nil {
		transport, err = NewFollowersTransport(p.followersAddrs[index], rp.replId)
		if err != nil {
			return
		}
		rp.lock.Lock()
		rp.followerConnects[p.followersAddrs[index]] = transport
		rp.lock.Unlock()
	}

	return
}

func (rp *ReplProtocol) getNextPacket() (e *list.Element) {
	rp.packetListLock.RLock()
	e = rp.packetList.Front()
	rp.packetListLock.RUnlock()

	return
}

func (rp *ReplProtocol) pushPacketToList(e *Packet) {
	rp.packetListLock.Lock()
	rp.packetList.PushBack(e)
	rp.packetListLock.Unlock()
}

func (rp *ReplProtocol) cleanToBeProcessCh() {
	for {
		select {
		case p := <-rp.toBeProcessedCh:
			if p == nil {
				return
			}
			_ = rp.postFunc(p)
			rp.forceCleanDataPoolFlag(p, "cleanToBeProcessCh")
			rp.forceCleanPacketPoolFlag(p, "cleanToBeProcessCh")
		default:
			return
		}
	}
}

func (rp *ReplProtocol) cleanResponseCh() {
	for {
		select {
		case p := <-rp.responseCh:
			if p == nil {
				return
			}
			_ = rp.postFunc(p)
			rp.forceCleanDataPoolFlag(p, "cleanResponseCh")
			log.LogErrorf("Action[cleanResponseCh] request(%v) because (%v) elapsed (%v)", p.GetUniqueLogId(), rp.stopError, p.Elapsed())
			rp.forceCleanPacketPoolFlag(p, "cleanResponseCh")
		default:
			return
		}
	}
}

func (rp *ReplProtocol) loggingIsAllThreadsExit() {
	allExit := true
	var threadStat [3]int
	rp.allThreadStatsLock.Lock()
	for index, stat := range rp.allThreadStats {
		threadStat[index] = stat
	}
	rp.allThreadStatsLock.Unlock()

	for _, stat := range threadStat {
		if stat != ReplProtocalThreadExit {
			allExit = false
			break
		}
	}

	if allExit {
		return
	}
	log.LogErrorf("ReplProtocol(%v) not only allThreads  exit threadStats(%v)", rp.sourceConn.RemoteAddr(), threadStat)
}

func (rp *ReplProtocol) cleanDataPoolFlag(p *Packet, srcFunc string) {
	var ok bool
	if !p.isUseDataPool() {
		return
	}
	if ok = p.cleanDataPoolFlag(srcFunc); ok {
		rp.addPutNumFromDataPoolCnt()
		return
	}
}

func (rp *ReplProtocol) cleanPacketPoolFlag(p *Packet, srcFunc string) {
	var ok bool
	if !p.isUsePacketPool() {
		return
	}
	if ok = p.cleanPacketPoolFlag(srcFunc); ok {
		rp.addPutNumFromPacketPoolCnt()
		return
	}
}

func (rp *ReplProtocol) forceCleanDataPoolFlag(p *Packet, srcFunc string) {
	var ok bool
	if !p.isUseDataPool() {
		return
	}
	if ok = p.forceCleanDataPoolFlag(srcFunc); ok {
		rp.addPutNumFromDataPoolCnt()
		return
	}

}

func (rp *ReplProtocol) forceCleanPacketPoolFlag(p *Packet, srcFunc string) {
	var ok bool
	if !p.isUsePacketPool() {
		return
	}
	if ok = p.forceCleanPacketPoolFlag(srcFunc); ok {
		rp.addPutNumFromPacketPoolCnt()
		return
	}
}

func (rp *ReplProtocol) addGetNumFromDataPoolCnt() {
	atomic.AddInt64(&rp.getNumFromDataPool, 1)
}

func (rp *ReplProtocol) addPutNumFromDataPoolCnt() {
	atomic.AddInt64(&rp.putNumToDataPool, 1)
}

func (rp *ReplProtocol) addGetNumFromPacketPoolCnt() {
	atomic.AddInt64(&rp.getPacketFromPoolCnt, 1)
}

func (rp *ReplProtocol) addPutNumFromPacketPoolCnt() {
	atomic.AddInt64(&rp.putPacketToPoolCnt, 1)
}

// If the replication protocol exits, then clear all the packet resources.
func (rp *ReplProtocol) cleanResource() {
	rp.loggingIsAllThreadsExit()
	rp.lock.RLock()
	for _, transport := range rp.followerConnects {
		transport.Destory()
	}
	rp.lock.RUnlock()
	close(rp.responseCh)
	close(rp.toBeProcessedCh)
	close(rp.ackCh)
	rp.cleanToBeProcessCh()
	rp.packetListLock.Lock()
	for e := rp.packetList.Front(); e != nil; e = e.Next() {
		request := e.Value.(*Packet)
		_ = rp.postFunc(request)
		log.LogErrorf("Action[cleanResource] request(%v) because (%v) elapsed (%v)", request.GetUniqueLogId(), rp.stopError, request.Elapsed())
		rp.forceCleanDataPoolFlag(request, "cleanResourcePacketList")
		rp.forceCleanPacketPoolFlag(request, "cleanResourcePacketList")
	}

	for e := rp.forwardPacketCheckList.Front(); e != nil; e = e.Next() {
		request := e.Value.(*Packet)
		rp.forceCleanDataPoolFlag(request, "cleanResourceforwardPacketCheckList")
		rp.forceCleanPacketPoolFlag(request, "cleanResourceforwardPacketCheckList")
	}
	rp.cleanResponseCh()
	rp.packetList = list.New()
	rp.forwardPacketCheckList = list.New()
	rp.packetList = nil
	rp.followerConnects = nil
	rp.packetListLock.Unlock()
	if atomic.LoadInt64(&rp.getNumFromDataPool) != atomic.LoadInt64(&rp.putNumToDataPool) ||
		atomic.LoadInt64(&rp.getPacketFromPoolCnt) != atomic.LoadInt64(&rp.putPacketToPoolCnt) {
		log.LogWarnf(fmt.Sprintf("repl(%v) ReplProtocol(%v) getNumFromDataPool(%v) putNumToDataPool(%v)"+
			" getPacketFromPoolCnt(%v) putPacketToPoolCnt(%v)",
			rp.replId, rp.sourceConn.RemoteAddr(), atomic.LoadInt64(&rp.getNumFromDataPool),
			atomic.LoadInt64(&rp.putNumToDataPool), atomic.LoadInt64(&rp.getPacketFromPoolCnt),
			atomic.LoadInt64(&rp.putPacketToPoolCnt)))
	}

	ReplProtocalMap.Delete(rp.replId)

}

func (rp *ReplProtocol) deletePacket(reply *Packet, e *list.Element) (success bool) {
	rp.packetListLock.Lock()
	defer rp.packetListLock.Unlock()
	rp.packetList.Remove(e)
	success = true
	_ = rp.putResponse(reply)
	return
}

func (rp *ReplProtocol) putResponse(reply *Packet) (err error) {
	select {
	case rp.responseCh <- reply:
		return
	default:
		_ = rp.postFunc(reply)
		msg := fmt.Sprintf("request(%v) response Chan is full(%v)", reply.GetUniqueLogId(), len(rp.responseCh))
		exporter.WarningCritical(msg)
		return err
	}
}

func (rp *ReplProtocol) putToBeProcess(request *Packet) (err error) {
	select {
	case rp.toBeProcessedCh <- request:
		return
	default:
		_ = rp.postFunc(request)
		msg := fmt.Sprintf("request(%v) toBeProcessed Chan is full(%v)", request.GetUniqueLogId(), len(rp.toBeProcessedCh))
		exporter.WarningCritical(msg)
		return err
	}
}

func (rp *ReplProtocol) putAck(request *Packet) (err error) {
	select {
	case rp.ackCh <- struct{}{}:
		return
	default:
		err = fmt.Errorf("request(%v) ack Chan has full (%v)", request.GetUniqueLogId(), len(rp.ackCh))
		log.LogErrorf(err.Error())
		return err
	}
}
