package main

import (
    "bytes"
    "context"
    "flag"
    "fmt"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "regexp"
    "sort"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "github.com/sirupsen/logrus"
    "gopkg.in/yaml.v3"
)

type Config struct {
    ListenPort            int           `yaml:"listen_port"`
    ScrapeInterval        time.Duration `yaml:"scrape_interval"`
    JstatTimeout          time.Duration `yaml:"jstat_timeout"`
    MaxMonitoredProcesses int           `yaml:"max_monitored_processes"`
    MaxConcurrentScrapes  int           `yaml:"max_concurrent_scrapes"`
    JstatPath             string        `yaml:"jstat_path"`
    LogLevel              string        `yaml:"log_level"`
    PidFilter             string        `yaml:"pid_filter"`
    AppNameLabels         []string      `yaml:"app_name_labels"`
}

var (
    config      Config
    configMu    sync.RWMutex
    bufferPool  = sync.Pool{New: func() interface{} { return bytes.NewBuffer(nil) }}
    currentPID  = os.Getpid()
    configPath  string
    appPatterns []*regexp.Regexp
)

func init() {
    flag.StringVar(&configPath, "config", "config.yml", "Path to config file")
    flag.Parse()

    // 检查并创建默认配置文件
    if _, err := os.Stat(configPath); os.IsNotExist(err) {
        createDefaultConfig(configPath)
    }

    if err := loadConfig(configPath); err != nil {
        logrus.Fatalf("Config load failed: %v", err)
    }

    go watchConfigReload()
}

// 创建默认配置文件
func createDefaultConfig(path string) {
    defaultConfig := `listen_port: 9101
scrape_interval: 30s
jstat_timeout: 5s
max_monitored_processes: 1000
max_concurrent_scrapes: 50
jstat_path: jstat
log_level: info
pid_filter: ""
app_name_labels:
  - "-Dapp.name"
  - "-Dapp"
  - "-Dspring.application.name"`

    if err := os.WriteFile(path, []byte(defaultConfig), 0644); err != nil {
        logrus.Fatalf("Failed to create default config: %v", err)
    }
    logrus.Infof("Created default config file at %s", path)
}

func main() {
    collector := NewJvmGcCollector()
    prometheus.MustRegister(collector)

    http.Handle("/metrics", promhttp.Handler())
    http.HandleFunc("/health", healthHandler)
    logrus.Infof("Starting server on :%d", config.ListenPort)
    logrus.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.ListenPort), nil))
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

func watchConfigReload() {
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, syscall.SIGHUP)
    for range sigs {
        if err := loadConfig(configPath); err != nil {
            logrus.Errorf("Config reload failed: %v", err)
        } else {
            logrus.Info("Config reloaded successfully")
        }
    }
}

func loadConfig(path string) error {
    configMu.Lock()
    defer configMu.Unlock()

    file, err := os.Open(path)
    if err != nil {
        return fmt.Errorf("open config: %w", err)
    }
    defer file.Close()

    var newConfig Config
    if err := yaml.NewDecoder(file).Decode(&newConfig); err != nil {
        return fmt.Errorf("parse config: %w", err)
    }

    if newConfig.AppNameLabels == nil {
        newConfig.AppNameLabels = []string{
            "-Dapp.name", 
            "-Dapp",
            "-Dspring.application.name",
        }
    }

    if err := validateConfig(&newConfig); err != nil {
        return err
    }

    compileAppPatterns(newConfig.AppNameLabels)
    setLogLevel(newConfig.LogLevel)
    config = newConfig
    return nil
}

func validateConfig(c *Config) error {
    for _, label := range c.AppNameLabels {
        if !strings.HasPrefix(label, "-D") || strings.Contains(label, " ") {
            return fmt.Errorf("invalid app_name_label: %q", label)
        }
    }
    return nil
}

func compileAppPatterns(labels []string) {
    appPatterns = make([]*regexp.Regexp, 0, len(labels))
    for _, label := range labels {
        pattern := regexp.MustCompile(
            fmt.Sprintf(`%s=(["']?)([^"'\s]+)`, 
            regexp.QuoteMeta(label)),
        )
        appPatterns = append(appPatterns, pattern)
    }
}

func setLogLevel(level string) {
    lvl, err := logrus.ParseLevel(level)
    if err != nil {
        logrus.SetLevel(logrus.InfoLevel)
        logrus.Warnf("Invalid log level %q, using info", level)
        return
    }
    logrus.SetLevel(lvl)
}

type JvmGcCollector struct {
    metrics     map[string]*prometheus.GaugeVec
    counters    map[string]*prometheus.CounterVec
    lastMetrics sync.Map
    lastPids    []int
}

