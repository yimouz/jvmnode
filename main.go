package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

// RollingFileWriter 实现支持大小限制和轮转的日志写入器
type RollingFileWriter struct {
	FilePath    string
	MaxSize     int64
	file        *os.File
	currentSize int64
	mu          sync.Mutex
}

func (w *RollingFileWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.openFile(); err != nil {
			return 0, err
		}
	}

	writeLen := int64(len(p))
	if w.currentSize+writeLen > w.MaxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = w.file.Write(p)
	w.currentSize += int64(n)
	return n, err
}

func (w *RollingFileWriter) openFile() error {
	f, err := os.OpenFile(w.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.file = f
	info, err := f.Stat()
	if err == nil {
		w.currentSize = info.Size()
	}
	return nil
}

func (w *RollingFileWriter) rotate() error {
	// 关闭当前文件
	w.file.Close()

	// 备份文件名为 .old
	backupName := w.FilePath + ".old"

	// 如果存在旧的备份，直接覆盖
	os.Remove(backupName)

	// 将当前日志重命名为备份
	if err := os.Rename(w.FilePath, backupName); err != nil {
		return err
	}

	// 重置状态并打开新文件
	w.file = nil
	w.currentSize = 0
	return w.openFile()
}

// Config 定义配置结构体
type Config struct {
	ListenPort            int           `yaml:"listen_port"`
	ScrapeInterval        time.Duration `yaml:"scrape_interval"`
	JstatTimeout          time.Duration `yaml:"jstat_timeout"`
	MaxMonitoredProcesses int           `yaml:"max_monitored_processes"`
	MaxConcurrentScrapes  int           `yaml:"max_concurrent_scrapes"`
	JstatPath             string        `yaml:"jstat_path"`
	LogLevel              string        `yaml:"log_level"`
	PidFilter             string        `yaml:"pid_filter"`
	LogPath               string        `yaml:"log_path"`
	MaxLogSize            int           `yaml:"max_log_size"`
}

var (
	config         Config
	configMu       sync.RWMutex
	currentPID     = os.Getpid()
	lastConfigHash string
)

func init() {
	configPath := flag.String("config", "config.yml", "Path to the configuration file")
	flag.Parse()

	// 尝试读取配置文件
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		log.Printf("Config file %s not found, creating default config...", *configPath)
		if err := createDefaultConfig(*configPath); err != nil {
			log.Printf("Warning: failed to create default config: %v", err)
		} else {
			log.Printf("Default config created at %s. Please edit it to configure 'pid_filter'.", *configPath)
		}
	}

	if err := loadConfig(*configPath); err != nil {
		log.Printf("Warning: failed to read config file %s: %v, using defaults", *configPath, err)
	}

	// 设置默认端口（若未配置）
	if config.ListenPort == 0 {
		config.ListenPort = 9101
	}

	// 设置其他默认值（若未配置）
	if config.ScrapeInterval == 0 {
		config.ScrapeInterval = 30 * time.Second
	}
	if config.JstatTimeout == 0 {
		config.JstatTimeout = 5 * time.Second
	}
	if config.MaxMonitoredProcesses == 0 {
		config.MaxMonitoredProcesses = 1000
	}
	if config.MaxConcurrentScrapes == 0 {
		config.MaxConcurrentScrapes = 50
	}
	if config.LogPath == "" {
		config.LogPath = "jvm_gc_collector.log"
	}
	if config.MaxLogSize == 0 {
		config.MaxLogSize = 200
	}

	// 设置日志输出到文件并支持轮转
	log.SetOutput(&RollingFileWriter{
		FilePath: config.LogPath,
		MaxSize:  int64(config.MaxLogSize) * 1024 * 1024,
	})

	// 检查系统中是否存在 jstat 命令
	jstatPath := "jstat"
	if err := checkJstatAvailable(jstatPath); err == nil {
		log.Println("jstat found in system PATH, using system jstat.")
		config.JstatPath = jstatPath
	} else {
		// 系统中不存在 jstat 命令，使用配置文件中的 jstat_path 或提示
		jstatPath := config.JstatPath
		if jstatPath == "" {
			jstatPath = "jstat"
		}
		if err := checkJstatAvailable(jstatPath); err != nil {
			log.Fatalf("jstat command not found or accessible.\n" +
				"Please make sure jstat is in your system PATH or update 'jstat_path' in the configuration file.")
		}
	}

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := loadConfig(*configPath); err != nil {
				log.Printf("Warning: failed to reload config file %s: %v", *configPath, err)
			}
		}
	}()
}

