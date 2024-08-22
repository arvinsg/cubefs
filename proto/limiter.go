package proto

import math "math"

const (
	OpExtentRepairWrite_ = iota + 512
	OpFlushDelete_
	OpExtentRepairWriteToApplyTempFile_
	OpExtentRepairWriteByPolicy_
	OpExtentRepairReadToRollback_
	OpExtentRepairReadToComputeCrc_
	OpExtentReadToGetCrc_
	OpFetchDataPartitionView_
	OpFixIssueFragments_
	OpPlaybackTinyDelete_
)

const (
	RateLimit       = "rate limit"
	ConcurrentLimit = "concurrent limit"
)

func GetOpMsgExtend(opcode int) (m string) {
	if opcode <= math.MaxUint8 {
		return GetOpMsg(uint8(opcode))
	}

	switch opcode {
	case OpExtentRepairWrite_:
		m = "OpExtentRepairWrite_"
	case OpExtentRepairWriteToApplyTempFile_:
		m = "OpExtentRepairWriteToApplyTempFile_"
	case OpExtentRepairWriteByPolicy_:
		m = "OpExtentRepairWriteByPolicy_"
	case OpExtentRepairReadToRollback_:
		m = "OpExtentRepairReadToRollback_"
	case OpExtentRepairReadToComputeCrc_:
		m = "OpExtentRepairReadToComputeCrc_"
	case OpExtentReadToGetCrc_:
		m = "OpExtentReadToGetCrc_"
	case OpFlushDelete_:
		m = "OpFlushDelete_"
	case OpFetchDataPartitionView_:
		m = "OpFetchDataPartitionView_"
	case OpFixIssueFragments_:
		m = "OpFixIssueFragments_"
	case OpPlaybackTinyDelete_:
		m = "OpPlaybackTinyDelete_"
	}
	return m
}

func GetOpCodeExtend(m string) (opcode int) {
	switch m {
	case "OpExtentRepairWrite_":
		opcode = OpExtentRepairWrite_
	case "OpExtentRepairWriteToApplyTempFile_":
		opcode = OpExtentRepairWriteToApplyTempFile_
	case "OpExtentRepairWriteByPolicy_":
		opcode = OpExtentRepairWriteByPolicy_
	case "OpExtentRepairReadToRollback_":
		opcode = OpExtentRepairReadToRollback_
	case "OpExtentRepairReadToComputeCrc_":
		opcode = OpExtentRepairReadToComputeCrc_
	case "OpExtentReadToGetCrc_":
		opcode = OpExtentReadToGetCrc_
	case "OpFlushDelete_":
		opcode = OpFlushDelete_
	case "OpFetchDataPartitionView_":
		opcode = OpFetchDataPartitionView_
	case "OpFixIssueFragments_":
		opcode = OpFixIssueFragments_
	case "OpPlaybackTinyDelete_":
		opcode = OpPlaybackTinyDelete_
	}
	return opcode
}
