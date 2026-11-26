# Linux、Windows 与 Docker 运行说明

三种方式复用同一个程序。原生压缩包面向 Linux amd64 和 Windows amd64，运行时不需要 Go 或 Python；从源码构建需要 Go 1.26 以上。Docker 使用 Linux 容器，Windows 宿主机需使用支持 Linux 容器的 Docker 环境。本文的原生命令在解压目录执行，Docker 命令在源码仓库根目录执行。

## 连接方式与共同配置

| 网关位置 | 客户端的 S3 Endpoint | 文件正文路径 |
| --- | --- | --- |
| 与客户端同机运行原生程序 | `http://127.0.0.1:7070` | 客户端 → 本机网关 → Cloudreve |
| 本机 Docker，客户端运行在宿主机 | `http://127.0.0.1:7070` | 客户端 → 本机容器 → Cloudreve |
| 备份程序与网关位于同一 Compose 网络 | `http://gateway:7070` | 备份容器 → 网关容器 → Cloudreve |
| 远程中转服务器 | 该服务器已配置的 Endpoint | 客户端 → 远程网关 → Cloudreve |

本机“直连”指不经过远程中转服务器，本机网关仍执行协议转换。Docker 只是部署方式，不会自动改变文件路径；容器中的 `localhost` 指该容器自身。同一 Compose 网络以外的客户端应使用从其环境可达的地址。

Linux 和 Docker 启动前复制对应模板；Windows 包已经带有待修改的 `gateway.json`。只修改自己新建的配置，不覆盖正在使用的配置：

- `site`：实际网盘的 HTTPS 源站地址，只含协议、域名和可选端口；不要附加 `/home`、`/api/v4` 等网页或 API 路径、查询参数、片段或账号密码。不要填写 S3 Endpoint。
- `allow_http`：默认 `false`。如果网盘确实仅提供 HTTP，需明确改成 JSON 布尔值 `true`（不加引号）；这会允许通过 HTTP 明文传输登录凭据、会话令牌和文件内容，只适用于已接受该风险的可信网络。优先使用实际可用的 HTTPS 地址，不能只改协议来假定站点支持 HTTPS。
- `single_bucket_mode`：省略或 `false` 时，`buckets_root` 是桶父目录，直属合法文件夹各自成为同名桶；设为 `true` 时，`buckets_root` 本身成为唯一桶，桶名取目录最后一级，例如 `/test` 对应 `test`。
- `buckets_root`：已经存在并已授权的 Cloudreve 目录 URI，不能填账号根目录。根据上面的开关填写父目录或桶目录本身，不自动创建、迁移或改名；不存在的桶目录不可用于读写。
- `_comments`：配置中的中文字段说明，仅作注释，不影响运行；可以修改说明或移除此对象。文件仍须为严格 JSON，不能添加裸 `//`、`/* */` 或尾逗号。
- S3 客户端使用该网关的 `S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY`、Region `us-east-1`，并启用 path-style。网盘登录与 S3 密钥彼此独立。
- 每个实例使用独立的网盘登录会话和状态目录，不复制或共用正在运行实例的会话。会话以明文保存，应只允许运行用户访问；Windows 需检查所在目录的 ACL。

例如网盘中已有 `/test`，希望直接作为 `test` 桶，配置以下两个字段（这是配置片段，保留其他字段）：

```json
"single_bucket_mode": true,
"buckets_root": "cloudreve://my/test"
```

客户端 Bucket 填 `test`，WinSCP 远程目录填 `/test`；Key=`a.txt` 会写入网盘 `/test/a.txt`，无需再建 `/test/test`。末级目录名需为 3～63 位小写字母、数字或连字符，首尾为字母或数字，不能为 `healthz`；不符合规则时停止并提示，不自动改名。不要同时填写 `bucket`、`root` 或 `buckets`。

如需多个桶，将 `single_bucket_mode` 保持 `false`，`buckets_root` 填 `cloudreve://my/S3`，在 `/S3` 下提前创建 `test`、`backups` 等目录。切换开关需要重启；修改多桶父目录下的文件夹则会在下一次请求时发现。

