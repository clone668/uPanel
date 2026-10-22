package shares

// 配置写入用的“假程序”（P08.2 测试）。
//
// 只假装三件外部程序：testparm、smbcontrol、systemctl。文件系统**不**假装——
// 写入、fsync、rename、备份、回滚全都作用在 t.TempDir() 里的真实文件上。
// 这个不对称是刻意的：这几段代码的全部价值就在于“什么时候真的动了那个文件”，
// 而内存假文件系统恰好会把这件事变成不可断言。

import (
	"os"
	"strings"

	"upanel/internal/syscmd"
)

// sambaDefaults 是 testparm 对没写过的键给出的默认值。
//
// 真机验证过：`testparm --parameter-name="map to guest"` 在配置里根本没写这一行时
// 也会打印 Bad User——有效值包含默认值，这正是我们不能靠解析文件文本来判断的原因。
var sambaDefaults = map[string]string{
	"map to guest":           "Bad User",
	"server min protocol":    "SMB2_02",
	"usershare allow guests": "Yes",
}

// installConfigPrograms 装上三个假程序。
func (h *harness) installConfigPrograms() {
	h.runner.on("/usr/bin/systemctl", func(spec syscmd.Spec) (syscmd.Result, error) {
		switch h.serviceState {
		case ServiceStateInactive:
			return syscmd.Result{ExitCode: 3, Stdout: "inactive\ninactive\n"},
				&syscmd.ExitError{ExitCode: 3}
		case ServiceStateUnknown:
			return syscmd.Result{ExitCode: 0, Stdout: "garbage\n"}, nil
		default:
			return syscmd.Result{ExitCode: 0, Stdout: "active\nactive\n"}, nil
		}
	})
	h.runner.on("/usr/bin/smbcontrol", func(spec syscmd.Spec) (syscmd.Result, error) {
		h.reloads++
		return resOK("")
	})
	h.runner.on("/usr/bin/testparm", h.fakeTestparm)
	h.runner.on("/usr/bin/smbstatus", func(spec syscmd.Spec) (syscmd.Result, error) {
		return resOK(h.statusJSON)
	})
}

// fakeTestparm 模拟 testparm 在本模块里用到的三种调用：
//
//	testparm -s <file>                                    整份校验
//	testparm -s --parameter-name=K <file>                 全局项的有效值
//	testparm -s --section-name=S --parameter-name=K <file> 某段某键的有效值
//
// 它是真读文件的：托管文件换成什么内容，读回来的就是什么内容。
func (h *harness) fakeTestparm(spec syscmd.Spec) (syscmd.Result, error) {
	var section, param, file string
	for _, a := range spec.Args {
		switch {
		case a == "-s":
		case strings.HasPrefix(a, "--section-name="):
			section = strings.TrimPrefix(a, "--section-name=")
		case strings.HasPrefix(a, "--parameter-name="):
			param = strings.TrimPrefix(a, "--parameter-name=")
		default:
			file = a
		}
	}

	if section == "" && param == "" {
		reject := (h.testparmBroken && file == h.cfg.Main) ||
			(h.rejectCandidate && strings.HasSuffix(file, ".upanel-new"))
		if reject {
			return syscmd.Result{ExitCode: 1, Stderr: "Error loading services."},
				&syscmd.ExitError{ExitCode: 1}
		}
		// 成功标记走 stderr，与真机一致（TestparmVerdict 会去找它）。
		return syscmd.Result{
			ExitCode: 0,
			Stderr:   "Load smb config files from " + file + "\nLoaded services file OK.\n",
		}, nil
	}

	view := h.effectiveConfig()
	if section != "" {
		values, ok := view[strings.ToLower(section)]
		if !ok {
			return syscmd.Result{ExitCode: 1, Stderr: "section not found"},
				&syscmd.ExitError{ExitCode: 1}
		}
		if v, ok := values[strings.ToLower(param)]; ok {
			return resOK(v + "\n")
		}
		return resOK("\n")
	}
	if v, ok := view["global"][strings.ToLower(param)]; ok {
		return resOK(v + "\n")
	}
	if v, ok := sambaDefaults[strings.ToLower(param)]; ok {
		return resOK(v + "\n")
	}
	return resOK("\n")
}

// effectiveConfig 把主配置与托管配置合成一份“Samba 会读到什么”的视图。
//
// 顺序是 main 然后 managed：include 在末尾，后出现的同名键生效（真机验证过）。
func (h *harness) effectiveConfig() map[string]map[string]string {
	out := map[string]map[string]string{"global": {}}
	for _, name := range []string{h.cfg.Main, h.cfg.Managed} {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		section := ""
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
				continue
			}
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
				if out[section] == nil {
					out[section] = map[string]string{}
				}
				continue
			}
			key, value, ok := splitConfigLine(trimmed)
			if !ok || section == "" {
				continue
			}
			out[section][strings.ToLower(key)] = value
		}
	}
	return out
}

// readManaged 读托管文件（不存在时返回空串）。
func (h *harness) readManaged() string {
	data, err := os.ReadFile(h.cfg.Managed)
	if err != nil {
		return ""
	}
	return string(data)
}

// readMain 读主配置。
func (h *harness) readMain() string {
	data, err := os.ReadFile(h.cfg.Main)
	if err != nil {
		return ""
	}
	return string(data)
}
