package accounts

// 阻塞码与告警码的**闭集**（P14.5）。
//
// 与共享那边同一个约定：码是给程序与页面用的（它可以被判断、被翻译、被当成测试
// 断言的锚点），消息是给人读的。新增一个码意味着新增一类“预检可以说出来的理由”，
// 必须同时出现在 CONTRACTS.md 第 13 节与 docs/DECISIONS.md 里。
const (
	// NoticeAccountNameInvalid 表示账号名不符合形状要求。
	NoticeAccountNameInvalid = "ACCOUNT_NAME_INVALID"
	// NoticeAccountReserved 表示名字是系统/面板的保留名。
	//
	// 与 NameInvalid 分开：前者是“写法不对”，后者是“这个账号本来就不归面板管”。
	// 页面上这两句话的下一步动作完全不同（改名 vs 别碰它）。
	NoticeAccountReserved = "ACCOUNT_RESERVED"
	// NoticeAccountTypeInvalid 表示 type 不在闭集里。
	NoticeAccountTypeInvalid = "ACCOUNT_TYPE_INVALID"
	// NoticeAccountShellProfileInvalid 表示 shell_profile 不在闭集里，或与 type 不相容。
	NoticeAccountShellProfileInvalid = "ACCOUNT_SHELL_PROFILE_INVALID"
	// NoticeAccountDisplayNameInvalid 表示展示名不合法（控制字符、超长、空）。
	NoticeAccountDisplayNameInvalid = "ACCOUNT_DISPLAY_NAME_INVALID"
	// NoticeAccountSecretInvalid 表示秘密引用形状不对。
	NoticeAccountSecretInvalid = "ACCOUNT_SECRET_INVALID"
	// NoticeAccountNameTaken 表示系统里已经有同名账号。
	NoticeAccountNameTaken = "ACCOUNT_NAME_TAKEN"
	// NoticeAccountNotManaged 表示同名账号存在，但它不是面板创建的。
	//
	// 这是“绝不静默接管既有账号”那条不变量的可见落点：系统上已经有 alice，
	// 面板既不能把它当成自己的、也不能改它（PLAN.md 第 19 节：已有系统账号只读）。
	NoticeAccountNotManaged = "ACCOUNT_NOT_MANAGED"
	// NoticeAccountMissing 表示记录里引用的账号已经不在系统上了。
	NoticeAccountMissing = "ACCOUNT_MISSING"
	// NoticeAccountUIDOutOfRange 表示读回来的 uid 不在普通账号区间里。
	NoticeAccountUIDOutOfRange = "ACCOUNT_UID_OUT_OF_RANGE"
	// NoticeAccountHasShares 表示还有共享定义引用这个账号，不能删。
	NoticeAccountHasShares = "ACCOUNT_HAS_SHARES"
	// NoticeAccountLookupTimeout 是告警：预检没能在预算内问出系统现状。
	NoticeAccountLookupTimeout = "ACCOUNT_LOOKUP_TIMEOUT"
	// NoticeAccountNoPassword 是告警：这个账号没有口令（shadow 里是 "!"）。
	//
	// 它是告警而不是阻塞：账号本身合法、也能建出来。但 Ubuntu 的 sshd 走 PAM，
	// 而 PAM 的 account 阶段会拒绝一个口令被锁定的账号——配了公钥也登不上。
	// 这句话必须说出来，否则人会以为“加了公钥就能用了”，然后在 SSH 客户端那边
	// 看到一条 Permission denied 再回来猜。
	NoticeAccountNoPassword = "ACCOUNT_NO_PASSWORD"

	// NoticeSSHKeyInvalid 表示公钥格式不合法。
	NoticeSSHKeyInvalid = "SSH_KEY_INVALID"
	// NoticeSSHKeyDuplicate 表示这把公钥已经在这个账号上。
	NoticeSSHKeyDuplicate = "SSH_KEY_DUPLICATE"
	// NoticeSSHKeyBlockDamaged 表示 authorized_keys 里的托管区标记坏了。
	//
	// 标记只有一半（有开始没结束，或顺序颠倒）时**不猜**：既不敢整份重写（会
	// 吃掉标记外面的行），也不敢在那半截里插一把钥匙（会插进别人写的块里）。
	NoticeSSHKeyBlockDamaged = "SSH_KEY_BLOCK_DAMAGED"
	// NoticeSSHKeyNotFound 表示要删的公钥不在这个账号的托管区里。
	NoticeSSHKeyNotFound = "SSH_KEY_NOT_FOUND"
	// NoticeSSHKeyTooMany 表示一个账号的公钥数量到了上限。
	//
	// 账号不是面板创建的、它的 authorized_keys 只读时，用的是 NoticeAccountNotManaged
	// ——同一个理由不设两个码。
	NoticeSSHKeyTooMany = "SSH_KEY_TOO_MANY"
	// NoticeSSHKeyHomeInvalid 表示这个账号的 home 不归面板写。
	//
	// 两个来源：读回来的 home 不在 /home 之下（有人把它指到了 /etc、/root 或别的
	// 地方），以及 home 是符号链接。panel 只会往“自己的账号自己的家目录”里写
	// authorized_keys；别的路径一律不碰，因为那等于在别人的目录里放一把钥匙。
	NoticeSSHKeyHomeInvalid = "SSH_KEY_HOME_INVALID"

	// NoticeSudoTemplateInvalid 表示模板标识不在闭集里。
	NoticeSudoTemplateInvalid = "SUDO_TEMPLATE_INVALID"
	// NoticeSudoAccountNotEligible 表示这个账号不能拿 sudo 规则。
	//
	// 三个来源：共享账号（nologin，能 sudo 也没有意义）、面板身份（PLAN.md 第 19
	// 节明确不提供）、已禁用的账号（给了也要等启用才生效，不如当场说清）。
	NoticeSudoAccountNotEligible = "SUDO_ACCOUNT_NOT_ELIGIBLE"
	// NoticeSudoRuleMissing 表示要删的 sudo 规则不存在。
	NoticeSudoRuleMissing = "SUDO_RULE_MISSING"
	// NoticeSudoFileDamaged 表示 sudoers.d 里那个文件不是面板写的（没有认领标记），
	// 或者同名文件被别的东西占着。
	NoticeSudoFileDamaged = "SUDO_FILE_DAMAGED"
	// NoticeSudoVisudoFailed 表示写下去之后 visudo 通不过。
	//
	// 这条一定要能说出来：它是“sudo 权限写坏了会把 sudo 整体弄坏”的唯一挡板，
	// 也是验收里必须真机制造一次的那条路径（STAGES 第 16 节：visudo 失败保留旧规则）。
	NoticeSudoVisudoFailed = "SUDO_VISUDO_FAILED"
	// NoticeSudoVisudoUnavailable 表示这台机器上没有 visudo。
	//
	// 没有 visudo 就不写 sudoers：这是“不接受任意 sudoers 文本”的一半，另一半
	// 是模板闭集。
	NoticeSudoVisudoUnavailable = "SUDO_VISUDO_UNAVAILABLE"

	// NoticeAccountsUnsupported 表示这台机器缺少做这件事的工具（useradd/usermod…）。
	NoticeAccountsUnsupported = "ACCOUNTS_UNSUPPORTED"

	// NoticeAccountShellMissing 表示某一档壳的落点在这台机器上不存在。
	//
	// 它是一条**告警**而不是阻塞：缺 /bin/rbash 时仍然能建 disabled 档的账号。
	// 两档都缺才是阻塞，那种情况由能力报告给 blocked + reason，不靠这条。
	NoticeAccountShellMissing = "ACCOUNT_SHELL_MISSING"
)
