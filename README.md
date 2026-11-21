# Cyrene Pilot

**面向 Cyrene 的 Go 原生自动化插件：模型回放评测、训练数据隐私处理、失败诊断与有界修复。**

通过 `tool.provider.v1` / gRPC `DirectPluginRuntime` 提供工具，复用 Echo、Catalyst 和 Yield 的现有资源与执行能力。插件负责采集输入、编排操作和保存证据，Product 服务保留评分、审核及训练执行权威。

开发验证环境没有 GPU，生产环境可以连接 GPU 推理服务和 Yield GPU 训练环境。模拟验证与真实模型效果验证分别记录。

## 能解决什么问题

| 功能 | 自动完成的工作 | 实际场景 |
| --- | --- | --- |
| 回放评测 | 固定题库 → 调用基线/候选 → 保存逐题回答、用量及耗时 → Echo 评分 → 生成差异报告 | 更新客服模型或提示词后，自动发现回答退化，无需人工准备两套 `actual` 文件 |
| 数据隐私处理 | 本地规则遮盖 → 可选 AI 实体识别 → 脱敏稿及位置报告 → 可选 Catalyst 新版本交接 | 将客服聊天整理成训练数据时，处理手机号、邮箱、身份证号和凭据，保留待审核稿 |
| 运行修复 | 读取失败日志和配置 → 诊断 → 生成绑定摘要的方案 → 创建/准备新草稿 → 在宿主预算内启动及回读 | CUDA OOM 后降低微批量、调整梯度累积，保存变更理由与执行回执 |

上游已经具备数据去重、LLM 评分、日志诊断及有限重试。三项功能的增量及检查修订见 [上游源码核对](docs/upstream-audit.md)。

## 构建

需要 Go 1.25+、Python 3 和用于竞态检测的 C 编译器。当前打包支持 Linux amd64 / arm64。

```bash
tools/build.sh
```

脚本运行 `go vet` 和 `go test -race`，然后生成 `dist/` 下的 ZIP、Platform descriptor 和 `pilotctl`。可以使用 `--output DIRECTORY` 指定输出位置，或设置 `CYRENE_GO`、`CYRENE_PYTHON` 选择工具链。GitHub Actions 使用同一构建入口并保存构建产物。

## 配置与启动

从 [配置示例](examples/automation.config.json) 开始，修改服务地址、资源 ID 和宿主路径。示例题库见 [questions.jsonl](examples/questions.jsonl)。配置契约为 [configuration.schema.json](configuration.schema.json) 和 [automation.schema.json](automation.schema.json)。

- 模型必须在 `automation.models` 登记固定服务 origin、模型名、版本、可选 `systemPrompt`、令牌文件与调用上限。请求只接受注册 ID。模型服务使用 `/v1/chat/completions`，可指向 Exchange 或兼容服务。
- 数据文件必须登记绝对路径及实际字节的 `sha256:` 摘要。文件发生变化后，需要更新配置并创建新任务键。
- 令牌从权限 `0600` 的私有文件读取。配置和存储目录应放在宿主私有目录。
- Echo 需要已有 suite：`expectedField=reference`、`actualField=actual`，并配置对应 engine binding。插件核对 suite 快照、输入摘要、完整覆盖及 Run/Result/Gate 关联。Echo 和插件必须共享 `artifactRoot` CAS 挂载及文件访问身份。
- `yield.maxRepairStarts` 默认 `0`，禁止启动修复训练；生产环境由宿主明确配置预算、`maxSteps` 和允许访问的 Run ID。当前 Yield 适配器面向独立部署的单工作区服务。

计算数据摘要：

```bash
sha256sum /srv/cyrene-pilot/questions.jsonl
```

可由 Platform Package Runtime 使用 ZIP、descriptor 和宿主信任的 [静态准备器](tools/prepare-static-go.py) 安装。业务服务为静态 Go 二进制；manifest 的 `mixed` 表示 Go 服务与 Python 准备脚本的组合，兼容当前上游的语言枚举。

也可以直接启动以验证标准协议：

