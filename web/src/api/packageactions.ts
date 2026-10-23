// 软件包写动作的前端约定（P11.2）：package.refresh / install / remove / upgrade。
//
// 这里**没有**请求函数。写操作在前端只有一条路——`createPreflight` + `submitJob`
// （见 api/preflights.ts），任何"专为某个动作开的快捷通道"都等于绕过预检、再认证
// 与审计。所以本模块只放三样东西：
//
//   1) 冻结的动作名与参数形状：`{"packages": [...]}`（CONTRACTS.md 第 5 节）；
//   2) 包名的**前端形状校验**：只是为了在输入框旁边立刻说"这不像个包名"，
//      真正的校验在代理侧（`internal/agent/packageaction.go`），前端放行的东西
//      代理仍然会拒；
//   3) 准入门槛的静态表：让按钮旁边能提前写一句"这一步要密码/要短语"，
//      而不是点下去才知道。真正的结论永远来自预检的 `requires_reauth` 与
//      `confirmation_phrase`。
//
// 一条来自 P11.2 的设计约束：**卸载一律要确认短语**，与要删几个包无关。
// 因为策略是在预检之前算的——那时还没跑 `apt-get -s`，不知道会不会连带删掉
// 26 个包。所以前端不能因为"只是删一个小工具"就少要一次确认。

export const PACKAGE_ACTIONS = [
  'package.refresh',
  'package.install',
  'package.upgrade',
  'package.remove',
] as const

export type PackageAction = (typeof PACKAGE_ACTIONS)[number]

/** 动作 → 界面上的一句话。 */
export const PACKAGE_ACTION_LABELS: Record<PackageAction, string> = {
  'package.refresh': '刷新软件源索引',
  'package.install': '安装软件包',
  'package.upgrade': '升级软件包',
  'package.remove': '卸载软件包',
}

/** 动作 → 确认按钮上的词（与 P05.1 的「确认启动」同一种写法）。 */
export const PACKAGE_ACTION_VERBS: Record<PackageAction, string> = {
  'package.refresh': '刷新',
  'package.install': '安装',
  'package.upgrade': '升级',
  'package.remove': '卸载',
}

/** 一次写事务最多点名多少个包（与代理的 MaxPackageNames 一致）。 */
export const MAX_PACKAGE_NAMES = 200

/**
 * 包名的形状（Debian 二进制包名 + 可选的多架构限定）。
 *
 * 与代理侧 `packageNamePattern` 逐字对应。前端这条只为了即时反馈：
 * 少一个字符、多一个空格、写成 `bash=5.2` 都能立刻看见，而不是等一次往返。
 */
const PACKAGE_NAME_PATTERN = /^[a-z0-9][a-z0-9+.-]+(:[a-z0-9][a-z0-9-]*)?$/

/** 一个包名是否合法。空串、首尾空白、大小写、通配符、版本限定都不合法。 */
export function isValidPackageName(name: string): boolean {
  return PACKAGE_NAME_PATTERN.test(name)
}

/**
 * 把输入框里的一串文字拆成包名列表。
 *
 * 只按逗号与空白切分：不猜 `--flag`、不猜 `name=version`，那些都是 apt-get 的
 * 选项/版本语法，而面板不提供这些语义（要让它们进来，等于把 argv 交给调用方）。
 * 空项被丢掉；重复项保留**首次出现**的位置（顺序会显示在确认框里）。
 */
export function parsePackageNames(text: string): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  for (const raw of text.split(/[\s,]+/)) {
    const name = raw.trim()
    if (name === '' || seen.has(name)) continue
    seen.add(name)
    out.push(name)
  }
  return out
}

/** 解析结果里不合法的那些包名（用来在输入框下面点名，而不是笼统说"格式错"）。 */
export function invalidPackageNames(names: string[]): string[] {
  return names.filter((name) => !isValidPackageName(name))
}

/**
 * 拼出写动作的参数。
 *
 * refresh 的参数必须是 `{}`，其余是 `{packages: [...]}`——这个差别要写在一处，
 * 否则某天有人给 refresh 也塞上 packages，代理会按未知字段拒绝，而报错点离原因很远。
 */
export function packageActionParams(action: PackageAction, names: string[]): Record<string, unknown> {
  if (action === 'package.refresh') return {}
  return { packages: [...names] }
}

/** 这个动作在提交时会不会要求再认证（界面提前提示用，真正的拦截在服务端）。 */
export function packageActionNeedsReauth(action: PackageAction): boolean {
  return action !== 'package.refresh'
}

/**
 * 这个动作会不会要求确认短语。
 *
 * 只有卸载要——原因写在文件顶部：策略在模拟之前就定了，那时还不知道会连带删几个包。
 */
export function packageActionNeedsPhrase(action: PackageAction): boolean {
  return action === 'package.remove'
}

/**
 * 为什么这个动作要短语（给人看的一句话；不需要短语时返回 null）。
 *
 * 它**不是**本次操作的影响描述：连带删除的清单来自预检的 warnings，
 * 这里说的是"为什么这一类操作要停顿一下"。
 */
export function packageActionPhraseReason(action: PackageAction): string | null {
  if (!packageActionNeedsPhrase(action)) return null
  return '卸载会连带删除依赖它的包，而且当前会话不会立刻断开——等你发现时可能已经连不上了。'
}