func createDefaultConfig(path string) error {
	defaultConfig := `
listen_port: 9101
scrape_interval: 30s
jstat_timeout: 5s
max_monitored_processes: 1000
max_concurrent_scrapes: 50
jstat_path: ""  # Leave empty to use system jstat or specify absolute path
log_level: "info"
pid_filter: ""  # REQUIRED: Keyword to filter processes (e.g., "tomcat", "java_app"). Must not be empty.
log_path: "jvm_gc_collector.log"
max_log_size: 200 # MB
`
	return os.WriteFile(path, []byte(strings.TrimSpace(defaultConfig)), 0644)
}

// loadConfig 读取并加载配置文件
func loadConfig(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	var newConfig Config
	if err := decoder.Decode(&newConfig); err != nil {
		return err
	}

	// 计算新配置的哈希值
	newConfigHash, err := getConfigHash(newConfig)
	if err != nil {
		return err
	}

	// 如果哈希值变化，说明配置有更新
	if newConfigHash != lastConfigHash {
		configMu.Lock()
		config = newConfig
		configMu.Unlock()

		lastConfigHash = newConfigHash
		log.Println("Configuration reloaded successfully.")
	}
	return nil
}

// getConfigHash 计算配置的哈希值
func getConfigHash(c Config) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", data), nil
}

// checkJstatAvailable 检查 jstat 是否可用
func checkJstatAvailable(jstatPath string) error {
	cmd := exec.Command(jstatPath, "-options")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jstat execution failed: %v", err)
	}
	return nil
}

// JvmGcCollector 结构体用于收集 JVM GC 指标
type JvmGcCollector struct {
	s0Capacity              *prometheus.GaugeVec
	s1Capacity              *prometheus.GaugeVec
	s0Usage                 *prometheus.GaugeVec
	s1Usage                 *prometheus.GaugeVec
	edenCapacity            *prometheus.GaugeVec
	edenUsage               *prometheus.GaugeVec
	oldGenCapacity          *prometheus.GaugeVec
	oldGenUsage             *prometheus.GaugeVec
	metaspaceCapacity       *prometheus.GaugeVec
	metaspaceUsage          *prometheus.GaugeVec
	compressedClassCapacity *prometheus.GaugeVec
	compressedClassUsage    *prometheus.GaugeVec
	youngGCCount            *prometheus.CounterVec
	youngGCTime             *prometheus.CounterVec
	fullGCCount             *prometheus.CounterVec
	fullGCTime              *prometheus.CounterVec
	gcTotalTime             *prometheus.CounterVec
	mu                      sync.Mutex
	lastMetrics             sync.Map
	appNameCache            sync.Map
	lastScrapeTime          time.Time
}

