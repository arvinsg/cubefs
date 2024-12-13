package datanode

import (
	"encoding/json"
	"fmt"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/repl"
	"github.com/cubefs/cubefs/storage"
	"hash/crc32"
)

// prepare
func (s *DataNode) Prepare(p *repl.Packet, remoteAddr string) (err error) {
	defer func() {
		if err != nil {
			p.PackErrorBody(repl.ActionPreparePkt, err.Error())
		}
	}()
	if p.IsMasterCommand() {
		return
	}
	if err = s.checkReplInfo(p); err != nil {
		return
	}
	err = s.checkStoreMode(p)
	if err != nil {
		return
	}
	if err = s.checkCrc(p); err != nil {
		return
	}
	if err = s.checkPartition(p); err != nil {
		return
	}
	if err = s.checkLimit(p); err != nil {
		return
	}
	// For certain packet, we need to add some additional extent information.
	if err = s.addExtentInfo(p); err != nil {
		return
	}

	return
}

func (s *DataNode) checkReplInfo(p *repl.Packet) (err error) {
	if p.IsLeaderPacket() && len(p.GetFollowers()) == 0 {
		err = fmt.Errorf("checkReplInfo: leader write packet without follower address")
		return
	}
	return
}

func (s *DataNode) checkStoreMode(p *repl.Packet) (err error) {
	if p.ExtentType == proto.TinyExtentType || p.ExtentType == proto.NormalExtentType {
		return nil
	}
	return ErrIncorrectStoreType
}

func (s *DataNode) checkCrc(p *repl.Packet) (err error) {
	if !p.IsWriteOperation() {
		return
	}
	crc := crc32.ChecksumIEEE(p.Data[:p.Size])
	if crc != p.CRC {
		return storage.CrcMismatchError
	}

	return
}

func (s *DataNode) checkPartition(p *repl.Packet) (err error) {
	dp := s.space.Partition(p.PartitionID)
	if dp == nil {
		err = proto.ErrDataPartitionNotExists
		return
	}
	p.Object = dp
	if p.IsWriteOperation() || p.IsCreateExtentOperation() {
		if err = dp.CheckWritable(); err != nil {
			return
		}
	}
	return
}

func (s *DataNode) addExtentInfo(p *repl.Packet) error {
	partition := p.Object.(*DataPartition)
	store := p.Object.(*DataPartition).ExtentStore()
	var (
		extentID uint64
		err      error
	)

	if p.IsLeaderPacket() && p.IsTinyExtentType() && p.IsWriteOperation() {
		extentID, err = store.GetAvailableTinyExtent()
		if err != nil {
			return fmt.Errorf("addExtentInfo partition %v GetAvailableTinyExtent error %v", p.PartitionID, err.Error())
		}
		p.ExtentID = extentID
		p.ExtentOffset, err = store.GetTinyExtentOffset(extentID)
		if err != nil {
			return fmt.Errorf("addExtentInfo partition %v  %v GetTinyExtentOffset error %v", p.PartitionID, extentID, err.Error())
		}
	} else if p.IsLeaderPacket() && p.IsCreateExtentOperation() {
		if partition.GetExtentCount() >= storage.MaxExtentCount*3 {
			return fmt.Errorf("addExtentInfo partition %v has reached maxExtentId", p.PartitionID)
		}
		p.ExtentID, err = partition.AllocateExtentID()
		if err != nil {
			return fmt.Errorf("addExtentInfo partition %v alloc NextExtentId error %v", p.PartitionID, err)
		}
	} else if p.IsLeaderPacket() && p.IsMarkDeleteExtentOperation() && p.IsTinyExtentType() {
		record := new(proto.InodeExtentKey)
		if err := json.Unmarshal(p.Data[:p.Size], record); err != nil {
			return fmt.Errorf("addExtentInfo failed %v", err.Error())
		}
		p.Data, _ = json.Marshal(record)
		p.Size = uint32(len(p.Data))
		p.OrgBuffer = p.Data
	}
	if (p.IsCreateExtentOperation() || p.IsWriteOperation()) && p.ExtentID == 0 {
		return fmt.Errorf("addExtentInfo extentId is 0")
	}

	return nil
}