升级已有 Windows 包时，先正常停止对应实例，更新程序 `.exe` 及启动脚本，保留自己的 `gateway.json` 和整个 `.state`，再按上例添加开关并启动。不要用模板覆盖自己的配置，也不要与另一个实例共用状态。旧 `.exe` 不认识新开关和 `_comments`，只更新脚本或配置会报配置错误。Linux 同样需要更新可执行文件；Docker 需用新源码重新构建镜像。本文不自动升级正在运行的实例。

切换单桶与多桶模式前，应在原模式完成或取消未完成上传；原模式仍保留恢复记录或 multipart 回执时，启动检查可能拒绝切换。出现提示时恢复原模式并处理记录，不要删除 `.state` 或手工搬动恢复文件。此开关复用现有单桶后端，没有自动迁移网盘目录或上传状态。

新增模板已开启写入与 multipart：单次 PUT 最多 32 MiB，单文件 multipart 配置上限 5 GiB，暂存预算 6 GiB，同时只允许一个未完成 multipart。两种桶目录模式都支持这些限制内的上传。S3 模式需要在网关所在机器暂存分段，并不具有 `upload-direct` 的无正文暂存特性。大文件客户端必须使用 multipart；建议分段 8 MiB、上传并发 1。5 GiB 是配置上限，不是完整传输实测结果。

S3 客户端如可配置可选校验和，应设为仅在必需时计算和验证；当前不支持额外 multipart checksum 算法。具体备份程序仍需验证其实际调用的接口，不能仅凭它支持 S3 就认定兼容。

## Linux 原生

解压 Linux amd64 包后，在独立的运行目录执行；从源码运行时，先把构建出的程序放在仓库根目录。

```bash
chmod +x './cloudreve-s3'
cp deploy/gateway.local.example.json gateway.json
chmod 600 gateway.json
```

编辑 `gateway.json` 的 `site` 和 `buckets_root`，然后登录：

```bash
read -r -p 'Cloudreve email: ' CLOUDREVE_EMAIL
export CLOUDREVE_EMAIL
read -r -s -p 'Cloudreve password: ' CLOUDREVE_PASSWORD
printf '\n'
printf '%s' "$CLOUDREVE_PASSWORD" | './cloudreve-s3' login -config gateway.json -password-stdin
unset CLOUDREVE_PASSWORD CLOUDREVE_EMAIL
```

登录成功后设置网关的 S3 凭据。以下仅为首次生成示例，需要宿主机已有 `openssl`；也可以使用密码管理器生成至少 32 个字符的随机 Secret。安全保存并在客户端填写同一组值，后续重启继续使用这组值，不要每次启动重新生成。

```bash
export S3_ACCESS_KEY_ID='gateway-local'
export S3_SECRET_ACCESS_KEY="$(openssl rand -hex 32)"
'./cloudreve-s3' serve -config gateway.json
```

前台运行时，按 Ctrl+C 停止。守护运行可按仓库已有的 systemd 模板自行安装；本文不会自动安装服务或修改现有服务。保持工作目录不变，因为模板的 `.state/` 是相对于进程工作目录的路径。

## Windows 原生

完整解压 Windows amd64 包到自己可写的独立目录。包内已经放好 `gateway.json`，使用系统自带 Windows PowerShell 5.1，不需要安装 Go、Python 或 PowerShell 7，也不需要管理员权限。

1. 编辑 `gateway.json` 的 `site`、`single_bucket_mode` 和 `buckets_root`。HTTPS 网盘可保留其他默认值；HTTP 网盘还需按前述说明显式设置 `allow_http: true`。单桶模式提前建好桶目录；默认多桶模式需父目录及其下的同名桶文件夹。
2. 双击 `start.cmd`。首次按提示输入网盘邮箱和密码，密码输入时不回显。登录成功后启动网关。
3. 打开 `.state/s3-credentials.json`，把 `access_key_id`、`secret_access_key` 填入 S3 客户端。Secret 首次随机生成，后续启动继续使用原值；不要把该文件发给其他人。
4. 下次直接双击 `start.cmd`，有效会话和密钥会自动复用。需要主动换账号或服务端拒绝旧会话时，先停止网关，再双击 `login.cmd` 重新登录；已有 S3 密钥保持不变。

保存位置如下，均相对于解压目录：

