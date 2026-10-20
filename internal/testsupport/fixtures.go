// Package testsupport 放测试专用的小工具。
//
// 只被 *_test.go 引用，因此不会进任何生产二进制；放在独立包里是为了让两个包
// （monitoring、agent）用同一套夹具查找逻辑，而不是各写一份相对路径。
package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxWalkDepth 是向上找源码树的最大层数。包目录最多四五层，8 层足够。
const maxWalkDepth = 8

// Fixture 读取仓库 tests/fixtures 下的夹具内容。
//
// 查找顺序：
//
//  1. 环境变量 UPANEL_FIXTURES 指名的目录（显式覆盖，优先级最高）；
//  2. 测试二进制旁边：`<bin>/fixtures`、`<bin>/../fixtures`、`<bin>/../../tests/fixtures`。
//     验收 VM 上二进制放在 /tmp/upanel-bin/tests，夹具放在 /tmp/upanel-bin/fixtures，
//     进程 cwd 是调用者的目录——只有这一步能在那种环境下找到夹具，
//     而 p02-3 这类脚本会**直接**执行测试二进制（不经过 run-native-tests.sh）；
//  3. 从当前工作目录向上逐层找 tests/fixtures（`go test ./...` 的 cwd 是包目录）。
//
// 找不到就报错并列出试过的所有路径。**不跳过、不返回空内容**：夹具缺失会让
// “解析真实 /proc 输出”的用例变成无意义的空跑，那比失败更糟。
func Fixture(name string) ([]byte, error) {
	var tried []string

	try := func(p string) ([]byte, bool) {
		raw, err := os.ReadFile(p)
		if err != nil {
			tried = append(tried, p)
			return nil, false
		}
		return raw, true
	}

	if root := os.Getenv("UPANEL_FIXTURES"); root != "" {
		if raw, ok := try(filepath.Join(root, name)); ok {
			return raw, nil
		}
	}

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, cand := range []string{
			filepath.Join(dir, "fixtures", name),
			filepath.Join(dir, "..", "fixtures", name),
			filepath.Join(dir, "..", "..", "tests", "fixtures", name),
		} {
			if raw, ok := try(cand); ok {
				return raw, nil
			}
		}
	}

	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("无法取得当前工作目录: %w", err)
	}
	for i := 0; i < maxWalkDepth; i++ {
		if raw, ok := try(filepath.Join(dir, "tests", "fixtures", name)); ok {
			return raw, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return nil, fmt.Errorf("读取夹具 %s 失败：试过 %s（可用 UPANEL_FIXTURES 指向 tests/fixtures）",
		name, strings.Join(tried, "、"))
}
