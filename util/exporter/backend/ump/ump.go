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

package ump

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/util/log"
)

type CollectMethod int

func (c CollectMethod) String() string {
	switch c {
	case CollectMethodFile:
		return "File"
	case CollectMethodJMTP:
		return "JMTP"
	default:
	}
	return "Unknown"
}

func (c CollectMethod) Valid() bool {
	switch c {
	case CollectMethodFile, CollectMethodJMTP:
		return true
	default:
	}
	return false
}

func (c CollectMethod) Int64() int64 {
	return int64(c)
}

func ParseCollectMethod(v int) CollectMethod {
	return CollectMethod(v)
}

const (
	CollectMethodUnknown CollectMethod = iota
	CollectMethodFile
	CollectMethodJMTP

	defaultCollectMethod = CollectMethodFile
)

type TpObject struct {
	StartTime time.Time
	EndTime   time.Time
	UmpType   interface{}
}

func NewTpObject() (o *TpObject) {
	o = new(TpObject)
	o.StartTime = time.Now()
	return
}

const (
	TpMethod        = "TP"
	HeartbeatMethod = "Heartbeat"
	FunctionError   = "FunctionError"
)

var (
	HostName      string
	AppName       string
	LogTimeForMat = "20060102150405000"
	AlarmPool     = &sync.Pool{New: func() interface{} {
		return new(BusinessAlarm)
	}}
	TpObjectPool = &sync.Pool{New: func() interface{} {
		return new(TpObject)
	}}
	SystemAlivePool = &sync.Pool{New: func() interface{} {
		return new(SystemAlive)
	}}
	FunctionTpPool = &sync.Pool{New: func() interface{} {
		return new(FunctionTp)
	}}
	FunctionTpGroupByPool = &sync.Pool{New: func() interface{} {
		return new(FunctionTpGroupBy)
	}}
	enableUmp          = true
	FunctionTPMapCount = 16
	FuncationTPMap     []sync.Map
	FunctionTPKeyMap   *sync.Map
	umpCollectMethod   CollectMethod
	jmtpWrite          *JmtpWrite
	jmtpWriteMutex     sync.Mutex

	checkUmpWaySleepTime = 10 * time.Second
	writeTpSleepTime     = time.Second
	aliveTickerTime      = 20 * time.Second
	alarmTickerTime      = time.Second
)

func init() {
	FuncationTPMap = make([]sync.Map, FunctionTPMapCount)
	FunctionTPKeyMap = new(sync.Map)
	umpCollectMethod = CollectMethodFile
}

func InitUmp(module, appName string) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("InitUmp err(%v)", err)
		}
	}()
	AppName = appName
	if err = initLogName(module); err != nil {
		return
	}
	backGroudWriteLog()
	return nil
}

func StopUmp() {
	stopLogWriter()
	if jmtpWrite != nil {
		jmtpWrite.stop()
	}
	jmtpWrite = nil
	FuncationTPMap = nil
	AlarmPool = nil
	TpObjectPool = nil
	SystemAlivePool = nil
	FunctionTpPool = nil
	FunctionTpGroupByPool = nil
}

func BeforeTP(key string) (o *TpObject) {
	if !enableUmp {
		return
	}

	o = TpObjectPool.Get().(*TpObject)
	o.StartTime = time.Now()
	tp := FunctionTpGroupByPool.Get().(*FunctionTpGroupBy)
	//tp.HostName = HostName
	//tp.currTime = o.StartTime
	tp.Key = key
	//tp.ProcessState = "0"
	o.UmpType = tp

	return
}

func BeforeTPWithStartTime(key string, start time.Time) (o *TpObject) {
	if !enableUmp {
		return
	}

	o = TpObjectPool.Get().(*TpObject)
	o.StartTime = start
	tp := FunctionTpGroupByPool.Get().(*FunctionTpGroupBy)
	tp.Key = key
	//tp.ProcessState = "0"
	o.UmpType = tp

	return
}

