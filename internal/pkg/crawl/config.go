package crawl

import (
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"sync"
	"time"

	"git.archive.org/wb/gocrawlhq"
	"github.com/CorentinB/warc"
	"github.com/google/uuid"
	"github.com/internetarchive/Zeno/config"
	"github.com/internetarchive/Zeno/internal/pkg/frontier"
	"github.com/internetarchive/Zeno/internal/pkg/log"
	"github.com/internetarchive/Zeno/internal/pkg/utils"
	"github.com/paulbellamy/ratecounter"
)

// Crawl define the parameters of a crawl process
type Crawl struct {
	*sync.Mutex
	StartTime time.Time
	SeedList  []frontier.Item
	Paused    *utils.TAtomBool
	Finished  *utils.TAtomBool
	LiveStats bool

	// Logger
	Log *log.Logger

	// Frontier
	Frontier *frontier.Frontier

	// Worker pool
	WorkerMutex       sync.RWMutex
	WorkerPool        []*Worker
	WorkerStopSignal  chan bool
	WorkerStopTimeout time.Duration

	// Crawl settings
	MaxConcurrentAssets            int
	Client                         *warc.CustomHTTPClient
	ClientProxied                  *warc.CustomHTTPClient
	DisabledHTMLTags               []string
	ExcludedHosts                  []string
	IncludedHosts                  []string
	ExcludedStrings                []string
	UserAgent                      string
	Job                            string
	JobPath                        string
	MaxHops                        uint8
	MaxRetry                       int
	MaxRedirect                    int
	HTTPTimeout                    int
	MaxConcurrentRequestsPerDomain int
	RateLimitDelay                 int
	CrawlTimeLimit                 int
	MaxCrawlTimeLimit              int
	DisableAssetsCapture           bool
	CaptureAlternatePages          bool
	DomainsCrawl                   bool
	Headless                       bool
	Seencheck                      bool
	Workers                        int
	RandomLocalIP                  bool
	MinSpaceRequired               int

	// Cookie-related settings
	CookieFile  string
	KeepCookies bool
	CookieJar   http.CookieJar

	// proxy settings
	Proxy       string
	BypassProxy []string

	// API settings
	API               bool
	APIPort           string
	Prometheus        bool
	PrometheusMetrics *PrometheusMetrics

	// Real time statistics
	URIsPerSecond *ratecounter.RateCounter
	ActiveWorkers *ratecounter.Counter
	CrawledSeeds  *ratecounter.Counter
	CrawledAssets *ratecounter.Counter

	// WARC settings
	WARCPrefix         string
	WARCOperator       string
	WARCWriter         chan *warc.RecordBatch
	WARCWriterFinish   chan bool
	WARCTempDir        string
	CDXDedupeServer    string
	WARCFullOnDisk     bool
	WARCPoolSize       int
	WARCDedupSize      int
	DisableLocalDedupe bool
	CertValidation     bool
	WARCCustomCookie   string

	// Crawl HQ settings
	UseHQ                  bool
	HQAddress              string
	HQProject              string
	HQKey                  string
	HQSecret               string
	HQStrategy             string
	HQBatchSize            int
	HQContinuousPull       bool
	HQClient               *gocrawlhq.Client
	HQFinishedChannel      chan *frontier.Item
	HQProducerChannel      chan *frontier.Item
	HQChannelsWg           *sync.WaitGroup
	HQRateLimitingSendBack bool
}

