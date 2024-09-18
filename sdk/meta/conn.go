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

package meta

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

const (
	SendRetryLimit    = 100
	SendRetryInterval = 100 * time.Millisecond
	SendTimeLimit     = 60 * time.Second

	ReadConsistenceRetryLimit   = 50
	ReadConsistenceRetryTimeout = 60 * time.Second
)

type MetaConn struct {
	conn *net.TCPConn
	id   uint64 //PartitionID
	addr string //MetaNode addr
}

// Connection managements
//

func (mc *MetaConn) String() string {
	if mc == nil {
		return ""
	}
	return fmt.Sprintf("partitionID(%v) addr(%v)", mc.id, mc.addr)
}

func (mw *MetaWrapper) getConn(ctx context.Context, partitionID uint64, addr string) (*MetaConn, error) {
	conn, err := mw.conns.GetConnect(addr)
	if err != nil {
		log.LogWarnf("GetConnect conn: addr(%v) err(%v)", addr, err)
		return nil, err
	}
	mc := &MetaConn{conn: conn, id: partitionID, addr: addr}
	return mc, nil
}

func (mw *MetaWrapper) putConn(mc *MetaConn, err error) {
	mw.conns.PutConnectWithErr(mc.conn, err)
}

func (mw *MetaWrapper) sendWriteToMP(ctx context.Context, mp *MetaPartition, req *proto.Packet) (resp *proto.Packet, needCheckRead bool, err error) {
	if err = mw.checkLimiter(ctx, req.Opcode); err != nil {
		log.LogWarnf("sendWriteToMP: check limit err(%v) req(%v)", err, req)
		return
	}

	addr := mp.GetLeaderAddr()
	retryCount := 0
	var successAddr string
	for {
		retryCount++
		resp, needCheckRead, successAddr, err = mw.sendToMetaPartition(ctx, mp, req, addr)
		if (err == nil && !resp.ShouldRetry()) || err == proto.ErrVolNotExists {
			if successAddr != "" && successAddr != addr {
				mp.LeaderAddr = proto.NewAtomicString(successAddr)
			}
			return
		}
		// operations don't need to retry
		if req.Opcode == proto.OpMetaCreateInode || !mw.InfiniteRetry {
			return
		}
		log.LogWarnf("sendWriteToMP: err(%v) resp(%v) req(%v) mp(%v) retry time(%v)", err, resp, req, mp, retryCount)
		umpMsg := fmt.Sprintf("send write(%v) to mp(%v) err(%v) resp(%v) retry time(%v)", req, mp, err, resp, retryCount)
		handleUmpAlarm(mw.cluster, mw.volname, req.GetOpMsg(), umpMsg)
		time.Sleep(SendRetryInterval)
	}
}

func (mw *MetaWrapper) sendReadToMP(ctx context.Context, mp *MetaPartition, req *proto.Packet) (resp *proto.Packet, err error) {
	if err = mw.checkLimiter(ctx, req.Opcode); err != nil {
		log.LogWarnf("sendReadToMP: check limit err(%v) req(%v)", err, req)
		return
	}

	addr := mp.GetLeaderAddr()
	retryCount := 0
	var successAddr string
	for {
		retryCount++
		resp, _, successAddr, err = mw.sendToMetaPartition(ctx, mp, req, addr)
		if (err == nil && !resp.ShouldRetry()) || err == proto.ErrVolNotExists {
			if successAddr != "" && successAddr != addr {
				mp.LeaderAddr = proto.NewAtomicString(successAddr)
			}
			return
		}
		if proto.IsDbBack {
			return
		}
		log.LogWarnf("sendReadToMP: send to leader failed and try to read consistent, req(%v) mp(%v) err(%v) resp(%v)", req, mp, err, resp)
		resp, err = mw.readConsistentFromHosts(ctx, mp, req, true)
		if err == nil && !resp.ShouldRetry() {
			return
		}
		if mw.CrossRegionHATypeQuorum() {
			resp, err = mw.readConsistentFromHosts(ctx, mp, req, false)
			if err == nil && !resp.ShouldRetry() {
				return
			}
		}
		if !mw.InfiniteRetry {
			return
		}
		log.LogWarnf("sendReadToMP: err(%v) resp(%v) req(%v) mp(%v) retry time(%v)", err, resp, req, mp, retryCount)
		umpMsg := fmt.Sprintf("send read(%v) to mp(%v) err(%v) resp(%v) retry time(%v)", req, mp, err, resp, retryCount)
		handleUmpAlarm(mw.cluster, mw.volname, req.GetOpMsg(), umpMsg)
		time.Sleep(SendRetryInterval)
	}
}

