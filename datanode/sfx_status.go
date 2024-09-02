package datanode

import (
	"github.com/cubefs/cubefs/util/log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

type sfxStatus struct {
	totalPhysicalCapability uint64
	freePhysicalCapability  uint64 //unit is sectorCount and the sector size is 512
	physicalUsageRatio      uint32
	compRatio               uint32
}

const (
	//nvme ioctl cmd
	NVME_IOCTL_ADMIN_CMD = 0xc0484e41 //NVME_IOCTL_ADMIN_CMD	_IOWR('N', 0x41, struct nvme_admin_cmd)
	//nvme admin opcode
	NVME_ADMIN_GET_LOG_PAGE = 0x02
	NVME_ADMIN_IDENTIFY     = 0x06
	//nvme logpage id
	NVME_LOG_EXTENDED_HEALTH_INFO = 0xc2 //SFX extended logid
	//VID
	PCI_VENDOR_ID_SFX = 0xcc53
)

/**
 * @brief GetDevCheckSfx get devName form file path and check if it is sfx csd
 *
 * @param path, file path
 * @param IsSfx, true:is on sfx csd; false:fail or not on sfx csd
 * @param devName, the sfx csd block dev name
 *
 */
func GetDevCheckSfx(path string) (isSfx bool, devName string) {
	isSfx = false
	dInfo, err := os.Stat(path)
	if err != nil {
		log.LogDebugf("get Disk %s stat fail err:%s\n", path, err.Error())
		return
	}
	dStat := dInfo.Sys().(*syscall.Stat_t)
	maj := dStat.Dev >> 8
	min := dStat.Dev & 0xFF
	b := make([]byte, 128)
	devLink := "/sys/dev/block/" + strconv.FormatUint(maj, 10) + ":" + strconv.FormatUint(min, 10)
	n, err := syscall.Readlink(devLink, b)
	if err != nil {
		log.LogErrorf("get devName fail err:%s\n", err.Error())
		return
	}
	devDir := string(b[0:n])
	n = strings.LastIndex(devDir, "/")
	devName = "/dev/" + devDir[n+1:]
	var idctl nvmeIdCtrl = nvmeIdCtrl{}
	var ioctlCmd nvmePassthruCmd = nvmePassthruCmd{}
	fd, err := syscall.Open(devName, syscall.O_RDWR, 0777)
	if err != nil {
		log.LogErrorf("%s device open failed %s\n", devName, err.Error())
		return
	}
	defer syscall.Close(fd)
	ioctlCmd.opcode = NVME_ADMIN_IDENTIFY
	ioctlCmd.nsid = 0
	ioctlCmd.addr = uint64(uintptr(unsafe.Pointer(&idctl)))
	ioctlCmd.dataLen = 4096
	ioctlCmd.cdw10 = 1
	ioctlCmd.cdw11 = 0
	err = nvmeAdminPassthru(fd, ioctlCmd)
	isSfx = err == nil && PCI_VENDOR_ID_SFX == idctl.vId
	return
}

/**
 * @brief GetCSDStatus get sfx status by devName
 *
 * @param devName, the sfx csd block dev name
 * @param dStatus.compRatio, full disk compression ratio (100%~800%)
 * @param dStatus.physicalUsageRatio, physical space usage ratio
 * @param dStatus.freePhysicalCapability, free physical space .Byte
 * @param dStatus.totalPhysicalCapability, total physical space .Byte
 *
 * @return nil success; err fail
 */
func GetSfxStatus(devName string) (dStatus sfxStatus, err error) {
	var dataLen uint32 = 128
	var numd uint32 = (dataLen >> 2) - 1
	var numdu uint16 = uint16(numd >> 16)
	var numdl uint16 = uint16(numd & 0xffff)
	var cdw10 uint32 = NVME_LOG_EXTENDED_HEALTH_INFO | (uint32(numdl) << 16)
	var extendHealth nvmeExtendedHealthInfo = nvmeExtendedHealthInfo{}
	var ioctlCmd nvmePassthruCmd = nvmePassthruCmd{}
	fd, err := syscall.Open(devName, syscall.O_RDWR, 0777)
	if err != nil {
		log.LogErrorf("device %s open failed %s\n", devName, err.Error())
		return
	}
	defer syscall.Close(fd)
	ioctlCmd.opcode = NVME_ADMIN_GET_LOG_PAGE
	ioctlCmd.nsid = 0
	ioctlCmd.addr = uint64(uintptr(unsafe.Pointer(&extendHealth)))
	ioctlCmd.dataLen = dataLen
	ioctlCmd.cdw10 = cdw10
	ioctlCmd.cdw11 = uint32(numdu)
	err = nvmeAdminPassthru(fd, ioctlCmd)
	if err != nil {
		log.LogErrorf("device %s get status failed %s\n", devName, err.Error())
		return
	}
	dStatus.compRatio = extendHealth.compRatio
	if dStatus.compRatio < 100 {
		dStatus.compRatio = 100
	} else if dStatus.compRatio > 800 {
		dStatus.compRatio = 800
	}
	dStatus.physicalUsageRatio = extendHealth.physicalUsageRatio
	//The unit of PhysicalCapability is the number of sectors, and the sector size is 512 bytes, converted to bytes
	dStatus.freePhysicalCapability = extendHealth.freePhysicalCapability * 512
	dStatus.totalPhysicalCapability = extendHealth.totalPhysicalCapability * 512
	return
}

/**
 * @brief GetCSDStatus get sfx status by devName
 *
 * @param devName, the sfx csd block dev name
 * @param dStatus.compRatio, full disk compression ratio (100%~800%)
 * @param dStatus.physicalUsageRatio, physical space usage ratio
 * @param dStatus.freePhysicalCapability, free physical space .Byte
 * @param dStatus.totalPhysicalCapability, total physical space .Byte
 *
 * @return nil success; err fail
 */
func CheckSfxSramErr(devName string) (sramErr bool, err error) {
	var (
		indexStart int
		nfe_err    uint64
	)
	sramErr = false
	indexStart = strings.Index(devName, "nvme")
	var file = path.Join(os.TempDir(), devName[indexStart:]+".raw")

	_ = os.Remove(file)

	cmd := exec.Command("sfx-nvme", "sfx", "evt-log-dump", devName, "--file", file, "--scanerr", "--length", "50")

	out, err := cmd.CombinedOutput()
	if err != nil {
		log.LogErrorf("dev:%s sfx dump evtlog fail,err: %s\n", devName, err.Error())
		return
	}

	lines := strings.Split(string(out), "\n")

	var regexpNum = regexp.MustCompile("0x(\\d)+")

	for _, line := range lines {
		indexStart = strings.Index(line, "nfe_hw_err_0")
		if indexStart >= 0 {
			sTemp := line[indexStart+13:]
			sTemp = strings.TrimSpace(sTemp)
			numStr := regexpNum.FindString(sTemp)
			if len(numStr) < 2 {
				continue
			}
			nfe_err, _ = strconv.ParseUint(numStr[2:], 16, 64)
			if (nfe_err & 0x200) != 0 {
				sramErr = true
				break
			}
			continue
		}
	}

	_ = os.Remove(file)
	return
}
