package datanode

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cubefs/cubefs/datanode/mock"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/repl"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
	"github.com/jacobsa/daemonize"
)

var (
	localIP          string
	localNodeAddress string
)

const (
	mockClusterName   = mock.TestCluster
	testLogDir        = "/cfs/log"
	testDiskPath      = "/cfs/mockdisk/data1"
	raftHeartBeatPort = "17331"
	raftReplicaPort   = "17341"
	tcpProtoPort      = "11010"
	profPort          = "11210"
	mockZone01        = "zone-01"
)

func getLocalIp() {
	nets, err := net.Interfaces()
	if err != nil {
		fmt.Printf("can not get sys interfaces info:%v\n", err.Error())
	}

	for _, iter := range nets {
		var addrIp []net.Addr
		addrIp, err = iter.Addrs()
		if err != nil {
			continue
		}
		if strings.Contains(iter.Name, "bo") || strings.Contains(iter.Name, "eth") || strings.Contains(iter.Name, "enp") {
			for _, addr := range addrIp {
				if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
					localIP = strings.Split(addr.String(), "/")[0]
					localNodeAddress = localIP + ":" + tcpProtoPort
					return
				}
			}
		}
	}
}

func newFakeDataNode() *fakeDataNode {
	fdn := &fakeDataNode{
		DataNode: DataNode{
			clusterID:       mockClusterName,
			raftHeartbeat:   raftHeartBeatPort,
			raftReplica:     raftReplicaPort,
			port:            tcpProtoPort,
			zoneName:        mockZone01,
			httpPort:        profPort,
			localIP:         localIP,
			localServerAddr: localNodeAddress,
			nodeID:          uint64(111),
			stopC:           make(chan bool),
		},
		fakeNormalExtentId: 1025,
		fakeTinyExtentId:   1,
	}
	MasterClient.AddNode(mock.TestMasterHost)

	cfgStr := `{
		"disks": [
			"` + testDiskPath + `:5368709120"
		],
		"enableRootDisk": true,
		"listen": "` + tcpProtoPort + `",
		"raftHeartbeat": "` + raftHeartBeatPort + `",
		"raftReplica": "` + raftReplicaPort + `",
		"logDir":"` + testLogDir + `",
		"raftDir":"` + testLogDir + `",
		"masterAddr": [
			"` + mock.TestMasterHost + `",
			"` + mock.TestMasterHost + `",
			"` + mock.TestMasterHost + `"
		],
		"prof": "` + profPort + `"
	}`
	_, err := log.InitLog(testLogDir, "mock_datanode", log.DebugLevel, nil)
	if err != nil {
		_ = daemonize.SignalOutcome(fmt.Errorf("Fatal: failed to init log - %v ", err))
		panic(err.Error())
	}
	defer log.LogFlush()
	cfg := config.LoadConfigString(cfgStr)

	if err = fdn.Start(cfg); err != nil {
		panic(fmt.Sprintf("startTCPService err(%v)", err))
	}
	return fdn
}

func FakeDirCreate() (err error) {
	_, err = os.Stat(testDiskPath)
	if err == nil {
		os.RemoveAll(testDiskPath)
	}
	if err = os.MkdirAll(testDiskPath, 0766); err != nil {
		panic(err)
	}
	return
}

var (
	partitionIdNum uint64
)

type fakeDataNode struct {
	DataNode
	fakeNormalExtentId uint64
	fakeTinyExtentId   uint64
}

func TestGetTinyExtentHoleInfo(t *testing.T) {
	partitionIdNum++
	dp := PrepareDataPartition(true, true, t, partitionIdNum)
	if dp == nil {
		t.Fatalf("prepare data partition failed")
		return
	}
	defer fakeNode.fakeDeleteDataPartition(t, partitionIdNum)
	if _, err := dp.getTinyExtentHoleInfo(fakeNode.fakeTinyExtentId); err != nil {
		t.Fatalf("getHttpRequestResp err(%v)", err)
	}
}

func TestHandleTinyExtentAvaliRead(t *testing.T) {
	partitionIdNum++
	dp := PrepareDataPartition(true, true, t, partitionIdNum)
	if dp == nil {
		t.Fatalf("prepare data partition failed")
		return
	}
	defer fakeNode.fakeDeleteDataPartition(t, partitionIdNum)
	p := repl.NewTinyExtentRepairReadPacket(context.Background(), partitionIdNum, fakeNode.fakeTinyExtentId, 0, 10, false)
	opCode, err, msg := fakeNode.operateHandle(t, p)
	if err != nil {
		t.Fatal(err)
		return
	}
	if opCode != proto.OpOk {
		t.Fatal(msg)
	}

}

func (fdn *fakeDataNode) fakeDeleteDataPartition(t *testing.T, partitionId uint64) {
	req := &proto.DeleteDataPartitionRequest{
		PartitionId: partitionId,
	}

	task := proto.NewAdminTask(proto.OpDeleteDataPartition, localNodeAddress, req)
	body, err := json.Marshal(task)
	p := &repl.Packet{
		Packet: proto.Packet{
			Magic:       proto.ProtoMagic,
			ReqID:       proto.GenerateRequestID(),
			Opcode:      proto.OpDeleteDataPartition,
			PartitionID: partitionId,
			Data:        body,
			Size:        uint32(len(body)),
			StartT:      time.Now().UnixNano(),
		},
	}
	if err != nil {
		t.Fatal(err)
		return
	}
	fdn.handlePacketToDeleteDataPartition(p)
	if p.ResultCode != proto.OpOk {
		t.Fatalf("delete partiotion failed msg[%v]", p.ResultCode)
	}

}

