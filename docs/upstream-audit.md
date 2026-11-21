# 上游能力核对

核对日期：2026-11-21。范围为下列公开仓库的默认分支及其源码、契约、测试。“缺口”指在这些修订中未找到对应完整流程，不推断私有代码或未来版本。

| 仓库 | 修订 |
| --- | --- |
| Cyrene-Echo | `5db51ca2971e9aa8b01a682ecb215eea38ef303c` |
| Cyrene-Catalyst | `1593216baebcf82f1c3bc230a3b3e2392a20192d` |
| Cyrene-Yield | `7b38e2c2278a252c89a9136aa9a99eb8e91d5427` |
| Cyrene-Exchange | `12fdd8966d061aadb335ae8f9a489a2c923f34e7` |
| Cyrene-Navigator | `4d3ed51852771f4a77f91b79a9142f137740cbb4` |
| Cyrene-Client | `7dac25e77b2a3bb98c7e484550d4d3a9c15a4897` |
| Cyrene-Plugins-Official | `b4df24066b8ae465185253081707437a536378c0` |

## 回放评测

Echo 已有评分、门禁和反馈集。其[评估输入处理](https://github.com/DoHorizon-AI/Cyrene-Echo/blob/5db51ca2971e9aa8b01a682ecb215eea38ef303c/src/cyrene_echo/lifecycle.py#L246)要求提供实际回答；缺失 `actual` 时跳过样本。[评分适配器](https://github.com/DoHorizon-AI/Cyrene-Echo/blob/5db51ca2971e9aa8b01a682ecb215eea38ef303c/src/cyrene_echo/plugin_evaluation.py#L61)读取既有记录交给评分插件。[双答案评审](https://github.com/DoHorizon-AI/Cyrene-Plugins-Official/blob/b4df24066b8ae465185253081707437a536378c0/plugins/evaluation/llm-judge/llm_judge.py#L343)也读取已提供的 A/B 回答。

增量是自动向两个目标模型发送冻结问题，逐题保存回答、耗时和实际用量，再复用 Echo 的 Product Run/Result/Gate。插件不增加另一套评分算法；逐题 exact-match 变化只是辅助诊断。

## 隐私处理

官方数据准备已有[精确重复、回答冲突和近似重复检测](https://github.com/DoHorizon-AI/Cyrene-Plugins-Official/blob/b4df24066b8ae465185253081707437a536378c0/plugins/tools/dataset-preparation/training_curation.py#L1525)。Catalyst 的[敏感字段遮盖](https://github.com/DoHorizon-AI/Cyrene-Catalyst/blob/1593216baebcf82f1c3bc230a3b3e2392a20192d/src/cyrene_catalyst/logging.py#L111)作用于日志属性。在数据准备、生成、校验及 Catalyst 源码中未找到样本文本隐私扫描、稳定替换、脱敏报告到复核草稿的完整流程。

增量是对实际训练文本执行范围明确的隐私识别，输出待审核脱敏稿。接入 Catalyst 时复用已有 `training-records:edit` 的 `remap`，不调用 `approve`，不改变 Product 的审核权威。

## 失败修复

Yield 已有[诊断分页](https://github.com/DoHorizon-AI/Cyrene-Yield/blob/7b38e2c2278a252c89a9136aa9a99eb8e91d5427/training/core/src/cy_exec/training/product_service.py#L678)、预检及静态 remediation。Client 已有[确认旧执行终止后的有限重试](https://github.com/DoHorizon-AI/Cyrene-Client/blob/7dac25e77b2a3bb98c7e484550d4d3a9c15a4897/packages/run-control/service.ts#L382)。未找到汇总诊断后产生参数变更方案、创建新草稿、限制启动预算并回读新运行的完整流程。

增量是一次有界修复周期。v0.2 只对有完整证据的 CUDA OOM 自动提出可执行参数方案；其他分类返回人工处理建议。AI 解释不能改变执行白名单或增加预算。

Yield 的 `PATCH /training-drafts/{id}` 校验制品并保存准备配置；完整运行预检由 Yield 在提交后的执行路径负责。插件不声称存在未启动草稿的完整 GPU dry-run API。

## 接入边界

- Echo 适配器使用现有 `/api/v1/evaluation-runs`，要求与 Echo 共用同一 UID 可读写的本地或共享 CAS 挂载。当前插件没有跨节点制品传输器。
- Yield 适配器使用公开 legacy Product API，适用于独立部署的单工作区服务；宿主必须登记允许诊断的 Run ID，插件还核对源草稿工作区。它不会绕过受保护的 workspace 私有接口。多租户部署应先补齐受授权的诊断与修复 API 适配。
- Catalyst 写入前核对版本、完整记录覆盖和每条原始记录；一次最多 200 条变更。插件返回新版本回执，不把草稿等同于已审核训练集。
- 原生 `tool.provider.v1` 通过真实 gRPC 传输验证。当前上游 Navigator 默认启动配置未自动登记本插件；宿主仍需提供工具绑定。v0.3 提供自有 Work 桥接启动器，通过公开 app factory 扩展登记工具，不依赖未发布的上游补丁；会话签名客户端的固定白名单仍保持原样。
- 官方 manifest schema 没有 `go` 枚举。清单使用合法的 `mixed`，对应 Go 服务和 Python 准备脚本；实际业务逻辑全部为 Go。Platform 需要宿主信任并登记本仓库的静态二进制准备器。

## 验证分层

Go 测试中的模型、Yield 和 Catalyst 是协议夹具，覆盖状态机、预算、失联恢复与错误边界。`tools/verify-echo.py` 使用真实 Echo 和官方 exact-match 插件，模型回答来自明确标记的模拟端点。当前开发环境没有 GPU；生产部署可以连接 GPU 模型端点及 Yield GPU 执行环境。GPU 训练修复效果、真实模型质量、吞吐量和多租户集成需要目标环境验收。
