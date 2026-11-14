# 邮箱和手机验证码模式

两个渠道独立选择模式；共用 `verifyCode.superCode` 固定测试码。默认不启用固定码，也不预置测试码。

| 配置 | 真实验证码 | 固定测试码 | 关闭 |
| --- | --- | --- | --- |
| `verifyCode.phone.use` | `ali`（需短信服务配置） | `superCode` | `disabled` 或空字符串 |
| `verifyCode.mail.use` | `mail`（需 SMTP 配置） | `superCode` | `disabled` |

可以只给手机开启固定码，同时保留邮箱真实验证码，反之亦然。固定码模式不发送邮件或短信，不写随机验证码；账号、注册条件等原有校验仍执行。错误码和空码不能通过校验。真实验证码模式不接受固定码兜底。

示例（仅供隔离开发测试，未应用到当前服务）：

```yaml
verifyCode:
  superCode: "YOUR_LOCAL_TEST_CODE"
  phone:
    use: "superCode"
  mail:
    use: "mail"
```

Docker 环境变量分别为 `CHATENV_CHAT_RPC_CHAT_VERIFYCODE_PHONE_USE`、`CHATENV_CHAT_RPC_CHAT_VERIFYCODE_MAIL_USE`、`CHATENV_CHAT_RPC_CHAT_VERIFYCODE_SUPERCODE`。修改配置后重建/重启对应 chat 服务生效。

兼容部署中的 v1.8.4-patch.5：邮箱 `use` 为空或省略时，沿用旧的 `mail.enable`；`enable: true` 使用 SMTP，false 则关闭。显式 `use` 优先，`use: disabled` 即使旧 enable 为 true 也关闭。新版本仓库不依赖旧 enable 配置。

启动时校验模式名称；启用固定码而未配置非空 superCode 时拒绝启动，避免无意开放。当前本地部署保持邮箱 Mailpit 随机验证码、手机关闭；本次没有启用任何渠道的万能码，也没有更改已有体验账号。