func GenerateCrawlConfig(config *config.Config) (*Crawl, error) {
	var c = new(Crawl)

	// Ensure that the log file output directory is well parsed
	logfileOutputDir := filepath.Dir(config.LogFileOutputDir)
	if logfileOutputDir == "." && config.LogFileOutputDir != "." {
		logfileOutputDir = filepath.Dir(config.LogFileOutputDir + "/")
	}

	// Logger
	customLoggerConfig := log.Config{
		FileConfig: &log.LogfileConfig{
			Dir:    logfileOutputDir,
			Prefix: "zeno",
		},
		FileLevel:                slog.LevelDebug,
		StdoutLevel:              slog.LevelInfo,
		RotateLogFile:            true,
		RotateElasticSearchIndex: true,
		ElasticsearchConfig: &log.ElasticsearchConfig{
			Addresses:   config.ElasticSearchURLs,
			Username:    config.ElasticSearchUsername,
			Password:    config.ElasticSearchPassword,
			IndexPrefix: config.ElasticSearchIndexPrefix,
			Level:       slog.LevelDebug,
		},
	}
	if len(config.ElasticSearchURLs) == 0 || (config.ElasticSearchUsername == "" && config.ElasticSearchPassword == "") {
		customLoggerConfig.ElasticsearchConfig = nil
	}

	customLogger, err := log.New(customLoggerConfig)
	if err != nil {
		return nil, err
	}
	c.Log = customLogger

	// Statistics counters
	c.CrawledSeeds = new(ratecounter.Counter)
	c.CrawledAssets = new(ratecounter.Counter)
	c.ActiveWorkers = new(ratecounter.Counter)
	c.URIsPerSecond = ratecounter.NewRateCounter(1 * time.Second)

	c.LiveStats = config.LiveStats

	// Frontier
	c.Frontier = new(frontier.Frontier)
	c.Frontier.Log = c.Log

	// If the job name isn't specified, we generate a random name
	if config.Job == "" {
		if config.HQProject != "" {
			c.Job = config.HQProject
		} else {
			UUID, err := uuid.NewUUID()
			if err != nil {
				c.Log.Error("cmd/utils.go:InitCrawlWithCMD():uuid.NewUUID()", "error", err)
				return nil, err
			}

			c.Job = UUID.String()
		}
	} else {
		c.Job = config.Job
	}

	c.JobPath = path.Join("jobs", config.Job)

	c.Workers = config.WorkersCount
	c.WorkerPool = make([]*Worker, 0)
	c.WorkerStopTimeout = time.Second * 60 // Placeholder for WorkerStopTimeout
	c.MaxConcurrentAssets = config.MaxConcurrentAssets
	c.WorkerStopSignal = make(chan bool)

	c.Seencheck = config.LocalSeencheck
	c.HTTPTimeout = config.HTTPTimeout
	c.MaxConcurrentRequestsPerDomain = config.MaxConcurrentRequestsPerDomain
	c.RateLimitDelay = config.ConcurrentSleepLength
	c.CrawlTimeLimit = config.CrawlTimeLimit

	// Defaults --max-crawl-time-limit to 10% more than --crawl-time-limit
	if config.CrawlMaxTimeLimit == 0 && config.CrawlTimeLimit != 0 {
		c.MaxCrawlTimeLimit = config.CrawlTimeLimit + (config.CrawlTimeLimit / 10)
	} else {
		c.MaxCrawlTimeLimit = config.CrawlMaxTimeLimit
	}

	c.MaxRetry = config.MaxRetry
	c.MaxRedirect = config.MaxRedirect
	c.MaxHops = uint8(config.MaxHops)
	c.DomainsCrawl = config.DomainsCrawl
	c.DisableAssetsCapture = config.DisableAssetsCapture
	c.DisabledHTMLTags = config.DisableHTMLTag
	c.ExcludedHosts = config.ExcludeHosts
	c.IncludedHosts = config.IncludeHosts
	c.CaptureAlternatePages = config.CaptureAlternatePages
	c.ExcludedStrings = config.ExcludeString

	c.MinSpaceRequired = config.MinSpaceRequired

	// WARC settings
	c.WARCPrefix = config.WARCPrefix
	c.WARCOperator = config.WARCOperator

	if config.WARCTempDir != "" {
		c.WARCTempDir = config.WARCTempDir
	} else {
		c.WARCTempDir = path.Join(c.JobPath, "temp")
	}

	c.CDXDedupeServer = config.CDXDedupeServer
	c.DisableLocalDedupe = config.DisableLocalDedupe
	c.CertValidation = config.CertValidation
	c.WARCFullOnDisk = config.WARCOnDisk
	c.WARCPoolSize = config.WARCPoolSize
	c.WARCDedupSize = config.WARCDedupeSize
	c.WARCCustomCookie = config.CDXCookie

	c.API = config.API
	c.APIPort = config.APIPort

	// If Prometheus is specified, then we make sure
	// c.API is true
	c.Prometheus = config.Prometheus
	if c.Prometheus {
		c.API = true
		c.PrometheusMetrics = new(PrometheusMetrics)
		c.PrometheusMetrics.Prefix = config.PrometheusPrefix
	}

	if config.UserAgent != "Zeno" {
		c.UserAgent = config.UserAgent
	} else {
		version := utils.GetVersion()
		c.UserAgent = "Mozilla/5.0 (compatible; archive.org_bot +http://archive.org/details/archive.org_bot) Zeno/" + version.Version[:7] + " warc/" + version.WarcVersion
	}
	c.Headless = config.Headless

	c.CookieFile = config.Cookies
	c.KeepCookies = config.KeepCookies

	// Proxy settings
	c.Proxy = config.Proxy
	c.BypassProxy = config.DomainsBypassProxy

	// Crawl HQ settings
	c.UseHQ = config.HQ
	c.HQProject = config.HQProject
	c.HQAddress = config.HQAddress
	c.HQKey = config.HQKey
	c.HQSecret = config.HQSecret
	c.HQStrategy = config.HQStrategy
	c.HQBatchSize = int(config.HQBatchSize)
	c.HQContinuousPull = config.HQContinuousPull
	c.HQRateLimitingSendBack = config.HQRateLimitSendBack

	return c, nil
}