```bash
mkdir -p dist
go build -o dist/cyrene-pilot .
go build -o dist/pilotctl ./cmd/pilotctl
install -d -m 700 /srv/cyrene-pilot/state
export CYRENE_CAPABILITY_BINDING_ID=pilot
export CYRENE_PLUGIN_STORAGE_PATH=/srv/cyrene-pilot/state

dist/cyrene-pilot \
  --capability tool.provider.v1 \
  --interface-version 1 \
  --listen 127.0.0.1:0 \
  --config /srv/cyrene-pilot/config.json
```

第一行标准输出包含 `connection_ref`；gRPC 仅绑定回环或 Unix socket。生产宿主应隔离本地进程并按绑定授权调用。HTTP 管理接口仅在回环监听，除 `/healthz` 外需要操作员 Bearer。

当前公开 Navigator 默认启动配置尚未自动登记此插件。接入 Navigator 需要宿主提供标准工具绑定，不能直接使用旧实验中的专用桥接配置。以下 `pilotctl` 可直接调用原生协议。

## 调用工具

将启动返回的值设置为 `PILOT_CONNECTION_REF`。绑定名应与启动配置一致。

```bash
dist/pilotctl --connection "$PILOT_CONNECTION_REF" --binding pilot

dist/pilotctl --connection "$PILOT_CONNECTION_REF" --binding pilot \
  --tool eval_replay_start <<'JSON'
{"datasetId":"support","baselineId":"baseline","candidateId":"candidate","idempotencyKey":"support-regression-001"}
JSON
```

启动工具返回任务 ID。使用 `pilot_job_status` 查询进度；参数可由标准输入或 `--args-file` 提供。

| 工具 | 参数 | 行为 |
| --- | --- | --- |
| `eval_replay_start` | `datasetId, baselineId, candidateId, idempotencyKey` | 异步采集双方回答并交由 Echo 评分 |
| `data_guard_start` | `datasetId, idempotencyKey` | 异步扫描，生成脱敏稿和报告 |
| `run_doctor_start` | `runId, idempotencyKey` | 异步诊断并保存修复方案，不启动训练 |
| `pilot_job_status` | `jobId` | 查询状态、操作回执、摘要及制品摘要 |
| `pilot_guard_apply` | `jobId, planDigest` | 核对 Catalyst 原始快照，创建脱敏子版本，保留待审核状态 |
| `pilot_doctor_prepare` | `jobId, planDigest` | 按已保存的方案创建新 Yield 草稿并准备配置 |
| `pilot_doctor_start` | `jobId, planDigest` | 在宿主授权预算内启动准备好的修复草稿 |
| `pilot_doctor_check` | `jobId` | 回读修复运行，或核对响应丢失后的草稿到运行关联 |

未配置对应 Product 的工具不会出现在目录中。写入动作的 `planDigest` 必须与任务返回值相同；模型建议不能改变预算或执行任意命令。

HTTP 管理接口也支持 `POST /admin/tools/{tool}`、`GET /admin/jobs/{id}` 和 `GET /admin/jobs/{id}/artifacts/{name}`。制品名来自任务的 `artifacts`，接口不接受任意文件路径。

### 回放评测

题库采用 JSONL，每条包含唯一 `sampleId`、非空 `prompt` 和字符串 `reference`。双方使用同一冻结题库及固定生成参数。结果包括 `baseline.jsonl`、`candidate.jsonl` 和 `comparison.json`。

`accepted` 表示候选通过 Echo 门禁且分数不低于基线。模型名及宿主登记的 revision 会记录到回答中；revision 是宿主声明，不等同于远端权重校验。报告中的延迟为本次请求耗时，不能直接当作生产性能基准。

### 隐私处理与 AI

本地规则覆盖邮箱、中国大陆手机号、18 位身份证形状、部分 API key / Bearer / PEM 私钥，以及敏感字段。使用私有随机 HMAC 盐生成一致替换标记，报告不包含原始匹配值。

可选配置 `dataGuard.aiModelId` 和 `allowTextToAI=true`。插件先遮盖已知敏感值，再将剩余文本交给登记模型；严格验证人名、地址、出生日期的 Unicode 字符区间。`maxAICalls` 限制调用数量。未开启时只运行本地规则。