//func AfterTPOld(o *TpObject, err error) {
//	if !enableUmp {
//		return
//	}
//	tp := o.UmpType.(*FunctionTpGroupBy)
//	tp.elapsedTime = (int64)(time.Since(o.StartTime) / 1e6)
//	tp.ProcessState = "0"
//	if err != nil {
//		tp.ProcessState = "1"
//	}
//	tp.count = 1
//	index := tp.elapsedTime % int64(FunctionTPMapCount)
//	mkey := tp.Key + "_" + strconv.FormatInt(tp.elapsedTime, 10)
//	v, ok := FuncationTPMap[index].Load(mkey)
//	if !ok {
//		FuncationTPMap[index].Store(mkey, tp)
//	} else {
//		atomic.AddInt64(&v.(*FunctionTpGroupBy).count, 1)
//		TpObjectPool.Put(o)
//	}
//}
//
//func AfterTPUsOld(o *TpObject, err error) {
//	if !enableUmp {
//		return
//	}
//	tp := o.UmpType.(*FunctionTpGroupBy)
//	tp.elapsedTime = (int64)(time.Since(o.StartTime) / 1e3)
//	tp.ProcessState = "0"
//	if err != nil {
//		tp.ProcessState = "1"
//	}
//	tp.count = 1
//	index := tp.elapsedTime % int64(FunctionTPMapCount)
//	mkey := tp.Key + "_" + strconv.FormatInt(tp.elapsedTime, 10)
//	v, ok := FuncationTPMap[index].Load(mkey)
//	if !ok {
//		FuncationTPMap[index].Store(mkey, tp)
//	} else {
//		atomic.AddInt64(&v.(*FunctionTpGroupBy).count, 1)
//		TpObjectPool.Put(o)
//	}
//	return
//}

func AfterTP(o *TpObject, err error) {
	if !enableUmp {
		return
	}
	tp := o.UmpType.(*FunctionTpGroupBy)
	tp.elapsedTime = (int64)(time.Since(o.StartTime) / 1e6)
	//tp.ProcessState = "0"
	if err != nil {
		//tp.ProcessState = "1"
		tp.elapsedTime = -1
	}
	tp.count = 1
	mergeLogByUMPKey(tp)
	TpObjectPool.Put(o)
}

func AfterTPWithCount(o *TpObject, value int64, err error) {
	if !enableUmp {
		return
	}
	tp := o.UmpType.(*FunctionTpGroupBy)
	tp.elapsedTime = (int64)(time.Since(o.StartTime) / 1e6)
	//tp.ProcessState = "0"
	if err != nil {
		//tp.ProcessState = "1"
		tp.elapsedTime = -1
	}
	tp.count = value
	mergeLogByUMPKey(tp)
	TpObjectPool.Put(o)
}

func AfterTPWithCostUS(o *TpObject, value int64, err error) {
	if !enableUmp {
		return
	}
	tp := o.UmpType.(*FunctionTpGroupBy)
	tp.elapsedTime = (int64)(value)
	//tp.ProcessState = "0"
	if err != nil {
		//tp.ProcessState = "1"
		tp.elapsedTime = -1
	}
	tp.count = 1
	if isNewElapsedTime := mergeLogByUMPKey(tp); !isNewElapsedTime {
		TpObjectPool.Put(o)
	}
	return
}

func AfterTPWithCost(o *TpObject, value int64, err error) {
	if !enableUmp {
		return
	}
	tp := o.UmpType.(*FunctionTpGroupBy)
	tp.elapsedTime = (int64)(value)
	//tp.ProcessState = "0"
	if err != nil {
		//tp.ProcessState = "1"
		tp.elapsedTime = -1
	}
	tp.count = 1
	if isNewElapsedTime := mergeLogByUMPKey(tp); !isNewElapsedTime {
		TpObjectPool.Put(o)
	}
	return
}

func AfterTPUs(o *TpObject, err error) {
	if !enableUmp {
		return
	}
	tp := o.UmpType.(*FunctionTpGroupBy)
	tp.elapsedTime = (int64)(time.Since(o.StartTime) / 1e3)
	//tp.ProcessState = "0"
	if err != nil {
		//tp.ProcessState = "1"
		tp.elapsedTime = -1
	}
	tp.count = 1
	if isNewElapsedTime := mergeLogByUMPKey(tp); !isNewElapsedTime {
		TpObjectPool.Put(o)
	}
	return
}

