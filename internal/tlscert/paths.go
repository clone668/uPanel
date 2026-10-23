package tlscert

// TLS 材料的落地路径（P10.3）。
//
// 这些常量放在 tlscert 里，是因为**两边都要用**：agent 按它们在磁盘上写文件、
// 用它们给证书和私钥设权限；面板按它们渲染 Nginx 的 ssl_certificate 指令、给用户
// 显示“证书在哪”。如果两边各写一份字面量，漂移的后果是配置指向不存在的文件——
// 而 nginx -t 只有在证书路径不存在时才会报错，签发成功、写入成功、apply 失败，
// 中间没有任何一步会提前发现。
//
// 所以：**这里是唯一的一处定义**，internal/certificates 的常量只是别名。
//
// 两套目录是两套信任边界：
//
//	/etc/ssl/upanel/          手动导入。目录 0700 root，证书 0644，私钥 0600。
//	/etc/letsencrypt/live/    Certbot 自己的目录（0700 root）。面板只读取其中的
//	                          fullchain.pem（公钥）；私钥自始至终不经过面板。
const (
	// CertDir 是手动导入证书的落地目录。
	CertDir = "/etc/ssl/upanel"

	// LiveDir 是 Certbot 的 live 目录（每个域名一个子目录）。
	LiveDir = "/etc/letsencrypt/live"

	// ACMEWebroot 是 HTTP-01 校验文件的根目录。
	//
	// 托管站点总会渲染一条 `location ^~ /.well-known/acme-challenge/` 指向它，
	// 所以续期不需要临时改配置：要在证书临期那次续期被访问到，配置里就必须一直
	// 留着这条路。
	ACMEWebroot = "/var/www/upanel-acme"

	// CertbotPath 是 Certbot 可执行文件的绝对路径。
	//
	// 写成绝对路径不是洁癖：syscmd 不接受 PATH 查找，而 certbot 的包把它装在
	// /usr/bin。换发行版时这一处要跟着改。
	CertbotPath = "/usr/bin/certbot"

	// 目录与文件的权限。写成常量是为了让 agent 的写入与验收脚本断言同一个值。
	CertDirMode  = 0o700
	CertFileMode = 0o644
	KeyFileMode  = 0o600
)
