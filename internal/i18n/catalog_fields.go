package i18n

// Backend field prompts live apart from the rest of the catalog because they
// are keyed mechanically: field.<backend type>.<config key>, and
// creds.<backend type>.note. A driver keeps registering a plain prompt beside
// its code; the control layer and the wizard prefer the entry here when there
// is one, so adding a language to a driver never means editing the driver.
var fieldsEN = map[string]string{
	"field.webdav.url":         "WebDAV collection URL",
	"field.webdav.user":        "Username",
	"creds.webdav.note":        "the account's password",
	"field.openlist.url":       "OpenList WebDAV URL",
	"field.openlist.user":      "Username",
	"creds.openlist.note":      "the account's password",
	"field.s3.endpoint":        "Endpoint URL, blank for AWS",
	"field.s3.region":          "Region",
	"field.s3.bucket":          "Bucket",
	"field.s3.prefix":          "Key prefix to confine this remote to",
	"field.s3.access_key_id":   "Access key ID, blank for an anonymous bucket",
	"creds.s3.note":            "the secret access key (and a session token if the credentials are temporary)",
	"field.sftp.host":          "SSH host",
	"field.sftp.port":          "SSH port",
	"field.sftp.user":          "SSH user, blank for the current OS user",
	"field.sftp.root":          "Directory to expose",
	"field.sftp.key_file":      "Private key file, blank to use the agent or a password",
	"creds.sftp.note":          "a password, or the passphrase of the key file",
	"field.smb.host":           "SMB server",
	"field.smb.port":           "Port",
	"field.smb.share":          "Share name",
	"field.smb.user":           "User",
	"field.smb.domain":         "Domain or workgroup",
	"field.smb.root":           "Directory inside the share to expose",
	"creds.smb.note":           "the account password, or its NTLM hash",
	"field.box.client_id":      "Box app client ID",
	"creds.box.note":           "the client secret from the Box developer console; config auth then opens a browser for the rest, and Box rotates the refresh token on every use",
	"field.dropbox.client_id":  "App key",
	"creds.dropbox.note":       "config auth opens a browser and Dropbox hands back a lasting grant; no client secret is needed, and no pasted token either",
	"field.gdrive.client_id":   "OAuth client ID",
	"field.gdrive.drive_id":    "Shared drive ID, blank for My Drive",
	"creds.gdrive.note":        "the client secret from the Google Cloud console; config auth then opens a browser for the rest",
	"field.onedrive.client_id": "Application (client) ID",
	"field.onedrive.tenant":    "Directory (tenant) ID",
	"field.onedrive.drive_id":  "Drive ID, blank for the signed-in user's drive",
	"creds.onedrive.note":      "a refresh token from the Microsoft Entra app registration",
	"field.aliyun.client_id":   "Open platform client id / AppId",
	"field.aliyun.drive_id":    "Drive id, blank to fetch it after signing in",
	"field.aliyun.root_id":     "Directory id to use as the root",
	"creds.aliyun.note":        "config auth opens a browser to authorize",
	"field.baidu.client_id":    "Open platform AppKey",
	"field.baidu.root_id":      "Directory to use as the root",
	"creds.baidu.note":         "config auth opens a browser to authorize",
	"field.pan115.client_id":   "115 open platform AppID",
	"field.pan115.root_id":     "Directory id to use as the root",
	"creds.pan115.note":        "config auth authorizes by QR code",
	"field.pan123.client_id":   "Open platform clientID",
	"field.pan123.root_id":     "Directory id to use as the root",
	"creds.pan123.note":        "the clientSecret from the open platform",
	"field.quark.root_id":      "Directory id to use as the root",
	"field.quark.user_agent":   "Request User-Agent, blank for the built-in value",
	"creds.quark.note":         "CloudFS obtains the session through Quark app QR authorization; this is an unofficial API, so rate limits are conservative",
	"field.tianyi.username":    "Tianyi Cloud account (phone number)",
	"field.tianyi.root_id":     "Directory id to use as the root",
	"creds.tianyi.note":        "the account password",
}