func mergeLogByUMPKey(tp *FunctionTpGroupBy) (isNewElapsedTime bool) {
	var elapsedTimeCounter *sync.Map
	value, ok := FunctionTPKeyMap.Load(tp.Key)
	if !ok {
		elapsedTimeCounter = &sync.Map{}
		elapsedTimeCounter.Store(tp.elapsedTime, tp)
		FunctionTPKeyMap.Store(tp.Key, elapsedTimeCounter)
		isNewElapsedTime = true
		return
	}
	elapsedTimeCounter = value.(*sync.Map)
	tpObj, ok := elapsedTimeCounter.Load(tp.elapsedTime)
	if !ok {
		elapsedTimeCounter.Store(tp.elapsedTime, tp)
		isNewElapsedTime = true
		return
	}
	c := tpObj.(*FunctionTpGroupBy)
	atomic.AddInt64(&c.count, 1)
	return
}

func Alive(key string) {
	if !enableUmp {
		return
	}
	alive := SystemAlivePool.Get().(*SystemAlive)
	alive.HostName = HostName
	alive.Key = key
	alive.Time = time.Now().Format(LogTimeForMat)
	ch := SystemAliveLogWrite.logCh
	if GetUmpCollectMethod() == CollectMethodJMTP && jmtpWrite != nil {
		ch = jmtpWrite.aliveCh
	}
	select {
	case ch <- alive:
	default:
	}
	return
}

func Alarm(key, detail string) {
	if !enableUmp {
		return
	}
	alarm := AlarmPool.Get().(*BusinessAlarm)
	alarm.Time = time.Now().Format(LogTimeForMat)
	alarm.Key = key
	alarm.HostName = HostName
	alarm.BusinessType = "0"
	alarm.Value = "0"
	alarm.Detail = detail
	if len(alarm.Detail) > 512 {
		rs := []rune(detail)
		alarm.Detail = string(rs[0:510])
	}

	inflight := &BusinessAlarmLogWrite.inflight
	ch := BusinessAlarmLogWrite.logCh
	if GetUmpCollectMethod() == CollectMethodJMTP && jmtpWrite != nil {
		inflight = &jmtpWrite.inflight
		ch = jmtpWrite.alarmCh
	}
	select {
	case ch <- alarm:
		atomic.AddInt32(inflight, 1)
	default:
	}
	return
}

func FlushAlarm() {
	flushAlarm(&BusinessAlarmLogWrite.inflight, BusinessAlarmLogWrite.empty)
	if jmtpWrite != nil {
		flushAlarm(&jmtpWrite.inflight, jmtpWrite.empty)
	}
}

func flushAlarm(inflight *int32, empty chan struct{}) {
	if atomic.LoadInt32(inflight) <= 0 {
		return
	}

	for {
		select {
		case <-empty:
			if atomic.LoadInt32(inflight) <= 0 {
				return
			}
		}
	}
}

func GetUmpCollectMethod() CollectMethod {
	return umpCollectMethod
}

// Delay jmtp client initialization to the first set of umpCollectMethod.
// Should call SetUmpJmtpAddr() before this function.
func SetUmpCollectMethod(method CollectMethod) {
	jmtpWriteMutex.Lock()
	defer jmtpWriteMutex.Unlock()
	if !method.Valid() {
		method = defaultCollectMethod
	}
	if method == CollectMethodJMTP && jmtpWrite == nil {
		if jmtp, err := NewJmtpWrite(); err == nil {
			jmtpWrite = jmtp
		}
	}
	umpCollectMethod = method
}

func SetUmpJmtpAddr(jmtpAddress string) {
	jmtpWriteMutex.Lock()
	defer jmtpWriteMutex.Unlock()
	if jmtpAddress == "" || jmtpAddress == umpJmtpAddress {
		return
	}
	umpJmtpAddress = jmtpAddress
	if jmtpWrite != nil {
		jmtpWrite.stop()
		if jmtp, err := NewJmtpWrite(); err == nil {
			jmtpWrite = jmtp
		}
	}
}

func SetUmpJmtpBatch(batch uint) {
	if batch > 0 && batch <= maxJmtpBatch {
		umpJmtpBatch = batch
	}
}
