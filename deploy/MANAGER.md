```bash
bash <(curl -fsSL https://github.com/byJoey/netshare/releases/latest/download/install.sh)
```

# NetShare 安装与管理脚本

在控制台服务器以 root 运行上面的命令，选 `1 安装控制台`，端口直接回车默认 `18440`。脚本自动识别架构、获取官方最新稳定 Release、校验 SHA256，生成配置与凭据，通过 `HTTP IP:端口` 启动管理台。普通安装不需要域名、Go、Node.js、机器清单 JSON 或手动整理证书目录。

登录网页面板，点 **接入节点**，名字可填也可留空，再点 **生成并复制命令**，到目标服务器以 root 执行。控制台所在机器也用同一个流程。IP 和网卡由节点自动上报。安装后使用 `sudo netshare-manage` 打开中文菜单。

## 环境要求

- Linux amd64 或 arm64，使用 systemd。推荐 Ubuntu 24.04。
- root、Bash、systemd 和常见系统工具；顶部下载命令需要 `curl`。安装器在 Debian / Ubuntu 自动补齐 Python 3 及节点所需的 `nftables`、`iproute2`，其他发行版先自行安装。
- 选择一个空闲管理端口，在云防火墙和本机防火墙放行，默认 `18440/tcp`。NAT 主机还要有对应公网端口映射。
- 节点私网已在云平台与 Linux 内配置完成，节点之间路由可达，目标服务监听私网 IP 或 `0.0.0.0`。

顶部命令因缺少 `curl` 无法下载时，Ubuntu 可先执行：

```bash
sudo apt-get update
sudo apt-get install -y curl ca-certificates
```

脚本不创建云私网、不修改网卡，不会恢复已关闭的私网或关闭其他防火墙。默认直接通过 HTTP 访问，需要 HTTPS 时自行配置反向代理。

管理脚本使用标准 `/etc/netshare`、`/etc/netshare-agent` 与 `/var/lib/netshare*` 布局；已有配置若使用自定义状态或证书路径，会在变更前拒绝，请按 README 手动维护。

## 一键安装控制台

运行顶部 Bash，选 `1 安装控制台`。首次只问管理端口，回车用 `18440`。脚本会完成：

1. 下载对应架构的发行包并校验。
2. 初始化空控制台，生成内部 CA、控制台叶子证书、随机管理员密码和管理路径。
3. 自动识别本机 IPv4，安装 `netshare.service`，默认监听 `0.0.0.0:18440`。
4. 显示完整 HTTP 管理入口、用户名 `admin` 和首次密码。

控制台不需要先等节点上线。用安装结果或 `entry` 给出的完整地址访问；只访问 `http://IP:端口/` 返回 404 正常。

自动识别优先采用本机全局 IPv4，找不到时采用默认路由网卡上的候选地址，不查询第三方 IP 服务。NAT 机器应通过 `--host` 指定实际访问的公网 IPv4，同时确认管理端口映射可用。

自动初始化资料保存到 `/var/lib/netshare-bootstrap/`，仅 root 可读；控制台进程只能读取它自己的运行凭据，不能读取 CA 主私钥。

| 路径 | 用途 |
| --- | --- |
| `/var/lib/netshare-bootstrap/authority/` | 原控制器 CA 与主私钥，保持原有隔离 |
| `/var/lib/netshare-bootstrap/registration/` | root 签发助手的私网服务端 CA，Web 不可读 |
| `/run/netshare-sign/registration.sock` | Web 与本机签发助手的受限套接字 |
| `/var/lib/netshare-bootstrap/controller/admin-password.txt` | 首次管理员密码，`0600` |
| `/etc/netshare/`、`/var/lib/netshare/` | 控制台运行配置、叶子凭据与状态 |

CA 与首次密码要做加密离机备份，不进 Git、不贴进公开日志。密码文件只记录初次生成的密码；在网页改密码后，请更新自己的密码库。

