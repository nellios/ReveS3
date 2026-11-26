# ReveS3

让 S3 客户端读写 Cloudreve 文件夹。支持 Windows、Linux 和 Docker，可以在自己电脑或服务器上运行。

**这是适配 Cloudreve 的非官方项目，与 Cloudreve 官方没有隶属关系。** Cloudreve 名称仅用于说明兼容对象。

当前为预发布版本。Windows / Linux 安装包与校验和见 [Releases](https://github.com/nellios/ReveS3/releases)。项目许可证和已知限制见 [发布检查](docs/release-checks.md)。

- [Windows、Linux 与 Docker 详细步骤](docs/running.md)
- [独立的本机文件上传命令](docs/direct-upload.md)
- [第三方许可证原文](third-party/NOTICES.txt)

## 能做什么

- 用 S3 客户端查看、下载、上传、覆盖和删除网盘文件。
- 将一个文件夹作为一个同名桶，或将父文件夹下的多个文件夹作为多个同名桶。
- 通过 S3 分段上传处理大文件，并在接收分段阶段保留恢复记录。
- 在本机运行，让文件直接从本机网关传到 Cloudreve，不经过远程中转服务器。
- 在服务器运行，为其他设备提供统一的 S3 入口。

网关使用 Cloudreve HTTP API。上传目前只适配已实现的 `local` 存储策略；其他存储策略及具体备份软件需要另外验证。

## 文件怎么传

| 运行位置 | 客户端填写的地址 | 文件经过哪里 |
| --- | --- | --- |
| 自己电脑 | `http://127.0.0.1:7070` | S3 客户端 → 本机网关 → Cloudreve |
| 自己电脑的 Docker | `http://127.0.0.1:7070` | S3 客户端 → 本机容器 → Cloudreve |
| 服务器 | 你配置的网关服务地址 | S3 客户端 → 中转服务器 → Cloudreve |
| 与网关处于同一 Docker Compose 网络的程序 | `http://gateway:7070` | 备份容器 → 网关容器 → Cloudreve |

文件会经过运行网关的机器。这里的本机“直连”是指不经过远程中转服务器；普通 S3 客户端仍由本机网关转换协议，S3 大文件分段也暂存在本机。Docker 只是运行方式，不能让远程服务器上的网关自动变成直传。

`localhost` 指客户端自己所在的环境。在另一个容器或另一台电脑中，不能直接照填 `127.0.0.1`。

## 先准备网盘文件夹

需要一个能够登录 Cloudreve、并且有目标文件夹访问权限的账号。文件夹先在网盘里建好，再配置网关；网关不提供 S3 创建或删除桶功能。

### 只用一个桶

例如网盘已有 `/test` 文件夹，希望客户端 Bucket 填 `test`。在自己的 `gateway.json` 中修改以下字段，其他字段保留原值：

```json
{
  "site": "https://drive.example.invalid",
  "single_bucket_mode": true,
  "buckets_root": "cloudreve://my/test"
}
```

把示例网址换成真实网盘网址。此时对应关系为：

| 客户端 | Cloudreve 文件 |
| --- | --- |
| Bucket=`test`，Key=`a.txt` | `/test/a.txt` |
| Bucket=`test`，Key=`photos/a.jpg` | `/test/photos/a.jpg` |

不需要再创建 `/test/test`。

### 使用多个桶

在网盘中创建 `/S3`，再在里面创建 `test`、`backups` 等文件夹：

```json
{
  "single_bucket_mode": false,
  "buckets_root": "cloudreve://my/S3"
}
```

`/S3/test` 对应 `test` 桶，`/S3/backups` 对应 `backups` 桶。父目录下面新增、改名或移除文件夹，会在后续请求时重新发现；切换配置中的桶模式则需要重启。

桶名使用 3～63 个小写字母、数字或 `-`，首尾为字母或数字，不能叫 `healthz`。文件名可以包含中文和空格。不要填写网盘最外层，也不要把无关私人文件放进要提供给 S3 客户端的目录。

旧的 `bucket` / `root` 和 `buckets` 配置仍保留兼容，但不能与 `buckets_root` 混用。切换模式前先完成或取消未完成上传；出现状态冲突时，不要通过删除 `.state` 跳过检查。

## Windows 快速开始

在 [Releases](https://github.com/nellios/ReveS3/releases) 下载 Windows amd64 压缩包。以下步骤用于完整解压后的程序包；从源码打包的方法见后文。

1. 完整解压 Windows 包到自己可写的文件夹，不要直接在压缩包里运行。
2. 编辑包内 `gateway.json`，填写真实 `site`，并按上面的示例选择桶模式和目录。`_comments` 中有每项的简单说明。
3. 双击 `start.cmd`，首次按提示输入网盘邮箱和密码。登录成功后启动网关，只保存会话，不保存密码。
4. 打开 `.state/s3-credentials.json`，将里面的 S3 密钥填写到客户端。
5. 保持窗口打开。按 Ctrl+C 停止；以后直接运行 `start.cmd`，需要重新登录时先停止网关，再运行 `login.cmd`。

网盘网址只填协议、域名和可选端口，例如 `https://drive.example.invalid`，不要带 `/home` 或 `/api/v4`。默认要求 HTTPS；确实只提供 HTTP 的网盘才设置 `allow_http: true`，这会明文传输密码、会话和文件。

原生包运行不需要安装 Go 或 Python。升级时保留自己的 `gateway.json` 和整个 `.state`，不要用模板覆盖它们，也不要只更新启动脚本而继续使用旧程序。

## S3 客户端怎么填

以下以网关运行在自己电脑、桶名为 `test` 为例：

| 项目 | 填写内容 |
| --- | --- |
| Endpoint | `http://127.0.0.1:7070` |
| Region | `us-east-1`，与网关配置保持一致 |
| Bucket | `test`，不要填写完整网盘路径 |
| Access Key | 网关的 S3 Access Key |
| Secret Key | 网关的 S3 Secret Key |
| 访问方式 | 路径样式 / path-style |

Windows 启动器会自动生成并保存 S3 密钥；Linux 和 Docker 使用启动时设置的环境变量。**S3 密钥与网盘邮箱、密码是两套信息。**

WinSCP 选择 Amazon S3 协议，主机填 `127.0.0.1`、端口填 `7070`，按本地 Endpoint 使用 HTTP；远程目录可填 `/test`。远程目录不是网盘的 `cloudreve://my/test`。

大文件建议每段 8 MiB、上传并发 1。SDK 或客户端如果能配置可选校验和，设为仅在必需时计算或校验；当前没有实现额外的 multipart checksum 算法。具体软件是否兼容，需要验证它实际使用的接口。

## Linux 快速开始

解压 Linux amd64 包后，在解压目录中执行；从源码构建时，将程序放在仓库根目录：

```bash
chmod +x './cloudreve-s3'
cp deploy/gateway.local.example.json gateway.json
chmod 600 gateway.json
```

编辑 `gateway.json` 的网盘网址和桶目录，再登录：

```bash
read -r -p 'Cloudreve email: ' CLOUDREVE_EMAIL
export CLOUDREVE_EMAIL
read -r -s -p 'Cloudreve password: ' CLOUDREVE_PASSWORD
printf '\n'
printf '%s' "$CLOUDREVE_PASSWORD" | './cloudreve-s3' login -config gateway.json -password-stdin
unset CLOUDREVE_PASSWORD CLOUDREVE_EMAIL
```

仅在登录成功后设置独立的 S3 密钥并启动。Secret 请用密码管理器生成至少 32 个字符的随机值，安全保存并在客户端填入同一组值；后续重启继续使用，不要每次重新生成：

```bash
export S3_ACCESS_KEY_ID='reves3-local'
read -r -s -p 'S3 Secret: ' S3_SECRET_ACCESS_KEY
printf '\n'
export S3_SECRET_ACCESS_KEY
'./cloudreve-s3' serve -config gateway.json
```

按 Ctrl+C 停止。保持工作目录不变，因为示例中的 `.state/` 是相对于当前目录的路径。登录失败时先处理报错，不要直接启动服务。

## Docker 快速开始

以下是 Linux Bash 示例，需要 Docker Engine 和 Compose v2；Windows PowerShell 步骤见 [详细运行说明](docs/running.md#docker-compose)。从源码仓库根目录执行：

```bash
cp deploy/gateway.docker.example.json deploy/gateway.json
```

编辑 `deploy/gateway.json` 的 `site` 和桶目录。容器内的监听地址、`/state` 会话路径等保持模板值；不要拿原生模板替换 Docker 模板。

先设置并安全保存一组 S3 密钥，后续启动继续使用同一组值：

```bash
export S3_ACCESS_KEY_ID='reves3-local'
read -r -s -p 'S3 Secret (random, at least 32 characters): ' S3_SECRET_ACCESS_KEY
printf '\n'
export S3_SECRET_ACCESS_KEY
docker compose -f deploy/compose.yaml build
```

构建成功后，使用同一镜像及状态卷登录网盘：

```bash
read -r -p 'Cloudreve email: ' CLOUDREVE_EMAIL
export CLOUDREVE_EMAIL
read -r -s -p 'Cloudreve password: ' CLOUDREVE_PASSWORD
printf '\n'
printf '%s' "$CLOUDREVE_PASSWORD" | docker compose -f deploy/compose.yaml run --rm --no-deps -T -e CLOUDREVE_EMAIL gateway login -config /config/gateway.json -password-stdin
unset CLOUDREVE_PASSWORD CLOUDREVE_EMAIL
```

只有登录成功后才启动服务：

```bash
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml logs --tail 50 gateway
```

客户端连接 `http://127.0.0.1:7070`。默认只发布到宿主机本地，不直接向公网开放。停止时使用 `docker compose -f deploy/compose.yaml down`，不要加 `-v` 或 `--volumes`，否则会删除登录会话和上传恢复记录所在的数据卷。

## 上传限制

| 项目 | 当前范围 |
| --- | --- |
| 不分段的单次上传 | 最多 32 MiB；更大文件必须由客户端使用 S3 multipart |
| S3 分段上传 | 配置上限为单文件 5 GiB，临时文件总预算最多 6 GiB |
| 大文件任务 | 整个实例同时只能有一个未完成 multipart；建议每段 8 MiB、并发 1 |
| 恢复 | 接收分段阶段可恢复；完成转传阶段中断可能需要取消后重传 |
| 上传策略 | 当前仅适配已实现的 Cloudreve `local` 策略 |
| 桶管理 | 桶文件夹提前在 Cloudreve 创建，不支持 S3 创建、删除桶 |
| 其他 S3 功能 | 不承诺完整 S3 兼容；未实现对象复制、版本管理、SSE、ACL 管理等功能 |

程序默认只读。原生与 Docker 部署模板为备份场景开启了 `write_enabled` 和 `multipart_enabled`；如只想下载，把 `write_enabled` 改为 `false`。

**5 GiB 是配置上限，不是已完成的整文件实测结果。** 暂存文件占用网关所在机器的硬盘，网盘自身配额和限制仍然有效。

如果要直接上传一个本机文件，可使用单独的 `upload-direct` 命令。它不提供 S3 服务，也不能让任意备份程序自动直传，见 [本机文件上传](docs/direct-upload.md)。

## 常见问题

**提示 `The specified bucket does not exist`？**

确认桶文件夹已经存在，账号可以访问，桶名符合规则。单桶模式的 `/test` 要配 `single_bucket_mode: true`；多桶模式的父目录下必须另有 `test` 文件夹。客户端 Bucket 只填 `test`。

**提示 `site must be an HTTPS origin; HTTP requires allow_http`？**

检查网盘网址是否只有协议、域名和端口。优先使用真实可用的 HTTPS 地址；只有明确使用 HTTP 时才打开 `allow_http`，不能只改协议来假定站点支持 HTTPS。

**提示登录失败？**

先看完整报错，检查网址和账号。站点要求验证码、2FA 或禁止密码登录时，需要进一步适配；当前登录流程不会绕过这些验证。不要为了重试登录删除 `.state`，里面可能有密钥和未完成上传。

**浏览器打开网关地址只有 XML 或错误？**

这是 S3 接口，不是网盘网页。请用 S3 客户端连接。`/healthz` 只能检查进程是否响应，不能证明网盘登录或文件读写正常。

## 从源码构建

需要 Go 1.26+。Python 3.10+ 仅用于打包和额外验证，网关运行本身不依赖 Python。

```bash
go test ./...
go vet ./...
go build -trimpath -buildvcs=false -o cloudreve-s3 ./cmd/cloudreve-s3
python tools/build_release.py
```

Windows 手工构建时将 `-o` 改为 `cloudreve-s3.exe`。打包工具输出 Windows / Linux amd64 压缩包和 `dist/SHA256SUMS`；可用 `--target windows-amd64` 或 `--target linux-amd64` 只构建一个平台。

ReveS3 是项目名。当前源码命令目录、生成的程序文件和镜像名仍沿用 `cloudreve-s3`，上面的命令按现有文件名编写。

Go 测试使用本机模拟网盘。真实账号读写、图形客户端、Docker 实跑和完整 Windows 启动器验收应分别检查，不能仅凭测试或构建成功就视为全部通过。具体记录见 [发布前检查](docs/release-checks.md)。

## 使用与反馈

首次使用请在专用测试文件夹验证上传、完整下载、覆盖和删除，重要文件保留独立备份。会话、S3 密钥和上传临时文件都应只允许运行网关的用户访问。

不要提交真实 `gateway.json`、`.config`、`.env`、`.state`、签名下载链接或带个人数据的运行目录。公网使用时另行配置 HTTPS 和访问限制。

反馈问题请说明系统、运行方式、程序版本、客户端名称、桶模式、操作步骤和已遮去敏感内容的错误。不要粘贴密码、Token、S3 Secret 或真实私有路径。

## 代码来源与许可证

项目自身许可证尚待维护者确定；当前不标注 MIT、Apache-2.0 等项目授权。公开仓库或附上免责声明不能替代实际授权、依赖合规或代码来源核对。

S3 协议层使用 Versity Gateway，Cloudreve 适配通过 HTTP API 通信。第三方依赖的原始许可证保留在 [third-party/NOTICES.txt](third-party/NOTICES.txt)。开发中参考上游接口的记录与发布前需要核对的事项见 [代码来源与许可说明](docs/code-origin.md)。