func NewJvmGcCollector() *JvmGcCollector {
    labels := []string{"pid", "app"} 
    return &JvmGcCollector{
        metrics: map[string]*prometheus.GaugeVec{
            "s0_capacity":  newGauge("s0_capacity_bytes", labels),
            "s1_capacity":  newGauge("s1_capacity_bytes", labels),
            "s0_usage":     newGauge("s0_usage_bytes", labels),
            "s1_usage":     newGauge("s1_usage_bytes", labels),
            "eden_capacity": newGauge("eden_capacity_bytes", labels),
            "eden_usage":    newGauge("eden_usage_bytes", labels),
            "old_capacity":  newGauge("old_gen_capacity_bytes", labels),
            "old_usage":     newGauge("old_gen_usage_bytes", labels),
            "meta_capacity": newGauge("metaspace_capacity_bytes", labels),
            "meta_usage":    newGauge("metaspace_usage_bytes", labels),
            "class_capacity": newGauge("compressed_class_capacity_bytes", labels),
            "class_usage":    newGauge("compressed_class_usage_bytes", labels),
        },
        counters: map[string]*prometheus.CounterVec{
            "young_gc_count": newCounter("young_gc_count", labels),
            "young_gc_time":  newCounter("young_gc_time_seconds", labels),
            "full_gc_count":  newCounter("full_gc_count", labels),
            "full_gc_time":   newCounter("full_gc_time_seconds", labels),
            "total_gc_time":  newCounter("total_gc_time_seconds", labels),
        },
    }
}

func newGauge(name string, labels []string) *prometheus.GaugeVec {
    return prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "jvm_gc_" + name,
        Help: "JVM GC " + name,
    }, labels)
}

func newCounter(name string, labels []string) *prometheus.CounterVec {
    return prometheus.NewCounterVec(prometheus.CounterOpts{
        Name: "jvm_gc_" + name,
        Help: "JVM GC " + name,
    }, labels)
}

func (c *JvmGcCollector) Describe(ch chan<- *prometheus.Desc) {
    for _, m := range c.metrics {
        m.Describe(ch)
    }
    for _, m := range c.counters {
        m.Describe(ch)
    }
}

func (c *JvmGcCollector) Collect(ch chan<- prometheus.Metric) {
    pids, err := getJavaPids()
    if err != nil {
        logrus.Errorf("Get PIDs failed: %v", err)
        return
    }

    alivePids := filterAlivePids(pids)
    sort.Ints(alivePids)

    c.cleanupStaleMetrics(alivePids)

    var wg sync.WaitGroup
    sem := make(chan struct{}, config.MaxConcurrentScrapes)

    for _, pid := range alivePids {
        if pid == currentPID {
            continue
        }

        wg.Add(1)
        go func(pid int) {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()

            if !isProcessAlive(pid) {
                return
            }

            app := getAppName(pid)
            metrics, err := scrapeJstat(pid)
            if err != nil {
                logrus.Errorf("Scrape PID %d failed: %v", pid, err)
                return
            }

            c.updateMetrics(pid, app, metrics)
        }(pid)
    }
    wg.Wait()

    for _, m := range c.metrics {
        m.Collect(ch)
    }
    for _, m := range c.counters {
        m.Collect(ch)
    }
}

func (c *JvmGcCollector) cleanupStaleMetrics(current []int) {
    currentSet := make(map[int]bool)
    for _, pid := range current {
        currentSet[pid] = true
    }

    var toDelete []int
    c.lastMetrics.Range(func(key, _ interface{}) bool {
        pid := key.(int)
        if !currentSet[pid] {
            toDelete = append(toDelete, pid)
        }
        return true
    })

    for _, pid := range toDelete {
        c.removeMetrics(pid)
    }
}

func (c *JvmGcCollector) removeMetrics(pid int) {
    c.lastMetrics.Delete(pid)
    for _, m := range c.metrics {
        m.DeletePartialMatch(prometheus.Labels{"pid": strconv.Itoa(pid)})
    }
    for _, m := range c.counters {
        m.DeletePartialMatch(prometheus.Labels{"pid": strconv.Itoa(pid)})
    }
}