已有控制台配置时，重新安装自动保留原监听、访问地址、证书、密码和映射，不重新问端口、不生成新 CA。

非交互安装：

```bash
sudo bash ./manage.sh install controller --port 18440
```

NAT 或多 IP 主机在首次安装时指定访问地址：

```bash
sudo bash ./manage.sh install controller --port 18440 --host 192.0.2.10
```

`--host` 决定生成的访问地址，默认仍监听 `0.0.0.0:端口`。访问地址参与 Origin 校验，不能只替换浏览器里的 IP。

已安装后需要换访问 IP 时，先备份并停止控制台，再编辑原配置，仅把 `external_url` 改成实际的 `http://公网IP:端口`。`sudoedit` 保留原文件属主和权限，其他字段保持原样：

```bash
sudo netshare-manage backup controller
sudo netshare-manage stop controller
sudoedit /etc/netshare/controller.json
sudo netshare-manage start controller
```

## 在面板接入服务器

1. 登录网页面板，在映射页或机器页点 **接入节点**。
2. 节点名称可选填；点 **生成并复制命令**，到要接入的服务器以 root 执行。
3. 面板自动显示进度。看到“服务器已接入”后即可创建映射。

无须填写节点 ID、IP、网卡或证书路径。控制台同机也照这三步操作，每台机器使用各自生成的命令。

名称留空时使用节点自己识别的服务器名。接入后在 **机器状态** 点名字旁的铅笔改名，输入 1–80 个字后保存；取消或 Esc 放弃修改。改名不改服务器 hostname、节点 ID、证书或已有映射，重启后仍保留。已有节点重新接入时，留空名字会保留原名，显式填写则使用新名。此功能只需升级控制台，节点代理不用重装。

命令 15 分钟内有效，只能绑定一台服务器。安装中断时直接重新执行同一条命令；已接入的原机重试不会重复登记，也不会删除映射。命令过期后在面板点“换一条命令”。关闭弹窗不影响安装。

首次接入由节点在本机生成私钥，只上传 CSR 与自动识别的网络信息。接入信息和返回配置使用接入码加密，密钥不随 HTTP 请求发送。原控制器 CA 主私钥继续保持原位置；独立的 root 签发助手仅能签发私网节点服务端证书，Web 进程无法读取该私钥，也不能用它签发控制器身份。

接入缓存保存在节点 `/var/backups/netshare-imports/auto-摘要/`，目录 `0700`、文件 `0600`，用于失败后安全重试。不要公开完整命令、私钥或缓存目录内容。

新增节点后的目标列表由控制台自动同步，离线节点上线后会补齐，不用再逐台更新白名单。从 v1.2.x 升级时，重新运行顶部 Bash 获取最新管理脚本，选 3 升级控制台，再在各已有节点用最新 Bash 选 3 升级代理，随后按新流程使用。旧本地管理脚本只会替换程序，首次迁移请用顶部 Bash。

### 安装遇到问题

- **还没有可用私网**：先在服务商面板启用私网，确认 Linux 已分配私网地址，再执行同一命令。
- **发现多张私网网卡**：终端会列出编号，选择连接其他节点的那一张；只有这一种歧义才需要选择。
- **公网地址识别失败**：NAT 等特殊主机可在生成命令后加 `--public-ip 实际公网IP`。
- **本机属于另一控制台**：安装器保留原配置，不能用新命令直接覆盖另一套节点身份。
- **点按钮提示升级**：在控制台服务器运行顶部最新版 Bash，选择 `3 升级` → `controller`，独立接入服务会自动配置。
- **HTTP 复制按钮不能用**：浏览器会选中命令，长按或按 Ctrl+C 复制即可。

菜单 2 的本机自动接入、菜单 13/14 手动登记及旧版配对命令保留兼容，普通用户使用网页按钮即可。

## 高级安装与离线包

