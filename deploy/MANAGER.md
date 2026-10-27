# NetShare 安装与管理脚本

适用于使用 systemd 的 Linux amd64、arm64 主机。推荐 Ubuntu 24.04。安装脚本需要 root、Bash、Python 3、systemd 和常见系统工具；节点另外需要 `nft`、`ip`、`ss`。Ubuntu 可安装依赖：

```bash
apt-get update
apt-get install -y python3 nftables iproute2
```

管理脚本使用标准 `/etc/netshare`、`/etc/netshare-agent` 与 `/var/lib/netshare*` 布局；已有配置若使用自定义状态或证书路径，会在变更前拒绝，请按 README 手动维护。

脚本不创建云私网、不修改网卡，不会替你关闭其他防火墙。控制台和节点共用一个二进制，但服务分开管理。初始化、首次部署顺序、HTTPS 和证书说明仍见根目录 README。

## 安装

下载与你的服务器架构匹配的发行包，先核对 `SHA256SUMS`，再解压。没有默认下载站，也不会从私人域名自动拉程序。

| `uname -m` | 文件 |
| --- | --- |
| `x86_64`、`amd64` | `netshare_v版本_linux_amd64.tar.gz` |
| `aarch64`、`arm64` | `netshare_v版本_linux_arm64.tar.gz` |

发行目录包含 `netshare`、`manage.sh`、`deploy/` 和许可证。先用 `netshare init` 在受限目录生成整套凭据，CA 私钥留在离线环境。每台节点只传自己的 `agents/节点ID/` 目录，控制台只传 `controller/` 中需要的叶子文件；脚本不会部署 `authority/` 或明文首次密码。

在每台节点上：

```bash
cd netshare_v版本_linux_对应架构
sudo bash ./manage.sh install agent --bundle /私密目录/agents/edge-a
```

节点代理都在线后，在控制台主机上：

```bash
sudo bash ./manage.sh install controller --bundle /私密目录/controller
```

安装会生成或保留随机管理路径，并将管理脚本放到 `/usr/local/sbin/netshare-manage`。初装旧配置没有随机路径时会自动生成；已有随机路径保持不变。若只准备文件、暂不启动，增加 `--no-start`。

初次控制台启动要求所有节点可连接且没有历史配置；若节点尚未部署，安装会失败并回退文件及服务状态。先部署节点后再装控制台，或使用 `--no-start`。已有有效状态的控制台允许节点离线。

安装已有角色时，脚本保留现有配置、证书、密码和映射，不把新 bundle 覆盖进去。重新安装可以不带 `--bundle`。需要更换证书时按 README 的证书轮换章节处理。

HTTPS 仍由自己的 Caddy、Nginx 等反向代理提供。转发整个站点到控制器回环地址，不剥离随机前缀。脚本不安装 Caddy，也不覆盖已有站点；参考 `deploy/Caddyfile` 配置 DNS 与证书。

## 日常管理

```bash
sudo netshare-manage                 # 中文菜单
sudo netshare-manage status controller
sudo netshare-manage logs controller --lines 100
sudo netshare-manage start controller
sudo netshare-manage stop controller
sudo netshare-manage restart controller

sudo netshare-manage status agent
sudo netshare-manage logs agent --lines 100
sudo netshare-manage restart agent
```

启停和重启只作用于指定角色。停止节点代理不会主动删除已写入内核的映射；需要移除映射时在管理台停用，或按下面的卸载语义操作。脚本不会重启主机、x-ui 或无关服务。

## 随机入口

```bash
sudo netshare-manage entry
sudo netshare-manage path rotate
sudo netshare-manage entry
```

`entry` 才会显示完整入口。请把它保存在书签或密码库中，不要贴进公开日志。`path rotate` 先停止控制台，再原子更换路径；原来运行中的服务会启动并检查新路径，原来停止的服务继续保持停止。失败会恢复原路径与原服务状态。只更换入口，不修改密码、节点、映射和私网。