var fieldsZH = map[string]string{
	"field.webdav.url":         "WebDAV 集合地址",
	"field.webdav.user":        "用户名",
	"creds.webdav.note":        "该账号的密码",
	"field.openlist.url":       "OpenList WebDAV 地址",
	"field.openlist.user":      "用户名",
	"creds.openlist.note":      "该账号的密码",
	"field.s3.endpoint":        "Endpoint 地址，AWS 留空",
	"field.s3.region":          "区域",
	"field.s3.bucket":          "Bucket",
	"field.s3.prefix":          "限定这个远端的 key 前缀",
	"field.s3.access_key_id":   "Access key ID，匿名 bucket 留空",
	"creds.s3.note":            "secret access key（临时凭据还要 session token）",
	"field.sftp.host":          "SSH 主机",
	"field.sftp.port":          "SSH 端口",
	"field.sftp.user":          "SSH 用户，留空用当前系统用户",
	"field.sftp.root":          "要暴露的目录",
	"field.sftp.key_file":      "私钥文件，留空则用 agent 或密码",
	"creds.sftp.note":          "密码，或私钥文件的口令",
	"field.smb.host":           "SMB 服务器",
	"field.smb.port":           "端口",
	"field.smb.share":          "共享名",
	"field.smb.user":           "用户",
	"field.smb.domain":         "域或工作组",
	"field.smb.root":           "共享内要暴露的目录",
	"creds.smb.note":           "账号密码，或它的 NTLM 哈希",
	"field.box.client_id":      "Box 应用 client ID",
	"creds.box.note":           "Box 开发者后台的 client secret；随后 config auth 会打开浏览器完成其余步骤，Box 每次使用都会轮换 refresh token",
	"field.dropbox.client_id":  "App key",
	"creds.dropbox.note":       "config auth 会打开浏览器，Dropbox 直接给出长期授权；不需要 client secret，也不用手工粘贴 token",
	"field.gdrive.client_id":   "OAuth client ID",
	"field.gdrive.drive_id":    "共享云端硬盘 ID，留空用“我的云端硬盘”",
	"creds.gdrive.note":        "Google Cloud 控制台的 client secret；随后 config auth 会打开浏览器完成其余步骤",
	"field.onedrive.client_id": "应用程序(客户端) ID",
	"field.onedrive.tenant":    "目录(租户) ID",
	"field.onedrive.drive_id":  "Drive ID，留空用登录用户的盘",
	"creds.onedrive.note":      "Microsoft Entra 应用注册里的 refresh token",
	"field.aliyun.client_id":   "开放平台 client id / AppId",
	"field.aliyun.drive_id":    "网盘 drive id，留空则登录后自动取得",
	"field.aliyun.root_id":     "作为根的目录 id",
	"creds.aliyun.note":        "config auth 会打开浏览器完成授权",
	"field.baidu.client_id":    "开放平台 AppKey",
	"field.baidu.root_id":      "作为根的目录",
	"creds.baidu.note":         "config auth 会打开浏览器完成授权",
	"field.pan115.client_id":   "115 开放平台 AppID",
	"field.pan115.root_id":     "作为根的目录 id",
	"creds.pan115.note":        "config auth 会用扫码授权取得",
	"field.pan123.client_id":   "开放平台 clientID",
	"field.pan123.root_id":     "作为根的目录 id",
	"creds.pan123.note":        "开放平台的 clientSecret",
	"field.quark.root_id":      "作为根的目录 id",
	"field.quark.user_agent":   "请求 User-Agent，留空用内置值",
	"creds.quark.note":         "CloudFS 通过夸克 App 扫码授权取得会话；这是非官方接口，限流更保守",
	"field.tianyi.username":    "天翼云盘账号（手机号）",
	"field.tianyi.root_id":     "作为根的目录 id",
	"creds.tianyi.note":        "账号密码",
}

func init() {
	for k, v := range fieldsEN {
		en[k] = v
	}
	for k, v := range fieldsZH {
		zh[k] = v
	}
}

// FieldPrompt renders the prompt for one backend configuration key. fallback
// is what the driver registered: a type with no catalog entry — a new driver,
// or one added out of tree — still asks a readable question.
func FieldPrompt(lang Lang, backendType, name, fallback string) string {
	key := "field." + backendType + "." + name
	if Has(key) {
		return T(lang, key)
	}
	return fallback
}

// CredentialNote renders the one-line explanation of how to obtain a
// backend's credentials, with the same fallback rule as FieldPrompt.
func CredentialNote(lang Lang, backendType, fallback string) string {
	key := "creds." + backendType + ".note"
	if Has(key) {
		return T(lang, key)
	}
	return fallback
}
