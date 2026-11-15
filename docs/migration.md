# 迁移与回滚

迁移工具只处理离线导出的副本，原目录不写入。它不是生产切换按钮；当前真实数据迁移尚未验收。

## 迁移内容

保留配置未知字段、账号文件、账号回收目录、API key 身份和加密主密钥、用量/状态数据、面板凭据以及任务状态。已识别的本地路径会重映射到新目录，首次启动的自动更新会关闭，旧更新运行指针不会被带入新实例。

外置路径、链接、注册表损坏、丢失原主密钥和源目录变化会明确拒绝。不要通过删除错误文件或生成替代密钥绕过校验。

## 离线演练

在仓库根目录运行；示例目录是专门准备的离线导出与新的空目标：

```powershell
python tools/migration/migrate.py copy --source D:\LoomGate-work\offline-export --destination D:\LoomGate-work\migration-copy
python tools/migration/migrate.py copy --source D:\LoomGate-work\offline-export --destination D:\LoomGate-work\migration-copy --apply --offline-confirmed
```

第一条是 dry-run。第二条要求明确确认导出已离线，先写固定 staging 槽，逐文件及整批核对摘要，再提交新目录。已有目标目录不会被覆盖。

复制完成后，仍需在独立端口启动副本，验证账号能解密、API 身份和额度未变化、用量完整、计划一致、旧端点兼容、更新公钥正确。工具本身的哈希检查不等同于这些运行验证。

## 中断和回滚

```powershell
python tools/migration/migrate.py recover --destination D:\LoomGate-work\migration-copy --offline-confirmed
python tools/migration/migrate.py rollback --destination D:\LoomGate-work\migration-copy --offline-confirmed
```

`recover` 将未完成的 staging 保留到固定失败槽，供检查后重新演练。`rollback` 核对迁移标记与绑定目标后停用新副本目录并保留数据；旧目录从未被迁移工具修改。恢复原运行进程由维护者在确认端口、目录和版本后执行，工具不会自动重启生产。

## 客户端更新身份

- Android 保留原包名与证书。必须比较已有 APK 的公开证书指纹，覆盖安装，不卸载。
- 保留原发布公钥。正式构建不得省略验签公钥，也不得生成替代信任根。
- Windows 的网关与辅助程序需要作为一致的运行包验证。
- 旧用户默认仍跟随旧更新源。新仓库存在并不会使旧客户端自动迁移；桥接更新和旧项目迁移入口需独立验收后发布。
