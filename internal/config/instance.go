package config

import (
	"time"

	configkit "github.com/italypaleale/go-kit/config"
)

var (
	config *Config

	defaultDevConfig ConfigDev
)

func init() {
	// Set the default config at startup
	config = GetDefaultConfig()

	// Set the instance ID
	// This may panic if there's not enough entropy in the system
	var err error
	config.internal.instanceID, err = configkit.GetInstanceID()
	if err != nil {
		panic("failed to set instance ID: " + err.Error())
	}
}

// Get returns the singleton instance
func Get() *Config {
	return config
}

// GetDefaultConfig returns the default configuration.
func GetDefaultConfig() *Config {
	return &Config{
		Logs: ConfigLogs{
			Level:            "info",
			OmitHealthChecks: true,
		},
		Server: ConfigServer{
			Bind:         "0.0.0.0",
			Port:         3000,
			PublicURL:    "http://127.0.0.1:3000",
			ReadTimeout:  5 * time.Minute,
			WriteTimeout: 6 * time.Minute,
		},
		Auth: ConfigAuth{
			Scopes: ConfigAuthScopes{
				Ingest: "ingest",
				Server: "server",
			},
			None: ConfigAuthNone{
				Subject: "anonymous",
				Scopes:  []string{"ingest", "server"},
			},
		},
		RateLimit: ConfigRateLimit{
			Rate:  10,
			Per:   time.Minute,
			Burst: 30,
			Status: ConfigRateLimitStatus{
				Rate:  600,
				Per:   time.Minute,
				Burst: 300,
			},
		},
		Actors: ConfigActors{
			Runtime:                    ActorRuntimeEmbedded,
			Address:                    "127.0.0.1:7571",
			Database:                   "sqlite://./data/actors.db",
			WorkflowTimeout:            10 * time.Minute,
			CompletedWorkflowRetention: 24 * time.Hour,
		},
		Storage: ConfigStorage{
			Originals: "./data/originals",
			Variants:  "./data/variants",
		},
		Images: ConfigImages{
			MaxUploadSize:      32 << 20, // 32MB
			MaxSourcePixels:    50_000_000,
			MaxVariants:        48,
			MaxResolution:      8192,
			VipsCacheMaxMemory: 50 << 20, // 50MB
		},
		Dev: defaultDevConfig,
	}
}
