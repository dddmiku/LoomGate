# 模型与协议体检

脚本 gateway/tests/provider_healthcheck.py 读取实际模型目录、能力字段与已有调用权限。调用凭据只在进程内保存，不新建密钥，不记录正文、工具参数或凭据。默认只生成目录和请求计划；推理必须显式增加 --run。

本轮按用户要求排除豆包。复现命令必须保留 --exclude-provider doubao，此前已完成的豆包测试作为范围外结果单独保留，不计入当前通过率。

## 隔离双实例

provider_health_run.py 对候选版本的每个模型运行 Chat、Responses、Anthropic Messages 与 Gemini 四协议基础请求；基线对每个模型运行 Chat，并对每条路由的代表模型补齐其余协议。相同 case 使用相同短问题，结果按 trial 分开。

    python3 -I -B gateway/tests/provider_health_run.py --root /root/loomgate-performance-check --output /root/loomgate-performance-check/provider-study --exclude-provider doubao --inventory-only

    python3 -I -B gateway/tests/provider_health_run.py --root /root/loomgate-performance-check --output /root/loomgate-performance-check/provider-study --exclude-provider doubao --run --max-calls 700

两个副本分别从 baseline/config.json、candidate/config.json 读取私有测试密钥，不能把原始密钥写进命令行。默认端口为 17873、17874。原服务的账号配置、调用权限、历史账本与生产端口不变；隔离任务引擎为空且自动计划停用，只调用模型生成端点。

整体并发最多 3，同一路由最多 1，每个请求最多 90 秒；不自动重试，不替换模型，不修改上游权限。连续三次认证或配额拒绝会暂停该路由。control.json 可用 paused_routes 或 stop_dispatch 停止新派发；SIGTERM 同样只停止派发并等待在途请求结束。

进程锁阻止两个 runner 同时执行同一计划；网络调用前先持久化 attempt，随后写结果 journal。恢复时保留已完成项；开始过却没有结果的请求标为 unknown，不自动重发。修复后的明确重测写入另一个输出目录，不覆盖故障证据。

## 通过条件

- 基础与流式文本：HTTP 成功、协议正常终态、有可见正文，并准确返回本次短标记和算术结果。只有思考、空正文、截断或安全拒绝不算通过。
- 工具：tools 模式要求 health_echo 调用及严格匹配的参数；tool_loop 模式继续把同一调用标识及合成工具结果按原协议交还给同一模型，最终正文必须精确返回结果里的短标记。闭环包含两次明确生成调用，工具执行只是本地合成回显，不调用外部业务。
- 多轮：携带用户、助手与后续用户的连续历史，答案必须精确恢复第一轮给出的标记；仅含标记子串不算通过。
- 图片：只对目录声明支持图片的代表模型运行；使用不含文字的红、蓝两张纯色 PNG，四协议均需辨认正确。单次猜中红色不足以通过该维度。

证据仅记录模型、trial、协议、模式、HTTP 状态、公开错误码、TTFB、首事件/正文时间、总耗时、事件数、字符数、数字用量及语义验证布尔值。原始错误仅在诊断进程内分类，不进入共享报告。

## 权限与环境边界

模型出现在目录中，不等于当前账号有调用权限。WorkBuddy 上游 11158 明确表示账号的模型权限不足，网关返回 HTTP 403 和 upstream_model_access_denied，保留业务码与请求标识。不能通过换模型、改推理预算或惩罚账号掩盖；需要具备权限的上游账号后才能验收该模型生成能力。

用户 VPS 的旧过渡实例与 hwcloud dev 实例不是同一部署。报告必须标明环境，共享调用密钥不能证明是同一实例。

单模型单次延迟受上游负载、缓存、网络和模型思考影响。交错取样只能减小部分波动，不能单独证明全部模型速度或质量完全不变。网关本地 CPU 基准、协议回归与真实生成结果应分别报告。