产物为 `sanitized.jsonl` 和 `guard-report.json`，状态始终为 `PENDING`。规则与 AI 都存在漏检可能，训练集发布仍需审核。

要交接 Catalyst，注册文件必须是完整的目标 revision 快照，每行包含 `recordId` 和 `rawRecord`，并配置该数据集的 `catalyst.datasetId/revisionId/resourceVersion` 及全局 Catalyst 连接。插件核对远端原始记录，调用已有 `remap` 创建子版本；一次最多 200 条变更，不自动批准记录。

### 诊断与修复

诊断分类包含 CUDA OOM、磁盘空间、环境、数据及授权错误。当前可执行修复仅覆盖完整证据下的 CUDA OOM：降低每设备微批量，通过梯度累积保持有效批量，并限制新运行步数。微批量已为 1、诊断不完整或错误类型不支持时，返回人工处理建议。

可配置 `yield.aiModelId` 与 `allowDiagnosticsToAI=true`，让 AI 解释经本地规则遮盖的日志。AI 输出只作建议；可执行参数由确定性规则产生。

`pilot_doctor_prepare` 复用 Yield 的制品校验与草稿准备。完整 GPU 预检由 Yield 的执行路径负责。源运行、工作区、配置和方案摘要发生变化时拒绝启动。预算按已派发启动记账，同一失败源最多派发一次修复；结果未知的启动也占用预算。

修复运行完成后标记 `COMPLETED_REQUIRES_EVALUATION`，需要再做模型评测确认效果。

## 持久化与恢复

任务、逐次派发意图和回执保存在私有 `automation.db`，结果文件位于私有 `artifacts/`。同一工具的同一幂等键复用原任务；键绑定了不同参数或宿主配置时返回冲突。

模型调用、Catalyst 修订和训练启动发生失联时保留 `UNKNOWN`，不自动重复收费或重复启动。可通过 Product 回读核对修复启动。Echo Run 创建和 Yield 草稿创建只在已检查的 Product 幂等契约下复用操作。

任务状态为 `QUEUED / RUNNING / SUCCEEDED / FAILED / NEEDS_ATTENTION`。`SUCCEEDED` 表示工作流完成；是否通过评测、是否仍待审核及修复结果分别见摘要。队列当前使用单 worker；账本达到 `maxJobs` 后拒绝新增，需宿主归档或调整容量。

## 验证

```bash
go test -race ./...
```

自动测试覆盖回答采集、退化拦截、Echo 证据不匹配、Unicode 脱敏、发送 AI 前遮盖、Catalyst 快照与幂等边界、训练预算、失联恢复、方案过期及原生 gRPC。测试中的模型、Yield 和 Catalyst 为明确的协议夹具。

与真实 Echo 和官方 exact-match 插件复跑：

```bash
python3 tools/verify-echo.py \
  --echo-python /path/to/Cyrene-Echo/.venv/bin/python \
  --plugins-root /path/to/Cyrene-Plugins-Official \
  --plugin "$PWD/dist/cyrene-pilot" \
  --pilotctl "$PWD/dist/pilotctl" \
  --report /tmp/pilot-echo-report.json
```

Echo 的 Python 环境需安装其锁定依赖和公开的 `cyrene_plugin_runtime` SDK。该脚本启动真实 Echo、官方评分器、已编译 Go 插件和原生客户端；模型推理为模拟端点，报告明确标记 `productionGPU=NOT_RUN`。真实模型质量、GPU 修复效果和吞吐需在生产目标环境验收。

## 兼容性与许可证

版本为 **0.2.0**。保留 `cyrene.tools.release-pilot` 插件 ID、原 Package Runtime 二进制入口和 `CYRENE_RELEASE_PILOT_CONFIG_FILE` 环境变量；也支持 `CYRENE_PILOT_CONFIG_FILE`。完整旧配置可继续启用 [试点发布兼容模块](docs/legacy-release.md)，新增功能不依赖它。

本次实现不携带其他 Cyrene 仓库的私有集成补丁。插件源码与附带 Protobuf 契约使用 [Apache-2.0](LICENSES/Apache-2.0.txt)，仓库保留所有者维护的 [LICENSE](LICENSE)。
