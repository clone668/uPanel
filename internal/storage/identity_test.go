package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 身份可信度的用例都是合成的：真机夹具只覆盖“sysfs 读不到序列号、只有 by-id 链接”
// 这一种情况（DECISIONS Q85 的反例原则——真实样本进 tests/fixtures，
// 为了覆盖分支而造的样本留在测试里，并且名字里写清 synthetic）。

func TestIdentityPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		files      map[string]string
		byID       map[string]string
		wantConf   string
		wantPrefix string
	}{
		{
			name: "有 wwid 最可信",
			files: map[string]string{
				"dev/wwid":   "naa.5000c500a1b2c3d4",
				"dev/serial": "SYNTH-0001",
			},
			// wwid 优先于 serial：wwid 是盘自己报的全球唯一名，serial 可能重复。
			wantConf:   ConfidenceHigh,
			wantPrefix: "wwn:naa.5000c500a1b2c3d4",
		},
		{
			name:       "只有序列号也够 high",
			files:      map[string]string{"dev/serial": "SYNTH-0002"},
			wantConf:   ConfidenceHigh,
			wantPrefix: "serial:synth-0002",
		},
		{
			name:       "by-id 的 wwn- 链接 high",
			files:      map[string]string{},
			byID:       map[string]string{"sdx": "wwn-0x5000c500a1b2c3d4"},
			wantConf:   ConfidenceHigh,
			wantPrefix: "byid:wwn-0x5000c500a1b2c3d4",
		},
		{
			name:       "by-id 的 scsi-S（含 VPD 序列号）medium",
			files:      map[string]string{},
			byID:       map[string]string{"sdx": "scsi-SQEMU_QEMU_HARDDISK_SYNTH-3"},
			wantConf:   ConfidenceMedium,
			wantPrefix: "byid:scsi-SQEMU",
		},
		{
			name:       "by-id 的 ata- 也 medium",
			files:      map[string]string{},
			byID:       map[string]string{"sdx": "ata-SYNTH_DISK_SYNTH-4"},
			wantConf:   ConfidenceMedium,
			wantPrefix: "byid:ata-",
		},
		{
			name:       "只有 scsi-（型号+序号，没有序列号）low",
			files:      map[string]string{},
			byID:       map[string]string{"sdx": "scsi-0QEMU_QEMU_HARDDISK_SYNTH-5"},
			wantConf:   ConfidenceLow,
			wantPrefix: "byid:scsi-0QEMU",
		},
		{
			name: "什么都没有：型号+地址兜底，low",
			files: map[string]string{
				"dev/model":    "SYNTHETIC DISK",
				"dev/realpath": "devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0",
			},
			wantConf:   ConfidenceLow,
			wantPrefix: "weak:",
		},
		{
			name:       "连型号都没有：unknown",
			files:      map[string]string{},
			wantConf:   ConfidenceUnknown,
			wantPrefix: "name:sdx",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, content := range c.files {
				p := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			dev := &Device{ID: "sdx", Name: "sdx", Kind: KindDisk}
			if v, ok := readFile(filepath.Join(dir, "dev", "wwid")); ok {
				dev.WWN = &v
			}
			if v, ok := readFile(filepath.Join(dir, "dev", "serial")); ok {
				dev.Serial = &v
			}
			if v, ok := readFile(filepath.Join(dir, "dev", "model")); ok {
				dev.Model = &v
			}
			// 设备目录用临时目录模拟：identity 会去读 <dir>/device/realpath。
			if err := os.MkdirAll(filepath.Join(dir, "device"), 0o755); err != nil {
				t.Fatal(err)
			}
			if v, ok := readFile(filepath.Join(dir, "dev", "realpath")); ok {
				if err := os.WriteFile(filepath.Join(dir, "device", "realpath"), []byte(v+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			byID := c.byID
			if byID == nil {
				byID = map[string]string{}
			}
			token, conf := identityToken(dir, dev, byID)
			fp := fingerprintFor(dev.Kind, token)
			if conf != c.wantConf {
				t.Errorf("可信度 = %q，想要 %q", conf, c.wantConf)
			}
			if !strings.HasPrefix(token, c.wantPrefix) {
				t.Errorf("身份依据 = %q，想要以 %q 开头", token, c.wantPrefix)
			}
			if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+64 {
				t.Errorf("指纹格式不对: %q", fp)
			}
		})
	}
}

// TestFingerprintIgnoresCapacity 是“扩容不该换身份”的用例：
// 同一块盘扩容后，指纹必须不变；容量变化是保护与预检要关心的事，不是身份。
func TestFingerprintIgnoresCapacity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wwid"), []byte("naa.5000c500a1b2c3d4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}

	small := &Device{ID: "sdx", Name: "sdx", Kind: KindDisk, SizeBytes: 8 << 30}
	small.WWN = strPtr("naa.5000c500a1b2c3d4")
	big := &Device{ID: "sdx", Name: "sdx", Kind: KindDisk, SizeBytes: 40 << 30}
	big.WWN = strPtr("naa.5000c500a1b2c3d4")

	fpSmall := fingerprintOf(dir, small, byID)
	fpBig := fingerprintOf(dir, big, byID)
	if fpSmall != fpBig {
		t.Errorf("同一块盘扩容后指纹变了：%s → %s（容量不该进身份）", fpSmall, fpBig)
	}

	// 换了设备类型（同一名字）就要换身份：sdx 从 disk 变成 raid 是不同的东西。
	asRAID := &Device{ID: "sdx", Name: "sdx", Kind: KindRAID, SizeBytes: 8 << 30}
	asRAID.WWN = strPtr("naa.5000c500a1b2c3d4")
	fpRAID := fingerprintOf(dir, asRAID, byID)
	if fpRAID == fpSmall {
		t.Errorf("类型不同却共用同一指纹：%s", fpRAID)
	}
}

// fingerprintOf 只在测试里用：把 identityToken 与 fingerprintFor 串起来。
func fingerprintOf(dir string, dev *Device, byID map[string]string) string {
	token, _ := identityToken(dir, dev, byID)
	return fingerprintFor(dev.Kind, token)
}

// TestByIDLinkRankPreference 检查同一块盘有多个 by-id 链接时取信息量最大的那个，
// 而不是靠目录遍历顺序碰运气。
func TestByIDLinkRankPreference(t *testing.T) {
	if linkRank("wwn-0x5000") <= linkRank("scsi-SQEMU_X") {
		t.Errorf("wwn- 应比 scsi-S 排得高")
	}
	if linkRank("scsi-SQEMU_X") <= linkRank("scsi-0QEMU_X") {
		t.Errorf("scsi-S 应比 scsi- 排得高")
	}
	if linkRank("unknown-link") != 1 {
		t.Errorf("未知前缀的链接应得最低分")
	}
}