func (c *JvmGcCollector) updateMetrics(pid int, app string, metrics *gcMetrics) {
    labels := prometheus.Labels{
        "pid": strconv.Itoa(pid),
        "app": app, // 保留pid和app标签
    }

    c.metrics["s0_capacity"].With(labels).Set(metrics.s0c)
    c.metrics["s1_capacity"].With(labels).Set(metrics.s1c)
    c.metrics["s0_usage"].With(labels).Set(metrics.s0u)
    c.metrics["s1_usage"].With(labels).Set(metrics.s1u)
    c.metrics["eden_capacity"].With(labels).Set(metrics.ec)
    c.metrics["eden_usage"].With(labels).Set(metrics.eu)
    c.metrics["old_capacity"].With(labels).Set(metrics.oc)
    c.metrics["old_usage"].With(labels).Set(metrics.ou)
    c.metrics["meta_capacity"].With(labels).Set(metrics.mc)
    c.metrics["meta_usage"].With(labels).Set(metrics.mu)
    c.metrics["class_capacity"].With(labels).Set(metrics.ccsc)
    c.metrics["class_usage"].With(labels).Set(metrics.ccsu)

    last, exists := c.lastMetrics.Load(pid)
    if !exists {
        c.lastMetrics.Store(pid, metrics)
        return
    }

    lastMetrics := last.(*gcMetrics)
    delta := &gcMetrics{
        ygc:  metrics.ygc - lastMetrics.ygc,
        ygct: metrics.ygct - lastMetrics.ygct,
        fgc:  metrics.fgc - lastMetrics.fgc,
        fgct: metrics.fgct - lastMetrics.fgct,
        gct:  metrics.gct - lastMetrics.gct,
    }

    c.counters["young_gc_count"].With(labels).Add(delta.ygc)
    c.counters["young_gc_time"].With(labels).Add(delta.ygct)
    c.counters["full_gc_count"].With(labels).Add(delta.fgc)
    c.counters["full_gc_time"].With(labels).Add(delta.fgct)
    c.counters["total_gc_time"].With(labels).Add(delta.gct)

    c.lastMetrics.Store(pid, metrics)
}

func isProcessAlive(pid int) bool {
    return syscall.Kill(pid, 0) == nil
}

func filterAlivePids(pids []int) []int {
    var alive []int
    for _, pid := range pids {
        if isProcessAlive(pid) {
            alive = append(alive, pid)
        }
    }
    return alive
}

func getJavaPids() ([]int, error) {
    configMu.RLock()
    defer configMu.RUnlock()

    cmd := exec.Command("pgrep", "-f", "java.*"+config.PidFilter)
    output, err := cmd.Output()
    if err != nil {
        return nil, fmt.Errorf("pgrep failed: %w", err)
    }

    var pids []int
    for _, line := range strings.Split(string(output), "\n") {
        if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
            pids = append(pids, pid)
        }
    }
    return pids, nil
}

func getAppName(pid int) string {
    cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=")
    output, err := cmd.Output()
    if err != nil {
        return fmt.Sprintf("pid-%d", pid)
    }

    args := string(output)
    for _, pattern := range appPatterns {
        if matches := pattern.FindStringSubmatch(args); len(matches) > 2 {
            return strings.Trim(matches[2], `"'`)
        }
    }
    return fmt.Sprintf("pid-%d", pid)
}

type gcMetrics struct {
    s0c, s1c, s0u, s1u float64
    ec, eu, oc, ou     float64
    mc, mu, ccsc, ccsu float64
    ygc, fgc           float64
    ygct, fgct, gct    float64
}

func scrapeJstat(pid int) (*gcMetrics, error) {
    ctx, cancel := context.WithTimeout(context.Background(), config.JstatTimeout)
    defer cancel()

    jstatPath := config.JstatPath
    if jstatPath == "" {
        jstatPath = "jstat"
    }

    cmd := exec.CommandContext(ctx, jstatPath, "-gc", strconv.Itoa(pid))
    buf := bufferPool.Get().(*bytes.Buffer)
    buf.Reset()
    defer bufferPool.Put(buf)

    cmd.Stdout = buf
    if err := cmd.Run(); err != nil {
        return nil, fmt.Errorf("jstat failed: %w", err)
    }

    lines := strings.Split(buf.String(), "\n")
    if len(lines) < 2 {
        return nil, fmt.Errorf("invalid jstat output")
    }

    headers := strings.Fields(lines[0])
    values := strings.Fields(lines[1])
    if len(headers) != len(values) {
        return nil, fmt.Errorf("header/value mismatch")
    }

    data := make(map[string]float64)
    for i, h := range headers {
        val, _ := strconv.ParseFloat(values[i], 64)
        data[h] = val
    }

    return &gcMetrics{
        s0c:   data["S0C"] * 1024,
        s1c:   data["S1C"] * 1024,
        s0u:   data["S0U"] * 1024,
        s1u:   data["S1U"] * 1024,
        ec:    data["EC"] * 1024,
        eu:    data["EU"] * 1024,
        oc:    data["OC"] * 1024,
        ou:    data["OU"] * 1024,
        mc:    data["MC"] * 1024,
        mu:    data["MU"] * 1024,
        ccsc:  data["CCSC"] * 1024,
        ccsu:  data["CCSU"] * 1024,
        ygc:   data["YGC"],
        fgc:   data["FGC"],
        ygct:  data["YGCT"],
        fgct:  data["FGCT"],
        gct:   data["GCT"],
    }, nil
}
