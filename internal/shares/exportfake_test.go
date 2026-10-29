package shares

// 假的 exportfs（P14.4 测试）。
//
// 它维护一张“内核里现在有什么”的表，并且**真的读那个临时文件**：
//
//	exportfs -ra   按 exports 文件重算导出表（除非要求它假装没生效）
//	exportfs -v    把当前表按真机的形状打印出来
//
// 为什么值得单独写一个假程序而不是让 `-ra` 直接返回 0：这一半的全部价值就在
// “退出码 0 不等于生效了”。真机上撞得到的情形有两种，都必须能在单测里造出来——
// 刷新失败（rc=1），以及刷新“成功”但表里还是旧的（stale）。

import (
	"os"
	"sort"
	"strings"

	"upanel/internal/syscmd"
)

// fakeExportfs 是一台“NFS 服务端”的导出表。
type fakeExportfs struct {
	// table 是当前生效的导出表（路径 → 客户端条目）。
	table ExportTable
	// foreign 是“别人的导出”（/etc/exports 或别的 *.exports 里的）：面板刷表
	// 不会让它们消失，所以每次重算都从这一份开始。
	foreign ExportTable
	// reloadCalls 记 `-ra` 被调了几次（幂等重放不该再刷）。
	reloadCalls int
	// viewCalls 记 `-v` 被调了几次。
	viewCalls int
	// reloadErr 非空时 `-ra` 以 1 退出并打印它（模拟 duplicated export entries）。
	reloadErr string
	// stale 让 `-ra` 返回 0 但**不**更新表：模拟“文件对了、内核没吃到”。
	stale bool
	// missingDirs 里的路径会被登记但让 `-ra` 以 1 退出并打印 Failed to stat。
	//
	// 这是真机行为里最反直觉的一条（VM 上实测过）：目录不存在时导出**仍然被注册**，
	// 只是退出码非零。面板的处置是“退出码非零即当作失败并还原”，所以这条必须能造出来。
	missingDirs map[string]bool
	// path 是托管导出文件的落点（install 时填）。
	path string
}

// install 把两个子命令挂到 runner 上。
//
// path 是托管导出文件在临时目录里的落点——假程序读它，所以“写了什么”与
// “生效了什么”之间那条界线是真实存在的，而不是被测试抹平。
func (f *fakeExportfs) install(r *fakeRunner, path string) {
	f.table = ExportTable{}
	f.foreign = ExportTable{}
	f.missingDirs = map[string]bool{}
	f.path = path
	r.on("/usr/sbin/exportfs", func(spec syscmd.Spec) (syscmd.Result, error) {
		if len(spec.Args) == 0 {
			return resFail(1, "exportfs: 没有参数")
		}
		switch spec.Args[0] {
		case "-ra":
			f.reloadCalls++
			if f.reloadErr != "" {
				return resFail(1, f.reloadErr)
			}
			missing := f.reload()
			if missing != "" {
				return resFail(1, "exportfs: Failed to stat "+missing+": No such file or directory")
			}
			return resOK("")
		case "-v":
			f.viewCalls++
			return resOK(f.view())
		}
		return resFail(1, "exportfs: 不认识的参数 "+strings.Join(spec.Args, " "))
	})
}

// reload 从托管导出文件重算导出表，返回第一条“目录不存在”的路径（没有则空串）。
func (f *fakeExportfs) reload() string {
	entries, _ := ParseExports(f.mustRead())
	next := ExportTable{}
	for p, clients := range f.foreign {
		next[p] = clients
	}
	var missing string
	for _, en := range entries {
		if f.missingDirs[en.Path] && missing == "" {
			missing = en.Path
		}
		clients := make([]ExportClient, 0, len(en.Specs))
		for _, spec := range en.Specs {
			client, opts, ok := splitClientSpec(spec)
			if !ok {
				continue
			}
			clients = append(clients, ExportClient{Client: client, Options: expandOptions(opts)})
		}
		next[en.Path] = clients
	}
	if !f.stale {
		f.table = next
	}
	return missing
}

// mustRead 读托管导出文件（不存在时当作空文件）。
func (f *fakeExportfs) mustRead() []byte {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil
	}
	return data
}

// expandOptions 把导出行里的选项展开成真机 `exportfs -v` 的样子。
//
// 真机输出会补上一堆默认项（wdelay、hide、secure、no_all_squash）。这些不是装饰：
// VerifyExport 判的是“实际有哪些选项”，不把默认项写出来，测试就看不到
// no_root_squash 与 async 有没有被别的东西抵掉。
func expandOptions(options string) string {
	has := map[string]bool{}
	for _, tok := range strings.Split(options, ",") {
		has[strings.TrimSpace(tok)] = true
	}
	out := make([]string, 0, 12)
	if has[ExportOptRO] {
		out = append(out, ExportOptRO)
	} else {
		out = append(out, ExportOptRW)
	}
	out = append(out, "wdelay", "hide", ExportOptNoSubtree, "sec=sys")
	if has["async"] {
		out = append(out, "async")
	} else {
		out = append(out, ExportOptSync)
	}
	out = append(out, "secure")
	if has["no_root_squash"] {
		out = append(out, "no_root_squash")
	} else {
		out = append(out, ExportOptRootSquash)
	}
	out = append(out, "no_all_squash")
	return strings.Join(out, ",")
}

// view 按真机格式打印导出表：路径一行，缩进的客户端行跟在后面。
func (f *fakeExportfs) view() string {
	var b strings.Builder
	paths := make([]string, 0, len(f.table))
	for p := range f.table {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		b.WriteString(p + "\n")
		clients := append([]ExportClient(nil), f.table[p]...)
		sort.Slice(clients, func(i, j int) bool { return clients[i].Client < clients[j].Client })
		for _, c := range clients {
			b.WriteString("\t\t" + c.Client + "(" + c.Options + ")\n")
		}
	}
	return b.String()
}