// NewJvmGcCollector 创建一个新的 JvmGcCollector 实例
func NewJvmGcCollector() *JvmGcCollector {
	return &JvmGcCollector{
		s0Capacity: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_s0_capacity_bytes",
				Help: "Survivor 0 capacity in bytes",
			},
			[]string{"pid", "app"},
		),
		s1Capacity: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_s1_capacity_bytes",
				Help: "Survivor 1 capacity in bytes",
			},
			[]string{"pid", "app"},
		),
		s0Usage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_s0_usage_bytes",
				Help: "Survivor 0 usage in bytes",
			},
			[]string{"pid", "app"},
		),
		s1Usage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_s1_usage_bytes",
				Help: "Survivor 1 usage in bytes",
			},
			[]string{"pid", "app"},
		),
		edenCapacity: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_eden_capacity_bytes",
				Help: "Eden capacity in bytes",
			},
			[]string{"pid", "app"},
		),
		edenUsage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_eden_usage_bytes",
				Help: "Eden usage in bytes",
			},
			[]string{"pid", "app"},
		),
		oldGenCapacity: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_old_gen_capacity_bytes",
				Help: "Old generation capacity in bytes",
			},
			[]string{"pid", "app"},
		),
		oldGenUsage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_old_gen_usage_bytes",
				Help: "Old generation usage in bytes",
			},
			[]string{"pid", "app"},
		),
		metaspaceCapacity: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_metaspace_capacity_bytes",
				Help: "Metaspace capacity in bytes",
			},
			[]string{"pid", "app"},
		),
		metaspaceUsage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_metaspace_usage_bytes",
				Help: "Metaspace usage in bytes",
			},
			[]string{"pid", "app"},
		),
		compressedClassCapacity: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_compressed_class_capacity_bytes",
				Help: "Compressed class capacity in bytes",
			},
			[]string{"pid", "app"},
		),
		compressedClassUsage: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "jvm_gc_compressed_class_usage_bytes",
				Help: "Compressed class usage in bytes",
			},
			[]string{"pid", "app"},
		),
		youngGCCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "jvm_gc_young_gc_count",
				Help: "Young GC count",
			},
			[]string{"pid", "app"},
		),
		youngGCTime: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "jvm_gc_young_gc_time_seconds",
				Help: "Young GC time in seconds",
			},
			[]string{"pid", "app"},
		),
		fullGCCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "jvm_gc_full_gc_count",
				Help: "Full GC count",
			},
			[]string{"pid", "app"},
		),
		fullGCTime: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "jvm_gc_full_gc_time_seconds",
				Help: "Full GC time in seconds",
			},
			[]string{"pid", "app"},
		),
		gcTotalTime: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "jvm_gc_gct_total_seconds",
				Help: "Total GC time in seconds",
			},
			[]string{"pid", "app"},
		),
	}
}

// Describe 实现 Prometheus Collector 接口的 Describe 方法
func (c *JvmGcCollector) Describe(ch chan<- *prometheus.Desc) {
	c.s0Capacity.Describe(ch)
	c.s1Capacity.Describe(ch)
	c.s0Usage.Describe(ch)
	c.s1Usage.Describe(ch)
	c.edenCapacity.Describe(ch)
	c.edenUsage.Describe(ch)
	c.oldGenCapacity.Describe(ch)
	c.oldGenUsage.Describe(ch)
	c.metaspaceCapacity.Describe(ch)
	c.metaspaceUsage.Describe(ch)
	c.compressedClassCapacity.Describe(ch)
	c.compressedClassUsage.Describe(ch)
	c.youngGCCount.Describe(ch)
	c.youngGCTime.Describe(ch)
	c.fullGCCount.Describe(ch)
	c.fullGCTime.Describe(ch)
	c.gcTotalTime.Describe(ch)
}

