# DEVELOP — 开发规范（easyops）

本仓是 `easy-vcs/agent-gateway` 中 K8s/BuildKit/Helm 运维能力的抽取，并升级为
**「库 + 服务」两层**（与 easyvcs / artifact 并列的第三个 server）。遵循 easy-vcs
org 的开发基准（见 `easy-vcs/easyvcs@main` 的 `DEVELOP.md`）以及 `agent-gateway`
的 `DEVELOP.md`。**不得放宽** org 基准的硬性规定。

## 1. 文件长度（硬性）

**一个代码文件尽量不超过 500 行；最多不超过 1000 行。**

- 超过 500 行：同一次改动中拆分，或在 MR 里说明为何暂不拆。
- 超过 1000 行：不得合入（reviewer 应拒绝），除非在 §4「例外」登记。
- 例外（不计入）：自动生成的文件（`gen/**`、`*.pb.go`、`*_connect.go`）、lock
  文件、纯数据/清单文件。测试文件同样适用。
- 拆分按职责，不按行数机械切；Go 同包内拆多个文件即可。

## 2. 两层结构与依赖规则（硬性）

- **库层**：`internal/*`（`runtimeprofiles`/`sandboxmgr`/`servicesmgr`/`k8swatch`/
  `k8scheck`/`imagebuild`/`helmmgr`/`sandboxreaper`/…）。纯 Go 库，**不得** import
  `connectrpc`，也不得 import 业务包。
- **服务层**：`cmd/easyops` + `gen/easyops/v1` + 服务适配层 `internal/opssvc`。
  **只有这三个位置**允许 import `connectrpc` / protobuf 生成物。

**红线（任何位置都不得 import）**：`gen/workspace/**`、`internal/roles`、
`internal/easyvcs`、`internal/agentclient`、`modernc.org/sqlite`、`nats-io`、
`abc-protocol`。提交前用 §6 自查。

## 3. 导出 API 契约（硬性）

`internal/*` 库包的导出符号与签名是 **契约**（gateway 依赖，后由 OpsService 暴露）。
迁移/重构**不得改动导出符号与签名**，只能改 import path。

- 迁移映射：`internal/<pkg>` 留在本 module `github.com/easy-platform/easyops`。
- 跨包类型（如 `sandboxmgr.Spec.Runtime` 的 `runtimeprofiles.Rendered`）随所依赖
  的包**一起留在同一 module**，保持同仓引用，不复制、不重声明。
- `OpsService` 的 proto 字段/RPC 一经发布不得随意改动（gateway 依赖它）。

## 4. 例外登记

| 路径 | 行数 | 理由 |
|---|---|---|
| `gen/**` | — | protobuf / Connect 生成物（`buf generate` 产出），自动生成，不计入 |

## 5. 现状与迁移记录

### 5.1 已迁入（库层）

| 目标路径 | 来源（agent-gateway@main） | 说明 |
|---|---|---|
| `internal/runtimeprofiles/*` | 同名 | 逐字节迁移 |
| `internal/sandboxmgr/*` | 同名 | 仅改 import path（`runtimeprofiles`/`k8scheck`） |
| `internal/k8scheck/*` | 同名 | 逐字节迁移（Phase 1 依赖闭包，见 §5.2） |
| `internal/servicesmgr/*`（16 文件） | 同名 | `services.go`/`preflight.go` 仅改 import path；其余逐字节 |
| `internal/k8swatch/*` | 同名 | 逐字节迁移 |
| `internal/imagebuild/*` | 同名 | 逐字节迁移（Phase 3） |
| `internal/helmmgr/*` | 同名 | 逐字节迁移（Phase 3） |
| `internal/sandboxreaper/*` | 同名 | Phase 4；`Job` 本地化 + `WorkerLister` 接口注入（见 §5.2），逻辑等价 |

全部 ≤500 行；`gofmt -l .` 为空，`go build/vet/test` 全绿。

### 5.2 依赖闭包取舍（import-graph finding）

Phase 1 任务书原写「只迁 `sandboxmgr` 与 `runtimeprofiles`」，但 `sandboxmgr` 的
`preflight.go` import 了 `internal/k8scheck`，不迁则编译不过，故把 `k8scheck` 作为
**最小依赖闭包**一并迁入。Phase 2/3 的 `servicesmgr`、`imagebuild`、`helmmgr` 无
新增仓外依赖（`helmmgr` 引入 `helm.sh/helm/v3`）。

Phase 4 的 `sandboxreaper` 依赖 `internal/sandboxmgr`（已在本仓）与
`internal/workerclient`。**`workerclient` 不干净**：它 import `connectrpc` 与生成的
`gen/worker/v1`（worker 协议），若迁入会破坏「库层不得 import connectrpc」的红线。
故**不迁 `workerclient`**，而是：