| 文件 | 内容 |
| --- | --- |
| `gateway.json` | 网盘地址、桶目录模式、目录 URI、监听等配置及中文注释；不填写网盘密码 |
| `.state/session.json` | 网盘登录会话，不保存网盘密码 |
| `.state/s3-credentials.json` | 网关独立的 S3 Access Key 和 Secret |
| `.state/uploads/` | 上传分段、恢复状态与完成回执 |

启动器将专用会话目录、会话和密钥文件的 ACL 限制为当前 Windows 用户及 SYSTEM；请使用支持 Windows ACL 的本地文件系统。会话和 S3 密钥仍是本机明文文件，应妥善保管。需要自定义 S3 密钥时，停止程序后修改密钥文件的两个字段再启动；Secret 至少 32 个字符。损坏的配置或密钥文件会导致启动停止，不自动覆盖。登录失败不会生成新密钥，也不会删除已有会话和上传恢复状态。

启动器固定以解压目录为工作目录，可从快捷方式或其他目录启动；同一状态目录只允许一个启动器运行。默认保留 `session_file: ".state/session.json"`，如需修改，必须仍放在包内的专用子目录，不能放在包根目录、包外或经过目录链接。密钥保存在会话所在目录；`upload_dir` 建议保留在该受限目录内。复杂目录布局可继续使用原生 `login` / `serve` 命令，自行管理环境变量和 ACL。

WinSCP 如果分开填写主机和端口，主机为 `127.0.0.1`、端口为 `7070`，协议选 S3，使用当前本机 HTTP 连接；如填写远程目录，可填 `/test`。其他客户端的 Endpoint 填 `http://127.0.0.1:7070`，Region 为 `us-east-1`，启用 path-style。运行期间保留窗口，按 Ctrl+C 停止。两个 `.cmd` 仅对本次 PowerShell 进程使用 ExecutionPolicy Bypass，不修改系统执行策略。当前不提供 Windows Service 安装器。

若旧启动器显示 `site must be an HTTPS origin; HTTP requires allow_http` 后又显示 `Login failed`，先检查上述站点配置；这发生在向网盘发送登录请求之前，不能据此认定账号或密码错误。修正后重新运行 `start.cmd` 即可，不需要删除 `.state`。新版启动器会在提示输入账号密码之前指出 HTTP 开关或站点路径问题，不会自动开启 HTTP 或改写配置。

登录仍失败时，新启动器会将原生程序已脱敏的错误合并到最后一行，例如 `Login failed (exit 1): ... error (HTTP 200, code 40020)`。反馈时提供完整的这一行即可，不要发送密码、会话文件或 S3 密钥。只有旧版通用 `Login failed` 无法区分配置、网盘拒绝、网络或本地保存失败；旧包需一并提供它前面的错误行。`Cloudreve response unavailable or invalid` 仍是网络或响应格式的通用错误，不代表已确认密码错误，也不代表已查明具体网络原因。验证码、2FA 或站点禁止密码登录时，现有密码登录入口不会绕过这些要求。

## Docker Compose

需要可用的 Docker Engine / Docker Desktop 和 Compose v2。本节从源码构建镜像，不拉取本项目未发布的镜像。Docker 不使用 Windows 启动器；需要自行设置并安全保存 `S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY` 两个环境变量，后续重启复用原值。Linux 可使用前述生成方式；Windows PowerShell 7 首次生成示例如下（只设置当前进程，不写系统环境变量）。Compose 对缺失变量会直接报错。

