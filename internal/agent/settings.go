package agent

import "time"

const (
	CollectTimeout = 3 * time.Second
	MaxConcurrency = 4

	CoreFastEvery = 10 * time.Second
	CoreSlowEvery = 60 * time.Second

	DockerEvery = 10 * time.Second
	MongoEvery  = 10 * time.Second
	NginxEvery  = 10 * time.Second
)