func (mw *MetaWrapper) readConsistentFromHosts(ctx context.Context, mp *MetaPartition, req *proto.Packet, strongConsistency bool) (resp *proto.Packet, err error) {
	var (
		targetHosts []string
		errMap      map[string]error
		isErr       bool
	)
	start := time.Now()
	// compare applied ID of replicas and choose the max one
	for i := 0; i < ReadConsistenceRetryLimit; i++ {
		errMap = make(map[string]error)
		if strongConsistency {
			members := excludeLearner(mp)
			targetHosts, isErr = mw.getTargetHosts(ctx, mp, members, (len(members)+1)/2)
		} else {
			targetHosts, isErr = mw.getTargetHosts(ctx, mp, mp.Members, len(mp.Members)-1)
		}
		if !isErr && len(targetHosts) > 0 {
			req.ArgLen = 1
			req.Arg = make([]byte, req.ArgLen)
			req.Arg[0] = proto.FollowerReadFlag
			for _, host := range targetHosts {
				resp, _, err = mw.sendToHost(ctx, mp, req, host)
				if (err == nil && !resp.ShouldRetry()) || err == proto.ErrVolNotExists {
					return
				}
				errMap[host] = errors.NewErrorf("err(%v) resp(%v)", err, resp)
				log.LogWarnf("mp readConsistentFromHosts: failed req(%v) mp(%v) addr(%v) err(%v) resp(%v), try next host", req, mp, host, err, resp)
			}
		}
		log.LogWarnf("mp readConsistentFromHosts failed: try next round, req(%v) isErr(%v) targetHosts(%v) errMap(%v)", req, isErr, targetHosts, errMap)
		if time.Since(start) > ReadConsistenceRetryTimeout {
			log.LogWarnf("mp readConsistentFromHosts: retry timeout, req(%v) mp(%v) time(%v)", req, mp, time.Since(start))
			break
		}
	}
	log.LogWarnf("mp readConsistentFromHosts exit: failed req(%v) mp(%v) isErr(%v) targetHosts(%v) errMap(%v)", req, mp, isErr, targetHosts, errMap)
	return nil, errors.New(fmt.Sprintf("readConsistentFromHosts: failed, req(%v) mp(%v) isErr(%v) targetHosts(%v) errMap(%v)", req, mp, isErr, targetHosts, errMap))
}

