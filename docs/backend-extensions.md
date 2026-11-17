# 后端扩展边界与兼容协议

## 模块职责

- `internal/api/chat/message_extras.go`：登录身份、会话/群成员校验和消息增强持久化。
- `message_visibility.go`：按当前用户从 OpenIM 拉取原始消息，验证 ID、序号、可见范围及撤回/删除状态。
- `message_sequence_hints.go`：只缓存定位序号/旧记录扫描进度，不缓存授权或消息内容；不修改旧记录。
- `message_admission.go`：请求准入与进程级并发限制。
- `message_read_progress.go`：已读进度查询和取消传播。上游现有接口按用户查询，最多8个在途进度请求；每进程最多4个已读明细请求、同账号1个。仍为O(成员数)，并非新增了上游批量接口。
- `pkg/common/messageextras`：编辑/回应规则及不可变表情策略；不引用页面或RN。
- `pkg/common/callprotocol`：通话消息编解码与协议字段；不负责权限/页面文案。
- `pkg/common/rtcauthority`：通话状态、媒体准入/回收、投递任务和Redis事务。

## 消息可见性

每次读取增强记录，先校验会话属主和当前群成员资格，再通过用户作用域的pull接口确认原始消息。不存在、已删除、撤回、通知或ID/序号不符不返回增强内容。OpenIM故障时失败关闭，不返回部分未完成验证的结果。不使用管理员SearchMessage作为可见性依据。

新编辑/回应记录保存服务端验证的seq。旧记录没有seq时按用户可见历史查找，每页最多500条、每次最多8页、8秒截止。进度按服务地址/用户/会话/消息/下界隔离，最多4096项、部分进度1分钟；完成但不可见的查找只保留5秒以避免每2.5秒重新扫描全库。所有定位序号每次仍重新拉取验证，不能放大权限。序号缓存最多4096项、1小时。

旧历史首次查找可能返回“正在补齐，请稍后重试”，后台轮询可继续；原元数据和消息内容不迁移。显式可见性收缩立即由当前pull检查生效，已完成但未找到的旧记录在5秒短缓存过期后可重新扫描；未完成的扫描进度保留1分钟。

## 通话协议

保留文本前缀 `[OpenIM 呼叫] `，它是旧传输标识，不作为页面展示文案。无version的旧消息和version=2记录继续兼容。服务端仍发version=2，eventID/room用于去重，缺失时长不推算。`rtcauthority.TerminalRecord`为协议Record别名；OpenIM消息发送适配器仅负责路由与序列化，不重复拼接格式。无版本旧记录重新编码仍省略version。前端解析和UI未修改。

## 表情策略

可选环境变量 `MESSAGE_REACTION_CHOICES` 为JSON字符串数组，如 `["👍","❤️","✅"]`。不配置保持原八种；空数组关闭新增回应；旧回应仍可由原用户取消，不影响其他用户。不合法配置启动失败。默认界面不增加或删除按钮，自定义前端选项需同步产品配置。

## Redis与并发

保留原Redis key/结构/数据，无迁移。只读View不写Redis；Update无变化时只执行TTL续期或事务GET，仍用WATCH/EXEC验证冲突；真正修改仍原子写回。待投递记录继续无TTL，成功投递前不被自动过期。仍保留全局原子预约快照和4096条容量，适合当前部署；不是无限容量或已分片的分布式存储。扩大规模需独立设计跨分片忙碌预约协议，不能直接拆key破坏并发安全。

## 数据与部署

本轮没有修改前端、RPC验证码配置或原体验账号/群/消息。仅替换Chat API二进制，继承现有v14镜像的RPC与配置；数据库卷不变。未提交或推送。验证结果见下方。

## 2026-11-17 验证结果

- 低级模型完成127项测试（含子用例）：API86、RTC35、消息策略4、协议2；全部通过race检查。
- go vet、git diff --check、Linux arm64 Chat API构建通过；补丁对干净源码基线重放检查通过。
- v15容器运行且healthy；容器内chat-api SHA256与构建产物一致：`25749f814ab0d1efae522e7daec8af0ce8dcd76b67ade21441d08d36c4060df5`。
- 部署后消息增强、已读明细、通话接口未登录均拒绝（1001）；前端/chats HTTP200。
- 其他运行容器ID与部署前一致，无新增或缺失。无账号登录、消息发送、数据库清理或体验数据迁移。
- 可见/撤回/删除、旧消息分页、请求取消、WATCH/EXEC及竞争验证使用隔离测试服务。部署后检查是只读冒烟，不宣称本轮重新做过整套浏览器/真机/视频验收。
- 镜像：`local/openim-chat:v1.8.4-patch.5-backend-boundaries-v15`。回滚仅把compose.rtc-authority.yaml的openim-chat镜像改回v14并仅重建该服务，无需数据回滚。


## 2026-11-17 v17 邮箱链路与读取并发补充

本节记录 v15 历史验收后的 v16/v17 增量；前面的 v15 镜像与“仅替换 Chat API”记录保留为历史，不代表当前部署。

- 消息增强 `get` 读取改为进程级 8 槽可等待准入；等待使用请求 context，最多 9 秒，取消或超时不会占用槽位。写操作不走这组读取槽位；已读进度与历史读取的准入规则未改。
- 新增登录保护的 `POST /user/self`。API 从已验证 token context 取得 actor，服务端仅以该 actor 调用现有 `FindUserFullInfo`，返回专用 `{userID,email}`。没有新增 protobuf；客户端请求体不能选择其他用户。
- 普通用户的 Find/Search public/full 以及 `SearchUserInfo` 响应只保留自己的 Email，其他用户 Email 清空；管理员响应保持原行为。其他资料字段和 protobuf 结构不变。前端个人资料通过 ChatServer `chatToken` 调用 `/user/self`，将邮箱并入当前 OpenIM self profile；读取失败或身份不匹配时仍展示基本资料，邮箱不写入 SDK。
- v17 使用 Linux ARM64 `chat-api` 与 `chat-rpc`，以 v16 镜像为基底同时覆盖两个可执行文件。仅重建 `openim-chat`，未重建依赖服务或更改数据库/Redis 数据。
- 部署后 `openim-chat` healthcheck 为 healthy；其他运行容器 ID 保持不变。C 的现有 token 只读验证：`/user/self` 返回 token 对应身份；Search public/full 的本人邮箱匹配、他人邮箱脱敏；无 token 的 self 请求被拒绝；非成员的消息增强与已读明细请求被拒绝。未登录 A/B，未发送消息或修改群数据。
- 后端全模块 `go test -race ./...` 通过。`go vet ./...` 有两条既有 `primitive.E` unkeyed-field 警告，均在未改动的 `pkg/common/db/model/admin/application.go:42`；本轮未改该无关文件。
- 镜像 `local/openim-chat:v1.8.4-patch.5-backend-boundaries-v17`，平台 `linux/arm64`，镜像 ID `sha256:3d984b49c8d5b5af71786ff4a88e459f7e6ebaabd583181eaa64e4c6325d8c48`；基底 v16 digest `sha256:85a40ae2a519b07fd111d71a9c26a205cac26c060cfbd6e4d2f9f46196145bfb`。部署需同时更新 `chat-api` 和 `chat-rpc`。
