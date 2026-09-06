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
  'empty': '没有内容',
};
export function t(key, ...args) {
  let s = zh[key] || key;
  for (const a of args) s = s.replace('%s', a);
  return s;
}