func (mw *MetaWrapper) sendToMetaPartition(ctx context.Context, mp *MetaPartition, req *proto.Packet, addr string) (resp *proto.Packet, needCheckRead bool, successAddr string, err error) {
	var (
		errMap        map[int]error
		start         time.Time
		retryInterval time.Duration
		failedAddr    string
		needCheck     bool
		j             int
	)
	resp, _, err = mw.sendToHost(ctx, mp, req, addr)
	if (err == nil && !resp.ShouldRetry()) || err == proto.ErrVolNotExists {
		successAddr = addr
		goto out
	}
	log.LogWarnf("sendToMetaPartition: leader failed req(%v) mp(%v) addr(%v) err(%v) resp(%v)", req, mp, addr, err, resp)

	errMap = make(map[int]error, len(mp.Members))
	start = time.Now()
	retryInterval = SendRetryInterval

	failedAddr = addr
	for i := 0; i < SendRetryLimit; i++ {
		for j, addr = range mp.Members {
			if addr == failedAddr {
				continue
			}
			resp, needCheck, err = mw.sendToHost(ctx, mp, req, addr)
			if (err == nil && !resp.ShouldRetry()) || err == proto.ErrVolNotExists {
				successAddr = addr
				goto out
			}
			if err == nil {
				err = errors.New(fmt.Sprintf("request should retry[%v]", resp.GetResultMsg()))
			}
			errMap[j] = err
			if needCheck {
				needCheckRead = true
			}
			log.LogWarnf("sendToMetaPartition: retry failed req(%v) mp(%v) addr(%v) err(%v) resp(%v)", req, mp, addr, err, resp)
		}
		if time.Since(start) > SendTimeLimit {
			log.LogWarnf("sendToMetaPartition: retry timeout req(%v) mp(%v) time(%v)", req, mp, time.Since(start))
			break
		}
		log.LogWarnf("sendToMetaPartition: req(%v) mp(%v) retry in (%v)", req, mp, retryInterval)
		time.Sleep(retryInterval)
		retryInterval += SendRetryInterval
	}

out:
	if err == nil && resp == nil {
		err = errors.New(fmt.Sprintf("sendToMetaPartition failed: req(%v) mp(%v) errs(%v) resp(%v)", req, mp, errMap, resp))
	}
	if err != nil {
		return nil, needCheckRead, successAddr, err
	}
	log.LogDebugf("sendToMetaPartition successful: req(%v) mp(%v) addr(%v) resp(%v)", req, mp, addr, resp)
	return resp, needCheckRead, successAddr, nil
}

func (mw *MetaWrapper) sendToHost(ctx context.Context, mp *MetaPartition, req *proto.Packet, addr string) (resp *proto.Packet, needCheckRead bool, err error) {
	if mw.VolNotExists() {
		return nil, false, proto.ErrVolNotExists
	}

	var mc *MetaConn
	if addr == "" {
		return nil, false, errors.New(fmt.Sprintf("sendToHost failed: leader addr empty, req(%v) mp(%v)", req, mp))
	}
	req.PartitionID = mp.PartitionID
	mc, err = mw.getConn(ctx, mp.PartitionID, addr)
	if err != nil {
		return
	}
	defer func() {
		mw.putConn(mc, err)
	}()

	// Write to connection with tracing.
	if err = func() (err error) {
		err = req.WriteToConnNs(mc.conn, mw.connConfig.WriteTimeoutNs)
		return
	}(); err != nil {
		return nil, false, errors.Trace(err, "Failed to write to conn, req(%v)", req)
	}

	resp = proto.NewPacket(req.Ctx())

	// Read from connection with tracing.
	if err = func() (err error) {
		err = resp.ReadFromConnNs(mc.conn, mw.connConfig.ReadTimeoutNs)
		return
	}(); err != nil {
		return nil, true, errors.Trace(err, "Failed to read from conn, req(%v)", req)
	}
	// Check if the ID and OpCode of the response are consistent with the request.
	if resp.ReqID != req.ReqID || resp.Opcode != req.Opcode {
		log.LogWarnf("sendToHost err: the response packet mismatch with request: conn(%v to %v) req(%v) resp(%v)",
			mc.conn.LocalAddr(), mc.conn.RemoteAddr(), req, resp)
		err = syscall.EBADMSG
		return nil, true, err
	}
	log.LogDebugf("sendToHost successful: mp(%v) addr(%v) req(%v) resp(%v)", mp, addr, req, resp)
	return resp, false, nil
}

//func sortMembers(leader string, members []string) []string {
//	if leader == "" {
//		return members
//	}
//	for i, addr := range members {
//		if addr == leader {
//			members[i], members[0] = members[0], members[i]
//			break
//		}
//	}
//	return members
//}