随机入口下的页面、API、静态资源和健康检查共用同一前缀；根路径和旧接口返回 404，不会跳转泄露新入口。随机路径降低扫描暴露，登录、会话和 CSRF 仍然需要。

## 升级

升级必须显式指定来源。推荐已解压发行目录：

```bash
sudo netshare-manage upgrade controller --source /opt/netshare_v新版本_linux_amd64
```

也可直接使用本地压缩包，并给出独立取得的 SHA256：

```bash
sudo netshare-manage upgrade controller \
  --source /tmp/netshare_v新版本_linux_amd64.tar.gz \
  --sha256 这里填写64位SHA256
```

远程来源只接受 HTTPS，且必须给校验和：

```bash
sudo netshare-manage upgrade agent \
  --source https://你信任的发行站点/发行包.tar.gz \
  --sha256 这里填写64位SHA256
```

归档会先校验大小、路径和文件类型，拒绝穿越路径、符号链接、设备文件及架构不匹配的二进制。仅更新程序和管理脚本，已有配置与状态保留。脚本先停指定服务再快照，原子替换程序，按原启停状态恢复；启动或健康检查失败时回退。

两个角色共用 `/usr/local/bin/netshare`。升级控制台时，正在运行的节点代理仍保持旧进程，不会被顺手重启；要更新节点进程时，再明确执行 `restart agent`。停机前最后一次保存的配置与映射会进入快照，避免失败回退覆盖这笔变更。

## 备份与回退

```bash
sudo netshare-manage backup controller
sudo netshare-manage backup agent
```

备份保存到 `/var/backups/netshare/时间-随机标识-角色/`，权限 0700，包含相应配置、状态、程序、服务文件和恢复清单。节点另存本工具的 nftables 表。备份命令本身不停服务，每份 JSON 文件由程序原子写入；需要严格同一时刻的跨文件备份时，先明确停止该角色再备份。

每次安装、升级、轮换和卸载也会先创建恢复快照。变更失败会自动恢复本次操作前的文件及服务状态。备份含私钥，应另做离机保管，不要放到公开仓库或网盘同步目录。

手动恢复先停止对应服务，按 `manifest.json` 的 `path` 将编号文件或目录恢复到原位置，按 `owners` 恢复 uid/gid/mode，再 `systemctl daemon-reload` 并启动。控制台配置与状态应归属 `netshare:netshare`，目录 0700、文件 0600。不要把 A 节点的凭据恢复到 B；也不要用旧状态覆盖后来新增的业务规则。

脚本不主动把全局 `net.ipv4.ip_forward` 改回 0。这个值可能已被 Docker、VPN 或其他服务使用；回退和卸载都只精确处理本工具的文件与规则表。

## 卸载

```bash
sudo netshare-manage uninstall agent
sudo netshare-manage uninstall controller
```

默认停止并禁用指定服务，移除该服务文件；配置、证书、映射状态和备份保留，方便重新安装。节点卸载另删除 `table ip netshare` 和 `/etc/sysctl.d/90-netshare-agent.conf`，不清空其他 nftables 表、iptables 或整机 conntrack。已有连接可能自然延续。

彻底删除这个角色的配置和状态，需要显式指定：

```bash
sudo netshare-manage uninstall controller --purge
sudo netshare-manage uninstall agent --purge
```

交互终端会要求输入 `PURGE`；非交互环境的显式 `--purge` 视为已授权删除。备份始终保留。另一角色仍安装或运行时，共享二进制保留。管理脚本、系统用户、Caddy 及其站点不会被自动删除。

## 开发验证

```bash
bash -n manage.sh
python3 scripts/manage_test.py
```

测试只使用带 `.netshare-test-root` 哨兵的临时沙箱和模拟 systemd/nft 命令，执行真实安装文件操作、角色隔离、轮换顺序、最后保存变更保护、失败回退、默认卸载、清理和归档拒绝验证。它不连接生产，不修改真实服务、防火墙或网卡。测试根目录仅配合显式测试模式生效，正常管理始终使用标准系统路径。