- `sandboxreaper` 保持纯库：把 worker 交互做成接口（`WorkerLister`）注入，
  并把所需的 `Job` 类型在包内**本地化**（仅 `State` / `FinishedAt` 两个字段）；
- 具体 worker 客户端放在**服务层** `internal/opssvc/workerclient.go`（JSON codec
  调 `/worker.v1.WorkerService/ListJobs`），由 `cmd/easyops` 通过 `NewWorker` 注入。

`NewWorker` 为 nil 时 `sandboxreaper` **不回收任何沙箱**（宁可漏收，不可误杀）。

### 5.3 服务外壳（Phase 4.5，全量面）

| 路径 | 说明 |
|---|---|
| `proto/easyops/v1/ops.proto` | `easyops.v1.OpsService` 契约（sandbox / services / pvc / helm / image / live） |
| `proto/buf.yaml` / `proto/buf.gen.easyops.yaml` | buf 配置（local 插件，离线可复现） |
| `scripts/gen-proto.sh` | 重新生成 `gen/easyops/v1`（生成物入库） |
| `gen/easyops/v1/**` | 生成的 pb + Connect 绑定 |
| `internal/opssvc/{service,sandbox,watch,services,pvc,helm,image,convert,workerclient,auth}.go` | 服务适配层：RPC↔库、owner 打标/过滤、服务令牌校验、参数校验；**不含策略**。每个 backend 可缺省（对应 RPC 返回 `CodeUnavailable`）。`watch.go` 把 sandbox/service/PVC 三类 watch 源汇聚成一条 `WatchWorkspace` 流，逐帧置位 `*_changed` |
| `internal/opssvc/{service,sandbox,watch,services,pvc,helm,image,convert,workerclient,auth,errs}.go` | 服务适配层：RPC↔库、owner 打标/过滤、服务令牌校验、参数校验；**不含策略**。每个 backend 可缺省（对应 RPC 返回 `CodeUnavailable`）。`watch.go` 把 sandbox/service/PVC 三类 watch 源汇聚成一条 `WatchWorkspace` 流，逐帧置位 `*_changed`。`errs.go` 的 `mapK8sErr` 把 k8s 后端错误映射为语义化 Connect code（见 §5.5） |
| `cmd/easyops/main.go` | 服务进程：按 env 构造 backend（sandbox/services/helm/image）+ idle reaper + `OpsService` + `/healthz` + `/readyz` |
| `internal/opssvc/sandbox_logs.go` | `SandboxLogs`/`WatchSandboxLogs`：owner 作用域的沙箱容器日志（沙箱由 easyops 创建，只有 easyops 的 SA 能读其 pod）；无输出时回填 pod 诊断 + k8s events（events 为 best-effort，缺 `events` 读权限则省略） |
| `Dockerfile` / `build-image.sh` | 服务镜像 |
| （平台清单） | **不在本仓**：Deployment/Service/Secret、SA + worker-ns Role/RoleBinding 已迁 **`easy-vcs/deploy`**（chart `easyvcs-platform`，见 `templates/easyops.yaml`、`templates/rbac.yaml`） |

helm / image RPC 的 repo tarball 由**请求携带**（`archive` bytes），easyops 不访问 VCS。

### 5.4 依赖

`go.mod`：库层用 `k8s.io/api|apimachinery|client-go`、`sigs.k8s.io/yaml`、
`helm.sh/helm/v3`（helmmgr）；服务层加 `connectrpc.com/connect`、
`google.golang.org/protobuf`。**不**带入 sqlite / nats / abc-protocol /
agent-gateway。由 `go mod tidy` 生成。

### 5.5 错误映射（k8s 后端错误）

`internal/opssvc/errs.go` 的 `mapK8sErr` 把后端/k8s 错误映射为**语义化 Connect code**，
使调用方能区分「无权限」与「真故障」（gateway 的错误处理与诊断依赖此）：

| 后端错误 | Connect code | HTTP |
|---|---|---|
| `Forbidden` | `CodePermissionDenied` | 403 |
| `Unauthorized` | `CodeUnauthenticated` | 401 |
| `NotFound` | `CodeNotFound` | 404 |
| `AlreadyExists` | `CodeAlreadyExists` | 409 |
| `Conflict` | `CodeAborted` | 409 |
| 其他 | `CodeInternal` | 500 |

**既有语义化映射保持不变**：参数校验 → `CodeInvalidArgument`；backend 未配置 →
`CodeUnavailable`（503）；`sandboxmgr.ErrExists` → `CodeAlreadyExists`；删除被挂载的
PVC → `CodeFailedPrecondition`；`CreateSandbox` ready 超时 → `CodeDeadlineExceeded`；
`Get*` 的 not-found 仍返回 `{found:false}`（**不是** 404）。

`watch.go` 中**单 kind 的 list 失败不断流**：该列表留空 + log warn，其他 kind 照常推帧
（已由 `watch_forbidden_test.go` 锁定）。

