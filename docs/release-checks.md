# v0.1.0-preview.1 发布记录与限制

仓库：[nellios/ReveS3](https://github.com/nellios/ReveS3)。维护者已明确授权推送源码并上传安装包，本次按预发布版本发布。项目许可证、源码来源核对和部分真实运行验收仍未完成，不作为稳定版发布。

## 已有验证

| 项目 | 2026-11-26 的实际记录 |
| --- | --- |
| Go 测试与静态检查 | 75 项测试（含子用例）及 `go vet ./...` 通过；网关代码与已测试副本一致 |
| 原生程序包 | Windows / Linux amd64 构建、包内允许清单、配置、SHA-256 与旧 Git 构建信息排除检查通过 |
| Windows 启动器 | PowerShell 5.1、本机模拟网盘的 45 项检查通过；包含首次登录、S3 列举、单桶/多桶、重启复用、失败保护与私有文件权限 |
| 文档示例 | 26 处相对链接、2 段 JSON、8 段 Bash 命令语法检查通过；配置默认值及上传限制与模板一致 |
| 公开源码 | 51 个文件按清单导入，仅保留目标仓库初始化提交；没有导入原开发仓库历史或真实运行配置 |
| 敏感信息初查 | 高置信特征扫描未命中；这只是初查，不等于完整安全审计 |

安装包沿用已验证程序，未因项目展示名或文档更新而重新编译；外部下载文件名改为 ReveS3，包内程序和目录名保持现有名称。发布时提供 `SHA256SUMS`，以 Git 标签、提交及附件校验和确认版本。

## 仍未完成

- [ ] 项目自身许可证选择及[代码来源核对](code-origin.md)。未声明整个项目采用 MIT、Apache-2.0 或其他许可。
- [ ] 该发布副本本轮真实网盘读写、WinSCP / Netcatty 图形界面与 Docker 实跑。
- [ ] 完整 5 GiB 文件传输、ARM64 及其他 Cloudreve 存储策略。

上述范围没有因本次上传自动变成通过。实际网盘使用前，先在专用测试文件夹确认上传、完整下载、覆盖和删除，重要文件保留独立备份。

## 复验命令

源码检查：

```bash
go test ./... -count=1
go vet ./...
```

Windows 启动器检查，需要 Windows、Python 和开发依赖：

```powershell
python -m pip install -r tools/requirements-validation.txt
python tools/build_release.py
python tools/verify_windows_launcher.py
```

验收工具仅使用本机模拟网盘和临时数据。已有实际输出为 `windows_launcher_acceptance: passed`；后续修改程序、配置或启动器后，应对新程序包重新执行。

## 运行文件与历史

真实 `gateway.json`、`.config`、`.env`、`.state`、研究缓存、服务器访问记录和个人日志不属于本仓库。`dist/` 的构建包通过 Releases 单独发布，不提交进源码历史。

ReveS3 与 Cloudreve 官方没有隶属关系。名称、依赖声明或免责声明不能代替实际授权与代码来源核查，细节见[代码来源与许可说明](code-origin.md)。