// Collect 实现 Prometheus Collector 接口的 Collect 方法
func (c *JvmGcCollector) Collect(ch chan<- prometheus.Metric) {
	configMu.RLock()
	// 注意：这里我们持有读锁直到 Collect 方法结束，或者在需要时释放。
	// 为了避免锁持有时间过长，我们可以先复制一份需要的配置。
	// 但考虑到 loadConfig 的频率很低（5秒一次且仅在变化时写锁），
	// 且 Collect 主要是 I/O 操作（exec command），
	// 长时间持有 configMu 读锁可能会阻塞 loadConfig 的写锁（取决于 RWMutex 实现，Go 的 RWMutex 写锁优先级较高或公平）。
	// 为了安全起见，我们只在读取配置时加锁。
	scrapeInterval := config.ScrapeInterval
	maxMonitored := config.MaxMonitoredProcesses
	maxConcurrent := config.MaxConcurrentScrapes
	pidFilter := config.PidFilter
	configMu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 检查采集间隔，如果距离上次采集时间小于配置的 ScrapeInterval，直接返回上次的缓存数据（通过 Prometheus 自动维护的 Metric 状态）
	// 注意：Prometheus Client 库中的 Collector 并不自动缓存 Metric 值。
	// 但在这个实现中，Counter 类型（如 youngGCCount）是累加的，Gauge 类型（如 s0Capacity）是瞬时的。
	// 为了真正实现“不执行 jstat”，我们需要跳过后续的采集逻辑，
	// 直接使用 Prometheus Client 库中已经注册的指标当前值（它们会保留上次 Set/Add 的结果）。
	// 所以这里直接 return 即可，Prometheus 会再次收集到上次 Set 的值（前提是 Metric 没被 Reset，这里是全局单例 Collector，状态是保持的）。
	if time.Since(c.lastScrapeTime) < scrapeInterval {
		// 直接收集当前指标状态（即上次采集的结果）
		c.collectMetrics(ch)
		return
	}

	pids, err := getJavaPids()
	if err != nil {
		log.Printf("Failed to get Java PIDs: %v", err)
		return
	}

	if len(pids) > maxMonitored {
		log.Printf("WARN: too many processes (%d), only monitoring first %d", len(pids), maxMonitored)
		pids = pids[:maxMonitored]
	}

	// 清理过期缓存
	currentPids := make(map[int]bool)
	for _, pid := range pids {
		currentPids[pid] = true
	}
	c.appNameCache.Range(func(key, value interface{}) bool {
		if pid, ok := key.(int); ok {
			if !currentPids[pid] {
				c.appNameCache.Delete(key)
				// 清理过期 PID 的指标，防止指标泄露（Zombie Metrics）
				if app, ok := value.(string); ok {
					pidStr := strconv.Itoa(pid)
					c.s0Capacity.DeleteLabelValues(pidStr, app)
					c.s1Capacity.DeleteLabelValues(pidStr, app)
					c.s0Usage.DeleteLabelValues(pidStr, app)
					c.s1Usage.DeleteLabelValues(pidStr, app)
					c.edenCapacity.DeleteLabelValues(pidStr, app)
					c.edenUsage.DeleteLabelValues(pidStr, app)
					c.oldGenCapacity.DeleteLabelValues(pidStr, app)
					c.oldGenUsage.DeleteLabelValues(pidStr, app)
					c.metaspaceCapacity.DeleteLabelValues(pidStr, app)
					c.metaspaceUsage.DeleteLabelValues(pidStr, app)
					c.compressedClassCapacity.DeleteLabelValues(pidStr, app)
					c.compressedClassUsage.DeleteLabelValues(pidStr, app)
					c.youngGCCount.DeleteLabelValues(pidStr, app)
					c.youngGCTime.DeleteLabelValues(pidStr, app)
					c.fullGCCount.DeleteLabelValues(pidStr, app)
					c.fullGCTime.DeleteLabelValues(pidStr, app)
					c.gcTotalTime.DeleteLabelValues(pidStr, app)
				}
				c.lastMetrics.Delete(pid)
			}
		}
		return true
	})

	var wg sync.WaitGroup
	scrapeSem := make(chan struct{}, maxConcurrent)

	for _, pid := range pids {
		if pid == currentPID {
			continue
		}
		wg.Add(1)
		scrapeSem <- struct{}{}
		go func(pid int) {
			defer wg.Done()
			defer func() { <-scrapeSem }()

			// 直接使用配置的 PidFilter 作为应用名
			// 注意：这里使用了局部变量 pidFilter，避免闭包引用全局 config
			app := pidFilter
			c.appNameCache.Store(pid, app)

			metrics, err := scrapeJstat(pid)
			if err != nil {
				log.Printf("Failed to scrape jstat for PID %d: %v", pid, err)
				return
			}

			c.updateMetrics(pid, app, metrics)
		}(pid)
	}

	wg.Wait()
	c.lastScrapeTime = time.Now()

	c.collectMetrics(ch)
}

