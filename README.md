# JVM GC Collector 操作手册

## 1. 简介
一个用于采集 Java 虚拟机（JVM）垃圾回收（GC）指标的轻量级 Exporter。它通过调用 `jstat` 命令获取数据，并将其转换为 Prometheus 格式，便于监控系统抓取。

**适用环境**：Linux 
**架构支持**：AMD64 (x86_64)
## 2. 部署步骤

### 2.1 上传二进制文件
二进制文件上传到服务器：
- **x86_64 (Intel/AMD)**: 使用 `jvm_gc_collector_centos7_2026`


### 2.2 赋予执行权限
```bash
chmod +x jvm_gc_collector_centos7
```

### 2.3 首次运行生成配置
首次运行程序会自动在当前目录生成默认配置文件 `config.yml`：
```bash
./jvm_gc_collector_centos7
```
*注意：首次运行提示 `pid_filter is empty`，这是正常现象，继续下一步配置。*

## 3. 配置说明

编辑生成的 `config.yml` 文件：

```yaml
listen_port: 9101              # Exporter 监听端口
scrape_interval: 30s           # 采集间隔(prometheus如果15s采集一次,只会采集到上一次的数据)
jstat_timeout: 5s              # jstat 命令超时时间
max_monitored_processes: 1000  # 最大监控进程数(修改为5)
max_concurrent_scrapes: 50     # 最大并发采集数(修改为5)
jstat_path: ""                 # jstat 路径，留空默认使用系统环境变量中的 jstat
log_level: "info"              # 日志级别
pid_filter: "tomcat"           # 【必填】进程过滤关键字。例如 "tomcat", "java_app"。支持热加载，修改后5秒生效。
log_path: "jvm_gc_collector.log" # 日志文件路径
max_log_size: 200              # 日志最大大小 (MB)，超过后会备份并重置
```

**关键配置项**：
*   **`pid_filter`**: **必须填写**。程序会通过 `ps -ef | grep <keyword>` 来查找目标 Java 进程。如果留空，程序将拒绝启动。
*   **`max_log_size`**: 日志文件限制为 200MB。当达到限制时，旧日志会被重命名为 `.old`（仅保留一份备份），新日志继续写入，**不会**无限增长占用磁盘。

## 4. 启动与验证

### 4.1 启动程序
配置完成后，再次启动程序：
```bash
./jvm_gc_collector_centos7
```
或者后台运行：
```bash
nohup ./jvm_gc_collector_centos7 > /dev/null 2>&1 &
```

### 4.2 验证指标
访问 Metrics 接口验证数据：
```bash
curl http://localhost:9101/metrics
```
应能看到 `jvm_gc_` 开头的指标，例如：
*   `jvm_gc_young_gc_count`: Young GC 次数
*   `jvm_gc_full_gc_count`: Full GC 次数
*   `jvm_gc_old_gen_usage_bytes`: 老年代使用量

## 5. 常见问题排查

**Q1: 日志报错 `failed to execute jstat ... exit status 1`**
*   **原因**：权限不足。采集器用户与目标 Java 进程用户不一致。
*   **解决**：请确保采集器以 **启动 Java 进程的相同用户**（或 root）身份运行。

**Q2: 报错 `pid_filter is empty`**
*   **原因**：`config.yml` 中未配置 `pid_filter`。
*   **解决**：编辑配置文件，填入能唯一匹配目标进程的关键字。

**Q3: 找不到 `jstat` 命令**
*   **解决**：在 `config.yml` 的 `jstat_path` 中填写 jstat 的绝对路径（如 `/usr/java/jdk1.8/bin/jstat`），或确保将其加入系统 PATH。
