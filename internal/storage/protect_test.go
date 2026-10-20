package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 系统盘保护的**链式**用例都是合成的：两台测试机上没有 LVM、没有软 RAID、
// 也没有 LUKS（只有直接分区盘），真机夹具覆盖不到这些形状。
//
// 这里手写最小 sysfs 树（只在内存/t.TempDir 里），明确标为 synthetic，
// 不混进 tests/fixtures 的真实采集目录里——那会让人以为真机长这样（DECISIONS Q85）。

// writeSysfs 按 map[相对路径]内容 造一棵 sysfs 树。
func writeSysfs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录 %s 失败: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", p, err)
		}
	}
	return dir
}

func TestProtectionThroughLVM(t *testing.T) {
	// 形状：sda（盘）→ sda2（LVM PV，type 8e）→ dm-0（LVM LV，挂着 /）。
	// 结论必须是：dm-0、sda2、sda 全部受保护。
	// 这正是“把 PV 盘当成数据盘格式化”会毁掉系统的那种配置。
	dir := writeSysfs(t, map[string]string{
		"sys/block/sda/dev":                        "8:0\n",
		"sys/block/sda/size":                       "1953525168\n",
		"sys/block/sda/queue/logical_block_size":   "512\n",
		"sys/block/sda/queue/physical_block_size":  "4096\n",
		"sys/block/sda/queue/rotational":           "1\n",
		"sys/block/sda/ro":                         "0\n",
		"sys/block/sda/removable":                  "0\n",
		"sys/block/sda/device/model":               "SYNTHETIC DISK\n",
		"sys/block/sda/device/serial":              "SYNTH-SDA-0001\n",
		"sys/block/sda/device/realpath":            "devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0\n",
		"sys/block/sda/holders/list":               "dm-0\n",
		"sys/block/sda/sda1/dev":                   "8:1\n",
		"sys/block/sda/sda1/size":                  "2048\n",
		"sys/block/sda/sda1/start":                 "2048\n",
		"sys/block/sda/sda1/partition":             "1\n",
		"sys/block/sda/sda1/ro":                    "0\n",
		"sys/block/sda/sda2/dev":                   "8:2\n",
		"sys/block/sda/sda2/size":                  "1953523119\n",
		"sys/block/sda/sda2/start":                 "4096\n",
		"sys/block/sda/sda2/partition":             "2\n",
		"sys/block/sda/sda2/ro":                    "0\n",
		"sys/block/sda/sda2/holders/list":          "dm-0\n",
		"sys/block/dm-0/dev":                       "253:0\n",
		"sys/block/dm-0/size":                      "1953520000\n",
		"sys/block/dm-0/ro":                        "0\n",
		"sys/block/dm-0/removable":                 "0\n",
		"sys/block/dm-0/dm/name":                   "vg0-root\n",
		"sys/block/dm-0/dm/uuid":                   "LVM-abcdefghijklmnopqrstuvwxyz0123\n",
		"sys/block/dm-0/queue/logical_block_size":  "512\n",
		"sys/block/dm-0/queue/physical_block_size": "4096\n",
		"sys/block/dm-0/queue/rotational":          "1\n",
		"sys/block/dm-0/slaves/list":               "sda2\n",
		"proc/self/mountinfo": "36 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/vg0-root rw\n" +
			"37 36 8:1 / /boot/efi rw,relatime shared:2 - vfat /dev/sda1 rw\n",
		"proc/swaps": "Filename\tType\tSize\tUsed\tPriority\n",
	})

	topo, err := Scan(t.Context(), Root{Sys: filepath.Join(dir, "sys"), Proc: filepath.Join(dir, "proc"), Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}

	lv := deviceByID(t, topo, "dm-0")
	if lv.Kind != KindLVM {
		t.Errorf("dm-0: Kind = %q，dm-uuid 以 LVM- 开头应为 %q", lv.Kind, KindLVM)
	}
	for _, id := range []string{"dm-0", "sda2", "sda"} {
		dev := deviceByID(t, topo, id)
		if !dev.Protection.SystemDisk {
			t.Errorf("%s: 没被标成系统盘（链：dm-0 承载 /，PV 是 sda2，整盘 sda）；理由 %v",
				id, dev.Protection.Reasons)
		}
		if len(dev.Protection.Reasons) == 0 {
			t.Errorf("%s: 受保护但没有理由", id)
		}
	}
	if has := hasEdge(topo, "dm-0", "sda2"); !has {
		t.Errorf("缺少 depends_on 边 dm-0->sda2")
	}
	if has := hasEdge(topo, "sda2", "sda"); !has {
		t.Errorf("缺少 partition_of 边 sda2->sda")
	}
	// sda1 只挂 /boot/efi，也应受保护；
	if !deviceByID(t, topo, "sda1").Protection.SystemDisk {
		t.Errorf("sda1 挂着 /boot/efi，应受保护")
	}
}

func TestProtectionThroughRAID(t *testing.T) {
	// 形状：两块盘 sdb/sdc 各一个分区组成 md0，md0 上跑 LUKS（dm-0），dm-0 挂着 /。
	// 这是最深的链条：# 根 → dm-0(crypt) → md0(raid) → sdb1/sdc1 → sdb/sdc。
	dir := writeSysfs(t, map[string]string{
		"sys/block/sdb/dev":                       "8:16\n",
		"sys/block/sdb/size":                      "2097152\n",
		"sys/block/sdb/ro":                        "0\n",
		"sys/block/sdb/removable":                 "0\n",
		"sys/block/sdb/queue/logical_block_size":  "512\n",
		"sys/block/sdb/device/serial":             "SYNTH-SDB\n",
		"sys/block/sdb/holders/list":              "md0\n",
		"sys/block/sdb/sdb1/dev":                  "8:17\n",
		"sys/block/sdb/sdb1/size":                 "2095104\n",
		"sys/block/sdb/sdb1/start":                "2048\n",
		"sys/block/sdb/sdb1/partition":            "1\n",
		"sys/block/sdb/sdb1/ro":                   "0\n",
		"sys/block/sdc/dev":                       "8:32\n",
		"sys/block/sdc/size":                      "2097152\n",
		"sys/block/sdc/ro":                        "0\n",
		"sys/block/sdc/removable":                 "0\n",
		"sys/block/sdc/queue/logical_block_size":  "512\n",
		"sys/block/sdc/device/serial":             "SYNTH-SDC\n",
		"sys/block/sdc/holders/list":              "md0\n",
		"sys/block/md0/dev":                       "9:0\n",
		"sys/block/md0/size":                      "2095104\n",
		"sys/block/md0/ro":                        "0\n",
		"sys/block/md0/removable":                 "0\n",
		"sys/block/md0/queue/logical_block_size":  "512\n",
		"sys/block/md0/md/level":                  "raid1\n",
		"sys/block/md0/md/present":                "present\n",
		"sys/block/md0/slaves/list":               "sdb1\nsdc1\n",
		"sys/block/md0/holders/list":              "dm-0\n",
		"sys/block/dm-0/dev":                      "253:0\n",
		"sys/block/dm-0/size":                     "2095104\n",
		"sys/block/dm-0/ro":                       "0\n",
		"sys/block/dm-0/removable":                "0\n",
		"sys/block/dm-0/dm/name":                  "cryptroot\n",
		"sys/block/dm-0/dm/uuid":                  "CRYPT-LUKS2-0123456789abcdef0123456789abcdef-cryptroot\n",
		"sys/block/dm-0/queue/logical_block_size": "512\n",
		"sys/block/dm-0/slaves/list":              "md0\n",
		"proc/self/mountinfo":                     "36 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/cryptroot rw\n",
		"proc/swaps":                              "Filename\tType\tSize\tUsed\tPriority\n",
	})

	topo, err := Scan(t.Context(), Root{Sys: filepath.Join(dir, "sys"), Proc: filepath.Join(dir, "proc"), Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}

	if k := deviceByID(t, topo, "md0").Kind; k != KindRAID {
		t.Errorf("md0: Kind = %q，想要 %q", k, KindRAID)
	}
	if k := deviceByID(t, topo, "dm-0").Kind; k != KindCrypt {
		t.Errorf("dm-0: Kind = %q，dm-uuid 以 CRYPT- 开头应为 %q", k, KindCrypt)
	}
	chain := []string{"dm-0", "md0", "sdb1", "sdb", "sdc"}
	for _, id := range chain {
		if !deviceByID(t, topo, id).Protection.SystemDisk {
			t.Errorf("%s: 没被标成系统盘；这条链是 / → dm-0 → md0 → sdb1/sdc1 → 整盘", id)
		}
	}
	// md1 不存在，但 sdc 的 sdc1 没被创建——上溯只走真实存在的边，
	// sdc 受保护是因为 md0 的 slaves 里有 sdc（这里没有 sdc1，边来自 holders）。
	if has := hasEdge(topo, "md0", "sdb1"); !has {
		t.Errorf("缺少 depends_on 边 md0->sdb1")
	}
}

func TestSwapDeviceAndSwapFileProtection(t *testing.T) {
	// 交换区两种形态：交换分区（sdb2）与交换文件（落在 sdc1 上的 /swapfile）。
	// 两者都必须让宿主设备受保护——把 swap 盘格式化会让服务器立刻失稳。
	dir := writeSysfs(t, map[string]string{
		"sys/block/sda/dev":                      "8:0\n",
		"sys/block/sda/size":                     "2097152\n",
		"sys/block/sda/ro":                       "0\n",
		"sys/block/sda/removable":                "0\n",
		"sys/block/sda/queue/logical_block_size": "512\n",
		"sys/block/sda/sda1/dev":                 "8:1\n",
		"sys/block/sda/sda1/size":                "1048576\n",
		"sys/block/sda/sda1/start":               "2048\n",
		"sys/block/sda/sda1/partition":           "1\n",
		"sys/block/sda/sda1/ro":                  "0\n",
		"sys/block/sdb/dev":                      "8:16\n",
		"sys/block/sdb/size":                     "2097152\n",
		"sys/block/sdb/ro":                       "0\n",
		"sys/block/sdb/removable":                "0\n",
		"sys/block/sdb/queue/logical_block_size": "512\n",
		"sys/block/sdb/sdb1/dev":                 "8:17\n",
		"sys/block/sdb/sdb1/size":                "1048576\n",
		"sys/block/sdb/sdb1/start":               "2048\n",
		"sys/block/sdb/sdb1/partition":           "1\n",
		"sys/block/sdb/sdb1/ro":                  "0\n",
		"sys/block/sdb/sdb2/dev":                 "8:18\n",
		"sys/block/sdb/sdb2/size":                "1046528\n",
		"sys/block/sdb/sdb2/start":               "1050624\n",
		"sys/block/sdb/sdb2/partition":           "2\n",
		"sys/block/sdb/sdb2/ro":                  "0\n",
		"proc/self/mountinfo":                    "36 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n",
		// 交换分区直接按 major:minor 对上；交换文件按路径落在 /data 上。
		"proc/swaps": "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n" +
			"/dev/sdb2                               partition\t1046524\t\t0\t\t-2\n" +
			"/data/swapfile                          file\t\t2097148\t\t0\t\t-3\n",
	})

	topo, err := Scan(t.Context(), Root{Sys: filepath.Join(dir, "sys"), Proc: filepath.Join(dir, "proc"), Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}

	swap := deviceByID(t, topo, "sdb2")
	if swap.Swap == nil {
		t.Fatalf("sdb2 是活动交换区，Swap 却是 nil")
	}
	if swap.Swap.SizeBytes == 0 || swap.Swap.Type != "partition" {
		t.Errorf("sdb2 交换区 = %+v", swap.Swap)
	}
	if !swap.Protection.SystemDisk {
		t.Errorf("sdb2 是活动交换区，必须受保护")
	}
	if !deviceByID(t, topo, "sdb").Protection.SystemDisk {
		t.Errorf("sdb 上放着交换分区，整盘必须受保护")
	}
	if deviceByID(t, topo, "sdb1").Protection.SystemDisk {
		t.Errorf("sdb1 既没挂载也不是交换区，不该受保护")
	}
}

func TestMountForPathBoundaries(t *testing.T) {
	mounts := []mountRecord{
		{path: "/"},
		{path: "/boot"},
		{path: "/boot/efi"},
		{path: "/var/lib/upanel"},
		{path: "/data"},
	}
	cases := []struct{ path, want string }{
		{"/", "/"},
		{"/etc/hosts", "/"},
		{"/boot", "/boot"},
		{"/boot/vmlinuz", "/boot"},
		{"/boot/efi/EFI", "/boot/efi"},
		{"/var/lib/upanel/db.sqlite", "/var/lib/upanel"},
		{"/var/log", "/"},
		{"/database-not-data", "/"}, // 前缀必须按目录边界，不能把 /data 配上
		{"/data/x", "/data"},
	}
	for _, c := range cases {
		got, ok := mountForPath(mounts, c.path)
		if !ok {
			t.Errorf("%s: 没找到挂载点", c.path)
			continue
		}
		if got != c.want {
			t.Errorf("%s: 挂载点 = %q，想要 %q", c.path, got, c.want)
		}
	}
}

func TestMissingRootMountIsNotGuessed(t *testing.T) {
	// 挂载表里根本没有 / 的时候（例如只采到了半个 mountinfo），
	// 不能靠“sda 看起来像系统盘”把它标成受保护：宁可漏报，不可假报。
	dir := writeSysfs(t, map[string]string{
		"sys/block/sda/dev":                      "8:0\n",
		"sys/block/sda/size":                     "2097152\n",
		"sys/block/sda/ro":                       "0\n",
		"sys/block/sda/removable":                "0\n",
		"sys/block/sda/queue/logical_block_size": "512\n",
		"proc/self/mountinfo":                    "36 1 0:20 / /proc rw,nosuid - proc proc rw\n",
		"proc/swaps":                             "Filename\tType\tSize\tUsed\tPriority\n",
	})
	topo, err := Scan(t.Context(), Root{Sys: filepath.Join(dir, "sys"), Proc: filepath.Join(dir, "proc"), Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if deviceByID(t, topo, "sda").Protection.SystemDisk {
		t.Errorf("没有任何挂载证据却把 sda 标成系统盘：这是按名字猜")
	}
}
