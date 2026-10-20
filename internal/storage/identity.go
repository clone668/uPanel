package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// deriveIdentity 计算设备的稳定身份指纹与可信度。
//
// 为什么要指纹：设备名（sda / dm-0）会随启动、插拔、枚举顺序变化，不能用来判断
// “还是不是同一块盘”。写动作的预检要把目标设备绑到一个指纹上，执行前再核一次
// （CONTRACTS.md 4.1 的“目标身份指纹”）。
func deriveIdentity(blockDir string, dev *Device, byID map[string]string) (fingerprint, confidence string) {
	token, confidence := identityToken(blockDir, dev, byID)
	return fingerprintFor(dev.Kind, token), confidence
}

// identityToken 返回身份依据（token）与它的可信度。
//
// 取值顺序（越靠前越可信）：
//
//  1. sysfs 的 wwid（真正的 WWN/EUI）——high；
//  2. sysfs 的 serial（硬盘序列号）——high；
//  3. /dev/disk/by-id 里的 wwn-* / nvme-*（含序列号）——high；
//  4. by-id 的 scsi-S* / ata-*（udev 从 VPD 读出的序列号）——medium；
//  5. by-id 的 scsi-*（没有序列号，只有 型号+序号）——low；
//  6. 型号 + sysfs 拓扑地址——low；连型号都没有——unknown。
//
// token 单独返回，是为了让测试能断言“依据到底是哪一条”——
// 只看指纹看不出它来自 wwid 还是 from name。
func identityToken(blockDir string, dev *Device, byID map[string]string) (token, confidence string) {
	confidence = ConfidenceUnknown

	switch {
	case dev.WWN != nil && *dev.WWN != "":
		token = "wwn:" + strings.ToLower(*dev.WWN)
		confidence = ConfidenceHigh
	case dev.Serial != nil && *dev.Serial != "":
		token = "serial:" + strings.ToLower(*dev.Serial)
		confidence = ConfidenceHigh
	}

	if token == "" {
		if link, ok := byID[dev.Name]; ok {
			switch {
			case strings.HasPrefix(link, "wwn-"):
				token = "byid:" + link
				confidence = ConfidenceHigh
			case strings.HasPrefix(link, "nvme-"):
				token = "byid:" + link
				confidence = ConfidenceHigh
			case strings.HasPrefix(link, "scsi-S"), strings.HasPrefix(link, "ata-"):
				token = "byid:" + link
				confidence = ConfidenceMedium
			case strings.HasPrefix(link, "scsi-"):
				token = "byid:" + link
				confidence = ConfidenceLow
			}
		}
	}

	if token == "" {
		// 最后兜底：型号 + 拓扑地址。能把同一台机器上两块同型号盘区分开，
		// 但换槽位就认不出来——可信度如实标成 low，P07 的预检据此可以要求更强的确认。
		model := ""
		if dev.Model != nil {
			model = strings.TrimSpace(*dev.Model)
		}
		addr := ""
		if s, ok := readFile(joinPath(blockDir, dev.Name, "device", "realpath")); ok {
			addr = s
		} else if resolved, err := resolveSymlink(joinPath(blockDir, dev.Name, "device")); err == nil {
			addr = resolved
		}
		if model != "" || addr != "" {
			token = "weak:" + strings.ToLower(model) + "|" + addr
			confidence = ConfidenceLow
		} else {
			// 连型号和地址都没有（某些虚拟设备）：只能承认不知道。
			token = "name:" + dev.Name
			confidence = ConfidenceUnknown
		}
	}
	return token, confidence
}

// fingerprintFor 把身份依据哈希成指纹。
//
// 指纹里**不放容量**：扩容（LVM 扩 LV、虚拟盘扩容）不该让“同一块盘”变成另一块盘。
// 容量另行比较，那属于保护与预检的判断，不属于身份。
// kind 参与哈希：同一个名字在 disk 与 raid 上代表不同的东西，不能共用身份。
func fingerprintFor(kind, token string) string {
	sum := sha256.Sum256([]byte("upanel-device-v1|" + kind + "|" + token))
	return "sha256:" + hex.EncodeToString(sum[:])
}
