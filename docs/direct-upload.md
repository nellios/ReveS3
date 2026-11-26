# 本机文件上传

`upload-direct` 直接读取本机已有文件并调用网盘接口，不启动 S3 服务，也不经过远程 S3 中转机。它是独立命令，只支持 S3 的备份软件仍应连接本机网关。

先把 `gateway.direct.example.json` 复制为自己单独的配置，填写网盘网址和桶目录，按 README 和运行说明登录。它必须使用独立会话和上传状态目录，不能与正在运行的网关共用。

下面以 Windows、配置文件 `direct.json` 为例，文件路径换成自己的：

```powershell
& '.\cloudreve-s3.exe' upload-direct -config direct.json -file '.\large.zip' -bucket test -key 'uploads/large.zip' -verify
```

默认不覆盖同名文件，需要覆盖时才加 `-overwrite`。`-verify` 会上传后再完整下载一次，对比 SHA-256，因此会多用一次完整文件的下载流量。

这个命令不需要 S3 密钥；源文件在上传期间不能修改。大小仍受 `max_multipart_bytes` 限制，最多 5 GiB，目前只支持 local 存储策略。它不会另存整份上传文件或分段副本，但会保存少量恢复记录，不承诺跨进程断点续传。成功输出 `verification=metadata` 只代表状态和大小确认，`verification=sha256` 才代表完整下载对比通过。
