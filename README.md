# JVM 监控工具（jvm-node）技术文档

## 项目概述
这是一个基于 Prometheus 的 JVM 监控工具，用于收集和导出 Java 虚拟机的 GC（垃圾回收）相关指标。

## 主要功能
1. **JVM进程发现**
   - 自动发现并监控系统中运行的 Java 进程
   - 支持进程过滤功能
   - 自动清理已停止进程的监控数据

2. **GC指标收集**
   - 收集新生代（Young Generation）使用情况
   - 收集老年代（Old Generation）使用情况
   - 收集元空间（Metaspace）使用情况
   - 监控 GC 次数和时间

3. **应用标识**
   - 支持通过 JVM 参数自动识别应用名称
   - 可配置多个应用名称标识参数
   - 默认支持 Spring Boot 应用名称识别

## 配置说明
```yaml
listen_port: 9101            # 监听端口
scrape_interval: 30s         # 采集间隔
jstat_timeout: 5s            # jstat命令超时时间
max_monitored_processes: 1000 # 最大监控进程数
max_concurrent_scrapes: 50    # 最大并发采集数
jstat_path: jstat           # jstat命令路径
log_level: info             # 日志级别
pid_filter: ""              # 进程过滤表达式
app_name_labels:            # 应用名称标识参数列表
  - "-Dapp.name"
  - "-Dapp"
  - "-Dspring.application.name"
```

## 监控指标
### 容量指标（Gauge）
- `jvm_gc_s0_capacity_bytes`: S0区容量
- `jvm_gc_s1_capacity_bytes`: S1区容量
- `jvm_gc_eden_capacity_bytes`: Eden区容量
- `jvm_gc_old_gen_capacity_bytes`: 老年代容量
- `jvm_gc_metaspace_capacity_bytes`: 元空间容量
- `jvm_gc_compressed_class_capacity_bytes`: 压缩类空间容量

### 使用量指标（Gauge）
- `jvm_gc_s0_usage_bytes`: S0区使用量
- `jvm_gc_s1_usage_bytes`: S1区使用量
- `jvm_gc_eden_usage_bytes`: Eden区使用量
- `jvm_gc_old_gen_usage_bytes`: 老年代使用量
- `jvm_gc_metaspace_usage_bytes`: 元空间使用量
- `jvm_gc_compressed_class_usage_bytes`: 压缩类空间使用量

### GC统计指标（Counter）
- `jvm_gc_young_gc_count`: Young GC次数
- `jvm_gc_young_gc_time_seconds`: Young GC总时间
- `jvm_gc_full_gc_count`: Full GC次数
- `jvm_gc_full_gc_time_seconds`: Full GC总时间
- `jvm_gc_total_gc_time_seconds`: GC总时间

## 特性
1. **动态配置加载**
   - 支持配置文件热重载（SIGHUP信号）
   - 自动创建默认配置文件

2. **高性能设计**
   - 使用协程池并发采集
   - 内存复用（Buffer Pool）
   - 高效的指标存储和更新

3. **可靠性保障**
   - 超时控制
   - 错误处理和日志记录
   - 进程存活性检查

## API接口
- `/metrics`: Prometheus指标接口
- `/health`: 健康检查接口

## 使用建议
1. 合理设置采集间隔和超时时间
2. 根据系统规模调整最大监控进程数和并发采集数
3. 使用进程过滤功能减少无关进程的监控
4. 配置合适的应用名称标识参数以便于识别应用
