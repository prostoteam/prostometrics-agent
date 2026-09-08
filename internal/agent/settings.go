package agent

import "time"

const (
	CollectTimeout = 3 * time.Second

	// MaxCollectTimeout bounds what a collector may ask for. Four collections run
	// at once, so a collector allowed to block indefinitely would starve the rest.
	MaxCollectTimeout = 30 * time.Second

	MaxConcurrency = 4

	CoreFastEvery = 10 * time.Second
	CoreSlowEvery = 60 * time.Second

	DockerEvery = 10 * time.Second
	MongoEvery  = 10 * time.Second
	NginxEvery  = 10 * time.Second

	// NginxLogEvery drains whatever the access log gained since the last tick, so
	// the cadence sets reporting resolution rather than how much is read.
	NginxLogEvery = 10 * time.Second

	// SystemdEvery is slow on purpose: a unit that just failed is still failed a
	// minute later, and listing units shells out.
	SystemdEvery = 60 * time.Second

	PostgresEvery = 10 * time.Second
	MySQLEvery    = 10 * time.Second
	RedisEvery    = 10 * time.Second
	RabbitMQEvery = 30 * time.Second

	// PrometheusEvery and ExecEvery are the defaults for the two generic
	// collectors. Each is configurable once, for the whole collector, under
	// `integrations.prometheus.every` and `integrations.exec.every`.
	PrometheusEvery = 30 * time.Second
	ExecEvery       = 60 * time.Second
)
