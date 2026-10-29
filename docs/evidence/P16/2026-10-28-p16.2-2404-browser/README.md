# P16.2（补）：24.04 上的页面链路，跑在**最终交付字节**上（2026-10-28）

## 0. 这条补的是什么洞

P16.2 的验收口径里有一条是"从浏览器完成主要用户流程"（STAGES §18）。到这个目录出现之前，
这句话在两个平台上的证据是**不对齐**的：

| 链路 | 22.04 | 24.04（本目录出现之前） | 24.04（本目录） |
| --- | --- | --- | --- |
| 页面冒烟 `smoke.mjs`（35 项） | 有，最终字节 | 只有 2026-10-27 那批字节上的 35/0/0 | **35 / 0 / 0** |
| 部署写链路 `deploy-smoke.mjs`（43 项） | 有，最终字节 | **没有** | **43 / 0 / 0** |

所以到本目录为止，P16.2 的"双平台端到端"**两条链路都在同一份最终字节（`3f3b40c1…daf0`）上各有读数**。
做法：24.04 的系统盘从基底镜像重建，装的就是最终交付包，页面链路两条都在上面跑一遍。

**没有重打包。** 包还是 22.04 那轮跑过的同一个 tar.gz，sha256 逐字节相同——这是 Q510 的要求：
读数只对它自己被跑的那份字节有效，为了让读数好看去重打包，等于把两边的证据都作废。

## 1. 环境与字节

```
基底镜像   ubuntu-24.04-server-cloudimg-amd64.img
          sha256 04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94（与 images/SHA256SUMS 一致）
系统盘     rm -f system.qcow2 && qemu-img create -f qcow2 -F qcow2 -b ../images/ubuntu-24.04-server-cloudimg-amd64.img system.qcow2 40G
启动       launch-vm.ps1 -Name upanel-2404 -SshPort 2222 -SeedPort 8899 -Accel tcg -MemoryMB 4096 -Cpus 2
客机       Ubuntu 24.04.5 LTS / 6.8.0-147-generic / systemd 255 (255.4-1ubuntu8.18) / 2 vCPU / 3915 MB
交付包     dist/upanel-1.0.0-linux-amd64.tar.gz
          sha256 3f3b40c11247ea1b15aba024ddb3571fada3a69291465eb7e43242b2e9d6daf0（44154866 B）
VERSION    1.0.0 / 8e52b36-dirty / build_date 2026-10-28T05:23:36Z / amd64
```

装完之后的机器现场（`facts-vm.txt` 有完整输出）：`upanel-agent.service` 与 `upanel-web.service` 都是
`active`；面板只监听 `127.0.0.1:9443`；`upanel-agent` 跑在 root，`upanel-web` 跑在 `upanel-web` 用户下
（**web 不是 root**）；证书 `CN = upanel-2404`，有效期 2026-10-28 → 2029-01-30。

容器运行时是这一轮补装的（cloud 镜像上本来没有 docker）：`apt-get update` 之后
`apt-get install -y docker.io docker-compose-v2`。理由与 P15.4 那台 22.04 一致——写链路要动真容器，
而这台机器在装之前 `docker：absent`（`facts-vm.txt` 第一段就是这句）。

## 2. 读数

| 链路 | 脚本 | 结果 |
| --- | --- | --- |
| 页面冒烟 | `web/scripts/smoke.mjs` | **PASS 35 / FAIL 0 / SKIP 0（35 项）** |
| 部署写链路 | `web/scripts/deploy-smoke.mjs` | **PASS 43 / FAIL 0（43 项）**，见 §3（首轮 10 项后卡在预检，成因与重跑都在那一节） |
| 安装相位 | `tests/integration/p16-2-run-on-2204.sh`（`PHASES=install`） | **47 PASS / 0 FAIL / 0 NOT RUN**，见 `RESULT-install`/`phase-install.log` |

调用方式（口令只走环境变量，不进 argv）：

```
ssh -N -L 9443:127.0.0.1:9443 -i …/upanel-2404/id_ed25519 -p 2222 upanel@127.0.0.1 &
UPANEL_SMOKE_USER=admin UPANEL_SMOKE_PASS=… UPANEL_SMOKE_TIMEOUT_SCALE=4 \
  node web/scripts/smoke.mjs --base https://127.0.0.1:9443 --no-build
```

`UPANEL_SMOKE_TIMEOUT_SCALE=4` 与 2026-10-27 那一轮用的是同一个值：单线程 TCG 下登录一次要烧 10—20 秒
（已知边界 C10），4 倍只是把**等待**放大到这个环境撑得住，断言本身一个字没放宽——放大的等待里
不通过的项目照样是 FAIL。

验证过的旁证：整条链路跑完，`journalctl -u upanel-web -u upanel-agent` 里 `会话校验失败` 计数为 **0**
（`facts-vm.txt` 末段）。页面链路不是靠"登录被拒但页面还能点"混过去的。

## 3. 部署写链路（43 项）

第一次跑**红了，红在一个真问题上**——不是脚本的问题，是这台机器的顺序问题：