func PrepareDataPartition(extentCreate, dataPrepare bool, t *testing.T, partitionId uint64) *DataPartition {
	dp := fakeNode.fakeCreateDataPartition(t, partitionId)
	if dp == nil {
		t.Fatalf("create Partition failed")
		return nil
	}

	if !extentCreate {
		return dp
	}

	if err := fakeNode.fakeCreateExtent(dp, t, fakeNode.fakeNormalExtentId); err != nil {
		t.Fatal(err)
		return nil
	}

	if err := fakeNode.fakeCreateExtent(dp, t, fakeNode.fakeTinyExtentId); err != nil {
		t.Fatal(err)
		return nil
	}

	if !dataPrepare {
		return dp
	}

	if _, err := fakeNode.prepareTestData(t, dp, fakeNode.fakeNormalExtentId); err != nil {
		return nil
	}

	if _, err := fakeNode.prepareTestData(t, dp, fakeNode.fakeTinyExtentId); err != nil {
		return nil
	}
	dp.ReloadSnapshot()
	return dp
}

func (fdn *fakeDataNode) fakeCreateDataPartition(t *testing.T, partitionId uint64) (dp *DataPartition) {
	req := &proto.CreateDataPartitionRequest{
		PartitionId:   partitionId,
		PartitionSize: 5 * unit.GB, // 5GB
		VolumeId:      "testVol",
		Hosts: []string{
			localNodeAddress,
			localNodeAddress,
			localNodeAddress,
		},
		Members: []proto.Peer{
			{ID: fdn.nodeID, Addr: fdn.localServerAddr},
			{ID: fdn.nodeID, Addr: fdn.localServerAddr},
			{ID: fdn.nodeID, Addr: fdn.localServerAddr},
		},
	}

	task := proto.NewAdminTask(proto.OpCreateDataPartition, localNodeAddress, req)
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
		return nil
	}
	p := &repl.Packet{
		Packet: proto.Packet{
			Magic:       proto.ProtoMagic,
			ReqID:       proto.GenerateRequestID(),
			Opcode:      proto.OpCreateDataPartition,
			PartitionID: partitionId,
			Data:        body,
			Size:        uint32(len(body)),
			StartT:      time.Now().UnixNano(),
		},
	}

	opCode, err, msg := fdn.operateHandle(t, p)
	if err != nil {
		t.Fatal(err)
		return
	}
	if opCode != proto.OpOk {
		t.Fatal(msg)
	}
	return fdn.space.Partition(partitionId)
}

func (fdn *fakeDataNode) fakeCreateExtent(dp *DataPartition, t *testing.T, extentId uint64) error {
	p := &repl.Packet{
		Packet: proto.Packet{
			Magic:       proto.ProtoMagic,
			ReqID:       proto.GenerateRequestID(),
			Opcode:      proto.OpCreateExtent,
			PartitionID: dp.ID(),
			StartT:      time.Now().UnixNano(),
			ExtentID:    extentId,
		},
	}

	opCode, err, msg := fdn.operateHandle(t, p)
	if err != nil {
		t.Fatal(err)
		return nil
	}
	if opCode != proto.OpOk && !strings.Contains(msg, "extent already exists") {
		return errors.NewErrorf("fakeCreateExtent fail %v", msg)
	}

	if p.ExtentID != extentId {
		return errors.NewErrorf("fakeCreateExtent fail, error not set ExtentId")
	}
	return nil
}

func (fdn *fakeDataNode) prepareTestData(t *testing.T, dp *DataPartition, extentId uint64) (crc uint32, err error) {
	size := 1 * unit.MB
	bytes := make([]byte, size)
	for i := 0; i < size; i++ {
		bytes[i] = 1
	}

	crc = crc32.ChecksumIEEE(bytes)
	p := &repl.Packet{
		Object: dp,
		Packet: proto.Packet{
			Magic:       proto.ProtoMagic,
			ReqID:       proto.GenerateRequestID(),
			Opcode:      proto.OpWrite,
			PartitionID: dp.ID(),
			ExtentID:    extentId,
			Size:        uint32(size),
			CRC:         crc,
			Data:        bytes,
			StartT:      time.Now().UnixNano(),
		},
	}

	opCode, err, msg := fdn.operateHandle(t, p)
	if err != nil {
		t.Fatal(err)
		return
	}
	if opCode != proto.OpOk {
		t.Fatal(msg)
	}
	return
}

func (fdn *fakeDataNode) operateHandle(t *testing.T, p *repl.Packet) (opCode uint8, err error, msg string) {
	err = fdn.Prepare(p, fdn.localServerAddr)
	if err != nil {
		t.Errorf("prepare err %v", err)
		return
	}

	conn, err := net.Dial("tcp", localNodeAddress)
	if err != nil {
		return
	}

	defer conn.Close()
	err = fdn.OperatePacket(p, conn.(*net.TCPConn))
	if err != nil {
		msg = fmt.Sprintf("%v", err)
	}

	err = fdn.Post(p)
	if err != nil {
		return
	}
	opCode = p.ResultCode
	return
}
