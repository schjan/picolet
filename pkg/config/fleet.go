package config

// FleetConfig holds fleet-wide configuration from fleet.yml.
type FleetConfig struct {
	Images     map[string]string `yaml:"images"`
	Ports      map[string]int    `yaml:"ports"`
	Prometheus PrometheusConfig  `yaml:"prometheus"`
}

// PrometheusConfig holds Prometheus-specific configuration.
type PrometheusConfig struct {
	ScrapeInterval         string `yaml:"scrape_interval"`
	ScrapeTimeout          string `yaml:"scrape_timeout"`
	ExporterScrapeInterval string `yaml:"exporter_scrape_interval"`
	RetentionTime          string `yaml:"retention_time"`
	RetentionSize          string `yaml:"retention_size"`
}