```
<== PASS  表单里只有闭集里的字段，没有 YAML 框也没有特权选项
<== PASS  本地校验对合法草稿不报错
（之后 8 分钟没有任何一行输出——预检一直没给出 allowed=true）
```

客机 journal 同时刻给出原因（原始行在 `journal-startup-gate.txt`）：

```
upanel-agent  WARN  拒绝未登记动作的预检  action=deployment.apply
upanel-web    WARN  请求被拒绝  POST /api/v1/operations/preflight  status=400
```

**成因**（`cmd/upanel-agent/main.go` 第 911—975 行）：三条部署写动作在代理**启动时**就决定了登记与否，
判据是 `firstExistingFile(deploy.DockerBinary)`。这台机器是先装面板（07:29，此时 `docker：absent`）
再装 docker（07:38），代理早就起好了 → 写动作没登记 → 预检按"未登记动作"拒掉。这是**有意的启动时门**，
代码注释写得很清楚（"缺任何一样就不登记：capabilities 于是如实说这台机器不能部署"），
`sh deploy/scripts/install.sh --install-deps docker` 或先装 docker 再装面板都不会踩到。

同一处代码里还有一个不一致，记在这里没修（Q516）：`hostCaps` 每 10 分钟重探一次、注释写着
"装完一个包不必重启代理"，而**写动作的登记不跟着重探**。于是"外部程序能力"页会先说 docker 有了，
部署页的新建表单也照常给得出来，点"获取预检"才撞上 400。修它要动二进制 → 重打包 → 按 Q510 两台 VM
的七相位与页面链路全部重跑，代价与收益不成比例，所以本轮只登记。

处理方式就是把顺序补正：`systemctl restart upanel-agent`（重启后日志出现 `已启用 Compose 部署`），
然后在**同一份字节、同一台机器**上重跑同一条链路：

```
<== PASS  预检给出结论并允许执行
…
PASS 43 / FAIL 0（共 43 项，目标 https://127.0.0.1:9443，部署名 upanel-smoke）
exit=0
```

第一次的整份日志留着（`deploy-smoke-run1-docker-after-install.log`），它是 Q516 的现场：
`<== FAIL  脚本异常：… 未登记的动作 "deployment.apply"`。**两次跑的是同一份包、同一台机器**，
差别只有"代理有没有重启过"。

顺带记下写链路的真机耗时（客机 journal，UTC）：部署那次 `07:48:00` 受理，`07:49:42` 才开始
`Container … Creating`（这 102 秒基本都花在拉 `nginx:1.27-alpine` 上），`07:49:59` 容器 `Started`；
换镜像那次 `07:52:09` Creating、`07:52:23` Started。这些数字只在单线程 TCG 下成立，不能当性能结论。

跑完之后机器是干净的（`facts-vm.txt` 末段）：`docker ps -a` 0 个容器、`docker compose ls` 0 个项目、
`/etc/upanel/deploy` 0 个文件、`upanel-deploy*` 0 个单元——删除那条链路没有留垃圾。

## 4. 没有证明什么

- **没有证明 arm64**。两份包里的 arm64 那份到现在没有任何真机读数。
- **没有证明裸机开机到可用**，也没有跑掉电语义——STAGES §18 不要求掉电演练，这一轮也没做。
- **没有证明 `go test -race`**。
- 这台机器是单线程 TCG 的 VM，不是云主机。性能类结论（安装耗时、登录耗时、拉镜像耗时）不能从这里往外推。
- **没有证明"面板装好之后再装 docker，部署能不经重启就可用"**——恰恰相反，这一轮证明它**不能**（Q516）。
  支持路径是装面板时点依赖（`--install-deps docker`）或装完 docker 重启代理。
- `smoke.mjs` 的 35 项是"页面能开、导航对、读到真数据"，它不检查写链路；写链路只有 §3 那一条。

## 5. 文件清单

| 文件 | 是什么 |
| --- | --- |
| `facts-vm.txt` | 机器现场：装包前的空机器、装完之后的面板、证书指纹、补装的 docker 版本、两条链路跑完之后的收尾读数（0 容器 / 0 项目 / 0 单元 / `会话校验失败` 0 / panic 0） |
| `facts/`、`logs/`、`run.log`、`phase-install.log`、`RESULT-install`、`RESULT-ALL` | 驱动留下的安装相位读数（47/0/0） |
| `smoke.log` | 页面冒烟 35 条逐条（35/0/0） |
| `deploy-smoke-run1-docker-after-install.log` | 写链路第一次：10 项后卡在预检，`exit=1`，Q516 的现场 |
| `deploy-smoke.log` | 写链路第二次（重启代理后）：43 条逐条（43/0/0）、`exit=0` |
| `journal-startup-gate.txt` | 两段原始 journal：① 07:30:05 代理启动时的 `WARN 部署动作未启用（只读现场仍然可用）` / `reason: 找不到 /usr/bin/docker`；② 07:39:22 的 `WARN 拒绝未登记动作的预检 action=deployment.apply` 与 `POST /api/v1/operations/preflight status=400` |
