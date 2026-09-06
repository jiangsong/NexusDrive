// Chinese first; strings routed through t() so an en table is later a
// copy-and-translate of one file. No locale negotiation — none is asked for.
const zh = {
  'nav.connections': '连接', 'nav.transfers': '传输', 'nav.storage': '缓存',
  'nav.proxy': '代理', 'nav.diagnostics': '诊断',
  'app.queue': '队列', 'app.cache': '缓存', 'app.egress': '出口', 'app.daemon': '守护进程运行中',
  'col.name': '名称', 'col.size': '大小', 'col.modified': '修改时间', 'col.state': '本地状态',
  'state.cached': '已缓存', 'state.dir': '目录已缓存', 'state.partial': '部分', 'state.pinned': '已固定',
  'state.pending': '等待上传', 'state.remote': '仅在远端',
  'action.pin': '固定', 'action.unpin': '取消固定', 'action.warm': '预热', 'action.link': '复制直链',
  'action.refresh': '刷新', 'action.retry': '重试', 'action.stop': '停止', 'action.delete': '删除',
  'action.newfolder': '新建文件夹', 'action.add': '添加网盘', 'action.check': '测试连通性',
  'search.placeholder': '搜索这个网盘',
  'transfers.title': '传输队列', 'transfers.active': '进行中', 'transfers.dead': '死信',
  'transfers.stopped': '已停止', 'transfers.done': '最近完成',
  'storage.title': '缓存与固定', 'storage.used': '已用', 'storage.hit': '命中率',
  'storage.rules': '固定规则', 'storage.gc': '立即回收',
  'proxy.title': '代理出口', 'proxy.outbounds': '出口', 'proxy.groups': '分组', 'proxy.rules': '规则',
  'proxy.recheck': '全部重新探测',
  'diag.title': '诊断与服务', 'diag.recheck': '重新检查', 'diag.fix': '修复',
  'confirm.type': '输入 %s 确认', 'confirm.cancel': '取消',
  'restart.required': '此改动需要重启守护进程才能生效',
  'diag.service': '开机自启服务', 'diag.service.installed': '已安装并启用',
  'diag.service.absent': '未安装', 'diag.service.unsupported': '此平台不支持自启服务',
  'diag.service.install': '安装并启用', 'diag.service.uninstall': '卸载',
  'diag.service.hint': '安装后系统登录时自动挂载（systemd / launchd 用户服务）。',
  'diag.daemon': '守护进程', 'diag.restart': '重启守护进程',
  'diag.restart.confirm': '重启会先把进行中的上传落盘，短暂断开挂载点，然后原地重启并重连。改动过的配置会在重启后生效。',
  'diag.restart.progress': '守护进程正在重启，稍候将自动重连…',
  'add.title': '添加网盘', 'add.type': '网盘类型', 'add.name': '名称（本地标识）',
  'add.name.ph': '例如 my-aliyun，仅字母数字-_', 'add.mount': '同时挂载为文件夹',
  'add.create': '创建', 'add.creating': '创建中…', 'add.required': '必填',
  'add.credstep': '凭据步骤', 'add.next': '下一步：授权',
  'add.auth.url': '在浏览器打开下面的地址完成授权，然后回到这里：',
  'add.auth.qr': '用该网盘的手机 App 扫描下面的内容完成授权：',
  'add.auth.term': '这个网盘的凭据需要在终端完成（密码 / cookie / 外部令牌不经过浏览器）：',
  'add.auth.open': '打开授权页', 'add.copy': '复制', 'add.copied': '已复制',
  'add.waiting': '等待授权完成…', 'add.done': '授权完成', 'add.denied': '授权未通过',
  'add.saved': '已保存到配置。重启守护进程后这个网盘才会生效。',
  'add.restart': '立即重启守护进程', 'add.finish': '完成',
  'empty': '没有内容',
};
export function t(key, ...args) {
  let s = zh[key] || key;
  for (const a of args) s = s.replace('%s', a);
  return s;
}
