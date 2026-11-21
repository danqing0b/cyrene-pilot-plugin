# Navigator Work 工具接入

该接入使用公开 Navigator 的 `create_persistence_app(work_connector_bridges=...)` 扩展点，保留原版 Work API、组织/工作区鉴权和 Harness 工具审批。无需修改上游源码或其固定 `TOOL_OPERATIONS` 白名单。

调用链为：Navigator Harness → 已鉴权的 Work tool-providers HTTP 路由 → 宿主 PilotBridge → Go DirectPluginRuntime。它不使用 `conversation_capabilities.DirectToolProviderClient`，因此不把该客户端的签名封装直接发送给仅接收工具参数的插件。授权由 Navigator Work API 执行，插件连接只能来自可信宿主的回环/Unix socket；这不是对会话签名客户端的通用兼容实现。

## 安装与启动

1. 在 Navigator 的 Python 环境中安装该 checkout 的锁定依赖及公开的 `cyrene_plugin_runtime`、`cyrene_tool_provider_contracts` SDK。
2. 启动 Go 插件，保存其 `connection_ref`。绑定名必须与下面配置一致。每个组织/工作区使用独立插件实例和私有存储，禁止跨租户复用连接或状态目录。
3. 使用本仓库的 `navigator.binding.example.json`，填写组织、工作区及连接环境变量。现有 principal 配置继续使用原格式，并至少提供该工作区的一名 `can_takeover=true` 写入者。
4. 用以下命令替换原持久化服务启动命令，其余参数继续传给原版启动器：

```bash
export CYRENE_PILOT_CONNECTION_REF='grpc://127.0.0.1:实际端口'
/path/to/navigator/.venv/bin/python integrations/navigator/serve.py \
  --navigator-root /path/to/Cyrene-Navigator \
  --pilot-bindings "$PWD/navigator.binding.example.json" \
  --database /srv/navigator/persistence.db \
  --principal-config /srv/navigator/principals.json \
  --host 127.0.0.1 --port 8765
```

启动器仍调用原版 Navigator 的 main，保留产品服务凭据、TLS、原有连接器及其他参数。更新原版启动器后，应重新运行兼容验证。

## 使用与权限

Harness 的标准工具提供者发现逻辑读取 `/api/v1/workspaces/{workspace}/work/tool-providers`，随后读取 `/{binding}/tools`。桥接将 protobuf 目录转换为 Harness 已支持的工具 schema 和 `readOnly` 字段，将 JSON 结果投影为 `content` 和 `structuredContent`。

只有 `pilot_job_status`、`pilot_doctor_check` 标记为只读。会产生调用费用、生成任务或修改 Product 的工具保留标准 Harness 写入审批与操作回执。Work API 本身还要求工具调用者具有工作区写入权限；直接 HTTP 调用不会获得额外的 Harness 会话审批，它属于宿主可信 API 边界。

宿主可以在每个 binding 中用 `allowed_tools` 缩小可发现和可调用的工具集合。组织不匹配的用户看不到绑定；只读用户无法通过 Work API 发起工具调用。未知工具、冲突绑定、没有对应写入者的配置、重复连接都被拒绝。

## 验证

```bash
/path/to/navigator/.venv/bin/python tools/verify-navigator.py \
  --navigator-root /path/to/Cyrene-Navigator \
  --plugin "$PWD/dist/cyrene-pilot"
```

验证使用未修改的 Navigator app factory、真实 Work API 和独立进程中的 Go gRPC 服务，覆盖工具发现、脱敏任务提交、状态轮询、未登录、只读与跨组织调用、工具白名单和连接隔离。该验证没有启动 LLM Agent 会话，也不证明 GPU 训练效果。

三个业务流程仍需主动触发；此版本补齐的是 Navigator 工具发现和调用入口，不增加训练失败监听或自动部署门禁。
