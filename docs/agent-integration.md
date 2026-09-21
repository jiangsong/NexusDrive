# 内置 Agent 接入

挂载启动后打开控制台的「Agent」页面，展开接入面板。勾选 Codex、Claude 后，
可直接完成安装、刷新状态和确认卸载；不需要复制或执行命令。若 owner 尚未启动
MCP HTTP 监听，点击「启用并重启」，控制台会安全修改当前配置并请求守护进程
重启，页面随后自动重连。

安装从同一份随二进制嵌入的 `internal/integration/SKILL.md` 写入
`~/.agents/skills/cloudfs/SKILL.md` 和 `~/.claude/skills/cloudfs/SKILL.md`。
Codex 的 MCP 配置合并进 `~/.codex/config.toml` 的标记块；Claude 的用户级
MCP 配置合并进 `~/.claude.json`。不会创建项目说明文件。启动、提示和结束
hooks 使用现有适配器，安装时生成当前版本。

界面安装生成两个同一用户 owner 的本地凭据，授权范围
继承 `mcp.allow` 与 `mcp.read_only`，连接同一个 HTTP 服务。凭据保存在用户配置，
不写入技能、网盘或日志。`--config /absolute/path.yaml` 指定的 owner 配置也会被
后续 hooks 使用。运行客户端时须能在 PATH 中找到 cloudfs。

接入面板分别显示客户端版本、技能、MCP 配置、真实认证握手结果、hooks、记忆状态
和自动注入验证状态。实际 cwd 与项目 scope 由每次客户端启动后的 hook 注入；
命令行状态接口仍为自动化和故障诊断保留，不是安装流程的必需步骤。
自动注入始终标为 **unverified**：认证握手不能证明客户端实际执行了 hook。
客户端版本通过限时 `--version` 检测。离线 hook 最多
等待三秒后退出；未启用 hooks 时仍可显式调用 cloudfs 技能。

cwd 与挂载先解析符号链接，嵌套挂载取最长匹配。scope 是挂载内最近 Git 根的
虚拟路径，没有 Git 根则使用启动目录。同一 owner 上两个客户端使用相同 scope。
这只是检索默认值，不授予权限。项目改名不会迁移旧 scope。

`context_search` 搜索项目知识与 personal 记忆；按返回版本调用 `read_text`。
`memory_propose` 的候选须显式接受。`finish_session` 的 `scope` 标记交接，允许
授权范围内另一个客户端检索工作区中同 scope 的交接。旧交接没有 scope 标记，
不会通过项目外工作区补充检索返回。关闭内容索引时保留文件名检索和显式记忆读取。

安装清单在 `~/.config/cloudfs/agents/`，记录发布版本及每个受管部分的 SHA-256。
重复安装幂等，升级前验证哈希；发现手工修改或同名未管理内容就保留并报错。
JSON 的其他值和 hooks 分组保留；Codex 标记块之外的 TOML 字节保留。
单客户端写入前预检所有部分，写入失败尽力恢复已改文件。两客户端顺序安装，
第二个失败不会撤销已成功的第一个；修复冲突后重跑即可。

界面中的「卸载所选客户端」需要输入确认词。卸载只移除清单管理且未修改的内容，
并撤销本次接入创建的凭据。保留用户新增
配置与修改过的受管内容。卸载不会删除缓存、索引、记忆或交接。

## 验证边界

自动测试覆盖配置保留、重复安装/升级/卸载、哈希冲突、凭据撤销、MCP 认证探测、
符号链接/嵌套挂载/Git scope、跨项目交接过滤以及已有版本检查与个人记忆过滤。
真实 Codex/Claude 的技能发现、信任提示与 hook 自动执行仍须在实际挂载上验收，
不能用生成的配置或单元测试替代；目前不报告“全自动就绪”。

客户端格式依据：
[Codex hooks](https://learn.chatgpt.com/docs/hooks)、
[Claude 用户级 MCP](https://code.claude.com/docs/en/mcp)。

本次隔离 HOME 冒烟检查：本机 `codex mcp get cloudfs` 与
`claude mcp get cloudfs` 均成功读取生成的配置。默认用户目录为本版本安装目标；
使用自定义 CODEX_HOME / CLAUDE_CONFIG_DIR 时须确保客户端仍读取上述默认路径。
MCP 测试连接以 Codex 与 Claude Code 两个身份完成候选接受、个人记忆和交接复用，
并验证另一个项目无结果。这是协议级验证，不代表真实模型会话已执行 hooks。
假后端缓存测试的一次样本：冷读 1.20 ms / 热读 0.67 ms，两次输出均为 359 bytes、
估算 99 tokens；远端 ReadRange 次数分别为 1 和 0。时间不是性能承诺。

本次还运行了真实 FUSE 挂载端到端测试：会话 manifest、Agent 写入后 shell 读取、
hook 变更注入/关闭会话，以及 v1/v2 记忆可见性均通过。浏览器相关测试因未启用
`CLOUDFS_BROWSER=1` 跳过。