// collectMetrics 统一收集所有指标到 channel
func (c *JvmGcCollector) collectMetrics(ch chan<- prometheus.Metric) {
	c.s0Capacity.Collect(ch)
	c.s1Capacity.Collect(ch)
	c.s0Usage.Collect(ch)
	c.s1Usage.Collect(ch)
	c.edenCapacity.Collect(ch)
	c.edenUsage.Collect(ch)
	c.oldGenCapacity.Collect(ch)
	c.oldGenUsage.Collect(ch)
	c.metaspaceCapacity.Collect(ch)
	c.metaspaceUsage.Collect(ch)
	c.compressedClassCapacity.Collect(ch)
	c.compressedClassUsage.Collect(ch)
	c.youngGCCount.Collect(ch)
	c.youngGCTime.Collect(ch)
	c.fullGCCount.Collect(ch)
	c.fullGCTime.Collect(ch)
	c.gcTotalTime.Collect(ch)
}

// updateMetrics 更新指标值
func (c *JvmGcCollector) updateMetrics(pid int, app string, metrics *gcMetrics) {
	pidStr := strconv.Itoa(pid)
	// Gauge 指标继续使用 Set
	c.s0Capacity.WithLabelValues(pidStr, app).Set(metrics.s0c)
	c.s1Capacity.WithLabelValues(pidStr, app).Set(metrics.s1c)
	c.s0Usage.WithLabelValues(pidStr, app).Set(metrics.s0u)
	c.s1Usage.WithLabelValues(pidStr, app).Set(metrics.s1u)
	c.edenCapacity.WithLabelValues(pidStr, app).Set(metrics.ec)
	c.edenUsage.WithLabelValues(pidStr, app).Set(metrics.eu)
	c.oldGenCapacity.WithLabelValues(pidStr, app).Set(metrics.oc)
	c.oldGenUsage.WithLabelValues(pidStr, app).Set(metrics.ou)
	c.metaspaceCapacity.WithLabelValues(pidStr, app).Set(metrics.mc)
	c.metaspaceUsage.WithLabelValues(pidStr, app).Set(metrics.mu)
	c.compressedClassCapacity.WithLabelValues(pidStr, app).Set(metrics.ccsc)
	c.compressedClassUsage.WithLabelValues(pidStr, app).Set(metrics.ccsu)

	// 获取上次指标
	last, _ := c.lastMetrics.Load(pid)
	lastMetrics, ok := last.(*gcMetrics)

	// 计算增量
	delta := &gcMetrics{
		ygc:  metrics.ygc,
		ygct: metrics.ygct,
		fgc:  metrics.fgc,
		fgct: metrics.fgct,
		gct:  metrics.gct,
	}
	if ok {
		// 检查是否发生进程重启（计数器回退）
		if metrics.ygc < lastMetrics.ygc || metrics.fgc < lastMetrics.fgc {
			log.Printf("Process %d restarted (counters reset), resetting metrics baseline", pid)
			// 如果检测到重启，本次增量设为当前值（视为从 0 开始）
			delta.ygc = metrics.ygc
			delta.ygct = metrics.ygct
			delta.fgc = metrics.fgc
			delta.fgct = metrics.fgct
			delta.gct = metrics.gct
		} else {
			delta.ygc = metrics.ygc - lastMetrics.ygc
			delta.ygct = metrics.ygct - lastMetrics.ygct
			delta.fgc = metrics.fgc - lastMetrics.fgc
			delta.fgct = metrics.fgct - lastMetrics.fgct
			delta.gct = metrics.gct - lastMetrics.gct
		}
	}

	// 更新 Counter 指标
	c.youngGCCount.WithLabelValues(pidStr, app).Add(delta.ygc)
	c.youngGCTime.WithLabelValues(pidStr, app).Add(delta.ygct)
	c.fullGCCount.WithLabelValues(pidStr, app).Add(delta.fgc)
	c.fullGCTime.WithLabelValues(pidStr, app).Add(delta.fgct)
	c.gcTotalTime.WithLabelValues(pidStr, app).Add(delta.gct)

	// 保存当前指标
	c.lastMetrics.Store(pid, metrics)
}