```powershell
$env:S3_ACCESS_KEY_ID = 'gateway-local'
$env:S3_SECRET_ACCESS_KEY = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

复制容器专用配置，不要直接使用监听 `127.0.0.1` 的原生模板：

```bash
cp deploy/gateway.docker.example.json deploy/gateway.json
```

Windows PowerShell 对应命令：

```powershell
Copy-Item -LiteralPath deploy/gateway.docker.example.json -Destination deploy/gateway.json
```

编辑 `deploy/gateway.json` 的 `site` 和 `buckets_root`；容器配置保留 `listen: "0.0.0.0:7070"`、`allow_remote_listen: true`、`session_file: "/state/session.json"` 和 `upload_dir: "/state/uploads"`。配置文件不含口令或 Token，Linux 宿主机需确保容器 UID 65532 可读取它；配置若设为只有宿主用户可读，容器可能读取失败。

```bash
docker compose -f deploy/compose.yaml build
```

使用同一镜像及状态卷执行一次登录。Linux Bash：

```bash
read -r -p 'Cloudreve email: ' CLOUDREVE_EMAIL
export CLOUDREVE_EMAIL
read -r -s -p 'Cloudreve password: ' CLOUDREVE_PASSWORD
printf '\n'
printf '%s' "$CLOUDREVE_PASSWORD" | docker compose -f deploy/compose.yaml run --rm --no-deps -T -e CLOUDREVE_EMAIL gateway login -config /config/gateway.json -password-stdin
unset CLOUDREVE_PASSWORD CLOUDREVE_EMAIL
```

Windows PowerShell 7：

```powershell
$env:CLOUDREVE_EMAIL = Read-Host 'Cloudreve email'
$loginPassword = Read-Host 'Cloudreve password' -AsSecureString
try {
    [System.Net.NetworkCredential]::new('', $loginPassword).Password |
        docker compose -f deploy/compose.yaml run --rm --no-deps -T -e CLOUDREVE_EMAIL gateway login -config /config/gateway.json -password-stdin
    if ($LASTEXITCODE -ne 0) { throw 'Login failed; do not start the gateway.' }
} finally {
    $loginPassword.Dispose()
    Remove-Item Env:CLOUDREVE_EMAIL
}
```

仅在登录成功后启动服务：

```bash
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml logs --tail 50 gateway
```

检查就绪端点可使用 `curl http://127.0.0.1:7070/healthz`（PowerShell 可用 `Invoke-WebRequest`）；它只表示进程可响应，不证明网盘会话或上传有效。客户端还应实际列举一个已存在的测试桶，并对自行创建的测试文件执行上传和完整下载校验。

容器以 UID/GID 65532 运行，根文件系统只读，`gateway-state` 命名卷保存登录会话、上传分段和恢复状态。镜像预建归该用户所有的 `/state`，新建空卷按 Docker 的默认行为初始化；已有卷或自行替换的 bind mount 仍需正确的所有权与写权限。宿主机只发布 `127.0.0.1:7070`，不会因容器内部监听 `0.0.0.0` 自动开放公网访问。

停止或移除容器可用 `docker compose -f deploy/compose.yaml down`；不要附加 `--volumes` / `-v`，否则会删除网盘会话和未完成上传的本地状态。需要让另一个 Compose 备份容器访问时，将它与 `gateway` 接入同一个网络，在备份程序填写 `http://gateway:7070`。独立 Compose 项目默认不共享网络。

## 从源码打包

开发机安装 Go 1.26+、Python 3.10+ 后，在仓库根目录运行：

```bash
python tools/build_release.py
```

默认生成 `dist/cloudreve-s3-linux-amd64.tar.gz`、`dist/cloudreve-s3-windows-amd64.zip` 和 `dist/SHA256SUMS`。可用 `--go` 指定 Go 可执行文件，`--output` 指定输出目录，或用 `--target linux-amd64` / `--target windows-amd64` 选择目标。工具只改变构建子进程的目标平台，不修改当前终端的 GOOS / GOARCH。

原生包包含程序、原生配置模板、本文、包内引导说明及 `third-party/NOTICES.txt`。Windows 包额外包含 `start.cmd`、`login.cmd`、`Start-Gateway.ps1` 和从模板复制的根目录 `gateway.json`；Linux 包结构不变。打包工具不读取实际 `gateway.json`、`.config`、`.state/`、网盘会话或 S3 凭据。Docker 构建仍使用源码仓库的允许列表上下文；原生包不包含 Docker 构建所需源码。

## 本地验收

在源码根目录执行：

```sh
go test ./...
go vet ./...
python -m pip install -r tools/requirements-validation.txt
python tools/build_release.py
python tools/verify_windows_launcher.py
```

最后一项只能在 Windows 运行。它会解压新包，在临时目录调用系统 PowerShell 5.1，只连接本机模拟网盘，不读取真实账号配置。若测试因权限或环境失败，应先处理失败原因，不要写成通过。

实际网盘使用前，请先在专门的测试文件夹检查小文件上传、完整下载、覆盖、删除和大文件上传，确认无误后再用于备份。已有生产文件和备份不应作为测试数据。当前发布准备结果见源码中的 `docs/release-checks.md`。