无法从节点连接控制台时，仍可安全传输对应配对包，使用 `install agent --pairing /路径/包.tar.gz`。控制台导出的包位于 `/var/lib/netshare-bootstrap/pairings/`，目录 `0700`、文件 `0600`；每包只含该节点配置和叶子凭据，不含 CA 主私钥或管理员密码。

离线 CA 或批量机器清单的用户，可按 [README 高级手动部署](../README.md#高级手动部署)准备同一 CA 的叶子凭据，再显式导入：

```bash
sudo bash ./manage.sh install controller --bundle /私密目录/controller --source .
sudo bash ./manage.sh install agent --bundle /私密目录/agents/edge-a --source .
```

批量清单初始化方式仍需先部署全部代理，再首次启动带节点的控制台；空控制台一键流程不受此顺序限制。只安装文件、暂不启动时加 `--no-start`。安装已有角色默认保留原配置，明确指定 `--pairing` 或 `--join` 才会导入节点更新凭据。

也可从 [GitHub Releases](https://github.com/byJoey/netshare/releases) 下载对应包，核对 `SHA256SUMS` 后解压。交互菜单仍会获取官方最新稳定版；离线使用当前目录要明确传 `--source .`：

```bash
sudo bash ./manage.sh install controller --port 18440 --source .
sudo bash ./manage.sh install agent --pairing /root/netshare-node-edge-a.tar.gz --source .
```

| `uname -m` | 文件 |
| --- | --- |
| `x86_64`、`amd64` | `netshare_v版本_linux_amd64.tar.gz` |
| `aarch64`、`arm64` | `netshare_v版本_linux_arm64.tar.gz` |

## 日常管理

```bash
sudo netshare-manage
sudo netshare-manage status controller
sudo netshare-manage logs controller --lines 100
sudo netshare-manage start controller
sudo netshare-manage stop controller
sudo netshare-manage restart controller

sudo netshare-manage status agent
sudo netshare-manage logs agent --lines 100
sudo netshare-manage restart agent
```

操作只针对指定角色，不重启主机、x-ui 或无关服务。停止节点代理不会主动删除内核里的映射；需要移除时先在管理台停用，或按卸载语义操作。

## 可选：自行配置 HTTPS

默认 HTTP 直连即可使用。需要 HTTPS 时，自行安装 Caddy、Nginx 等反向代理，转发整个站点并保留随机前缀。

修改 `/etc/netshare/controller.json`：`external_url` 填 `https://你的域名`，`cookie_secure` 设为 `true`。同机反代可将 `listen` 改回 `127.0.0.1:18440`，然后重启控制台。参考 [README 反代说明](../README.md#可选自行反代-https)和 `deploy/Caddyfile`。旧 `init --nodes --domain` 初始化仍适用这套手工部署方式。

## 随机入口

```bash
sudo netshare-manage entry
sudo netshare-manage path rotate
sudo netshare-manage entry
```

`entry` 显示完整入口。请保存到书签或密码库，别贴进公开日志。轮换会先停止控制台，再原子更换路径；原来运行中的服务会启动并检查新地址，原来停止的服务保持停止。失败恢复原路径与服务状态。只更换入口，不改密码、节点、映射或私网。

页面、API、静态资源和健康检查共用同一随机前缀；根路径和旧接口返回 404，不跳转泄露新入口。随机路径减少扫描暴露，登录、会话和 CSRF 校验继续生效。

## 升级

菜单选 `3 升级`，再选控制台或节点。默认下载官方最新稳定包和 `SHA256SUMS`，自动匹配架构、核对文件名与 SHA256，不需要 GitHub token。

```bash
sudo netshare-manage upgrade controller
```

离线目录或本地压缩包：

```bash
sudo netshare-manage upgrade controller --source /opt/netshare_v新版本_linux_amd64
sudo netshare-manage upgrade controller \
  --source /tmp/netshare_v新版本_linux_amd64.tar.gz \
  --sha256 这里填写64位SHA256
```

远程自定义来源只接受 HTTPS，且必须提供独立取得的 SHA256：

```bash
sudo netshare-manage upgrade agent \
  --source https://你信任的发行站点/发行包.tar.gz \
  --sha256 这里填写64位SHA256
```

解包前校验大小、路径、文件类型和二进制架构，拒绝穿越、链接与设备文件。升级先停指定服务再做稳定快照，原子替换程序，按原启停状态恢复；启动或健康检查失败回退。已有配置与状态保留。

两个角色共用 `/usr/local/bin/netshare`。升级控制台不会顺手重启正在运行的节点代理；要更新另一角色的运行进程，再明确执行对应的 `restart`。快照包含停机前最后一次保存的配置与映射。

## 备份与回退

```bash
sudo netshare-manage backup controller
sudo netshare-manage backup agent
```

备份在 `/var/backups/netshare/时间-随机标识-角色/`，权限 `0700`，包含对应配置、状态、程序、服务文件和恢复清单。控制台备份包含初始化 CA 与配对资料；节点另存本工具的 nftables 表。自行配置的反向代理另做备份。备份命令不停服务，每份 JSON 由程序原子写入；需要跨文件同一时刻快照时，先停止对应角色。

安装、升级、节点添加、入口轮换和卸载也会先创建恢复快照。失败自动恢复本次操作前的文件与服务状态。备份含私钥，另做加密离机保护。

手动恢复先停止对应服务，按 `manifest.json` 的 `path` 恢复编号文件或目录，再按 `owners` 恢复 uid/gid/mode，执行 `systemctl daemon-reload` 并启动。控制台运行配置与状态归属 `netshare:netshare`，目录 `0700`、文件 `0600`；CA 主私钥与配对资料维持 root 专用。不要将 A 节点凭据恢复到 B，也不要用旧状态覆盖后续业务规则。

脚本不主动把全局 `net.ipv4.ip_forward` 改回 `0`。Docker、VPN 等服务可能也在使用它，回退和卸载只处理本工具自己的文件与规则表。

## 卸载

菜单选 `12 卸载`，再选角色。默认保留数据，彻底清理还需输入 `PURGE`；取消不会卸载。停止服务、更换入口也会先确认，完成或失败都返回菜单。

```bash
sudo netshare-manage uninstall agent
sudo netshare-manage uninstall controller
```

默认停止并禁用指定服务、移除该服务文件；配置、证书、映射状态、初始化资料与备份保留。自行配置的反向代理不受影响。

节点卸载另删除 `table ip netshare` 和 `/etc/sysctl.d/90-netshare-agent.conf`，不清空其他 nftables 表、iptables 或整机 conntrack。已有连接可能自然延续。

明确删除角色数据时加 `--purge`：

```bash
sudo netshare-manage uninstall controller --purge
sudo netshare-manage uninstall agent --purge
```

交互终端要求输入 `PURGE`；非交互显式 `--purge` 视为已授权删除。控制台彻底清理包含 `/var/lib/netshare-bootstrap` 的 CA 与配对资料。确认不再需要再执行。备份始终保留；另一角色还在时，共享二进制保留。管理脚本、系统用户和原有网站不会被自动删除。

## 开发验证

```bash
bash -n manage.sh
python3 scripts/manage_test.py
python3 scripts/release_package_test.py \
  dist/netshare_v1.2.2_linux_amd64.tar.gz \
  dist/netshare_v1.2.2_linux_arm64.tar.gz \
  --sums dist/SHA256SUMS
```

隔离测试使用显式测试模式及带 `.netshare-test-root` 哨兵的临时目录，不连接生产，不改真实服务、防火墙或网卡。发行包回归还要使用实际构建产物校验归档布局与安装路径，不能只验证附件 SHA256 或进入菜单后退出。