// getJavaPids 获取所有 Java 进程的 PID
func getJavaPids() ([]int, error) {
	if config.PidFilter == "" {
		return nil, fmt.Errorf("pid_filter is empty. Please configure 'pid_filter' to specify target processes")
	}

	var cmd *exec.Cmd
	cmd = exec.Command("sh", "-c", fmt.Sprintf("ps -ef | grep %s | grep -v grep | awk '{print $2}'", config.PidFilter))

	output, err := cmd.Output()
	if err != nil {
		// 如果 exit code 为 1，说明 grep 没有匹配到任何进程，这不是错误
		if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() == 1 {
			return []int{}, nil
		}
		return nil, fmt.Errorf("failed to execute command to get PIDs: %v", err)
	}

	var pids []int
	for _, s := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		pid, err := strconv.Atoi(s)
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// getAppName 已经不再使用，可以删除或保留作为工具函数
// func getAppName(pid int) string { ... }

// gcMetrics 存储从 jstat 输出解析得到的指标
type gcMetrics struct {
	s0c, s1c, s0u, s1u, ec, eu, oc, ou, mc, mu, ccsc, ccsu float64
	ygc, fgct, ygct, fgc, gct                              float64
}

// scrapeJstat 执行 jstat 命令并解析输出
func scrapeJstat(pid int) (*gcMetrics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.JstatTimeout)
	defer cancel()

	jstatCmd := config.JstatPath
	if jstatCmd == "" {
		jstatCmd = "jstat"
	}

	cmd := exec.CommandContext(ctx, jstatCmd, "-gc", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to execute jstat for PID %d: %v", pid, err)
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("invalid jstat output for PID %d", pid)
	}

	headers := strings.Fields(lines[0])
	values := strings.Fields(lines[1])
	if len(headers) != len(values) {
		return nil, fmt.Errorf("header and value count mismatch for PID %d", pid)
	}

	data := make(map[string]float64)
	for i, h := range headers {
		val, err := strconv.ParseFloat(values[i], 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse value for %s in PID %d: %v", h, pid, err)
		}
		data[h] = val
	}

	return &gcMetrics{
		s0c:  data["S0C"] * 1024, // 转换为字节
		s1c:  data["S1C"] * 1024,
		s0u:  data["S0U"] * 1024,
		s1u:  data["S1U"] * 1024,
		ec:   data["EC"] * 1024,
		eu:   data["EU"] * 1024,
		oc:   data["OC"] * 1024,
		ou:   data["OU"] * 1024,
		mc:   data["MC"] * 1024,
		mu:   data["MU"] * 1024,
		ccsc: data["CCSC"] * 1024,
		ccsu: data["CCSU"] * 1024,
		ygc:  data["YGC"],
		fgct: data["FGCT"],
		ygct: data["YGCT"],
		fgc:  data["FGC"],
		gct:  data["GCT"],
	}, nil
}

func main() {
	collector := NewJvmGcCollector()
	prometheus.MustRegister(collector)

	http.Handle("/metrics", promhttp.Handler())
	log.Printf("Starting server on :%d", config.ListenPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.ListenPort), nil))
}