### 5.6 上游 workspace-gateway 运维侧同步（本分支）

同步了上游 `coding-workspace/workspace-gateway` 的一批集群运维侧改进（**只增不改**既有
OpsService 方法签名）：

- **沙箱 `restartPolicy`**：默认 `Never`（原 `Always`）。沙箱退出（OOMKilled / crash）
  后保持 `Failed` 并带可读原因，不再静默重启。`SANDBOX_RESTART_POLICY` 可覆盖。
- **资源拆分默认**：沙箱/服务的 requests 与 limits 各自可分别覆盖（默认 requests
  `500m`/`1Gi`、limits `2`/`4Gi`，Burstable）；显式 `cpu`/`memory` 仍 request == limit。
  Env：`SANDBOX_{CPU,MEMORY}_{REQUEST,LIMIT}` / `SERVICE_{CPU,MEMORY}_{REQUEST,LIMIT}`。
- **OOM 诊断**：`sandboxmgr.Sandbox` 新增 `Restarts`/`Message`；`Sandbox` proto 新增
  `restarts`=9 / `message`=10（诊断首个不健康容器的 reason+message）。`WaitReady`
  对 `Failed` pod **立即**返回原因（不空等超时）。
- **失败服务启动诊断 + 有界等待**：`servicesmgr.WaitReady` 有界轮询 Deployment，
  确定性失败（`ImagePullBackOff`/`CrashLoopBackOff`/`Unschedulable`/`RunContainerError`…）
  立即返回；`DeployService` 透出 `FailedPrecondition` + pod 原因。`ServiceLogs`/
  `WatchServiceLogs` 在容器**无输出**时附带 pod 诊断（`ServiceLogsResponse` 新增
  `pod_phase`/`restarts`/`message`/`available`；`WatchServiceLogsResponse` 终帧新增
  `pod_phase`/`restarts`/`message`）。
- **`WatchWorkspace` 流式 Helm releases**：帧新增 `helm_releases`=8 /
  `helm_releases_changed`=9（只增字段，1..7 不变；帧仍自包含；helm backend 缺省时列表
  空、位 false）。watch 源为 `helmmgr.WatchReleases`（release 状态 ConfigMap）。
- **`ListPVCs` 只读全量**：`ListPVCs` 不再按 owner 过滤（PVC 为只读共享视图）；`GetPVC`/
  `CreatePVC`/`DeletePVC` 与其它资源仍按 owner 过滤。

## 6. 边界自查命令

```sh
# 全传递闭包里混入业务/禁用依赖？（应无输出）
go list -deps ./... | grep -Ei \
  'agent-gateway|gen/workspace|internal/roles|internal/easyvcs|internal/agentclient|modernc.org/sqlite|nats-io|abc-protocol'

# connectrpc 是否泄漏进库包？（应无输出）
go list -deps ./internal/runtimeprofiles ./internal/sandboxmgr ./internal/servicesmgr \
  ./internal/k8swatch ./internal/k8scheck ./internal/imagebuild ./internal/helmmgr \
  ./internal/sandboxreaper | grep connectrpc
```

## 7. 本仓既有约定

- **提交/分支**：功能走分支 + MR 到 `main`；`main` 只经 MR 变更。
- **格式与检查**：`gofmt -l .` 必须为空；`go build ./...`、`go vet ./...`、
  `go test ./...` 必须全绿。
- **proto**：改 `proto/easyops/v1` 后必须 `bash scripts/gen-proto.sh` 重新生成并一并
  提交 `gen/easyops/v1`。
- **镜像**：`Dockerfile` 的 `GOPROXY`/`GOSUMDB`/`APK_MIRROR` 沿用 org 约定（默认
  `GOPROXY=http://artifact.worker.svc.cluster.local/artifacts/go,direct` + `GOSUMDB=off`
  + USTC apk 镜像），可用 build-arg 覆盖；构建走 `bash build-image.sh`（`buildctl` +
  `skopeo`，无本地 docker daemon）。
- **密钥**：绝不提交真实凭据；示例值标注 `CHANGE THIS`。
- **语言**：代码注释用英文；面向人的文档可用中文。

## 8. 后续迁移顺序

Phase 5 `agent-gateway` 改走网络 `OpsService` 并删除库依赖（同时可去掉自身的集群
RBAC）。详见 `README.md`。

## Packages via artifact (not GitHub/public registries)
- Publish (release): curl -X PUT --data-binary @easyops.zip \
    "http://artifact.worker.svc.cluster.local/artifacts/go/upload?name=github.com/easy-platform/easyops&version=vX.Y.Z"
- Consume: GOPROXY=http://artifact.worker.svc.cluster.local/artifacts/go GOSUMDB=off
See easy-vcs/deploy:PUBLISHING.md.
