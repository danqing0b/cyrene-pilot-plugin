# v0.1 试点发布兼容模块

完整的旧版配置继续启用 `release_prepare`、`release_status` 和原 `/admin/releases/` 路由。未提供 `candidates` 时，这个模块不启动，也不要求 Reactor、Exchange 或试点推理凭据。现有 `release-pilot.db` 不迁移；新增工作流使用独立的 `automation.db`。

旧模块比较预先导入的 100 条回答，执行受控部署，在操作员确认后更新本地 `release-pilot` 代理入口，并观察 60 秒。它只影响经过该入口的请求；发布结束后不继续进行长期质量监测。探测检查可达性、模型别名和非空内容，不证明业务回答正确。

`tools/demo.py` 和 `navigator.binding.example.json` 是旧实验材料，依赖未随本仓库发布的 Navigator 桥接及 Platform 验收入口。它们不属于 v0.2 的可独立复跑验收。新功能使用 README 中的标准 gRPC 客户端和 `tools/verify-echo.py`。

本仓库保留 `cyrene.tools.release-pilot` 插件 ID、二进制入口名称和旧环境变量，避免破坏已有私有试点安装。展示名称为 Cyrene Pilot，版本为 0.2.0。
